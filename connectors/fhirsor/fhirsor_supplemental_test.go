package fhirsor_test

import (
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
