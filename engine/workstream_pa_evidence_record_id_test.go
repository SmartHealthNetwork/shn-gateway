//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// recordIDInner is a fixture system of record recordIDSoR stands in front of:
// the authored-QR one, or the order-dispatch one.
type recordIDInner interface {
	SystemOfRecord
	SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error)
}

// recordIDSoR is a provider-data fixture system of record holding the
// member's Patient under recordID rather than the member id, as a real EHR
// keys its records: every record it answers about the member references
// Patient/<recordID>, and the member's Patient has that id. It also holds the
// clinical records an order names (evidence), as the system keeps them.
type recordIDSoR struct {
	recordIDInner
	member, recordID string
	demo             Demo
	evidence         map[string][]byte
	selfSubscriber   bool
}

func renameRecordID(v any, member, recordID string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && k == "reference" && s == "Patient/"+member {
				t[k] = "Patient/" + recordID
				continue
			}
			t[k] = renameRecordID(val, member, recordID)
		}
		if t["resourceType"] == "Patient" && t["id"] == member {
			t["id"] = recordID
		}
	case []any:
		for i := range t {
			t[i] = renameRecordID(t[i], member, recordID)
		}
	}
	return v
}

func (s *recordIDSoR) held(b []byte) []byte {
	if s.recordID == s.member {
		return b
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.Marshal(renameRecordID(v, s.member, s.recordID))
	if err != nil {
		return b
	}
	return out
}

func (s *recordIDSoR) PatientFHIRRef(m string) (string, bool) {
	if m == s.member {
		return "Patient/" + s.recordID, true
	}
	return s.recordIDInner.PatientFHIRRef(m)
}

func (s *recordIDSoR) OpenOrder(m string) ([]byte, bool) {
	b, ok := s.recordIDInner.OpenOrder(m)
	if !ok {
		return b, ok
	}
	return s.held(b), ok
}

func (s *recordIDSoR) OpenCoverage(m string) ([]byte, bool) {
	b, ok := s.recordIDInner.OpenCoverage(m)
	if !ok || m != s.member {
		return b, ok
	}
	if s.selfSubscriber {
		var c map[string]any
		if json.Unmarshal(b, &c) == nil {
			c["subscriber"] = map[string]any{"reference": "Patient/" + s.member}
			if out, err := json.Marshal(c); err == nil {
				b = out
			}
		}
	}
	return s.held(b), ok
}

func (s *recordIDSoR) ResolveByReference(ref string) ([]byte, bool) {
	if b, ok := s.evidence[ref]; ok {
		return b, true
	}
	if ref == "Patient/"+s.recordID && s.recordID != s.member {
		b, err := json.Marshal(map[string]any{
			"resourceType": "Patient", "id": s.recordID,
			"identifier": []map[string]string{{"system": shnsdk.MemberSystem, "value": s.member}},
			"name":       []map[string]string{{"family": s.demo.FamilyName}},
			"birthDate":  s.demo.BirthDate,
		})
		return b, err == nil
	}
	b, ok := s.recordIDInner.ResolveByReference(ref)
	if !ok {
		return b, ok
	}
	return s.held(b), ok
}

func (s *recordIDSoR) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	if id == s.recordID {
		id = s.member
	}
	res, err := s.recordIDInner.SearchPatientContext(ctx, resourceType, id, dates...)
	if err != nil {
		return res, err
	}
	out := SearchResult{Total: res.Total}
	for _, p := range res.Pages {
		held := s.held(p)
		parsed, err := ParseSearchPage(held, resourceType)
		if err != nil {
			return SearchResult{}, err
		}
		out.Pages = append(out.Pages, held)
		out.Entries = append(out.Entries, parsed.Entries...)
	}
	return out, nil
}

// evidenceCondition is the Condition the order names as its reason, as the
// system holds it: about subject.
func evidenceCondition(subject string) []byte {
	b, _ := json.Marshal(map[string]any{
		"resourceType": "Condition", "id": "cond-reason",
		"code":    map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "J44.9"}}},
		"subject": map[string]any{"reference": subject},
	})
	return b
}

