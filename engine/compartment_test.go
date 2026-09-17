package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const fenceBase = "https://sor.example/fhir/TENANT-A"

// testFence binds patient "p1" on the system of record, known to the network
// as member MBR-1.
func testFence() patientFence {
	return newPatientFence("urn:shn:member", "MBR-1", []string{fenceBase}, "p1", "MBR-1")
}

func TestPatientFence_Allows(t *testing.T) {
	cases := map[string]string{
		// The four compartment rows whose other paths name non-Patient
		// targets: only the Patient reference counts.
		"observation with practitioner performer": `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"performer":[{"reference":"Practitioner/dr"},{"reference":"Organization/lab"}]}`,
		"coverage with organization payor":        `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Organization/payer"}],"subscriber":{"reference":"RelatedPerson/rp"}}`,
		"condition with practitioner asserter":    `{"resourceType":"Condition","id":"c","subject":{"reference":"Patient/p1"},"asserter":{"reference":"Practitioner/dr"}}`,
		"documentreference with org author":       `{"resourceType":"DocumentReference","id":"d","subject":{"reference":"Patient/p1"},"author":[{"reference":"Organization/o"},{"display":"Dr Who"}]}`,
		// Only the binding path must name the bound patient: other Patient
		// references are only references.
		"dependent coverage":             `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/parent"},"policyHolder":{"reference":"Patient/parent"},"payor":[{"reference":"Organization/payer"}]}`,
		"claim payee is another patient": `{"resourceType":"Claim","id":"cl","patient":{"reference":"Patient/p1"},"payee":{"type":{"text":"subscriber"},"party":{"reference":"Patient/parent"}}}`,
		"performer is another patient":   `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"performer":[{"reference":"Patient/p2"}]}`,
		"untyped foreign performer":      `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"performer":[{"identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1"}}]}`,
		"unresolvable non-binding ref":   `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"urn:uuid:55555555-5555-5555-5555-555555555555"}}`,
		"questionnaire response":         `{"resourceType":"QuestionnaireResponse","id":"q","status":"completed","subject":{"reference":"Patient/p1"},"author":{"reference":"Patient/parent"}}`,
		// Provenance bound through its targets.
		"provenance of a bound coverage": `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"fullUrl":"` + fenceBase + `/Coverage/c","resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Provenance","id":"pv","target":[{"reference":"Coverage/c"},{"reference":"` + fenceBase + `/Coverage/c"}],"agent":[{"who":{"reference":"Practitioner/dr"}}]}}]}`,
		"provenance of a contained record": `{"resourceType":"Provenance","id":"pv","contained":[{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"}}],"target":[{"reference":"#o"}]}`,
		"provenance of the patient":        `{"resourceType":"Provenance","id":"pv","target":[{"reference":"Patient/p1"}]}`,
		"provenance of a provenance": `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Provenance","id":"a","target":[{"reference":"Observation/o"}]}},` +
			`{"resource":{"resourceType":"Provenance","id":"b","target":[{"reference":"Provenance/a/_history/1"}]}}]}`,
		// Reference forms.
		"member id reference":           `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/MBR-1"}}`,
		"versioned reference":           `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1/_history/3"}}`,
		"absolute on the base":          `{"resourceType":"Observation","id":"o","subject":{"reference":"` + fenceBase + `/Patient/p1"}}`,
		"typed reference":               `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1","type":"Patient"}}`,
		"member identifier":             `{"resourceType":"Observation","id":"o","subject":{"type":"Patient","identifier":{"system":"urn:shn:member","value":"MBR-1"}}}`,
		"member identifier, no type":    `{"resourceType":"Observation","id":"o","subject":{"identifier":{"system":"urn:shn:member","value":"MBR-1"}}}`,
		"non-patient logical":           `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"performer":[{"type":"Practitioner","identifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1"}}]}`,
		"contained patient":             `{"resourceType":"Coverage","id":"c","contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR-1"}]}],"beneficiary":{"reference":"#pat"}}`,
		"sibling contained patient":     `{"resourceType":"Claim","id":"cl","contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR-1"}]},{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"#pat"}}],"patient":{"reference":"#pat"}}`,
		"contained points at container": `{"resourceType":"Patient","id":"p1","contained":[{"resourceType":"Observation","id":"o","subject":{"reference":"#"}}]}`,
		"contained practitioner":        `{"resourceType":"Observation","id":"o","contained":[{"resourceType":"Practitioner","id":"dr"}],"subject":{"reference":"Patient/p1"},"performer":[{"reference":"#dr"}]}`,
		"other server practitioner":     `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"performer":[{"reference":"https://elsewhere.example/fhir/Practitioner/9"}]}`,
		"patient by id":                 `{"resourceType":"Patient","id":"p1"}`,
		"patient by member identifier":  `{"resourceType":"Patient","id":"ehr-77","identifier":[{"system":"urn:other","value":"x"},{"system":"urn:shn:member","value":"MBR-1"}]}`,
		"non-compartment resource":      `{"resourceType":"Organization","id":"o","partOf":{"reference":"Patient/other"}}`,
		"appointment actor array":       `{"resourceType":"Appointment","id":"a","participant":[{"actor":{"reference":"Practitioner/dr"}},{"actor":{"reference":"Patient/p1"}}]}`,
		"null prefetch value":           `null`,
		"searchset": `{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"fullUrl":"urn:uuid:11111111-1111-1111-1111-111111111111","resource":{"resourceType":"Patient","id":"p1"}},` +
			`{"resource":{"resourceType":"Observation","id":"o","subject":{"reference":"urn:uuid:11111111-1111-1111-1111-111111111111"}}},` +
			`{"fullUrl":"` + fenceBase + `/Patient/p1","resource":{"resourceType":"Patient","id":"p1"}},` +
			`{"resource":{"resourceType":"Condition","id":"c","subject":{"reference":"` + fenceBase + `/Patient/p1"}}},` +
			`{"resource":{"resourceType":"OperationOutcome","issue":[]},"search":{"mode":"outcome"}}]}`,
		"empty searchset": `{"resourceType":"Bundle","type":"searchset"}`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if err := testFence().check([]byte(value)); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

func TestPatientFence_Refuses(t *testing.T) {
	cases := map[string]string{
		// The same four rows about another patient.
		"observation, other patient":       `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p2"},"performer":[{"reference":"Practitioner/dr"}]}`,
		"coverage, other patient":          `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p2"},"payor":[{"reference":"Organization/payer"}]}`,
		"condition, other patient":         `{"resourceType":"Condition","id":"c","subject":{"reference":"Patient/p2"},"asserter":{"reference":"Practitioner/dr"}}`,
		"documentreference, other patient": `{"resourceType":"DocumentReference","id":"d","subject":{"reference":"Patient/p2"},"author":[{"reference":"Organization/o"}]}`,
		// The binding reference must be the bound patient, and no other
		// person's Patient resource may be carried.
		"dependent coverage, other beneficiary": `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/parent"},"subscriber":{"reference":"Patient/p1"}}`,
		"claim for another patient":             `{"resourceType":"Claim","id":"cl","patient":{"reference":"Patient/p2"},"payee":{"party":{"reference":"Patient/p1"}}}`,
		"questionnaire response, other subject": `{"resourceType":"QuestionnaireResponse","id":"q","status":"completed","subject":{"reference":"Patient/p2"},"author":{"reference":"Patient/p1"}}`,
		"sibling parent patient": `{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"urn:uuid:66666666-6666-6666-6666-666666666666"}}},` +
			`{"fullUrl":"urn:uuid:66666666-6666-6666-6666-666666666666","resource":{"resourceType":"Patient","id":"parent","identifier":[{"system":"urn:shn:member","value":"MBR-2"}]}}]}`,
		"contained parent patient": `{"resourceType":"Coverage","id":"c","contained":[{"resourceType":"Patient","id":"parent"}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#parent"}}`,
		// A Provenance is bound through its targets, each resolved in the
		// same document and itself fenced.
		"provenance, unresolvable target": `{"resourceType":"Provenance","id":"pv","target":[{"reference":"Observation/o"}],"recorded":"2026-09-17T00:00:00Z","agent":[{"who":{"reference":"Practitioner/dr"}}]}`,
		"provenance, other patient's record": `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"fullUrl":"urn:uuid:77777777-7777-7777-7777-777777777777","resource":{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p2"}}},` +
			`{"resource":{"resourceType":"Provenance","id":"pv","target":[{"reference":"urn:uuid:77777777-7777-7777-7777-777777777777"}],"agent":[{"who":{"reference":"Practitioner/dr"}}]}}]}`,
		"provenance, one failing target": `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Provenance","id":"pv","target":[{"reference":"Coverage/c"},{"reference":"Observation/missing"}],"agent":[{"who":{"reference":"Practitioner/dr"}}]}}]}`,
		"provenance, other patient":   `{"resourceType":"Provenance","id":"pv","target":[{"reference":"Patient/p2"}]}`,
		"provenance, no target":       `{"resourceType":"Provenance","id":"pv","agent":[{"who":{"reference":"Practitioner/dr"}}]}`,
		"provenance, identifier only": `{"resourceType":"Provenance","id":"pv","target":[{"type":"Observation","identifier":{"system":"urn:x","value":"1"}}]}`,
		"provenance, circular": `{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Provenance","id":"a","target":[{"reference":"Provenance/b"}]}},` +
			`{"resource":{"resourceType":"Provenance","id":"b","target":[{"reference":"Provenance/a"}]}}]}`,
		"unresolvable binding ref":            `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"urn:uuid:55555555-5555-5555-5555-555555555555"}}`,
		"appointment second actor":            `{"resourceType":"Appointment","id":"a","participant":[{"actor":{"reference":"Patient/p1"}},{"actor":{"reference":"Patient/p2"}}]}`,
		"no patient reference":                `{"resourceType":"Observation","id":"o","subject":{"reference":"Group/g"},"performer":[{"reference":"Practitioner/dr"}]}`,
		"no subject at all":                   `{"resourceType":"Condition","id":"c","asserter":{"reference":"Practitioner/dr"}}`,
		"display-only subject":                `{"resourceType":"Condition","id":"c","subject":{"display":"Jane"}}`,
		"contained patient mismatch":          `{"resourceType":"Coverage","id":"c","contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR-2"}]}],"beneficiary":{"reference":"#pat"}}`,
		"contained patient, id only":          `{"resourceType":"Coverage","id":"c","contained":[{"resourceType":"Patient","id":"p1"}],"beneficiary":{"reference":"#p1"}}`,
		"unreferenced contained other":        `{"resourceType":"Observation","id":"o","contained":[{"resourceType":"Patient","id":"x","identifier":[{"system":"urn:shn:member","value":"MBR-2"}]}],"subject":{"reference":"Patient/p1"}}`,
		"contained observation other":         `{"resourceType":"DiagnosticReport","id":"r","contained":[{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p2"}}],"subject":{"reference":"Patient/p1"}}`,
		"sibling contained other":             `{"resourceType":"Claim","id":"cl","contained":[{"resourceType":"Patient","id":"pat","identifier":[{"system":"urn:shn:member","value":"MBR-2"}]},{"resourceType":"Coverage","id":"cov","beneficiary":{"reference":"#pat"}}],"patient":{"reference":"Patient/p1"}}`,
		"contained points at other container": `{"resourceType":"Patient","id":"p2","contained":[{"resourceType":"Observation","id":"o","subject":{"reference":"#"}}]}`,
		"nested contained":                    `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"contained":[{"resourceType":"Observation","id":"i","subject":{"reference":"Patient/p1"},"contained":[]}]}`,
		"unresolvable contained":              `{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"#missing"}}`,
		"unresolvable urn":                    `{"resourceType":"Observation","id":"o","subject":{"reference":"urn:uuid:22222222-2222-2222-2222-222222222222"}}`,
		"other server patient":                `{"resourceType":"Observation","id":"o","subject":{"reference":"https://elsewhere.example/fhir/Patient/p1"}}`,
		"base prefix only":                    `{"resourceType":"Observation","id":"o","subject":{"reference":"` + fenceBase + `X/Patient/p1"}}`,
		"absolute, other patient":             `{"resourceType":"Observation","id":"o","subject":{"reference":"` + fenceBase + `/Patient/p2"}}`,
		"absolute, unparseable":               `{"resourceType":"Observation","id":"o","subject":{"reference":"https://elsewhere.example/"}}`,
		"bare type":                           `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient"}}`,
		"query reference":                     `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient?identifier=x"}}`,
		"reference not a string":              `{"resourceType":"Observation","id":"o","subject":{"reference":7}}`,
		"reference not an object":             `{"resourceType":"Observation","id":"o","subject":"Patient/p1"}`,
		"type contradicts reference":          `{"resourceType":"Observation","id":"o","subject":{"reference":"Group/p1","type":"Patient"},"performer":[{"reference":"Patient/p1"}]}`,
		"identifier of other member":          `{"resourceType":"Observation","id":"o","subject":{"type":"Patient","identifier":{"system":"urn:shn:member","value":"MBR-2"}}}`,
		"patient identifier, no member":       `{"resourceType":"Observation","id":"o","subject":{"type":"Patient","identifier":{"system":"urn:mrn","value":"1"}}}`,
		"patient, other id":                   `{"resourceType":"Patient","id":"p2"}`,
		"patient, other member":               `{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"MBR-2"}]}`,
		"patient without id":                  `{"resourceType":"Patient"}`,
		"unknown resource type":               `{"resourceType":"Widget","id":"w","subject":{"reference":"Patient/p1"}}`,
		"no resource type":                    `{"id":"w","subject":{"reference":"Patient/p1"}}`,
		"not an object":                       `[{"resourceType":"Patient","id":"p1"}]`,
		"not JSON":                            `{`,
		"duplicate key":                       `{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p1"},"subject":{"reference":"Patient/p2"}}`,
		"one bad entry": `{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"resource":{"resourceType":"Observation","id":"o1","subject":{"reference":"Patient/p1"}}},` +
			`{"resource":{"resourceType":"Observation","id":"o2","subject":{"reference":"Patient/p2"}}}]}`,
		"urn resolves to other patient": `{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"fullUrl":"urn:uuid:33333333-3333-3333-3333-333333333333","resource":{"resourceType":"Patient","id":"p2"}},` +
			`{"resource":{"resourceType":"Observation","id":"o","subject":{"reference":"urn:uuid:33333333-3333-3333-3333-333333333333"}}}]}`,
		"urn resolves to non-patient": `{"resourceType":"Bundle","type":"searchset","entry":[` +
			`{"fullUrl":"urn:uuid:44444444-4444-4444-4444-444444444444","resource":{"resourceType":"Group","id":"g"}},` +
			`{"resource":{"resourceType":"Observation","id":"o","subject":{"reference":"urn:uuid:44444444-4444-4444-4444-444444444444"}}}]}`,
		"nested bundle": `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Bundle","type":"collection","entry":[` +
			`{"resource":{"resourceType":"Observation","id":"o","subject":{"reference":"Patient/p2"}}}]}}]}`,
		"entry without resource": `{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"x"}]}`,
		"entries not an array":   `{"resourceType":"Bundle","type":"searchset","entry":{}}`,
		"patient in bundle":      `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p2"}}]}`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			err := testFence().check([]byte(value))
			var ce *CompartmentError
			if !errors.As(err, &ce) {
				t.Fatalf("not refused as a compartment error: %v", err)
			}
			if strings.Contains(err.Error(), "p2") || strings.Contains(err.Error(), "MBR-2") {
				t.Fatalf("error names a patient: %v", err)
			}
		})
	}
}

