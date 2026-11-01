//go:build !shn_coreonly

package engine_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// A facility gateway reading its own FHIR system through the bundled
// connector refuses that system's answer that repeats a member name, at every
// level, and records it on message integrity, the call malformed: wired end to
// end, from the connector's read to the answer and the record. The member's
// Patient search repeating a member at the Bundle level, which the connector
// read one way before, is the patient-match refusal (403); the member's own
// Patient read repeating one is the invalid response (502), where the
// connector's lenient read once reached the patient fence ("another patient's
// resource"). The same system answering cleanly passes both reads and answers
// that no named records are held.
func TestFederatedQuery_SystemRepeatingAMemberThroughTheConnector(t *testing.T) {
	t.Parallel()
	patient := e2ePatient(engine.RepeatedMemberE2EMember())
	clean := e2eSearchset(patient)
	for name, row := range map[string]struct {
		search, read string
		status       int
		msg          string
		integrity    bool
	}{
		"clean": {clean, patient, http.StatusNotFound, "named records not held", false},
		"the Patient search repeating a member at the Bundle level": {
			strings.Replace(clean, `"type":"searchset"`, `"type":"searchset","type":"searchset"`, 1), patient,
			http.StatusForbidden, "", true},
		"the own Patient read repeating a member": {
			clean, strings.Replace(patient, `"id":"e2e-1"`, `"id":"e2e-1","ID":"e2e-1"`, 1),
			http.StatusBadGateway, "system of record returned an invalid response", true},
	} {
		for _, level := range []engine.ConformanceEnforcement{engine.EnforcementNone, engine.EnforcementObserve, engine.EnforcementStructural, engine.EnforcementStrict} {
			srv := e2eFHIRServer(t, row.search, row.read)
			status, body, rec := engine.FederatedQueryWithSoRForTest(t, level, fhirsor.NewFromURL(srv.URL, srv.Client()))
			if status != row.status || (row.msg != "" && !bytes.Contains(body, []byte(row.msg))) || (row.msg == "" && bytes.Contains(body, []byte("records"))) {
				t.Errorf("%s/%s: answered %d %s, want %d naming %q", name, level, status, body, row.status, row.msg)
			}
			integrity := rec.Outcome == engine.ExchangeRefused && rec.Rule == engine.RefusalIntegrity && rec.Backend != nil && rec.Backend.ErrorClass == engine.BackendMalformed
			if integrity != row.integrity {
				t.Errorf("%s/%s: record %s %s/%s backend %+v, want refused on integrity, malformed: %v", name, level, rec.Outcome, rec.RefusedBy, rec.Rule, rec.Backend, row.integrity)
			}
		}
	}
}
