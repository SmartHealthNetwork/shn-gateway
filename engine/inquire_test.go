package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inquiryFixture reads one of the synthetic inquiry fixtures. They are derived
// from the IG's own examples and the published $inquire operation definitions, and
// each says in its own narrative that it is synthetic: no real payer has answered
// an inquiry yet.
func inquiryFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

// recordingInquirePartner is a payer system that answers `Claim/$inquire` and
// records exactly what it was asked, so a relay can be checked byte for byte
// rather than by shape.
type recordingInquirePartner struct {
	srv         *httptest.Server
	path        string
	body        []byte
	contentType string
}

func newInquirePartner(t *testing.T, status int, answer []byte, answerType string) *recordingInquirePartner {
	t.Helper()
	p := &recordingInquirePartner{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.path = r.URL.Path
		p.body, _ = io.ReadAll(r.Body)
		p.contentType = r.Header.Get("Content-Type")
		if answerType != "" {
			w.Header().Set("Content-Type", answerType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(answer)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// TestPASInquire_RelayedExactly: at BOTH answer shapes the payer's inquiry travels
// to the payer's own `Claim/$inquire` byte for byte, and the payer's answer comes
// back byte for byte with the payer's own media type. The two shapes are the
// published operation definitions' own: the response Bundle at 2.0.1 and 2.1.0,
// and a `Parameters` of `return` Bundles at 2.2.1.
func TestPASInquire_RelayedExactly(t *testing.T) {
	request := inquiryFixture(t, "pas-inquiry-request-2.0.json")
	for _, tc := range []struct {
		name, fixture, mediaType string
	}{
		{"2.0.1 and 2.1.0 return one response Bundle", "pas-inquiry-response-2.0.json", "application/fhir+json; charset=utf-8"},
		{"2.2.1 returns a Parameters of response Bundles", "pas-inquiry-response-2.2.json", "application/fhir+json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := inquiryFixture(t, tc.fixture)
			p := newInquirePartner(t, http.StatusOK, answer, tc.mediaType)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", newCensusSoR(), fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim-inquire", "corr-inq", "PCI-1", request)
			if err != nil || res.Status != 0 {
				t.Fatalf("inquiry: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if p.path != "/Claim/$inquire" {
				t.Errorf("posted to %q, want /Claim/$inquire", p.path)
			}
			if !bytes.Equal(p.body, request) {
				t.Errorf("the inquiry did not reach the payer's system byte for byte")
			}
			if got := responseBytes(res); !bytes.Equal(got, answer) {
				t.Errorf("the payer's answer was not relayed byte for byte")
			}
			if got := res.ResponseContentType(); got != tc.mediaType {
				t.Errorf("answer media type = %q, want the payer's own %q", got, tc.mediaType)
			}
			if !res.ResponseSubjectForeign {
				t.Error("the answer is the payer's, in the payer's own patient namespace — it must be declared foreign")
			}
			if !res.ResponseRelayed() {
				t.Error("the answer must be relayed, not authored by this gateway")
			}
			if bad := validatePASInquiryAnswer(responseBytes(res)); bad.Status != 0 {
				t.Errorf("a published answer shape was refused: %d %s", bad.Status, bad.Message)
			}
		})
	}
}

// TestPASInquire_AnswerShapeRejections is the answer-shape guard's rejection set:
// the valid shapes above minus the shape itself must be refused, loudly, rather
// than passed on as an answer nothing can read.
func TestPASInquire_AnswerShapeRejections(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a bare ClaimResponse is not an inquiry answer", `{"resourceType":"ClaimResponse","outcome":"complete"}`},
		{"an unrelated resource", `{"resourceType":"Claim","use":"preauthorization"}`},
		{"no resourceType at all", `{"parameter":[]}`},
		{"a return parameter that is not a Bundle", `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"ClaimResponse"}}]}`},
		{"malformed JSON", `{"resourceType":"Bundle"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := validatePASInquiryAnswer([]byte(tc.body))
			if bad.Status != http.StatusBadGateway {
				t.Fatalf("status=%d msg=%q, want 502", bad.Status, bad.Message)
			}
		})
	}
	// Non-vacuous control: an OperationOutcome IS an answer the operation
	// definitions declare, so the guard must not refuse it.
	if bad := validatePASInquiryAnswer([]byte(`{"resourceType":"OperationOutcome","issue":[]}`)); bad.Status != 0 {
		t.Fatalf("an OperationOutcome answer was refused: %d %s", bad.Status, bad.Message)
	}
}

// TestPASInquire_ReadsEveryClaimResponse: the lookup keys come from EVERY
// ClaimResponse the answer carries, in both shapes. An answer that decides two
// authorizations decides two, not one.
func TestPASInquire_ReadsEveryClaimResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"one response Bundle with two ClaimResponses", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","identifier":[{"system":"s","value":"a"}]}},
			{"resource":{"resourceType":"Organization"}},
			{"resource":{"resourceType":"ClaimResponse","identifier":[{"system":"s","value":"b"}]}}]}`, 2},
		{"two return Bundles of one ClaimResponse each", `{"resourceType":"Parameters","parameter":[
			{"name":"return","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","identifier":[{"system":"s","value":"a"}]}}]}},
			{"name":"return","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","identifier":[{"system":"s","value":"b"}]}}]}}]}`, 2},
		{"an answer that names no authorization", `{"resourceType":"Bundle","entry":[]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readPASInquiryAnswers("provider", []byte(tc.body))
			if len(got) != tc.want {
				t.Fatalf("read %d ClaimResponses, want %d", len(got), tc.want)
			}
			for _, a := range got {
				if a.keys.RequesterHolder != "provider" {
					t.Errorf("keys were not namespaced to the requester: %q", a.keys.RequesterHolder)
				}
			}
		})
	}
}

// TestPASInquire_NeverMatchesOnTheInquirysOwnIdentifier: the keys an answer is
// matched by are the payer's own, never the identifier the inquiry minted for
// itself. At 2.2.1 `Claim.identifier` IS the inquiry's own trace number, and the
// IG says it is not used to search for previous authorizations, so a ledger that
// could match on it would resolve a follow-up by a fact the follow-up invented.
func TestPASInquire_NeverMatchesOnTheInquirysOwnIdentifier(t *testing.T) {
	answers := readPASInquiryAnswers("provider", []byte(`{"resourceType":"Bundle","entry":[{"resource":{
		"resourceType":"ClaimResponse",
		"identifier":[{"system":"http://payer.example/cr","value":"CR-1"}],
		"request":{"identifier":{"system":"http://provider.example/claim","value":"SUB-1"}},
		"preAuthRef":"AUTH-1",
		"item":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
			"valueIdentifier":{"system":"http://provider.example/trn","value":"TRN-1"}}]}]}}]}`))
	if len(answers) != 1 {
		t.Fatalf("read %d ClaimResponses, want 1", len(answers))
	}
	k := answers[0].keys
	if len(k.ClaimResponseIDs) != 1 || k.ClaimResponseIDs[0] != "http://payer.example/cr|CR-1" {
		t.Errorf("ClaimResponseIDs = %v", k.ClaimResponseIDs)
	}
	if len(k.RequestIDs) != 1 || k.RequestIDs[0] != "http://provider.example/claim|SUB-1" {
		t.Errorf("RequestIDs = %v", k.RequestIDs)
	}
	if k.PreAuthRef != "AUTH-1" {
		t.Errorf("PreAuthRef = %q", k.PreAuthRef)
	}
	if len(k.ItemTraceNumbers) != 1 || k.ItemTraceNumbers[0] != "http://provider.example/trn|TRN-1" {
		t.Errorf("ItemTraceNumbers = %v", k.ItemTraceNumbers)
	}
	// The key struct has no field for the inquiry's own Claim.identifier, so the
	// reader cannot produce one — pinned here as well as in the ledger's own test,
	// because THIS is the reader that would have to supply it.
	for _, ref := range ProbePendKeys(k) {
		if strings.Contains(ref.Key, "INQUIRY-TRN") {
			t.Errorf("a lookup key came from the inquiry's own identifier: %+v", ref)
		}
	}
}

// ---- the inquiry request's subject bind ----

// inquiryBundle builds a minimal but correctly shaped inquiry request Bundle for
// one member, with one item line carrying a trace number and a product coding.
// coverageMember lets a row point the Coverage at a DIFFERENT member, which is the
// one mutation the bind must catch.
func inquiryBundle(member, coverageMember, trn, code string) []byte {
	if coverageMember == "" {
		coverageMember = member
	}
	return []byte(`{"resourceType":"Bundle","type":"collection",
		"identifier":{"system":"http://provider.example/inq","value":"INQ-1"},
		"timestamp":"2026-09-18T00:00:00Z",
		"entry":[
		  {"fullUrl":"urn:uuid:claim","resource":{"resourceType":"Claim","use":"preauthorization",
		    "identifier":[{"system":"http://provider.example/inq","value":"INQUIRY-TRN"}],
		    "patient":{"reference":"Patient/` + member + `"},
		    "item":[{"sequence":1,
		      "productOrService":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"` + code + `","display":"MRI lumbar spine"}]},
		      "extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
		        "valueIdentifier":{"system":"http://provider.example/trn","value":"` + trn + `"}}]}]}},
		  {"fullUrl":"urn:uuid:patient","resource":{"resourceType":"Patient","id":"` + member + `"}},
		  {"fullUrl":"urn:uuid:coverage","resource":{"resourceType":"Coverage",
		    "beneficiary":{"reference":"Patient/` + coverageMember + `"}}}]}`)
}

// TestPASInquire_SubjectBoundOverEveryPatient: the inquiry binds to ONE member
// across the WHOLE Bundle, not just the Claim's patient. The control binds; the
// same Bundle with only the Coverage repointed at another member is refused.
func TestPASInquire_SubjectBoundOverEveryPatient(t *testing.T) {
	facts, status, msg := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if status != 0 {
		t.Fatalf("control inquiry refused: %d %s", status, msg)
	}
	if facts.member != "MBR-COVERED" || len(facts.items) != 1 || facts.items[0].code != "72148" {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.items[0].traceNumber != "http://provider.example/trn|TRN-1" {
		t.Errorf("trace number = %q", facts.items[0].traceNumber)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"a second member on the Coverage", string(inquiryBundle("MBR-COVERED", "MBR-NOTCOVERED", "TRN-1", "72148")), http.StatusForbidden},
		{"not a Bundle", `{"resourceType":"Parameters"}`, http.StatusBadRequest},
		{"not a collection", `{"resourceType":"Bundle","type":"searchset","entry":[]}`, http.StatusBadRequest},
		{"no Claim", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}}]}`, http.StatusBadRequest},
		{"the Claim is not a preauthorization", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Claim","use":"claim","patient":{"reference":"Patient/MBR-COVERED"}}}]}`, http.StatusBadRequest},
		{"an entry carries a request", `{"resourceType":"Bundle","type":"collection","entry":[{"request":{"method":"POST","url":"Claim/$inquire"},"resource":{"resourceType":"Claim","use":"preauthorization","patient":{"reference":"Patient/MBR-COVERED"}}}]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, status, _ := parsePASInquiryFacts([]byte(tc.body))
			if status != tc.status {
				t.Fatalf("status=%d, want %d", status, tc.status)
			}
		})
	}
}

// inquiryBundleNoItems is an inquiry that asks by the authorization number alone:
// the later prior-authorization lines carry that number on the inquiry Claim
// itself rather than on an item, so this Bundle names no item at all and is a
// conformant request.
func inquiryBundleNoItems(member string) []byte {
	return []byte(`{"resourceType":"Bundle","type":"collection",
		"identifier":{"system":"http://provider.example/inq","value":"INQ-2"},
		"timestamp":"2026-09-18T00:00:00Z",
		"entry":[
		  {"fullUrl":"urn:uuid:claim","resource":{"resourceType":"Claim","use":"preauthorization",
		    "identifier":[{"system":"http://provider.example/inq","value":"INQUIRY-TRN"}],
		    "extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-authorizationNumber",
		      "valueString":"AUTH0001"}],
		    "patient":{"reference":"Patient/` + member + `"}}},
		  {"fullUrl":"urn:uuid:patient","resource":{"resourceType":"Patient","id":"` + member + `"}},
		  {"fullUrl":"urn:uuid:coverage","resource":{"resourceType":"Coverage",
		    "beneficiary":{"reference":"Patient/` + member + `"}}}]}`)
}

// TestPASInquire_ByAuthorizationNumberWithNoItems: an inquiry that names NO item
// is accepted. A requester may ask by the authorization number alone, which the
// later lines carry on the Claim rather than on an item, so refusing an
// item-less Bundle would refuse a conformant peer — and would make this gateway
// stricter than the published Responder, whose own inquiry reader requires no
// item either.
//
// The items feed only the product coding a recorded decision's
// ExplanationOfBenefit states, so an inquiry with none records its decision
// without one rather than inventing a code.
func TestPASInquire_ByAuthorizationNumberWithNoItems(t *testing.T) {
	facts, status, msg := parsePASInquiryFacts(inquiryBundleNoItems("MBR-COVERED"))
	if status != 0 {
		t.Fatalf("an inquiry by authorization number alone was refused: %d %s", status, msg)
	}
	if facts.member != "MBR-COVERED" {
		t.Errorf("member = %q, want MBR-COVERED", facts.member)
	}
	if len(facts.items) != 0 {
		t.Fatalf("items = %+v, want none", facts.items)
	}
	// It still reaches the ledger and still decides — with no EOB, because the
	// requester named no product coding and this gateway will not choose one.
	store := NewMemStore()
	g := &Gateway{cfg: Config{Store: store, Clock: fixedClock}}
	const subject, corr = "pci:MBR-COVERED", "corr-noitems"
	if _, err := store.RecordPendedKeyed(subject, corr, fixedClock(), PendKeys{
		RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	result := LegResult{}
	commit, _ := g.inquiryLedgerEffect(inquiryRequester, subject, "Patient/MBR-COVERED", "corr-inquiry-leg", facts, decidedAnswer(t), &result)
	if commit == nil {
		t.Fatal("an item-less inquiry produced no ledger write")
	}
	if err := commit(); err != nil {
		t.Fatalf("ledger write: %v", err)
	}
	rec, _, _ := store.PendRecordOf(subject, corr)
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("ledger = %+v, want decided/approved", rec)
	}
	if len(result.SideEffectFHIR) != 0 {
		t.Errorf("an item-less inquiry produced %d EOBs; it names no product coding, so it must produce none", len(result.SideEffectFHIR))
	}
}

// TestIngressInquire_SubjectBound: the provider-facing `POST /Claim/$inquire`
// refuses an inquiry that names two members, and it refuses it at the BIND —
// before anything is routed anywhere. The control reaches routing (and fails
// there, on this fixture's unroutable Coverage), so the 403 is attributable to
// the mutated member and not to some other property of the request.
func TestIngressInquire_SubjectBound(t *testing.T) {
	newGateway := func() *Gateway {
		return &Gateway{cfg: Config{ingressAuthBypass: true, SoR: newCensusSoR(), Clock: fixedClock}}
	}
	post := func(t *testing.T, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		newGateway().handlePASInquireIngress(w, httptest.NewRequest(http.MethodPost, "/Claim/$inquire", bytes.NewReader(body)))
		return w
	}
	control := post(t, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if control.Code == http.StatusForbidden {
		t.Fatalf("the control inquiry was refused by the subject bind (%s) — the rejection row below would be vacuous", control.Body)
	}
	if control.Code != http.StatusUnprocessableEntity {
		t.Fatalf("control status=%d body=%s, want 422 (past the bind, refused at routing)", control.Code, control.Body)
	}
	mutated := post(t, inquiryBundle("MBR-COVERED", "MBR-NOTCOVERED", "TRN-1", "72148"))
	if mutated.Code != http.StatusForbidden {
		t.Fatalf("a two-member inquiry was not refused: status=%d body=%s", mutated.Code, mutated.Body)
	}
	if !strings.Contains(mutated.Body.String(), "inconsistent patient in PAS inquiry") {
		t.Errorf("refusal does not name the inconsistency: %s", mutated.Body)
	}
}

// ---- the ledger effect ----

// inquiryLedgerFixture is a payer gateway with a real pend ledger, the pended
// authorization the rows below inquire about, and the inquiry that asks.
type inquiryLedgerFixture struct {
	g       *Gateway
	store   *MemStore
	facts   pasInquiryFacts
	subject string
	corr    string
}

const (
	inquiryRequester      = "provider"
	inquiryOtherRequester = "provider-b"
	inquiryCRKey          = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"
	// inquiryTraceKey is the trace number the fixture inquiry's line states
	// (inquiryBundle's "TRN-1"): a pend holding it, alone, is the authorization the
	// inquiry asks about, so its decision's EOB may be stated from the inquiry's line.
	inquiryTraceKey = "http://provider.example/trn|TRN-1"
)

func newInquiryLedgerFixture(t *testing.T, keys PendKeys) *inquiryLedgerFixture {
	t.Helper()
	store := NewMemStore()
	g := &Gateway{cfg: Config{Store: store, Clock: fixedClock}}
	facts, status, msg := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if status != 0 {
		t.Fatalf("fixture inquiry refused: %d %s", status, msg)
	}
	f := &inquiryLedgerFixture{g: g, store: store, facts: facts, subject: "pci:MBR-COVERED", corr: "corr-submit-1"}
	if keys.RequesterHolder != "" {
		if _, err := store.RecordPendedKeyed(f.subject, f.corr, fixedClock(), keys); err != nil {
			t.Fatalf("seed pend: %v", err)
		}
	}
	return f
}

// state reads the ledger row the rows below assert on.
func (f *inquiryLedgerFixture) state(t *testing.T) PendRecord {
	t.Helper()
	rec, found, err := f.store.PendRecordOf(f.subject, f.corr)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !found {
		t.Fatalf("the seeded pend is gone")
	}
	return rec
}

// apply runs the ledger effect and its write, the way the inbound handler does.
func (f *inquiryLedgerFixture) apply(t *testing.T, requester string, answer []byte) LegResult {
	t.Helper()
	result := LegResult{}
	commit, _ := f.g.inquiryLedgerEffect(requester, f.subject, "Patient/MBR-COVERED", "corr-inquiry-leg", f.facts, answer, &result)
	if commit != nil {
		if err := commit(); err != nil {
			t.Fatalf("ledger write: %v", err)
		}
	}
	return result
}

// decidedAnswer is the synthetic 2.0.1/2.1.0-shape answer, whose ClaimResponse
// states the payer's approval (review action A1) and carries the identifier the
// pend was keyed on.
func decidedAnswer(t *testing.T) []byte {
	t.Helper()
	return inquiryFixture(t, "pas-inquiry-response-2.0.json")
}

// pendedAnswer states a re-pend for the same authorization: the same lookup key,
// but no decision.
func pendedAnswer() []byte {
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{
		"resourceType":"ClaimResponse","outcome":"queued","created":"2026-09-18T00:00:00Z",
		"identifier":[{"system":"http://example.org/PATIENT_EVENT_TRACE_NUMBER","value":"111099"}],
		"item":[{"itemSequence":1,"adjudication":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction",
		  "extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",
		    "valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A4","display":"Pended"}]}}]}],
		  "category":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/adjudication","code":"submitted"}]}}]}]}}]}`)
}

