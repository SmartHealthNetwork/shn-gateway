//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer's answer whose only finding is a code the lane could not check is
// carried at every level, strict included, and recorded as unchecked
// terminology naming the code system: the validator found no defect, it
// could not look. The same gap beside a defect still refuses where
// the defect does: a structural fault, a slice mismatch the code did not
// cause, and a code the lane checked and did not find.
func TestPASIngress_CarriedAnswerTerminologyGapIsNeverRefused(t *testing.T) {
	t.Parallel()
	const reason = "Bundle.entry[0].resource/*ClaimResponse/cr*/.item[0].adjudication[0].extension[0].extension[1].value.ofType(CodeableConcept)"
	gap := reasonCodeGap(reason)
	structural := shnsdk.Issue{Severity: "error", Code: "structure", MessageID: "Extension_EXTP_Context_Wrong",
		Expression:  []string{"Bundle.entry[0].resource/*ClaimResponse/cr*/.item[0]"},
		Diagnostics: "The extension http://example.org/ext is not allowed to be used at this point"}
	// A slice no-match on another entry, with nothing on that entry the lane
	// could not check: the code on entry 0 did not cause it.
	otherSlice := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: summaryBundleEntryNoMatch,
		Expression:  []string{"Bundle.entry[2].resource/*Task/t*/"},
		Diagnostics: "Bundle.entry[2].resource: Unable to find a match for profile among choices"}
	// The lane holds the code system, checked the code and did not find it.
	checkedMiss := []shnsdk.Issue{
		{Severity: "error", Code: "processing", MessageID: "Terminology_PassThrough_TX_Message", Expression: []string{reason},
			Diagnostics: "Unknown code '" + x12ReviewReason + "#0U'"},
		gap[1],
	}
	// An invariant, a deeper rule's defect the lane decided.
	invariant := func(at string) shnsdk.Issue {
		return shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "http://hl7.org/fhir/StructureDefinition/Period#per-1",
			Expression: []string{at}, Diagnostics: "Constraint failed: per-1: 'If present, start SHALL have a lower or equal value than end'"}
	}
	// On entry 2, an invariant and the no-match summary it causes: the summary
	// follows from a defect the lane decided there, not from the code on entry
	// 0, so the gap does not excuse it.
	decidedSlice := []shnsdk.Issue{invariant("Bundle.entry[2].resource/*Task/t*/.executionPeriod"),
		{Severity: "error", Code: "processing", MessageID: summaryBundleEntryNoMatch, Expression: []string{"Bundle.entry[2].resource/*Task/t*/"},
			Diagnostics: "Bundle.entry[2].resource: Unable to find a match for profile among choices"}}
	// A code system the lane does not hold, X12 or any other, is unknown to
	// it: on an element no binding constrains, the code is a gap.
	const otherSystem = "http://example.org/fhir/CodeSystem/local-codes"
	unheldSystem := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_PassThrough_TX_Message", Expression: []string{reason},
		Diagnostics: "CodeSystem is unknown and can't be validated: " + otherSystem + " for '" + otherSystem + "#x'"}
	// The validator's own catch for an HL7 FHIR namespace system it does not
	// know (a misspelled HL7 URL looks the same) is no gap the lane names.
	hl7Unknown := shnsdk.Issue{Severity: "error", Code: "processing", MessageID: "Terminology_TX_System_Unknown", Expression: []string{reason},
		Diagnostics: "Unknown Code System 'http://terminology.hl7.org/CodeSystem/adjudcation'"}
	// A required binding whose value set the lane could expand missed a code
	// from a system it does not hold: the lane decided the miss.
	decidedBindingMiss := []shnsdk.Issue{unheldSystem, {Severity: "error", Code: "processing", MessageID: "Terminology_TX_NoValid_1_CC", Expression: []string{reason},
		Diagnostics: "None of the codings provided are in the value set 'Adjudication Reason' (http://hl7.org/fhir/ValueSet/adjudication-reason|4.0.1), and a coding from this value set is required) (codes = " + otherSystem + "#x)"}}
	type row struct {
		name      string
		issues    []shnsdk.Issue
		refusedAt []ConformanceEnforcement
		unchecked []string // the systems an unchecked finding names; nil for any other reading
	}
	rows := []row{
		{"gap only", gap, nil, []string{"https://codesystem.x12.org/external/886"}},
		{"a code system the lane does not hold, on an unbound element", []shnsdk.Issue{unheldSystem}, nil, []string{namedCodeSystem(otherSystem)}},
		{"an HL7 namespace code system the validator does not know", []shnsdk.Issue{hl7Unknown}, []ConformanceEnforcement{EnforcementStrict}, nil},
		{"a binding miss under a value set the lane could expand", decidedBindingMiss, []ConformanceEnforcement{EnforcementStrict}, nil},
		{"gap beside a structural fault", append(slices.Clone(gap), structural), []ConformanceEnforcement{EnforcementStructural, EnforcementStrict}, nil},
		{"gap beside a slice mismatch it did not cause", append(slices.Clone(gap), otherSlice), []ConformanceEnforcement{EnforcementStructural, EnforcementStrict}, nil},
		{"a code the lane checked and did not find", checkedMiss, []ConformanceEnforcement{EnforcementStrict}, nil},
		{"gap beside a deeper defect", append(slices.Clone(gap), invariant("Bundle.entry[0].resource/*ClaimResponse/cr*/.preAuthPeriod")), []ConformanceEnforcement{EnforcementStrict}, nil},
		{"gap beside a slice mismatch a decided defect caused", append(slices.Clone(gap), decidedSlice...), []ConformanceEnforcement{EnforcementStrict}, nil},
	}
	for _, leg := range carriedAnswerLegs() {
		for _, r := range rows {
			for _, level := range allLevels {
				t.Run(leg.leg+"/"+r.name+"/"+level.String(), func(t *testing.T) {
					v := issuesValidator{ifContains: `"ClaimResponse"`, issues: r.issues}
					env, rec, findings, xr, answer := carriedAnswerRun(t, leg, level, "", v)
					if env.routeHitCount() != 1 {
						t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
					}
					refused := slices.Contains(r.refusedAt, level)
					if refused {
						if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ingress validation failed") {
							t.Fatalf("answer %d %s, want it refused 502", rec.Code, rec.Body)
						}
						wantRefusal(t, xr, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
					} else if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), relayedPASAnswer(t, answer)) {
						t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
					}
					if level == EnforcementNone {
						if len(findings) != 0 {
							t.Fatalf("at none nothing is checked or recorded, got %+v", findings)
						}
						return
					}
					if len(findings) != 1 {
						t.Fatalf("want one finding, got %+v", findings)
					}
					f := findings[0]
					want := map[bool]string{true: "refused", false: "relayed"}[refused]
					if f.Decision != want {
						t.Fatalf("decision %q, want %q: %+v", f.Decision, want, f)
					}
					if r.unchecked != nil {
						if f.Verdict != "unchecked" || !slices.Equal(f.UncheckedCodeSystems, r.unchecked) {
							t.Fatalf("a terminology gap is recorded as unchecked naming %v, got verdict %q systems %v", r.unchecked, f.Verdict, f.UncheckedCodeSystems)
						}
						if f.Tolerated {
							t.Fatal("a gap is carried by the reading, not by a tolerance")
						}
						return
					}
					if f.Verdict == "unchecked" || len(f.UncheckedCodeSystems) != 0 {
						t.Fatalf("a defect beside the gap is no terminology gap, got verdict %q systems %v", f.Verdict, f.UncheckedCodeSystems)
					}
				})
			}
		}
	}
}