// signedSuffix marks a record signed: a Signature in the record itself,
// outside any resource it contains, which covers its subject.
const signedSuffix = `,"modifierExtension":[{"url":"urn:x","valueSignature":{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-01-01T00:00:00Z","who":{"display":"a clinician"}}}]}`

// signedCondition is evidenceCondition(subject) carrying that signature.
func signedCondition(subject string) []byte {
	b := evidenceCondition(subject)
	return append(b[:len(b)-1], signedSuffix...)
}

// withReason is order naming the evidence Condition as its reason.
func withReason(t *testing.T, order []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(order, &m); err != nil {
		t.Fatal(err)
	}
	m["reasonReference"] = []any{map[string]any{"reference": "Condition/cond-reason"}}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// useRecordIDSoR puts g's authored-QR system behind recordIDSoR, in the
// Coverage shape named (the member's own, the member their own subscriber, or
// a dependent's naming the parent), holding the evidence Condition about
// conditionSubject.
func useRecordIDSoR(t *testing.T, g *Gateway, member, recordID string, demo Demo, shape, conditionSubject string) {
	t.Helper()
	useRecordIDSoRHolding(t, g, member, recordID, demo, shape, evidenceCondition(conditionSubject))
}

// useRecordIDSoRHolding is useRecordIDSoR holding condition as the evidence.
func useRecordIDSoRHolding(t *testing.T, g *Gateway, member, recordID string, demo Demo, shape string, condition []byte) {
	t.Helper()
	inner, ok := g.cfg.SoR.(recordIDInner)
	if !ok {
		t.Fatalf("fixture SoR is %T", g.cfg.SoR)
	}
	var sor SystemOfRecord = &recordIDSoR{recordIDInner: inner, member: member, recordID: recordID, demo: demo,
		evidence: map[string][]byte{"Condition/cond-reason": condition}, selfSubscriber: shape == "self-subscriber"}
	if shape == "dependent" {
		sor = newDependentCoverageSoR(sor, parentFound)
	}
	g.cfg.SoR = sor
}

// carriesEvidenceFor fails unless the PAS request carries the evidence
// Condition, about the request's own patient (Patient/<member>).
func carriesEvidenceFor(t *testing.T, what string, bundle []byte, member string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(bundle, &doc); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	entries, _ := doc["entry"].([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		r, _ := em["resource"].(map[string]any)
		if r["resourceType"] == "Condition" && r["id"] == "cond-reason" {
			if got := slotReference(r, "subject"); got != "Patient/"+member {
				t.Fatalf("%s: the evidence names %q, want the request's patient Patient/%s", what, got, member)
			}
			return
		}
	}
	t.Fatalf("%s carries no evidence Condition: %.600s", what, bundle)
}

// slotReference is the literal reference r's slot names, or "".
func slotReference(r map[string]any, slot string) string {
	m, _ := r[slot].(map[string]any)
	s, _ := m["reference"].(string)
	return s
}

var evidenceShapes = []string{"member's own Coverage", "self-subscriber", "dependent"}

// On the provider-data lane the PAS request names the patient by member, and
// the clinical records it references are read from the participant's system,
// which may hold the member's Patient under a record id of its own. A record
// about that record id is the request's patient's: it is named by member, as
// the build names the order (namePatientByMember), and rides the submission,
// in each Coverage shape and at every level. The submission is still the
// builder's own (authored by sdk-pas-submit), and its record names E-06, the
// edit made to the record it carries. Its control: the system holds the
// Patient under the member id, and nothing is edited or named.
func TestProviderData_EvidenceUnderTheMembersRecordIDRides(t *testing.T) {
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	for _, shape := range evidenceShapes {
		for _, recordID := range []string{"pat-77", member} {
			for _, level := range allLevels {
				t.Run(shape+"/record "+recordID+"/"+level.String(), func(t *testing.T) {
					order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx))
					g, stub, sent := newAuthoredQRSingleShotFixture(t, member, demo, order)
					mountOriginationRoutes(g)
					g.cfg.ConformanceEnforcement = level
					useRecordIDSoR(t, g, member, recordID, demo, shape, "Patient/"+recordID)
					var events capturedEvents
					g.cfg.Diagnostic = events.sink
					rec, recs := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
					if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim") {
						t.Fatalf("POST /scenario/uc04 = %d %s legs %v, want 200 with the PAS claim sent", rec.Code, rec.Body, stub.legTypes)
					}
					claims := sent.get("pas-claim")
					if len(claims) == 0 {
						t.Fatal("no PAS claim captured")
					}
					carriesEvidenceFor(t, "the PAS claim", claims[len(claims)-1], member)
					r := onlyRecord(t, recs)
					if r.Outcome != ExchangeAnswered {
						t.Fatalf("recorded %s on %q, want the payer's answer", r.Outcome, r.Rule)
					}
					var want []string
					if recordID != member {
						want = []string{"E-06"}
					}
					if r.Ownership != "authored" || r.Builder != string(relay.BuilderSDKPASSubmit) || !slices.Equal(r.RequestEdits, want) || !slices.Equal(r.Edits, want) {
						t.Fatalf("recorded ownership %q builder %q request edits %v edits %v, want authored by sdk-pas-submit naming %v",
							r.Ownership, r.Builder, r.RequestEdits, r.Edits, want)
					}
					// The sealed leg's event names them too.
					var sealed capturedEvents
					for _, e := range events.of("leg.sealed") {
						if e.LegType == "pas-claim" {
							sealed.events = append(sealed.events, e)
						}
					}
					wantEventEdits(t, &sealed, "leg.sealed", want)
				})
			}
		}
	}
}