// TestPASInquire_LedgerDecidedOnMatch: exactly one match whose answer states a
// decision records that decision, with its ExplanationOfBenefit in the same
// write. The EOB's product coding comes from the REQUESTER's own inquiry line.
func TestPASInquire_LedgerDecidedOnMatch(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}, ItemTraceNumbers: []string{inquiryTraceKey}})
	if got := f.state(t).State; got != PendStatePended {
		t.Fatalf("seeded state = %q, want pended", got)
	}
	result := f.apply(t, inquiryRequester, decidedAnswer(t))
	rec := f.state(t)
	if rec.State != PendStateDecided || rec.Outcome != PendOutcomeApproved {
		t.Fatalf("ledger = %+v, want decided/approved", rec)
	}
	if rec.DecidedAt.IsZero() {
		t.Error("the decision was recorded without the payer's own date")
	}
	if len(result.SideEffectFHIR) != 1 {
		t.Fatalf("want one decision EOB, got %d", len(result.SideEffectFHIR))
	}
	eob := result.SideEffectFHIR[0]
	if !bytes.Contains(eob, []byte("72148")) {
		t.Errorf("the EOB's product coding did not come from the inquiry's own line:\n%s", eob)
	}
	if ref, err := parseEOBPatient(eob); err != nil || ref != "Patient/MBR-COVERED" {
		t.Errorf("EOB patient = %q (err=%v), want the bound member", ref, err)
	}
	if written, found := f.store.EOBByID("eob-" + f.corr); !found || !bytes.Equal(written, eob) {
		t.Errorf("the EOB was not written in the same write as the decision (found=%v)", found)
	}
}

