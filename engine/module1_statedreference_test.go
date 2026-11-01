//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// statedReference is a claim fact that states a reference no amendment's bundle
// can carry, on the request body m.
func statedReference(m map[string]json.RawMessage) {
	var f map[string]json.RawMessage
	_ = json.Unmarshal(m["claimFacts"], &f)
	var coding map[string]json.RawMessage
	_ = json.Unmarshal(f["requestType"], &coding)
	coding["extension"] = json.RawMessage(`[{"url":"http://example.org/stated","valueReference":{"reference":"Observation/stated"}}]`)
	f["requestType"], _ = json.Marshal(coding)
	m["claimFacts"], _ = json.Marshal(f)
}

func statedReferenceOpts(member, birthDate, familyName, pendedItem, line string, extraRoles map[string]string) pendFixtureOpts {
	opts := pendFixtureOpts{member: member, birthDate: birthDate, familyName: familyName, pendedItem: pendedItem, extraRoles: extraRoles,
		declared: []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}}
	if line == "2.2" {
		opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
	}
	return opts
}

func statedReferenceGateway(g *Gateway, line string, level ConformanceEnforcement) {
	declareOriginationProfile(g, "demo")
	g.cfg.Validator = NewLineFakeValidator("2.0")
	g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)})
	g.cfg.ConformanceEnforcement = level
}

// A stated reference no amendment can carry stops an origination only where the
// amendment is certain. UC-05's consented branch ends, when the Authorization
// Framework denies the disclosure, with the pend and no amendment: a 200, so its
// submission is carried as stated, at every level.
func TestStatedReference_UC05ConsentDeniedIsCarried(t *testing.T) {
	t.Parallel()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, line := range []string{"2.1", "2.2"} {
			t.Run(fmt.Sprintf("%s/%s", level, line), func(t *testing.T) {
				g, stub := newPendResumeFixture(t, statedReferenceOpts("MBR-D-UC05", "1968-03-12", "Johansson-Demo", "operative-diagnostic-report", line,
					map[string]string{"facility": "metro-spine", "phg": "phg"}))
				statedReferenceGateway(g, line, level)
				mountOriginationRoutes(g)
				stub.denyOperation = "federated-query-submit"
				rec, _ := serveRecorded(t, g, "POST /scenario/uc05", editBody(t, testRouteBody(g, "/scenario/uc05", ""), statedReference))
				var got uc05Resp
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.ConsentDenied {
					t.Fatalf("%d %s, want 200 consentDenied", rec.Code, rec.Body.String())
				}
				sent := stub.requests["pas-claim"]
				if len(sent) != 1 || !bytes.Contains(sent[0], []byte("Observation/stated")) {
					t.Fatalf("%d submissions, want one carrying the statement", len(sent))
				}
				if n := len(stub.requests["pas-claim-update"]); n != 0 {
					t.Fatalf("%d amendments, want none", n)
				}
			})
		}
	}
}

// An amendment's own statement is no prior Claim's: a two-phase /complete that
// states a reference over a clean submission is carried as stated, at every level.
func TestStatedReference_AnAmendmentsOwnStatementIsCarried(t *testing.T) {
	t.Parallel()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		for _, line := range []string{"2.1", "2.2"} {
			t.Run(fmt.Sprintf("%s/%s", level, line), func(t *testing.T) {
				g, stub := newPendResumeFixture(t, statedReferenceOpts("MBR-D-UC07", "1985-10-23", "Kowalczyk", "patient-reported-functional-status", line,
					map[string]string{"phg": "phg"}))
				statedReferenceGateway(g, line, level)
				start, _ := serveRecorded(t, g, "POST /scenario/uc07/start", testRouteBody(g, "/scenario/uc07/start", ""))
				var started startResp
				if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
					t.Fatalf("start %d %s", start.Code, start.Body.String())
				}
				rec, _ := serveRecorded(t, g, "POST /scenario/uc07/complete", editBody(t,
					testRouteBody(g, "/scenario/uc07/complete", `"resumeToken":"`+started.ResumeToken+`"`), statedReference))
				sent := stub.requests["pas-claim-update"]
				if rec.Code != http.StatusOK || len(sent) != 1 || !bytes.Contains(sent[0], []byte("Observation/stated")) {
					t.Fatalf("%d %s, %d amendments, want 200 and one carrying the statement", rec.Code, rec.Body.String(), len(sent))
				}
			})
		}
	}
}

