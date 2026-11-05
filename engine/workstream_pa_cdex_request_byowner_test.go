//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexRequestPlainPost is the payer system's POST of body to the payer
// gateway's plain Task endpoint.
func cdexRequestPlainPost(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/Task", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	return req
}

// ownerRoutingExchange is the payer's in-process exchange with three
// registered providers that take the attachment request: owner-a attested for
// baseProviderNPI (the IG example's organization), owner-b for
// otherProviderNPI, and owner-c for nothing. A fourth, owner-d, is attested for
// sharedNPI, as owner-b also is. The payer itself carries unattestedNPI, which no provider's entry does.
func ownerRoutingExchange(t *testing.T, level ConformanceEnforcement) (*inProcessExchange, *exchangeRecords) {
	t.Helper()
	env, got := payerIngressExchange(t, level)
	reg := env.originator.cfg.Reg
	npi := func(vs ...string) []shnsdk.ProviderIdentifier {
		var out []shnsdk.ProviderIdentifier
		for _, v := range vs {
			out = append(out, shnsdk.ProviderIdentifier{System: providerIDSystem, Value: v})
		}
		return out
	}
	provider := func(id string, legs []string, ids []shnsdk.ProviderIdentifier) {
		reg.Set(id, shnsdk.RegistryEntry{ID: id, Role: "provider", EncPub: env.substrate.recipientEncPub, Legs: legs,
			MessageFrames: shnsdk.SupportedMessageFrames(), ProviderIDs: ids})
	}
	takes := []string{legCDexRequest}
	provider("owner-a", takes, npi(baseProviderNPI))
	provider("owner-b", takes, npi(otherProviderNPI, sharedNPI))
	provider("owner-c", takes, nil)
	provider("owner-d", takes, npi(sharedNPI))
	payer, _ := reg.Lookup(env.payerID)
	payer.ProviderIDs = npi(unattestedNPI)
	reg.Set(env.payerID, payer)
	return env, got
}

// Synthetic NPIs that pass the check digit: one two providers are attested
// for.
const (
	sharedNPI = "9000000015"
	// nobodysNPI is attested for no holder at all.
	nobodysNPI = "9000000031"
)

// ownerOnTheRole sets the IG example's contained PractitionerRole's
// organization NPI.
func ownerOnTheRole(value string) func(map[string]any) {
	return func(m map[string]any) {
		for _, c := range m["contained"].([]any) {
			if r := c.(map[string]any); r["id"] == "practitionerrole" {
				r["organization"].(map[string]any)["identifier"].(map[string]any)["value"] = value
			}
		}
	}
}

// ownerIdentifier sets an identifier on the owner reference itself, beside
// the reference to the contained PractitionerRole.
func ownerIdentifier(system, value string) func(map[string]any) {
	return func(m map[string]any) {
		m["owner"].(map[string]any)["identifier"] = map[string]any{"system": system, "value": value}
	}
}

// A Task at the plain Task endpoint whose owner names no one provider the
// network can route it to is refused on routing, at every level, by the
// payer's gateway, before anything crosses the network: an owner that cannot
// be read (its role's organization naming no contained resource among them);
// one with no organization NPI a provider is attested for (none at
// all, one attested for no one, a practitioner's only, another system's only,
// or one only a payer's holder carries); and one naming organizations of
// different providers, on two identifiers or on one identifier two providers
// are attested for. The one provider it names not taking the leg is the Hub's
// refusal, on the network's audit chain (the end-to-end row).
func TestCDexRequestByOwner_RefusesWhatItCannotRoute(t *testing.T) {
	for name, row := range map[string]struct {
		edit func(map[string]any)
		msg  string
	}{
		"an owner that cannot be read": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": []any{map[string]any{"system": providerIDSystem, "value": baseProviderNPI}}}
		}, errCDexRequestOwnerUnreadable},
		"an owner naming no contained resource":              {func(m map[string]any) { m["owner"] = map[string]any{"reference": "#nobody"} }, errCDexRequestOwnerUnreadable},
		"a role's organization naming no contained resource": {func(m map[string]any) { roleOrganizationRef(m, "#nobody") }, errCDexRequestOwnerUnreadable},
		"no owner":                    {func(m map[string]any) { delete(m, "owner") }, errCDexRequestOwnerNoProvider},
		"an NPI attested for no one":  {ownerOnTheRole(nobodysNPI), errCDexRequestOwnerNoProvider},
		"an NPI only a payer carries": {ownerOnTheRole(unattestedNPI), errCDexRequestOwnerNoProvider},
		"a literal reference alone":   {func(m map[string]any) { m["owner"] = map[string]any{"reference": "Organization/o"} }, errCDexRequestOwnerNoProvider},
		"a practitioner's NPI only": {func(m map[string]any) {
			for _, c := range m["contained"].([]any) {
				if r := c.(map[string]any); r["id"] == "practitionerrole" {
					delete(r, "organization")
					r["practitioner"].(map[string]any)["identifier"].(map[string]any)["value"] = baseProviderNPI
				}
			}
		}, errCDexRequestOwnerNoProvider},
		"another system's identifier only": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": "urn:oid:2.16.840.1.113883.4.4", "value": baseProviderNPI}}
		}, errCDexRequestOwnerNoProvider},
		"organizations of two providers": {func(m map[string]any) {
			ownerIdentifier(providerIDSystem, otherProviderNPI)(m)
		}, errCDexRequestOwnerTwoProviders},
		"an NPI two providers are attested for": {ownerOnTheRole(sharedNPI), errCDexRequestOwnerTwoProviders},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, got := ownerRoutingExchange(t, level)
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCDexRequest)(rec, cdexRequestPlainPost(cdexRequestMutate(t, row.edit)))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, row.msg)
				x := got.only(t)
				wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalRouting)
				wantReasonInAnswer(t, x, rec.Body.String())
				if x.RefusalReason != row.msg {
					t.Fatalf("reason %q, want %q", x.RefusalReason, row.msg)
				}
				if x.Exchange != legCDexRequest || x.Route != RouteCDexRequest {
					t.Fatalf("record %s on %s, want %s on %s", x.Exchange, x.Route, legCDexRequest, RouteCDexRequest)
				}
			})
		}
	}
}

