//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS topic's subscribe leg at the payer's gateway: provider-x subscribes,
// through the Hub, at payer-x, whose system serves its Subscription endpoint at
// its PAS base. The rows hold the leg's PAS-specific rules; the subscribe leg's
// shared rules are the HRex Task topic's rows (handleSubscribeInbound).

// pasProviderNPI is the NPI the registry attests for provider-x.
const pasProviderNPI = "1234567893"

// pasIngressBase is the payer gateway's Da Vinci ingress, under which it
// writes each Subscription's notification address.
const pasIngressBase = "https://payer-gw.example/ingress"

// pasProviderEndpoint is the endpoint provider-x's system writes into its
// Subscription.
const pasProviderEndpoint = "https://ehr.example/api/pas/notification?ehr=a"

// pasBinding binds a provider-x Subscription at payer-x on the PAS topic.
var pasBinding = subscription.Binding{Subscriber: "provider-x", Notifier: "payer-x", Topic: pastopic.TokenValue}

// pasProviderKeys are provider-x's gateway's subscription keys: the
// subscriber's, which seal what only its gateway opens.
func pasProviderKeys(t testing.TB) subscription.Keys {
	t.Helper()
	k, err := subscription.DeriveKeys("provider-x", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pasSubscriptionJSON is provider-x's Subscription to the PAS topic, as its
// system writes it (org-identifier names its NPI, with no system, as the PAS
// specification's example does), then edit applied to the JSON text.
func pasSubscriptionJSON(edit func(s string) string) string {
	s := `{"resourceType":"Subscription","id":"sub-1",` +
		`"meta":{"profile":["http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"]},` +
		`"status":"requested","reason":"PAS decisions","criteria":"` + pastopic.Canonical + `",` +
		`"_criteria":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria","valueString":"org-identifier=` + pasProviderNPI + `"}]},` +
		`"channel":{"type":"rest-hook","endpoint":"` + pasProviderEndpoint + `","payload":"application/fhir+json",` +
		`"_payload":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content","valueCode":"id-only"}]},` +
		`"header":["Authorization: Bearer ehr-secret"]}}`
	if edit != nil {
		s = edit(s)
	}
	return s
}

// pasCreateFrame is provider-x's create of sub, framed as its gateway frames
// it: the channel sealed (E-18).
func pasCreateFrame(t testing.TB, sub string) subFrame {
	t.Helper()
	s, rf := subscription.ParseSubscription([]byte(sub), "application/fhir+json")
	if rf != nil {
		t.Fatalf("fixture: %+v", rf)
	}
	body, se, sh, err := subscription.SealOutbound(pasProviderKeys(t), pasBinding, s)
	if err != nil {
		t.Fatal(err)
	}
	return subFrame{request: "POST Subscription", endpoint: se, channel: sh, contentType: "application/fhir+json", body: body, leg: pastopic.SubscribeLeg}
}

// newPASSubPayer is payer-x's gateway, whose participant sets the
// subscription scope, with its Subscription endpoint at sys's /fhir base and
// its ingress base, and provider-x registered with its attested NPI. events
// collects the observer events it raises.
func newPASSubPayer(t *testing.T, level ConformanceEnforcement, sys *subStore, events *[]ObserverEvent, opts ...func(*Config)) *cdexRequestProvider {
	t.Helper()
	return newPASSubPayerAt(t, level, sys, events, nil, opts...)
}

// newPASSubPayerErr is New's answer for newPASSubPayer's configuration with
// mutate applied, into got.
func newPASSubPayerErr(t *testing.T, sys *subStore, mutate func(*Config), got *error) {
	t.Helper()
	newPASSubPayerAt(t, EnforcementNone, sys, nil, got, mutate)
}

func newPASSubPayerAt(t *testing.T, level ConformanceEnforcement, sys *subStore, events *[]ObserverEvent, newErr *error, opts ...func(*Config)) *cdexRequestProvider {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	hubPub, hubPriv := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	provEncPub, provEncPriv := genKeyPair(t)
	provSignPub, _ := genED25519(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	reg := shnsdk.NewRegistry()
	reg.Set("provider-x", shnsdk.RegistryEntry{ID: "provider-x", Role: "provider", EncPub: provEncPub, SignPub: provSignPub,
		MessageFrames: shnsdk.SupportedMessageFrames(), ProviderIDs: []shnsdk.ProviderIdentifier{{System: "http://hl7.org/fhir/sid/us-npi", Value: pasProviderNPI}}})
	sor := newCensusSoR()
	var mu sync.Mutex
	cfg := Config{
		Role:                     "payer",
		HolderID:                 "payer-x",
		Identity:                 shnsdk.Identity{HolderID: "payer-x", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:                 "http://stub.test",
		AuthzPub:                 authzPub,
		HubTransportPub:          hubPub,
		Reg:                      reg,
		Validator:                shnsdk.NewFakeValidator(),
		SoR:                      sor,
		Store:                    sor,
		Clock:                    clock,
		Client:                   &http.Client{Transport: &inboundAuthzStub{authzPriv: authzPriv, clock: clock, holder: "payer-x"}},
		PayerSubscriptionScope:   "system/Subscription.cruds",
		PayerSubscriptionBaseURL: sys.srv.URL + "/fhir",
		PayerSubscriptionClient:  sys.srv.Client(),
		IngressBaseURL:           pasIngressBase,
		Observer: func(e ObserverEvent) {
			if events != nil {
				mu.Lock()
				*events = append(*events, e)
				mu.Unlock()
			}
		},
	}
	// The payer's own Da Vinci endpoint, its content occupant, at the same
	// system: no row here reaches it.
	cfg.Responder = NewNativeResponder(sys.srv.Client(), sys.srv.URL+"/fhir", clock)
	EnableIngressForTest(&cfg)
	for _, o := range opts {
		o(&cfg)
	}
	if newErr != nil {
		_, *newErr = New(cfg)
		return nil
	}
	g := mustNew(t, cfg)
	g.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	return &cdexRequestProvider{
		g:       g,
		payer:   inboundTestRequester{ID: "provider-x", EncPub: provEncPub, EncPriv: provEncPriv},
		hubPriv: hubPriv, authzPriv: authzPriv, got: &got,
	}
}

// A payer's gateway takes the PAS topic's subscribe leg from its setting
// alone: set, it declares the leg; unset, it declares none and refuses the leg
// 422 on routing, before anything reaches its system, at every level.
func TestPASSubscribe_TakenFromTheSettingAlone(t *testing.T) {
	sys := newSubStore(t)
	p := newPASSubPayer(t, EnforcementNone, sys, nil)
	if legs := TakenLegs("payer", p.g.cfg.settingSet); !slices.Contains(legs, pastopic.SubscribeLeg) {
		t.Fatalf("the payer with the scope declares %v, want %s among them", legs, pastopic.SubscribeLeg)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newPASSubPayer(t, level, sys, nil, func(c *Config) {
				c.PayerSubscriptionScope, c.PayerSubscriptionClient, c.PayerSubscriptionBaseURL = "", nil, ""
			})
			if legs := TakenLegs("payer", p.g.cfg.settingSet); slices.Contains(legs, pastopic.SubscribeLeg) {
				t.Fatalf("the payer without the scope declares %v", legs)
			}
			a := p.subscribe(t, pasCreateFrame(t, pasSubscriptionJSON(nil)))
			if a.status != http.StatusUnprocessableEntity || !strings.Contains(string(a.body), refusalPASSubscribeNotTaken) {
				t.Fatalf("answer %d %s, want 422 %q", a.status, a.body, refusalPASSubscribeNotTaken)
			}
			if c := sys.calls(); len(c) != 0 {
				t.Fatalf("the payer's system was called: %+v", c)
			}
		})
	}
}

// A provider's create is carried to the payer's Subscription endpoint, at
// every level, with its channel re-keyed to the payer gateway's notification
// address for the provider (E-19), when its org-identifier names the
// provider's attested NPI, with or without its system; the forward names the
// provider in no header of the network's (the PAS topic registers none).
func TestPASSubscribe_CreateNamingTheAttestedNPIIsCarried(t *testing.T) {
	for _, level := range allLevels {
		for name, filter := range map[string]string{
			"bare value":   pasProviderNPI,
			"system|value": "http://hl7.org/fhir/sid/us-npi|" + pasProviderNPI,
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				sys := newSubStore(t)
				p := newPASSubPayer(t, level, sys, nil)
				sub := pasSubscriptionJSON(func(s string) string {
					return strings.Replace(s, "org-identifier="+pasProviderNPI, "org-identifier="+filter, 1)
				})
				a := p.subscribe(t, pasCreateFrame(t, sub))
				if a.status/100 != 2 || !a.framed {
					t.Fatalf("answer %d %s, want the payer system's 2xx", a.status, a.body)
				}
				calls := sys.calls()
				if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].requestURI != "/fhir/Subscription" {
					t.Fatalf("calls %+v, want one POST /fhir/Subscription", calls)
				}
				if v := calls[0].header.Values(RequesterHeader); len(v) != 0 {
					t.Errorf("the forward carried %s %q: the PAS topic registers no such header", RequesterHeader, v)
				}
				body := string(calls[0].body)
				if !strings.Contains(body, pasIngressBase+"/notify/pas/provider-x/") {
					t.Errorf("the forwarded channel names no notification address for provider-x under the ingress: %s", body)
				}
				if strings.Contains(body, pasProviderEndpoint) || strings.Contains(body, "ehr-secret") {
					t.Errorf("the provider's own endpoint or header reached the payer's system: %s", body)
				}
				if !strings.Contains(body, "org-identifier="+filter) {
					t.Errorf("the filter was not carried as sent: %s", body)
				}
			})
		}
	}
}

