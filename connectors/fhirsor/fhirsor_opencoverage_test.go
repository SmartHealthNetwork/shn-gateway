package fhirsor_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// openCoverageFHIR stands up an httptest FHIR stub that:
//   - answers Patient?identifier=urn:shn:member|<memberID> → 1 entry with id "p"
//     (or an empty searchset when memberID == "", simulating an unknown member)
//   - answers Coverage?beneficiary=Patient/p → a Bundle containing a Coverage with id
//     "cov-1" and a payor naming the contained "cms-payer" Organization
func openCoverageFHIR(t *testing.T, memberID string) *fhirclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			if memberID == "" {
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
				return
			}
			patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"` + memberID + `"}],"name":[{"family":"Test"}],"birthDate":"1970-01-01"}`
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Coverage"):
			coverage := `{"resourceType":"Coverage","id":"cov-1","status":"active","beneficiary":{"reference":"Patient/p"},"payor":[{"reference":"#cms-payer"}],"contained":[{"resourceType":"Organization","id":"cms-payer","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}]}`
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + coverage + `}]}`))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

// TestOpenCoverageReturnsRecord proves OpenCoverage returns the seeded Coverage entry
// bytes (id + payor intact) for a known member, and found=false for an unknown member
// (empty Patient searchset) — the same resolvePatient-miss fail-closed contract as
// CoverageInforce/OpenOrder.
func TestOpenCoverageReturnsRecord(t *testing.T) {
	s := fhirsor.New(openCoverageFHIR(t, "MBR-PD-COV"))
	covJSON, found := s.OpenCoverage("MBR-PD-COV")
	if !found {
		t.Fatalf("OpenCoverage(known member) found=false, want true")
	}
	if !strings.Contains(string(covJSON), `"id":"cov-1"`) {
		t.Fatalf("OpenCoverage bytes missing seeded Coverage id %q, got %s", "cov-1", covJSON)
	}
	if !strings.Contains(string(covJSON), `"reference":"#cms-payer"`) {
		t.Fatalf("OpenCoverage bytes missing seeded payor reference, got %s", covJSON)
	}
}

func TestOpenCoverageUnknownMember(t *testing.T) {
	s := fhirsor.New(openCoverageFHIR(t, ""))
	covJSON, found := s.OpenCoverage("MBR-UNKNOWN")
	if found {
		t.Fatalf("OpenCoverage(unknown member) found=true, want false")
	}
	if covJSON != nil {
		t.Fatalf("OpenCoverage(unknown member) bytes=%s, want nil", covJSON)
	}
}