// A Task at the plain Task endpoint whose owner names one provider is routed
// to it and carried as sent, at every level, by each shape the owner can name
// its organization in: the IG example's PractitionerRole, the owner reference
// itself, and a contained Organization, the owner's own or one the role's
// organization names. An identifier that matches no one is
// passed over when another matches: an unattested NPI beside a provider's, and
// another system's identifier beside it, each route to that provider. The
// IG's own questionnaire-request Tasks route as a request by code does.
func TestCDexRequestByOwner_RoutesToTheOneProviderItsOwnerNames(t *testing.T) {
	rows := map[string]struct {
		edit      func(map[string]any)
		recipient string
	}{
		"the IG example's PractitionerRole": {func(map[string]any) {}, "owner-a"},
		"the owner reference itself": {func(m map[string]any) {
			m["owner"] = map[string]any{"identifier": map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}
		}, "owner-b"},
		"a contained Organization": {func(m map[string]any) {
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "org",
				"identifier": []any{map[string]any{"system": "urn:oid:2.16.840.1.113883.4.4", "value": "123456789"}, map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}})
			m["owner"] = map[string]any{"reference": "#org"}
		}, "owner-b"},
		"an unattested NPI beside a provider's": {ownerIdentifier(providerIDSystem, nobodysNPI), "owner-a"},
		"the same provider twice":               {ownerIdentifier(providerIDSystem, baseProviderNPI), "owner-a"},
		"an Organization the role's organization names": {func(m map[string]any) {
			roleOrganizationRef(m, "#org2")
			m["contained"] = append(m["contained"].([]any), map[string]any{"resourceType": "Organization", "id": "org2",
				"identifier": []any{map[string]any{"system": providerIDSystem, "value": otherProviderNPI}}})
		}, "owner-b"},
		"a practitioner's NPI of another provider beside its organization": {func(m map[string]any) {
			for _, c := range m["contained"].([]any) {
				if r := c.(map[string]any); r["id"] == "practitionerrole" {
					r["practitioner"].(map[string]any)["identifier"].(map[string]any)["value"] = otherProviderNPI
				}
			}
		}, "owner-a"},
	}
	// The IG's questionnaire-request Tasks (D128), as published: each names the
	// IG example's organization, so goes to owner-a.
	fixtureOf := map[string]string{}
	for _, f := range cdexQuestionnaireTasks {
		rows["the IG's "+f] = struct {
			edit      func(map[string]any)
			recipient string
		}{nil, "owner-a"}
		fixtureOf["the IG's "+f] = f
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, got := ownerRoutingExchange(t, level)
				var events []ObserverEvent
				var mu sync.Mutex
				env.originator.cfg.Observer = func(e ObserverEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
				answer := []byte(`{"resourceType":"Task","id":"created"}`)
				env.payerReturns(LegResult{Response: relayedResponse(answer), AnswerStatus: http.StatusCreated})
				var body []byte
				if f, ok := fixtureOf[name]; ok {
					body = cdexFixture(t, f)
				} else {
					body = cdexRequestMutate(t, row.edit)
				}
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(RouteCDexRequest)(rec, cdexRequestPlainPost(body))
				x := got.only(t)
				if rec.Code != http.StatusCreated || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want the provider's 201 relayed exactly", rec.Code, rec.Body)
				}
				if x.Outcome != ExchangeAnswered || x.Route != RouteCDexRequest || x.Recipient != row.recipient || x.Exchange != legCDexRequest {
					t.Fatalf("record %s on %s to %s (%s), want %s answered on %s to %s", x.Outcome, x.Route, x.Recipient, x.Exchange,
						legCDexRequest, RouteCDexRequest, row.recipient)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, body) {
					t.Fatalf("the Task crossed the network as %s, want it as sent", sent)
				}
				// The owner names the recipient here, so no difference is recorded.
				mu.Lock()
				defer mu.Unlock()
				for _, e := range events {
					if e.Kind == CDexOwnerDiffersEvent {
						t.Fatalf("%s raised at the plain Task endpoint: %+v", CDexOwnerDiffersEvent, e)
					}
				}
			})
		}
	}
}
