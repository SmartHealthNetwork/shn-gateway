// populator_seeded.go — generic foreign-DTR populator for the composite harness lane.
//
// THE HONESTY INVARIANT (the spine of this file):
// Every QR answer produced here must trace to a real recorded source — the persona DTR
// answer book — NEVER to a literal chosen to clear br-payer's $submit. Each entry in
// compositeDTRAnswers is the clinically-coherent recorded answer a clinician/provider
// would give for the order: G0151 IS physical-therapist home-health → "Physical therapy
// procedure"; L8000's item is a provider PA-acknowledgment → true. These choices are
// coherent with the order, not with the payer's adjudication path.
//
// The structural enforcement of this invariant is FillQuestionnaireFromAnswers: it
// ERRORS on any required questionnaire item that has no supplied answer. A new payer
// questionnaire therefore forces a new real seed in this file — the gateway can never
// silently fabricate an answer.
//
// A real partner replaces compositeDTRAnswers with their SoR/attestation connector.
package engine

import (
	"context"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// compositeDTRAnswers is the composite harness's recorded persona DTR answers for the
// FOREIGN Da Vinci questionnaires br-payer advertises. THE HONESTY INVARIANT: each
// answer is the clinically-coherent recorded entry for that order — NEVER a literal
// chosen to clear $submit. A real partner replaces this with their SoR/attestation
// connector; the gateway never fabricates an answer (FillQuestionnaireFromAnswers
// errors on a required item with no recorded answer). Keyed by the questionnaire's
// (version-stripped) canonical → linkId → Answer.
var compositeDTRAnswers = map[string]map[string]shnsdk.Answer{
	"http://example.org/fhir/Questionnaire/PriorAuthRequired": {
		// L8000: provider attests prior authorization is acknowledged.
		"1.2": {Boolean: boolPtrSeeded(true)},
	},
	"http://example.org/fhir/Questionnaire/HomeHealthAssessment": {
		// G0151 = physical-therapist home-health → "Physical therapy procedure" (SNOMED 91251008).
		"1.1": {Coding: &shnsdk.AnswerCoding{
			System:  "http://snomed.info/sct",
			Code:    "91251008",
			Display: "Physical therapy procedure",
		}},
	},
}

// boolPtrSeeded returns a pointer to b. A local helper (boolPtr in sdk/dtr.go is
// SDK-package-private; boolStr/itoa in originate.go are not pointer helpers).
func boolPtrSeeded(b bool) *bool { return &b }

// seededPopulator fills br-payer's manual-entry foreign questionnaires from the
// recorded persona DTR answer book (compositeDTRAnswers). It implements Populator.
//
// Honesty guard: if the questionnaire canonical is not present in the answer book,
// Populate returns an error (the gateway cannot fabricate answers). This forces any
// new payer questionnaire to be explicitly seeded with real recorded data.
type seededPopulator struct {
	author  string
	answers map[string]map[string]shnsdk.Answer
}

// NewSeededPopulator builds the seeded backend. author is the Practitioner reference
// that recorded the manual answers (e.g. "Practitioner/1234567890"); it is stamped
// onto every QR item as the dtrx-1 source="manual" author sub-extension.
func NewSeededPopulator(author string) *seededPopulator {
	return &seededPopulator{author: author, answers: compositeDTRAnswers}
}

// Populate extracts the Questionnaire from the package, looks up the canonical in the
// answer book, fills the QR via FillQuestionnaireFromAnswers, and returns the filled
// QR bytes plus a per-item fill summary with Origin="manual".
func (s *seededPopulator) Populate(_ context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	// Step 1: extract the bare Questionnaire from the $questionnaire-package (handles
	// both br-payer's Parameters wrapper and the bare-Bundle sandbox shape).
	q, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		return nil, nil, err // no-Questionnaire → consumer maps to 502
	}

	// Step 2: resolve the canonical (version-stripped) so the answer book key matches
	// regardless of whether the payer appended |version.
	url, err := shnsdk.ParseQuestionnaireURL(q)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: seededPopulator: parse questionnaire url: %w", err)
	}
	canonical := shnsdk.StripCanonicalVersion(url)

	// Step 3: honesty guard — the answer book must have a recorded entry for this
	// questionnaire. If not, we error rather than fabricate.
	ans, ok := s.answers[canonical]
	if !ok {
		return nil, nil, fmt.Errorf(
			"engine: seededPopulator: no recorded DTR answers for questionnaire %q (honesty: the gateway never fabricates answers — seed the persona's recorded answer)",
			canonical,
		)
	}

	// Step 4: fill the QR from the recorded answers. FillQuestionnaireFromAnswers
	// errors on any required item without a supplied answer (the SDK-level honesty guard).
	qrJSON, err := shnsdk.FillQuestionnaireFromAnswers(q, ans, s.author, shnsdk.QRContext{
		PatientRef:  pc.PatientRef,
		CoverageRef: pc.CoverageRef,
		OrderRef:    pc.OrderRef,
		Authored:    pc.Authored,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("engine: seededPopulator: fill questionnaire: %w", err)
	}

	// Step 5: build the per-item fill summary for the ops console (Origin="manual",
	// SourceRef=author). One FilledItem per supplied answer in the recorded book.
	fill := make([]FilledItem, 0, len(ans))
	for linkID, a := range ans {
		fill = append(fill, FilledItem{
			LinkID:    linkID,
			Answer:    answerString(a),
			Origin:    "manual",
			SourceRef: s.author,
		})
	}

	return qrJSON, fill, nil
}

// answerString returns a human-readable string for a filled Answer (used in the
// FilledItem.Answer field for the ops console surface). Mirrors fillSummary's pattern.
func answerString(a shnsdk.Answer) string {
	switch {
	case a.Boolean != nil:
		return boolStr(*a.Boolean)
	case a.Integer != nil:
		return itoa(*a.Integer)
	case a.String != nil:
		return *a.String
	case a.Coding != nil:
		return a.Coding.Code
	default:
		return ""
	}
}
