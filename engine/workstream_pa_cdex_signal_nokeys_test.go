//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer's gateway that holds no subscription keys (its identity names no
// holder, or another) cannot open a notification's sealed endpoint: it answers
// 500, its own failure and never a refusal, records it so, and delivers
// nothing, at every level. A notification framed for the payer under keys it
// no longer holds is never read as another payer's (403).
func TestCDexSignal_APayersGatewayWithNoKeysFailsItsOwn(t *testing.T) {
	event := []byte(signalEvent(signalProviderBase + "/Task/t1"))
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			p := newSignalPayer(t, level)
			se, sh := p.sealed(t, "provider-x", payerEndpoint, []string{"Authorization: Bearer payer-secret"})
			p.g.paState().subscriberKeys = subscription.Keys{}
			a := p.signal(t, map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
				shnsdk.FrameHeaderStatedBase: signalProviderBase, "Content-Type": "application/fhir+json"}, event)
			if a.status != http.StatusInternalServerError || !strings.Contains(string(a.body), "holds no subscription keys") {
				t.Fatalf("answered %d %s, want 500 naming the missing keys", a.status, a.body)
			}
			// Its own failure settles on no network primitive: the record
			// names the rule "other", as every own failure an inbound leg
			// answers through its refusal path does.
			x := p.got.only(t)
			if x.Rule != "other" || x.Status != http.StatusInternalServerError {
				t.Fatalf("record %s on %q %d, want its own failure (rule other, 500)", x.Outcome, x.Rule, x.Status)
			}
			if len(p.bodies) != 0 {
				t.Fatalf("the endpoint got %d notifications", len(p.bodies))
			}
		})
	}
}
