//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// e06FHIRPatientElements is FHIR R4's member naming the patient of each type
// E-06 re-keys, written out here from the specification, apart from the
// registry it checks: a registry row naming another element for a type is a
// re-key at the wrong place.
var e06FHIRPatientElements = map[string]string{
	"Condition": "subject", "Observation": "subject", "ClinicalImpression": "subject", "Goal": "subject",
	"DiagnosticReport": "subject",
	"ServiceRequest":   "subject", "DeviceRequest": "subject", "MedicationRequest": "subject",
	"NutritionOrder": "patient", "Encounter": "subject",
}

// TestE06Registry_NamesEachTypesPatientElement: E-06's registered places are
// exactly the (type, element) pairs FHIR R4 names, and every type a QR
// context is read as is among them.
func TestE06Registry_NamesEachTypesPatientElement(t *testing.T) {
	if !maps.Equal(evidencePatientElement, e06FHIRPatientElements) {
		t.Fatalf("E-06 registry %v, want %v", evidencePatientElement, e06FHIRPatientElements)
	}
	for typ := range qrContextTypes {
		if _, ok := evidencePatientElement[typ]; !ok {
			t.Fatalf("a QR context of type %s is read but E-06 is not registered for it", typ)
		}
	}
}

// TestE06Registry_SupplementalReportPlace: the supplemental report's re-key
// (core, supplementalReportPatientElement) is at E-06's registered place for
// a DiagnosticReport.
func TestE06Registry_SupplementalReportPlace(t *testing.T) {
	if got := evidencePatientElement["DiagnosticReport"]; got != supplementalReportPatientElement {
		t.Fatalf("E-06 registers DiagnosticReport at %q, the supplemental report re-keys %q", got, supplementalReportPatientElement)
	}
}

// e06Record is a record of typ naming patientRef at element.
func e06Record(typ, element, patientRef string) []byte {
	return []byte(`{"resourceType":"` + typ + `","id":"rec-1","status":"active","` + element + `":{"reference":"` + patientRef + `","display":"kept"},"note":[{"text":"kept"}]}`)
}

// TestE06_ReKeysEachContextTypeAtItsPatientElement: where the provider's
// system holds the member under its own id, a record of each QR context type
// is re-keyed by E-06 at its registered element, and only there, the edit
// noted for the request's seal; one naming its patient at another element
// names none where its type names it, and is refused; and one a signature
// covers is refused 502 E-06.
func TestE06_ReKeysEachContextTypeAtItsPatientElement(t *testing.T) {
	for typ := range qrContextTypes {
		element := e06FHIRPatientElements[typ]
		t.Run(typ, func(t *testing.T) {
			held := e06Record(typ, element, "Patient/pat-77")
			read := func(context.Context, string) ([]byte, bool, error) { return held, true, nil }
			var note inputEditNote
			got, found, err := evidenceReadByMember(read, "pat-77", "MBR-1", &note)(context.Background(), typ+"/rec-1")
			if err != nil || !found {
				t.Fatalf("read: %v found=%v", err, found)
			}
			want := bytes.Replace(held, []byte(`"reference":"Patient/pat-77"`), []byte(`"reference":"Patient/MBR-1"`), 1)
			if !bytes.Equal(got, want) {
				t.Fatalf("re-keyed %s, want %s", got, want)
			}
			if edits := note.list(); len(edits) != 1 || edits[0] != relay.EditEvidenceSubjectRekey {
				t.Fatalf("edits %v, want E-06", edits)
			}

			wrong := "subject"
			if element == "subject" {
				wrong = "patient"
			}
			held = e06Record(typ, wrong, "Patient/pat-77")
			_, _, err = evidenceReadByMember(read, "pat-77", "MBR-1", new(inputEditNote))(context.Background(), typ+"/rec-1")
			if !errors.Is(err, errRecordNotCarried) || !strings.Contains(err.Error(), typ+"/rec-1 names no "+element+".reference") {
				t.Fatalf("a record naming its patient at %s: %v, want it refused as naming none at %s", wrong, err, element)
			}

			b := e06Record(typ, element, "Patient/pat-77")
			held = append(b[:len(b)-1], signedSuffix...)
			_, _, err = evidenceReadByMember(read, "pat-77", "MBR-1", new(inputEditNote))(context.Background(), typ+"/rec-1")
			var signed *relay.SignedContentError
			if !errors.As(err, &signed) || signed.Edit != relay.EditEvidenceSubjectRekey || buildFidelityStatus(err) != 502 {
				t.Fatalf("a signed %s: %v, want 502 signed content cannot be edited (E-06, ...)", typ, err)
			}
			if !strings.Contains(err.Error(), "signed content cannot be edited (E-06, Signature in "+typ+")") {
				t.Fatalf("signed refusal %q", err)
			}
		})
	}
}