// The order-dispatch route (originateDispatch, the PAS tail) reads its
// evidence the same way: with the member's Patient under a record id of its
// own, the dispatch's PAS claim is built and sent, at every level, where it
// was refused 502 before; its control, the member id.
func TestProviderData_EvidenceUnderTheMembersRecordIDRidesTheDispatch(t *testing.T) {
	const member = "MBR-PD-UC03"
	const performerRef = "Organization/org-dme-uc03"
	demo := Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}
	for _, recordID := range []string{"pat-55", member} {
		for _, level := range allLevels {
			t.Run("record "+recordID+"/"+level.String(), func(t *testing.T) {
				order, err := buildDispatchDeviceRequest("dr-uc03", "Patient/"+member, performerRef, "E1390", "Stationary oxygen concentrator", "J96.10")
				if err != nil {
					t.Fatal(err)
				}
				order = withReason(t, order)
				supplier, err := buildHomeOxygenSupplier("org-dme-uc03")
				if err != nil {
					t.Fatal(err)
				}
				fix := newDispatchFixture(t, member, demo, order, performerRef, supplier)
				fix.gw.cfg.ConformanceEnforcement = level
				useRecordIDSoR(t, fix.gw, member, recordID, demo, "member's own Coverage", "Patient/"+recordID)
				rec := httptest.NewRecorder()
				fix.gw.handleDispatch(rec, httptest.NewRequest(http.MethodPost, "/scenario/dispatch", strings.NewReader(testRouteBody(fix.gw, "/scenario/dispatch", `"member":"`+member+`"`))))
				if rec.Code != http.StatusOK || !legAttempted(fix.stub.legTypes, "pas-claim") {
					t.Fatalf("dispatch = %d %s legs %v, want 200 with the PAS claim sent", rec.Code, rec.Body, fix.stub.legTypes)
				}
			})
		}
	}
}

