//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Organization NPIs for the owner rows, synthetic: the IG example's own
// organization NPI, which the base's provider is attested for, another
// provider's, and one the network attests for no provider.
const (
	baseProviderNPI  = "1234567893"
	otherProviderNPI = "9999999995"
	unattestedNPI    = "9000000007"
)

// roleOrganizationRef sets the IG example's contained PractitionerRole's
// organization to a reference alone.
func roleOrganizationRef(m map[string]any, ref string) {
	for _, c := range m["contained"].([]any) {
		if r := c.(map[string]any); r["id"] == "practitionerrole" {
			r["organization"] = map[string]any{"reference": ref}
		}
	}
}

// A Task created at a provider's base goes to that provider, carried as sent,
// whatever its owner names, at every level. An owner naming an organization
// the network attests for another provider, on the owner reference, the
// contained PractitionerRole or a contained Organization (the owner's own, or
// the one that role's organization names), or an owner that cannot be read,
// is recorded (cdex.owner-differs), naming nothing beyond the leg. An owner
// naming the base's own provider, an organization the network attests for no
// provider, a practitioner (never routed on, nor an unreadable one read), an
// identifier in another system (even one a provider's entry carries), an
// identifier only a payer's holder carries, a literal reference alone, or no
// owner at all, states no difference the network can see.
func TestCDexRequestIngress_AnOwnerOfAnotherProviderIsCarriedAndRecorded(t *testing.T) {
	org := func(value string) func(map[string]any) {
		return func(m map[string]any) {
			for _, c := range m["contained"].([]any) {
				if r := c.(map[string]any); r["id"] == "practitionerrole" {
					r["organization"].(map[string]any)["identifier"].(map[string]any)["value"] = value
				}
			}
		}
	}
	for name, c := range map[string]struct {
		edit    func(map[string]any)
		differs bool
	}{
		"the base's own organization (the IG example)": {func(map[string]any) {}, false},
		"another provider's organization on the role":  {org(otherProviderNPI), true},
		"another provider's organization on the reference": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}
		}, true},
		"another provider's contained Organization": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "org",
				"identifier": []any{map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}})
			m["owner"] = map[string]any{"reference": "#org"}
		}, true},
		"the base's own and another provider's": {func(m map[string]any) {
			m["owner"].(map[string]any)["identifier"] = map[string]any{"system": providerIDSystem, "value": otherProviderNPI}
		}, true},
		"an organization attested for no provider": {org(unattestedNPI), false},
		"another provider's NPI as the practitioner's": {func(m map[string]any) {
			for _, c := range m["contained"].([]any) {
				if r := c.(map[string]any); r["id"] == "practitionerrole" {
					r["practitioner"].(map[string]any)["identifier"].(map[string]any)["value"] = otherProviderNPI
				}
			}
		}, false},
		"another provider's value in another system": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": "urn:oid:2.16.840.1.113883.4.4", "value": otherProviderNPI}}
		}, false},
		"an identifier only a payer's holder carries": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": providerIDSystem, "value": unattestedNPI}}
		}, false},
		"a literal reference alone": {func(m map[string]any) { m["owner"] = map[string]any{"reference": "Organization/o"} }, false},
		"an owner that cannot be read": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": []any{map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}}
		}, true},
		"an owner naming no contained resource": {func(m map[string]any) { m["owner"] = map[string]any{"reference": "#nobody"} }, true},
		"an owner that is empty":                {func(m map[string]any) { m["owner"] = map[string]any{} }, true},
		"a contained Organization with an unreadable identifier": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "org",
				"identifier": []any{map[string]any{"system": providerIDSystem, "value": baseProviderNPI}, map[string]any{"system": providerIDSystem}}})
			m["owner"] = map[string]any{"reference": "#org"}
		}, true},
		"another provider's Organization the role's organization names": {func(m map[string]any) {
			roleOrganizationRef(m, "#org2")
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "org2",
				"identifier": []any{map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}})
		}, true},
		"a role's organization naming no contained resource": {func(m map[string]any) { roleOrganizationRef(m, "#nobody") }, true},
		"no owner": {func(m map[string]any) { delete(m, "owner") }, false},
		// The practitioner is never routed on, so one that cannot be read
		// leaves the owner's organization to decide.
		"an unreadable practitioner beside the base's own organization": {func(m map[string]any) {
			for _, c := range m["contained"].([]any) {
				if r := c.(map[string]any); r["id"] == "practitionerrole" {
					r["practitioner"] = map[string]any{"identifier": []any{}}
				}
			}
		}, false},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, got := payerIngressExchange(t, level)
				var events []ObserverEvent
				var mu sync.Mutex
				env.originator.cfg.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
				reg := env.originator.cfg.Reg
				reg.Set(cdexRequestRecipientID, shnsdk.RegistryEntry{ID: cdexRequestRecipientID, Role: "provider",
					EncPub: env.substrate.recipientEncPub, Legs: []string{legCDexRequest}, MessageFrames: shnsdk.SupportedMessageFrames(),
					ProviderIDs: []shnsdk.ProviderIdentifier{{System: providerIDSystem, Value: baseProviderNPI}}})
				// The other provider also carries an identifier in another system,
				// as no registrar attests: it is never matched.
				reg.Set("other-provider", shnsdk.RegistryEntry{ID: "other-provider", Role: "provider", Legs: []string{legCDexRequest},
					ProviderIDs: []shnsdk.ProviderIdentifier{{System: providerIDSystem, Value: otherProviderNPI}, {System: "urn:oid:2.16.840.1.113883.4.4", Value: otherProviderNPI}}})
				payer, _ := reg.Lookup(env.payerID)
				payer.ProviderIDs = []shnsdk.ProviderIdentifier{{System: providerIDSystem, Value: unattestedNPI}}
				reg.Set(env.payerID, payer)
				env.payerReturns(LegResult{Response: relayedResponse([]byte(`{"resourceType":"Task"}`)), AnswerStatus: http.StatusCreated})

				body := cdexRequestMutate(t, c.edit)
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(body, cdexRequestRecipientID))
				x := got.only(t)
				if rec.Code != http.StatusCreated || x.Outcome != ExchangeAnswered || x.Recipient != cdexRequestRecipientID {
					t.Fatalf("answer %d %s (record %s to %s), want the Task carried to the base's provider", rec.Code, rec.Body, x.Outcome, x.Recipient)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, body) {
					t.Fatalf("the Task crossed the network as %s, want it as sent", sent)
				}
				mu.Lock()
				defer mu.Unlock()
				raised := false
				for _, e := range events {
					if e.Kind == CDexOwnerDiffersEvent {
						raised = true
						wantOwnerEventOnly(t, e, x.CorrelationID)
					}
				}
				if raised != c.differs {
					t.Fatalf("%s raised %v, want %v", CDexOwnerDiffersEvent, raised, c.differs)
				}
			})
		}
	}
}