// TestE06_AQRContextIsCarriedReKeyed: through the whole provider-data build,
// at every level, where the provider's system holds the member under its own
// id, a QR context it holds about that Patient is carried with its patient
// re-keyed by E-06 to the request's, at the element its type names it by, and
// the request's record names E-06. One a signature covers is refused 502 E-06
// before the payer, recorded as the fidelity of the build.
func TestE06_AQRContextIsCarriedReKeyed(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		for typ := range qrContextTypes {
			for _, signed := range []bool{false, true} {
				t.Run(level.String()+"/"+typ+map[bool]string{true: "/signed"}[signed], func(t *testing.T) {
					t.Parallel()
					element := e06FHIRPatientElements[typ]
					held := bytes.Replace(e06Record(typ, element, "Patient/pat-77"), []byte(`"id":"rec-1"`), []byte(`"id":"optional-order"`), 1)
					if signed {
						held = append(held[:len(held)-1], signedSuffix...)
					}
					got := runAuthoredHomeOxygen(t, authoredHomeOxygenRun{level: level, sorID: "pat-77", optional: optionalContextReference(typ, "optional-order"),
						records: map[string][]byte{typ + "/optional-order": held}})
					if signed {
						wantQRContextRefused(t, got, 502, "signed content cannot be edited (E-06, Signature in "+typ+")", false)
						return
					}
					wantAuthoredHomeOxygenSent(t, got)
					// E-06 re-encodes the record it edits, as it does a clinical
					// record: the record carried is the one held, re-keyed, by value.
					want := bytes.Replace(held, []byte(`"reference":"Patient/pat-77"`), []byte(`"reference":"Patient/MBR-OX"`), 1)
					if !carriesRecord(t, got.outgoing, typ+"/optional-order", want) {
						t.Fatalf("the %s is not carried re-keyed at %s: want %s", typ, element, want)
					}
					if r := onlyRecord(t, got.recs); !slices.Contains(r.RequestEdits, string(relay.EditEvidenceSubjectRekey)) {
						t.Fatalf("recorded edits %v, want E-06 named", r.RequestEdits)
					}
				})
			}
		}
	}
}

// carriesRecord reports whether bundle carries the record ref names as an
// entry equal, by value, to want.
func carriesRecord(t *testing.T, bundle []byte, ref string, want []byte) bool {
	t.Helper()
	var b struct {
		Entry []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	var w map[string]any
	if json.Unmarshal(bundle, &b) != nil || json.Unmarshal(want, &w) != nil {
		t.Fatal("unreadable bundle or record")
	}
	for _, e := range b.Entry {
		if e.Resource["resourceType"].(string)+"/"+fmt.Sprint(e.Resource["id"]) == ref {
			// Encoded from decoded maps, both are canonical: equal encodings
			// are equal records.
			got, err1 := json.Marshal(e.Resource)
			exp, err2 := json.Marshal(w)
			return err1 == nil && err2 == nil && bytes.Equal(got, exp)
		}
	}
	return false
}

// TestE06_NeverReKeysATypeOutsideItsRegistry: a record of a type E-06 is
// not registered for is never re-keyed, whatever it names: it comes back
// exactly as held, and no edit is noted.
func TestE06_NeverReKeysATypeOutsideItsRegistry(t *testing.T) {
	for _, typ := range []string{"CarePlan", "Appointment", "Practitioner"} {
		held := e06Record(typ, "subject", "Patient/pat-77")
		var note inputEditNote
		got, found, err := evidenceReadByMember(func(context.Context, string) ([]byte, bool, error) { return held, true, nil }, "pat-77", "MBR-1", &note)(context.Background(), typ+"/rec-1")
		if err != nil || !found || !bytes.Equal(got, held) || note.list() != nil {
			t.Fatalf("%s: %s %v %v edits %v, want it exactly as held and no edit", typ, got, found, err, note.list())
		}
	}
}
