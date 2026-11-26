package engine

import "testing"

// A literal reference names a Patient by FHIR's reference grammar, read by
// its structure: "Patient/<id>", or an http(s) URL whose path ends in
// "Patient/<id>", either with an optional "/_history/<version>". Anything
// else names no Patient, however much of it reads "Patient/". An id that
// names one Patient by structure but is not a FHIR id is read as written, and
// reported as not conforming.
func TestReadPatientReference(t *testing.T) {
	for _, row := range []struct {
		ref, id      string
		ok, conforms bool
	}{
		{"Patient/MBR-1", "MBR-1", true, true},
		{"Patient/MBR-1/_history/2", "MBR-1", true, true},
		{"https://ehr.example/fhir/Patient/MBR-1", "MBR-1", true, true},
		{"http://ehr.example:8080/fhir/Patient/MBR-1/_history/2", "MBR-1", true, true},
		{"https://ehr.example/Patient/MBR-1", "MBR-1", true, true},
		{"https://ehr.example/_history/fhir/Patient/MBR-1", "MBR-1", true, true}, // a base whose path holds /_history/
		{"https://ehr.example/Patient/fhir/Patient/MBR-1", "MBR-1", true, true},
		// Any base: no base is matched against another, and its own segments
		// may carry an escape or any other character.
		{"https://other-host.example:8443/r4/base/Patient/MBR-1/_history/3", "MBR-1", true, true},
		{"http://10.0.0.5/Patient/MBR-1", "MBR-1", true, true},
		{"https://h.example/fhir%20r4/Patient/MBR-1", "MBR-1", true, true},
		{"https://h_x.example/fhir_r4/Patient/MBR-1", "MBR-1", true, true},
		{"HTTPS://EHR.EXAMPLE/fhir/Patient/MBR-1", "MBR-1", true, true},
		{"https://ehr.example/fhir/Patient/%20", "", false, false}, // an escape in the id
		{"Practitioner/Patient", "", false, false},                 // another type, whatever its id
		{"https://h.example/Patient/fhir/Group/1", "", false, false},
		// The path is read as sent: an escaped "/" is never a separator,
		// whatever else the path holds.
		{"http://h.example/x y/Patient%2F1", "", false, false},
		{"http://h.example/\u00e9/Patient%2F1", "", false, false},
		{"http://h.example/x|/Patient%2F1", "", false, false},
		{"http://h.example/x y/Patient%2FA%2F_history%2F2", "", false, false},
		{"http://h.example/x y/Patient/%2F1", "", false, false},
		// An absolute reference's id that is not a FHIR id reads as written,
		// as a relative one's does.
		{"http://h.example/Patient/a b", "a b", true, false},
		{"http://h.example/Patient/\u00e9", "\u00e9", true, false},
		{"http://h.example/fhir r4/Patient/MBR-1", "MBR-1", true, true},
		// One Patient by structure, an id that is not a FHIR id: read as written.
		{"Patient/MBR_001", "MBR_001", true, false},
		{"Patient/a b", "a b", true, false},
		{"Patient/a:b", "a:b", true, false},
		{"Patient/" + string(make65()), string(make65()), true, false},
		{"Patient/MBR-1/_history/v_2", "MBR-1", true, false},
		{"Patient/ ", " ", true, false}, // blank: names no one, which the binding refuses
		// No Patient by the grammar.
		{"", "", false, false},
		{"Patient/", "", false, false},
		{"Patient/Patient/", "", false, false},
		{"Patient/1/_history/", "", false, false},
		{"Patient/1/_history/2/x", "", false, false},
		{"Patient/1/x", "", false, false},
		{"/Patient/1", "", false, false},
		{"patient/1", "", false, false},
		{"XPatient/1", "", false, false},
		{"urn:Patient/1", "", false, false},
		{"urn:uuid:9b2c6a1e-0d2f-4b8e-9a51-3c1f2e7d8a10", "", false, false},
		{"#Patient/1", "", false, false},
		{"#p1", "", false, false}, // contained: resolved by the caller, never read here
		{"Basic/1/_history/Patient/2", "", false, false},
		{"Group/1/Patient/2", "", false, false},
		{"Practitioner/9", "", false, false},
		{"Practitioner/Patient/_history/7", "", false, false},
		{"https://h.example/fhir/Practitioner/9", "", false, false},
		{"https://h.example/Patient/fhir/Practitioner/9", "", false, false},
		{"https://h.example/fhirPatient/Practitioner/9", "", false, false},
		{"https://h.example/fhir/Patient/1?x=y", "", false, false},
		{"https://h.example/fhir/Patient/1#x", "", false, false},
		{"https://h.example//Patient/1", "", false, false},
		{"https:///fhir/Patient/1", "", false, false},
		{"https://user@h.example/fhir/Patient/1", "", false, false},
		{"ftp://h.example/fhir/Patient/1", "", false, false},
		{"Patient/$everything", "", false, false}, // an operation
		{"Patient/a%2Fb", "", false, false},       // an escape could hide a "/"
		{"https://h.example/fhir%2FPatient/1", "", false, false},
		{"Patient/1/$everything", "", false, false},
	} {
		id, conforms, ok := readPatientReference(row.ref)
		if id != row.id || ok != row.ok || conforms != row.conforms {
			t.Errorf("readPatientReference(%q) = (%q, conforms %v, ok %v), want (%q, %v, %v)", row.ref, id, conforms, ok, row.id, row.conforms, row.ok)
		}
	}
}

func make65() []byte {
	b := make([]byte, 65)
	for i := range b {
		b[i] = 'a'
	}
	return b
}
