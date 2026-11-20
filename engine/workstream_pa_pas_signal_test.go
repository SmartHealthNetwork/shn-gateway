//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS topic's signal leg (pas-subscription-signal): payer-x's system posts
// each notification of provider-x's Subscription to the notification address
// payer-x's gateway wrote (the notifier's half), and provider-x's gateway
// delivers it to the endpoint provider-x's system wrote (the subscriber's
// half). The rows hold the leg's PAS-specific rules; the halves' shared rules
// are the HRex Task topic's rows.

// pasSignalPayerBase is the payer's PAS base its notifications' addresses are
// under, as its gateway states it.
const pasSignalPayerBase = "https://payer.example/pas"

// pasSignalProviderIngress is provider-x's gateway's Da Vinci ingress, under
// which its documentation return paths are (E-22).
const pasSignalProviderIngress = "https://provider-gw.example"

// pasEvent is an id-only event notification of the PAS topic naming
// ClaimResponse cr1, its addresses under base.
func pasEvent(base string) string {
	return `{"resourceType":"Bundle","type":"history","timestamp":"2026-11-17T10:00:00Z","entry":[` +
		`{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"` + base + `/Subscription/s1"}},` +
		`{"name":"topic","valueCanonical":"` + pastopic.Canonical + `"},` +
		`{"name":"status","valueCode":"active"},{"name":"type","valueCode":"event-notification"},` +
		`{"name":"events-since-subscription-start","valueString":"1"},` +
		`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},` +
		`{"name":"focus","valueReference":{"reference":"` + base + `/ClaimResponse/cr1"}}]}]},` +
		`"request":{"method":"GET","url":"` + base + `/Subscription/s1/$status"},"response":{"status":"200"}},` +
		`{"fullUrl":"` + base + `/ClaimResponse/cr1","request":{"method":"PUT","url":"ClaimResponse/cr1"},"response":{"status":"200"}}]}`
}

// pasHandshake is a handshake notification of the PAS topic, naming its
// Subscription relatively.
func pasHandshake() string {
	return `{"resourceType":"Bundle","type":"history","entry":[{"fullUrl":"urn:uuid:0b6a1f8e-2c3d-4e5f-8a9b-0c1d2e3f4a5b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},` +
		`{"name":"topic","valueCanonical":"` + pastopic.Canonical + `"},` +
		`{"name":"status","valueCode":"requested"},{"name":"type","valueCode":"handshake"}]}}]}`
}

// pasEdit is pasEvent(base) with old replaced by new, once.
func pasEdit(t *testing.T, base, old, new string) []byte {
	t.Helper()
	b := strings.Replace(pasEvent(base), old, new, 1)
	if b == pasEvent(base) {
		t.Fatalf("no %s in the event", old)
	}
	return []byte(b)
}