// A create whose org-identifier names an identifier the registry does not
// attest for the provider, with or without a system, is refused 403 on
// authority, at every level, and nothing reaches the payer's system.
func TestPASSubscribe_CreateNamingAnotherIdentifierIsRefused(t *testing.T) {
	for _, level := range allLevels {
		for name, filter := range map[string]string{
			"bare value":   "1111111111",
			"system|value": "http://hl7.org/fhir/sid/us-npi|1111111111",
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				sys := newSubStore(t)
				p := newPASSubPayer(t, level, sys, nil)
				sub := pasSubscriptionJSON(func(s string) string {
					return strings.Replace(s, "org-identifier="+pasProviderNPI, "org-identifier="+filter, 1)
				})
				a := p.subscribe(t, pasCreateFrame(t, sub))
				if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "attested") {
					t.Fatalf("answer %d %s, want 403 naming the provider's attested identifiers", a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
				if c := sys.calls(); len(c) != 0 {
					t.Fatalf("the payer's system was called: %+v", c)
				}
			})
		}
	}
}

// An update naming an identifier the network does not attest for the
// provider is refused 403 on authority too, at every level, before the
// gateway's own read of the stored Subscription: nothing at all reaches the
// payer's system, whether it holds the Subscription or not. The control: the
// same update naming the provider's attested NPI reads, then forwards.
func TestPASSubscribe_UpdateNamingAnotherIdentifierIsRefused(t *testing.T) {
	for _, level := range allLevels {
		for name, filter := range map[string]string{
			"bare value":   "1111111111",
			"system|value": "http://hl7.org/fhir/sid/us-npi|1111111111",
		} {
			for _, held := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/held=%v", level, name, held), func(t *testing.T) {
					sys := newSubStore(t)
					if held {
						sys.store["sub-1"] = []byte(pasSubscriptionJSON(nil))
					}
					p := newPASSubPayer(t, level, sys, nil)
					f := pasCreateFrame(t, pasSubscriptionJSON(func(s string) string {
						return strings.Replace(s, "org-identifier="+pasProviderNPI, "org-identifier="+filter, 1)
					}))
					f.request = "PUT Subscription/sub-1"
					a := p.subscribe(t, f)
					if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "attested") {
						t.Fatalf("answer %d %s, want 403 naming the provider's attested identifiers", a.status, a.body)
					}
					wantRefusal(t, p.got.only(t), http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
					if c := sys.calls(); len(c) != 0 {
						t.Fatalf("the payer's system was called: %+v", c)
					}
				})
			}
		}
	}
	sys := newSubStore(t)
	p := newPASSubPayer(t, EnforcementStrict, sys, nil)
	f := pasCreateFrame(t, pasSubscriptionJSON(nil))
	f.request = "PUT Subscription/sub-1"
	p.subscribe(t, f)
	if c := sys.calls(); len(c) != 2 || c[0].method != http.MethodGet || c[1].method != http.MethodPut {
		t.Fatalf("control: the payer's system got %+v, want the gateway's read, then the update", c)
	}
}