// The pend keeps the member's record id beside the member, so the resume
// ClaimUpdate's evidence is read the way the submission's was: the
// submission the pend sent carries the record about that id.
func TestProviderData_ThePendKeepsTheMembersRecordID(t *testing.T) {
	const member = "MBR-UC06"
	demo := Demo{BirthDate: "1969-07-21", FamilyName: "Reyes"}
	uc06 := DemoOrderCodes().UC06
	for _, recordID := range []string{"pat-66", member} {
		t.Run("record "+recordID, func(t *testing.T) {
			order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc06-evidence", "Patient/"+member, uc06.System, uc06.Code, uc06.Display, uc06.Dx))
			gw, _, capture := newAuthoredQRPendFixture(t, member, demo, order, "functional-status")
			useRecordIDSoR(t, gw, member, recordID, demo, "member's own Coverage", "Patient/"+recordID)
			rec := httptest.NewRecorder()
			st, ok := gw.scenarioToPend(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc06", strings.NewReader(testOrderBody(order, `"member":"`+member+`"`))), "uc06", member)
			if !ok {
				t.Fatalf("scenarioToPend = %d %s", rec.Code, rec.Body)
			}
			if st.sorID != recordID || st.member != member {
				t.Fatalf("the pend keeps record %q for %q, want %q for %q", st.sorID, st.member, recordID, member)
			}
			subs := capture.get("pas-claim")
			if len(subs) == 0 {
				t.Fatal("no PAS claim captured")
			}
			carriesEvidenceFor(t, "the PAS claim", subs[len(subs)-1], member)
		})
	}
}

// The completion reads the update's evidence as it reads the submission's:
// built from the same order, each completes with the record about the
// member's record id named by member, and each refuses it without the record
// id (the gap this closes) and a record about another patient. Each names
// E-06 for its seal only when it re-pointed a record (relay.InputEdited); a
// record already naming the member names nothing.
func TestCompleteAuthoredPASRequest_EvidenceUnderTheMembersRecordID(t *testing.T) {
	t.Parallel()
	in := guardInputs(t, contextQR)
	member := in.MemberID
	in.SR = withReason(t, in.SR)
	update := attachmentUpdateInputs("2.2", true, false)
	update.QR, update.SR = in.QR, in.SR
	for name, build := range map[string]func() ([]byte, error){
		"submit": func() ([]byte, error) { return buildModuleOnePASSubmit("2.2", in, nil) },
		"update": func() ([]byte, error) { return buildModuleOnePASUpdate("2.2", update, nil) },
	} {
		for _, row := range []struct {
			name, subject, sorID string
			ok                   bool
			edits                []relay.EditID
		}{
			{"about the member's record id", "Patient/pat-ox", "pat-ox", true, []relay.EditID{relay.EditEvidenceSubjectRekey}},
			{"the record id unknown", "Patient/pat-ox", "", false, nil},
			{"about another patient", "Patient/someone-else", "pat-ox", false, nil},
			{"control: the system holds the member under the member id", "Patient/" + member, member, true, nil},
		} {
			t.Run(name+"/"+row.name, func(t *testing.T) {
				sor := requesterSoR(map[string]string{"Condition/cond-reason": string(evidenceCondition(row.subject))})
				g := &Gateway{cfg: Config{SoR: sor, OriginationProfile: "provider-data"}}
				body, err := build()
				if err != nil {
					t.Fatal(err)
				}
				out, edits, err := g.completeAuthoredPASRequest(context.Background(), body, in.QR, in.SR, in.CoverageRef, row.sorID, member)
				if (err == nil) != row.ok {
					t.Fatalf("completion err = %v, want ok %v", err, row.ok)
				}
				if !slices.Equal(edits, row.edits) {
					t.Fatalf("completion names input edits %v, want %v", edits, row.edits)
				}
				if row.ok {
					carriesEvidenceFor(t, name, out, member)
				}
			})
		}
	}
}

// REJECTION, at every level: a clinical record the order references is
// carried only when the system holds the member under the member id and the
// record names it, or holds the member under a record id of its own and the
// record names exactly that record id (registered edit E-06 re-points it).
// Anything else is refused by the completion, 502 PAS evidence linkage failed,
// after the CRD and questionnaire legs, which stand, recorded as the provider's
// gateway's own refusal, and no PAS claim is sent:
//   - another patient, whichever id the system holds the member under;
//   - under a record id of its own, the member id itself (in that system's
//     own numbering, another record), a versioned reference to the record id,
//     and an absolute one.
//
// A record E-06 would re-point that a signature covers is refused as the
// supplemental report is (D113): 422 signed content cannot be edited,
// recorded as the fidelity of SHN's own edit at the build.
func TestProviderData_EvidenceAboutAnotherPatientIsRefused(t *testing.T) {
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	for _, row := range []struct {
		name, recordID string
		condition      []byte
		status         int
		body           string
	}{
		{"another patient, record id differs", "pat-77", evidenceCondition("Patient/someone-else"), http.StatusBadGateway, "PAS evidence linkage failed"},
		{"another patient, record id is the member id", member, evidenceCondition("Patient/someone-else"), http.StatusBadGateway, "PAS evidence linkage failed"},
		{"the member id under a record id of its own", "pat-77", evidenceCondition("Patient/" + member), http.StatusBadGateway, "PAS evidence linkage failed"},
		{"a versioned reference to the record id", "pat-77", evidenceCondition("Patient/pat-77/_history/2"), http.StatusBadGateway, "PAS evidence linkage failed"},
		{"an absolute reference to the record id", "pat-77", evidenceCondition("https://ehr.example/fhir/Patient/pat-77"), http.StatusBadGateway, "PAS evidence linkage failed"},
		{"signed, about the record id", "pat-77", signedCondition("Patient/pat-77"), http.StatusUnprocessableEntity, "signed content cannot be edited (E-06, Signature in Condition)"},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx))
				g, stub, _ := newAuthoredQRSingleShotFixture(t, member, demo, order)
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				useRecordIDSoRHolding(t, g, member, row.recordID, demo, "member's own Coverage", row.condition)
				rec, recs := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.body) {
					t.Fatalf("POST /scenario/uc04 = %d %s, want %d %s", rec.Code, rec.Body, row.status, row.body)
				}
				if legAttempted(stub.legTypes, "pas-claim") || !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
					t.Fatalf("legs %v: want the questionnaire fetched and no PAS claim", stub.legTypes)
				}
				wantCarried(t, rec.Body.String(), stub.legTypes...)
				r := onlyRecord(t, recs)
				if row.status == http.StatusUnprocessableEntity {
					// Signed content: the fidelity of SHN's own edit, at the build.
					wantRefusal(t, r, row.status, RefusedByProviderGateway, RefusalFidelity)
					if r.RefusalStage != vocab.RefusalStageBuild {
						t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
					}
					return
				}
				// The provider's gateway's own refusal of the request it built,
				// under no named rule, as the completion records it today.
				wantRefusal(t, r, row.status, RefusedByProviderGateway, ExchangeOther)
			})
		}
	}
}

