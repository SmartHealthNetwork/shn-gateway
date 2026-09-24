package fhirsor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
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

// TestOpenCoverageContextReturnsEveryMatch: the contextual read returns every
// Coverage the search matched, each exactly as the server sent it, and leaves
// the choice to the caller. The single-record read does not choose either.
func TestOpenCoverageContextReturnsEveryMatch(t *testing.T) {
	covA := "{ \"resourceType\" : \"Coverage\", \"id\" : \"cov-a\", \"status\" : \"active\", \"beneficiary\" : { \"reference\" : \"Patient/p\" }, \"payor\" : [ { \"display\" : \"A <&> B\" } ] }"
	covB := `{"resourceType":"Coverage","id":"cov-b","status":"active","beneficiary":{"reference":"Patient/p"},"payor":[{"reference":"Organization/o"}]}`
	included := `{"resourceType":"Organization","id":"o"}`
	var coverageQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/Coverage"):
			coverageQuery = r.URL.RawQuery
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + covA + `},{"resource":` + included + `,"search":{"mode":"include"}},{"resource":` + covB + `,"search":{"mode":"match"}}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	s := fhirsor.New(fhirclient.New(srv.URL, nil))
	covs, err := s.OpenCoverageContext(context.Background(), "MBR-TWO")
	if err != nil {
		t.Fatal(err)
	}
	if len(covs) != 2 || string(covs[0]) != covA || string(covs[1]) != covB {
		t.Fatalf("coverages = %q", covs)
	}
	if coverageQuery != "patient=Patient%2Fp&_include=Coverage%3Apayor" {
		t.Fatalf("query = %q, want the bounded patient search", coverageQuery)
	}
	if cov, found := s.OpenCoverage("MBR-TWO"); found || cov != nil {
		t.Fatalf("single-record read chose one of several: %s", cov)
	}
}

// TestOpenCoverageContextReadsEveryPage: every Coverage on every page of the
// bounded search is returned (the search follows the server's next links
// within its bounds); a search over its bounds is a read failure, never a
// partial answer.
func TestOpenCoverageContextReadsEveryPage(t *testing.T) {
	covA := `{"resourceType":"Coverage","id":"cov-a","status":"active","beneficiary":{"reference":"Patient/p"},"payor":[{"reference":"Organization/o"}]}`
	covB := "{ \"resourceType\" : \"Coverage\", \"id\" : \"cov-b\", \"beneficiary\" : { \"reference\" : \"Patient/p\" } }"
	pages := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p"}}]}`))
		case r.URL.Query().Get("page") == "2":
			pages++
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + covB + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Coverage"):
			pages++
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"` + srv.URL + `/Coverage?page=2"}],"entry":[{"resource":` + covA + `},{"resource":{"resourceType":"Organization","id":"o"},"search":{"mode":"include"}}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	s := fhirsor.New(fhirclient.New(srv.URL, nil))
	covs, err := s.OpenCoverageContext(context.Background(), "MBR-TWO")
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(covs) != 2 || string(covs[0]) != covA || string(covs[1]) != covB {
		t.Fatalf("%d pages, coverages = %q", pages, covs)
	}

	t.Run("over the bounds", func(t *testing.T) {
		loop := httptest.NewServer(nil)
		n := 0
		loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/fhir+json")
			if strings.HasPrefix(r.URL.Path, "/Patient") {
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p"}}]}`))
				return
			}
			n++
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"` + loop.URL + `/Coverage?page=` + strconv.Itoa(n+1) + `"}],"entry":[{"resource":` + covA + `}]}`))
		})
		t.Cleanup(loop.Close)
		covs, err := fhirsor.New(fhirclient.New(loop.URL, nil)).OpenCoverageContext(context.Background(), "MBR-TWO")
		var re *engine.SoRReadError
		if covs != nil || !errors.As(err, &re) || re.Kind != engine.SoRInvalidResponse {
			t.Fatalf("got %d coverages, %v", len(covs), err)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/Patient") {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p"}}]}`))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(down.Close)
		_, err := fhirsor.New(fhirclient.New(down.URL, nil)).OpenCoverageContext(context.Background(), "MBR-TWO")
		var re *engine.SoRReadError
		if !errors.As(err, &re) || re.Kind != engine.SoRUnavailable {
			t.Fatalf("got %v", err)
		}
	})
}
