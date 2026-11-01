package fhirsor_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	// The order comes back exactly as the server holds it, subject included:
	// naming the patient for the network is the gateway's job, not the
	// connector's.
	if !bytes.Contains(raw, []byte(`"reference":"Patient/p1"`)) {
		t.Errorf("OpenOrder must keep the server's subject Patient/p1; got: %s", raw)
	}
}

// TestOpenOrder_ReadsOnlyAnActiveOrder: the open order is an order at active. The
// server here applies the search's status, as a FHIR server does, so a member
// whose only order is at another status has no open order, whichever resource
// type holds it. It reads as a gateway does, so a failed read is no empty answer.
func TestOpenOrder_ReadsOnlyAnActiveOrder(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p1","identifier":[{"system":"urn:shn:member","value":"MBR-OX"}]}`
	for _, rtype := range []string{"DeviceRequest", "ServiceRequest"} {
		for _, row := range []struct {
			status string
			open   bool
		}{{"draft", false}, {"revoked", false}, {"entered-in-error", false}, {"active", true}} {
			t.Run(rtype+"/"+row.status, func(t *testing.T) {
				order := `{"resourceType":"` + rtype + `","id":"o-1","status":"` + row.status + `"}`
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/fhir+json")
					switch strings.TrimSuffix(r.URL.Path, "/") {
					case "/Patient":
						w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
					case "/" + rtype:
						if s := r.URL.Query().Get("status"); s == "" || slices.Contains(strings.Split(s, ","), row.status) {
							w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + order + `}]}`))
							return
						}
						fallthrough
					default:
						w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
					}
				}))
				t.Cleanup(srv.Close)
				raw, ok, err := fhirsor.New(fhirclient.New(srv.URL, nil)).OpenOrderContext(context.Background(), "MBR-OX")
				if err != nil {
					t.Fatal(err)
				}
				if ok != row.open {
					t.Fatalf("open %v (%s), want %v", ok, raw, row.open)
				}
			})
		}
	}
}

// TestOpenOrder_KeepsSystemOfRecordBytes: the open order is the server's
// record byte for byte — layout, member order, escapes, number lexemes and
// the subject the server holds all survive.
func TestOpenOrder_KeepsSystemOfRecordBytes(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"pat-7","identifier":[{"system":"urn:shn:member","value":"MBR-7"}],"name":[{"family":"Seven"}],"birthDate":"1960-01-01"}`
	bs := string(rune(92))
	order := "{ \"subject\" : {\"reference\":\"Patient/pat-7\", \"display\":\"A " + bs + "u00e9 " + bs + "u003c\"},\n" +
		"  \"resourceType\":\"ServiceRequest\",\"id\":\"sr-7\",\"status\":\"active\",\"intent\":\"order\"," +
		"\"quantityQuantity\":{\"value\":1.50E+0},\"code\":{\"coding\":[{\"system\":\"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets\",\"code\":\"G0151\"}]} }"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch r.URL.Path {
		case "/Patient":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case "/ServiceRequest":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[ {"fullUrl":"x","resource":` + order + ` } ]}`))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	s := fhirsor.New(fhirclient.New(srv.URL, nil))
	raw, found, err := s.OpenOrderContext(context.Background(), "MBR-7")
	if err != nil || !found {
		t.Fatalf("OpenOrderContext = found %v, err %v; want the order", found, err)
	}
	if string(raw) != order {
		t.Fatalf("open order changed:\n got %s\nwant %s", raw, order)
	}
}
