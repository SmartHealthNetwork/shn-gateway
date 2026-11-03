//go:build !shn_coreonly

package engine

import (
	"context"
	"strings"
	"testing"
)

// federatedRecordsRefused, isolated: records about the member are carried at
// every level and record nothing; a record about another patient is the
// content check patient.answer, refused at strict with a reason that names no
// patient, and carried below it, recorded at observe and structural and not
// at none.
func TestFederatedRecordsRefused(t *testing.T) {
	const answer = `{"resourceType":"Task","status":"completed"}`
	records := func(subject string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/MBR-1"}}},` +
			`{"resource":{"resourceType":"Observation","id":"o2","status":"final","code":{"text":"x"},"subject":{"reference":"` + subject + `"}}}]}`)
	}
	ctx := withFindingContext(context.Background(), findingContext{LegType: "federated-query", Whose: "peer", CorrelationID: "c1"})
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			g, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC05", birthDate: "1968-03-12", familyName: "Johansson", pendedItem: "operative-diagnostic-report"})
			g.cfg.ConformanceEnforcement = level
			ev := observeLevelEvents(g)
			if reason := g.federatedRecordsRefused(ctx, "MBR-1", []byte(answer), records("Patient/MBR-1")); reason != "" || len(ev.findings) != 0 {
				t.Fatalf("records about the member: reason %q, %d findings; want carried, none recorded", reason, len(ev.findings))
			}
			reason := g.federatedRecordsRefused(ctx, "MBR-1", []byte(answer), records("Patient/MBR-2"))
			if want := map[bool]int{true: 0, false: 1}[level == EnforcementNone]; patientAnswerFindings(ev) != want {
				t.Fatalf("%d %s findings, want %d", patientAnswerFindings(ev), RulePatientAnswer, want)
			}
			if level != EnforcementStrict {
				if reason != "" {
					t.Fatalf("refused below strict: %q", reason)
				}
				return
			}
			if reason == "" || strings.Contains(reason, "MBR-2") {
				t.Fatalf("reason %q, want a refusal naming no patient", reason)
			}
		})
	}
}