// TestPASInquire_NoMatchNoLedgerChange: an answer whose keys name no authorization
// this requester has changes nothing. The ledger is deliberately NOT empty, so the
// row proves the lookup discriminated rather than that there was nothing to find.
func TestPASInquire_NoMatchNoLedgerChange(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://payer.example/cr|SOMETHING-ELSE"}})
	result := f.apply(t, inquiryRequester, decidedAnswer(t))
	if got := f.state(t); got.State != PendStatePended || got.Outcome != "" {
		t.Fatalf("ledger moved on a non-matching answer: %+v", got)
	}
	if len(result.SideEffectFHIR) != 0 {
		t.Errorf("a non-matching answer produced %d side-effects", len(result.SideEffectFHIR))
	}
}

// TestPASInquire_AmbiguousMatchNoLedgerChange: two authorizations sharing the
// answer's key are ambiguous, and the leg will not guess which one the requester
// meant. Both stay pended.
func TestPASInquire_AmbiguousMatchNoLedgerChange(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	const other = "corr-submit-2"
	if _, err := f.store.RecordPendedKeyed(f.subject, other, fixedClock(), PendKeys{
		RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey},
	}); err != nil {
		t.Fatalf("seed the colliding pend: %v", err)
	}
	result := f.apply(t, inquiryRequester, decidedAnswer(t))
	if got := f.state(t); got.State != PendStatePended {
		t.Fatalf("the first authorization moved on an ambiguous answer: %+v", got)
	}
	rec, found, err := f.store.PendRecordOf(f.subject, other)
	if err != nil || !found || rec.State != PendStatePended {
		t.Fatalf("the second authorization moved on an ambiguous answer: %+v (found=%v err=%v)", rec, found, err)
	}
	if len(result.SideEffectFHIR) != 0 {
		t.Errorf("an ambiguous answer produced %d side-effects", len(result.SideEffectFHIR))
	}
}

// TestPASInquire_StillPendedNoChange: a match the payer still reports as pended is
// not a decision. The answer relays; the ledger stays as it was.
func TestPASInquire_StillPendedNoChange(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	result := f.apply(t, inquiryRequester, pendedAnswer())
	if got := f.state(t); got.State != PendStatePended || got.Outcome != "" {
		t.Fatalf("a still-pended answer moved the ledger: %+v", got)
	}
	if len(result.SideEffectFHIR) != 0 {
		t.Errorf("a still-pended answer produced %d side-effects", len(result.SideEffectFHIR))
	}
}

// TestPASInquire_LedgerNeverMatchesTheInquirysOwnIdentifier is the ledger-effect
// half of the same rule, and it is the half a mutation can reach: an authorization
// keyed ONLY on the identifier the inquiry minted for itself must not be decided
// by that inquiry's answer.
//
// Pinning it at the reader alone leaves this open — threading the inquiry's own
// Claim.identifier into the probe keys before LookupPended keeps every other row
// green, because they all key the pend on something the payer's response carries.
// Here the pend is keyed on NOTHING ELSE, so a probe that included the inquiry's
// own identifier would match and decide.
//
// The rule is KIND-AGNOSTIC, and the index is keyed per kind, so each of the four
// kinds gets its own row: seeding the own-identifier pend under only one of them
// would leave the other three spellings of the same mistake green.
func TestPASInquire_LedgerNeverMatchesTheInquirysOwnIdentifier(t *testing.T) {
	// The value the fixture inquiry's own Claim.identifier states, and nothing the
	// payer's answer carries.
	const ownKey = "http://provider.example/inq|INQUIRY-TRN"
	for _, tc := range []struct {
		name string
		keys PendKeys
	}{
		{"as a submitted-claim identifier", PendKeys{RequesterHolder: inquiryRequester, RequestIDs: []string{ownKey}}},
		{"as a ClaimResponse identifier", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{ownKey}}},
		{"as an authorization reference", PendKeys{RequesterHolder: inquiryRequester, PreAuthRef: ownKey}},
		{"as an item trace number", PendKeys{RequesterHolder: inquiryRequester, ItemTraceNumbers: []string{ownKey}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInquiryLedgerFixture(t, tc.keys)
			result := f.apply(t, inquiryRequester, decidedAnswer(t))
			if got := f.state(t); got.State != PendStatePended || got.Outcome != "" {
				t.Fatalf("an authorization keyed only on the inquiry's own identifier (%s) was decided: %+v — the lookup matched on a fact the inquiry minted for itself", tc.name, got)
			}
			if len(result.SideEffectFHIR) != 0 {
				t.Errorf("that match produced %d side-effects", len(result.SideEffectFHIR))
			}
		})
	}
	// Non-vacuous control: the same answer against a pend keyed on what the
	// PAYER's response carries does decide it.
	control := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	control.apply(t, inquiryRequester, decidedAnswer(t))
	if got := control.state(t).State; got != PendStateDecided {
		t.Fatalf("control: a payer-stated key did not decide it (%q) — every row above would be vacuous", got)
	}
}

// unreadableLedger is a pend ledger whose LOOKUP fails, the one case that has no
// authorization correlation to report — and therefore the one whose event would
// otherwise carry an empty one.
type unreadableLedger struct{ *MemStore }

func (unreadableLedger) LookupPended(string, PendKeys, []PendKeyRef) (PendMatch, error) {
	return PendMatch{}, errors.New("pend index unavailable")
}

// TestPASInquire_LookupUnavailableNamesTheExchange: when the ledger cannot be
// consulted, the payer's answer still relays and the operator gets an event
// saying why nothing was recorded. That event is useless unless it names the
// exchange that raised it — a failed lookup returns no correlation of its own, so
// the leg's is what it must carry.
func TestPASInquire_LookupUnavailableNamesTheExchange(t *testing.T) {
	var seen []ObserverEvent
	g := &Gateway{cfg: Config{
		Store:    unreadableLedger{MemStore: NewMemStore()},
		Clock:    fixedClock,
		Observer: func(e ObserverEvent) { seen = append(seen, e) },
	}}
	facts, status, _ := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if status != 0 {
		t.Fatal("fixture inquiry refused")
	}
	const legCorr = "corr-inquiry-leg-42"
	result := LegResult{}
	commit, events := g.inquiryLedgerEffect(inquiryRequester, "pci:MBR-COVERED", "Patient/MBR-COVERED", legCorr, facts, decidedAnswer(t), &result)
	if commit != nil {
		t.Fatal("an unreadable ledger produced a write")
	}
	if len(result.SideEffectFHIR) != 0 {
		t.Errorf("an unreadable ledger produced %d side-effects", len(result.SideEffectFHIR))
	}
	if len(events) != 1 || events[0].Kind != "pend.lookup-unavailable" {
		t.Fatalf("events = %+v, want one pend.lookup-unavailable", events)
	}
	if events[0].CorrelationID != legCorr {
		t.Errorf("the event's correlation = %q, want the exchange's %q — an event that cannot be traced to its exchange explains nothing", events[0].CorrelationID, legCorr)
	}
	if events[0].LegType != "pas-claim-inquire" || events[0].Op != "pas-inquire" {
		t.Errorf("event = %+v, want it named to this leg and operation", events[0])
	}
	// The events the effect returns are emitted by the handler, not here, so
	// nothing was observed yet — the caller decides when they are seen.
	if len(seen) != 0 {
		t.Errorf("the effect emitted %d events itself; the handler emits them after the answer is sealed", len(seen))
	}
}

