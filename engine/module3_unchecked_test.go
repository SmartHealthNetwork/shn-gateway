package engine

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// issuesValidator answers a payload containing ifContains as invalid with
// exactly issues, and any other payload as valid.
type issuesValidator struct {
	ifContains string
	issues     []shnsdk.Issue
}

func (v issuesValidator) Validate(_ context.Context, body []byte, _ string) (shnsdk.Result, error) {
	if !bytes.Contains(body, []byte(v.ifContains)) {
		return shnsdk.Result{Valid: true}, nil
	}
	res := shnsdk.Result{Details: slices.Clone(v.issues)}
	for _, iss := range v.issues {
		res.Issues = append(res.Issues, iss.Diagnostics)
	}
	return res, nil
}

// x12ReviewReason is the X12 code system PAS binds a review action's reason
// to (X12278ReviewDecisionReasonCode): a whole licensed code system no IG
// package ships, so no lane can expand the value set.
const x12ReviewReason = "https://codesystem.x12.org/external/886"

// reasonCodeGap is how a lane reports a review-action reason it could not
// check, in the lanes' own words (uncheckedCodeSystemShapes, the required
// binding miss's code list): it could not expand the value set for want of
// the code system, so the binding miss on that element is undecidable too.
func reasonCodeGap(at string) []shnsdk.Issue {
	return []shnsdk.Issue{
		{Severity: "error", Code: "processing", MessageID: "Terminology_PassThrough_TX_Message", Expression: []string{at},
			Diagnostics: "Unable to expand ValueSet because CodeSystem could not be found: " + x12ReviewReason},
		{Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC", Expression: []string{at},
			Diagnostics: "None of the codings provided are in the value set 'X12 278 Review Decision Reason Code' (http://hl7.org/fhir/us/davinci-pas/ValueSet/X12278ReviewDecisionReasonCode|2.0.1), and a coding from this value set is required) (codes = " + x12ReviewReason + "#0U)"},
	}
}

// The decision table is total over the verdicts: a terminology gap is
// recorded at every level, never refused.
func TestDecideRecordsUncheckedAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindCDSEnvelope, KindContent} {
			if d := NewConformancePolicy(level).Decide(kind, "", VerdictUnchecked); d != Record {
				t.Fatalf("%s %s: unchecked decides %s, want relayed", level, kind, d)
			}
		}
	}
}

// A finding names an X12 code system canonical as written, a versioned code
// list or one of X12's external code lists, and any other code system by its
// size and digest, since it quotes the payload's coding.
func TestNamedCodeSystem(t *testing.T) {
	t.Parallel()
	for _, system := range []string{"https://codesystem.x12.org/005010/306", "https://codesystem.x12.org/external/886"} {
		if got := namedCodeSystem(system); got != system {
			t.Errorf("%s is named %q, want it as written", system, got)
		}
	}
	for _, system := range []string{"https://codesystem.x12.org/external/886x", "https://codesystem.x12.org/external/886/1", "http://example.org/cs", "https://codesystem.x12.org/other/886"} {
		if got := namedCodeSystem(system); !strings.HasPrefix(got, "code system bytes=") {
			t.Errorf("%s is named %q, want its size and digest", system, got)
		}
	}
}
