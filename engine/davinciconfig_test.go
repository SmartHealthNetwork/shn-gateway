package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestBuildDavinciConfiguration pins the HRex 1.2.0 well-known document
// (spec 2026-08-10 §3 path 3; shape verified against HL7/davinci-ehrx:
// Binary-Wellknown.json + HRexWellknownDefinition — identifier is REQUIRED,
// endpoints is a code→uri map, version-specific codes are <code>#<major.minor>).
// Codes derive from SupportedContractVersions, so a future line ships here
// with zero new code. pa.pdex is EXCLUDED: the patient-access endpoint lives
// on the payer edge, which has no self base URL yet (see plan Deferred).
func TestBuildDavinciConfiguration(t *testing.T) {
	b, err := buildDavinciConfiguration("https://gw.example/fhir", "prov-1", shnsdk.SupportedContractVersions())
	if err != nil {
		t.Fatalf("buildDavinciConfiguration: %v", err)
	}
	var doc struct {
		Identifier struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Identifier.Value != "prov-1" || doc.Identifier.System == "" {
		t.Fatalf("identifier = %+v (required 1..1 with a namespacing system)", doc.Identifier)
	}
	want := map[string]string{
		"davinci_crd_hook_endpoint#2.0":       "https://gw.example/fhir",
		"davinci_dtr_qpackage_endpoint#2.0":   "https://gw.example/fhir",
		"davinci_pas_submission_endpoint#2.0": "https://gw.example/fhir",
	}
	if len(doc.Endpoints) != len(want) {
		t.Fatalf("endpoints = %v, want exactly %v", doc.Endpoints, want)
	}
	for code, url := range want {
		if doc.Endpoints[code] != url {
			t.Fatalf("endpoints[%q] = %q, want %q", code, doc.Endpoints[code], url)
		}
	}
}

// TestDavinciConfigurationRoute: served on the ingress edge, absent without it.
// Fixtures per the TestProviderIngressMetadata idiom (ingressauth_test.go):
// gatewayWithAuth already sets IngressBaseURL (testIngressBaseURL), so the
// enabled fixture needs no extra setup.
func TestDavinciConfigurationRoute(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/davinci-configuration", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "davinci_pas_submission_endpoint#2.0") {
		t.Fatal("body is not the davinci-configuration document")
	}

	off := &Gateway{cfg: Config{Role: "provider"}} // IngressEnabled/IngressBaseURL default false/""
	rr2 := httptest.NewRecorder()
	off.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/.well-known/davinci-configuration", nil))
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("ingress disabled: want 404, got %d", rr2.Code)
	}
}