// wantOwnerEventOnly fails unless e is CDexOwnerDiffersEvent carrying the leg,
// its direction and the leg's correlation, and nothing else: no identifier,
// counterpart or payload. Every field is compared, so a field added to the
// event is caught too.
func wantOwnerEventOnly(t *testing.T, e ObserverEvent, corrID string) {
	t.Helper()
	if corrID == "" {
		t.Fatal("the record carries no correlation id")
	}
	e.Time = time.Time{}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(ObserverEvent{Kind: CDexOwnerDiffersEvent, Direction: "ingress", LegType: legCDexRequest, CorrelationID: corrID})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("event %s, want %s", got, want)
	}
}

// A Task refused before it is sealed raises no cdex.owner-differs, at every
// level, though its owner names another provider: the event records a Task
// the gateway carries, and this one is not.
func TestCDexRequestIngress_ATaskRefusedBeforeSealingRaisesNoOwnerEvent(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, got := payerIngressExchange(t, level)
			var events []ObserverEvent
			var mu sync.Mutex
			env.originator.cfg.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
			reg := env.originator.cfg.Reg
			reg.Set(cdexRequestRecipientID, shnsdk.RegistryEntry{ID: cdexRequestRecipientID, Role: "provider",
				EncPub: env.substrate.recipientEncPub, Legs: []string{legCDexRequest}, MessageFrames: shnsdk.SupportedMessageFrames()})
			reg.Set("other-provider", shnsdk.RegistryEntry{ID: "other-provider", Role: "provider", Legs: []string{legCDexRequest},
				ProviderIDs: []shnsdk.ProviderIdentifier{{System: providerIDSystem, Value: otherProviderNPI}}})
			body := cdexRequestMutate(t, func(m map[string]any) {
				delete(m, "for")
				m["owner"] = map[string]any{"identifier": map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}
			})
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(body, cdexRequestRecipientID))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("answer %d %s, want the Task naming no patient refused", rec.Code, rec.Body)
			}
			wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalAuthority)
			mu.Lock()
			defer mu.Unlock()
			for _, e := range events {
				if e.Kind == CDexOwnerDiffersEvent {
					t.Fatalf("%s raised for a refused Task: %+v", CDexOwnerDiffersEvent, e)
				}
			}
		})
	}
}
