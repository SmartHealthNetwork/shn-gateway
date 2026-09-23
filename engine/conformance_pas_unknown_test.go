package engine

import (
	"context"
	"fmt"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"sync/atomic"
	"testing"
)

// Unsupported declarations cannot select a known PAS schema. These are real
// registry checks; native endpoint admission is a separate transport concern.
func TestUnknownPASStructuralAvailability(t *testing.T) {
	for _, row := range []struct{ leg, direction, rule, body string }{
		{"pas-claim", "request", "pas.request.bundle", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`},
		{"pas-claim-update", "request", "pas.request.bundle", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}}]}`},
		{"pas-claim-inquire", "request", "pas.request.bundle", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}}]}`},
		{"pas-claim", "response", "pas.response.bundle", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`},
		{"pas-claim-update", "response", "pas.response.bundle", `{"resourceType":"Bundle","entry":[]}`},
		{"pas-claim-inquire", "response", "pas.inquiry.response", `{"resourceType":"Parameters"}`},
		{"pas-claim-inquire", "response", "pas.inquiry.return", `{"resourceType":"Parameters","parameter":[{"name":"future","resource":{"resourceType":"Bundle"}}]}`},
	} {
		t.Run(row.leg+"/"+row.direction+"/"+row.rule, func(t *testing.T) {
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				t.Run(level.String(), func(t *testing.T) {
					var checkerCalls atomic.Int32
					g := newObservationGateway(t, level, observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
						checkerCalls.Add(1)
						panic("unknown PAS must not be passed to a known checker")
					}))
					in := CheckInput{Direction: row.direction, Status: 200, Body: []byte(row.body), DeclaredVersion: "pa.pas@9.9", Exchange: ExchangeContext{legType: row.leg, policy: NewConformancePolicy(level), contractVersion: "pa.pas@9.9", holder: "provider", subjectPCI: "pci", correlationID: "future-schema"}}
					found := false
					for _, rule := range StructuralRules() {
						if rule.ID == row.rule {
							found = true
							if !rule.Applies(in) {
								t.Fatal("rule not applicable")
							}
							got := rule.Check(context.Background(), in)
							if got.State != CheckUnavailable || got.Code != row.rule || got.Severity != "error" {
								t.Fatalf("rule=%+v", got)
							}
						}
					}
					if !found {
						t.Fatal("missing registered rule")
					}
					err := g.enforceContent(context.Background(), in)
					if level >= EnforcementBasic {
						first := row.rule
						if first == "pas.inquiry.return" {
							first = "pas.inquiry.response"
						}
						wantStructuralError(t, err, 503, first)
					} else if err != nil {
						t.Fatal(err)
					}
					observationFlush(t, g)
					findings, drops := g.ConformanceObservationsForTest()
					if drops != 0 || checkerCalls.Load() != 0 {
						t.Fatalf("drops=%d checker=%d", drops, checkerCalls.Load())
					}
					if level == EnforcementNone && (len(findings) != 0 || g.certification != nil) {
						t.Fatalf("none did optional work: %+v", findings)
					}
					if level == EnforcementObserve {
						seen := 0
						for _, f := range findings {
							if f.Rule == row.rule {
								seen++
								if f.State != CheckUnavailable || f.Action != "not_enforced" || f.Direction != row.direction || f.PayloadSHA256 != sha256hex(in.Body) {
									t.Fatalf("finding=%+v", f)
								}
							}
						}
						if seen != 1 {
							t.Fatalf("matching findings=%d: %+v", seen, findings)
						}
					}
					for _, status := range []int{302, 400, 422, 503} {
						in.Status = status
						if in.Direction == "response" {
							if err := g.enforceContent(context.Background(), in); err != nil {
								t.Fatalf("application status%d: %v", status, err)
							}
						}
					}
				})
			}
		})
	}
}

func TestUnknownPASGenericShapePrecedence(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementBasic, EnforcementStrict} {
		for _, row := range []struct {
			body, rule string
			status     int
		}{
			{`{"resourceType":`, "json.syntax", 422},
			{`{"a":1,"a":2}`, "json.duplicate_key", 422},
			{`[]`, "json.object", 422},
			{`{"resourceType":"OperationOutcome","issue":{}}`, "fhir.operation-outcome", 502},
		} {
			t.Run(fmt.Sprintf("%s/%s", level, row.rule), func(t *testing.T) {
				in := structuralInput("pas-claim-inquire", "", "request", row.body)
				in.DeclaredVersion = "pa.pas@9.9"
				in.Exchange.policy = NewConformancePolicy(level)
				if row.rule == "fhir.operation-outcome" {
					in.Direction = "response"
				}
				wantStructuralError(t, (&Gateway{}).enforceContent(context.Background(), in), row.status, row.rule)
			})
		}
	}
}