// TestPASInquire_SearchesByStrongKeysOnly: an answer stating all four key
// kinds decides a pend keyed on either STRONG kind (the payer's own ClaimResponse
// identifier, or its preAuthRef), and does NOT decide a pend keyed only on a weak
// kind (the requester's request identifier, or an item trace number): those are
// what reused example bodies share across claims, so they may confirm a match but
// never make one. A regression to any-key matching would decide the weak rows.
func TestPASInquire_SearchesByStrongKeysOnly(t *testing.T) {
	answer := inquiryAnswerOf(t, inquiryCR{id: "http://payer.example/cr|CR-9", request: "http://provider.example/claim|SUB-9",
		preAuthRef: "AUTH-9", traces: []string{"http://provider.example/trn|TRN-9"}, code: "A1"})
	for _, tc := range []struct {
		name    string
		keys    PendKeys
		decided bool
	}{
		{"the payer's own ClaimResponse identifier", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://payer.example/cr|CR-9"}}, true},
		{"the authorization reference", PendKeys{RequesterHolder: inquiryRequester, PreAuthRef: "AUTH-9"}, true},
		{"the identifier the payer echoed for the submitted claim", PendKeys{RequesterHolder: inquiryRequester, RequestIDs: []string{"http://provider.example/claim|SUB-9"}}, false},
		{"an item trace number", PendKeys{RequesterHolder: inquiryRequester, ItemTraceNumbers: []string{"http://provider.example/trn|TRN-9"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInquiryLedgerFixture(t, tc.keys)
			events := f.applyEvents(t, inquiryRequester, answer)
			got := f.state(t)
			if tc.decided {
				if got.State != PendStateDecided || got.Outcome != PendOutcomeApproved {
					t.Fatalf("a pend keyed on %s was not decided: %+v", tc.name, got)
				}
				wantNoUnmatched(t, events)
				return
			}
			if got.State != PendStatePended {
				t.Fatalf("a pend keyed only on %s was decided: %+v", tc.name, got)
			}
			wantUnmatched(t, events, "none=1")
		})
	}
	// Non-vacuous control: a pend keyed on a strong key the answer does not state
	// is not decided by the same answer, so each decided row above is the key it
	// names and not a match on anything else.
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://payer.example/cr|SOMETHING-ELSE"}})
	f.apply(t, inquiryRequester, answer)
	if got := f.state(t).State; got != PendStatePended {
		t.Fatalf("control: an unrelated key was decided (%q) — the rows above prove nothing", got)
	}
}

// inquiryCR is one ClaimResponse of a synthetic inquiry answer: its strong keys
// (id, preAuthRef), its weak keys (request, traces), its review action code (A1
// certified, A3 not certified, A4 pended) and its created date.
type inquiryCR struct {
	id, request, preAuthRef string
	traces                  []string
	code                    string
	created                 string
}

// inquiryAnswerOf builds a 2.0.1/2.1.0-shape answer carrying the given
// ClaimResponses, all about the fixture's member. Keys are "system|value".
func inquiryAnswerOf(t *testing.T, crs ...inquiryCR) []byte {
	t.Helper()
	ident := func(key string) map[string]any {
		sys, val, ok := strings.Cut(key, "|")
		if !ok {
			t.Fatalf("key %q is not system|value", key)
		}
		return map[string]any{"system": sys, "value": val}
	}
	var entries []any
	for _, c := range crs {
		created := c.created
		if created == "" {
			created = "2026-09-17T00:00:00Z"
		}
		outcome := "complete"
		if c.code == "A4" {
			outcome = "queued"
		}
		var itemExt []any
		for _, tr := range c.traces {
			itemExt = append(itemExt, map[string]any{
				"url":             "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-itemTraceNumber",
				"valueIdentifier": ident(tr)})
		}
		item := map[string]any{"itemSequence": 1, "adjudication": []any{map[string]any{
			"extension": []any{map[string]any{
				"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction",
				"extension": []any{map[string]any{
					"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode",
					"valueCodeableConcept": map[string]any{"coding": []any{map[string]any{
						"system": "https://codesystem.x12.org/005010/306", "code": c.code}}}}}}},
			"category": map[string]any{"coding": []any{map[string]any{
				"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}}}}}
		if len(itemExt) > 0 {
			item["extension"] = itemExt
		}
		cr := map[string]any{"resourceType": "ClaimResponse", "outcome": outcome, "created": created,
			"patient": map[string]any{"reference": "Patient/SubscriberExample"}, "item": []any{item}}
		if c.id != "" {
			cr["identifier"] = []any{ident(c.id)}
		}
		if c.request != "" {
			cr["request"] = map[string]any{"identifier": ident(c.request)}
		}
		if c.preAuthRef != "" {
			cr["preAuthRef"] = c.preAuthRef
		}
		entries = append(entries, map[string]any{"resource": cr})
	}
	b, err := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// applyEvents is apply, returning the observer events the ledger effect raised.
func (f *inquiryLedgerFixture) applyEvents(t *testing.T, requester string, answer []byte) []ObserverEvent {
	t.Helper()
	result := LegResult{}
	commit, events := f.g.inquiryLedgerEffect(requester, f.subject, "Patient/MBR-COVERED", "corr-inquiry-leg", f.facts, answer, &result)
	if commit != nil {
		// The write reports what it changed through the gateway's observer.
		f.g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
		defer func() { f.g.cfg.Observer = nil }()
		if err := commit(); err != nil {
			t.Fatalf("ledger write: %v", err)
		}
	}
	return events
}

// withFacts is the fixture over the same store, inquiring by trace number trn
// (system http://provider.example/trn) instead of the fixture's own.
func (f *inquiryLedgerFixture) withFacts(t *testing.T, trn string) *inquiryLedgerFixture {
	t.Helper()
	facts, status, msg := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", trn, "72148"))
	if status != 0 {
		t.Fatalf("fixture inquiry refused: %d %s", status, msg)
	}
	g := *f
	g.facts = facts
	return &g
}

// pend seeds another pended authorization of the fixture's member.
func (f *inquiryLedgerFixture) pend(t *testing.T, corr string, keys PendKeys) {
	t.Helper()
	if _, err := f.store.RecordPendedKeyed(f.subject, corr, fixedClock(), keys); err != nil {
		t.Fatalf("seed pend %s: %v", corr, err)
	}
}

// stateOf reads one authorization's ledger row.
func (f *inquiryLedgerFixture) stateOf(t *testing.T, corr string) PendRecord {
	t.Helper()
	rec, found, err := f.store.PendRecordOf(f.subject, corr)
	if err != nil || !found {
		t.Fatalf("ledger row %s: found=%v err=%v", corr, found, err)
	}
	return rec
}

// wantUnmatched asserts the inquiry raised exactly one pend.inquiry-unmatched
// event, tied to the inquiry's own exchange, whose Detail is want.
func wantUnmatched(t *testing.T, events []ObserverEvent, want string) {
	t.Helper()
	var got []ObserverEvent
	for _, e := range events {
		if e.Kind == PendInquiryUnmatchedEvent {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d %s events, want 1: %+v", len(got), PendInquiryUnmatchedEvent, events)
	}
	if got[0].Detail != want || got[0].CorrelationID != "corr-inquiry-leg" || got[0].LegType != "pas-claim-inquire" {
		t.Fatalf("%s = %+v, want Detail %q on the inquiry's exchange", PendInquiryUnmatchedEvent, got[0], want)
	}
}

// wantNoUnmatched asserts the inquiry raised no pend.inquiry-unmatched event.
func wantNoUnmatched(t *testing.T, events []ObserverEvent) {
	t.Helper()
	for _, e := range events {
		if e.Kind == PendInquiryUnmatchedEvent {
			t.Fatalf("unexpected %s: %+v", PendInquiryUnmatchedEvent, e)
		}
	}
}

// The example's item trace number, which every claim built from the IG example
// shares, and two claims of one member that both carry it.
const (
	exampleTrace = "http://example.org/ITEM_TRACE_NUMBER|1"
	corrClaimB   = "corr-claim-B"
	corrClaimC   = "corr-claim-C"
	crClaimB     = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|B-1"
	crClaimC     = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|C-1"
)

// TestPASInquire_SharedTraceNumberDecidesOnlyTheNamedClaim: two pended
// claims share the example's item trace number. A ClaimResponse naming one of them
// by its strong key decides that one only; a ClaimResponse carrying only the
// shared trace number decides neither, and is counted.
func TestPASInquire_SharedTraceNumberDecidesOnlyTheNamedClaim(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{exampleTrace}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimC}, ItemTraceNumbers: []string{exampleTrace}})

	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{traces: []string{exampleTrace}, code: "A3"}))
	for _, corr := range []string{corrClaimB, corrClaimC} {
		if got := f.stateOf(t, corr).State; got != PendStatePended {
			t.Fatalf("an answer naming no claim decided %s: %q", corr, got)
		}
	}
	wantUnmatched(t, events, "no-strong-key=1")

	events = f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{exampleTrace}, code: "A1"}))
	if got := f.stateOf(t, corrClaimB); got.State != PendStateDecided || got.Outcome != PendOutcomeApproved {
		t.Fatalf("claim B was not decided by its own ClaimResponse: %+v", got)
	}
	if got := f.stateOf(t, corrClaimC).State; got != PendStatePended {
		t.Fatalf("claim B's ClaimResponse decided claim C: %q", got)
	}
	wantNoUnmatched(t, events)
}

// TestPASInquire_ForeignDecisionCannotFlipAClaim: claim B is decided
// approved. A later-dated denial for claim C, which shares B's trace number and
// request identifier but is not in this requester's ledger (another requester's
// claim for the same example member, or one decided at submit), changes nothing of
// B's: not its outcome and not its EOB.
func TestPASInquire_ForeignDecisionCannotFlipAClaim(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const request = "http://example.org/claim|111099"
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, RequestIDs: []string{request}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, request: request, preAuthRef: "AUTH-B", traces: []string{inquiryTraceKey}, code: "A1", created: "2026-09-17T00:00:00Z"}))
	approved, ok := f.store.EOBByID(decisionEOBID(corrClaimB))
	if !ok {
		t.Fatal("claim B's decision wrote no EOB")
	}

	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimC, request: request, traces: []string{inquiryTraceKey}, code: "A3", created: "2026-09-20T00:00:00Z"}))
	got := f.stateOf(t, corrClaimB)
	wantRecordedDecision(t, got, PendOutcomeApproved)
	if eob, _ := f.store.EOBByID(decisionEOBID(corrClaimB)); !bytes.Equal(eob, approved) {
		t.Fatalf("claim C's denial replaced claim B's EOB:\n got: %s\nwant: %s", eob, approved)
	}
	wantUnmatched(t, events, "none=1")
}

// wantRecordedDecision asserts a decided row with outcome.
func wantRecordedDecision(t *testing.T, rec PendRecord, outcome string) {
	t.Helper()
	if rec.State != PendStateDecided || rec.Outcome != outcome {
		t.Fatalf("ledger row = %+v, want decided %s", rec, outcome)
	}
}

// TestPASInquire_LosingDecisionKeepsTheEOB: claim B is decided approved
// by a later-dated answer. An earlier-dated denial for B itself loses (the payer's
// later word stands), and it must not replace the EOB of the decision the ledger
// keeps: the ledger and Patient Access would otherwise disagree.
func TestPASInquire_LosingDecisionKeepsTheEOB(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", code: "A1", created: "2026-09-20T00:00:00Z"}))
	approved, ok := f.store.EOBByID(decisionEOBID(corrClaimB))
	if !ok {
		t.Fatal("claim B's decision wrote no EOB")
	}
	f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, code: "A3", created: "2026-09-17T00:00:00Z"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	if eob, _ := f.store.EOBByID(decisionEOBID(corrClaimB)); !bytes.Equal(eob, approved) {
		t.Fatalf("a losing denial replaced the kept decision's EOB:\n got: %s\nwant: %s", eob, approved)
	}
}

// TestPASInquire_SharedWeakKeyIsNotAmbiguous: two pended claims share the
// example's trace number. An answer naming claim B by its strong key and its
// request identifier, and stating the shared trace number too, decides B: a
// shared weak key confirms and never makes a match ambiguous.
func TestPASInquire_SharedWeakKeyIsNotAmbiguous(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const requestB = "http://provider.example/claim|SUB-B"
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, RequestIDs: []string{requestB}, ItemTraceNumbers: []string{exampleTrace}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimC}, RequestIDs: []string{"http://provider.example/claim|SUB-C"}, ItemTraceNumbers: []string{exampleTrace}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, request: requestB, preAuthRef: "AUTH-B", traces: []string{exampleTrace}, code: "A1"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	if got := f.stateOf(t, corrClaimC).State; got != PendStatePended {
		t.Fatalf("claim C was decided: %q", got)
	}
	wantNoUnmatched(t, events)
}