// pasNotifyRequest is a POST of body to the notification address payer-x's
// gateway p writes for provider-x on topic, with the credential p issues for
// that address when authed: a PAS address unless topic says otherwise.
func pasNotifyRequest(t *testing.T, p *cdexRequestProvider, topic string, body []byte, authed bool) *http.Request {
	t.Helper()
	b := pasBinding
	b.Topic = topic
	se := pasProviderKeys(t).SealEndpoint(b, pasProviderEndpoint)
	sh, err := pasProviderKeys(t).SealHeaders(b, se, nil)
	if err != nil {
		t.Fatal(err)
	}
	served := subscription.Address{Ingress: pasIngressBase, Topic: pastopic.TokenValue, Subscriber: "provider-x", SealedEndpoint: se}
	credentialFor := served
	credentialFor.Topic = topic
	r := httptest.NewRequest(http.MethodPost, strings.TrimPrefix(served.String(), pasIngressBase), bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/fhir+json")
	if authed {
		r.Header.Set(subscription.AuthorizationHeader, p.g.paState().pasSubscription.keys.Credential(credentialFor, sh))
		r.Header.Set(subscription.ChannelHeader, sh)
	}
	return r
}

// The payer's gateway's checks of its system's PAS notification, each a
// network rule at every conformance level: valid notification − one mutation
// → refused on its primitive, recorded as the payer gateway's refusal on the
// leg, nothing sent. The controls (an id-only event and a handshake) pass
// every check and go to the network.
func TestPASNotify_ThePayersGatewaysChecks(t *testing.T) {
	sys := newSubStore(t)
	base := sys.srv.URL + "/fhir"
	for _, level := range allLevels {
		for name, r := range map[string]struct {
			topic  string
			authed bool
			body   []byte
			status int
			rule   string
			reason string
		}{
			"control: an id-only event":       {pastopic.TokenValue, true, []byte(pasEvent(base)), 0, "", ""},
			"control: a handshake":            {pastopic.TokenValue, true, []byte(pasHandshake()), 0, "", ""},
			"no credential":                   {pastopic.TokenValue, false, []byte(pasEvent(base)), http.StatusUnauthorized, RefusalAuthentication, errNotifyCredential},
			"a credential for the CDex topic": {cdextopic.TokenValue, true, []byte(pasEvent(base)), http.StatusUnauthorized, RefusalAuthentication, errNotifyCredential},
			"not a notification":              {pastopic.TokenValue, true, []byte(`{"resourceType":"Bundle","type":"history"}`), http.StatusBadRequest, RefusalRouting, errPASNotifyNotANotification},
			"a CDex Task notification": {pastopic.TokenValue, true, pasEdit(t, base, pastopic.Canonical, "http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"),
				http.StatusBadRequest, RefusalRouting, errPASNotifyNotANotification},
			// Content rides the notify leg, where the receiver check (N6)
			// refuses a decision naming no applicationReceiverCode.
			"a full-resource notification naming no receiver": {pastopic.TokenValue, true, pasEdit(t, base, `{"fullUrl":"`+base+`/ClaimResponse/cr1",`, `{"fullUrl":"`+base+`/ClaimResponse/cr1","resource":{"resourceType":"ClaimResponse","id":"cr1"},`),
				http.StatusForbidden, RefusalAuthority, errPASNotifyNotSubscribers},
			"an additional-context": {pastopic.TokenValue, true, pasEdit(t, base, `{"name":"focus",`, `{"name":"additional-context","valueReference":{"reference":"Patient/p1"}},{"name":"focus",`),
				http.StatusForbidden, RefusalAuthority, errPASNotifyContext},
			"an extension": {pastopic.TokenValue, true, pasEdit(t, base, `"type":"history",`, `"type":"history","extension":[{"url":"http://example.org/x","valueString":"y"}],`),
				http.StatusForbidden, RefusalAuthority, errPASNotifyShape},
			"a Task as the focus": {pastopic.TokenValue, true, pasEdit(t, base, base+`/ClaimResponse/cr1"}}]}]}`, base+`/Task/t1"}}]}]}`),
				http.StatusForbidden, RefusalAuthority, errPASNotifyShape},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSubPayer(t, level, sys, nil)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, r.topic, r.body, r.authed))
				x := p.got.only(t)
				wantLeg := legPASSubscriptionSignal
				if r.reason == errPASNotifyNotSubscribers {
					wantLeg = legPASSubscriptionNotify
				}
				if x.Exchange != wantLeg {
					t.Fatalf("recorded on %q, want %s", x.Exchange, wantLeg)
				}
				if r.rule == "" {
					// Past every check, the notification is sent: with no
					// Hub here, its sending fails, never a refusal of the
					// notification.
					if x.Outcome == ExchangeRefused {
						t.Fatalf("control refused: %d %s, record %+v", rec.Code, rec.Body.String(), x)
					}
					return
				}
				if rec.Code != r.status || !strings.Contains(rec.Body.String(), strings.TrimSuffix(r.reason, ": ")) {
					t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), r.status, r.reason)
				}
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway || x.Rule != r.rule {
					t.Fatalf("record %s by %q on %q, want refused by the payer's gateway on %q", x.Outcome, x.RefusedBy, x.Rule, r.rule)
				}
			})
		}
	}
}

