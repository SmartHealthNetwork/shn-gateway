package fhirsor_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// openOrderFHIR stands up an httptest FHIR stub that:
//   - answers Patient?identifier=urn:shn:member|MBR-OX → 1 entry with id "p1"
//   - answers DeviceRequest?patient=p1&status=active → Bundle with one E0431 DeviceRequest
//   - returns empty for all other searches
func openOrderFHIR(t *testing.T) *fhirclient.Client {
	t.Helper()
	const patient = `{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"MBR-OX"}],"name":[{"family":"Oxygen"}],"birthDate":"1960-01-01"}`
	const deviceRequest = `{"resourceType":"DeviceRequest","id":"dr-1","status":"active","codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431","display":"Portable gaseous oxygen system, rental"}]},"subject":{"reference":"Patient/p1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		empty := `{"resourceType":"Bundle","type":"searchset"}`
		switch {
		case r.URL.Path == "/Patient" || r.URL.Path == "/Patient/":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case r.URL.Path == "/DeviceRequest" || r.URL.Path == "/DeviceRequest/":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + deviceRequest + `}]}`))
		default:
			w.Write([]byte(empty))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

func TestSoR_OpenOrder_DeviceRequest(t *testing.T) {
	s := fhirsor.New(openOrderFHIR(t))
	raw, ok := s.OpenOrder("MBR-OX")
	if !ok {
		t.Fatal("expected an order (found=true), got found=false")
	}
	sys, code, _, err := shnsdk.ParseOrderProductCoding(raw)
	if err != nil {
		t.Fatalf("ParseOrderProductCoding error: %v", err)
	}
	if code != "E0431" {
		t.Errorf("code = %q, want E0431 (sys=%q)", code, sys)
	}
	// The order's subject must be rewritten to the canonical member ref (Patient/MBR-OX), NOT the
	// HAPI-scoped store id (Patient/p1) — the payer-side order-dispatch AI-11 bind resolves the
	// subject by MEMBER id, so a scoped id 403s "inconsistent patient in order-dispatch".
	if !bytes.Contains(raw, []byte(`"reference":"Patient/MBR-OX"`)) {
		t.Errorf("OpenOrder must rewrite subject to Patient/MBR-OX (canonical member ref); got: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"reference":"Patient/p1"`)) {
		t.Errorf("OpenOrder leaked the HAPI-scoped subject Patient/p1: %s", raw)
	}
}
