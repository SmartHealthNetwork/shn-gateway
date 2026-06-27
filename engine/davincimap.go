// davincimap.go — translators between SHN's internal leg shapes and the real Da Vinci
// wire ops. SHN's CRD hook omits the CDS-Hooks-required
// hookInstance; the DTR leg's ResponseFHIR is the full $questionnaire-package collection
// Bundle — buildQuestionnairePackage wraps the sandbox Questionnaire, native.go
// forwards a real partner's package VERBATIM, and extractQuestionnaireFromPackage
// (consumer-side, called from originate.go) extracts the bare Questionnaire for
// F5/auto-fill. Deps survive the wire. normalizeCRDCoverage (FR-G25) projects a partner
// CRD service's coverage-information onto the canonical shnsdk.CardCoverage — this file
// therefore references shnsdk.CardCoverage (the engine package already depends on shnsdk
// via nativepas.go).
package engine

import (
	"encoding/json"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ext-coverage-information is the Da Vinci CRD StructureDefinition url under which a CRD
// service grafts coverage guidance onto a card / its update-action resource. The normalizer
// locates this extension and reads the split shape (covered / pa-needed / questionnaire* /
// satisfied-pa-id sub-extensions) from it — the uniform shape across all published CRD STUs
// (2.0.1 / 2.1 / 2.2.1). The normalizer reads only this split shape; the single-coverageInfo
// valueCoding shape some older draft images emit is a pre-STU ballot artifact and is not read.
const extCoverageInformation = "http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information"

// buildQuestionnairePackageRequest translates SHN's {canonical[, coverage]} DTR fetch into
// a Da Vinci $questionnaire-package Parameters request. When coverage is present, it is
// appended VERBATIM as a `coverage` parameter resource — a real Da Vinci payer (br-payer)
// 400s "The 'coverage' parameter is required (min=1)" without it (FR-G28). The
// coverage is the PROVIDER's inbound Coverage carried through the leg; the payer-gw never
// fabricates one (non-aggregation). When coverage is nil the output is canonical-only —
// byte-identical to the pre-fix request, so the sandbox / 8-UC-demo path is unchanged.
func buildQuestionnairePackageRequest(canonical string, coverage json.RawMessage) ([]byte, error) {
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

// unwrapQuestionnairePackage normalises the two $questionnaire-package response shapes:
//
//   - br-payer (a8bece4) returns a Parameters resource profiled on
//     dtr-qpackage-output-parameters; the inner collection Bundle lives at
//     parameter[name=="packagebundle"].resource.
//   - SHN's own sandbox/native path returns a bare collection Bundle (resourceType=="Bundle").
//
// When the input is a Parameters wrapper the function returns the packagebundle resource
// bytes so that the downstream walker sees a plain Bundle in both cases. If the
// Parameters has no packagebundle parameter, raw is returned unchanged and the downstream
// walk will fail with its normal "no Questionnaire" error (not a silent mismatch). A bare
// Bundle (or any other top-level resourceType) is returned byte-identical — the sandbox
// and native paths are completely unaffected.
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
		if p.Name == "packagebundle" && len(p.Resource) > 0 {
			return p.Resource
		}
	}
	return raw // Parameters with no packagebundle — downstream walk will error
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
// the sandbox/native bare-Bundle path is byte-identical.
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

// buildQuestionnairePackage wraps a bare Questionnaire into a one-entry
// $questionnaire-package collection Bundle — the uniform DTR-fetch leg response
// shape. The sandbox payer has no dependent Libraries/ValueSets, so this
// wrapper is honestly deps-free; a real partner's package (forwarded VERBATIM by
// native.go) carries them. This function uses no shnsdk symbols directly (the file
// itself does, via the CRD/PAS normalizers). The byte shape
// (json.Marshal of this map) is load-bearing: the test loopback's default wrap
// must match it exactly for the DTR-fetch leg to stay byte-parity in
// test/responderparity (the wrapper is engine-value-free — no corrID/clock).
func buildQuestionnairePackage(questionnaire []byte) ([]byte, error) {
	// A FHIR collection Bundle requires every entry to carry a fullUrl (IG-HAPI
	// $validate enforces this — caught by make validate, not the hermetic gate).
	// Use the Questionnaire's canonical url as the entry identity (deterministic;
	// keeps the wrap byte-identical across the gateway/sdk/substrate/loopback
	// producers for test/responderparity + test/sdkparity byte-parity).
	var probe struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(questionnaire, &probe); err != nil {
		return nil, fmt.Errorf("engine: buildQuestionnairePackage: questionnaire is not valid json: %w", err)
	}
	if probe.URL == "" {
		return nil, fmt.Errorf("engine: buildQuestionnairePackage: questionnaire has no url for entry fullUrl")
	}
	pkg := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"entry": []map[string]any{
			{"fullUrl": probe.URL, "resource": json.RawMessage(questionnaire)},
		},
	}
	return json.Marshal(pkg)
}