// The fence reads the generated table: every compartment type is refused
// when its binding reference names another patient.
func TestPatientFence_EveryCompartmentType(t *testing.T) {
	for rt, paths := range patientBindingPaths {
		if len(paths) == 0 || rt == "Patient" || rt == "Provenance" {
			continue
		}
		for _, path := range paths {
			good := resourceWithRef(rt, path, "Patient/p1")
			if err := testFence().check(good); err != nil {
				t.Errorf("%s.%s bound patient refused: %v", rt, path, err)
			}
			bad := resourceWithRef(rt, path, "Patient/p2")
			if err := testFence().check(bad); err == nil {
				t.Errorf("%s.%s other patient allowed", rt, path)
			}
		}
	}
}

// resourceWithRef builds {"resourceType":rt, <path>: {"reference":ref}}, as
// an array at every level (the fence walks both shapes).
func resourceWithRef(rt, path, ref string) []byte {
	v := `{"reference":"` + ref + `"}`
	segs := strings.Split(path, ".")
	for i := len(segs) - 1; i >= 1; i-- {
		v = `[{"` + segs[i] + `":` + v + `}]`
	}
	return []byte(`{"resourceType":"` + rt + `","id":"x","` + segs[0] + `":` + v + `}`)
}

// provenanceLayers builds a Bundle of layers×width Provenances in which each
// Provenance targets every resource of the next layer, and the last layer is
// width Observations about the bound patient.
func provenanceLayers(layers, width int) []byte {
	var entries []string
	for w := 0; w < width; w++ {
		entries = append(entries, fmt.Sprintf(`{"resource":{"resourceType":"Observation","id":"o%d","subject":{"reference":"Patient/p1"}}}`, w))
	}
	for l := layers - 1; l >= 0; l-- {
		for w := 0; w < width; w++ {
			var targets []string
			for x := 0; x < width; x++ {
				if l == layers-1 {
					targets = append(targets, fmt.Sprintf(`{"reference":"Observation/o%d"}`, x))
				} else {
					targets = append(targets, fmt.Sprintf(`{"reference":"Provenance/l%d-%d"}`, l+1, x))
				}
			}
			entries = append([]string{fmt.Sprintf(`{"resource":{"resourceType":"Provenance","id":"l%d-%d","target":[%s]}}`, l, w, strings.Join(targets, ","))}, entries...)
		}
	}
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`)
}

// Provenance targets are resolved once per document: a wide, deep acyclic
// graph costs one resolution per reference, and a document that needs more
// resolutions than the fence allows is refused.
func TestPatientFence_ProvenanceFanOutBounded(t *testing.T) {
	resolutions, err := testFence().checkCounting(provenanceLayers(16, 8))
	if err != nil {
		t.Fatalf("a bounded acyclic graph was refused: %v", err)
	}
	if want := 16 * 8 * 8; resolutions != want {
		t.Fatalf("resolutions = %d, want %d (one per reference)", resolutions, want)
	}
	width := 1
	for width*width <= maxFenceResolutions {
		width++
	}
	resolutions, err = testFence().checkCounting(provenanceLayers(1, width))
	var ce *CompartmentError
	if !errors.As(err, &ce) || resolutions > maxFenceResolutions+1 {
		t.Fatalf("a document over the resolution bound: err=%v resolutions=%d", err, resolutions)
	}
	// A cycle is refused wherever the check starts.
	for _, order := range []string{"ab", "ba"} {
		a := `{"resource":{"resourceType":"Provenance","id":"a","target":[{"reference":"Provenance/b"}]}}`
		b := `{"resource":{"resourceType":"Provenance","id":"b","target":[{"reference":"Provenance/a"}]}}`
		first, second := a, b
		if order == "ba" {
			first, second = b, a
		}
		if err := testFence().check([]byte(`{"resourceType":"Bundle","type":"collection","entry":[` + first + `,` + second + `]}`)); err == nil {
			t.Fatalf("cycle (%s) accepted", order)
		}
	}
}

// The member a check marks resources with cannot come from a document: its
// key is not UTF-8, which the strict decoding refuses.
func TestPatientFence_MarkKeyNotForgeable(t *testing.T) {
	doc := []byte(`{"resourceType":"Observation","` + fenceMarkKey + `":0,"subject":{"reference":"Patient/p1"}}`)
	var ce *CompartmentError
	if err := testFence().check(doc); !errors.As(err, &ce) {
		t.Fatalf("a document carrying the mark key was accepted: %v", err)
	}
}

// A prefetch value never carries a Binary: opaque content the fence cannot
// read for the patient. It is refused wherever it appears; the records fence
// is unchanged.
func TestPatientFence_PrefetchRefusesBinary(t *testing.T) {
	binary := `{"resourceType":"Binary","id":"b1","contentType":"application/pdf","data":"JVBERi0="}`
	for name, value := range map[string]string{
		"top level":     binary,
		"bundle entry":  `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Observation","id":"o1","subject":{"reference":"Patient/p1"}}},{"resource":` + binary + `}]}`,
		"nested bundle": `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + binary + `}]}}]}`,
		"contained":     `{"resourceType":"Observation","id":"o1","subject":{"reference":"Patient/p1"},"contained":[` + binary + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := testFence().forPrefetch().check([]byte(value))
			var ce *CompartmentError
			if !errors.As(err, &ce) || ce.ResourceType != "Binary" || ce.Reason != opaqueContentReason {
				t.Fatalf("got %v, want the Binary refused", err)
			}
			if err := testFence().check([]byte(value)); err != nil {
				t.Fatalf("records fence: %v", err)
			}
		})
	}
}
