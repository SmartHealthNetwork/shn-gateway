//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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

// withFacilityRecords edits the facility's records Bundle in a federated
// answer: change gets the Bundle's entries and returns them.
func withFacilityRecords(t *testing.T, payload []byte, change func([]any) []any) []byte {
	t.Helper()
	var task map[string]any
	if err := json.Unmarshal(payload, &task); err != nil {
		t.Fatal(err)
	}
	contained, _ := task["contained"].([]any)
	for _, c := range contained {
		if res, _ := c.(map[string]any); res["resourceType"] == "Bundle" {
			entries, _ := res["entry"].([]any)
			res["entry"] = change(entries)
			return mustJSON(t, task)
		}
	}
	t.Fatal("the facility answer holds no records Bundle")
	return nil
}

// facilityEntry returns the resource of type rt among a records Bundle's
// entries.
func facilityEntry(t *testing.T, entries []any, rt string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if res, _ := e.(map[string]any)["resource"].(map[string]any); res["resourceType"] == rt {
			return res
		}
	}
	t.Fatalf("the records Bundle holds no %s", rt)
	return nil
}

// facilitySubject gives the facility's report subject, and, when the subject
// names the facility's own Patient, adds that Patient, carried with the member
// identifier, to the records Bundle.
func facilitySubject(t *testing.T, subject map[string]any, signedProvenance bool) func(int, []byte) []byte {
	return func(n int, p []byte) []byte {
		if n != 1 {
			return p
		}
		return withFacilityRecords(t, p, func(entries []any) []any {
			facilityEntry(t, entries, "DiagnosticReport")["subject"] = subject
			if signedProvenance {
				facilityEntry(t, entries, "Provenance")["signature"] = []any{map[string]any{
					"type": []any{map[string]any{"system": "urn:iso-astm:E1762-95:2013", "code": "1.2.840.10065.1.12.1.1"}},
					"when": "2026-01-01T00:00:00Z", "who": map[string]any{"display": "a clinician"},
				}}
			}
			if subject["reference"] == "Patient/fac-77" {
				entries = append(entries, map[string]any{"fullUrl": "urn:shn:fedquery:fac-77", "resource": map[string]any{
					"resourceType": "Patient", "id": "fac-77",
					"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-UC05"}},
				}})
			}
			return entries
		})
	}
}

// sentReportSubject is the subject of the DiagnosticReport in the claim
// update the payer received.
func sentReportSubject(t *testing.T, stub *pendResumeSubstrate) map[string]any {
	t.Helper()
	sent := stub.requests["pas-claim-update"]
	if len(sent) != 1 {
		t.Fatalf("%d claim updates sent, want 1", len(sent))
	}
	var b struct {
		Entry []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(sent[0], &b); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.Entry {
		if e.Resource["resourceType"] == "DiagnosticReport" {
			s, _ := e.Resource["subject"].(map[string]any)
			return s
		}
	}
	t.Fatal("the claim update carries no DiagnosticReport")
	return nil
}

// End to end, registered edit E-14: the claim update carries the facility's
// report naming the Claim's patient by subject.reference alone, the rest of
// the subject as the facility wrote it, and the update's sealed leg names
// E-14. A report already naming the Claim's patient, or naming the member by
// identifier alone, reaches the payer as the facility wrote its subject, and
// names nothing. A report about anyone else is refused before any edit
// (TestHandleUC05_AnotherPatientsReportAsEvidenceIsAFidelityRefusal).
func TestHandleUC05_FacilityReportSubjectE14(t *testing.T) {
	const route = "POST /scenario/uc05"
	kept := map[string]any{"type": "Patient", "identifier": map[string]any{"system": "urn:fac:mrn", "value": "77"}, "display": "L. Johansson"}
	with := func(ref string) map[string]any {
		m := map[string]any{"reference": ref}
		for k, v := range kept {
			m[k] = v
		}
		return m
	}
	byIdentifier := map[string]any{"identifier": map[string]any{"system": shnsdk.MemberSystem, "value": "MBR-UC05"}, "display": "L. Johansson"}
	for _, row := range []struct {
		name          string
		subject, want map[string]any
		edits         []string
	}{
		{"the facility's own Patient: subject.reference re-pointed alone", with("Patient/fac-77"), with("Patient/MBR-UC05"), []string{"E-14"}},
		{"control: already the Claim's patient", with("Patient/MBR-UC05"), with("Patient/MBR-UC05"), nil},
		{"control: the member by identifier alone", byIdentifier, byIdentifier, nil},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				gw, stub := uc05Fixture(t, level, facilitySubject(t, row.subject, false))
				var events capturedEvents
				gw.cfg.Diagnostic = events.sink
				rec, _ := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
				if rec.Code != http.StatusOK {
					t.Fatalf("uc05 = %d %s legs %v, want 200 with the claim update sent", rec.Code, rec.Body, stub.legTypes)
				}
				if got := sentReportSubject(t, stub); !reflect.DeepEqual(got, row.want) {
					t.Fatalf("the payer received subject %v, want %v", got, row.want)
				}
				var update capturedEvents
				for _, e := range events.of("leg.sealed") {
					if e.LegType == "pas-claim-update" {
						update.events = append(update.events, e)
					}
				}
				wantEventEdits(t, &update, "leg.sealed", row.edits)
			})
		}
	}
}

// A signature over the facility's report, here the facility's Provenance
// that attributes it, refuses E-14 at every level: 422 signed content cannot
// be edited, the fidelity of SHN's own edit at the build, and no claim update
// is sent. Its control: the same signed report already naming the Claim's
// patient needs no edit and is carried.
func TestHandleUC05_SignedFacilityReportRefusesE14(t *testing.T) {
	const route = "POST /scenario/uc05"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			gw, stub := uc05Fixture(t, level, facilitySubject(t, map[string]any{"reference": "Patient/fac-77"}, true))
			rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
			body := rec.Body.String()
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "signed content cannot be edited (E-14, Provenance.signature)") ||
				!strings.Contains(body, "federated-query legs this request sent were carried and stand.") {
				t.Fatalf("status %d: %s; want 422 signed content cannot be edited (E-14, Provenance.signature), stating the legs carried", rec.Code, body)
			}
			if legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("a claim update was sent (legs: %v)", stub.legTypes)
			}
			r := onlyRecord(t, recs)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		})
		t.Run("control: already the Claim's patient/"+level.String(), func(t *testing.T) {
			gw, stub := uc05Fixture(t, level, facilitySubject(t, map[string]any{"reference": "Patient/MBR-UC05"}, true))
			rec, _ := serveRecorded(t, gw, route, testRouteBody(gw, "/scenario/uc05", ""))
			if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim-update") {
				t.Fatalf("uc05 = %d %s, want 200 with the claim update sent", rec.Code, rec.Body)
			}
		})
	}
}