// TestPASInquire_AnotherRequestersClaimDecidesNothing: the payer's answer
// to requester X carries a ClaimResponse about requester Y's claim C, which shares
// X's claim B's trace number. X's lookup searches X's namespace by C's strong key,
// finds nothing, and B is untouched.
func TestPASInquire_AnotherRequestersClaimDecidesNothing(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{exampleTrace}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryOtherRequester, ClaimResponseIDs: []string{crClaimC}, ItemTraceNumbers: []string{exampleTrace}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimC, traces: []string{exampleTrace}, code: "A3"}))
	for _, corr := range []string{corrClaimB, corrClaimC} {
		if got := f.stateOf(t, corr).State; got != PendStatePended {
			t.Fatalf("another requester's ClaimResponse decided %s: %q", corr, got)
		}
	}
	wantUnmatched(t, events, "none=1")
}

// TestPASInquire_ADisagreeingKeyIsNoMatch: the strong key names claim B,
// but the answer states an item trace number B does not hold, so the answer is
// about another claim: B is untouched, and the event names the kind that
// disagreed. A kind the row holds none of is not a disagreement: B holds no
// preAuthRef (a payer states it on the decision, not on the pend), and an answer
// stating one with B's own keys decides B.
func TestPASInquire_ADisagreeingKeyIsNoMatch(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{"http://example.org/ITEM_TRACE_NUMBER|B"}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, traces: []string{"http://example.org/ITEM_TRACE_NUMBER|C"}, code: "A3"}))
	if got := f.stateOf(t, corrClaimB).State; got != PendStatePended {
		t.Fatalf("a disagreeing answer decided claim B: %q", got)
	}
	wantUnmatched(t, events, "disagrees=1 disagreed on "+PendKeyItemTraceNumber)

	events = f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B",
		traces: []string{"http://example.org/ITEM_TRACE_NUMBER|B"}, code: "A1"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	wantNoUnmatched(t, events)
}

// TestPASInquire_UnmatchedAreCountedByVerdict: one inquiry whose answer
// carries a ClaimResponse of every kind that cannot be recorded raises ONE event
// that counts each, never a bare skip, and still records the one it can.
func TestPASInquire_UnmatchedAreCountedByVerdict(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const sharedCR = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{"http://example.org/ITEM_TRACE_NUMBER|B"}})
	f.pend(t, "corr-reused-1", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{sharedCR}})
	f.pend(t, "corr-reused-2", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{sharedCR}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimC}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t,
		inquiryCR{traces: []string{exampleTrace}, code: "A1"},                                           // no strong key
		inquiryCR{id: "http://example.org/PATIENT_EVENT_TRACE_NUMBER|NOT-HERE", code: "A1"},             // none
		inquiryCR{id: sharedCR, code: "A1"},                                                             // ambiguous
		inquiryCR{id: crClaimB, traces: []string{"http://example.org/ITEM_TRACE_NUMBER|X"}, code: "A1"}, // disagrees
		inquiryCR{id: crClaimC, preAuthRef: "AUTH-C", code: "A1"},                                       // found
	))
	wantUnmatched(t, events, "no-strong-key=1 none=1 ambiguous=1 disagrees=1 disagreed on "+PendKeyItemTraceNumber)
	wantRecordedDecision(t, f.stateOf(t, corrClaimC), PendOutcomeApproved)
	for _, corr := range []string{corrClaimB, "corr-reused-1", "corr-reused-2"} {
		if got := f.stateOf(t, corr).State; got != PendStatePended {
			t.Fatalf("%s was decided: %q", corr, got)
		}
	}
}

// TestPASInquire_AnswerSubjectLinkage: every patient an answer names, at every
// depth, must be the same one. Reading only the ClaimResponses would pass an
// answer whose Coverage beneficiary, Patient entry or contained resource named a
// different member — one inquiry asks about one member's authorizations.
func TestPASInquire_AnswerSubjectLinkage(t *testing.T) {
	// The controls first: both published fixtures, and an answer that names no
	// authorization at all, are consistent.
	for _, name := range []string{"pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json"} {
		if !consistentPASInquiryAnswerSubjects(inquiryFixture(t, name)) {
			t.Fatalf("%s was judged inconsistent — every rejection row below would be vacuous", name)
		}
	}
	if !consistentPASInquiryAnswerSubjects([]byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)) {
		t.Fatal("an answer naming no authorization must be consistent: nothing matched is an answer")
	}
	for _, tc := range []struct{ name, body string }{
		{"two ClaimResponses about different patients", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/B"}}}]}`},
		{"a Coverage beneficiary naming another patient", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/B"}}}]}`},
		{"a Patient entry that is another patient", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"Patient","id":"B"}}]}`},
		{"a contained resource about another patient", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"},
			  "contained":[{"resourceType":"Task","id":"t1","for":{"reference":"Patient/B"}}]}}]}`},
		{"the second return Bundle is about another patient", `{"resourceType":"Parameters","parameter":[
			{"name":"return","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}}]}},
			{"name":"return","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/B"}}}]}}]}`},
		{"malformed", `{"resourceType":"Bundle"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				t.Fatal("an answer naming two patients was judged consistent")
			}
		})
	}
}

// TestPASInquire_AnswerSubjectReferenceForms: a reference is resolved the way
// FHIR says one is, not by reading the text after "Patient/". Every row here
// names TWO different patients in a form the substring reading called
// consistent — each one a way a second member reaches the requester.
func TestPASInquire_AnswerSubjectReferenceForms(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// The urn reference resolves to a Patient entry; the other ClaimResponse
		// names a patient that entry is not.
		{"a urn reference whose Patient entry has no id", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:pat-1","resource":{"resourceType":"Patient"}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:pat-1"}}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}}]}`},
		{"a urn reference with no Patient entry at all", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:pat-9"}}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}}]}`},
		{"an identifier-only logical reference to another member", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"identifier":{"system":"http://payer.example/mb","value":"OTHER"}}}}]}`},
		{"a typed Patient reference in a non-subject field", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"},
			  "extension":[{"url":"http://example.org/x","valueReference":{"type":"Patient","identifier":{"system":"http://payer.example/mb","value":"OTHER"}}}]}}]}`},
		{"the same id on two different servers", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"http://a.example/fhir/Patient/A","resource":{"resourceType":"Patient","id":"A"}},
			{"fullUrl":"http://b.example/fhir/Patient/A","resource":{"resourceType":"Patient","id":"A"}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"http://a.example/fhir/Patient/A"}}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"http://b.example/fhir/Patient/A"}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				members, _ := pasInquiryAnswerSubjects([]byte(tc.body))
				t.Fatalf("an answer naming two patients was judged consistent; identities read: %v", members)
			}
		})
	}
}

// TestPASInquire_AnswerSubjectIdentifiersNeverCollapseEntries: a business
// identifier NAMES a patient; it never decides that two of them are one.
//
// Within a Bundle the identity is the entry's address, so two Patient records are
// two patients however their identifiers read. Across Bundles the comparison is
// the FULL identifier set: any single-key rule would make the answer depend on
// which identifier sorts first, so whether two patients collapsed would turn on
// the spelling of an identifier system — no kind of property to rest a patient
// boundary on. Each row below is a shape a smallest-key rule called consistent.
func TestPASInquire_AnswerSubjectIdentifiersNeverCollapseEntries(t *testing.T) {
	// Two distinct Patient records in ONE Bundle that happen to state the same
	// member id, each answered about by its own ClaimResponse.
	const sharedMemberIDOneBundle = `{"resourceType":"Bundle","entry":[
		{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A","name":[{"family":"Alpha"}],
		  "identifier":[{"system":"http://payer.example/mb","value":"X"}]}},
		{"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"B","name":[{"family":"Beta"}],
		  "identifier":[{"system":"http://payer.example/mb","value":"X"}]}},
		{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a"}}},
		{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:b"}}}]}`

	// Two patients with DIFFERENT member ids that share a secondary identifier,
	// in two Bundles. The only difference between the rows is the system of that
	// secondary — one sorts before the member id, one after — and the verdict must
	// not depend on it.
	twoBundlesSharingASecondary := func(secondarySystem string) string {
		return `{"resourceType":"Parameters","parameter":[
			{"name":"return","resource":{"resourceType":"Bundle","entry":[
			  {"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A","identifier":[
			    {"system":"http://payer.example/mb","value":"X"},
			    {"system":"` + secondarySystem + `","value":"1"}]}},
			  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a"}}}]}},
			{"name":"return","resource":{"resourceType":"Bundle","entry":[
			  {"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"B","identifier":[
			    {"system":"http://payer.example/mb","value":"Y"},
			    {"system":"` + secondarySystem + `","value":"1"}]}},
			  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:b"}}}]}}]}`
	}

	for _, tc := range []struct{ name, body string }{
		{"two Patient records sharing a member id in one Bundle", sharedMemberIDOneBundle},
		// "http://a…" sorts BEFORE the member-id system: a smallest-key rule made
		// both patients that secondary and called them one.
		{"different members sharing a secondary that sorts first", twoBundlesSharingASecondary("http://a.example/dl")},
		// "http://z…" sorts AFTER it. The same pair, and the same verdict: the
		// answer cannot depend on the spelling.
		{"different members sharing a secondary that sorts last", twoBundlesSharingASecondary("http://z.example/dl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				members, _ := pasInquiryAnswerSubjects([]byte(tc.body))
				t.Fatalf("two patients were judged one; identities read: %v", members)
			}
		})
	}

	// The two secondary spellings must reach the SAME verdict. A rule that refused
	// one and accepted the other would be deciding a patient boundary on a sort
	// order, which is the defect these rows exist to hold shut.
	first := consistentPASInquiryAnswerSubjects([]byte(twoBundlesSharingASecondary("http://a.example/dl")))
	last := consistentPASInquiryAnswerSubjects([]byte(twoBundlesSharingASecondary("http://z.example/dl")))
	if first != last {
		t.Fatalf("the verdict changed with the identifier system's spelling: sorts-first=%v sorts-last=%v", first, last)
	}
}