// crdResponse / crdCard / crdSuggestion / crdAction / crdSystemAction model just enough of
// a CDS Hooks CRD response to walk to the coverage-information extension. A CRD STU 2.2.1 RI
// places it on a top-level systemAction's resource; the card-suggestion path and
// card.extension[] fallback are retained for compatibility with other RIs. Both `extension`
// shapes are decoded as raw arrays of
// subExtension because FHIR extensions are recursive; we only need url + the value* leaves
// at the coverage-information level.
type crdResponse struct {
	Cards         []crdCard         `json:"cards"`
	SystemActions []crdSystemAction `json:"systemActions"` // br-payer / STU-2.2 primary path
}
type crdSystemAction struct {
	Resource struct {
		Extension []subExtension `json:"extension"`
	} `json:"resource"`
}
type crdCard struct {
	Suggestions []crdSuggestion `json:"suggestions"`
	Extension   json.RawMessage `json:"extension"` // some RIs put coverage-information here (fallback)
}
type crdSuggestion struct {
	Actions []crdAction `json:"actions"`
}
type crdAction struct {
	Resource struct {
		Extension []subExtension `json:"extension"`
	} `json:"resource"`
}

// subExtension is one FHIR Extension entry. The coverage-information extension nests its
// sub-extensions under `extension`; its leaves carry the value[x] we read. Any other
// value[x] keys on the wire are tolerated (json.Unmarshal ignores unknown fields).
type subExtension struct {
	URL            string         `json:"url"`
	Extension      []subExtension `json:"extension"`
	ValueCode      string         `json:"valueCode"`
	ValueCanonical string         `json:"valueCanonical"`
	ValueString    string         `json:"valueString"`
}

// normalizeCRDCoverage parses a partner CRD service's CDS-Hooks response and projects its
// coverage-information extension onto SHN's canonical shnsdk.CardCoverage. It is
// SPLIT-SHAPE ONLY (FR-G25): a current Da Vinci CRD RI (e.g. HL7-DaVinci/br-payer, CRD STU
// 2.2.1) emits the split covered / pa-needed / questionnaire* / satisfied-pa-id
// sub-extensions — the uniform shape across all published CRD STUs (2.0.1 / 2.1 / 2.2.1).
// It walks to the coverage-information extension at the load-bearing paths:
//   - systemActions[].resource.extension[] (the STU-2.2 primary path)
//   - cards[].suggestions[].actions[].resource.extension[] (the card-suggestion path)
//   - cards[].extension[] (defensive fallback for other RIs)
//
// The CRD leg has NO $validate net, so the normalizer is the gate: it is tolerant on the
// way in but FAILS CLOSED (502 LegResult) on any unresolvable signal — no
// coverage-information found, or the split covered sub-extension is absent. A 0 Status
// means proceed.
func normalizeCRDCoverage(body []byte) (shnsdk.CardCoverage, LegResult) {
	var resp crdResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return shnsdk.CardCoverage{}, fail502("CRD response is not valid JSON")
	}
	ext := findCoverageInformation(resp)
	if ext == nil {
		return shnsdk.CardCoverage{}, fail502("CRD response carries no coverage-information")
	}
	return mapCoverageInformation(ext)
}

