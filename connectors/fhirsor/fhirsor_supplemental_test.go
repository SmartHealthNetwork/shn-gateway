package fhirsor_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// fakeReportFHIR returns the operative-note DiagnosticReport ONLY when the DiagnosticReport
// search's code filter includes the operative-note LOINC (11504-8). This proves
// SupplementalReport searches the whole ReportValueSet, not just the imaging code.
func fakeReportFHIR(t *testing.T) *fhirclient.Client {
	t.Helper()
	const patient = `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"MBR-OP"}],"name":[{"family":"Op"}],"birthDate":"1970-01-01"}`
	const opNote = `{"resourceType":"DiagnosticReport","id":"op-1","status":"final","code":{"coding":[{"system":"http://loinc.org","code":"11504-8"}]},"subject":{"reference":"Patient/p"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case strings.HasPrefix(r.URL.Path, "/DiagnosticReport"):
			if strings.Contains(r.URL.Query().Get("code"), shnsdk.ReportOperativeNoteLOINC) {
				w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + opNote + `}]}`))
				return
			}
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return fhirclient.New(srv.URL, nil)
}

func TestSupplementalReport_FindsOperativeNote(t *testing.T) {
	s := fhirsor.New(fakeReportFHIR(t))
	raw, ok := s.SupplementalReport("MBR-OP")
	if !ok {
		t.Fatal("want ok=true: SupplementalReport must search the operative-note LOINC (11504-8), not only 18748-4")
	}
	if !strings.Contains(string(raw), `"11504-8"`) {
		t.Errorf("returned report = %s, want the operative-note DR", raw)
	}
}

// TestSupplementalReport_KeepsSystemOfRecordBytes: the supplemental report is
// the server's record byte for byte — layout, member order, escapes, number
// lexemes and the subject the server holds all survive. Naming the member's
// network patient is the gateway's registered edit, not the connector's.
func TestSupplementalReport_KeepsSystemOfRecordBytes(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"pat-7","identifier":[{"system":"urn:shn:member","value":"MBR-7"}],"name":[{"family":"Seven"}],"birthDate":"1960-01-01"}`
	bs := string(rune(92))
	report := "{ \"subject\" : {\"reference\":\"Patient/pat-7\", \"display\":\"A " + bs + "u00e9 " + bs + "u003c\"},\n" +
		"  \"resourceType\":\"DiagnosticReport\",\"id\":\"dr-7\",\"status\":\"final\"," +
		"\"extension\":[{\"url\":\"urn:x\",\"valueDecimal\":1.50E+0}],\"code\":{\"coding\":[{\"system\":\"http://loinc.org\",\"code\":\"11504-8\"}]} }"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch r.URL.Path {
		case "/Patient":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
		case "/DiagnosticReport":
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[ {"fullUrl":"x","resource":` + report + ` } ]}`))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	s := fhirsor.New(fhirclient.New(srv.URL, nil))
	raw, found, err := s.SupplementalReportContext(context.Background(), "MBR-7")
	if err != nil || !found {
		t.Fatalf("SupplementalReportContext = found %v, err %v; want the report", found, err)
	}
	if string(raw) != report {
		t.Fatalf("supplemental report changed:\n got %s\nwant %s", raw, report)
	}
}
