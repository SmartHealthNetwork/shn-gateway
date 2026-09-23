package fhirsor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The FHIR source supplies an already issued network identity. Neither a member
// number nor demographics is allowed to manufacture a missing link.
func TestSourceSubjectLinkage(t *testing.T) {
	const linked = `{"resourceType":"Patient","id":"local","identifier":[{"system":"urn:shn:member","value":"m1"},{"system":"urn:shn:pci","value":"pci:issued-a"}],"name":[{"family":"Name"}],"birthDate":"1970-01-01"}`
	const unlinked = `{"resourceType":"Patient","id":"other","identifier":[{"system":"urn:shn:member","value":"m2"}],"name":[{"family":"Name"}],"birthDate":"1970-01-01"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case r.URL.Path == "/Patient/local":
			w.Write([]byte(linked))
		case r.URL.Path == "/Patient/other":
			w.Write([]byte(unlinked))
		case r.URL.Path == "/Patient" && r.URL.Query().Get("identifier") == shnsdk.MemberSystem+"|m1":
			w.Write([]byte(searchResource(linked)))
		case r.URL.Path == "/Patient" && r.URL.Query().Get("identifier") == shnsdk.MemberSystem+"|m2":
			w.Write([]byte(searchResource(unlinked)))
		default:
			w.Write([]byte(emptySearch))
		}
	}))
	defer srv.Close()
	s := NewFromURLForHolder(srv.URL, srv.Client(), "provider")
	for _, ref := range []engine.PatientReference{
		{Holder: "provider", System: "fhir-relative", Value: "Patient/local"},
		{Holder: "provider", System: srv.URL, Value: "Patient/local"},
		{Holder: "provider", System: shnsdk.MemberSystem, Value: "m1"},
	} {
		pci, found, err := s.ResolveSubject(context.Background(), ref)
		if err != nil || !found || pci != "pci:issued-a" {
			t.Fatalf("%+v: pci=%q found=%v err=%v", ref, pci, found, err)
		}
	}
	for _, ref := range []engine.PatientReference{
		{Holder: "payer", System: "fhir-relative", Value: "Patient/local"},
		{Holder: "provider", System: "https://foreign.example/fhir", Value: "Patient/local"},
		{Holder: "provider", System: "fhir-relative", Value: "Patient/other"},
		{Holder: "provider", System: shnsdk.MemberSystem, Value: "m2"},
		{Holder: "provider", System: "fhir-relative", Value: "Patient/local/../other"},
		{Holder: "provider", System: "fhir-relative", Value: "Patient/.."},
	} {
		pci, found, err := s.ResolveSubject(context.Background(), ref)
		if err != nil || found || pci != "" {
			t.Fatalf("unsupported %+v resolved: pci=%q found=%v err=%v", ref, pci, found, err)
		}
	}
	pci, _, found, err := s.ResolvePatientContext(context.Background(), "m1")
	if err != nil || !found || pci != "pci:issued-a" {
		t.Fatalf("authored source PCI = %q found=%v err=%v", pci, found, err)
	}
	if pci, _, found, err := s.ResolvePatientContext(context.Background(), "m2"); err != nil || found || pci != "" {
		t.Fatalf("unlinked authored source PCI = %q found=%v err=%v", pci, found, err)
	}
}

func TestSourceSubjectLinkageRequiresSourceAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	s := NewFromURLForHolder(srv.URL, srv.Client(), "provider")
	for _, ref := range []engine.PatientReference{
		{Holder: "provider", System: "fhir-relative", Value: "Patient/local"},
		{Holder: "provider", System: shnsdk.MemberSystem, Value: "known"},
	} {
		if pci, found, err := s.ResolveSubject(context.Background(), ref); pci != "" || found || err == nil {
			t.Fatalf("forbidden source %+v resolved: pci=%q found=%v err=%v", ref, pci, found, err)
		}
	}
}

func TestSourceSubjectLinkageAmbiguousPCI(t *testing.T) {
	patient := `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:pci","value":"pci:a"},{"system":"urn:shn:pci","value":"pci:b"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(patient)) }))
	defer srv.Close()
	s := NewFromURLForHolder(srv.URL, srv.Client(), "provider")
	if pci, found, err := s.ResolveSubject(context.Background(), engine.PatientReference{Holder: "provider", System: "fhir-relative", Value: "Patient/p"}); pci != "" || found || err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("ambiguous PCI = %q found=%v err=%v", pci, found, err)
	}
}

func TestPatientResolutionNeverMintsPCI(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"m"}],"name":[{"family":"Name"}],"birthDate":"1970-01-01"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(searchResource(patient))) }))
	defer srv.Close()
	for _, s := range []*SoR{NewFromURL(srv.URL, srv.Client()), NewFromURLForHolder(srv.URL, srv.Client(), "provider")} {
		if pci, _, found, err := s.ResolvePatientContext(context.Background(), "m"); err != nil || found || pci != "" {
			t.Fatalf("invented mapping: %q %v %v", pci, found, err)
		}
	}
}

func TestPatientResolutionRejectsUnmatchedSearchResult(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"different"},{"system":"urn:shn:pci","value":"pci:issued"}],"name":[{"family":"Name"}],"birthDate":"1970-01-01"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(searchResource(patient))) }))
	defer srv.Close()
	for _, s := range []*SoR{NewFromURL(srv.URL, srv.Client()), NewFromURLForHolder(srv.URL, srv.Client(), "provider")} {
		if pci, _, found, err := s.ResolvePatientContext(context.Background(), "m"); err == nil || found || pci != "" {
			t.Fatalf("trusted wrong search hit: %q %v %v", pci, found, err)
		}
	}
}
