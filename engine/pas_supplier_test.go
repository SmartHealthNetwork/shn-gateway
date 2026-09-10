package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPASSubmitSupplierRetention(t *testing.T) {
	supplier := []byte(`{"resourceType":"Organization","id":"supplier","name":"Actual supplier","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}]}`)
	for _, ref := range []string{"Organization/supplier", "https://provider.example/fhir/Organization/supplier"} {
		t.Run(ref, func(t *testing.T) {
			original := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://provider.example/fhir/DeviceRequest/order","resource":{"resourceType":"DeviceRequest","id":"order","performer":{"reference":"` + ref + `"}}}]}`)
			got, err := retainPASSubmitSupplier(original, supplier)
			if err != nil {
				t.Fatal(err)
			}
			var bundle struct {
				Entry []struct {
					FullURL  string          `json:"fullUrl"`
					Resource json.RawMessage `json:"resource"`
				} `json:"entry"`
			}
			if err := json.Unmarshal(got, &bundle); err != nil {
				t.Fatal(err)
			}
			if len(bundle.Entry) != 2 || bundle.Entry[1].FullURL != "https://provider.example/fhir/Organization/supplier" || !bytes.Equal(bundle.Entry[1].Resource, supplier) {
				t.Fatalf("supplier not retained: %s", got)
			}
			if !strings.Contains(string(bundle.Entry[0].Resource), `"reference":"`+ref+`"`) {
				t.Fatalf("performer rewritten: %s", got)
			}
			for name, mutation := range map[string][]byte{
				"missing":    nil,
				"wrong type": []byte(strings.Replace(string(supplier), `"Organization"`, `"Patient"`, 1)),
				"wrong id":   []byte(strings.Replace(string(supplier), `"supplier"`, `"other"`, 1)),
				"malformed":  []byte(`{`),
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := retainPASSubmitSupplier(original, mutation); err == nil {
						t.Fatal("invalid supplier accepted")
					}
				})
			}
			if _, err := retainPASSubmitSupplier(got, supplier); err == nil {
				t.Fatal("conflicting entry accepted")
			}
		})
	}
	service := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","id":"order"}}]}`)
	if got, err := retainPASSubmitSupplier(service, nil); err != nil || !bytes.Equal(got, service) {
		t.Fatalf("service request changed: %s %v", got, err)
	}
}

func TestDispatchPASSuppliesActualOrganization(t *testing.T) {
	order, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", "Organization/org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	validator := &recordingValidator{valid: true}
	fixture := newDispatchFixtureWith(t, "MBR-OX", Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"}, order, "Organization/org-dme-ox", supplier, func(c *Config) { c.Validator = validator })
	rec := httptest.NewRecorder()
	fixture.gw.handleDispatch(rec, httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"MBR-OX"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch: %d %s", rec.Code, rec.Body.String())
	}
	found := false
	for _, body := range validator.calls {
		var bundle struct {
			ResourceType string `json:"resourceType"`
			Entry        []struct {
				FullURL  string          `json:"fullUrl"`
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		}
		if json.Unmarshal(body, &bundle) != nil || bundle.ResourceType != "Bundle" {
			continue
		}
		isClaim := false
		for _, entry := range bundle.Entry {
			var identity struct {
				ResourceType string `json:"resourceType"`
			}
			_ = json.Unmarshal(entry.Resource, &identity)
			if identity.ResourceType == "Claim" {
				isClaim = true
			}
		}
		if !isClaim {
			continue
		}
		for _, entry := range bundle.Entry {
			var actual, want any
			_ = json.Unmarshal(entry.Resource, &actual)
			_ = json.Unmarshal(supplier, &want)
			if reflect.DeepEqual(actual, want) && entry.FullURL == "https://shn.example/fhir/Organization/org-dme-ox" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("actual SoR supplier absent from validated PAS request")
	}
}

func TestPASSubmitSupplierRejectsInvalidLinkage(t *testing.T) {
	const original = `{"resourceType":"Bundle","entry":[{"fullUrl":"https://provider.example/fhir/DeviceRequest/order","resource":{"resourceType":"DeviceRequest","id":"order","performer":{"reference":"Organization/supplier"}}}]}`
	supplier := []byte(`{"resourceType":"Organization","id":"supplier"}`)
	for name, body := range map[string]string{
		"missing performer":           strings.Replace(original, `"Organization/supplier"`, `""`, 1),
		"different performer":         strings.Replace(original, `"Organization/supplier"`, `"Organization/other"`, 1),
		"query":                       strings.Replace(original, `"Organization/supplier"`, `"Organization/supplier?"`, 1),
		"fragment":                    strings.Replace(original, `"Organization/supplier"`, `"Organization/supplier#x"`, 1),
		"encoded path":                strings.Replace(original, `"Organization/supplier"`, `"Organization%2Fsupplier"`, 1),
		"unsupported scheme":          strings.Replace(original, `"Organization/supplier"`, `"ftp://provider.example/fhir/Organization/supplier"`, 1),
		"credentials":                 strings.Replace(original, `"Organization/supplier"`, `"https://user@provider.example/fhir/Organization/supplier"`, 1),
		"invalid base":                strings.Replace(original, `https://provider.example/fhir/DeviceRequest/order`, `urn:uuid:order`, 1),
		"base identity mismatch":      strings.Replace(original, `https://provider.example/fhir/DeviceRequest/order`, `https://provider.example/fhir/DeviceRequest/other`, 1),
		"same id other fullUrl":       strings.Replace(original, `}]}`, `},{"fullUrl":"https://other.example/Organization/supplier","resource":{"resourceType":"Organization","id":"supplier"}}]}`, 1),
		"same fullUrl other resource": strings.Replace(original, `}]}`, `},{"fullUrl":"https://provider.example/fhir/Organization/supplier","resource":{"resourceType":"Organization","id":"other"}}]}`, 1),
		"duplicate order":             strings.Replace(original, `}]}`, `},{"fullUrl":"https://provider.example/fhir/DeviceRequest/second","resource":{"resourceType":"DeviceRequest","id":"second"}}]}`, 1),
		"supplier without order":      strings.Replace(original, `"DeviceRequest"`, `"ServiceRequest"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := retainPASSubmitSupplier([]byte(body), supplier); err == nil {
				t.Fatal("invalid linkage accepted")
			}
		})
	}
}
