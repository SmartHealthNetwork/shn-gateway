// davincimap.go — readers and builders around the Da Vinci wire operations. A
// payer's $questionnaire-package answer is relayed exactly (native.go), and
// extractQuestionnaireFromPackage (consumer-side, called from originate.go) reads
// the bare Questionnaire out of it for F5/auto-fill. The request builders here
// serve only the older questionnaire request envelope (dtrLegRequest), which a
// payer gateway still accepts from requesters that do not name the operation:
// a requester that names it sends its own $questionnaire-package Parameters, and
// nothing here rebuilds them. A partner CRD service's answer is never projected
// here: it is relayed exactly (native.go).
package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildQuestionnairePackageRequest translates SHN's {canonical[, coverage]} DTR fetch into
// a Da Vinci $questionnaire-package Parameters request. It is
// buildQuestionnairePackageRequestAtLine("2.0", canonical, coverage), byte-identical
// (regression-fenced by davincimap_test.go) — the legacy name stays the 2.0 delegate so the
// 8-UC demo path is unchanged.
func buildQuestionnairePackageRequest(canonical string, coverage json.RawMessage) ([]byte, error) {
	return buildQuestionnairePackageRequestAtLine("2.0", canonical, coverage)
}

// buildQuestionnairePackageRequestAtLine is buildQuestionnairePackageRequest
// parameterized by DTR line ("2.0", "2.1", "2.2"). When coverage is present, it is
// appended VERBATIM as a `coverage` parameter resource — a real Da Vinci payer (br-payer)
// 400s "The 'coverage' parameter is required (min=1)" without it (FR-G28, every line). The
// coverage is the PROVIDER's inbound Coverage carried through the leg; the payer-gw never
// fabricates one (non-aggregation).
//
// At a line whose DTRDef sets QuestionnairePackageCoverageRequired (2.2 —
// StructureDefinition-dtr-qpackage-input-parameters.json's `coverage` slice tightens to
// min=1 max=1, verified live 2026-08-12), an EMPTY coverage refuses BEFORE the wire: a
// legible local error naming the line and the 1..1 cardinality, replacing what would
// otherwise be the partner's opaque 400. At 2.0/2.1 (coverage required but unbounded, not
// yet gated locally — see DTRDef's doc comment) the pre-existing behavior is unchanged:
// coverage is carried when supplied, omitted otherwise, no local refusal — so with coverage
// nil at "2.0" the output stays canonical-only, byte-identical to the pre-fix request.
func buildQuestionnairePackageRequestAtLine(line, canonical string, coverage json.RawMessage) ([]byte, error) {
	if err := dtrPackageRequireCoverage(line, coverage); err != nil {
		return nil, err
	}
	parameter := []map[string]any{
		{"name": "questionnaire", "valueCanonical": canonical},
	}
	if len(coverage) > 0 {
		parameter = append(parameter, map[string]any{"name": "coverage", "resource": coverage})
	}
	params := map[string]any{
		"resourceType": "Parameters",
		"parameter":    parameter,
	}
	return json.Marshal(params)
}

// dtrPackageRequireCoverage is the shared coverage-1..1 gate for the two
// $questionnaire-package request builders below: at a DTR line whose
// DTRDef sets QuestionnairePackageCoverageRequired, an empty coverage is refused before
// any bytes are built. Unknown line -> error (fail-closed, never a silent 2.0 fallback,
// same posture as buildQuestionnairePackageAtLine).
func dtrPackageRequireCoverage(line string, coverage json.RawMessage) error {
	def, ok := shnsdk.DTRLineDef(line)
	if !ok {
		return fmt.Errorf("engine: $questionnaire-package request: unknown DTR line %q", line)
	}
	if def.QuestionnairePackageCoverageRequired && len(coverage) == 0 {
		return fmt.Errorf("engine: $questionnaire-package request at DTR line %q (profile dtr-qpackage-input-parameters) requires the coverage parameter (1..1, exactly one) but none was supplied", line)
	}
	return nil
}