// The PAS notification route checks its credential before it reads a body,
// as the HRex Task topic's does: unauthenticated, a body over the limit is
// refused 401 on authentication; authenticated, 413 on routing. At every
// level.
func TestPASNotify_TheCredentialComesBeforeTheBody(t *testing.T) {
	over := bytes.Repeat([]byte(" "), int(shnsdk.MaxRequestBytes)+1)
	sys := newSubStore(t)
	for _, level := range allLevels {
		for name, row := range map[string]struct {
			authed bool
			status int
			rule   string
		}{
			"unauthenticated, over the limit": {false, http.StatusUnauthorized, RefusalAuthentication},
			"authenticated, over the limit":   {true, http.StatusRequestEntityTooLarge, RefusalRouting},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSubPayer(t, level, sys, nil)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, over, row.authed))
				if rec.Code != row.status {
					t.Fatalf("%d %s, want %d", rec.Code, rec.Body.String(), row.status)
				}
				if x := p.got.only(t); x.Outcome != ExchangeRefused || x.Rule != row.rule {
					t.Fatalf("record %s on %q, want refused on %q", x.Outcome, x.Rule, row.rule)
				}
			})
		}
	}
}

// The PAS notification route is mounted exactly where the payer's gateway
// takes a provider's Subscription to the PAS topic, and so writes the
// addresses: a payer that sets no subscription scope mounts none, and the
// HRex Task topic's route is a provider's alone.
func TestPASNotify_MountedWithTheSubscribeLeg(t *testing.T) {
	const pattern = "POST /notify/pas/{subscriber}/{kid}/{sealed}"
	sys := newSubStore(t)
	if got := newPASSubPayer(t, EnforcementNone, sys, nil).g.RoutePatternsForTest(); !slices.Contains(got, pattern) || slices.Contains(got, "POST /notify/cdex-task/{subscriber}/{kid}/{sealed}") {
		t.Fatalf("a payer taking the subscribe leg mounts %v", got)
	}
	without := newPASSubPayer(t, EnforcementNone, sys, nil, func(c *Config) { c.PayerSubscriptionScope = "" })
	if got := without.g.RoutePatternsForTest(); slices.Contains(got, pattern) {
		t.Fatalf("a payer without the subscription scope mounts %s", pattern)
	}
}

// pasSignalProvider is provider-x's gateway, which takes the PAS topic's
// notifications, receiving payer-x's from the Hub, at one conformance level,
// recording its exchanges, with its own system's notification endpoint served
// over https.
type pasSignalProvider struct {
	g         *Gateway
	payer     inboundTestRequester
	hubPriv   []byte
	authzPriv []byte
	got       *exchangeRecords
	endpoint  *httptest.Server
	mu        sync.Mutex
	received  []*http.Request
	bodies    [][]byte
	answer    func(w http.ResponseWriter)
}

