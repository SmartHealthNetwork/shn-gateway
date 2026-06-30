package fhirsor_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// coverageFHIR stands up an httptest FHIR stub that:
//   - answers Patient?identifier=urn:shn:member|<memberID> → 1 entry with id "p"
//   - answers Coverage?beneficiary=Patient/p → a Bundle containing a Coverage
//     with the given status (empty statusJSON → empty searchset, simulating absent coverage)
func coverageFHIR(t *testing.T, memberID, statusJSON string) *fhirclient.Client {
	t.Helper()
	patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"` + memberID + `"}],"name":[{"family":"Test"}],"birthDate":"1970-01-01"}`
	var coverageBundle string
	if statusJSON != "" {
		coverageBundle = `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"cov-1","status":"` + statusJSON + `","beneficiary":{"reference":"Patient/p"}}}]}`
	} else {
		coverageBundle = `{"resourceType":"Bundle","type":"searchset"}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/Coverage"):
			w.Write([]byte(coverageBundle))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

// TestCoverageInforce_TerminatedTracesToSeed proves the not-covered determination traces to a
// SEEDED terminated/cancelled Coverage (false,"coverage-terminated"), distinct from an ABSENT
// Coverage (false,"") — so the provider-data UC-01 notcovered branch can't be faked by a
// missing seed.  A bare absence must never be mistaken for a genuine "terminated" signal.
func TestCoverageInforce_TerminatedTracesToSeed(t *testing.T) {
	// cancelled/terminated Coverage → reason-bearing negative
	s := fhirsor.New(coverageFHIR(t, "MBR-PD-UC01-NC", "cancelled"))
	inforce, reason := s.CoverageInforce("MBR-PD-UC01-NC")
	if inforce || reason != "coverage-terminated" {
		t.Fatalf("terminated Coverage → (%v,%q), want (false,\"coverage-terminated\")", inforce, reason)
	}

	// absent Coverage (empty searchset) → empty reason: the bug-indistinguishable case that
	// must NOT be substituted for a seeded terminated Coverage.
	s2 := fhirsor.New(coverageFHIR(t, "MBR-PD-UC01-NC", ""))
	inforce2, reason2 := s2.CoverageInforce("MBR-PD-UC01-NC")
	if inforce2 || reason2 != "" {
		t.Fatalf("absent Coverage → (%v,%q), want (false,\"\")", inforce2, reason2)
	}
}
