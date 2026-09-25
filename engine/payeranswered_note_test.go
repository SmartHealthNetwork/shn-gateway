package engine

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A refusal this payer gateway makes after its payer's system answered a PAS
// submission says the payer answered, so the requester does not submit twice;
// a refusal made before anything was forwarded does not.
func TestPASRefusalAfterThePayerAnsweredSaysSo(t *testing.T) {
	t.Run("the payer's answer refused at strict", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementStrict)
		p.partner.respByPath[pasSubmitPath] = []byte(`{"resourceType":"ClaimResponse"}`)
		got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
		if got.status != http.StatusBadGateway || !bytes.Contains(got.body, []byte(notePayerAnswered)) {
			t.Fatalf("got %d %s, want 502 saying the payer answered", got.status, got.body)
		}
	})
	t.Run("the payer's answer refused at strict, on the update leg", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementStrict)
		request, related := updateBundle(t)
		p.seedPend(t, related)
		p.partner.respByPath[pasSubmitPath] = []byte(`{"resourceType":"ClaimResponse"}`)
		got := p.send(t, "pas-claim-update", "", request)
		if got.status/100 != 4 && got.status/100 != 5 || !bytes.Contains(got.body, []byte(notePayerAnswered)) {
			t.Fatalf("got %d %s, want a refusal saying the payer answered", got.status, got.body)
		}
	})
	t.Run("the payer's answer names another patient, at strict", func(t *testing.T) {
		approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
		foreign := bytes.Replace(approved, []byte(`"entry":[`), []byte(`"entry":[{"fullUrl":"http://localhost:8081/fhir/Patient/Other","resource":{"resourceType":"Patient","id":"Other"}},`), 1)
		p := newLevelPayer(t, EnforcementStrict)
		p.partner.respByPath[pasSubmitPath] = foreign
		got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
		if got.status != http.StatusForbidden || !bytes.Contains(got.body, []byte(notePayerAnswered)) {
			t.Fatalf("got %d %s, want 403 saying the payer answered", got.status, got.body)
		}
	})
	t.Run("the payer's own refusal is relayed without the note", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementStrict)
		p.partner.status = http.StatusUnprocessableEntity
		got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
		if got.status != http.StatusUnprocessableEntity || bytes.Contains(got.body, []byte(notePayerAnswered)) {
			t.Fatalf("got %d %s, want the payer's own 422 without the note", got.status, got.body)
		}
	})
	// A record the gateway cannot write after the payer answered is not a
	// refusal: the payer's answer is relayed exactly, with no note, and the
	// operator is told (the ledger records and never gates).
	t.Run("a failed record after the payer answered is relayed, not refused", func(t *testing.T) {
		long := strings.Repeat("x", MaxPendKeyBytes+1)
		answer := []byte(strings.Replace(assemblyRealPending, `"value":"2ca2e10c-e924-4e12-9d69-5b71e4e91c31"`, `"value":"`+long+`"`, 1))
		if bytes.Equal(answer, []byte(assemblyRealPending)) {
			t.Fatal("fixture: the ClaimResponse identifier was not replaced")
		}
		p := newLevelPayer(t, EnforcementNone)
		failed := 0
		prev := p.g.cfg.Observer
		p.g.cfg.Observer = func(e ObserverEvent) {
			if e.Kind == LocalWriteFailedEvent && e.LegType == "pas-claim" && strings.Contains(e.Detail, "longer than the ledger keeps") {
				failed++
			}
			if prev != nil {
				prev(e)
			}
		}
		p.partner.respByPath[pasSubmitPath] = answer
		got := p.send(t, "pas-claim", "", originatorBuiltConformantBundle(t, "MBR-COVERED"))
		if got.status != http.StatusOK || !bytes.Equal(got.body, answer) {
			t.Fatalf("got %d, want the payer's answer relayed exactly", got.status)
		}
		if bytes.Contains(got.body, []byte(notePayerAnswered)) || failed != 1 {
			t.Fatalf("note present=%v, write-failed events=%d; want no note and one event", bytes.Contains(got.body, []byte(notePayerAnswered)), failed)
		}
	})
	t.Run("control: refused before the forward", func(t *testing.T) {
		item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
		if err != nil {
			t.Fatal(err)
		}
		qr := `{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[` + string(stripItemExtension(t, item)) + `]}}`
		p := newLevelPayer(t, EnforcementStrict)
		got := p.send(t, "pas-claim", "", []byte(levelPASBundleWithEntry(qr)))
		if got.status != http.StatusForbidden || bytes.Contains(got.body, []byte(notePayerAnswered)) {
			t.Fatalf("got %d %s, want 403 without the note", got.status, got.body)
		}
	})
}