// dtrLegRequest is the older wire shape of the dtr-questionnaire-fetch leg, accepted from
// requesters that do not name the operation in the request frame (this gateway's own
// requests name it and carry the operation's input instead). It is a
// SUPERSET of shnsdk.QuestionnaireFetchRequest: Canonical + Coverage match the SDK type's JSON
// (so the br-payer / adjudicator paths that unmarshal the SDK type are unaffected, and
// with an empty Order the marshal is byte-identical), plus Order — the CRD-updated ServiceRequest
// a partner requires as the `$questionnaire-package` `order` param (its questionnaire is
// keyed off the order's coverage-assertion-id; it has no `questionnaire` param support). Order is
// defined here, not in the published SDK, so the DEPLOYED payer gateway reads it without an SDK bump.
//
// NextQuestion turns the leg into an SDC adaptive $next-question round (dtr_adaptive.go): the
// in-progress QuestionnaireResponse whose contained Questionnaire is the delivered-so-far tree
// (derivedFrom the source canonical). The payer side forwards it to the partner's
// Questionnaire/$next-question and relays the answer verbatim; a responder that serves no
// adaptive questionnaire refuses it rather than answering with a package. Same
// publish posture as Order: gateway-internal, both gateways read it without an SDK bump.
type dtrLegRequest struct {
	Canonical    string          `json:"canonical"`
	Coverage     json.RawMessage `json:"coverage,omitempty"`
	Order        json.RawMessage `json:"order,omitempty"`
	NextQuestion json.RawMessage `json:"nextQuestion,omitempty"`
}

// buildQuestionnairePackageOrderRequest builds an order-driven $questionnaire-package Parameters
// (the order-driven lane): the CRD-updated `order` (carrying the coverage-assertion-id) + the required
// `coverage`. No `questionnaire` canonical — such a partner 500s without the order and has no canonical path.
// It is buildQuestionnairePackageOrderRequestAtLine("2.0", order, coverage), byte-identical
// (regression-fenced by davincimap_test.go).
func buildQuestionnairePackageOrderRequest(order, coverage json.RawMessage) ([]byte, error) {
	return buildQuestionnairePackageOrderRequestAtLine("2.0", order, coverage)
}

// buildQuestionnairePackageOrderRequestAtLine is buildQuestionnairePackageOrderRequest
// parameterized by DTR line ("2.0", "2.1", "2.2") — same coverage-1..1 gate as
// buildQuestionnairePackageRequestAtLine (dtrPackageRequireCoverage), for the order-driven
// request shape.
func buildQuestionnairePackageOrderRequestAtLine(line string, order, coverage json.RawMessage) ([]byte, error) {
	if err := dtrPackageRequireCoverage(line, coverage); err != nil {
		return nil, err
	}
	parameter := []map[string]any{{"name": "order", "resource": order}}
	if len(coverage) > 0 {
		parameter = append(parameter, map[string]any{"name": "coverage", "resource": coverage})
	}
	return json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": parameter})
}

// isPackageBundleParameter reports whether name is the $questionnaire-package
// output parameter that carries the package Bundle at one of the published DTR
// lines: "return" (the 2.0.1 operation definition), "PackageBundle" (the 2.0.1
// and 2.1.0 output Parameters profile) or "packagebundle" (2.2.0).
func isPackageBundleParameter(name string) bool {
	switch name {
	case "return", "PackageBundle", "packagebundle":
		return true
	}
	return false
}

// unwrapQuestionnairePackage normalises the two $questionnaire-package response shapes:
//
//   - a Parameters resource profiled on dtr-qpackage-output-parameters (the
//     reference payer's answer), whose collection Bundle is the package Bundle
//     parameter under any published name (isPackageBundleParameter);
//   - a bare collection Bundle.
//
// When the input is a Parameters wrapper the function returns the first package
// Bundle's bytes so that the downstream walker sees a plain Bundle in both cases. If
// the Parameters has no package Bundle parameter, raw is returned unchanged and the
// downstream walk will fail with its normal "no Questionnaire" error (not a silent
// mismatch). A bare Bundle (or any other top-level resourceType) is returned
// byte-identical.
func unwrapQuestionnairePackage(raw []byte) []byte {
	var top struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw // malformed — let the downstream walker surface the error
	}
	if top.ResourceType != "Parameters" {
		return raw // bare Bundle or anything else — byte-identical pass-through
	}
	for _, p := range top.Parameter {
		if isPackageBundleParameter(p.Name) && len(p.Resource) > 0 {
			return p.Resource
		}
	}
	return raw // Parameters with no package Bundle — downstream walk will error
}

// extractQuestionnaireFromPackage pulls the bare Questionnaire entry out of a
// $questionnaire-package collection Bundle, returning its bytes VERBATIM. Called by the
// consumer (originate.go) after the full package has crossed the wire — the package's
// dependent Libraries/ValueSets survive the wire intact inside the Bundle; this extractor
// returns the bare Questionnaire that originate.go feeds to ParseQuestionnaireURL (F5)
// and FillQuestionnaire (auto-fill). A package with no Questionnaire entry returns an
// error (→ 502 at the consumer: partner fault).
//
// unwrapQuestionnairePackage is called first so that br-payer's Parameters wrapper
// (dtr-qpackage-output-parameters) is normalised to its inner Bundle before the walk;
// the bare-Bundle path is byte-identical.
func extractQuestionnaireFromPackage(packageBundle []byte) ([]byte, error) {
	packageBundle = unwrapQuestionnairePackage(packageBundle)
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(packageBundle, &bundle); err != nil {
		return nil, fmt.Errorf("engine: parse $questionnaire-package bundle: %w", err)
	}
	for _, e := range bundle.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &probe); err != nil {
			continue
		}
		if probe.ResourceType == "Questionnaire" {
			return e.Resource, nil
		}
	}
	return nil, fmt.Errorf("engine: $questionnaire-package response contains no Questionnaire")
}