// evidenceReadByMember re-points, by E-06, a record whose subject is exactly
// the member's record id, answers any other record as not held, refuses a
// signed one, and edits nothing when the record id is unknown or is the
// member id. It notes E-06 only for the record it re-pointed.
func TestEvidenceReadByMember(t *testing.T) {
	held := map[string][]byte{
		"Condition/mine":      evidenceCondition("Patient/pat-77"),
		"Condition/theirs":    evidenceCondition("Patient/someone-else"),
		"Condition/member-id": evidenceCondition("Patient/MBR-X"),
		"Condition/versioned": evidenceCondition("Patient/pat-77/_history/2"),
		"Condition/absolute":  evidenceCondition("https://ehr.example/fhir/Patient/pat-77"),
		"Condition/signed":    signedCondition("Patient/pat-77"),
	}
	read := func(_ context.Context, ref string) ([]byte, bool, error) {
		b, ok := held[ref]
		return b, ok, nil
	}
	subject := func(raw []byte) string {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return slotReference(m, "subject")
	}
	for _, row := range []struct {
		name, sorID, ref string
		found            bool
		want             string
	}{
		{"about the member's record id", "pat-77", "Condition/mine", true, "Patient/MBR-X"},
		{"about someone else", "pat-77", "Condition/theirs", false, ""},
		{"the member id under a record id of its own", "pat-77", "Condition/member-id", false, ""},
		{"a versioned reference", "pat-77", "Condition/versioned", false, ""},
		{"an absolute reference", "pat-77", "Condition/absolute", false, ""},
		{"not held", "pat-77", "Condition/none", false, ""},
		{"record id unknown", "", "Condition/mine", true, "Patient/pat-77"},
		{"record id is the member id", "MBR-X", "Condition/mine", true, "Patient/pat-77"},
		{"signed, record id is the member id", "MBR-X", "Condition/signed", true, "Patient/pat-77"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var note inputEditNote
			raw, found, err := evidenceReadByMember(read, row.sorID, "MBR-X", &note)(context.Background(), row.ref)
			if err != nil || found != row.found {
				t.Fatalf("read: found %v err %v, want found %v", found, err, row.found)
			}
			var wantNoted []relay.EditID
			if row.sorID == "pat-77" && found {
				wantNoted = []relay.EditID{relay.EditEvidenceSubjectRekey}
			}
			if got := note.list(); !slices.Equal(got, wantNoted) {
				t.Fatalf("noted %v, want %v", got, wantNoted)
			}
			if found && subject(raw) != row.want {
				t.Fatalf("subject %q, want %q", subject(raw), row.want)
			}
			if row.sorID == "pat-77" && found {
				// E-06 changes the subject alone.
				if want := bytes.Replace(held[row.ref], []byte(`"Patient/pat-77"`), []byte(`"Patient/MBR-X"`), 1); !bytes.Equal(raw, want) {
					t.Fatalf("re-pointed record\n%s\nwant only its subject changed\n%s", raw, want)
				}
			}
		})
	}
	var note inputEditNote
	_, found, err := evidenceReadByMember(read, "pat-77", "MBR-X", &note)(context.Background(), "Condition/signed")
	if got := note.list(); got != nil {
		t.Fatalf("a refused signed record noted %v, want nothing", got)
	}
	var signed *relay.SignedContentError
	if found || !errors.As(err, &signed) || signed.Edit != relay.EditEvidenceSubjectRekey {
		t.Fatalf("a signed record: found %v err %v, want E-06's signed-content refusal", found, err)
	}
}