// Among a payer answer's candidate lines, a terminology gap is the reading
// closest to valid: with a deeper defect at 2.0 and a structural one at 2.2,
// the gap at 2.1 decides, so strict carries the answer and records it as
// unchecked at 2.1, each line listed with its own verdict.
func TestPayerAnswerIngress_TerminologyGapBeatsDeeperAndStructural(t *testing.T) {
	verdicts := map[string]scriptedVerdict{"2.0": scriptDeeper, "2.1": scriptUnchecked, "2.2": scriptStructural}
	for _, profile := range providerLanes {
		g, _, findings := payerIngressGateway(t, profile, EnforcementStrict, verdicts)
		status, _ := drainedPayerIngress(g, answerCtx("dtr-questionnaire-fetch"), []byte(answerBundle), "2.0", "pa.dtr", partner)
		if status != 0 {
			t.Fatalf("%s: status %d, want the answer carried", profile, status)
		}
		if len(*findings) != 1 {
			t.Fatalf("%s: findings %+v, want one", profile, *findings)
		}
		f := (*findings)[0]
		if f.Decision != "relayed" || f.Line != "2.1" || f.Verdict != "unchecked" {
			t.Fatalf("%s: finding %+v, want relayed, unchecked at 2.1", profile, f)
		}
		got := map[string]string{}
		for _, l := range f.Lines {
			got[l.Line] = l.Verdict
		}
		if got["2.1"] != "unchecked" || got["2.0"] != "deeper" {
			t.Fatalf("%s: lines %+v, want 2.1 unchecked and 2.0 deeper", profile, f.Lines)
		}
	}
}

