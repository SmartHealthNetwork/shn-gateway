//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// signalAckPatient is the Patient a refused acknowledgement holds, and the
// name no refusal may carry.
const (
	signalAckLeak    = "Van Doe"
	signalAckPatient = `{"resourceType":"Patient","id":"p1","name":[{"family":"` + signalAckLeak + `"}]}`
)

// signalAckRow is one answer a subscriber's endpoint gives a no-content
// notification, and whether its gateway carries it.
type signalAckRow struct {
	contentType, body string
	carried           bool
}

// signalAckRows are the acknowledgements a FHIR server gives the transaction
// a rest-hook notification is delivered as (the reference PAS subscriber's
// first), carried as sent, and the ones holding a resource, refused.
var signalAckRows = map[string]signalAckRow{
	"the reference subscriber's acknowledgement":           {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[]}`, true},
	"an acknowledgement whose entries hold their response": {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","location":"Bundle/b1/_history/1"}}]}`, true},
	"a batch-response": {"application/fhir+json", `{"resourceType":"Bundle","type":"batch-response","entry":[{"response":{"status":"201 Created"}}]}`, true},
	"an acknowledgement with an outcome holding none": {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","outcome":{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}}}]}`, true},
	// Decided by structure: a reference is carried as the endpoint wrote it,
	// whatever it names.
	"an acknowledgement whose location names a Patient": {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"201 Created","location":"Patient/p1/_history/1"}}]}`, true},
	"an XML acknowledgement":                            {"application/fhir+xml", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><status value="200 OK"/></response></entry></Bundle>`, true},
	"an acknowledgement holding a Patient in an entry":  {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"resource":` + signalAckPatient + `,"response":{"status":"200 OK"}}]}`, false},
	"an acknowledgement holding a contained Patient":    {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","contained":[` + signalAckPatient + `],"entry":[]}`, false},
	"an acknowledgement whose outcome holds a Patient":  {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","outcome":{"resourceType":"OperationOutcome","contained":[` + signalAckPatient + `],"issue":[]}}}]}`, false},
	"an entry holding more than its response":           {"application/fhir+json", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"fullUrl":"https://ehr.test/fhir/Patient/p1","response":{"status":"200 OK"}}]}`, false},
	"a searchset": {"application/fhir+json", `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + signalAckPatient + `}]}`, false},
	"an XML acknowledgement holding a Patient": {"application/fhir+xml", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><resource><Patient><name><family value="` + signalAckLeak + `"/></name></Patient></resource><response><status value="200 OK"/></response></entry></Bundle>`, false},
}

// answering is an endpoint that answers status with row's body.
func (row signalAckRow) answering(status int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", row.contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(row.body))
	}
}

// wantSignalAck holds a no-content notification's answer a to row: carried
// as the endpoint answered it, or refused 502 on authority by the
// subscriber's gateway, carrying none of the Patient.
func wantSignalAck(t *testing.T, row signalAckRow, status int, a subAnswer, x ExchangeRecord, refusedBy string) {
	t.Helper()
	if row.carried {
		if a.status != status || !bytes.Equal(a.body, []byte(row.body)) {
			t.Fatalf("answered %d %s, want the endpoint's %d carried as sent", a.status, a.body, status)
		}
		if x.Outcome == ExchangeRefused {
			t.Fatalf("record %+v, want delivered", x)
		}
		return
	}
	if a.status != http.StatusBadGateway {
		t.Fatalf("answered %d %s, want 502", a.status, a.body)
	}
	if x.Outcome != ExchangeRefused || x.RefusedBy != refusedBy || x.Rule != RefusalAuthority {
		t.Fatalf("record %s by %q on %q, want refused by %q on authority", x.Outcome, x.RefusedBy, x.Rule, refusedBy)
	}
	if strings.Contains(string(a.body), signalAckLeak) || strings.Contains(string(a.body), "Patient") && strings.Contains(string(a.body), `"p1"`) {
		t.Fatalf("the refusal carries the Patient: %s", a.body)
	}
}

// On pas-subscription-signal, the provider's endpoint's acknowledgement of a
// no-content notification (an event or a handshake) is carried to the payer
// as sent when it holds no resource, at every conformance level and at a 2xx
// or an error status: refusing it would tell the payer a notification its
// subscriber took had failed, and a payer that sends it again would deliver
// it twice. One holding a resource is refused 502 on authority, as any answer
// on a leg that names no patient is.
func TestPASSignal_TheSubscribersAcknowledgement(t *testing.T) {
	auth := []string{"Authorization: Bearer ehr-secret"}
	for _, level := range allLevels {
		for name, row := range signalAckRows {
			for _, notification := range []struct{ name, body string }{{"an event", pasEvent(pasSignalPayerBase)}, {"a handshake", pasHandshake()}} {
				for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
					t.Run(level.String()+"/"+name+"/"+notification.name+"/"+http.StatusText(status), func(t *testing.T) {
						p := newPASSignalProvider(t, level)
						p.answer = row.answering(status)
						a := p.signal(t, p.frame(t, pasBinding, pasProviderEndpoint, auth), []byte(notification.body))
						x := p.got.only(t)
						if x.Exchange != legPASSubscriptionSignal {
							t.Fatalf("recorded on %q, want %s", x.Exchange, legPASSubscriptionSignal)
						}
						if len(p.bodies) != 1 {
							t.Fatalf("the endpoint got %d notifications, want the one it answered", len(p.bodies))
						}
						wantSignalAck(t, row, status, a, x, RefusedByProviderGateway)
					})
				}
			}
		}
	}
}

// On cdex-task-signal, the same rule holds for the payer's endpoint's
// acknowledgement of a provider's Task notification: the one rule both
// no-content legs share.
func TestCDexSignal_TheSubscribersAcknowledgement(t *testing.T) {
	event := []byte(signalEvent(signalProviderBase + "/Task/t1"))
	for _, level := range allLevels {
		for name, row := range signalAckRows {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
				t.Run(level.String()+"/"+name+"/"+http.StatusText(status), func(t *testing.T) {
					p := newSignalPayer(t, level)
					p.answer = row.answering(status)
					se, sh := p.sealed(t, "provider-x", payerEndpoint, []string{"Authorization: Bearer payer-secret"})
					a := p.signal(t, map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
						shnsdk.FrameHeaderStatedBase: signalProviderBase, "Content-Type": "application/fhir+json"}, event)
					x := p.got.only(t)
					if x.Exchange != legCDexTaskSignal {
						t.Fatalf("recorded on %q, want %s", x.Exchange, legCDexTaskSignal)
					}
					if len(p.bodies) != 1 {
						t.Fatalf("the endpoint got %d notifications, want the one it answered", len(p.bodies))
					}
					wantSignalAck(t, row, status, a, x, RefusedByPayerGateway)
				})
			}
		}
	}
}
