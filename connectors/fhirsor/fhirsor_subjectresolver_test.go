package fhirsor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func conformantProviderPatient(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../seed/conformant-personas.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	for _, entry := range bundle.Entry {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(entry.Resource, &head) == nil && head.ResourceType == "Patient" && head.ID == "MBR-COVERED" {
			return append([]byte(nil), entry.Resource...)
		}
	}
	t.Fatal("canonical provider fixture has no Patient/MBR-COVERED")
	return nil
}

func TestFHIRSoRRestoresLegacyCRDAddressingFromPublicDoorPayload(t *testing.T) {
	payload, err := os.ReadFile("testdata/public-door-crd.json")
	if err != nil {
		t.Fatal(err)
	}
	canonical := conformantProviderPatient(t)
	for _, tc := range []struct {
		name      string
		patient   []byte
		wantCode  int
		wantAuthz int32
	}{
		{name: "issued PCI reaches dispatch", patient: canonical, wantCode: http.StatusBadGateway, wantAuthz: 1},
		{name: "missing issued PCI is context missing", patient: []byte(`{"resourceType":"Patient","id":"MBR-COVERED"}`), wantCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/fhir/Patient/MBR-COVERED" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = w.Write(tc.patient)
			}))
			defer fhir.Close()

			var authzCalls atomic.Int32
			authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authzCalls.Add(1)
				var request struct {
					Operation  string `json:"operation"`
					SubjectPCI string `json:"subjectPCI"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Operation != "crd-order-select" || request.SubjectPCI != "pci:223665feed95ab0cd1e7b0cda6cfa948" {
					t.Errorf("authorization request = %+v", request)
				}
				http.Error(w, "intentional stop after addressing", http.StatusServiceUnavailable)
			}))
			defer authz.Close()

			provider, err := shnsdk.GenerateIdentity("provider")
			if err != nil {
				t.Fatal(err)
			}
			payer, err := shnsdk.GenerateIdentity("conformance-payer")
			if err != nil {
				t.Fatal(err)
			}
			reg := shnsdk.NewRegistry()
			reg.Set("conformance-payer", shnsdk.RegistryEntry{
				ID: "conformance-payer", Role: "payer", BaseURL: authz.URL,
				EncPub: payer.EncPub, SignPub: payer.SignPub,
				MessageFrames: shnsdk.SupportedMessageFrames(), RequestFrames: engine.SupportedRequestFrames(),
				ContractVersions: []string{"pa.crd@2.0"},
			})
			router, err := engine.NewConfigPayerRouter([]engine.PayerDirectoryEntry{{
				System: shnsdk.CMSPayerIdentity.System, Value: "00001", HolderID: "conformance-payer",
			}})
			if err != nil {
				t.Fatal(err)
			}
			cfg := engine.Config{
				Role: "provider", HolderID: "provider", Identity: provider,
				SoR: NewFromURL(fhir.URL+"/fhir", fhir.Client()), Store: engine.NewMemStore(),
				Reg: reg, PayerRouter: router, AuthzURL: authz.URL, HubURL: authz.URL,
				Client: authz.Client(), ConformanceEnforcement: engine.EnforcementNone,
			}
			engine.EnableIngressForTest(&cfg)
			gateway, err := engine.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/cds-services/shn-order-sign", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(response, req)
			if response.Code != tc.wantCode || authzCalls.Load() != tc.wantAuthz {
				t.Fatalf("response=%d body=%s authzCalls=%d", response.Code, response.Body.String(), authzCalls.Load())
			}
			if tc.wantAuthz == 0 && !strings.Contains(response.Body.String(), "context_missing") {
				t.Fatalf("missing issued identity response = %s", response.Body.String())
			}
		})
	}
}

func TestSoRResolveSubjectReadsExactIssuedPCI(t *testing.T) {
	patient := conformantProviderPatient(t)
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path != "/fhir/Patient/MBR-COVERED" {
			t.Errorf("read path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write(patient)
	}))
	defer srv.Close()
	sor := NewFromURL(srv.URL+"/fhir", srv.Client())

	for _, ref := range []engine.PatientReference{
		{Holder: "provider", System: "fhir-relative", Value: "Patient/MBR-COVERED"},
		{Holder: "provider", System: srv.URL + "/fhir", Value: "Patient/MBR-COVERED"},
	} {
		pci, found, err := sor.ResolveSubject(context.Background(), ref)
		if err != nil || !found || pci != "pci:223665feed95ab0cd1e7b0cda6cfa948" {
			t.Fatalf("ResolveSubject(%+v) = (%q,%v,%v)", ref, pci, found, err)
		}
	}
	if reads.Load() != 2 {
		t.Fatalf("FHIR reads = %d, want 2", reads.Load())
	}
}

func TestSoRResolveSubjectRefusesNonAuthoritativeReferencesBeforeRead(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		http.Error(w, "must not read", http.StatusInternalServerError)
	}))
	defer srv.Close()
	sor := NewFromURL(srv.URL+"/fhir", srv.Client())

	for _, ref := range []engine.PatientReference{
		{System: "fhir-relative", Value: "Patient/MBR-COVERED"},
		{Holder: "provider", System: "https://other.example/fhir", Value: "Patient/MBR-COVERED"},
		{Holder: "provider", System: "fhir-relative", Value: "Observation/MBR-COVERED"},
		{Holder: "provider", System: "fhir-relative", Value: "Patient/MBR-COVERED/_history/1"},
		{Holder: "provider", System: "fhir-relative", Value: "Patient/../Observation"},
	} {
		if pci, found, err := sor.ResolveSubject(context.Background(), ref); err != nil || found || pci != "" {
			t.Fatalf("ResolveSubject(%+v) = (%q,%v,%v), want absent", ref, pci, found, err)
		}
	}
	if reads.Load() != 0 {
		t.Fatalf("non-authoritative references caused %d FHIR reads", reads.Load())
	}
}

func TestSoRResolveSubjectRefusesInvalidPatientIdentity(t *testing.T) {
	canonical := conformantProviderPatient(t)
	var canonicalPatient map[string]any
	if err := json.Unmarshal(canonical, &canonicalPatient); err != nil {
		t.Fatal(err)
	}
	patient := func(mutate func(map[string]any)) string {
		copyRaw, _ := json.Marshal(canonicalPatient)
		var copyPatient map[string]any
		_ = json.Unmarshal(copyRaw, &copyPatient)
		mutate(copyPatient)
		out, _ := json.Marshal(copyPatient)
		return string(out)
	}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		found   bool
		wantErr bool
	}{
		{name: "not found", status: http.StatusNotFound},
		{name: "missing PCI", status: http.StatusOK, body: patient(func(p map[string]any) { p["identifier"] = []any{} })},
		{name: "empty PCI", status: http.StatusOK, body: patient(func(p map[string]any) { p["identifier"] = []any{map[string]any{"system": "urn:shn:pci", "value": " "}} }), wantErr: true},
		{name: "duplicate PCI", status: http.StatusOK, body: patient(func(p map[string]any) {
			p["identifier"] = []any{map[string]any{"system": "urn:shn:pci", "value": "pci:one"}, map[string]any{"system": "urn:shn:pci", "value": "pci:two"}}
		}), wantErr: true},
		{name: "wrong resource", status: http.StatusOK, body: `{"resourceType":"Observation","id":"MBR-COVERED"}`, wantErr: true},
		{name: "wrong Patient id", status: http.StatusOK, body: patient(func(p map[string]any) { p["id"] = "another-patient" }), wantErr: true},
		{name: "read failure", status: http.StatusServiceUnavailable, body: `backend private detail`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			pci, found, err := NewFromURL(srv.URL, srv.Client()).ResolveSubject(context.Background(), engine.PatientReference{Holder: "provider", System: "fhir-relative", Value: "Patient/MBR-COVERED"})
			if pci != "" || found != tc.found || (err != nil) != tc.wantErr {
				t.Fatalf("ResolveSubject = (%q,%v,%v), want found=%v err=%v", pci, found, err, tc.found, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), tc.body) {
				t.Fatalf("resolver error disclosed response body: %v", err)
			}
		})
	}
}