// A payer whose system does not serve Subscription still takes the leg: a
// provider's create is carried to it, and its own answer goes back as sent,
// framed, as a direct connection would give; the network adds no refusal.
func TestPASSubscribe_ASystemWithoutSubscriptionAnswersForItself(t *testing.T) {
	notFound := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-supported","diagnostics":"Subscription is not supported"}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			sys.answer = func(r *http.Request) (readAnswer, bool) {
				return readAnswer{status: http.StatusNotFound, header: fhirJSONHeader, body: notFound}, true
			}
			p := newPASSubPayer(t, level, sys, nil)
			a := p.subscribe(t, pasCreateFrame(t, pasSubscriptionJSON(nil)))
			if a.status != http.StatusNotFound || !a.framed || !bytes.Equal(a.body, notFound) {
				t.Fatalf("answer %d framed=%v %s, want the payer system's own 404, framed, as sent", a.status, a.framed, a.body)
			}
		})
	}
}

// At startup a payer that takes the leg reads its system's CapabilityStatement
// as a signal for its operator only: where it declares Subscription, nothing
// is raised; where it declares none, or cannot be read, the gateway raises
// pas.subscription-unsupported naming why. The leg is declared in every case.
func TestPASSubscribe_TheCapabilityStatementIsASignalOnly(t *testing.T) {
	const withSub = `{"resourceType":"CapabilityStatement","status":"active","kind":"instance","fhirVersion":"4.0.1","format":["json"],"rest":[{"mode":"server","resource":[{"type":"Claim"},{"type":"Subscription"}]}]}`
	const withoutSub = `{"resourceType":"CapabilityStatement","status":"active","kind":"instance","fhirVersion":"4.0.1","format":["json"],"rest":[{"mode":"server","resource":[{"type":"Claim"}]}]}`
	for name, c := range map[string]struct {
		status    int
		body      string
		supported bool
		reason    string
	}{
		"set and supported":              {http.StatusOK, withSub, true, ""},
		"set and unsupported":            {http.StatusOK, withoutSub, false, "declares no Subscription resource"},
		"set and unreadable":             {http.StatusInternalServerError, `{}`, false, "answered 500"},
		"set, not a CapabilityStatement": {http.StatusOK, `{"resourceType":"OperationOutcome"}`, false, "is not a CapabilityStatement"},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newSubStore(t)
			sys.answer = func(r *http.Request) (readAnswer, bool) {
				if r.URL.Path != "/fhir/metadata" {
					return readAnswer{}, false
				}
				return readAnswer{status: c.status, header: fhirJSONHeader, body: []byte(c.body)}, true
			}
			var events []ObserverEvent
			p := newPASSubPayer(t, EnforcementNone, sys, &events)
			supported, reason := p.g.CheckPASSubscriptionSupport(context.Background())
			if supported != c.supported || !strings.Contains(reason, c.reason) {
				t.Fatalf("supported=%v reason=%q, want %v and a reason naming %q", supported, reason, c.supported, c.reason)
			}
			var raised []string
			for _, e := range events {
				if e.Kind == PASSubscriptionUnsupportedEvent {
					raised = append(raised, e.Detail)
				}
			}
			if c.supported != (len(raised) == 0) {
				t.Fatalf("events %q, want one exactly when unsupported", raised)
			}
			if legs := TakenLegs("payer", p.g.cfg.settingSet); !slices.Contains(legs, pastopic.SubscribeLeg) {
				t.Fatalf("the payer declares %v: the signal changed the declaration", legs)
			}
		})
	}
	// A gateway that takes no such Subscription reads nothing.
	sys := newSubStore(t)
	p := newPASSubPayer(t, EnforcementNone, sys, nil, func(c *Config) {
		c.PayerSubscriptionScope, c.PayerSubscriptionClient, c.PayerSubscriptionBaseURL = "", nil, ""
	})
	if ok, _ := p.g.CheckPASSubscriptionSupport(context.Background()); !ok {
		t.Fatal("a gateway without the scope reported its system unsupported")
	}
	if c := sys.calls(); len(c) != 0 {
		t.Fatalf("a gateway without the scope read %+v", c)
	}
}