// TestPASInquire_AnswerSubjectDistinctIdentifierSetsRefused: the same member id
// in two Bundles, where each Patient also carries a secondary the other does not,
// is refused. The sets differ, and a payer answering about one member under two
// different identifier sets is itself inconsistent — refusing is the safe reading
// of an answer this cannot tell apart from two patients.
func TestPASInquire_AnswerSubjectDistinctIdentifierSetsRefused(t *testing.T) {
	body := `{"resourceType":"Parameters","parameter":[
		{"name":"return","resource":{"resourceType":"Bundle","entry":[
		  {"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A","identifier":[
		    {"system":"http://payer.example/mb","value":"X"},
		    {"system":"http://a.example/dl","value":"1"}]}},
		  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a"}}}]}},
		{"name":"return","resource":{"resourceType":"Bundle","entry":[
		  {"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"B","identifier":[
		    {"system":"http://payer.example/mb","value":"X"},
		    {"system":"http://a.example/dl","value":"2"}]}},
		  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:b"}}}]}}]}`
	if consistentPASInquiryAnswerSubjects([]byte(body)) {
		members, _ := pasInquiryAnswerSubjects([]byte(body))
		t.Fatalf("two differing identifier sets were judged one patient; identities read: %v", members)
	}
}

// TestPASInquire_AnswerSubjectAmbiguousAddressRefused: when two entries claim one
// spelling, a reference using it names neither. Resolving it to whichever entry
// happened to be indexed last would attach the answer to a patient by accident of
// iteration order.
//
// Each of the THREE forms a collision takes is asserted at the guard — the answer
// is reported UNREADABLE — and not merely at the verdict. Two of them involve two
// Patient records, which the one-patient-per-Bundle rule already refuses, so a
// verdict-only row would stay green with the collision check gone and would pin
// nothing.
func TestPASInquire_AnswerSubjectAmbiguousAddressRefused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// Tracked by the per-Bundle address set.
		{"two entries sharing a fullUrl", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A"}},
			{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Organization","id":"O"}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a"}}}]}`},
		{"two entries sharing a Type/id under different fullUrls", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A"}},
			{"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"A"}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}}]}`},
		// Tracked by the registration collision, and the only form the address set
		// does not see: an identifier is not an address. Without the guard the
		// reference below resolves to whichever Patient was indexed last.
		{"one identifier stated by two Patient records, referenced by identifier", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A",
			  "identifier":[{"system":"http://payer.example/mb","value":"X"}]}},
			{"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"B",
			  "identifier":[{"system":"http://payer.example/mb","value":"X"}]}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"identifier":{"system":"http://payer.example/mb","value":"X"}}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members, ok := pasInquiryAnswerSubjects([]byte(tc.body))
			if ok {
				t.Fatalf("a spelling two entries claim was not reported unreadable; identities read: %v", members)
			}
			if consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				t.Fatal("an answer whose references name two entries at once was accepted")
			}
		})
	}
	// Non-vacuous control: the same shapes with the collisions removed resolve and
	// are consistent, so each refusal above is the collision and nothing else.
	control := `{"resourceType":"Bundle","entry":[
		{"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A",
		  "identifier":[{"system":"http://payer.example/mb","value":"X"}]}},
		{"fullUrl":"urn:uuid:o","resource":{"resourceType":"Organization","id":"O"}},
		{"resource":{"resourceType":"ClaimResponse","patient":{"identifier":{"system":"http://payer.example/mb","value":"X"}}}}]}`
	if !consistentPASInquiryAnswerSubjects([]byte(control)) {
		members, _ := pasInquiryAnswerSubjects([]byte(control))
		t.Fatalf("control: an unambiguous Bundle was refused; identities read: %v", members)
	}
}

// TestPASInquire_AnswerSubjectSelfAddressedEntryAccepted: an entry whose relative
// fullUrl IS its own `Type/id` states one address twice. That is a nonconformant
// fullUrl, not two entries claiming one spelling, and reading it as a collision
// would refuse an answer on a technicality that costs nobody anything.
func TestPASInquire_AnswerSubjectSelfAddressedEntryAccepted(t *testing.T) {
	body := `{"resourceType":"Bundle","entry":[
		{"fullUrl":"Patient/A","resource":{"resourceType":"Patient","id":"A"}},
		{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}}]}`
	members, ok := pasInquiryAnswerSubjects([]byte(body))
	if !ok {
		t.Fatalf("an entry addressed by its own Type/id was reported unreadable; identities read: %v", members)
	}
	if !consistentPASInquiryAnswerSubjects([]byte(body)) {
		t.Fatalf("an entry addressed by its own Type/id was refused; identities read: %v", members)
	}
}

// TestPASInquire_AnswerSubjectRepeatedIdentifierAccepted: a Patient stating one
// identifier twice states one identifier. Without deduplication its set would
// read differently from the same record written once, and one patient would be
// refused across two Bundles.
func TestPASInquire_AnswerSubjectRepeatedIdentifierAccepted(t *testing.T) {
	body := `{"resourceType":"Parameters","parameter":[
		{"name":"return","resource":{"resourceType":"Bundle","entry":[
		  {"fullUrl":"urn:uuid:a","resource":{"resourceType":"Patient","id":"A","identifier":[
		    {"system":"http://payer.example/mb","value":"X"},
		    {"system":"http://payer.example/mb","value":"X"}]}},
		  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a"}}}]}},
		{"name":"return","resource":{"resourceType":"Bundle","entry":[
		  {"fullUrl":"urn:uuid:b","resource":{"resourceType":"Patient","id":"B","identifier":[
		    {"system":"http://payer.example/mb","value":"X"}]}},
		  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:b"}}}]}}]}`
	if !consistentPASInquiryAnswerSubjects([]byte(body)) {
		members, _ := pasInquiryAnswerSubjects([]byte(body))
		t.Fatalf("a repeated identifier made one patient read as two; identities read: %v", members)
	}
}

