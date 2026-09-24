package engine

import (
	"bytes"
	"context"
	"encoding/json"
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

// assertJSONObject is a small readability guard for the fixture rows above: every
// fixture the leg reads must be one JSON object.
func assertJSONObject(t *testing.T, raw []byte) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("fixture is not one JSON object: %v", err)
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
