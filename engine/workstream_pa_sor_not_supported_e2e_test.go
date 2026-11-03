//go:build !shn_coreonly

package engine_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// A provider gateway reading its own FHIR system through the bundled
// connector, for a CDS Hooks request that carries no coverage: a coverage
// search the system answers definitely unsupported (501, or an error status
// whose OperationOutcome's every error issue is not-supported) is refused
// 422, the system as connected cannot supply the coverage; any other error
// answer, an OperationOutcome that pairs not-supported with another error
// code included, is a backend failure, 502. The answer's text is never read.
// Each holds at every level.
func TestCRDIngress_CoverageSearchNotSupportedThroughTheConnector(t *testing.T) {
	patient := e2ePatient("example", "1960-01-01", "Doe")
	outcome := func(issues ...string) string {
		return `{"resourceType":"OperationOutcome","issue":[` + strings.Join(issues, ",") + `]}`
	}
	const (
		notSupported = `{"severity":"error","code":"not-supported","diagnostics":"an error occurred"}`
		invalid      = `{"severity":"error","code":"invalid","diagnostics":"the search is not supported"}`
		cannot       = "the system of record cannot search for it"
		answered     = "the system of record answered the coverage search with an error"
	)
	for _, row := range []struct {
		name   string
		status int
		body   string
		want   int
		msg    string
	}{
		{"a system holding no coverage", http.StatusOK, `{"resourceType":"Bundle","type":"searchset"}`, http.StatusPreconditionFailed, "no coverage in request or system of record"},
		{"not-supported", http.StatusBadRequest, outcome(notSupported), http.StatusUnprocessableEntity, cannot},
		{"501", http.StatusNotImplemented, "", http.StatusUnprocessableEntity, cannot},
		{"not-supported beside another error code", http.StatusBadRequest, outcome(notSupported, invalid), http.StatusBadGateway, answered},
		{"another code whose text says not supported", http.StatusBadRequest, outcome(invalid), http.StatusBadGateway, answered},
		{"404 with no answer", http.StatusNotFound, "", http.StatusBadGateway, answered},
	} {
		for _, level := range engine.AllLevelsForTest {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				coverageSearched := false
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/fhir+json")
					switch r.URL.Path {
					case "/Patient":
						_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
					case "/Patient/e2e-1":
						_, _ = w.Write([]byte(patient))
					case "/Coverage":
						coverageSearched = true
						w.WriteHeader(row.status)
						_, _ = w.Write([]byte(row.body))
					default:
						_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
					}
				}))
				t.Cleanup(srv.Close)
				status, body := engine.CRDIngressWithoutCoverageForTest(t, fhirsor.NewFromURL(srv.URL, srv.Client()), level)
				if status != row.want || !strings.Contains(body, row.msg) {
					t.Fatalf("answered %d %s, want %d %q", status, body, row.want, row.msg)
				}
				if !coverageSearched {
					t.Fatal("the coverage was not searched")
				}
			})
		}
	}
}
