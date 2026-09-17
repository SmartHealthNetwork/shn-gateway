package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// eobProcessNotes decodes an EOB's processNote texts, in order (nil when it
// carries none).
func eobProcessNotes(t *testing.T, eob []byte) []string {
	t.Helper()
	var e struct {
		ProcessNote []struct {
			Text string `json:"text"`
		} `json:"processNote"`
	}
	if err := json.Unmarshal(eob, &e); err != nil {
		t.Fatalf("decode EOB: %v", err)
	}
	var out []string
	for _, n := range e.ProcessNote {
		out = append(out, n.Text)
	}
	return out
}

// TestNativeSubmit_DeniedEOBCarriesOnlyPayerNotes: the EOB a payer gateway
// projects from its payer's denial carries the notes that denial carries,
// exactly and in order, and nothing the payer did not say: no fixed appeal
// window is added when the payer sent none. An approval carries none.
func TestNativeSubmit_DeniedEOBCarriesOnlyPayerNotes(t *testing.T) {
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED")
	project := func(t *testing.T, decision []byte) []byte {
		t.Helper()
		srv := stubPartnerSrv(t, http.StatusOK, fixturePASResponse(t, decision, true))
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-notes", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if len(res.SideEffectFHIR) != 1 {
			t.Fatalf("want 1 EOB side-effect, got %d", len(res.SideEffectFHIR))
		}
		return res.SideEffectFHIR[0]
	}
	denial := func(t *testing.T, notes []shnsdk.PASProcessNote) []byte {
		t.Helper()
		b, err := shnsdk.BuildDeniedResponseWithNotesAtLine("2.0", "Patient/MBR-COVERED", "corr-notes",
			"Não atende à política <de> imagem & revisão", notes, fixedClock())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("the payer's notes, exactly", func(t *testing.T) {
		notes := []shnsdk.PASProcessNote{
			{Type: "display", Text: "Appeal within 60 days — **call** 1-800-555-0100 <ref #7>"},
			{Type: "print", Text: "Segunda nota: revisão por par disponível"},
		}
		eob := project(t, denial(t, notes))
		want := []string{notes[0].Text, notes[1].Text}
		if got := eobProcessNotes(t, eob); !slices.Equal(got, want) {
			t.Fatalf("EOB notes = %q, want the payer's %q", got, want)
		}
	})
	t.Run("no notes from the payer, none on the EOB", func(t *testing.T) {
		eob := project(t, denial(t, nil))
		if got := eobProcessNotes(t, eob); got != nil {
			t.Fatalf("EOB notes = %q, want none", got)
		}
		for _, invented := range []string{"Appeal window", "peer-to-peer", shnsdk.EOBAppealNote} {
			if strings.Contains(string(eob), invented) {
				t.Fatalf("the EOB carries text the payer never sent (%q)", invented)
			}
		}
	})
	t.Run("an approval carries the payer's notes and nothing invented", func(t *testing.T) {
		eob := project(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-NOTES","preAuthPeriod":{"end":"2030-01-01"},`+
			`"processNote":[{"number":1,"type":"display","text":"approved note"}]}`))
		if got := eobProcessNotes(t, eob); !slices.Equal(got, []string{"approved note"}) {
			t.Fatalf("approved EOB notes = %q, want the payer's [\"approved note\"]", got)
		}
		for _, invented := range []string{"Appeal window", "peer-to-peer", shnsdk.EOBAppealNote} {
			if strings.Contains(string(eob), invented) {
				t.Fatalf("the approved EOB carries text the payer never sent (%q)", invented)
			}
		}
	})
	t.Run("no notes from the payer on an approval, none on the EOB", func(t *testing.T) {
		eob := project(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-NONE","preAuthPeriod":{"end":"2030-01-01"}}`))
		if got := eobProcessNotes(t, eob); got != nil {
			t.Fatalf("approved EOB notes = %q, want none", got)
		}
	})
}
