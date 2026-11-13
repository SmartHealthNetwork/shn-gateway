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
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	for _, shape := range evidenceShapes {
		for _, recordID := range []string{"pat-77", member} {
			for _, level := range allLevels {
				t.Run(shape+"/record "+recordID+"/"+level.String(), func(t *testing.T) {
					t.Parallel()
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
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC03"
	const performerRef = "Organization/org-dme-uc03"
	demo := Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}
	for _, recordID := range []string{"pat-55", member} {
		for _, level := range allLevels {
			t.Run("record "+recordID+"/"+level.String(), func(t *testing.T) {
				t.Parallel()
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

// A record nested exactly as deep as the request carries it (one level less
// than the refusal row "nested past the relay's depth as carried") rides the
// submission under either record id, at every level: the gateway's limit is
// the relay's, at the record's depth in the request, and no shallower.
func TestProviderData_EvidenceAtTheRelayDepthRides(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	deep := strings.Repeat("[", 60) + strings.Repeat("]", 60)
	for _, recordID := range []string{"pat-77", member} {
		for _, level := range allLevels {
			t.Run("record "+recordID+"/"+level.String(), func(t *testing.T) {
				t.Parallel()
				order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx))
				g, stub, sent := newAuthoredQRSingleShotFixture(t, member, demo, order)
				mountOriginationRoutes(g)
				g.cfg.ConformanceEnforcement = level
				useRecordIDSoRHolding(t, g, member, recordID, demo, "member's own Coverage", heldBytes(recordID, `"code":`, `"note":`+deep+`,"code":`))
				rec, _ := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
				if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim") || len(sent.get("pas-claim")) == 0 {
					t.Fatalf("POST /scenario/uc04 = %d %s legs %v, want 200 with the record carried", rec.Code, rec.Body, stub.legTypes)
				}
				if claim := sent.get("pas-claim"); !bytes.Contains(claim[len(claim)-1], []byte(deep)) {
					t.Fatal("the PAS claim does not carry the record as held")
				}
			})
		}
	}
}

// The order-dispatch route reaches the PAS tail with no leg sent before it:
// a record the completion will not read (past the request graph's own bound)
// is refused 502 PAS evidence linkage failed, recorded as the provider
// gateway's own refusal under no named rule with no leg before it, under
// either record id and at every level.
func TestProviderData_TheDispatchRecordsItsOwnEvidenceRefusal(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC03"
	const performerRef = "Organization/org-dme-uc03"
	demo := Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}
	deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
	for _, recordID := range []string{"pat-55", member} {
		for _, level := range allLevels {
			t.Run("record "+recordID+"/"+level.String(), func(t *testing.T) {
				t.Parallel()
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
				useRecordIDSoRHolding(t, fix.gw, member, recordID, demo, "member's own Coverage", heldBytes(recordID, `"code":`, `"note":`+deep+`,"code":`))
				rec, recs := serveRecorded(t, fix.gw, "POST /scenario/dispatch", testRouteBody(fix.gw, "/scenario/dispatch", `"member":"`+member+`"`))
				if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "PAS evidence linkage failed") || legAttempted(fix.stub.legTypes, "pas-claim") {
					t.Fatalf("dispatch = %d %s legs %v, want 502 PAS evidence linkage failed and no PAS claim", rec.Code, rec.Body, fix.stub.legTypes)
				}
				wantRefusal(t, onlyRecord(t, recs), http.StatusBadGateway, RefusedByProviderGateway, ExchangeOther)
			})
		}
	}
}

// The pend keeps the member's record id beside the member, so the resume
// ClaimUpdate's evidence is read the way the submission's was: the
// submission the pend sent carries the record about that id.
func TestProviderData_ThePendKeepsTheMembersRecordID(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-UC06"
	demo := Demo{BirthDate: "1969-07-21", FamilyName: "Reyes"}
	uc06 := DemoOrderCodes().UC06
	for _, recordID := range []string{"pat-66", member} {
		t.Run("record "+recordID, func(t *testing.T) {
			t.Parallel()
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

// evidenceSubjectRow is one mutation of the record the system holds about
// the member's Patient under recordID, and the answer it must get.
type evidenceSubjectRow struct {
	name string
	// ownRecordIDOnly: the row runs only with the member under a record id of
	// its own; otherwise it runs under that record id and under the member id.
	ownRecordIDOnly bool
	mutate          func(c map[string]any, recordID string) any
	status          int
	body            string
	rule            string // evidenceFidelity, evidenceUpstream or evidenceOther
}

const (
	evidenceFidelity  = "fidelity"
	evidenceUpstream  = "upstream"
	evidenceIntegrity = "integrity"
	evidenceOther     = "other"
)

// heldEvidence is the control each evidenceSubjectRow mutates: the record as
// the system holds it about the Patient under recordID.
func heldEvidence(recordID string) map[string]any {
	return map[string]any{
		"resourceType": "Condition", "id": "cond-reason",
		"code":    map[string]any{"coding": []any{map[string]any{"system": "http://hl7.org/fhir/sid/icd-10-cm", "code": "J44.9"}}},
		"subject": map[string]any{"reference": "Patient/" + recordID},
	}
}

// failingRead, returned by an evidenceSubjectRow's mutate, is the system's
// read of the evidence failing with err: it holds the control record, but the
// read does not answer it. The deployed connector (fhirsor) answers this way
// for an answer it cannot read as a resource of the referenced type
// (TestSoR_ResolveByReference_EvidenceShapes).
type failingRead struct{ err error }

// evidenceReadFails is a system whose read of ref fails with err: a read
// that did not answer the record, never an answer of absence.
type evidenceReadFails struct {
	refReadFails
	err error
}

func (s evidenceReadFails) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	if ref == s.ref {
		return nil, false, s.err
	}
	return s.refReadFails.ResolveByReferenceContext(ctx, ref)
}

// mutatedEvidence is row's record, or nil when the system holds none, and the
// error the system's read of it fails with, if any.
func mutatedEvidence(t *testing.T, row evidenceSubjectRow, recordID string) ([]byte, error) {
	t.Helper()
	m := row.mutate(heldEvidence(recordID), recordID)
	var fails error
	if f, ok := m.(failingRead); ok {
		m, fails = heldEvidence(recordID), f.err
	}
	if m == nil {
		return nil, nil
	}
	if raw, ok := m.([]byte); ok {
		return raw, fails // bytes as the system answers them, not re-encoded
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b, fails
}

// heldBytes is heldEvidence(recordID) as the system answers it, with old
// replaced once by new: a mutation of the bytes themselves.
func heldBytes(recordID, old, new string) []byte {
	b, _ := json.Marshal(heldEvidence(recordID))
	return bytes.Replace(b, []byte(old), []byte(new), 1)
}

// evidenceSubjectRows is every shape of the evidence record a PAS completion
// decides, for member, each one mutation of heldEvidence.
func evidenceSubjectRows(member string) []evidenceSubjectRow {
	const (
		notExactly = "Condition/cond-reason's subject is not exactly the request's patient"
		noSubject  = "Condition/cond-reason names no subject.reference"
		unreadable = "PAS evidence linkage failed: Condition/cond-reason as the system answered it is not that resource"
		notHeld    = "Condition/cond-reason is not held by the system of record"
		linkage    = "PAS evidence linkage failed"
	)
	requestsPat := "https://shn.example/fhir/Patient/" + member
	subjectRef := func(ref any) func(map[string]any, string) any {
		return func(c map[string]any, _ string) any { c["subject"] = map[string]any{"reference": ref}; return c }
	}
	return []evidenceSubjectRow{
		{"another patient", false, subjectRef("Patient/someone-else"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"the member id under a record id of its own", true, subjectRef("Patient/" + member), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a versioned reference to the record id", false, func(c map[string]any, id string) any {
			c["subject"] = map[string]any{"reference": "Patient/" + id + "/_history/2"}
			return c
		}, http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"an absolute reference to the record id on another base", false, func(c map[string]any, id string) any {
			c["subject"] = map[string]any{"reference": "https://ehr.example/fhir/Patient/" + id}
			return c
		}, http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"the request's Patient fullUrl, under a record id of its own", true, subjectRef(requestsPat), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a Group", false, subjectRef("Group/cohort-1"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a Device", false, subjectRef("Device/pump-1"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a Location", false, subjectRef("Location/ward-3"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a contained reference", false, subjectRef("#p1"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a urn reference", false, subjectRef("urn:uuid:9d6c1f0e-4b7a-4c2e-9a51-3f0d8e2b7c11"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"a malformed reference", false, subjectRef("Patient/"), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"an empty reference", false, subjectRef(""), http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		{"no subject", false, func(c map[string]any, _ string) any { delete(c, "subject"); return c }, http.StatusUnprocessableEntity, noSubject, evidenceFidelity},
		{"a subject with no reference", false, func(c map[string]any, _ string) any {
			c["subject"] = map[string]any{"display": "the member"}
			return c
		}, http.StatusUnprocessableEntity, noSubject, evidenceFidelity},
		{"a reference that is not a string", false, subjectRef(42), http.StatusUnprocessableEntity, noSubject, evidenceFidelity},
		{"signed, about the record id", true, func(_ map[string]any, id string) any {
			return json.RawMessage(signedCondition("Patient/" + id))
		}, http.StatusUnprocessableEntity, "signed content cannot be edited (E-06, Signature in Condition)", evidenceFidelity},
		{"not a JSON object", false, func(map[string]any, string) any { return []any{} }, http.StatusBadGateway, unreadable, evidenceUpstream},
		{"another resourceType", false, func(c map[string]any, _ string) any { c["resourceType"] = "Observation"; return c }, http.StatusBadGateway, unreadable, evidenceUpstream},
		{"another id", false, func(c map[string]any, _ string) any { c["id"] = "cond-other"; return c }, http.StatusBadGateway, unreadable, evidenceUpstream},
		// Bytes the build cannot carry as read (the splice's own rule), decided
		// before the subject is read, the same on every path.
		{"bytes that are not UTF-8", false, func(_ map[string]any, id string) any {
			return heldBytes(id, `"J44.9"`, "\"J44.9\xff\"")
		}, http.StatusUnprocessableEntity, "Condition/cond-reason is not UTF-8", evidenceFidelity},
		{"a lone surrogate escape", false, func(_ map[string]any, id string) any {
			return heldBytes(id, `"J44.9"`, `"J44.9\ud800"`)
		}, http.StatusUnprocessableEntity, "Condition/cond-reason holds a lone surrogate escape", evidenceFidelity},
		{"a member named twice", false, func(_ map[string]any, id string) any {
			return heldBytes(id, `"id":"cond-reason"`, `"id":"cond-reason","id":"cond-reason"`)
		}, http.StatusUnprocessableEntity, "Condition/cond-reason names a member twice, or in another case", evidenceFidelity},
		{"a member named again in another case", false, func(_ map[string]any, id string) any {
			return heldBytes(id, `"code":`, `"Code":{},"code":`)
		}, http.StatusUnprocessableEntity, "Condition/cond-reason names a member twice, or in another case", evidenceFidelity},
		// The request graph's own bound: the gateway's limit, never blamed on
		// the system.
		{"nested past the request graph's bound", false, func(_ map[string]any, id string) any {
			deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
			return heldBytes(id, `"code":`, `"note":`+deep+`,"code":`)
		}, http.StatusBadGateway, linkage, evidenceOther},
		// Within the graph's bound, past the relay's limits on the record as
		// the request carries it (its depth in the Bundle, its token count).
		{"nested past the relay's depth as carried", false, func(_ map[string]any, id string) any {
			// One level past the deepest the request carries
			// (TestProviderData_EvidenceAtTheRelayDepthRides).
			deep := strings.Repeat("[", 61) + strings.Repeat("]", 61)
			return heldBytes(id, `"code":`, `"note":`+deep+`,"code":`)
		}, http.StatusBadGateway, linkage, evidenceOther},
		{"more tokens than the relay carries", false, func(_ map[string]any, id string) any {
			many := "[" + strings.Repeat("0,", 1_000_000) + "0]"
			return heldBytes(id, `"code":`, `"note":`+many+`,"code":`)
		}, http.StatusBadGateway, linkage, evidenceOther},
		// The request's size bound is the gateway's own, decided after the
		// record's shape on both paths: alone, the gateway's own 502; with a
		// shape the build refuses, that refusal.
		{"over the request's size bound", false, func(c map[string]any, _ string) any {
			c["note"] = strings.Repeat("x", 9<<20)
			return c
		}, http.StatusBadGateway, linkage, evidenceOther},
		{"over the request's size bound, about another patient", false, func(c map[string]any, _ string) any {
			c["note"] = strings.Repeat("x", 9<<20)
			c["subject"] = map[string]any{"reference": "Patient/someone-else"}
			return c
		}, http.StatusUnprocessableEntity, notExactly, evidenceFidelity},
		// A record the system definitely does not hold cannot be filled: the
		// build's fidelity. A read that fails to answer is not absence: it is
		// the system's failure, answered with its own status.
		{"not held", false, func(map[string]any, string) any { return nil }, http.StatusUnprocessableEntity, notHeld, evidenceFidelity},
		{"the system unavailable", false, func(map[string]any, string) any { return failingRead{&SoRReadError{Kind: SoRUnavailable}} }, http.StatusServiceUnavailable, linkage, evidenceUpstream},
		// The deployed connector's answer for a record it cannot read as a
		// resource of the referenced type (not a JSON object, another
		// resourceType, an element of the wrong type).
		{"the system's answer not a resource of that type", false, func(map[string]any, string) any { return failingRead{&SoRReadError{Kind: SoRInvalidResponse}} }, http.StatusBadGateway, linkage, evidenceUpstream},
		// Its answer for a record naming a member twice, in any case: it
		// reads two ways, so it is message integrity.
		{"the system's answer repeating a member", false, func(map[string]any, string) any { return failingRead{SoRRepeatedMember()} }, http.StatusBadGateway, linkage, evidenceIntegrity},
	}
}

// wantEvidenceAnswer fails unless the answer is row's: its status and
// message, and for a refusal under no named rule, the bare message, naming no
// record.
func wantEvidenceAnswer(t *testing.T, route string, status int, body string, row evidenceSubjectRow) {
	t.Helper()
	if status != row.status || !strings.Contains(body, row.body) {
		t.Fatalf("%s = %d %s, want %d %s", route, status, body, row.status, row.body)
	}
	if row.rule == evidenceOther && strings.Contains(body, "Condition/cond-reason") {
		t.Fatalf("%s = %d %s, want the bare %q, naming no record", route, status, body, row.body)
	}
}

// wantEvidenceRecorded fails unless r records row's answer: the fidelity of
// SHN's own build at the build stage, whatever legs came before it; the
// system's failure (an invalid answer, or a read that failed to answer) as the
// upstream error it is, or as message integrity for an answer repeating a
// member; or, past the gateway's own limits, the provider gateway's own
// refusal under no named rule.
func wantEvidenceRecorded(t *testing.T, r ExchangeRecord, row evidenceSubjectRow) {
	t.Helper()
	switch row.rule {
	case evidenceOther:
		wantRefusal(t, r, row.status, RefusedByProviderGateway, ExchangeOther)
	case evidenceUpstream:
		if r.Outcome != ExchangeUpstreamError || r.Status != row.status {
			t.Fatalf("record = %s %s/%s status %d, want %s status %d", r.Outcome, r.RefusedBy, r.Rule, r.Status, ExchangeUpstreamError, row.status)
		}
	case evidenceIntegrity:
		wantRefusal(t, r, row.status, RefusedByProviderGateway, RefusalIntegrity)
	case evidenceFidelity:
		wantRefusal(t, r, row.status, RefusedByProviderGateway, RefusalFidelity)
		if r.RefusalStage != vocab.RefusalStageBuild {
			t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
		}
	default:
		t.Fatalf("row %q: unknown rule %q", row.name, row.rule)
	}
}

// REJECTION, at every level: a clinical record the order references is
// carried only when the system holds the member under the member id and the
// record names it (relative, or absolute as the request's own Patient
// fullUrl), or holds the member under a record id of its own and the record
// names exactly that record id (registered edit E-06 re-points it). Each other
// shape is its own decided answer, the same whichever id the system holds the
// member under, after the CRD and questionnaire legs, which stand, and no PAS
// claim is sent:
//   - a subject that is not exactly the request's patient (another patient;
//     under a record id of its own, the member id itself, a versioned
//     reference to the record id, an absolute one; a Group or other
//     non-Patient reference; a contained, urn, malformed, empty or other-base
//     reference) is refused 422 naming the record;
//   - a record naming no subject.reference (no subject, a subject with no
//     reference, a reference that is not a string) is refused 422 naming the
//     record: one that does not say whose it is cannot be filled as this
//     patient's;
//   - each is recorded as the fidelity of SHN's own build at the build stage,
//     whatever the request sent before it, as a record E-06 would re-point
//     that a signature covers is (D113);
//   - bytes the build cannot carry as read, and a record the system
//     definitely does not hold (it cannot be filled), are refused 422 the
//     same way;
//   - an answer that is not the resource the reference names (not a JSON
//     object, another resourceType or id) is the system's invalid answer,
//     502 naming the record, and a read the system fails to answer is the
//     system's failure, 503 or 502, each recorded as the upstream error it
//     is (one repeating a member as message integrity);
//   - past the gateway's own limits (the request graph's bound, the relay's
//     on the record as carried, the request's size) it is the gateway's own
//     502 PAS evidence linkage failed, under no named rule.
//
// Each row is the carried control with one mutation of the record or of its
// read (evidenceSubjectRows); the deployed connector's own reading of each
// shape is pinned in fhirsor's TestSoR_ResolveByReference_EvidenceShapes.
func TestProviderData_EvidenceAboutAnotherPatientIsRefused(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	for _, row := range evidenceSubjectRows(member) {
		ids := []string{"pat-77", member}
		if row.ownRecordIDOnly {
			ids = ids[:1]
		}
		for _, recordID := range ids {
			for _, level := range allLevels {
				t.Run(row.name+"/record "+recordID+"/"+level.String(), func(t *testing.T) {
					t.Parallel()
					condition, readFails := mutatedEvidence(t, row, recordID)
					order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx))
					g, stub, _ := newAuthoredQRSingleShotFixture(t, member, demo, order)
					mountOriginationRoutes(g)
					g.cfg.ConformanceEnforcement = level
					useRecordIDSoRHolding(t, g, member, recordID, demo, "member's own Coverage", condition)
					if condition == nil {
						// The system holds no record the order names.
						delete(g.cfg.SoR.(*recordIDSoR).evidence, "Condition/cond-reason")
					}
					if readFails != nil {
						g.cfg.SoR = evidenceReadFails{refReadFails{SystemOfRecord: g.cfg.SoR, ref: "Condition/cond-reason"}, readFails}
					}
					rec, recs := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
					wantEvidenceAnswer(t, "POST /scenario/uc04", rec.Code, rec.Body.String(), row)
					if legAttempted(stub.legTypes, "pas-claim") || !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
						t.Fatalf("legs %v: want the questionnaire fetched and no PAS claim", stub.legTypes)
					}
					wantCarried(t, rec.Body.String(), stub.legTypes...)
					wantEvidenceRecorded(t, onlyRecord(t, recs), row)
				})
			}
		}
	}
}

// The request's own Patient fullUrl, absolute, is that patient: with the
// member under the member id, a record naming it rides the submission as
// held, at every level, and names no edit. (Under a record id of its own it
// is refused: E-06 re-points only the exact record id.)
func TestProviderData_EvidenceNamingTheRequestsPatientFullURLRides(t *testing.T) {
	t.Parallel() // each row builds its own gateway, keys, substrate and system of record
	const member = "MBR-PD-UC04"
	demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
	uc04 := DemoOrderCodes().UC04
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			t.Parallel()
			order := withReason(t, buildAuthoredQROrderWithID(t, "sr-uc04-evidence", "Patient/"+member, uc04.System, uc04.Code, uc04.Display, uc04.Dx))
			g, stub, sent := newAuthoredQRSingleShotFixture(t, member, demo, order)
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			full := "https://shn.example/fhir/Patient/" + member
			useRecordIDSoRHolding(t, g, member, member, demo, "member's own Coverage", evidenceCondition(full))
			rec, recs := serveRecorded(t, g, "POST /scenario/uc04", testOrderBody(order, `"member":"`+member+`"`))
			if rec.Code != http.StatusOK || !legAttempted(stub.legTypes, "pas-claim") {
				t.Fatalf("POST /scenario/uc04 = %d %s legs %v, want 200 with the PAS claim sent", rec.Code, rec.Body, stub.legTypes)
			}
			claims := sent.get("pas-claim")
			if len(claims) == 0 {
				t.Fatal("no PAS claim captured")
			}
			var bundle map[string]any
			if err := json.Unmarshal(claims[len(claims)-1], &bundle); err != nil {
				t.Fatal(err)
			}
			var patientURL, got string
			for _, e := range bundle["entry"].([]any) {
				m := e.(map[string]any)
				r := m["resource"].(map[string]any)
				switch r["resourceType"] {
				case "Patient":
					patientURL, _ = m["fullUrl"].(string)
				case "Condition":
					got = slotReference(r, "subject")
				}
			}
			if patientURL != full || got != full {
				t.Fatalf("Patient fullUrl %q, Condition subject %q: want the record carried naming %q as held", patientURL, got, full)
			}
			r := onlyRecord(t, recs)
			if r.Outcome != ExchangeAnswered || len(r.RequestEdits) != 0 {
				t.Fatalf("recorded %s naming edits %v, want the payer's answer and no edit", r.Outcome, r.RequestEdits)
			}
		})
	}
}

// evidenceReadByMember re-points, by E-06, a record whose subject is exactly
// the member's record id; refuses as not the patient's one whose subject is
// anything else, and as naming no subject one with no subject.reference; answers
// one that is not the resource its reference names as the system's invalid
// answer; answers a record not held as not held; refuses a signed one; and
// edits nothing when the record id is unknown or is the member id. It notes
// E-06 only for the record it re-pointed.
func TestEvidenceReadByMember(t *testing.T) {
	// at is the record the system holds at Condition/<id>, about subject.
	at := func(id string, b []byte) []byte {
		return bytes.Replace(b, []byte(`"id":"cond-reason"`), []byte(`"id":"`+id+`"`), 1)
	}
	held := map[string][]byte{
		"Condition/mine":       at("mine", evidenceCondition("Patient/pat-77")),
		"Condition/theirs":     at("theirs", evidenceCondition("Patient/someone-else")),
		"Condition/member-id":  at("member-id", evidenceCondition("Patient/MBR-X")),
		"Condition/versioned":  at("versioned", evidenceCondition("Patient/pat-77/_history/2")),
		"Condition/absolute":   at("absolute", evidenceCondition("https://ehr.example/fhir/Patient/pat-77")),
		"Condition/group":      at("group", evidenceCondition("Group/cohort-1")),
		"Condition/signed":     at("signed", signedCondition("Patient/pat-77")),
		"Condition/no-subject": []byte(`{"resourceType":"Condition","id":"no-subject"}`),
		"Condition/no-ref":     []byte(`{"resourceType":"Condition","id":"no-ref","subject":{"display":"x"}}`),
		"Condition/not-object": []byte(`[]`),
		"Condition/other-type": []byte(`{"resourceType":"Observation","id":"other-type","subject":{"reference":"Patient/pat-77"}}`),
		"Condition/other-id":   at("cond-elsewhere", evidenceCondition("Patient/pat-77")),
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
	const (
		notExactly = "'s subject is not exactly the request's patient"
		noSubject  = " names no subject.reference"
	)
	for _, row := range []struct {
		name, sorID, ref string
		found            bool
		want             string
		refused          string // the refusal's text after the ref, when refused as not carried
		unreadable       bool   // answered as the system's invalid answer
	}{
		{"about the member's record id", "pat-77", "Condition/mine", true, "Patient/MBR-X", "", false},
		{"about someone else", "pat-77", "Condition/theirs", false, "", notExactly, false},
		{"the member id under a record id of its own", "pat-77", "Condition/member-id", false, "", notExactly, false},
		{"a versioned reference", "pat-77", "Condition/versioned", false, "", notExactly, false},
		{"an absolute reference", "pat-77", "Condition/absolute", false, "", notExactly, false},
		{"a Group", "pat-77", "Condition/group", false, "", notExactly, false},
		{"no subject", "pat-77", "Condition/no-subject", false, "", noSubject, false},
		{"a subject with no reference", "pat-77", "Condition/no-ref", false, "", noSubject, false},
		{"not a JSON object", "pat-77", "Condition/not-object", false, "", "", true},
		{"another resourceType", "pat-77", "Condition/other-type", false, "", "", true},
		{"another id", "pat-77", "Condition/other-id", false, "", "", true},
		// A record the system does not hold stays not held, never refused.
		{"not held", "pat-77", "Condition/none", false, "", "", false},
		{"record id unknown", "", "Condition/mine", true, "Patient/pat-77", "", false},
		{"record id is the member id", "MBR-X", "Condition/mine", true, "Patient/pat-77", "", false},
		{"signed, record id is the member id", "MBR-X", "Condition/signed", true, "Patient/pat-77", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var note inputEditNote
			raw, found, err := evidenceReadByMember(read, row.sorID, "MBR-X", &note)(context.Background(), row.ref)
			if row.refused != "" || row.unreadable {
				var unreadable *evidenceUnreadableError
				switch {
				case found:
					t.Fatalf("read: found %s, want it refused", row.ref)
				case row.unreadable && (!errors.As(err, &unreadable) || err.Error() != row.ref+" as the system answered it is not that resource"):
					t.Fatalf("read: err %v, want %s answered as the system's invalid answer", err, row.ref)
				case row.unreadable:
					var readErr *SoRReadError
					if !errors.As(err, &readErr) || readErr.Kind != SoRInvalidResponse {
						t.Fatalf("read: err %v is not the system's invalid response", err)
					}
				case !errors.Is(err, errRecordNotCarried) || !strings.HasSuffix(err.Error(), ": "+row.ref+row.refused):
					t.Fatalf("read: err %v, want %s refused: %s%s", err, row.ref, row.ref, row.refused)
				}
				if got := note.list(); got != nil {
					t.Fatalf("a refused record noted %v, want nothing", got)
				}
				return
			}
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
