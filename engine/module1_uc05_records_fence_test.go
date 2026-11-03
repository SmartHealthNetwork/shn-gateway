//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The facility's federated answers on /scenario/uc05, as the requester's
// gateway reads them. The report it builds into the claim update is SHN's own
// build: one about another patient is a fidelity refusal at every level. Any
// other record about another patient is the content check patient.answer:
// refused at strict, recorded and carried below it (nothing recorded at
// none), as on the partner's own federated-query ingress.

// uc05Fixture is a provider gateway at level with UC-05's routes mounted, its
// facility answering through edit: edit(n, payload) changes the nth (from 1)
// federated-query answer.
func uc05Fixture(t *testing.T, level ConformanceEnforcement, edit func(n int, payload []byte) []byte) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC05", birthDate: "1968-03-12", familyName: "Johansson",
		pendedItem: "operative-diagnostic-report",
		extraRoles: map[string]string{"facility": "metro-spine"},
	})
	mountOriginationRoutes(gw)
	gw.cfg.ConformanceEnforcement = level
	n := 0
	stub.overrideResponse = func(legType string, payload []byte) []byte {
		if legType != "federated-query" {
			return payload
		}
		n++
		return edit(n, payload)
	}
	return gw, stub
}

// reportAboutSomeoneElse re-points the facility's report at another patient.
func reportAboutSomeoneElse(t *testing.T, payload []byte) []byte {
	t.Helper()
	out := bytes.ReplaceAll(payload, []byte(`"subject":{"reference":"Patient/MBR-UC05"}`), []byte(`"subject":{"reference":"Patient/someone-else"}`))
	if bytes.Equal(out, payload) {
		t.Fatal("the facility answer carried no report subject to replace")
	}
	return out
}

// withRecordAboutSomeoneElse adds to the facility's records Bundle an
// Observation about another patient, which no report or Provenance names.
func withRecordAboutSomeoneElse(t *testing.T, payload []byte) []byte {
	t.Helper()
	var task map[string]any
	if err := json.Unmarshal(payload, &task); err != nil {
		t.Fatal(err)
	}
	contained, _ := task["contained"].([]any)
	added := false
	for _, c := range contained {
		res, _ := c.(map[string]any)
		if res["resourceType"] != "Bundle" {
			continue
		}
		entries, _ := res["entry"].([]any)
		res["entry"] = append(entries, map[string]any{
			"fullUrl": "urn:shn:fedquery:other",
			"resource": map[string]any{
				"resourceType": "Observation", "id": "obs-other", "status": "final",
				"code":    map[string]any{"text": "lumbar range of motion"},
				"subject": map[string]any{"reference": "Patient/someone-else"},
			},
		})
		added = true
	}
	if !added {
		t.Fatal("the facility answer holds no records Bundle")
	}
	return mustJSON(t, task)
}

func patientAnswerFindings(ev *levelIngressEvents) int {
	n := 0
	for _, f := range ev.findings {
		if f.Rule == RulePatientAnswer {
			n++
		}
	}
	return n
}