// On a PAS 2.1 request Bundle, the Claim slice-match cascade a terminology
// gap causes is excused with it only on the entry that carries the gap: the
// same cascade on another Claim entry, with no gap of its own, is no gap, and
// the Bundle reads deeper, which strict refuses.
func TestTerminologyGapExcusesOnlyItsOwnEntrysSliceCascade(t *testing.T) {
	t.Parallel()
	body := []byte(`{"resourceType":"Bundle","meta":{"profile":["` + pasRequestBundleCanonical + `"]},"entry":[` +
		`{"resource":{"resourceType":"Claim","id":"a"}},{"resource":{"resourceType":"Claim","id":"b"}}]}`)
	// cascade is what HAPI reports on Claim entry n that matches neither PAS
	// Claim profile: the no-match summary naming both, and the sibling's own
	// Claim.related minimum, attributed to it alone.
	cascade := func(n int) []shnsdk.Issue {
		at := fmt.Sprintf("Bundle.entry[%d].resource/*Claim/x*/", n)
		return []shnsdk.Issue{
			{Severity: "error", Code: "processing", MessageID: summaryBundleEntryNoMatch, Expression: []string{at},
				Diagnostics: "Bundle.entry[" + fmt.Sprint(n) + "].resource: Unable to find a match for profile among choices: " + pasClaimCanonical + "|2.1.0, " + pasClaimUpdateCanonical + "|2.1.0"},
			{Severity: "error", Code: "processing", MessageID: siblingMinimum, Expression: []string{at + ".related"},
				Diagnostics: "Claim.related: minimum required = 1, but only found 0 (validating against " + pasClaimUpdateCanonical + "|2.1.0 [PAS Claim Update])"},
		}
	}
	gapOn0 := append(reasonCodeGap("Bundle.entry[0].resource/*Claim/a*/.item[0].productOrService"), cascade(0)...)
	for _, c := range []struct {
		name   string
		issues []shnsdk.Issue
		want   string
	}{
		{"the gap and its own entry's cascade", gapOn0, "unchecked"},
		{"plus the cascade on another Claim with no gap", append(slices.Clone(gapOn0), cascade(1)...), "deeper"},
	} {
		res := shnsdk.Result{Details: c.issues}
		for _, iss := range c.issues {
			res.Issues = append(res.Issues, iss.Diagnostics)
		}
		if got := ClassifyFHIRForTest(res, body, "2.1", pasRequestBundleCanonical); got != c.want {
			t.Errorf("%s: reads %s, want %s", c.name, got, c.want)
		}
	}
}

// A request this gateway built whose only finding is a code the lane could
// not check is sent at every level, strict included, and recorded at observe
// and above as unchecked: the gateway's own build is not refused for a code
// its validator could not look at.
func TestBuiltRequestTerminologyGapIsSentAtEveryLevel(t *testing.T) {
	gap := reasonCodeGap("Bundle.entry[0].resource.item[0].productOrService")
	res := shnsdk.Result{Issues: []string{gap[0].Diagnostics, gap[1].Diagnostics}, Details: gap}
	for _, row := range builtRequestRows() {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				gw, stub := row.fixture(t)
				gw.cfg.ConformanceEnforcement = level
				var findings []ConformanceFinding
				gw.cfg.Observer = func(e ObserverEvent) {
					var f ConformanceFinding
					if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.LegType == row.leg && f.Whose == "own" {
						findings = append(findings, f)
					}
				}
				var checks []ownRequestCheck
				gw.cfg.Validator = ownRequestValidator{leg: row.leg, res: res, checks: &checks}
				code := row.run(t, gw)
				gw.drainObserveChecks()
				if !legAttempted(stub.legTypes, row.leg) {
					t.Fatalf("answered %d and the leg was not sent (legs %v)", code, stub.legTypes)
				}
				if level == EnforcementNone {
					if len(findings) != 0 || len(checks) != 0 {
						t.Fatalf("at none: %d findings, %d checks, want none", len(findings), len(checks))
					}
					return
				}
				if len(findings) == 0 || findings[0].Decision != "relayed" || findings[0].Verdict != "unchecked" {
					t.Fatalf("findings %+v, want the first relayed and unchecked", findings)
				}
			})
		}
	}
}
