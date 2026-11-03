//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// gatewayRoles are the roles a gateway registers as.
var gatewayRoles = []string{"payer", "provider", "facility", "phg"}

// A leg goes only to a gateway of a role its row names for the recipient. A
// gateway of another role that receives one anyway (a misrouted or injected
// envelope) refuses it 422 on routing before any token is read or its
// participant's system called, recorded as its own refusal, at every
// conformance level: a network rule. Every leg the compiled-in workstreams
// declare, to a gateway of every other role.
func TestInbound_ALegForAnotherRoleIsRefusedOnRouting(t *testing.T) {
	t.Parallel()
	refused := 0
	for _, leg := range networkLegs() {
		spec, _ := legSpecFor(leg)
		for _, role := range gatewayRoles {
			if slices.Contains(spec.RecipientRoles, role) {
				continue
			}
			for _, level := range allLevels {
				t.Run(leg+"/"+role+"/"+level.String(), func(t *testing.T) {
					p, got := newRecordingLevelPayer(t, level)
					p.g.cfg.Role = role
					ans := p.send(t, leg, "", []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`))
					var e struct{ Error string }
					if err := json.Unmarshal(ans.body, &e); err != nil || ans.status != http.StatusUnprocessableEntity || ans.framed ||
						e.Error != recipientRoleRefusal(leg, role, spec.RecipientRoles) {
						t.Fatalf("answer %d (framed %v) %s, want a bare 422 %q", ans.status, ans.framed, ans.body, recipientRoleRefusal(leg, role, spec.RecipientRoles))
					}
					wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, gatewayParty(role), RefusalRouting)
					if n := p.partner.calls.Load(); n != 0 {
						t.Errorf("the participant's system was called %d time(s)", n)
					}
				})
			}
			refused++
		}
	}
	if refused < len(networkLegs()) {
		t.Fatalf("%d leg and role pairs refused for %d legs: every leg has a role it is not answered by", refused, len(networkLegs()))
	}
}

// The control: a leg delivered to a gateway of the role its row names passes
// the role check, whatever its handler then answers.
func TestInbound_ALegForItsOwnRoleIsNotRefusedOnRole(t *testing.T) {
	t.Parallel()
	for _, leg := range networkLegs() {
		t.Run(leg, func(t *testing.T) {
			p := newLevelPayer(t, EnforcementNone)
			p.answering(t, leg)
			ans := p.send(t, leg, "", []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`))
			var e struct{ Error string }
			_ = json.Unmarshal(ans.body, &e)
			spec, _ := legSpecFor(leg)
			if e.Error == recipientRoleRefusal(leg, p.g.cfg.Role, spec.RecipientRoles) {
				t.Fatalf("a %s gateway refused %s on its role: %d %s", p.g.cfg.Role, leg, ans.status, ans.body)
			}
		})
	}
}

// A gateway registered with no role takes no leg: unknown is not empty.
func TestInbound_AGatewayWithNoRoleTakesNoLeg(t *testing.T) {
	p, got := newRecordingLevelPayer(t, EnforcementNone)
	p.g.cfg.Role = ""
	ans := p.send(t, "pas-claim", "", []byte(`{}`))
	want := "this gateway cannot take pas-claim: it is a gateway with no role, and pas-claim is answered by payer"
	var e struct{ Error string }
	if err := json.Unmarshal(ans.body, &e); err != nil || ans.status != http.StatusUnprocessableEntity || e.Error != want {
		t.Fatalf("answer %d %s, want 422 %q", ans.status, ans.body, want)
	}
	wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, ExchangeOther, RefusalRouting)
}

// The party a refusal of a leg's recipient is recorded as is decided by the
// leg's row: its recipient roles' gateway, "other" for a role with no party
// of its own or a leg this gateway does not know.
func TestRecipientParty_ByTheLegsRow(t *testing.T) {
	for leg, want := range map[string]string{
		"pas-claim":       RefusedByPayerGateway,
		legCDexRequest:    RefusedByProviderGateway,
		"federated-query": ExchangeOther,
		"patient-dtr":     ExchangeOther,
		"no-such-leg":     ExchangeOther,
		"":                ExchangeOther,
	} {
		if got := recipientParty(leg); got != want {
			t.Errorf("recipientParty(%q) = %q, want %q", leg, got, want)
		}
	}
	for name, c := range map[string]struct {
		roles []string
		want  string
	}{
		"one party twice":     {[]string{"payer", "payer"}, RefusedByPayerGateway},
		"two parties":         {[]string{"payer", "provider"}, ExchangeOther},
		"a party and unnamed": {[]string{"provider", "facility"}, ExchangeOther},
		"none":                {nil, ExchangeOther},
	} {
		if got := rolesParty(c.roles); got != c.want {
			t.Errorf("%s: rolesParty(%v) = %q, want %q", name, c.roles, got, c.want)
		}
	}
	for role, want := range map[string]string{"payer": RefusedByPayerGateway, "provider": RefusedByProviderGateway, "facility": ExchangeOther, "phg": ExchangeOther, "": ExchangeOther} {
		if got := gatewayParty(role); got != want {
			t.Errorf("gatewayParty(%q) = %q, want %q", role, got, want)
		}
	}
}