// TestPASInquire_AnswerSubjectUnreadableSubjectRefused: a subject member that
// names nobody readable — a display string, or an identifier with no system — is
// refused rather than skipped. The submission walk refuses both, and a subject
// naming nobody is not an answer about one patient.
func TestPASInquire_AnswerSubjectUnreadableSubjectRefused(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a display-only subject", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"display":"Jane Roe"}}}]}`},
		{"an identifier with no system", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"identifier":{"value":"X"}}}}]}`},
		{"an identifier with no value", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"identifier":{"system":"http://payer.example/mb"}}}}]}`},
		{"a subject member that is not a Reference", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":"Patient/A"}}]}`},
		// A typed Patient reference OUTSIDE a subject member gets the same answer:
		// it says it points at a patient and then names none.
		{"a typed Patient reference with an incomplete identifier", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"},
			  "extension":[{"url":"http://example.org/x","valueReference":{"type":"Patient","identifier":{"value":"X"}}}]}}]}`},
		{"a typed Patient reference naming nothing at all", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"},
			  "extension":[{"url":"http://example.org/x","valueReference":{"type":"Patient","display":"Jane Roe"}}]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				t.Fatal("a subject naming nobody readable was accepted")
			}
		})
	}
}

// TestPASInquire_AnswerSubjectFormsAccepted: the other direction, which matters
// just as much — every one of these names ONE patient in a form that must not be
// refused. Turning away a conformant answer is the same defect as passing a
// nonconformant one.
func TestPASInquire_AnswerSubjectFormsAccepted(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// The version-specific reference names the same patient as the
		// version-less one.
		{"a version-specific reference beside a version-less one", `{"resourceType":"Bundle","entry":[
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A/_history/2"}}}]}`},
		{"a urn reference and a Type/id reference to the one entry", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:pat-1","resource":{"resourceType":"Patient","id":"A"}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:pat-1"}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/A"}}}]}`},
		{"an identifier-only reference to the Patient the Bundle carries", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:pat-1","resource":{"resourceType":"Patient","id":"A",
			  "identifier":[{"system":"http://payer.example/mb","value":"MEM-1"}]}},
			{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"identifier":{"system":"http://payer.example/mb","value":"MEM-1"}}}}]}`},
		// The 2.2.1 shape returns one Bundle per authorization, and a payer may
		// address the same patient differently in each. The member identifier it
		// carries in both is what says they are one patient.
		{"two return Bundles addressing one patient differently", `{"resourceType":"Parameters","parameter":[
			{"name":"return","resource":{"resourceType":"Bundle","entry":[
			  {"fullUrl":"urn:uuid:a1","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"http://payer.example/mb","value":"MEM-1"}]}},
			  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:a1"}}}]}},
			{"name":"return","resource":{"resourceType":"Bundle","entry":[
			  {"fullUrl":"urn:uuid:b2","resource":{"resourceType":"Patient","id":"p2","identifier":[{"system":"http://payer.example/mb","value":"MEM-1"}]}},
			  {"resource":{"resourceType":"ClaimResponse","patient":{"reference":"urn:uuid:b2"}}}]}}]}`},
		{"a contained Patient the ClaimResponse names by fragment", `{"resourceType":"Bundle","entry":[
			{"fullUrl":"urn:uuid:cr-1","resource":{"resourceType":"ClaimResponse",
			  "contained":[{"resourceType":"Patient","id":"p"}],
			  "patient":{"reference":"#p"}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !consistentPASInquiryAnswerSubjects([]byte(tc.body)) {
				members, _ := pasInquiryAnswerSubjects([]byte(tc.body))
				t.Fatalf("a conformant answer naming ONE patient was refused; identities read: %v", members)
			}
		})
	}
}

// TestIngressInquire_AnswerSubjectLinkageRefused: the provider-facing ingress
// refuses such an answer rather than handing it to the participant's system. The
// control proves the same route accepts a consistent answer from the same payer.
func TestIngressInquire_AnswerSubjectLinkageRefused(t *testing.T) {
	inconsistent := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
		{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/A"}}},
		{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/B"}}}]}`)
	if status, msg := inquireAnswerGuard(inconsistent); status != http.StatusBadGateway {
		t.Fatalf("status=%d msg=%q, want 502", status, msg)
	}
	if status, _ := inquireAnswerGuard(inquiryFixture(t, "pas-inquiry-response-2.0.json")); status != 0 {
		t.Fatal("control: a consistent published answer was refused — the row above would be vacuous")
	}
}

// inquireAnswerGuard is the pair of answer checks both hops apply, in the order
// the handlers apply them: the shape the operation definitions declare, then the
// subject linkage.
func inquireAnswerGuard(answer []byte) (int, string) {
	if bad := validatePASInquiryAnswer(answer); bad.Status != 0 {
		return bad.Status, bad.Message
	}
	if !consistentPASInquiryAnswerSubjects(answer) {
		return http.StatusBadGateway, "PAS inquiry answer has inconsistent patient linkage"
	}
	return 0, ""
}

// TestPASInquire_RequesterNamespace: the keys are namespaced by the requester that
// submitted the claim. Another participant asking with the same keys resolves to
// nothing — one participant can never decide another's authorization.
func TestPASInquire_RequesterNamespace(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	// Control: the requester that submitted it does decide it.
	control := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	control.apply(t, inquiryRequester, decidedAnswer(t))
	if got := control.state(t).State; got != PendStateDecided {
		t.Fatalf("control: the submitting requester did not decide it (%q) — the row below would be vacuous", got)
	}
	f.apply(t, inquiryOtherRequester, decidedAnswer(t))
	if got := f.state(t); got.State != PendStatePended || got.Outcome != "" {
		t.Fatalf("another requester's inquiry decided this authorization: %+v", got)
	}
}

// TestPASInquire_OtherSubjectNoLedgerChange: an answer that resolves to an
// authorization for a DIFFERENT patient than the one this inquiry is bound to
// changes nothing. The inquiry's authority covers one subject.
func TestPASInquire_OtherSubjectNoLedgerChange(t *testing.T) {
	store := NewMemStore()
	g := &Gateway{cfg: Config{Store: store, Clock: fixedClock}}
	facts, status, _ := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if status != 0 {
		t.Fatal("fixture inquiry refused")
	}
	const otherSubject, otherCorr = "pci:MBR-NOTCOVERED", "corr-other"
	if _, err := store.RecordPendedKeyed(otherSubject, otherCorr, fixedClock(), PendKeys{
		RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	result := LegResult{}
	commit, _ := g.inquiryLedgerEffect(inquiryRequester, "pci:MBR-COVERED", "Patient/MBR-COVERED", "corr-inquiry-leg", facts, decidedAnswer(t), &result)
	if commit != nil {
		t.Fatal("an answer about another patient's authorization produced a ledger write")
	}
	rec, _, _ := store.PendRecordOf(otherSubject, otherCorr)
	if rec.State != PendStatePended {
		t.Fatalf("another patient's authorization was decided: %+v", rec)
	}
}

// TestPASInquire_NoLedgerNoEffect: a Store with no pend ledger has no inquiry
// entry point at all. The answer relays and nothing is derived.
func TestPASInquire_NoLedgerNoEffect(t *testing.T) {
	g := &Gateway{cfg: Config{Store: storeWithoutLedger{inner: NewMemStore()}, Clock: fixedClock}}
	facts, status, _ := parsePASInquiryFacts(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"))
	if status != 0 {
		t.Fatal("fixture inquiry refused")
	}
	result := LegResult{}
	commit, events := g.inquiryLedgerEffect(inquiryRequester, "pci:MBR-COVERED", "Patient/MBR-COVERED", "corr-inquiry-leg", facts, decidedAnswer(t), &result)
	if commit != nil || len(events) != 0 || len(result.SideEffectFHIR) != 0 {
		t.Fatalf("a store with no ledger produced an effect: commit=%v events=%d sideEffects=%d", commit != nil, len(events), len(result.SideEffectFHIR))
	}
	// Non-vacuous control: the same answer against a ledger-bearing store DOES
	// produce a write, so the absence above is the missing ledger and nothing else.
	f := newInquiryLedgerFixture(t, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{inquiryCRKey}})
	if c, _ := f.g.inquiryLedgerEffect(inquiryRequester, f.subject, "Patient/MBR-COVERED", "corr-inquiry-leg", f.facts, decidedAnswer(t), &LegResult{}); c == nil {
		t.Fatal("control: a ledger-bearing store produced no write — the row above would be vacuous")
	}
}

// TestPASInquire_ItemLineForTheAnswer: the decision EOB's product coding comes
// from the inquiry line whose trace number the payer echoed, so an answer about
// the second line is not costed as the first.
func TestPASInquire_ItemLineForTheAnswer(t *testing.T) {
	facts := pasInquiryFacts{items: []pasInquiryItemFact{
		{traceNumber: "s|TRN-1", code: "72148"},
		{traceNumber: "s|TRN-2", code: "E0424"},
	}}
	if got := inquiryItemFor(facts, []string{"s|TRN-2"}).code; got != "E0424" {
		t.Errorf("echoed TRN-2 selected %q, want E0424", got)
	}
	if got := inquiryItemFor(facts, nil).code; got != "72148" {
		t.Errorf("an answer echoing no trace number selected %q, want the first line", got)
	}
	if got := inquiryItemFor(pasInquiryFacts{}, nil).code; got != "" {
		t.Errorf("an inquiry with no line selected %q", got)
	}
}

// assertJSONObject is a small readability guard for the fixture rows above: every
// fixture the leg reads must be one JSON object.
func assertJSONObject(t *testing.T, raw []byte) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("fixture is not one JSON object: %v", err)
	}
}

// TestPASInquire_NotProfileValidatedButObserved pins WHAT LOOKS AT an inbound
// peer's inquiry, because the answer is "no enforcing $validate does" and that is
// a fact worth being executable rather than assumed.
//
// Nothing on this path refuses an inquiry, or a payer's answer, for failing its
// profile: the gateway preserves a peer's bytes and does not certify content it
// did not produce, the same posture the submit and update legs take. The payer's
// own system is what certifies an inquiry it receives, and the certification lane
// records an observation of both directions — at the inquiry's OWN profiles, at
// every candidate line, which is what makes a line-specific defect (a 2.0.1
// inquiry naming no item, say) visible as evidence without refusing the peer.
func TestPASInquire_NotProfileValidatedButObserved(t *testing.T) {
	request := inquiryFixture(t, "pas-inquiry-request-2.0.json")
	answer := inquiryFixture(t, "pas-inquiry-response-2.0.json")

	// The request and the Bundle-shaped answer are both recognised, and each
	// resolves to the INQUIRY profile rather than the submit leg's.
	for _, tc := range []struct{ name, species, want string }{
		{"the inquiry request", detectSpecies(request), "profile-pas-inquiry-request-bundle"},
		{"the answer Bundle", detectSpecies(answer), "profile-pas-inquiry-response-bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.species == "" {
				t.Fatalf("%s is not a recognised species — nothing would observe it", tc.name)
			}
			for _, line := range []string{"2.0", "2.1", "2.2"} {
				profile, ok := profileFor(tc.species, line, "pas-claim-inquire")
				if !ok {
					t.Fatalf("line %s: no certification profile for %s", line, tc.name)
				}
				if !strings.Contains(profile, tc.want) {
					t.Errorf("line %s: profile %q, want the inquiry profile %s", line, profile, tc.want)
				}
			}
		})
	}
	// The 2.2.1 answer's Parameters wrapper is deliberately NOT observed: the IG
	// governs it by its operation definition and declares no profile for it, so
	// there is nothing to certify it against.
	if got := detectSpecies(inquiryFixture(t, "pas-inquiry-response-2.2.json")); got != "" {
		t.Errorf("the 2.2.1 Parameters wrapper resolved to species %q; the IG declares no profile for it", got)
	}
}

// TestPASInquire_FixturesAreSynthetic: the inquiry fixtures this leg's rows read
// say in their own bytes that they are synthetic. No real payer has answered an
// inquiry yet, and a fixture that did not say so could be mistaken for a recorded
// answer by the later step that compares a real one against it.
func TestPASInquire_FixturesAreSynthetic(t *testing.T) {
	for _, name := range []string{"pas-inquiry-request-2.0.json", "pas-inquiry-response-2.0.json", "pas-inquiry-response-2.2.json"} {
		raw := inquiryFixture(t, name)
		assertJSONObject(t, raw)
		if !bytes.Contains(raw, []byte("Synthetic relay fixture")) {
			t.Errorf("%s does not say in its own narrative that it is synthetic", name)
		}
	}
}

// eob reads one authorization's decision EOB from the fixture store.
func (f *inquiryLedgerFixture) eob(t *testing.T, corr string) ([]byte, bool) {
	t.Helper()
	return f.store.EOBByID(decisionEOBID(corr))
}

// wantEOBWithheld asserts the inquiry raised one pend.inquiry-eob-withheld
// event counting n decisions, tied to the inquiry's own exchange.
func wantEOBWithheld(t *testing.T, events []ObserverEvent, n int) {
	t.Helper()
	var got []ObserverEvent
	for _, e := range events {
		if e.Kind == PendInquiryEOBWithheldEvent {
			got = append(got, e)
		}
	}
	want := fmt.Sprintf("recorded-without-eob=%d", n)
	if len(got) != 1 || got[0].Detail != want || got[0].CorrelationID != "corr-inquiry-leg" {
		t.Fatalf("%s events = %+v, want one with Detail %q on the inquiry's exchange", PendInquiryEOBWithheldEvent, got, want)
	}
}

// TestPASInquire_AnotherClaimsDecisionNeverTakesTheInquirysLines: an inquiry
// asks about claim B by B's trace number, which claim C shares (both built from
// one example body). The payer's answer carries C's decision. C's decision is
// recorded, by C's own ClaimResponse identifier, but its EOB is not built: the
// inquiry's line (B's product coding) does not name C alone, so C's EOB would
// state B's code.
func TestPASInquire_AnotherClaimsDecisionNeverTakesTheInquirysLines(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.pend(t, corrClaimC, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimC}, ItemTraceNumbers: []string{inquiryTraceKey}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimC, preAuthRef: "AUTH-C", traces: []string{inquiryTraceKey}, code: "A1"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimC), PendOutcomeApproved)
	if eob, ok := f.eob(t, corrClaimC); ok {
		t.Fatalf("claim C's decision EOB was built from the inquiry's line, which is claim B's: %s", eob)
	}
	if got := f.stateOf(t, corrClaimB).State; got != PendStatePended {
		t.Fatalf("claim B was decided: %q", got)
	}
	wantEOBWithheld(t, events, 1)
}

// TestPASInquire_OmittedClaimLendsNoLines: an inquiry asks about claim D by D's
// own trace number. The payer's answer omits D's ClaimResponse and carries claim
// B's decision instead. B is decided by its own key, and gets no EOB: the
// inquiry's line is D's, and B does not hold it (or, where B shares it with D,
// does not hold it alone).
func TestPASInquire_OmittedClaimLendsNoLines(t *testing.T) {
	const traceD = "http://provider.example/trn|TRN-1"
	for _, tc := range []struct {
		name   string
		traceB string
	}{
		{"B holds another trace number", "http://provider.example/trn|TRN-B"},
		{"B shares D's trace number", traceD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInquiryLedgerFixture(t, PendKeys{})
			f.pend(t, "corr-claim-D", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|D-1"}, ItemTraceNumbers: []string{traceD}})
			f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{tc.traceB}})
			events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{tc.traceB}, code: "A1"}))
			wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
			if eob, ok := f.eob(t, corrClaimB); ok {
				t.Fatalf("claim B's EOB was built from claim D's inquiry: %s", eob)
			}
			wantEOBWithheld(t, events, 1)
		})
	}
}

