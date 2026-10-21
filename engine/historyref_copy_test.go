package engine

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
)

// TestHistoryRefReadersAgree: the PAS graph carries its own copy of the
// engine's history-reference readers (fhirref.go), which stay in core for the
// routing coverage read. The two must read every reference the same way.
func TestHistoryRefReadersAgree(t *testing.T) {
	for _, ref := range []string{
		"Patient/1", "Patient/1/_history/2", "Patient/1/_history/", "Patient/1/_history",
		"https://fhir.test/r4/Patient/1/_history/3", "urn:uuid:abc", "", "/_history/1",
		"Patient/_history/1", "Coverage/c-1/_history/v1.2", "Patient/1/_history/2/_history/3",
		"https://host/_history/fhir/Patient/p", "https://host/fhir/_history/x/Patient/p/_history/4",
	} {
		if got, want := pasgraph.StripHistoryRef(ref), stripHistoryRef(ref); got != want {
			t.Errorf("StripHistoryRef(%q) = %q, engine %q", ref, got, want)
		}
		gb, gv, gok := pasgraph.SplitHistory(ref)
		wb, wv, wok := splitHistory(ref)
		if gb != wb || gv != wv || gok != wok {
			t.Errorf("SplitHistory(%q) = %q %q %t, engine %q %q %t", ref, gb, gv, gok, wb, wv, wok)
		}
	}
}