// A record about another patient beside the evidence, on the report's leg or
// on the DocumentReference leg, is never built into anything: patient.answer
// refuses it at strict, and below strict the flow goes on and the claim
// update is sent, with the finding recorded (none records nothing).
func TestHandleUC05_AnotherPatientsRecordBesideTheEvidence(t *testing.T) {
	const route = "POST /scenario/uc05"
	for name, edit := range map[string]func(t *testing.T) func(int, []byte) []byte{
		"on the report's leg": func(t *testing.T) func(int, []byte) []byte {
			return func(n int, p []byte) []byte {
				if n == 1 {
					return withRecordAboutSomeoneElse(t, p)
				}
				return p
			}
		},
		"on the DocumentReference leg": func(t *testing.T) func(int, []byte) []byte {
			return func(n int, p []byte) []byte {
				if n == 2 {
					return reportAboutSomeoneElse(t, p)
				}
				return p
			}
		},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := uc05Fixture(t, level, edit(t))
				ev := observeLevelEvents(gw)
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
				if want := map[bool]int{true: 0, false: 1}[level == EnforcementNone]; patientAnswerFindings(ev) != want {
					t.Fatalf("%d %s findings, want %d", patientAnswerFindings(ev), RulePatientAnswer, want)
				}
				r := onlyRecord(t, recs)
				if level != EnforcementStrict {
					if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim-update") {
						t.Fatalf("status %d, legs %v: %s; want the flow carried through the claim update", rec.Code, stub.legTypes, rec.Body)
					}
					return
				}
				body := rec.Body.String()
				if rec.Code != http.StatusBadGateway || !strings.Contains(body, "federated response refused: ") ||
					!strings.Contains(body, "carried and stand.") || strings.Contains(body, "someone-else") {
					t.Fatalf("status %d: %s; want 502 naming no patient, stating the legs carried", rec.Code, body)
				}
				if legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("a claim update was sent after the refusal (legs: %v)", stub.legTypes)
				}
				wantRefusal(t, r, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
			})
		}
	}
}

// provenanceAboutSomeoneElse adds another patient to the targets of the
// Provenance that attributes the facility's report.
func provenanceAboutSomeoneElse(t *testing.T, payload []byte) []byte {
	t.Helper()
	out := bytes.Replace(payload, []byte(`"target":[{"reference":"DiagnosticReport/dr-uc05-operative"}]`),
		[]byte(`"target":[{"reference":"DiagnosticReport/dr-uc05-operative"},{"reference":"Patient/someone-else"}]`), 1)
	if bytes.Equal(out, payload) {
		t.Fatal("the facility answer carried no Provenance target to extend")
	}
	return out
}

// withUnindexableEntry adds to the facility's records Bundle an entry with no
// resource: the evidence's references cannot be resolved against the Bundle.
func withUnindexableEntry(t *testing.T, payload []byte) []byte {
	t.Helper()
	var task map[string]any
	if err := json.Unmarshal(payload, &task); err != nil {
		t.Fatal(err)
	}
	contained, _ := task["contained"].([]any)
	for _, c := range contained {
		if res, _ := c.(map[string]any); res["resourceType"] == "Bundle" {
			entries, _ := res["entry"].([]any)
			res["entry"] = append(entries, map[string]any{"resource": nil})
			return mustJSON(t, task)
		}
	}
	t.Fatal("the facility answer holds no records Bundle")
	return nil
}

// The report the gateway builds into the claim update, re-pointed at the
// Claim's patient, must be about that patient: one about another patient
// would have SHN state what the facility never said. A fidelity refusal of
// SHN's own build at every level (422, build stage), before the claim update,
// naming no patient and stating the legs carried. So is an answer whose
// entries cannot be indexed to resolve the evidence's references.
func TestHandleUC05_AnotherPatientsReportAsEvidenceIsAFidelityRefusal(t *testing.T) {
	const route = "POST /scenario/uc05"
	for name, edit := range map[string]func(*testing.T, []byte) []byte{
		"the report":                    reportAboutSomeoneElse,
		"the Provenance attributing it": provenanceAboutSomeoneElse,
		"an answer it cannot index":     withUnindexableEntry,
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				gw, stub := uc05Fixture(t, level, func(n int, p []byte) []byte {
					if n == 1 {
						return edit(t, p)
					}
					return p
				})
				rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
				body := rec.Body.String()
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "(fidelity of SHN's own build)") ||
					!strings.Contains(body, "federated-query legs this request sent were carried and stand.") || strings.Contains(body, "someone-else") {
					t.Fatalf("status %d: %s; want 422 fidelity naming no patient, stating the legs carried", rec.Code, body)
				}
				if legAttempted(stub.legTypes, "pas-claim-update") {
					t.Fatalf("a claim update was sent with another patient's report (legs: %v)", stub.legTypes)
				}
				r := onlyRecord(t, recs)
				wantOrigination(t, r, route, http.StatusUnprocessableEntity)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}
