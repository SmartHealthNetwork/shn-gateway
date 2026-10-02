package engine

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInspectNativePASResponsePreservesIngressContract(t *testing.T) {
	approved := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	for name, b := range map[string][]byte{"pending": []byte(assemblyRealPending), "approved": approved} {
		facts, err := InspectNativePASResponse(b)
		if err != nil || !facts.ReferencesComplete || facts.ClaimResponseID == "" || facts.Decision == "" {
			t.Fatalf("%s: %+v %v", name, facts, err)
		}
		got, result := validateNativePASResponse(b)
		if result.Status != 0 || !bytes.Equal(got, b) {
			t.Fatal(result)
		}
	}
	// A ClaimResponse the payer placed under a urn:uuid fullUrl without an id is
	// identified by that fullUrl; the facts carry the payer's own identifier and
	// the fullUrl, and no id is minted for it.
	b := assemblySmallGraph()
	e := b["entry"].([]any)[0].(map[string]any)
	e["fullUrl"] = "urn:uuid:10000000-0000-4000-8000-000000000001"
	cr := e["resource"].(map[string]any)
	delete(cr, "id")
	cr["identifier"] = []any{map[string]any{"system": "https://payer.test/pa", "value": "PA-7781"}}
	cr["outcome"] = "queued"
	cr["patient"] = map[string]any{"reference": "https://payer.test/fhir/Patient/p"}
	cr["request"] = map[string]any{"reference": "https://payer.test/fhir/Claim/c"}
	cr["item"] = []any{map[string]any{"itemSequence": 1, "adjudication": []any{map[string]any{
		"category":  map[string]any{"coding": []any{map[string]any{"system": "http://terminology.hl7.org/CodeSystem/adjudication", "code": "submitted"}}},
		"extension": []any{map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction", "extension": []any{map[string]any{"url": "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode", "valueCodeableConcept": map[string]any{"coding": []any{map[string]any{"system": "https://codesystem.x12.org/005010/306", "code": "A4"}}}}}}},
	}}}}
	idless, _ := json.Marshal(b)
	facts, err := InspectNativePASResponse(idless)
	if err != nil {
		t.Fatalf("id-less ClaimResponse under urn:uuid refused: %v", err)
	}
	if facts.Decision != "pending" || facts.ClaimResponseID != "" || facts.ClaimResponseIdentity != "urn:uuid:10000000-0000-4000-8000-000000000001" || facts.ClaimResponseIdentifier != "https://payer.test/pa|PA-7781" || !facts.ReferencesComplete {
		t.Fatalf("facts do not carry the payer's own identity: %+v", facts)
	}
	if got, result := validateNativePASResponse(idless); result.Status != 0 || !bytes.Equal(got, idless) {
		t.Fatalf("id-less answer not relayed unchanged: %+v", result)
	}
	withID := pasBundleWithResponse(t, []byte(assemblyRealPending), []byte(assemblyRealTerminal))
	facts, err = InspectNativePASResponse(withID)
	if err != nil || facts.ClaimResponseID != "1770" || facts.ClaimResponseIdentity != "http://localhost:8081/fhir/ClaimResponse/1770" || facts.ClaimResponseIdentifier != "http://example.org/PATIENT_EVENT_TRACE_NUMBER|2ca2e10c-e924-4e12-9d69-5b71e4e91c31" {
		t.Fatalf("facts for an id-bearing ClaimResponse: %+v %v", facts, err)
	}
	for _, b := range [][]byte{[]byte(`{}`), []byte(`{"resourceType":"ClaimResponse","id":"bare","outcome":"queued"}`)} {
		if _, err := InspectNativePASResponse(b); err == nil {
			t.Fatal("accepted invalid graph")
		}
		if _, r := validateNativePASResponse(b); r.Status != 502 {
			t.Fatal(r)
		}
	}
}

// NativePASUnresolvedReference names the reference the response graph rule
// refuses, by the type of the resource holding it, never by that resource's
// id or the address the reference resolved to.
func TestNativePASUnresolvedReferenceNamesTheReference(t *testing.T) {
	encode := func(b map[string]any) []byte { raw, _ := json.Marshal(b); return raw }
	open := assemblySmallGraph()
	open["entry"] = open["entry"].([]any)[:1]
	holder, element, reference, ok := NativePASUnresolvedReference(encode(open))
	if !ok || holder != "ClaimResponse" || element != "/patient" || reference != "Patient/p" {
		t.Fatalf("open graph: %q %q %q %v", holder, element, reference, ok)
	}
	meta := assemblySmallGraph()
	meta["signature"] = map[string]any{"who": map[string]any{"reference": "Organization/signer"}}
	if holder, element, reference, ok = NativePASUnresolvedReference(encode(meta)); !ok || holder != "Bundle" || element != "/signature/who" || reference != "Organization/signer" {
		t.Fatalf("Bundle metadata: %q %q %q %v", holder, element, reference, ok)
	}
	idless := assemblySmallGraph()
	idless["entry"] = idless["entry"].([]any)[:1]
	first := idless["entry"].([]any)[0].(map[string]any)
	first["fullUrl"] = "urn:uuid:9d2c1b7e-0000-4000-8000-000000000001"
	delete(first["resource"].(map[string]any), "id")
	first["resource"].(map[string]any)["patient"] = map[string]any{"reference": "urn:uuid:9d2c1b7e-0000-4000-8000-000000000002"}
	if holder, _, _, ok = NativePASUnresolvedReference(encode(idless)); !ok || holder != "ClaimResponse" {
		t.Fatalf("id-less holder: %q %v", holder, ok)
	}
	// A graph that resolves, and one refused for a reason that names no
	// reference, name none.
	for name, b := range map[string][]byte{"closed": encode(assemblySmallGraph()), "not a Bundle": []byte(`{"resourceType":"Parameters"}`)} {
		if h, e, r, ok := NativePASUnresolvedReference(b); ok || h != "" || e != "" || r != "" {
			t.Fatalf("%s: %q %q %q %v", name, h, e, r, ok)
		}
	}
}

// The exported subject rule is the inquiry leg's own: one patient across every
// response Bundle reads consistent, two do not.
func TestConsistentPASInquiryAnswerSubjectsIsTheLegRule(t *testing.T) {
	bundle := func(id, min string) map[string]any {
		return map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{
			map[string]any{"fullUrl": "https://payer.test/fhir/ClaimResponse/cr-" + id, "resource": map[string]any{"resourceType": "ClaimResponse", "id": "cr-" + id, "patient": map[string]any{"reference": "Patient/" + id}}},
			map[string]any{"fullUrl": "https://payer.test/fhir/Patient/" + id, "resource": map[string]any{"resourceType": "Patient", "id": id, "identifier": []any{map[string]any{"system": "http://example.org/MIN", "value": min}}}},
		}}
	}
	answer := func(bs ...map[string]any) []byte {
		var params []any
		for _, b := range bs {
			params = append(params, map[string]any{"name": "return", "resource": b})
		}
		raw, _ := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": params})
		return raw
	}
	one, two := answer(bundle("p", "1"), bundle("q", "1")), answer(bundle("p", "1"), bundle("q", "2"))
	if !ConsistentPASInquiryAnswerSubjects(one) || ConsistentPASInquiryAnswerSubjects(two) {
		t.Fatalf("one patient %v, two patients %v", ConsistentPASInquiryAnswerSubjects(one), ConsistentPASInquiryAnswerSubjects(two))
	}
	if got, want := ConsistentPASInquiryAnswerSubjects(two), consistentPASInquiryAnswerSubjects(two); got != want {
		t.Fatal("the export is not the leg's rule")
	}
}