func newPASSignalProvider(t *testing.T, level ConformanceEnforcement, opts ...func(*Config)) *pasSignalProvider {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	hubPub, hubPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	payEncPub, payEncPriv := genKeyPair(t)
	paySignPub, _ := genED25519(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	p := &pasSignalProvider{hubPriv: hubPriv, authzPriv: authzPriv, payer: inboundTestRequester{ID: "payer-x", EncPub: payEncPub, EncPriv: payEncPriv}}
	p.endpoint = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.received, p.bodies = append(p.received, r), append(p.bodies, b)
		answer := p.answer
		p.mu.Unlock()
		if answer != nil {
			answer(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(p.endpoint.Close)
	pool := x509.NewCertPool()
	pool.AddCert(p.endpoint.Certificate())
	addr := p.endpoint.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}
	reg := shnsdk.NewRegistry()
	reg.Set("payer-x", shnsdk.RegistryEntry{ID: "payer-x", Role: "payer", EncPub: payEncPub, SignPub: paySignPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	sor := newCensusSoR()
	cfg := Config{
		Role:                           "provider",
		HolderID:                       "provider-x",
		Identity:                       shnsdk.Identity{HolderID: "provider-x", SignPriv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), EncPub: provEncPub, EncPriv: provEncPriv},
		AuthzURL:                       "http://stub.test",
		AuthzPub:                       authzPub,
		HubTransportPub:                hubPub,
		Reg:                            reg,
		Validator:                      shnsdk.NewFakeValidator(),
		SoR:                            sor,
		Store:                          sor,
		Clock:                          clock,
		Client:                         &http.Client{Transport: &inboundAuthzStub{authzPriv: authzPriv, clock: clock, holder: "provider-x"}},
		ProviderPASSubscription:        true,
		SubscriptionNotificationClient: client,
		IngressBaseURL:                 pasSignalProviderIngress,
	}
	// The ingress its documentation return path is under (E-22).
	EnableIngressForTest(&cfg)
	for _, o := range opts {
		o(&cfg)
	}
	g := mustNew(t, cfg)
	g.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	p.g, p.got = g, &got
	return p
}

// frame is the request frame payer-x's gateway sends: the endpoint and
// headers provider-x's gateway sealed for b (pasBinding unless edited), and
// the payer's PAS base.
func (p *pasSignalProvider) frame(t *testing.T, b subscription.Binding, endpoint string, headers []string) map[string]string {
	t.Helper()
	k := p.g.paState().subscriberKeys
	se := k.SealEndpoint(b, endpoint)
	sh, err := k.SealHeaders(b, se, headers)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
		shnsdk.FrameHeaderStatedBase: pasSignalPayerBase, "Content-Type": "application/fhir+json"}
}

// signal delivers body on pas-subscription-signal as the Hub would, framed
// with headers, under a token naming no patient.
func (p *pasSignalProvider) signal(t *testing.T, headers map[string]string, body []byte) subAnswer {
	t.Helper()
	return p.deliverOn(t, legPASSubscriptionSignal, "", headers, body)
}

// deliverOn delivers body on leg as the Hub would, framed with headers,
// under a token naming subject (none when it is empty).
func (p *pasSignalProvider) deliverOn(t *testing.T, leg, subject string, headers map[string]string, body []byte) subAnswer {
	t.Helper()
	spec, _ := legSpecFor(leg)
	clock := p.g.cfg.Clock()
	const corr = "corr-pas-signal"
	payload, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, body)
	if err != nil {
		t.Fatal(err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.payer.ID, Recipient: p.g.cfg.HolderID, TransactionType: spec.WireName,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Frame: spec.ReqFrame, Holder: p.payer.ID, Subject: subject,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tokBytes, _ := json.Marshal(tok)
	env.Metadata.AuthzToken = string(tokBytes)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, _ := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r)
	p.g.drainObserveChecks()
	if rec.Code != http.StatusOK {
		return subAnswer{status: rec.Code, body: rec.Body.Bytes()}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.payer, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return subAnswer{status: hdr.Status, framed: true, headers: hdr.Headers, body: answer}
}

// The provider's gateway's checks of a PAS notification it is delivered, each
// a network rule at every conformance level: valid notification − one
// mutation → refused on its primitive, recorded as the provider gateway's
// refusal, nothing reaching its own endpoint. The controls are delivered
// byte for byte as the payer's system sent them (no gateway edits a PAS
// notification), with the provider's own header value, and the endpoint's
// answer carried.
func TestPASSignal_TheProvidersGatewaysChecks(t *testing.T) {
	const (
		leak = "Van Doe"
		pat  = `{"resourceType":"Patient","id":"p1","name":[{"family":"` + leak + `"}]}`
	)
	auth := []string{"Authorization: Bearer ehr-secret"}
	event := []byte(pasEvent(pasSignalPayerBase))
	for _, level := range allLevels {
		type row struct {
			frame  func(t *testing.T, p *pasSignalProvider) map[string]string
			body   []byte
			answer func(w http.ResponseWriter)
			status int
			rule   string
		}
		valid := func(t *testing.T, p *pasSignalProvider) map[string]string {
			return p.frame(t, pasBinding, pasProviderEndpoint, auth)
		}
		without := func(name string) func(t *testing.T, p *pasSignalProvider) map[string]string {
			return func(t *testing.T, p *pasSignalProvider) map[string]string {
				f := valid(t, p)
				delete(f, name)
				return f
			}
		}
		sealed := func(edit func(*subscription.Binding), endpoint string, headers []string) func(t *testing.T, p *pasSignalProvider) map[string]string {
			return func(t *testing.T, p *pasSignalProvider) map[string]string {
				b := pasBinding
				if edit != nil {
					edit(&b)
				}
				return p.frame(t, b, endpoint, headers)
			}
		}
		patient := func(status int) func(w http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(pat))
			}
		}
		for name, r := range map[string]row{
			"control: an id-only event":             {valid, event, nil, http.StatusOK, ""},
			"control: a handshake":                  {valid, []byte(pasHandshake()), nil, http.StatusOK, ""},
			"no sealed endpoint":                    {without(shnsdk.FrameHeaderSubscriptionEndpoint), event, nil, http.StatusForbidden, RefusalAuthority},
			"no sealed headers":                     {without(shnsdk.FrameHeaderSubscriptionChannel), event, nil, http.StatusForbidden, RefusalAuthority},
			"values sealed for another payer":       {sealed(func(b *subscription.Binding) { b.Notifier = "payer-y" }, pasProviderEndpoint, auth), event, nil, http.StatusForbidden, RefusalAuthority},
			"values sealed for the CDex topic":      {sealed(func(b *subscription.Binding) { b.Topic = cdextopic.TokenValue }, pasProviderEndpoint, auth), event, nil, http.StatusForbidden, RefusalAuthority},
			"values sealed for another provider":    {sealed(func(b *subscription.Binding) { b.Subscriber = "provider-y" }, pasProviderEndpoint, auth), event, nil, http.StatusForbidden, RefusalAuthority},
			"an endpoint that is not https":         {sealed(nil, "http://ehr.example/api/pas/notification", auth), event, nil, http.StatusUnprocessableEntity, RefusalIntegrity},
			"a header that is not an HTTP field":    {sealed(nil, pasProviderEndpoint, []string{"no colon here"}), event, nil, http.StatusUnprocessableEntity, RefusalFidelity},
			"a full-resource notification":          {valid, []byte(strings.Replace(string(event), `{"fullUrl":"`+pasSignalPayerBase+`/ClaimResponse/cr1",`, `{"fullUrl":"`+pasSignalPayerBase+`/ClaimResponse/cr1","resource":{"resourceType":"ClaimResponse","id":"cr1"},`, 1)), nil, http.StatusForbidden, RefusalAuthority},
			"a CDex Task notification":              {valid, []byte(strings.Replace(string(event), pastopic.Canonical, "http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task", 1)), nil, http.StatusForbidden, RefusalAuthority},
			"not a notification":                    {valid, []byte(`{"resourceType":"Bundle","type":"history"}`), nil, http.StatusForbidden, RefusalAuthority},
			"not FHIR":                              {valid, []byte(`notify`), nil, http.StatusForbidden, RefusalAuthority},
			"an additional-context":                 {valid, []byte(strings.Replace(string(event), `{"name":"focus",`, `{"name":"additional-context","valueReference":{"reference":"Patient/p1"}},{"name":"focus",`, 1)), nil, http.StatusForbidden, RefusalAuthority},
			"an extension":                          {valid, []byte(strings.Replace(string(event), `"type":"history",`, `"type":"history","extension":[{"url":"http://example.org/x","valueString":"y"}],`, 1)), nil, http.StatusForbidden, RefusalAuthority},
			"a Patient as a root Bundle member":     {valid, []byte(strings.Replace(string(event), `"type":"history",`, `"type":"history","x":`+pat+`,`, 1)), nil, http.StatusForbidden, RefusalAuthority},
			"an answer holding a Patient":           {valid, event, patient(http.StatusOK), http.StatusBadGateway, RefusalAuthority},
			"an error answer holding a Patient":     {valid, event, patient(http.StatusBadRequest), http.StatusBadGateway, RefusalAuthority},
			"an address on another server, as sent": {valid, []byte(strings.Replace(string(event), `"request":{"method":"PUT","url":"ClaimResponse/cr1"}`, `"request":{"method":"PUT","url":"https://elsewhere.example/ClaimResponse/cr1"}`, 1)), nil, http.StatusOK, ""},
			"an address escaping the base, as sent": {valid, []byte(strings.Replace(string(event), `{"fullUrl":"`+pasSignalPayerBase+`/ClaimResponse/cr1",`, `{"fullUrl":"`+pasSignalPayerBase+`/../ClaimResponse/cr1",`, 1)), nil, http.StatusOK, ""},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSignalProvider(t, level)
				p.answer = r.answer
				a := p.signal(t, r.frame(t, p), r.body)
				if a.status != r.status {
					t.Fatalf("answered %d %s, want %d", a.status, a.body, r.status)
				}
				x := p.got.only(t)
				if x.Exchange != legPASSubscriptionSignal {
					t.Fatalf("recorded on %q, want %s", x.Exchange, legPASSubscriptionSignal)
				}
				if r.rule == "" {
					if len(p.bodies) != 1 || !bytes.Equal(p.bodies[0], r.body) {
						t.Fatalf("delivered %q, want the payer's bytes as sent", p.bodies)
					}
					if got := p.received[0].Header.Get("Authorization"); got != "Bearer ehr-secret" {
						t.Fatalf("Authorization %q", got)
					}
					if got := p.received[0].URL.RequestURI(); got != "/api/pas/notification?ehr=a" {
						t.Fatalf("delivered at %q, want the path and query the provider wrote", got)
					}
					if x.Outcome == ExchangeRefused {
						t.Fatalf("control record %+v", x)
					}
					return
				}
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByProviderGateway || x.Rule != r.rule {
					t.Fatalf("record %s by %q on %q, want refused by the provider's gateway on %q", x.Outcome, x.RefusedBy, x.Rule, r.rule)
				}
				if strings.Contains(string(a.body), "ehr-secret") || strings.Contains(string(a.body), "ehr.example") {
					t.Fatalf("the refusal names the provider's own values: %s", a.body)
				}
				// No refusal carries the Patient a notification or an answer
				// held.
				if strings.Contains(string(a.body), leak) || strings.Contains(string(a.body), `"resourceType":"Patient"`) {
					t.Fatalf("the refusal carries the Patient: %s", a.body)
				}
				if r.answer == nil && len(p.bodies) != 0 {
					t.Fatalf("the endpoint got %d notifications", len(p.bodies))
				}
			})
		}
	}
}