// The loop rule knows the PAS topic's notification addresses as it
// knows the HRex Task topic's: an endpoint that is a registered gateway's
// /notify/pas/ address is the network's own, whichever topic's Subscription
// names it, and a token no topic serves is not.
func TestSubscriptionTopics_TheLoopRuleNamesEveryServedTopic(t *testing.T) {
	for _, tok := range []string{"cdex-task", "pas"} {
		if !isSubscriptionTopic(tok) {
			t.Errorf("the loop rule does not know topic %q", tok)
		}
	}
	if isSubscriptionTopic("other") || isSubscriptionTopic("") {
		t.Error("the loop rule knows a topic no gateway serves")
	}
	registered := func(id string) bool { return id == "provider-x" }
	kid := strings.Repeat("A", subscription.KeyIDLen)
	for _, addr := range []string{pasIngressBase + "/notify/pas/provider-x/" + kid + "/sealed", pasIngressBase + "/notify/cdex-task/provider-x/" + kid + "/sealed"} {
		if !subscription.LooksLikeNotificationAddress(addr, registered, isSubscriptionTopic) {
			t.Errorf("%s is not read as the network's own notification address", addr)
		}
	}
}

// pasStoredFor is a Subscription as the payer's system stores it once the
// network carried it for subscriber with E-19, under p's keys.
func pasStoredFor(t testing.TB, p *cdexRequestProvider, subscriber, sub string) []byte {
	t.Helper()
	s, rf := subscription.ParseSubscription([]byte(sub), "application/fhir+json")
	if rf != nil {
		t.Fatalf("fixture: %+v", rf)
	}
	b := pasBinding
	b.Subscriber = subscriber
	body, se, sh, err := subscription.SealOutbound(pasProviderKeys(t), b, s)
	if err != nil {
		t.Fatal(err)
	}
	sealed, rf := subscription.ParseSubscription(body, "application/fhir+json")
	if rf != nil {
		t.Fatal(rf)
	}
	out, err := subscription.RekeyForNotifier(p.g.paState().pasSubscription.keys, subscription.Address{Ingress: pasIngressBase, Topic: pastopic.TokenValue, Subscriber: subscriber, SealedEndpoint: se}, sh, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A payer's gateway refuses to start, naming the setting, where it could not
// deliver the leg it would declare: the scope on another role, without the
// ingress or its base, without a base or a client for its Subscription
// endpoint, with a base that is not an absolute http or https URL with a host
// and no query or fragment, or with an identity of another holder.
func TestPASSubscribe_BootRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"another role":        {func(c *Config) { c.Role = "provider" }, "is a payer's"},
		"no ingress":          {func(c *Config) { c.IngressEnabled = false }, "requires the Da Vinci ingress"},
		"no ingress base":     {func(c *Config) { c.IngressBaseURL = "" }, "requires the Da Vinci ingress"},
		"no base":             {func(c *Config) { c.PayerSubscriptionBaseURL = "" }, "requires PayerSubscriptionBaseURL"},
		"no client":           {func(c *Config) { c.PayerSubscriptionClient = nil }, "requires PayerSubscriptionClient"},
		"a base with a query": {func(c *Config) { c.PayerSubscriptionBaseURL += "?x=1" }, "to be an absolute http or https URL"},
		"a relative base":     {func(c *Config) { c.PayerSubscriptionBaseURL = "/fhir" }, "to be an absolute http or https URL"},
		"another holder":      {func(c *Config) { c.Identity.HolderID = "payer-y" }, "the notification keys are derived under one holder"},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newSubStore(t)
			var got error
			newPASSubPayerErr(t, sys, func(cfg *Config) { c.mutate(cfg) }, &got)
			if got == nil || !strings.Contains(got.Error(), c.want) || !strings.Contains(got.Error(), "PAYER_DAVINCI_SUBSCRIPTION_SCOPE") {
				t.Fatalf("New: %v, want a refusal naming PAYER_DAVINCI_SUBSCRIPTION_SCOPE and %q", got, c.want)
			}
		})
	}
}

