//go:build !shn_coreonly

package engine_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// A provider gateway reading its own FHIR system through the bundled
// connector, for a request its participant sends, refuses that system's
// answer that repeats a member name (the connector read it one way before):
// the participant's own caller is answered the system's invalid response, the
// same answer, byte for byte, and the same record as that system answering
// unreadably any other way. The same system answering cleanly lets the
// request through to the payer.
func TestCRDIngress_SystemRepeatingAMemberThroughTheConnector(t *testing.T) {
	patient := e2ePatient(engine.RepeatedMemberE2EMember())
	clean := e2eSearchset(patient)
	send := func(search string) (int, string, engine.ExchangeRecord) {
		t.Helper()
		srv := e2eFHIRServer(t, search, patient)
		return engine.CRDIngressWithSoRForTest(t, fhirsor.NewFromURL(srv.URL, srv.Client()))
	}
	if status, body, rec := send(clean); status != http.StatusOK || rec.Outcome != engine.ExchangeAnswered {
		t.Fatalf("clean: answered %d %s, record %s", status, body, rec.Outcome)
	}
	status, body, rec := send(strings.Replace(clean, `"type":"searchset"`, `"type":"searchset","type":"searchset"`, 1))
	cstatus, cbody, crec := send(`not JSON`)
	if status != http.StatusBadGateway || !strings.Contains(body, "system of record returned an invalid response") {
		t.Fatalf("a Patient search repeating a member: answered %d %s, want the system's invalid response", status, body)
	}
	if status != cstatus || body != cbody {
		t.Errorf("a Patient search repeating a member answered %d %s, one not JSON %d %s: want them byte-equal", status, body, cstatus, cbody)
	}
	if rec.Outcome != crec.Outcome || rec.Rule != crec.Rule || rec.Rule == engine.RefusalIntegrity {
		t.Errorf("record %s %s/%s, the record of one not JSON %s %s/%s: want the same, a requester's own read recorded as before", rec.Outcome, rec.RefusedBy, rec.Rule, crec.Outcome, crec.RefusedBy, crec.Rule)
	}
}