// TestPASInquire_ALaterAttributableInquirySuppliesTheEOB: claim B's decision is
// first learned from an inquiry about another claim, and recorded without an
// EOB. A later inquiry about B itself, restating the same decision, writes B's
// EOB: the decision the ledger keeps is the one it states.
func TestPASInquire_ALaterAttributableInquirySuppliesTheEOB(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, "corr-claim-D", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|D-1"}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{"http://provider.example/trn|TRN-B"}})
	answer := inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{"http://provider.example/trn|TRN-B"}, code: "A1"})
	f.applyEvents(t, inquiryRequester, answer)
	if _, ok := f.eob(t, corrClaimB); ok {
		t.Fatal("the first inquiry was not about claim B, but wrote its EOB")
	}

	// The same answer, to an inquiry about B by B's own trace number.
	about := f.withFacts(t, "TRN-B")
	events := about.applyEvents(t, inquiryRequester, answer)
	if _, ok := f.eob(t, corrClaimB); !ok {
		t.Fatal("an inquiry about claim B restating its kept decision did not supply the EOB")
	}
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	for _, e := range events {
		if e.Kind == PendInquiryEOBWithheldEvent {
			t.Fatalf("the attributable inquiry withheld an EOB: %+v", e)
		}
	}
}

// TestPASInquire_AnOlderAttributableAnswerDoesNotOverwrite: claim B's EOB states
// the decision the ledger keeps. An attributable answer restating the same
// outcome but dated earlier does not replace it.
func TestPASInquire_AnOlderAttributableAnswerDoesNotOverwrite(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B-LATER", traces: []string{inquiryTraceKey}, code: "A1", created: "2026-09-20T00:00:00Z"}))
	kept, ok := f.eob(t, corrClaimB)
	if !ok {
		t.Fatal("claim B's attributable decision wrote no EOB")
	}
	f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B-EARLIER", traces: []string{inquiryTraceKey}, code: "A1", created: "2026-09-17T00:00:00Z"}))
	if eob, _ := f.eob(t, corrClaimB); !bytes.Equal(eob, kept) {
		t.Fatalf("an older answer replaced the kept decision's EOB:\n got: %s\nwant: %s", eob, kept)
	}
}

// TestPASInquire_AnUnattributableFlipRemovesTheStaleEOB: claim B was decided
// denied by an inquiry about B, with its EOB. A later-dated approval for B then
// arrives in the answer to an inquiry about another claim, so it brings no EOB
// of its own. The ledger keeps the approval, and B's denial EOB is removed:
// Patient Access shows nothing for B rather than a denial the ledger no longer
// keeps, until an inquiry about B supplies the approval's EOB.
func TestPASInquire_AnUnattributableFlipRemovesTheStaleEOB(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const traceB = "http://provider.example/trn|TRN-B"
	f.pend(t, "corr-claim-D", PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|D-1"}, ItemTraceNumbers: []string{inquiryTraceKey}})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{traceB}})
	aboutB := f.withFacts(t, "TRN-B")
	aboutB.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, traces: []string{traceB}, code: "A3", created: "2026-09-17T00:00:00Z"}))
	if _, ok := f.eob(t, corrClaimB); !ok {
		t.Fatal("claim B's denial, from an inquiry about B, wrote no EOB")
	}

	// The approval, in an answer to the inquiry about D.
	approval := inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{traceB}, code: "A1", created: "2026-09-20T00:00:00Z"})
	wantEOBRemoved(t, f.applyEvents(t, inquiryRequester, approval), corrClaimB)
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	if eob, ok := f.eob(t, corrClaimB); ok {
		t.Fatalf("Patient Access still serves claim B's denial after the ledger kept its approval: %s", eob)
	}

	// An inquiry about B restates the approval and supplies its EOB; nothing
	// is removed.
	wantEOBRemoved(t, aboutB.applyEvents(t, inquiryRequester, approval))
	eob, ok := f.eob(t, corrClaimB)
	if !ok || !bytes.Contains(eob, []byte("AUTH-B")) {
		t.Fatalf("the inquiry about B did not supply the approval's EOB: %s (found=%v)", eob, ok)
	}
}

// wantEOBRemoved asserts the inquiry raised exactly one pend.eob-removed event
// per authorization in corrs, each tied to the inquiry's own exchange.
func wantEOBRemoved(t *testing.T, events []ObserverEvent, corrs ...string) {
	t.Helper()
	var got []string
	for _, e := range events {
		if e.Kind == PendEOBRemovedEvent {
			if e.CorrelationID != "corr-inquiry-leg" {
				t.Fatalf("%s event on exchange %q, want the inquiry's own", PendEOBRemovedEvent, e.CorrelationID)
			}
			got = append(got, e.Detail)
		}
	}
	var want []string
	for _, c := range corrs {
		want = append(want, "authorization "+c)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s events = %q, want %q", PendEOBRemovedEvent, got, want)
	}
}

// TestPASInquire_WithheldCountsOnlyAMissingEOB: a decision already filed with
// its EOB, restated in an answer to an inquiry about another claim, is not
// counted as withheld again.
func TestPASInquire_WithheldCountsOnlyAMissingEOB(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const traceB = "http://provider.example/trn|TRN-B"
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{traceB}})
	answer := inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{traceB}, code: "A1"})
	f.withFacts(t, "TRN-B").applyEvents(t, inquiryRequester, answer)
	if _, ok := f.eob(t, corrClaimB); !ok {
		t.Fatal("claim B's decision, from an inquiry about B, wrote no EOB")
	}
	for _, e := range f.applyEvents(t, inquiryRequester, answer) {
		if e.Kind == PendInquiryEOBWithheldEvent {
			t.Fatalf("a decision that has its EOB was counted as withheld: %+v", e)
		}
	}
}

// TestPASInquire_ANewClaimResponseForTheDecisionStillDecides: a payer that
// issues a new ClaimResponse, with a new identifier, for its decision, and
// repeats the authorization number it gave on the pend. The authorization
// number names the claim, so the decision is recorded.
func TestPASInquire_ANewClaimResponseForTheDecisionStillDecides(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB + "-PEND"}, PreAuthRef: "AUTH-B", ItemTraceNumbers: []string{inquiryTraceKey}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB + "-DECISION", preAuthRef: "AUTH-B", traces: []string{inquiryTraceKey}, code: "A1"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	wantNoUnmatched(t, events)
}

// TestPASInquire_AnEchoedClaimIdentifierNamesNoClaim: a payer that echoes the
// submitted claim's identifier as its ClaimResponse identifier. Claim B is pended
// under that echo; claim C, built from the same body and decided at submit, is
// in no ledger row. The payer's answer carries C's denial, under the same echoed
// identifier and trace number, with C's own authorization number. It must not be
// recorded on B: the echo is the requester's identifier, not the payer's.
func TestPASInquire_AnEchoedClaimIdentifierNamesNoClaim(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	const echoed = "http://example.org/PATIENT_EVENT_TRACE_NUMBER|111099"
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{echoed}, RequestIDs: []string{echoed}, ItemTraceNumbers: []string{inquiryTraceKey}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: echoed, request: echoed, preAuthRef: "AUTH-C", traces: []string{inquiryTraceKey}, code: "A3"}))
	if got := f.stateOf(t, corrClaimB).State; got != PendStatePended {
		t.Fatalf("claim C's denial was recorded on claim B through the echoed identifier: %q", got)
	}
	wantUnmatched(t, events, "requester-key-only=1")
}

// TestPASInquire_ASubmitDecidedClaimLendsNoLines: claim D was decided at submit
// (no pend), and claim B is pended; both carry the inquiry's trace number. An
// inquiry about D is answered with B's decision under B's own key. B is decided,
// but its EOB is not built from D's line: D's submit-time decision filed D's keys
// too, so the inquiry's trace does not name B alone.
func TestPASInquire_ASubmitDecidedClaimLendsNoLines(t *testing.T) {
	f := newInquiryLedgerFixture(t, PendKeys{})
	if _, err := f.store.RecordDecision(f.subject, "corr-claim-D", PendOutcomeDenied, fixedClock(),
		PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{"http://example.org/PATIENT_EVENT_TRACE_NUMBER|D-1"}, ItemTraceNumbers: []string{inquiryTraceKey}}, nil); err != nil {
		t.Fatal(err)
	}
	f.pend(t, corrClaimB, PendKeys{RequesterHolder: inquiryRequester, ClaimResponseIDs: []string{crClaimB}, ItemTraceNumbers: []string{inquiryTraceKey}})
	events := f.applyEvents(t, inquiryRequester, inquiryAnswerOf(t, inquiryCR{id: crClaimB, preAuthRef: "AUTH-B", traces: []string{inquiryTraceKey}, code: "A1"}))
	wantRecordedDecision(t, f.stateOf(t, corrClaimB), PendOutcomeApproved)
	if eob, ok := f.eob(t, corrClaimB); ok {
		t.Fatalf("claim B's EOB was built from the line of claim D, decided at submit: %s", eob)
	}
	wantEOBWithheld(t, events, 1)
}

// TestPASInquiryFacts_AnUnkeyedLineIsAboutNothing: an inquiry with a line that
// states neither a trace number nor an authorization number states nothing to be
// about, so no decision's EOB is built from its lines.
func TestPASInquiryFacts_AnUnkeyedLineIsAboutNothing(t *testing.T) {
	keyed := pasInquiryItemFact{traceNumber: inquiryTraceKey, code: "72148"}
	unkeyed := pasInquiryItemFact{code: "99999"}
	if got := (pasInquiryFacts{items: []pasInquiryItemFact{keyed}}).aboutKeys(); len(got) != 1 {
		t.Fatalf("a keyed inquiry's about keys = %+v, want its trace number", got)
	}
	for _, items := range [][]pasInquiryItemFact{{unkeyed, keyed}, {keyed, unkeyed}, nil} {
		if got := (pasInquiryFacts{items: items}).aboutKeys(); got != nil {
			t.Fatalf("lines %+v: about keys = %+v, want none", items, got)
		}
	}
}