// The response graph rule holds for a Bundle with no ClaimResponse as for one
// with: a collection, every entry a resource under an absolute fullUrl, every
// reference resolving.
func TestCheckNativePASGraphWithoutClaimResponse(t *testing.T) {
	encode := func(b map[string]any) []byte { raw, _ := json.Marshal(b); return raw }
	patient := func() map[string]any {
		return map[string]any{"fullUrl": "https://payer.test/fhir/Patient/p", "resource": map[string]any{"resourceType": "Patient", "id": "p"}}
	}
	bundle := func(typ string, entries ...any) map[string]any {
		return map[string]any{"resourceType": "Bundle", "type": typ, "entry": entries}
	}
	if err := CheckNativePASGraphWithoutClaimResponse(encode(bundle("collection", patient()))); err != nil {
		t.Fatalf("a closed Bundle: %v", err)
	}
	dangling := patient()
	dangling["resource"].(map[string]any)["managingOrganization"] = map[string]any{"reference": "Organization/o"}
	for name, b := range map[string][]byte{
		"a ClaimResponse":      encode(assemblySmallGraph()),
		"not a collection":     encode(bundle("searchset", patient())),
		"an entry no fullUrl":  encode(bundle("collection", map[string]any{"resource": map[string]any{"resourceType": "Patient", "id": "p"}})),
		"an entry no resource": encode(bundle("collection", map[string]any{"fullUrl": "https://payer.test/fhir/Patient/p"})),
		"a dangling reference": encode(bundle("collection", dangling)),
		"no entry":             encode(bundle("collection")),
	} {
		if CheckNativePASGraphWithoutClaimResponse(b) == nil {
			t.Errorf("%s: read", name)
		}
	}
	if holder, element, reference, ok := NativePASUnresolvedReference(encode(bundle("collection", dangling))); !ok || holder != "Patient" || element != "/managingOrganization" || reference != "Organization/o" {
		t.Fatalf("the dangling reference named %q %q %q %v", holder, element, reference, ok)
	}
}