// dtrQRCoverageExtURL / dtrIntendedUseExtURL are the DTR QuestionnaireResponse-level
// extensions the 2.2 QR shell (below) carries — same canonicals as sdk/dtr.go's
// qrCoverageExt/intendedUseExt (unexported there; this file cannot reach them, so they
// are re-declared byte-identically).
const (
	dtrQRCoverageExtURL  = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage"
	dtrIntendedUseExtURL = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse"
)

// validateNativePASResponse checks the complete native submit graph and the
// shared SDK decision contract (FR-G28), retaining the payer's exact bytes.
// A bare ClaimResponse is supported only by the separate polling read path.
//
// A graph refusal states its reason: the first reference the answer makes that
// the Bundle does not carry, with the entry that made it. A receiver that only
// said "invalid" could not tell a payer whose answer really is incomplete from
// a rule that is wrong about it (measured 2026-09-19: three of four submissions
// to the 2.2 reference payer refused, no record of which reference dangled).
func validateNativePASResponse(body []byte) ([]byte, LegResult) {
	if err := validatePASBundleGraph(body); err != nil {
		return nil, fail502("invalid native PAS response Bundle: " + strings.TrimPrefix(err.Error(), "engine: "))
	}
	pended, _, err := shnsdk.ParsePendedResponse(body)
	if err != nil {
		return nil, fail502("invalid native PAS response decision")
	}
	if !pended {
		if _, err := shnsdk.ParseClaimResponse(body); err != nil {
			return nil, fail502("invalid native PAS response decision")
		}
	}
	return body, LegResult{}
}

// ValidateNativePASResponseForTest exposes the native response contract to the
// cross-module adversarial harness. It never normalizes or repairs payer bytes.
func ValidateNativePASResponseForTest(body []byte) ([]byte, LegResult) {
	return validateNativePASResponse(body)
}

// validateRelayedPASResponse is validateNativePASResponse for a payer's answer a
// leg is about to relay: the same check, and when it refuses, the refusal is
// logged at THIS node with the correlation id before the framed error goes
// back. The payer's bytes are what a requester would have needed to read the
// refusal, and a refused answer is not relayed — so the record of what was
// refused, and why, has to be made here or nowhere.
func validateRelayedPASResponse(corrID, leg string, body []byte) ([]byte, LegResult) {
	response, lr := validateNativePASResponse(body)
	if lr.Status != 0 {
		logPASResponseRefused(corrID, leg, lr, body)
	}
	return response, lr
}

// pasResponseRefusal is the log record of a payer answer this node refused.
type pasResponseRefusal struct {
	CorrelationID string           `json:"correlationId"`
	LegType       string           `json:"legType"`
	Status        int              `json:"status"`
	Reason        string           `json:"reason"`
	Reference     *pasGraphRefusal `json:"reference,omitempty"` // the first reference the graph does not resolve, when that is the reason
	Entries       int              `json:"entries"`
	Bytes         int              `json:"bytes"`
}

// logPASResponseRefused writes the refusal record. The payer's bytes are not
// logged (an answer can carry member data); the reference that dangled is, as
// the payer wrote it, because it is the one fact that settles whether the payer's
// answer is incomplete or the rule is wrong.
func logPASResponseRefused(corrID, leg string, lr LegResult, body []byte) {
	rec := pasResponseRefusal{CorrelationID: corrID, LegType: leg, Status: lr.Status, Reason: lr.Message, Bytes: len(body)}
	if err := validatePASBundleGraph(body); err != nil {
		rec.Reference = pasGraphRefusalOf(err)
	}
	var shape struct {
		Entry []json.RawMessage `json:"entry"`
	}
	if json.Unmarshal(body, &shape) == nil {
		rec.Entries = len(shape.Entry)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	log.Printf("gateway: pas response refused: %s", raw)
}

// fail502 builds the fail-closed LegResult (502) for a partner answer that does not
// meet its contract.
func fail502(msg string) LegResult {
	return LegResult{Status: http.StatusBadGateway, Message: "engine: " + msg}
}
