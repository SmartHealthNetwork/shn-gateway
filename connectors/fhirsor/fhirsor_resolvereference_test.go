package fhirsor_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// resolveReferenceFHIR stands up an httptest FHIR stub that:
//   - answers GET Organization/dme-1 → an Organization resource
//   - returns 404 for all other resource reads
func resolveReferenceFHIR(t *testing.T) *fhirclient.Client {
	t.Helper()
	const org = `{"resourceType":"Organization","id":"dme-1","name":"Acme Home Medical","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1922334455"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch r.URL.Path {
		case "/Organization/dme-1":
			w.Write([]byte(org))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"resourceType":"OperationOutcome"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

// TestSoR_ResolveByReference_Found asserts that a GET {type}/{id} returning a resource
// yields the resource bytes with found=true.
func TestSoR_ResolveByReference_Found(t *testing.T) {
	s := fhirsor.New(resolveReferenceFHIR(t))
	raw, found := s.ResolveByReference("Organization/dme-1")
	if !found {
		t.Fatal("expected found=true for existing Organization/dme-1")
	}
	if len(raw) == 0 {
		t.Fatal("expected non-empty resource bytes")
	}
	if string(raw) == "" {
		t.Fatal("resource bytes are empty")
	}
}

// TestSoR_ResolveByReference_NotFound asserts that a 404 GET yields found=false.
func TestSoR_ResolveByReference_NotFound(t *testing.T) {
	s := fhirsor.New(resolveReferenceFHIR(t))
	raw, found := s.ResolveByReference("Organization/nonexistent")
	if found {
		t.Fatalf("expected found=false for missing resource, got bytes: %s", raw)
	}
}

// TestFHIRBase: the connector names the FHIR base it reads (the gateway
// compares it with a CDS Hooks request's fhirServer to tell where a payor
// reference's id belongs), with the trailing slash the client drops.
func TestFHIRBase(t *testing.T) {
	s := fhirsor.NewFromURL("https://sor.example/fhir/", nil)
	if got := s.FHIRBase(); got != "https://sor.example/fhir" {
		t.Fatalf("FHIRBase() = %q, want the configured base", got)
	}
}