// The base the payer's gateway states (statedBase) has no ending slash,
// whatever the setting's spelling, and a base at a host's root is its own:
// the Subscription endpoint is reached below it as written.
func TestPASSubscribe_TheStatedBase(t *testing.T) {
	for name, c := range map[string]struct{ base, want, uri string }{
		"an ending slash": {"/fhir/", "/fhir", "/fhir/Subscription"},
		"two":             {"/fhir//", "/fhir", "/fhir/Subscription"},
		"a host's root":   {"", "", "/Subscription"},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newSubStore(t)
			p := newPASSubPayer(t, EnforcementNone, sys, nil, func(cfg *Config) { cfg.PayerSubscriptionBaseURL = sys.srv.URL + c.base })
			a := p.subscribe(t, pasCreateFrame(t, pasSubscriptionJSON(nil)))
			if a.status/100 != 2 {
				t.Fatalf("answer %d %s", a.status, a.body)
			}
			if got := a.headers[shnsdk.FrameHeaderStatedBase]; got != sys.srv.URL+c.want {
				t.Errorf("statedBase %q, want %q", got, sys.srv.URL+c.want)
			}
			if calls := sys.calls(); len(calls) != 1 || calls[0].requestURI != c.uri {
				t.Errorf("calls %+v, want one to %s", calls, c.uri)
			}
		})
	}
}