// findCoverageInformation walks the response to the first coverage-information extension.
// Walk order (first match wins):
//  1. systemActions[].resource.extension[] — the STU-2.2 primary path.
//  2. cards[].suggestions[].actions[].resource.extension[] — the card-suggestion path
//     used by some RIs.
//  3. cards[].extension[] — defensive fallback for RIs that attach coverage-information
//     directly to the card.
//
// Returns nil if none is present.
func findCoverageInformation(resp crdResponse) []subExtension {
	// (1) systemActions primary path (br-payer / STU-2.2).
	for _, a := range resp.SystemActions {
		for i := range a.Resource.Extension {
			if a.Resource.Extension[i].URL == extCoverageInformation {
				return a.Resource.Extension[i].Extension
			}
		}
	}
	for _, c := range resp.Cards {
		// (2) cards[].suggestions[].actions[].resource.extension[].
		for _, s := range c.Suggestions {
			for _, a := range s.Actions {
				for i := range a.Resource.Extension {
					if a.Resource.Extension[i].URL == extCoverageInformation {
						return a.Resource.Extension[i].Extension
					}
				}
			}
		}
		// (3) Fallback: cards[].extension[] (other RIs). card.extension can be an array
		// (FHIR extension) — decode it lazily only when present.
		if len(c.Extension) > 0 {
			var cardExts []subExtension
			if err := json.Unmarshal(c.Extension, &cardExts); err == nil {
				for i := range cardExts {
					if cardExts[i].URL == extCoverageInformation {
						return cardExts[i].Extension
					}
				}
			}
		}
	}
	return nil
}

// mapCoverageInformation reads the split coverage-information sub-extensions 1:1 onto
// CardCoverage (FR-G25, split-shape only). covered is 1..1: absent ⇒
// fail closed (502). Unknown sub-extension URLs are tolerated (ignored) — the split shape
// carries additional informational sub-extensions (doc-needed, billingCode, date,
// coverage-assertion-id, etc.) that the normalizer does not need.
func mapCoverageInformation(subs []subExtension) (shnsdk.CardCoverage, LegResult) {
	var covered, paNeeded, satisfiedPaID string
	var questionnaires []string

	for _, s := range subs {
		switch s.URL {
		case "covered":
			covered = s.ValueCode
		case "pa-needed":
			paNeeded = s.ValueCode
		case "questionnaire":
			if s.ValueCanonical != "" {
				questionnaires = append(questionnaires, s.ValueCanonical)
			}
		case "satisfied-pa-id":
			satisfiedPaID = s.ValueString
		}
	}

	// covered is 1..1 in the split shape — a missing covered is an unresolvable signal.
	if covered == "" {
		return shnsdk.CardCoverage{}, fail502("CRD coverage-information has no covered value")
	}
	return shnsdk.CardCoverage{
		Covered:        covered,
		PANeeded:       paNeeded,
		Questionnaires: questionnaires,
		SatisfiedPaID:  satisfiedPaID,
	}, LegResult{}
}

