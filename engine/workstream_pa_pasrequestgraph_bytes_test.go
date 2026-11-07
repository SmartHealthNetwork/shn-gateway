//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// The provider-data evidence step completes a request the build already
// encoded: it adds the clinical records the request references, read from
// the participant's system, and changes nothing else. The request's own bytes
// (a participant's record carried as written among them: member order,
// whitespace, characters a JSON encoder would escape) stay exactly as they
// were, and each record it adds rides exactly as it was read.
func TestPASRequestEvidence_KeepsTheRequestsBytes(t *testing.T) {
	const head = `{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"id":"p","resourceType":"Patient","text":{"status":"generated","div":"<div>a &amp; b</div>"}}},` + "\n  " +
		`{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest", "id":"s","subject":{"reference":"Patient/p"}`
	withEvidence := []byte(head + `,"supportingInfo":[{"reference":"ClinicalImpression/c"}]}}]}`)
	without := []byte(head + `}}]}`)
	clinical := []byte(`{"subject":{"reference":"Patient/p"},"resourceType":"ClinicalImpression","id":"c","note":[{"text":"x < y"}]}`)
	read := func(_ context.Context, ref string) ([]byte, bool, error) {
		if ref == "ClinicalImpression/c" {
			return clinical, true, nil
		}
		return nil, false, fmt.Errorf("unexpected read %s", ref)
	}

	got, err := retainPASRequestEvidence(context.Background(), without, read)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, without) {
		t.Fatalf("a request with no evidence to add changed:\n got %s\nwant %s", got, without)
	}

	got, err = retainPASRequestEvidence(context.Background(), withEvidence, read)
	if err != nil {
		t.Fatal(err)
	}
	closeAt := bytes.LastIndexByte(withEvidence, ']')
	if !bytes.HasPrefix(got, withEvidence[:closeAt]) || !bytes.HasSuffix(got, withEvidence[closeAt:]) {
		t.Fatalf("the request's own bytes changed:\n got %s\nwant %s plus an entry", got, withEvidence)
	}
	added := got[closeAt : len(got)-len(withEvidence[closeAt:])]
	want := []byte(`,{"fullUrl":"https://example.test/fhir/ClinicalImpression/c","resource":` + string(clinical) + `}`)
	if !bytes.Equal(added, want) {
		t.Fatalf("the added entry is %s, want the record as read: %s", added, want)
	}
}

// A record read from the participant's system that is not UTF-8 is refused,
// never spliced: the checks read decoded text (invalid bytes read as U+FFFD),
// so the bytes sent would not be the text checked.
func TestPASRequestEvidence_RefusesARecordThatIsNotUTF8(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"https://example.test/fhir/Patient/p","resource":{"resourceType":"Patient","id":"p"}},` +
		`{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p"},"supportingInfo":[{"reference":"ClinicalImpression/c"}]}}]}`)
	clinical := []byte("{\"resourceType\":\"ClinicalImpression\",\"id\":\"c\",\"subject\":{\"reference\":\"Patient/p\"},\"note\":[{\"text\":\"\xff\"}]}")
	_, err := retainPASRequestEvidence(context.Background(), body, func(context.Context, string) ([]byte, bool, error) { return clinical, true, nil })
	if !errors.Is(err, errRecordNotCarried) {
		t.Fatalf("a record that is not UTF-8: %v, want the fidelity refusal errRecordNotCarried", err)
	}
}

// The order's requester records are spliced into the request's bytes, which
// stay as built; each rides as read. One that is not UTF-8 is refused.
func TestCarryOrderRequester_KeepsTheRequestsBytes(t *testing.T) {
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"https://example.test/fhir/ServiceRequest/s","resource":{"id":"s","resourceType":"ServiceRequest","requester":{"reference":"PractitionerRole/r"},"note":[{"text":"a < b & c"}]}}` + "\n]}")
	role := []byte(`{"resourceType":"PractitionerRole","id":"r","practitioner":{"reference":"Practitioner/x"}}`)
	got, err := carryOrderRequester(body, [][]byte{role})
	if err != nil {
		t.Fatal(err)
	}
	closeAt := bytes.LastIndexByte(body, ']')
	want := string(body[:closeAt]) + `,{"fullUrl":"https://example.test/fhir/PractitionerRole/r","resource":` + string(role) + `}` + string(body[closeAt:])
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	bad := []byte("{\"resourceType\":\"PractitionerRole\",\"id\":\"r\",\"text\":{\"div\":\"\xff\"}}")
	if _, err := carryOrderRequester(body, [][]byte{bad}); !errors.Is(err, errRecordNotCarried) || !isPASBuildFidelity(err) {
		t.Fatalf("a requester record that is not UTF-8: %v, want the build's fidelity refusal", err)
	}
	// The record's id is read by its exact member name: one spelled in another
	// case states no id, and no entry is built from it.
	for _, spelled := range []string{
		`{"resourceType":"PractitionerRole","Id":"r","practitioner":{"reference":"Practitioner/x"}}`,
		`{"ResourceType":"PractitionerRole","id":"r","practitioner":{"reference":"Practitioner/x"}}`,
	} {
		if out, err := carryOrderRequester(body, [][]byte{[]byte(spelled)}); !errors.Is(err, errRecordNotCarried) || !isPASBuildFidelity(err) {
			t.Fatalf("a requester record %s: %v (%s), want the build's fidelity refusal", spelled, err, out)
		}
	}
}
