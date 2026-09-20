// davincimap.go — readers around the Da Vinci wire operations. A payer's
// $questionnaire-package answer is relayed exactly (native.go), and
// extractQuestionnaireFromPackage (consumer-side, called from originate.go) reads
// the bare Questionnaire out of it for F5/auto-fill. Nothing here builds a
// request: a requester sends its own $questionnaire-package Parameters in a
// request frame naming the operation, and the payer gateway sends them on as
// they are. A partner CRD service's answer is never projected here: it is
// relayed exactly (native.go).
package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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

// PASResponseSubjectMismatchForTest exposes the subject-binding read of a
// payer's answer — "" when every subject binds, else why not — to the
// adversarial rows that pin it on captured bytes.
func PASResponseSubjectMismatchForTest(body []byte) string {
	if r := pasResponseSubjectMismatch(body); r != nil {
		return r.Why
	}
	return ""
}

// RefusePASResponseSubjectsForTest runs the subject-binding fence over a payer's
// answer exactly as the payer leg does (status 403), including the refusal log
// record, for the adversarial rows that pin what the record names.
func RefusePASResponseSubjectsForTest(corrID, leg string, body []byte) (int, string) {
	return refusePASResponseSubjects(corrID, leg, http.StatusForbidden, body)
}

// refusePASResponseSubjects is the subject-binding fence over a payer's answer:
// (0, "") when every subject binds to the ClaimResponse's patient, else the
// status the caller refuses with and a message naming the cause — and the
// refusal logged at THIS node, as closure refusals are, because the answer is
// not relayed and the requester cannot read for itself which subject did not
// bind.
func refusePASResponseSubjects(corrID, leg string, status int, body []byte) (int, string) {
	r := pasResponseSubjectMismatch(body)
	if r == nil {
		return 0, ""
	}
	msg := "PAS response has inconsistent patient linkage: " + r.Why
	logPASResponseRefusal(pasResponseRefusal{CorrelationID: corrID, LegType: leg, Status: status, Reason: msg, Subject: r, Entries: pasEntryCount(body), Bytes: len(body)})
	return status, msg
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
	CorrelationID string             `json:"correlationId"`
	LegType       string             `json:"legType"`
	Status        int                `json:"status"`
	Reason        string             `json:"reason"`
	Reference     *pasGraphRefusal   `json:"reference,omitempty"` // the first reference the graph does not resolve, when that is the reason
	Subject       *pasSubjectRefusal `json:"subject,omitempty"`   // the first subject that does not bind, when that is the reason
	Entries       int                `json:"entries"`
	Bytes         int                `json:"bytes"`
}

// logPASResponseRefused writes the refusal record. The payer's bytes are not
// logged (an answer can carry member data); the reference that dangled is, as
// the payer wrote it, because it is the one fact that settles whether the payer's
// answer is incomplete or the rule is wrong.
func logPASResponseRefused(corrID, leg string, lr LegResult, body []byte) {
	rec := pasResponseRefusal{CorrelationID: corrID, LegType: leg, Status: lr.Status, Reason: lr.Message, Entries: pasEntryCount(body), Bytes: len(body)}
	if err := validatePASBundleGraph(body); err != nil {
		rec.Reference = pasGraphRefusalOf(err)
	}
	logPASResponseRefusal(rec)
}

func logPASResponseRefusal(rec pasResponseRefusal) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	log.Printf("gateway: pas response refused: %s", raw)
}

// pasEntryCount is how many entries the answer carries, for the refusal record.
func pasEntryCount(body []byte) int {
	var shape struct {
		Entry []json.RawMessage `json:"entry"`
	}
	if json.Unmarshal(body, &shape) != nil {
		return 0
	}
	return len(shape.Entry)
}

// fail502 builds the fail-closed LegResult (502) for a partner answer that does not
// meet its contract.
func fail502(msg string) LegResult {
	return LegResult{Status: http.StatusBadGateway, Message: "engine: " + msg}
}