// normalizePASResponse is the PAS-response Bundle discriminator (FR-G28). A real Da Vinci
// $submit endpoint ALWAYS returns a Bundle, but SHN's canonical wire convention is:
//
//   - bare ClaimResponse → approved or denied (originator calls shnsdk.ParseClaimResponse).
//   - Bundle{ClaimResponse(queued) + Task} → pended (originator calls shnsdk.ParsePendedResponse).
//
// Without normalization shnsdk.ParsePendedResponse (sdk/pas.go:431) misclassifies any
// top-level Bundle — including a real approved response — as "pended". This function
// discriminates on CONTENT, never on Bundle shape alone:
//
//   - bare ClaimResponse → pass through (already canonical).
//   - Bundle with a ClaimResponse whose outcome=="complete" → unwrap that ClaimResponse
//     (covers both approve A1 and deny A3 — a denial is also outcome:complete; the
//     originator's ParseClaimResponse reads the reviewAction code to distinguish them).
//   - Bundle with a Task entry → SHN pended shape → pass through unchanged.
//   - Bundle with a ClaimResponse whose outcome=="queued" (real-RI pended shape, no SHN Task) →
//     DEF-G1 lifted: pass through unchanged so ParsePendedResponse identifies it as pended.
//     br-payer's amended re-POST response is exactly this shape (A4, queued, no Task).
//   - any other Bundle (no complete/queued ClaimResponse, no Task) → 502 fail-closed.
//   - unparseable or unknown top-level resourceType → 502 fail-closed.
//
// A zero-Status LegResult means "proceed" (caller should use the returned bytes).
// A non-zero Status means "return this error to the caller now" (bytes are nil).
func normalizePASResponse(body []byte) ([]byte, LegResult) {
	var top struct {
		ResourceType string `json:"resourceType"`
		Entry        []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fail502("PAS response is not valid JSON")
	}

	switch top.ResourceType {
	case "ClaimResponse":
		// Already canonical — pass through.
		return body, LegResult{}

	case "Bundle":
		// Walk entries: find a ClaimResponse(complete) to unwrap, or a Task (SHN pended),
		// or a ClaimResponse(queued) (real-RI pended, DEF-G1 lifted).
		hasTask := false
		hasCompleteClaimResponse := false
		hasQueuedClaimResponse := false
		var completeClaimResponseBytes json.RawMessage
		for _, e := range top.Entry {
			var rt struct {
				ResourceType string `json:"resourceType"`
				Outcome      string `json:"outcome"`
			}
			if err := json.Unmarshal(e.Resource, &rt); err != nil {
				continue
			}
			switch {
			case rt.ResourceType == "Task":
				hasTask = true
			case rt.ResourceType == "ClaimResponse" && rt.Outcome == "complete":
				hasCompleteClaimResponse = true
				completeClaimResponseBytes = e.Resource
			case rt.ResourceType == "ClaimResponse" && rt.Outcome == "queued":
				hasQueuedClaimResponse = true
			}
		}
		if hasTask {
			// SHN pended Bundle (ClaimResponse + Task) — pass through unchanged.
			return body, LegResult{}
		}
		if hasCompleteClaimResponse {
			// Unwrap the complete ClaimResponse (A1 approve or A3 deny).
			return []byte(completeClaimResponseBytes), LegResult{}
		}
		if hasQueuedClaimResponse {
			// Real-RI pended Bundle (queued ClaimResponse, no SHN Task) — DEF-G1 lifted.
			// br-payer's amended re-POST response is exactly this shape (A4 queued, no Task);
			// pass through so ParsePendedResponse identifies it as pended. The update
			// responder (handlePASClaimUpdateNative) converts a pended re-POST to 422.
			return body, LegResult{}
		}
		// Bundle with no complete/queued ClaimResponse and no Task → 502 fail-closed.
		return nil, fail502("PAS response Bundle is neither SHN-pended (no Task) nor a complete or queued ClaimResponse")

	default:
		return nil, fail502("PAS response has unexpected resourceType: " + top.ResourceType)
	}
}

// NormalizePASResponseForTest is a thin exported wrapper around normalizePASResponse
// for the test/adversarial package, which cannot access unexported engine symbols.
// Production code must always call normalizePASResponse directly (nativepas.go).
// Named *ForTest to signal it is a test seam, not a public API.
func NormalizePASResponseForTest(body []byte) ([]byte, LegResult) {
	return normalizePASResponse(body)
}

// fail502 builds the fail-closed LegResult (502) the CRD normalizer returns when no
// canonical coverage can be resolved (the CRD leg has no $validate net).
func fail502(msg string) LegResult {
	return LegResult{Status: http.StatusBadGateway, Message: "engine: " + msg}
}