// A request that can still succeed is carried: on a route that would amend, a CRD
// not-covered answer ends the request with no Claim and no amendment, at every
// level, so nothing that reads a stated reference may refuse it before that leg.
func TestStatedReference_NotCoveredIsCarried(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, path, member, birthDate, familyName, pendedItem, extra string
		roles                                                        map[string]string
	}{
		{name: "uc04", path: "/scenario/uc04", member: "MBR-D-UC04", birthDate: "1958-12-19", familyName: "Okereke", pendedItem: "operative-diagnostic-report"},
		{name: "uc06", path: "/scenario/uc06", member: "MBR-D-UC06", birthDate: "1970-05-08", familyName: "Adeyemi", pendedItem: "functional-status", extra: `"answer":"x","npi":"1234567893"`},
		{name: "uc07", path: "/scenario/uc07", member: "MBR-D-UC07", birthDate: "1985-10-23", familyName: "Kowalczyk", pendedItem: "patient-reported-functional-status", extra: `"answer":"x"`,
			roles: map[string]string{"phg": "phg"}},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			for _, line := range []string{"2.1", "2.2"} {
				t.Run(fmt.Sprintf("%s/%s/%s", row.name, level, line), func(t *testing.T) {
					g, stub := newPendResumeFixture(t, statedReferenceOpts(row.member, row.birthDate, row.familyName, row.pendedItem, line, row.roles))
					statedReferenceGateway(g, line, level)
					mountOriginationRoutes(g)
					stub.overrideResponse = func(leg string, p []byte) []byte {
						if leg == "crd-order-select" {
							return crdAnswerFor(shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth})
						}
						return p
					}
					rec, _ := serveRecorded(t, g, "POST "+row.path, editBody(t, testRouteBody(g, row.path, row.extra), statedReference))
					if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"not-covered"`)) {
						t.Fatalf("%d %s, want 200 not-covered", rec.Code, rec.Body.String())
					}
					if n := len(stub.requests["pas-claim"]) + len(stub.requests["pas-claim-update"]); n != 0 {
						t.Fatalf("%d claims sent, want none", n)
					}
				})
			}
		}
	}
}

// Where the amendment is certain, a stated reference it cannot carry is refused
// before the claim goes out: the UC-04 demo lane and the single-call UC-06 and
// UC-07 amend in the same request, so a pend the payer held would be one no
// amendment could answer. The refusal is the fidelity of SHN's own
// build, 422 at the build stage, at every level and line, with no claim and no
// amendment sent.
func TestStatedReference_RefusedBeforeTheClaimWhereTheAmendmentIsCertain(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, path, member, birthDate, familyName, pendedItem, extra string
		roles                                                        map[string]string
	}{
		{name: "uc04", path: "/scenario/uc04", member: "MBR-D-UC04", birthDate: "1958-12-19", familyName: "Okereke", pendedItem: "operative-diagnostic-report"},
		{name: "uc06", path: "/scenario/uc06", member: "MBR-D-UC06", birthDate: "1970-05-08", familyName: "Adeyemi", pendedItem: "functional-status", extra: `"answer":"x","npi":"1234567893"`},
		{name: "uc07", path: "/scenario/uc07", member: "MBR-D-UC07", birthDate: "1985-10-23", familyName: "Kowalczyk", pendedItem: "patient-reported-functional-status", extra: `"answer":"x"`,
			roles: map[string]string{"phg": "phg"}},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			for _, line := range []string{"2.1", "2.2"} {
				t.Run(fmt.Sprintf("%s/%s/%s", row.name, level, line), func(t *testing.T) {
					g, stub := newPendResumeFixture(t, statedReferenceOpts(row.member, row.birthDate, row.familyName, row.pendedItem, line, row.roles))
					statedReferenceGateway(g, line, level)
					mountOriginationRoutes(g)
					rec, recs := serveRecorded(t, g, "POST "+row.path, editBody(t, testRouteBody(g, row.path, row.extra), statedReference))
					if rec.Code != http.StatusUnprocessableEntity || !bytes.Contains(rec.Body.Bytes(), []byte("Observation/stated")) {
						t.Fatalf("%d %s, want 422 naming the reference", rec.Code, rec.Body.String())
					}
					if n := len(stub.requests["pas-claim"]) + len(stub.requests["pas-claim-update"]); n != 0 {
						t.Fatalf("%d claims sent, want none", n)
					}
					refused := false
					for _, x := range recs {
						if x.Outcome == ExchangeRefused && x.Rule == RefusalFidelity && x.RefusalStage == vocab.RefusalStageBuild {
							refused = true
						}
					}
					if !refused {
						t.Fatalf("no exchange recorded a fidelity refusal at the build stage: %+v", recs)
					}
				})
			}
		}
	}
}

// A two-phase start is carried as stated, at every level and line: it ends at
// the pend, a 200, and the amendment comes, if at all, in a later request, so
// a stated reference cannot be judged against an amendment it may never send.
func TestStatedReference_TwoPhaseStartIsCarried(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, path, member, birthDate, familyName, pendedItem string
		roles                                                 map[string]string
	}{
		{name: "uc06", path: "/scenario/uc06/start", member: "MBR-D-UC06", birthDate: "1970-05-08", familyName: "Adeyemi", pendedItem: "functional-status"},
		{name: "uc07", path: "/scenario/uc07/start", member: "MBR-D-UC07", birthDate: "1985-10-23", familyName: "Kowalczyk", pendedItem: "patient-reported-functional-status",
			roles: map[string]string{"phg": "phg"}},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			for _, line := range []string{"2.1", "2.2"} {
				t.Run(fmt.Sprintf("%s/%s/%s", row.name, level, line), func(t *testing.T) {
					g, stub := newPendResumeFixture(t, statedReferenceOpts(row.member, row.birthDate, row.familyName, row.pendedItem, line, row.roles))
					statedReferenceGateway(g, line, level)
					rec, _ := serveRecorded(t, g, "POST "+row.path, editBody(t, testRouteBody(g, row.path, ""), statedReference))
					var started startResp
					if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &started) != nil || started.ResumeToken == "" {
						t.Fatalf("%d %s, want 200 with a resume token", rec.Code, rec.Body.String())
					}
					sent := stub.requests["pas-claim"]
					if len(sent) != 1 || !bytes.Contains(sent[0], []byte("Observation/stated")) {
						t.Fatalf("%d submissions, want one carrying the statement", len(sent))
					}
				})
			}
		}
	}
}
