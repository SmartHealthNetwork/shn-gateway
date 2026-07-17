// gateway/engine/relay_adversarial_test.go
//
// Adversarial row for the opaque-payload message-frame spec (2026-07-17):
// valid exchange − one mutation (the response-leg authz token) → reject.
//
// A frame-capable responder (payer) seals its application answer — any status —
// as a v1 message frame inside a 200-to-Hub response leg (respondLegError →
// buildResponseLeg), which mints a response-leg authz token bound to
// sha256(ciphertext) exactly like any other response leg. roundTripInner decodes
// that frame into a typed *RelayError so callers see the recipient's real
// status+body instead of the Hub's generic mechanical fault.
//
// This row proves the relay path is not a bypass of per-leg authorization
// (AI-11 on errors): a relayed response whose response-leg authz token is
// MUTATED — re-signed with a key the originator's cfg.AuthzPub does not
// correspond to — is rejected by roundTripInner's VerifyBound check
// ("response leg authorization failed") BEFORE the frame is ever decoded.
// The mutated relay is dropped, never surfaced as a *RelayError.
//
// The valid CONTROL for this mutant is
// TestRoundTrip_RecipientNon2xx_SurfacesRelayError (relay_roundtrip_test.go):
// the exact same exchange, minus the one mutation, yields *RelayError{502, oo}.
// This test is that same exchange PLUS corruptResponseToken — the one mutation —
// asserting the opposite outcome.
package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestAdversarial_RelayResponseTokenMutated_Rejected(t *testing.T) {
	env := newInProcessExchange(t)
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	env.payerReturns(LegResult{Status: 502, ResponseFHIR: oo})
	env.corruptResponseToken(t) // THE one mutation

	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})

	var re *RelayError
	if errors.As(err, &re) {
		t.Fatalf("mutated relay must NOT surface as a relay, got %d", re.Status)
	}
	if err == nil || !strings.Contains(err.Error(), "response leg authorization failed") {
		t.Fatalf("want \"response leg authorization failed\", got %v", err)
	}
}