// E-06 re-points each evidence type's root subject.reference alone, admitted
// by the evidence check as E-06 and nothing else; a record contained in
// another is not edited.
func TestRekeyEvidenceSubject_EachEvidenceType(t *testing.T) {
	for _, rt := range []string{"Condition", "Observation", "ClinicalImpression", "Goal", "DiagnosticReport"} {
		t.Run(rt, func(t *testing.T) {
			held := []byte(`{"resourceType":"` + rt + `","id":"r1","status":"final","contained":[{"resourceType":"Observation","id":"c1","subject":{"reference":"Patient/pat-77"}}],"subject":{"reference":"Patient/pat-77"}}`)
			out, _, err := rekeyEvidenceSubject(held, "Patient/pat-77", "MBR-X")
			if err != nil {
				t.Fatal(err)
			}
			want := []byte(`{"resourceType":"` + rt + `","id":"r1","status":"final","contained":[{"resourceType":"Observation","id":"c1","subject":{"reference":"Patient/pat-77"}}],"subject":{"reference":"Patient/MBR-X"}}`)
			if !bytes.Equal(out, want) {
				t.Fatalf("re-pointed\n%s\nwant\n%s", out, want)
			}
			body := relay.NewBody(held, relay.OriginUpstreamResponse)
			doc, err := relay.Doc(body)
			if err != nil {
				t.Fatal(err)
			}
			subject, _ := doc.Member(doc.Root(), "subject")
			ref, _ := doc.Member(subject, "reference")
			p, err := relay.Apply(body, "application/fhir+json", relay.EditEvidenceSubjectRekey, doc.Replace(ref, jsonString("Patient/MBR-X")))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(p.Edits(), []relay.EditID{relay.EditEvidenceSubjectRekey}) || supplementalReportCheck(p) != nil {
				t.Fatalf("edits %v: want E-06 alone, admitted", p.Edits())
			}
			other, err := relay.Apply(body, "application/fhir+json", relay.EditPASPendReturnRekey, doc.Replace(ref, jsonString("Patient/MBR-X")))
			if err != nil {
				t.Fatal(err)
			}
			if supplementalReportCheck(other) == nil {
				t.Fatal("the evidence check admitted another edit")
			}
		})
	}
}