// The ownership test on the PAS topic, at every level: a read of the
// provider's own Subscription is delivered; a read, update, delete or $status
// of another provider's is the network's 404 (authority), and an update,
// delete or $status of it is never forwarded past the gateway's own read.
func TestPASSubscribe_AnotherProvidersSubscriptionIsTheNetworks404(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newPASSubPayer(t, level, sys, nil)
			own := pasStoredFor(t, p, "provider-x", pasSubscriptionJSON(nil))
			sys.store["sub-1"] = own
			if a := p.subscribe(t, subFrame{request: "GET Subscription/sub-1", leg: pastopic.SubscribeLeg}); a.status != http.StatusOK || !bytes.Equal(a.body, own) {
				t.Fatalf("the provider's own read: %d %s, want it delivered", a.status, a.body)
			}
			sys.store["sub-1"] = pasStoredFor(t, p, "provider-y", pasSubscriptionJSON(nil))
			for _, f := range []subFrame{
				{request: "GET Subscription/sub-1"},
				{request: "DELETE Subscription/sub-1"},
				{request: "GET Subscription/sub-1/$status"},
				func() subFrame {
					f := pasCreateFrame(t, pasSubscriptionJSON(nil))
					f.request = "PUT Subscription/sub-1"
					return f
				}(),
			} {
				f.leg = pastopic.SubscribeLeg
				p.got.mu.Lock()
				p.got.recs = nil
				p.got.mu.Unlock()
				before := len(sys.calls())
				a := p.subscribe(t, f)
				if a.status != http.StatusNotFound || !bytes.Equal(a.body, cdexSubscriptionNotFound) {
					t.Fatalf("%s of another provider's: %d %s, want the network's 404", f.request, a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusNotFound, RefusedByPayerGateway, RefusalAuthority)
				if calls := sys.calls()[before:]; len(calls) != 1 || calls[0].method != http.MethodGet {
					t.Fatalf("%s of another provider's reached the payer's system as %+v, want only the gateway's own read", f.request, calls)
				}
			}
		})
	}
}

// A provider's search on the PAS topic, at every level: the search is carried
// with its url re-keyed to the payer gateway's notification address for the
// provider (E-21), and an answer that holds the provider's own Subscription
// is delivered; one holding a Subscription that is not the provider's is
// refused 403 on authority.
func TestPASSubscribe_TheSearch(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newPASSubPayer(t, level, sys, nil)
			sealed := pasProviderKeys(t).SealEndpointForSearch(pasBinding, pasProviderEndpoint)
			q, err := shnsdk.ParseSubscriptionSearch("url=" + url.QueryEscape(pasProviderEndpoint) + "&status=requested,active")
			if err != nil {
				t.Fatal(err)
			}
			search := subFrame{request: q.Request.String(), endpoint: strings.Join(sealed, " "), leg: pastopic.SubscribeLeg}
			own := pasStoredFor(t, p, "provider-x", pasSubscriptionJSON(nil))
			sys.store["sub-1"] = own
			a := p.subscribe(t, search)
			if a.status != http.StatusOK || !bytes.Contains(a.body, []byte(`"sub-1"`)) {
				t.Fatalf("the provider's search: %d %s, want its own Subscription delivered", a.status, a.body)
			}
			calls := sys.calls()
			if len(calls) != 1 || !strings.Contains(calls[0].requestURI, url.QueryEscape(pasIngressBase+"/notify/pas/provider-x/")) {
				t.Fatalf("the search reached the payer's system as %+v, want its url re-keyed to the notification address", calls)
			}
			forged := bytes.Replace(own, []byte(`Authorization: Bearer `), []byte(`Authorization: Bearer x`), 1)
			if bytes.Equal(forged, own) {
				t.Fatal("fixture: the stored Subscription carries no credential to alter")
			}
			sys.store["sub-1"] = forged
			p.got.mu.Lock()
			p.got.recs = nil
			p.got.mu.Unlock()
			if a := p.subscribe(t, search); a.status != http.StatusForbidden || !strings.Contains(string(a.body), refusalPASSubscribeSearchNotOwned) {
				t.Fatalf("a search answer holding another's: %d %s, want 403 %q", a.status, a.body, refusalPASSubscribeSearchNotOwned)
			}
			wantRefusal(t, p.got.only(t), http.StatusForbidden, RefusedByPayerGateway, RefusalAuthority)
		})
	}
}