// A provider takes the PAS topic's notifications only when its participant
// asks for them: unset, its gateway declares no leg (and so mounts no inbound
// route and needs no Hub transport key); set, it declares the signal and
// notify legs. A
// payer's gateway never takes it, and refuses to start with the setting.
func TestPASSignal_TakenFromTheSettingAlone(t *testing.T) {
	if got := (Config{Role: "provider", ProviderPASSubscription: true}).TakenLegs(); !slices.Equal(got, []string{legPASSubscriptionNotify, legPASSubscriptionSignal}) {
		t.Fatalf("a provider asking for them declares %v", got)
	}
	if got := (Config{Role: "provider"}).TakenLegs(); slices.Contains(got, legPASSubscriptionSignal) {
		t.Fatalf("a provider not asking declares %v", got)
	}
	if got := (Config{Role: "payer", ProviderPASSubscription: true}).TakenLegs(); slices.Contains(got, legPASSubscriptionSignal) {
		t.Fatalf("a payer declares %v", got)
	}
	var err error
	newPASSubPayerErr(t, newSubStore(t), func(c *Config) { c.ProviderPASSubscription = true }, &err)
	if err == nil || !strings.Contains(err.Error(), "ProviderPASSubscription (PROVIDER_PAS_SUBSCRIPTION) is a provider's") {
		t.Fatalf("New for a payer with the setting: %v", err)
	}
}

// A provider whose participant has not asked for the PAS topic's
// notifications refuses one that reaches it anyway (a Hub reading an older
// declaration) 422 on routing, recorded as its gateway's refusal, nothing
// delivered, at every level: never a failure of its own.
func TestPASSignal_AProviderThatDoesNotTakeItRefusesOnRouting(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newPASSignalProvider(t, level, func(c *Config) { c.ProviderPASSubscription = false })
			a := p.signal(t, p.frame(t, pasBinding, pasProviderEndpoint, []string{"Authorization: Bearer ehr-secret"}), []byte(pasEvent(pasSignalPayerBase)))
			if a.status != http.StatusUnprocessableEntity || !strings.Contains(string(a.body), refusalPASSignalNotTaken) {
				t.Fatalf("answered %d %s, want 422 naming %q", a.status, a.body, refusalPASSignalNotTaken)
			}
			x := p.got.only(t)
			if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByProviderGateway || x.Rule != RefusalRouting {
				t.Fatalf("record %s by %q on %q, want refused by the provider's gateway on routing", x.Outcome, x.RefusedBy, x.Rule)
			}
			if len(p.bodies) != 0 {
				t.Fatalf("the endpoint got %d notifications", len(p.bodies))
			}
		})
	}
}
