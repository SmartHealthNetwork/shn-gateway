package fhirsor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// Every read of the participant's own FHIR system refuses an answer that
// repeats a member name, at the Bundle level, inside the resource or under
// case folding: it can be read two ways (FHIR JSON: property names SHALL be
// unique), so no reading of it is acted on. The refusal is the invalid
// response every unreadable answer is (502), and says so by structure
// (errors.Is relay.ErrDuplicateKey), which the gateway records as message
// integrity. The same read of a clean answer is not refused.
func TestEveryReadRefusesARepeatedMember(t *testing.T) {
	clean := map[string]string{
		"/Patient":           failurePatient,
		"/Organization/p":    `{"resourceType":"Organization","id":"p","identifier":[{"system":"urn:x","value":"1"}]}`,
		"/Coverage":          `{"resourceType":"Coverage","id":"c","status":"active","beneficiary":{"reference":"Patient/p"}}`,
		"/DeviceRequest":     `{"resourceType":"DeviceRequest","id":"o","status":"active","intent":"order","subject":{"reference":"Patient/p"}}`,
		"/DiagnosticReport":  `{"resourceType":"DiagnosticReport","id":"r","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/p"}}`,
		"/DocumentReference": `{"resourceType":"DocumentReference","id":"d","status":"current","subject":{"reference":"Patient/p"}}`,
		"/Condition":         `{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"J44.9"}]}}`,
		"/Observation":       `{"resourceType":"Observation","id":"ob","status":"final","code":{"text":"x"}}`,
	}
	reads := []struct {
		name, path string
		read       func(*SoR) error
	}{
		{"patient", "/Patient", func(s *SoR) error { _, _, _, e := s.ResolvePatientContext(context.Background(), "m"); return e }},
		{"patient-ref", "/Patient", func(s *SoR) error { _, _, e := s.PatientFHIRRefContext(context.Background(), "m"); return e }},
		{"reference", "/Organization/p", func(s *SoR) error {
			_, _, e := s.ResolveByReferenceContext(context.Background(), "Organization/p")
			return e
		}},
		{"coverage", "/Coverage", func(s *SoR) error { _, _, e := s.CoverageInforceContext(context.Background(), "m"); return e }},
		{"open-coverage", "/Coverage", func(s *SoR) error { _, e := s.OpenCoverageContext(context.Background(), "m"); return e }},
		{"order", "/DeviceRequest", func(s *SoR) error { _, _, e := s.OpenOrderContext(context.Background(), "m"); return e }},
		{"report", "/DiagnosticReport", func(s *SoR) error { _, _, e := s.SupplementalReportContext(context.Background(), "m"); return e }},
		{"facility-records", "/DocumentReference", func(s *SoR) error { _, _, e := s.FacilityRecordsContext(context.Background(), "m"); return e }},
		{"clinical-condition", "/Condition", func(s *SoR) error { _, _, e := s.ClinicalContextContext(context.Background(), "m"); return e }},
		{"clinical-observation", "/Observation", func(s *SoR) error { _, _, e := s.ClinicalContextContext(context.Background(), "m"); return e }},
	}
	// Each mutation repeats a member of the answer at path.
	mutations := map[string]func(path, resource string) string{
		"clean": func(path, r string) string { return wrap(path, r) },
		"a repeat at the Bundle level": func(path, r string) string {
			if !isSearch(path) {
				return strings.Replace(r, `"resourceType":`, `"resourceType":"x","resourceType":`, 1)
			}
			return strings.Replace(wrap(path, r), `"type":"searchset"`, `"type":"searchset","type":"searchset"`, 1)
		},
		"a repeat inside the resource": func(path, r string) string {
			return wrap(path, strings.Replace(r, `"id":`, `"id":"other","id":`, 1))
		},
		"a repeat under case folding": func(path, r string) string {
			return wrap(path, strings.Replace(r, `"id":`, `"ID":"other","id":`, 1))
		},
	}
	for _, rd := range reads {
		for name, mutate := range mutations {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == rd.path {
					_, _ = w.Write([]byte(mutate(r.URL.Path, clean[rd.path])))
					return
				}
				if res, ok := clean[r.URL.Path]; ok {
					_, _ = w.Write([]byte(wrap(r.URL.Path, res)))
					return
				}
				_, _ = w.Write([]byte(emptySearch))
			}))
			err := rd.read(NewFromURL(srv.URL, srv.Client()))
			srv.Close()
			if name == "clean" {
				if err != nil {
					t.Errorf("%s, clean: %v, want it read", rd.name, err)
				}
				continue
			}
			var readErr *engine.SoRReadError
			if !errors.As(err, &readErr) || readErr.Kind != engine.SoRInvalidResponse || !errors.Is(err, relay.ErrDuplicateKey) {
				t.Errorf("%s, %s: %v, want an invalid response that repeats a member", rd.name, name, err)
				continue
			}
			assertFailure(t, err, http.StatusBadGateway)
		}
	}
}

func isSearch(path string) bool { return strings.Count(path, "/") == 1 }

// wrap is the answer at path: a searchset holding resource for a search, the
// resource itself for a read.
func wrap(path, resource string) string {
	if isSearch(path) {
		return searchResource(resource)
	}
	return resource
}