// The startup read's other answers: a system it cannot reach, a body past its
// limit, and a CapabilityStatement that declares Subscription only in a client
// rest each read as not shown, naming why, and the client's error is never
// named (it can carry an address).
func TestPASSubscribe_TheCapabilityStatementReadsOtherwise(t *testing.T) {
	huge := `{"resourceType":"CapabilityStatement","rest":[{"mode":"server","resource":[{"type":"Subscription"}]}],"x":"` + strings.Repeat("a", pasMetadataLimit) + `"}`
	clientOnly := `{"resourceType":"CapabilityStatement","rest":[{"mode":"client","resource":[{"type":"Subscription"}]}]}`
	for name, c := range map[string]struct {
		answer func(r *http.Request) (readAnswer, bool)
		closed bool
		want   string
	}{
		"unreachable": {closed: true, want: "the request failed"},
		"past its limit": {answer: func(*http.Request) (readAnswer, bool) {
			return readAnswer{status: 200, header: fhirJSONHeader, body: []byte(huge)}, true
		}, want: "cannot be read whole"},
		"a client rest only": {answer: func(*http.Request) (readAnswer, bool) {
			return readAnswer{status: 200, header: fhirJSONHeader, body: []byte(clientOnly)}, true
		}, want: "declares no Subscription resource"},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newSubStore(t)
			sys.answer = c.answer
			var events []ObserverEvent
			p := newPASSubPayer(t, EnforcementNone, sys, &events)
			if c.closed {
				sys.srv.Close()
			}
			supported, reason := p.g.CheckPASSubscriptionSupport(context.Background())
			if supported || !strings.Contains(reason, c.want) || strings.Contains(reason, "127.0.0.1") {
				t.Fatalf("supported=%v reason=%q, want unsupported naming %q and no address", supported, reason, c.want)
			}
			if len(events) != 1 || events[0].Kind != PASSubscriptionUnsupportedEvent || events[0].Detail != reason {
				t.Fatalf("events %+v, want one %s naming the reason", events, PASSubscriptionUnsupportedEvent)
			}
		})
	}
}

// The payer's backend settings reach the Subscription forward itself, as every
// call to its own system takes them: its fixed backend headers, and, where it
// turns correlation off, no CorrelationHeader; by default the leg's id is sent.
func TestPASSubscribe_TheForwardTakesTheBackendSettings(t *testing.T) {
	for name, c := range map[string]struct {
		opts        []NativeOption
		correlation bool
	}{
		"by default":          {nil, true},
		"headers, no id sent": {[]NativeOption{WithBackendHeaders(http.Header{"X-Backend": {"edge-key"}}), WithoutBackendCorrelation()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			sys := newSubStore(t)
			p := newPASSubPayer(t, EnforcementNone, sys, nil, func(cfg *Config) { cfg.PayerSubscriptionOptions = c.opts })
			if a := p.subscribe(t, pasCreateFrame(t, pasSubscriptionJSON(nil))); a.status/100 != 2 {
				t.Fatalf("answer %d %s", a.status, a.body)
			}
			calls := sys.calls()
			if len(calls) != 1 {
				t.Fatalf("calls %+v", calls)
			}
			h := calls[0].header
			if got := h.Get(CorrelationHeader) != ""; got != c.correlation {
				t.Errorf("the forward carried %s %q, want one %v", CorrelationHeader, h.Get(CorrelationHeader), c.correlation)
			}
			if wantHeader := len(c.opts) > 0; (h.Get("X-Backend") == "edge-key") != wantHeader {
				t.Errorf("the forward carried X-Backend %q, want it %v", h.Get("X-Backend"), wantHeader)
			}
		})
	}
}
