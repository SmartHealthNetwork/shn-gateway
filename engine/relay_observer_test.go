// gateway/engine/relay_observer_test.go
package engine

import (
	"errors"
	"testing"
)

func TestRoundTrip_RelayError_EmitsLegResponseWithStatus(t *testing.T) {
	env := newInProcessExchange(t)
	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
	env.payerReturns(LegResult{Status: 502, ResponseFHIR: []byte(`{"resourceType":"OperationOutcome"}`)})
	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	var re *RelayError
	if !errors.As(err, &re) {
		t.Fatalf("want *RelayError, got %v", err)
	}
	var got *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.response" {
			got = &events[i]
		}
	}
	if got == nil {
		t.Fatal("expected a leg.response event for a relayed non-2xx")
	}
	if got.Status != 502 {
		t.Fatalf("leg.response Status = %d, want 502", got.Status)
	}
}
