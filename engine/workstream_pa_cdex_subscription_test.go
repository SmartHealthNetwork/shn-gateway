//go:build !shn_coreonly

package engine

// workstream_pa_cdex_subscription_test.go — the provider gateway's side of
// the payer's topic Subscription to its CDex Tasks' changes
// (cdex-task-subscribe): each interaction
// carried to the provider's own Subscription endpoint, the channel re-keyed
// (E-19) and a search's url re-keyed (E-21), the ownership test on what is
// read back, the answer check, and every refusal by its primitive, each row
// at every conformance level. The payer's side is framed directly here: the
// subscriber's half comes with the release that serves it.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// subStore is a provider's own FHIR server answering Subscription
// interactions as HAPI FHIR does, over a store keyed by id: it records each
// request as it arrived, and answer, when set, answers a request in its place.
type subStore struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []readReceived
	store  map[string][]byte
	answer func(r *http.Request) (readAnswer, bool)
	// locations are Location values the store adds to the answer the
	// answer hook gives, each as its own field line.
	locations []string
}

func newSubStore(t *testing.T) *subStore {
	t.Helper()
	s := &subStore{store: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, readReceived{method: r.Method, requestURI: r.RequestURI, header: r.Header.Clone(), body: b})
		hook, locations := s.answer, s.locations
		s.mu.Unlock()
		a, ok := readAnswer{}, false
		if hook != nil {
			a, ok = hook(r)
		}
		if !ok {
			a = s.serve(r, b)
		}
		for k, v := range a.header {
			if v == "" {
				// An empty value declares no header at all, so the
				// server sniffs no media type for the answer.
				w.Header()[k] = nil
				continue
			}
			w.Header().Set(k, v)
		}
		if hook != nil {
			for _, l := range locations {
				w.Header().Add("Location", l)
			}
		}
		w.WriteHeader(a.status)
		_, _ = w.Write(a.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

var fhirJSONHeader = map[string]string{"Content-Type": "application/fhir+json; charset=UTF-8"}

const subNotFound = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"HAPI-2001: Resource Subscription/x is not known"}]}`

// serve is the store's own answer.
func (s *subStore) serve(r *http.Request, body []byte) readAnswer {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/fhir")
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	ok := func(status int, b []byte) readAnswer {
		return readAnswer{status: status, header: fhirJSONHeader, body: b}
	}
	switch {
	case r.Method == http.MethodPost && path == "/Subscription":
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		id, _ := m["id"].(string)
		s.store[id] = body
		return ok(http.StatusCreated, body)
	case r.Method == http.MethodPut && len(segs) == 2:
		s.store[segs[1]] = body
		return ok(http.StatusOK, body)
	case r.Method == http.MethodGet && path == "/Subscription":
		var entries []string
		wanted, _ := subscription.SearchURLValues(r.URL.Query().Get("url"))
		for _, id := range storeIDs(s.store) {
			if slices.Contains(wanted, endpointOf(s.store[id])) {
				entries = append(entries, `{"fullUrl":"`+s.srv.URL+`/fhir/Subscription/`+id+`","resource":`+string(s.store[id])+`,"search":{"mode":"match"}}`)
			}
		}
		return ok(http.StatusOK, []byte(`{"resourceType":"Bundle","type":"searchset","total":`+itoa(len(entries))+`,"entry":[`+strings.Join(entries, ",")+`]}`))
	case r.Method == http.MethodGet && path == "" && r.URL.Query().Has("_getpages"):
		var entries []string
		for _, id := range storeIDs(s.store) {
			entries = append(entries, `{"resource":`+string(s.store[id])+`,"search":{"mode":"match"}}`)
		}
		return ok(http.StatusOK, []byte(`{"resourceType":"Bundle","type":"searchset","entry":[`+strings.Join(entries, ",")+`]}`))
	case len(segs) >= 2 && segs[0] == "Subscription":
		b, held := s.store[segs[1]]
		if !held {
			return ok(http.StatusNotFound, []byte(subNotFound))
		}
		switch {
		case r.Method == http.MethodDelete:
			delete(s.store, segs[1])
			return ok(http.StatusOK, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Successfully deleted 1 resource(s)"}]}`))
		case len(segs) == 3 && segs[2] == "$status":
			return ok(http.StatusOK, []byte(`{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","parameter":[`+
				`{"name":"subscription","valueReference":{"reference":"Subscription/`+segs[1]+`"}},{"name":"status","valueCode":"active"},{"name":"type","valueCode":"query-status"}]}}]}`))
		}
		return ok(http.StatusOK, subset(b, r.URL.Query()))
	}
	return ok(http.StatusBadRequest, []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing"}]}`))
}

// subset is a read's answer with its query applied, as a FHIR server answers
// it: _summary=true keeps the summary elements, so channel.header, which is
// not one, goes; _elements keeps only the elements it names (and the
// resource's type, id and meta).
func subset(b []byte, q url.Values) []byte {
	if q.Get("_summary") != "true" && !q.Has("_elements") {
		return b
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if q.Get("_summary") == "true" {
		if ch, ok := m["channel"].(map[string]any); ok {
			delete(ch, "header")
		}
	}
	if q.Has("_elements") {
		keep := map[string]bool{"resourceType": true, "id": true, "meta": true}
		for _, e := range strings.Split(q.Get("_elements"), ",") {
			keep[e] = true
		}
		for k := range m {
			if !keep[k] {
				delete(m, k)
			}
		}
	}
	out, _ := json.Marshal(m)
	return out
}

func storeIDs(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func endpointOf(sub []byte) string {
	var m struct {
		Channel struct{ Endpoint string } `json:"channel"`
	}
	_ = json.Unmarshal(sub, &m)
	return m.Channel.Endpoint
}

func (s *subStore) calls() []readReceived {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// subTaskURL is the provider's own Task endpoint on s; its base serves the
// Subscription endpoint.
func (s *subStore) taskURL() string { return s.srv.URL + "/fhir/Task" }

// cdexSubscriptionScope is the subscription scope a provider sets in these
// rows.
const cdexSubscriptionScope = "system/Subscription.cruds"

// subProvider is a provider gateway whose participant sets its own Task
// endpoint and the subscription scope, with the ingress base its notification
// addresses are written under, and payer-x registered with its attested payer
// identity.
type subProvider struct {
	*cdexRequestProvider
	sys *subStore
}

func newSubProvider(t *testing.T, level ConformanceEnforcement, sys *subStore, opts ...func(*Config)) *subProvider {
	t.Helper()
	p := newCDexRequestProvider(t, level, append([]func(*Config){func(c *Config) {
		c.ProviderCDexTaskURL = sys.taskURL()
		c.ProviderCDexTaskClient = sys.srv.Client()
		c.ProviderCDexSubscriptionScope = cdexSubscriptionScope
		c.ProviderCDexSubscriptionClient = sys.srv.Client()
		c.IngressBaseURL = cdexIngressBase
		EnableIngressForTest(c)
		e, _ := c.Reg.Lookup("payer-x")
		e.PayerIDs = []shnsdk.PayerIdentifier{payerIdentity}
		c.Reg.Set("payer-x", e)
	}}, opts...)...)
	return &subProvider{cdexRequestProvider: p, sys: sys}
}

// keys are the provider gateway's own subscription keys.
func (p *subProvider) keys() subscription.Keys { return p.g.paState().cdexSubscription.keys }

// payerKeys are the payer gateway's subscription keys in these rows: the
// subscriber's, which seal what only its gateway opens.
func payerKeys(t testing.TB) subscription.Keys {
	t.Helper()
	k, err := subscription.DeriveKeys("payer-x", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var payerBinding = subscription.Binding{Subscriber: "payer-x", Notifier: "provider-x", Topic: cdextopic.TokenValue}

const payerEndpoint = "https://payer-sys.example/api/cdex/notify?tenant=a"

// cdexSubscriptionJSON is the payer's Subscription to the HRex Task topic, as
// its system writes it, then edit applied to the JSON text.
func cdexSubscriptionJSON(edit func(s string) string) string {
	s := `{"resourceType":"Subscription","id":"sub-1",` +
		`"meta":{"profile":["http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"]},` +
		`"status":"requested","reason":"Task changes","criteria":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task",` +
		`"_criteria":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria","valueString":"Task?requester:identifier=http://example.org/cdex/payer/payer-ids|Payer123&status=completed"}]},` +
		`"channel":{"type":"rest-hook","endpoint":"` + payerEndpoint + `","payload":"application/fhir+json",` +
		`"_payload":{"extension":[{"url":"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content","valueCode":"id-only"}]},` +
		`"header":["Authorization: Bearer payer-secret"]}}`
	if edit != nil {
		s = edit(s)
	}
	return s
}

// sealedForPayer is what the payer's gateway sends for sub (E-18): the body
// with its channel.endpoint and channel.header removed, and the sealed
// endpoint and headers.
func sealedForPayer(t testing.TB, sub string) (body []byte, sealedEndpoint, sealedHeaders string) {
	t.Helper()
	s, rf := subscription.ParseSubscription([]byte(sub), "application/fhir+json")
	if rf != nil {
		t.Fatalf("fixture: %+v", rf)
	}
	body, se, sh, err := subscription.SealOutbound(payerKeys(t), payerBinding, s)
	if err != nil {
		t.Fatal(err)
	}
	return body, se, sh
}

// storedFor is a Subscription as the provider's system stores it once the
// network carried it for subscriber with E-19, under p's keys.
func (p *subProvider) storedFor(t testing.TB, subscriber, sub string) []byte {
	t.Helper()
	body, se, sh := sealedForPayer(t, sub)
	s, rf := subscription.ParseSubscription(body, "application/fhir+json")
	if rf != nil {
		t.Fatal(rf)
	}
	out, err := subscription.RekeyForNotifier(p.keys(), subscription.Address{Ingress: cdexIngressBase, Topic: cdextopic.TokenValue, Subscriber: subscriber, SealedEndpoint: se}, sh, s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// subFrame is what a payer's gateway frames on the subscribe leg.
type subFrame struct {
	request, endpoint, channel string
	contentType, ifMatch       string
	prefer                     string
	body                       []byte
	raw                        []byte // the frame exactly, in place of one built from the rest

	leg string // the subscribe leg unless set
}

// subAnswer is what the payer's gateway receives.
type subAnswer struct {
	status  int
	framed  bool
	headers map[string]string
	body    []byte
	subject string
}

// subscribe delivers f as the Hub would, under a token naming no patient: the
// subscribe leg names none on either side.
func (p *cdexRequestProvider) subscribe(t *testing.T, f subFrame) subAnswer {
	t.Helper()
	leg := f.leg
	if leg == "" {
		leg = legCDexTaskSubscribe
	}
	spec, ok := legSpecFor(leg)
	if !ok {
		t.Fatalf("no catalog entry for %s", leg)
	}
	clock := p.g.cfg.Clock()
	corr := "corr-" + leg
	headers := map[string]string{}
	for k, v := range map[string]string{shnsdk.FrameHeaderSubscriptionRequest: f.request, shnsdk.FrameHeaderSubscriptionEndpoint: f.endpoint,
		shnsdk.FrameHeaderSubscriptionChannel: f.channel, "Content-Type": f.contentType, shnsdk.FrameHeaderIfMatch: f.ifMatch,
		shnsdk.FrameHeaderPrefer: f.prefer} {
		if v != "" {
			headers[k] = v
		}
	}
	payload := f.raw
	if payload == nil {
		var err error
		if payload, err = shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, f.body); err != nil {
			t.Fatal(err)
		}
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.payer.ID, Recipient: p.g.cfg.HolderID, TransactionType: spec.WireName,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	subject := ""
	if leg != legCDexTaskSubscribe && leg != legCDexTaskStatus && leg != pastopic.SubscribeLeg {
		subject = "pci:payer-task-subject"
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Subject: subject, Frame: spec.ReqFrame, Holder: p.payer.ID,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tokBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tokBytes)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.inboundRoute()(rec, r)
	p.g.drainObserveChecks()
	if rec.Code != http.StatusOK {
		return subAnswer{status: rec.Code, body: rec.Body.Bytes()}
	}
	respEnv, err := shnsdk.DecodeEnvelope(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	var respTok shnsdk.Token
	if err := json.Unmarshal([]byte(respEnv.Metadata.AuthzToken), &respTok); err != nil {
		t.Fatalf("response token: %v", err)
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.payer, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return subAnswer{status: hdr.Status, framed: true, headers: hdr.Headers, body: answer, subject: respTok.Subject}
}

// create is the payer's create of sub, framed as its gateway frames it.
func createFrame(t testing.TB, sub string) subFrame {
	t.Helper()
	body, se, sh := sealedForPayer(t, sub)
	return subFrame{request: "POST Subscription", endpoint: se, channel: sh, contentType: "application/fhir+json", body: body}
}

// sealedCreateFrame is the payer's create as the subscriber's gateway frames
// it, E-18 sealing the whole channel: the body with channel.endpoint and
// channel.header removed, both travelling only sealed (createFrame). It fails
// unless the body states neither.
func sealedCreateFrame(t testing.TB, sub, endpoint string) subFrame {
	t.Helper()
	f := createFrame(t, sub)
	if bytes.Contains(f.body, []byte(endpoint)) || bytes.Contains(f.body, []byte(`"endpoint"`)) || bytes.Contains(f.body, []byte(`"header"`)) {
		t.Fatalf("fixture: the sealed body states the endpoint or a header: %s", f.body)
	}
	return f
}

// endpointStatedFrame is a create whose body still states channel.endpoint,
// as a subscriber's gateway older than E-18's widening sent it: the sealed
// create with the endpoint written back as channel's first member.
func endpointStatedFrame(t testing.TB, sub, endpoint string) subFrame {
	t.Helper()
	f := sealedCreateFrame(t, sub, endpoint)
	const channel = `"channel":{`
	if strings.Count(string(f.body), channel) != 1 {
		t.Fatalf("fixture: the body does not open channel once: %s", f.body)
	}
	f.body = []byte(strings.Replace(string(f.body), channel, channel+`"endpoint":"`+endpoint+`",`, 1))
	return f
}

// searchFrame is the payer's search by url, framed as its gateway frames it:
// the endpoint sealed (SealEndpointForSearch) as subscriptionEndpoint, and the
// rest of the query, with no url (shnsdk.ParseSubscriptionSearch).
func searchFrame(t testing.TB, endpoint string) subFrame {
	t.Helper()
	sealed := payerKeys(t).SealEndpointForSearch(payerBinding, endpoint)
	// The search as the payer's system sent it, url=<endpoint>&status=…,
	// framed as its gateway frames it: the url removed, the endpoint sealed.
	q, err := shnsdk.ParseSubscriptionSearch("url=" + url.QueryEscape(endpoint) + "&status=requested,active")
	if err != nil {
		t.Fatal(err)
	}
	return subFrame{request: q.Request.String(), endpoint: strings.Join(sealed, " ")}
}

// wantDelivered fails unless a is the provider's 2xx answer, carried byte for
// byte, framed with the provider's base as statedBase, under no patient.
func (p *subProvider) wantDelivered(t *testing.T, a subAnswer, status int, body []byte) {
	t.Helper()
	if !a.framed || a.status != status || !bytes.Equal(a.body, body) {
		t.Fatalf("answer %d (framed %v) %s, want %d with the provider's answer byte for byte %s", a.status, a.framed, a.body, status, body)
	}
	if got := a.headers[shnsdk.FrameHeaderStatedBase]; got != p.sys.srv.URL+"/fhir" {
		t.Errorf("statedBase %q, want the provider's base %s", got, p.sys.srv.URL+"/fhir")
	}
	if a.subject != "" {
		t.Errorf("the answer's token names %q, want no patient", a.subject)
	}
}

// The control, at every level: a create reaches the provider's own
// Subscription endpoint with its channel re-keyed (E-19): its endpoint this
// gateway's notification address for the payer and the topic, naming the
// payer's sealed endpoint, and its header this gateway's credential for that
// address and the payer's sealed headers, and nothing else of the
// Subscription changed. The forward discloses the edit; the record names it;
// the provider's 201 is delivered byte for byte, with the statedBase.
func TestCDexSubscribe_CreateIsRekeyedAndDelivered(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			f := createFrame(t, cdexSubscriptionJSON(nil))
			a := p.subscribe(t, f)
			calls := sys.calls()
			if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].requestURI != "/fhir/Subscription" {
				t.Fatalf("the provider's system got %+v, want one POST /fhir/Subscription", calls)
			}
			got := calls[0]
			p.wantDelivered(t, a, http.StatusCreated, got.body)
			if ct := got.header.Get("Content-Type"); ct != "application/fhir+json" {
				t.Errorf("forwarded as %q", ct)
			}
			// The payer's gateway sealed the channel (E-18): the forward names
			// it beside this gateway's re-key (E-19).
			if e := got.header.Values(EditedHeader); !slices.Equal(e, []string{"subscription-header-seal", "subscription-endpoint-rekey"}) {
				t.Errorf("%s = %q, want subscription-header-seal, subscription-endpoint-rekey", EditedHeader, e)
			}
			s, rf := subscription.ParseSubscription(got.body, "application/fhir+json")
			if rf != nil {
				t.Fatal(rf)
			}
			if !p.keys().Owns(s, cdexIngressBase, "payer-x", cdextopic.Topic{}) {
				t.Fatalf("what the provider's system holds is not the payer's by the ownership test: %s", got.body)
			}
			addr, ok := subscription.ParseAddress(endpointOf(got.body), cdexIngressBase)
			if !ok || addr.Subscriber != "payer-x" || addr.Topic != cdextopic.TokenValue || addr.SealedEndpoint != f.endpoint {
				t.Fatalf("endpoint %q, want this gateway's notification address for payer-x naming the sealed endpoint", endpointOf(got.body))
			}
			if bytes.Contains(got.body, []byte("payer-secret")) || bytes.Contains(got.body, []byte("payer-sys.example")) {
				t.Fatalf("the payer's endpoint or secret reached the provider's system: %s", got.body)
			}
			// Every byte but the channel's endpoint and header is the payer's.
			var sent, held map[string]any
			_ = json.Unmarshal(f.body, &sent)
			_ = json.Unmarshal(got.body, &held)
			delete(sent["channel"].(map[string]any), "endpoint")
			delete(held["channel"].(map[string]any), "endpoint")
			delete(held["channel"].(map[string]any), "header")
			if a, b := jsonOf(sent), jsonOf(held); !bytes.Equal(a, b) {
				t.Fatalf("the Subscription changed beyond its channel:\n%s\n%s", a, b)
			}
			rec := p.got.only(t)
			if rec.Outcome != ExchangeAnswered || rec.Exchange != legCDexTaskSubscribe || rec.Ownership != "edited" || !slices.Equal(rec.Edits, []string{"E-19"}) {
				t.Fatalf("record %s %s (request %s, edits %v), want answered with E-19", rec.Outcome, rec.Exchange, rec.Ownership, rec.Edits)
			}
		})
	}
}

// The subscriber's gateway seals the whole channel (E-18), so a create or an
// update reaches this gateway with no channel.endpoint: it is accepted, E-19
// writes the endpoint from the sealed value alone, and the provider's system
// gets the notification address, at every level. A body that still states an
// endpoint is not refused: E-19 overwrites it.
func TestCDexSubscribe_ASealedChannelIsWrittenByE19(t *testing.T) {
	for _, level := range allLevels {
		for name, frame := range map[string]func(t *testing.T) subFrame{
			"no endpoint in the body": func(t *testing.T) subFrame { return sealedCreateFrame(t, cdexSubscriptionJSON(nil), payerEndpoint) },
			"an endpoint still in the body": func(t *testing.T) subFrame {
				return endpointStatedFrame(t, cdexSubscriptionJSON(nil), payerEndpoint)
			},
		} {
			for _, request := range []string{"POST Subscription", "PUT Subscription/sub-1"} {
				t.Run(level.String()+"/"+name+"/"+request, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					f := frame(t)
					f.request = request
					a := p.subscribe(t, f)
					if a.status/100 != 2 {
						t.Fatalf("answer %d %s, want the Subscription forwarded", a.status, a.body)
					}
					calls := sys.calls()
					got := calls[len(calls)-1]
					s, rf := subscription.ParseSubscription(got.body, "application/fhir+json")
					if rf != nil || !p.keys().Owns(s, cdexIngressBase, "payer-x", cdextopic.Topic{}) {
						t.Fatalf("what the provider's system got is not the payer's by the ownership test (%v): %s", rf, got.body)
					}
					if addr, ok := subscription.ParseAddress(endpointOf(got.body), cdexIngressBase); !ok || addr.SealedEndpoint != f.endpoint {
						t.Fatalf("endpoint %q, want this gateway's notification address naming the sealed endpoint", endpointOf(got.body))
					}
					if bytes.Contains(got.body, []byte("payer-sys.example")) {
						t.Fatalf("the payer's endpoint reached the provider's system: %s", got.body)
					}
					// E-18, declared by the frame's sealed values, then E-19.
					want := []string{"subscription-header-seal", "subscription-endpoint-rekey"}
					if e := got.header.Values(EditedHeader); !slices.Equal(e, want) {
						t.Errorf("%s = %q, want %q", EditedHeader, e, want)
					}
				})
			}
		}
	}
}

// The forward discloses E-18, the payer's gateway's edit, before E-19, its
// own, by the published frame contract: a create's or an update's
// subscriptionEndpoint and subscriptionChannel are E-18's output, so a frame
// carrying both declares it, whatever the body still states; the body is
// never read for it. Each names the signature that covered the channel, at
// every level, on a create and an update, and the record never names E-18
// as this gateway's. A frame missing either value is refused, never
// forwarded with no claim (TestCDexSubscribe_RefusalsByPrimitive's "no
// sealed endpoint" and "no sealed headers" rows).
func TestCDexSubscribe_E18IsDisclosedFromTheFramesSealedValues(t *testing.T) {
	const sig = `{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-13T00:00:00Z","who":{"reference":"Organization/payer"}}`
	signed := cdexSubscriptionJSON(func(s string) string {
		return strings.Replace(s, `"status":"requested",`, `"status":"requested","signature":`+sig+`,`, 1)
	})
	const signedBy = `; signed="Signature in Subscription"`
	both := []string{"subscription-header-seal", "subscription-endpoint-rekey"}
	bothSigned := []string{"subscription-header-seal" + signedBy, "subscription-endpoint-rekey" + signedBy}
	for _, level := range allLevels {
		for name, c := range map[string]struct {
			frame func(t *testing.T) subFrame
			want  []string
		}{
			"a sealed channel":                {func(t *testing.T) subFrame { return sealedCreateFrame(t, cdexSubscriptionJSON(nil), payerEndpoint) }, both},
			"a sealed, signed channel":        {func(t *testing.T) subFrame { return sealedCreateFrame(t, signed, payerEndpoint) }, bothSigned},
			"an endpoint still in the body":   {func(t *testing.T) subFrame { return endpointStatedFrame(t, cdexSubscriptionJSON(nil), payerEndpoint) }, both},
			"a signed body with its endpoint": {func(t *testing.T) subFrame { return endpointStatedFrame(t, signed, payerEndpoint) }, bothSigned},
			"headers still in the body": {func(t *testing.T) subFrame {
				f := sealedCreateFrame(t, cdexSubscriptionJSON(nil), payerEndpoint)
				f.body = bytes.Replace(f.body, []byte(`"channel":{`), []byte(`"channel":{"header":["X-Payer: a"],`), 1)
				return f
			}, both},
			"an empty header array still in the body": {func(t *testing.T) subFrame {
				f := sealedCreateFrame(t, cdexSubscriptionJSON(nil), payerEndpoint)
				f.body = bytes.Replace(f.body, []byte(`"channel":{`), []byte(`"channel":{"header":[],`), 1)
				return f
			}, both},
		} {
			for _, request := range []string{"POST Subscription", "PUT Subscription/sub-1"} {
				t.Run(level.String()+"/"+name+"/"+request, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					f := c.frame(t)
					f.request = request
					if a := p.subscribe(t, f); a.status/100 != 2 {
						t.Fatalf("answer %d %s, want the Subscription forwarded", a.status, a.body)
					}
					calls := sys.calls()
					got := calls[len(calls)-1]
					if e := got.header.Values(EditedHeader); !slices.Equal(e, c.want) {
						t.Fatalf("%s = %q, want %q", EditedHeader, e, c.want)
					}
					rec := p.got.only(t)
					for _, e := range append(slices.Clone(rec.Edits), rec.RequestEdits...) {
						if strings.Contains(e, "E-18") || strings.Contains(e, "subscription-header-seal") {
							t.Fatalf("the record names E-18 as this gateway's edit: %v %v", rec.Edits, rec.RequestEdits)
						}
					}
				})
			}
		}
	}
}

// A signed Subscription is re-keyed all the same, and the forward
// names the signature that covered the channel. The closed shape allows no
// extension but the Backport IG's (the HRex Task topic defines none), so a
// signature in an extension is refused by the shape first; one the shape
// does not judge (a Signature element in a member of the Subscription's own)
// is carried, re-keyed and disclosed.
func TestCDexSubscribe_ASignedSubscriptionIsRekeyedAndDisclosed(t *testing.T) {
	const sig = `{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-13T00:00:00Z","who":{"reference":"Organization/payer"}}`
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			signed := cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"status":"requested",`, `"status":"requested","signature":`+sig+`,`, 1)
			})
			a := p.subscribe(t, createFrame(t, signed))
			calls := sys.calls()
			if a.status != http.StatusCreated || len(calls) != 1 {
				t.Fatalf("answer %d %s, calls %d; want the signed Subscription created", a.status, a.body, len(calls))
			}
			if e := calls[0].header.Values(EditedHeader); !slices.Equal(e, []string{`subscription-header-seal; signed="Signature in Subscription"`,
				`subscription-endpoint-rekey; signed="Signature in Subscription"`}) {
				t.Fatalf("%s = %q, want the seal and the re-key, each with its signature named", EditedHeader, e)
			}
			if !bytes.Contains(calls[0].body, []byte(`"signature":`+sig)) {
				t.Fatalf("the signature is not carried as sent: %s", calls[0].body)
			}
			inExtension := cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"status":"requested",`, `"status":"requested","extension":[{"url":"http://example.org/sig","valueSignature":`+sig+`}],`, 1)
			})
			if a := p.subscribe(t, createFrame(t, inExtension)); a.status != http.StatusForbidden {
				t.Fatalf("a signature in an extension: %d %s, want the closed shape's 403", a.status, a.body)
			}
		})
	}
}

// An update of a Subscription that does not exist is checked as a create
// and forwarded: the read finds nothing, then the update carries the
// re-keyed channel. An update of the payer's own is forwarded after the read.
// An update of another's is the network's 404, after the read alone.
func TestCDexSubscribe_UpdateReadsFirst(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			for name, c := range map[string]struct {
				stored  func(p *subProvider) []byte
				calls   []string
				status  int
				network bool
			}{
				"none held": {nil, []string{"GET /fhir/Subscription/sub-1", "PUT /fhir/Subscription/sub-1"}, http.StatusOK, false},
				"the payer's own": {func(p *subProvider) []byte { return p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil)) },
					[]string{"GET /fhir/Subscription/sub-1", "PUT /fhir/Subscription/sub-1"}, http.StatusOK, false},
				"another payer's": {func(p *subProvider) []byte { return p.storedFor(t, "payer-y", cdexSubscriptionJSON(nil)) },
					[]string{"GET /fhir/Subscription/sub-1"}, http.StatusNotFound, true},
				"one outside the network": {func(*subProvider) []byte { return []byte(cdexSubscriptionJSON(nil)) },
					[]string{"GET /fhir/Subscription/sub-1"}, http.StatusNotFound, true},
			} {
				t.Run(name, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					if c.stored != nil {
						sys.store["sub-1"] = c.stored(p)
					}
					f := createFrame(t, cdexSubscriptionJSON(nil))
					f.request = "PUT Subscription/sub-1"
					a := p.subscribe(t, f)
					var got []string
					for _, call := range sys.calls() {
						got = append(got, call.method+" "+call.requestURI)
					}
					if !slices.Equal(got, c.calls) || a.status != c.status {
						t.Fatalf("calls %v, answer %d %s; want %v and %d", got, a.status, a.body, c.calls, c.status)
					}
					if c.network {
						if !bytes.Equal(a.body, cdexSubscriptionNotFound) {
							t.Fatalf("answer %s, want the network's 404", a.body)
						}
						wantRefusal(t, p.got.only(t), http.StatusNotFound, RefusedByProviderGateway, RefusalAuthority)
						return
					}
					p.wantDelivered(t, a, http.StatusOK, sys.calls()[1].body)
				})
			}
		})
	}
}

// A read's and a vread's query (_elements, _summary) is carried to the
// provider's system as sent, and the ownership test decides the answer: one
// that keeps the channel is delivered to its owner, one that hides it
// (_summary=true drops channel.header, an _elements without channel drops
// the channel) is the network's 404.
func TestCDexSubscribe_AReadsQueryIsCarriedAndTheOwnershipTestDecides(t *testing.T) {
	for _, level := range allLevels {
		for _, c := range []struct {
			request, uri string
			delivered    bool
		}{
			{"GET Subscription/sub-1?_summary=false", "/fhir/Subscription/sub-1?_summary=false", true},
			{"GET Subscription/sub-1?_elements=channel,status", "/fhir/Subscription/sub-1?_elements=channel,status", true},
			{"GET Subscription/sub-1/_history/2?_summary=false", "/fhir/Subscription/sub-1/_history/2?_summary=false", true},
			{"GET Subscription/sub-1?_summary=true", "/fhir/Subscription/sub-1?_summary=true", false},
			{"GET Subscription/sub-1?_elements=status", "/fhir/Subscription/sub-1?_elements=status", false},
			{"GET Subscription/sub-1/_history/2?_elements=status,criteria", "/fhir/Subscription/sub-1/_history/2?_elements=status,criteria", false},
		} {
			t.Run(level.String()+"/"+c.request, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				a := p.subscribe(t, subFrame{request: c.request})
				calls := sys.calls()
				if len(calls) != 1 || calls[0].requestURI != c.uri {
					t.Fatalf("calls %+v, want one GET %s", calls, c.uri)
				}
				if c.delivered {
					p.wantDelivered(t, a, http.StatusOK, a.body)
					if !bytes.Contains(a.body, []byte(`"channel"`)) {
						t.Fatalf("delivered %s, want the Subscription's channel", a.body)
					}
					return
				}
				if a.status != http.StatusNotFound || !bytes.Equal(a.body, cdexSubscriptionNotFound) {
					t.Fatalf("answer %d %s, want the network's 404", a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusNotFound, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}

// A page's link may carry the search's _elements after the rest, as a HAPI
// FHIR server writes it: carried as sent when it keeps channel.
func TestCDexSubscribe_APageCarriesItsElements(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
			const q = "_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset&_elements=channel,status"
			a := p.subscribe(t, subFrame{request: "GET ?" + q})
			if calls := sys.calls(); len(calls) != 1 || calls[0].requestURI != "/fhir?"+q {
				t.Fatalf("calls %+v, want the page with its _elements", calls)
			}
			p.wantDelivered(t, a, http.StatusOK, a.body)
		})
	}
}

// The payer's If-Match is carried to the provider's system as the HTTP
// If-Match of an update's and a delete's own forward, as sent, and never on
// the gateway's own read before it; with none framed, none is sent.
func TestCDexSubscribe_IfMatchIsCarriedOnTheForwardOnly(t *testing.T) {
	const etag = `W/"3"`
	for _, level := range allLevels {
		for _, c := range []struct{ request, method string }{
			{"PUT Subscription/sub-1", http.MethodPut},
			{"DELETE Subscription/sub-1", http.MethodDelete},
		} {
			for _, framed := range []bool{true, false} {
				t.Run(level.String()+"/"+c.request+"/framed="+map[bool]string{true: "yes", false: "no"}[framed], func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
					f := subFrame{request: c.request}
					if c.method == http.MethodPut {
						f = createFrame(t, cdexSubscriptionJSON(nil))
						f.request = c.request
					}
					if framed {
						f.ifMatch = etag
					}
					if a := p.subscribe(t, f); a.status/100 != 2 {
						t.Fatalf("answer %d %s", a.status, a.body)
					}
					calls := sys.calls()
					if len(calls) != 2 || calls[0].method != http.MethodGet || calls[1].method != c.method {
						t.Fatalf("calls %+v, want the read, then the %s", calls, c.method)
					}
					if v := calls[0].header.Values("If-Match"); len(v) != 0 {
						t.Errorf("the gateway's own read carried If-Match %q", v)
					}
					want := []string(nil)
					if framed {
						want = []string{etag}
					}
					if v := calls[1].header.Values("If-Match"); !slices.Equal(v, want) {
						t.Errorf("the %s carried If-Match %q, want %q", c.method, v, want)
					}
				})
			}
		}
	}
}

// The subscriber's endpoint never reaches the notifier's gateway or the
// provider's system in clear, across a create and a search, at every level:
// the subscriber's gateway seals the whole channel (E-18) and frames the
// search with no url. The frame the notifier's gateway decrypts, headers and
// body, names it in no form; the provider's system sees only the
// notification address, in the create's body and the search's url, and its
// headers and queries name the endpoint in none; the gateway's exchange
// records and its log name it in none either.
func TestCDexSubscribe_TheSubscribersEndpointIsNeverSeen(t *testing.T) {
	const endpoint = "https://zq7-endpoint-canary.payer-sys.example/hooks/cdex?tenant=zq7-canary-tenant"
	forms := []string{endpoint, url.QueryEscape(endpoint), url.PathEscape(endpoint), "zq7-endpoint-canary", "zq7-canary-tenant"}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prev) })
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			sub := cdexSubscriptionJSON(func(s string) string { return strings.Replace(s, payerEndpoint, endpoint, 1) })
			create := sealedCreateFrame(t, sub, endpoint)
			// The frame as the notifier's gateway decrypts it: its headers and
			// its body.
			for _, f := range forms {
				if strings.Contains(create.request+create.endpoint+create.channel+create.contentType+string(create.body), f) {
					t.Fatalf("the create's frame names the endpoint (%q): %+v %s", f, create, create.body)
				}
			}
			if a := p.subscribe(t, create); a.status != http.StatusCreated {
				t.Fatalf("create: %d %s", a.status, a.body)
			}
			search := searchFrame(t, endpoint)
			for _, f := range forms {
				if strings.Contains(search.request+search.endpoint+string(search.body), f) {
					t.Fatalf("the search's frame names the endpoint (%q): %+v", f, search)
				}
			}
			if a := p.subscribe(t, search); a.status != http.StatusOK || !bytes.Contains(a.body, []byte(`"total":1`)) {
				t.Fatalf("search: %d %s, want the Subscription found", a.status, a.body)
			}
			var seen []string
			for _, c := range sys.calls() {
				seen = append(seen, c.requestURI, string(c.body))
				for k, vs := range c.header {
					seen = append(seen, k+": "+strings.Join(vs, ", "))
				}
			}
			p.got.mu.Lock()
			recs := jsonOf(p.got.recs)
			p.got.mu.Unlock()
			seen = append(seen, string(recs), logs.String())
			for _, s := range seen {
				for _, f := range forms {
					if strings.Contains(s, f) {
						t.Fatalf("the endpoint (%q) was seen: %s", f, s)
					}
				}
			}
		})
	}
}

// The subscriber's dedupe search finds the Subscription its create made, and
// only it: the create's endpoint and the search's are sealed by the same
// deterministic seal, so E-21 writes the very address E-19 wrote. Another
// endpoint's Subscription of the same payer, and another payer's of the same
// endpoint, are not found.
func TestCDexSubscribe_TheDedupeSearchFindsExactlyItsSubscription(t *testing.T) {
	const other = "https://payer-sys.example/api/cdex/other"
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			if a := p.subscribe(t, createFrame(t, cdexSubscriptionJSON(nil))); a.status != http.StatusCreated {
				t.Fatalf("create: %d %s", a.status, a.body)
			}
			sys.store["sub-2"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(strings.Replace(s, payerEndpoint, other, 1), `"id":"sub-1"`, `"id":"sub-2"`, 1)
			}))
			sys.store["sub-3"] = p.storedFor(t, "payer-y", cdexSubscriptionJSON(func(s string) string { return strings.Replace(s, `"id":"sub-1"`, `"id":"sub-3"`, 1) }))
			a := p.subscribe(t, searchFrame(t, payerEndpoint))
			var got struct {
				Total int
				Entry []struct {
					Resource struct{ ID string }
				}
			}
			if err := json.Unmarshal(a.body, &got); err != nil || a.status != http.StatusOK {
				t.Fatalf("search: %d %s", a.status, a.body)
			}
			if got.Total != 1 || len(got.Entry) != 1 || got.Entry[0].Resource.ID != "sub-1" {
				t.Fatalf("the search found %+v, want exactly sub-1", got)
			}
		})
	}
}

// The read before an update, a delete or a $status that the provider's
// system answers with an error other than 404 or 410 is carried as sent, and
// nothing more is sent: only an id the system does not hold is an update as
// create.
func TestCDexSubscribe_TheReadsOwnErrorIsCarried(t *testing.T) {
	for _, level := range allLevels {
		for _, request := range []string{"PUT Subscription/sub-1", "DELETE Subscription/sub-1", "GET Subscription/sub-1/$status"} {
			t.Run(level.String()+"/"+request, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				const failed = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"exception"}]}`
				sys.answer = func(r *http.Request) (readAnswer, bool) {
					if r.Method != http.MethodGet || r.URL.Path != "/fhir/Subscription/sub-1" {
						return readAnswer{}, false
					}
					return readAnswer{status: http.StatusInternalServerError, header: fhirJSONHeader, body: []byte(failed)}, true
				}
				f := subFrame{request: request}
				if strings.HasPrefix(request, "PUT") {
					f = createFrame(t, cdexSubscriptionJSON(nil))
					f.request = request
				}
				a := p.subscribe(t, f)
				if a.status != http.StatusInternalServerError || string(a.body) != failed {
					t.Fatalf("answer %d %s, want the system's 500 as sent", a.status, a.body)
				}
				if got := sys.calls(); len(got) != 1 {
					t.Fatalf("calls %+v, want the read alone", got)
				}
			})
		}
	}
}

// A read and a vread are delivered only when the Subscription is the
// payer's; another payer's, or one outside the network, is the network's
// 404, the same bytes in every case. The provider's own 404 is carried.
func TestCDexSubscribe_ReadIsDeliveredOnlyToItsOwner(t *testing.T) {
	for _, level := range allLevels {
		for _, request := range []string{"GET Subscription/sub-1", "GET Subscription/sub-1/_history/2"} {
			t.Run(level.String()+"/"+request, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				own := p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				sys.store["sub-1"] = own
				p.wantDelivered(t, p.subscribe(t, subFrame{request: request}), http.StatusOK, own)
				for name, stored := range map[string][]byte{
					"another payer's":         p.storedFor(t, "payer-y", cdexSubscriptionJSON(nil)),
					"one outside the network": []byte(cdexSubscriptionJSON(nil)),
					"another topic's address": bytes.Replace(own, []byte("/notify/cdex-task/"), []byte("/notify/pas/"), 1),
				} {
					sys.store["sub-1"] = stored
					a := p.subscribe(t, subFrame{request: request})
					if a.status != http.StatusNotFound || !bytes.Equal(a.body, cdexSubscriptionNotFound) {
						t.Fatalf("%s: %d %s, want the network's 404", name, a.status, a.body)
					}
				}
				delete(sys.store, "sub-1")
				if a := p.subscribe(t, subFrame{request: request}); a.status != http.StatusNotFound || string(a.body) != subNotFound {
					t.Fatalf("the provider's own 404: %d %s, want it carried as sent", a.status, a.body)
				}
			})
		}
	}
}

// A delete and an instance $status are forwarded only once a read shows the
// Subscription is the payer's; otherwise the network's 404 after the read
// alone.
func TestCDexSubscribe_DeleteAndStatusReadFirst(t *testing.T) {
	for _, level := range allLevels {
		for _, c := range []struct{ request, method, uri string }{
			{"DELETE Subscription/sub-1", http.MethodDelete, "/fhir/Subscription/sub-1"},
			{"GET Subscription/sub-1/$status", http.MethodGet, "/fhir/Subscription/sub-1/$status"},
		} {
			t.Run(level.String()+"/"+c.request, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				a := p.subscribe(t, subFrame{request: c.request})
				calls := sys.calls()
				if len(calls) != 2 || calls[0].requestURI != "/fhir/Subscription/sub-1" || calls[1].method != c.method || calls[1].requestURI != c.uri || len(calls[1].body) != 0 {
					t.Fatalf("calls %+v, want the read, then %s %s with no body", calls, c.method, c.uri)
				}
				p.wantDelivered(t, a, http.StatusOK, a.body)
				sys2 := newSubStore(t)
				p2 := newSubProvider(t, level, sys2)
				sys2.store["sub-1"] = p2.storedFor(t, "payer-y", cdexSubscriptionJSON(nil))
				if a := p2.subscribe(t, subFrame{request: c.request}); a.status != http.StatusNotFound || !bytes.Equal(a.body, cdexSubscriptionNotFound) {
					t.Fatalf("another payer's: %d %s, want the network's 404", a.status, a.body)
				}
				if got := sys2.calls(); len(got) != 1 {
					t.Fatalf("another payer's: calls %+v, want the read alone", got)
				}
			})
		}
	}
}

// A search by url is forwarded with the url re-keyed to the notification
// address E-19 wrote (E-21), every other parameter as sent, and finds the
// payer's own Subscription, as its pre-subscribe search would directly. Its
// answer is delivered when every Subscription in it is the payer's; one
// holding another's is refused 403 on authority.
func TestCDexSubscribe_SearchIsRekeyedAndChecked(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			if a := p.subscribe(t, createFrame(t, cdexSubscriptionJSON(nil))); a.status != http.StatusCreated {
				t.Fatalf("create: %d %s", a.status, a.body)
			}
			f := searchFrame(t, payerEndpoint)
			a := p.subscribe(t, f)
			calls := sys.calls()
			search := calls[len(calls)-1]
			if a.status != http.StatusOK || !bytes.Contains(a.body, []byte(`"total":1`)) {
				t.Fatalf("search: %d %s, want the payer's Subscription found", a.status, a.body)
			}
			p.wantDelivered(t, a, http.StatusOK, a.body)
			if !strings.HasSuffix(search.requestURI, "&status=requested,active") || !strings.HasPrefix(search.requestURI, "/fhir/Subscription?url=") {
				t.Fatalf("forwarded %s, want the url re-keyed and the status carried as sent", search.requestURI)
			}
			if bytes.Contains([]byte(search.requestURI), []byte("payer-sys.example")) {
				t.Fatalf("the payer's endpoint reached the provider's system: %s", search.requestURI)
			}
			if e := search.header.Get(EditedHeader); e != "subscription-url-search-rekey" {
				t.Errorf("%s = %q", EditedHeader, e)
			}
			if rec := p.got.last(t); !slices.Equal(rec.Edits, []string{"E-21"}) {
				t.Errorf("search record edits %v, want E-21", rec.Edits)
			}
			// Another payer's Subscription in the answer is refused.
			sys.answer = func(r *http.Request) (readAnswer, bool) {
				if r.URL.Path != "/fhir/Subscription" {
					return readAnswer{}, false
				}
				other := p.storedFor(t, "payer-y", cdexSubscriptionJSON(nil))
				return readAnswer{status: http.StatusOK, header: fhirJSONHeader, body: []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + string(other) + `}]}`)}, true
			}
			if a := p.subscribe(t, f); a.status != http.StatusForbidden || !strings.Contains(string(a.body), refusalCDexSubscribeSearchNotOwned) {
				t.Fatalf("another's in the answer: %d %s, want 403", a.status, a.body)
			}
		})
	}
}

// A page of a search is forwarded to the provider's base with its query as
// sent, and checked as a search's answer is.
func TestCDexSubscribe_APageIsForwardedAndChecked(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
			a := p.subscribe(t, subFrame{request: "GET ?_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset"})
			calls := sys.calls()
			if len(calls) != 1 || calls[0].requestURI != "/fhir?_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset" {
				t.Fatalf("calls %+v", calls)
			}
			p.wantDelivered(t, a, http.StatusOK, a.body)
			sys.store["sub-2"] = p.storedFor(t, "payer-y", cdexSubscriptionJSON(nil))
			if a := p.subscribe(t, subFrame{request: "GET ?_getpages=abc"}); a.status != http.StatusForbidden {
				t.Fatalf("a page holding another's: %d %s, want 403", a.status, a.body)
			}
		})
	}
}

// Every 2xx answer is held to the answer check first: a resource other than
// a Subscription, an OperationOutcome, a searchset of them or a $status
// answer is refused 502 on authority, and not delivered.
func TestCDexSubscribe_TheAnswerCheckComesFirst(t *testing.T) {
	patient := `{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`
	for _, level := range allLevels {
		for name, c := range map[string]struct {
			frame func(p *subProvider) subFrame
			body  string
		}{
			"a Patient for a create":               {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) }, patient},
			"a Patient in a search":                {func(*subProvider) subFrame { return searchFrame(t, payerEndpoint) }, `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`},
			"a search whose entry is not an array": {func(*subProvider) subFrame { return searchFrame(t, payerEndpoint) }, `{"resourceType":"Bundle","type":"searchset","entry":{"resource":` + patient + `}}`},
			"a read answering a contained Patient": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1"} }, `{"resourceType":"Subscription","contained":[` + patient + `]}`},
			"a page whose entry is not an object":  {func(*subProvider) subFrame { return subFrame{request: "GET ?_getpages=abc"} }, `{"resourceType":"Bundle","type":"searchset","entry":["x"]}`},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.answer = func(r *http.Request) (readAnswer, bool) {
					return readAnswer{status: http.StatusOK, header: fhirJSONHeader, body: []byte(c.body)}, true
				}
				a := p.subscribe(t, c.frame(p))
				// The refused answer is the gateway's fixed OperationOutcome: its reason echoes only the answer's resource type, member names and FHIR type names, never a minted value, so "Doe" cannot match one by chance.
				if a.status != http.StatusBadGateway || bytes.Contains(a.body, []byte("Doe")) {
					t.Fatalf("answer %d %s, want 502 with nothing of the answer", a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}

// The answer check, at every level: a shape that names no patient is
// delivered whatever the interaction, and one holding a resource, a $status
// Bundle on an interaction naming no Subscription by id, or anything that is
// not FHIR JSON is refused 502 on authority, with nothing of it delivered. What each
// shape gets on each interaction, ownership included, is the matrix on both
// topics (TestSubscribe_TheAnswerMatrix).
func TestCDexSubscribe_TheAnswerCheck(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`
	outcome := `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`
	outcomeContained := `{"resourceType":"OperationOutcome","contained":[` + patient + `],"issue":[{"severity":"information","code":"informational"}]}`
	status := func(entries ...string) string {
		return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
	}
	statusEntry := func(id, part string) string {
		return `{"resource":{"resourceType":"Parameters","parameter":[{"name":"subscription","valueReference":{"reference":"Subscription/` + id + `"}},` +
			`{"name":"status","valueCode":"active"},{"name":"type","valueCode":"query-status"},{"name":"notification-event","part":[{"name":"focus"` + part + `}]}]}}`
	}
	for _, level := range allLevels {
		for name, c := range map[string]struct {
			frame     func(p *subProvider) subFrame
			body      func(p *subProvider) string
			status    int
			delivered bool
		}{
			"a search answered with a status Bundle": {func(*subProvider) subFrame { return searchFrame(t, payerEndpoint) },
				func(*subProvider) string { return status(statusEntry("sub-1", "")) }, http.StatusOK, false},
			"a delete answered with the payer's Subscription": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(p *subProvider) string { return string(p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))) }, http.StatusOK, true},
			"a delete answered with a $status Bundle": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(*subProvider) string { return status(statusEntry("sub-1", "")) }, http.StatusOK, true},
			"a delete answered with no body": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(*subProvider) string { return "" }, http.StatusNoContent, true},
			"a create answered with no body": {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
				func(*subProvider) string { return "" }, http.StatusCreated, true},
			"a create answered with an OperationOutcome": {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
				func(*subProvider) string { return outcome }, http.StatusCreated, true},
			"an update answered with an OperationOutcome": {func(*subProvider) subFrame {
				f := createFrame(t, cdexSubscriptionJSON(nil))
				f.request = "PUT Subscription/sub-1"
				return f
			}, func(*subProvider) string { return outcome }, http.StatusOK, true},
			"a create answered with an OperationOutcome holding a resource": {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
				func(*subProvider) string { return outcomeContained }, http.StatusCreated, false},
			"a $status answered as a searchset": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string { return status(statusEntry("sub-1", "")) }, http.StatusOK, true},
			"a $status answered with two status entries": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string { return status(statusEntry("sub-1", ""), statusEntry("sub-1", "")) }, http.StatusOK, false},
			"a $status naming another id": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string { return status(statusEntry("sub-2", "")) }, http.StatusOK, false},
			"a read answered with FHIR XML": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1"} },
				func(*subProvider) string {
					return `<Subscription xmlns="http://hl7.org/fhir"><status value="active"/></Subscription>`
				}, http.StatusOK, false},
			"a create answered with an XML OperationOutcome": {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
				func(*subProvider) string {
					return `<OperationOutcome xmlns="http://hl7.org/fhir"><issue/></OperationOutcome>`
				}, http.StatusCreated, false},
			"a delete answered with plain text": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(*subProvider) string { return "Deleted" }, http.StatusOK, false},
			"a read answered with a JSON object naming no resource": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1"} },
				func(*subProvider) string { return `{"status":"active"}` }, http.StatusOK, false},
			"a read answered with an array holding the payer's": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1"} },
				func(p *subProvider) string {
					return "[" + string(p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))) + "]"
				}, http.StatusOK, false},
			"a create answered with an object holding a Patient": {func(*subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
				func(*subProvider) string { return `{"ok":true,"patient":` + patient + `}` }, http.StatusCreated, false},
			"a delete answered with UTF-16 JSON, no byte order mark": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(*subProvider) string { return string(utf16Body(outcome, true)[2:]) }, http.StatusOK, false},
			"a delete answered with whitespace": {func(*subProvider) subFrame { return subFrame{request: "DELETE Subscription/sub-1"} },
				func(*subProvider) string { return " \n" }, http.StatusOK, true},
			"a $status whose part carries a resource": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string { return status(statusEntry("sub-1", `,"resource":`+patient)) }, http.StatusOK, false},
			// The topic's notification shape: a focus is a Task, and no
			// additional-context is declared.
			"a $status whose focus is a Task": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string {
					return status(statusEntry("sub-1", `,"valueReference":{"reference":"Task/t1"}`))
				}, http.StatusOK, true},
			"a $status whose focus is a Patient": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string {
					return status(statusEntry("sub-1", `,"valueReference":{"reference":"Patient/p1"}`))
				}, http.StatusOK, false},
			"a $status naming a Patient beside its parameters": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string {
					return status(strings.Replace(statusEntry("sub-1", `,"valueReference":{"reference":"Task/t1"}`), `"parameter":[`, `"subject":{"reference":"Patient/p1"},"parameter":[`, 1))
				}, http.StatusOK, false},
			"a $status with an additional-context": {func(*subProvider) subFrame { return subFrame{request: "GET Subscription/sub-1/$status"} },
				func(*subProvider) string {
					return status(statusEntry("sub-1", `,"valueReference":{"reference":"Task/t1"}},{"name":"additional-context","valueReference":{"reference":"Task/t2"}`))
				}, http.StatusOK, false},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				f := c.frame(p)
				answer := c.body(p)
				forwarded := map[string]string{"GET Subscription/sub-1": "/fhir/Subscription/sub-1", "DELETE Subscription/sub-1": "/fhir/Subscription/sub-1",
					"GET Subscription/sub-1/$status": "/fhir/Subscription/sub-1/$status", "POST Subscription": "/fhir/Subscription", "PUT Subscription/sub-1": "/fhir/Subscription/sub-1"}
				sys.answer = func(r *http.Request) (readAnswer, bool) {
					// The gateway's own read before an update, a delete or a
					// $status is answered by the store; the interaction's own
					// forward with the row's answer.
					if r.Method == http.MethodGet && r.URL.Path == "/fhir/Subscription/sub-1" && f.request != "GET Subscription/sub-1" {
						return readAnswer{}, false
					}
					if want, ok := forwarded[f.request]; ok && r.URL.Path != want {
						return readAnswer{}, false
					}
					return readAnswer{status: c.status, header: fhirJSONHeader, body: []byte(answer)}, true
				}
				a := p.subscribe(t, f)
				if c.delivered {
					if a.status != c.status || string(a.body) != answer {
						t.Fatalf("answer %d %s, want %d delivered as sent", a.status, a.body, c.status)
					}
					return
				}
				// The refused answer is the gateway's fixed OperationOutcome: its reason echoes only the answer's resource type, member names and FHIR type names, never a minted value, so "Doe" cannot match one by chance.
				if a.status != http.StatusBadGateway || bytes.Contains(a.body, []byte("Doe")) || bytes.Contains(a.body, []byte("notify/cdex-task")) {
					t.Fatalf("answer %d %s, want 502 with nothing of the answer", a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
			})
		}
	}
}

// A create's or an update's answer that carries a Subscription carries the
// payer's own, at every level: another payer's is the network's 404, with
// nothing of it delivered; the payer's own is delivered.
func TestCDexSubscribe_ACreatesAnswerIsThePayersOwn(t *testing.T) {
	for _, level := range allLevels {
		for _, request := range []string{"POST Subscription", "PUT Subscription/sub-1"} {
			for name, subscriber := range map[string]string{"the payer's own": "payer-x", "another payer's": "payer-y"} {
				t.Run(level.String()+"/"+request+"/"+name, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
					answer := p.storedFor(t, subscriber, cdexSubscriptionJSON(nil))
					method := map[string]string{"POST Subscription": http.MethodPost, "PUT Subscription/sub-1": http.MethodPut}[request]
					sys.answer = func(r *http.Request) (readAnswer, bool) {
						if r.Method != method {
							return readAnswer{}, false
						}
						return readAnswer{status: http.StatusOK, header: fhirJSONHeader, body: answer}, true
					}
					f := createFrame(t, cdexSubscriptionJSON(nil))
					f.request = request
					a := p.subscribe(t, f)
					if subscriber == "payer-x" {
						p.wantDelivered(t, a, http.StatusOK, answer)
						return
					}
					if a.status != http.StatusNotFound || !bytes.Equal(a.body, cdexSubscriptionNotFound) {
						t.Fatalf("answer %d %s, want the network's 404", a.status, a.body)
					}
					wantRefusal(t, p.got.only(t), http.StatusNotFound, RefusedByProviderGateway, RefusalAuthority)
				})
			}
		}
	}
}

// The body a create or an update carries is read by its structure, never
// its label: a JSON Subscription labelled as FHIR XML or as text is re-keyed
// and forwarded, at every level, as the network forwards a request whose
// media type it does not carry.
func TestCDexSubscribe_TheLabelDoesNotDecideTheBody(t *testing.T) {
	for _, level := range allLevels {
		for _, ct := range []string{"application/fhir+xml", "text/plain", "application/fhir+json; charset"} {
			t.Run(level.String()+"/"+ct, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				f := createFrame(t, cdexSubscriptionJSON(nil))
				f.contentType = ct
				a := p.subscribe(t, f)
				calls := sys.calls()
				if a.status != http.StatusCreated || len(calls) != 1 {
					t.Fatalf("answer %d %s, calls %d; want the Subscription created", a.status, a.body, len(calls))
				}
				s, rf := subscription.ReadSubscription(calls[0].body)
				if rf != nil || !p.keys().Owns(s, cdexIngressBase, "payer-x", cdextopic.Topic{}) {
					t.Fatalf("forwarded %s, want it re-keyed (%v)", calls[0].body, rf)
				}
			})
		}
	}
}

// A read's answer is the payer's only when it carries exactly one Subscription
// the payer owns: the answer check refuses a read answered with none or with
// a searchset first, and ownsOne holds the same line on its own, so a read
// never indexes an answer carrying none or delivers one carrying two.
func TestCDexSubscribe_OwnsOneIsExactlyOne(t *testing.T) {
	p := newSubProvider(t, EnforcementNone, newSubStore(t))
	s, rf := subscription.ParseSubscription(p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil)), "application/fhir+json")
	if rf != nil {
		t.Fatal(rf)
	}
	st := p.g.paState().cdexSubscription
	for name, c := range map[string]struct {
		subs []*subscription.Subscription
		want bool
	}{
		"none":               {nil, false},
		"the payer's":        {[]*subscription.Subscription{s}, true},
		"the payer's, twice": {[]*subscription.Subscription{s, s}, false},
	} {
		if got := st.ownsOne(c.subs, cdexIngressBase, "payer-x", cdextopic.Topic{}); got != c.want {
			t.Errorf("%s: ownsOne = %v, want %v", name, got, c.want)
		}
	}
	if st.ownsOne([]*subscription.Subscription{s}, cdexIngressBase, "payer-y", cdextopic.Topic{}) {
		t.Error("ownsOne holds the payer's Subscription another payer's")
	}
}

// A create's or an update's 2xx is framed with subscriptionLocation, the tail
// of the provider's Location, only when that Location lies under the
// provider's own base by the segment-boundary match E-16 uses, and reads as a
// Subscription id with an optional version (shnsdk.ParseSubscriptionLocation),
// at every level. Anything else frames none and is still delivered: a
// Location at another host or port, at a sibling path that merely shares the
// base's spelling, naming another type, carrying a query or a fragment,
// naming an id that is not one, or more than one Location. A non-2xx and every
// other interaction frame none. Whatever is framed reads as the SDK reads it.
func TestCDexSubscribe_TheLocationIsFramedUnderTheBase(t *testing.T) {
	create := func(p *subProvider) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) }
	update := func(p *subProvider) subFrame {
		f := createFrame(t, cdexSubscriptionJSON(nil))
		f.request = "PUT Subscription/sub-1"
		return f
	}
	request := func(v string) func(*subProvider) subFrame {
		return func(*subProvider) subFrame { return subFrame{request: v} }
	}
	for _, level := range allLevels {
		for name, c := range map[string]struct {
			frame     func(p *subProvider) subFrame
			method    string
			status    int
			body      string
			locations func(base string) []string
			want      string
		}{
			"a create's 201":                    {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, "Subscription/sub-1"},
			"a create's 201 with a version":     {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/_history/1"} }, "Subscription/sub-1/_history/1"},
			"a create's 200":                    {create, http.MethodPost, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, "Subscription/sub-1"},
			"a create's relative Location":      {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return []string{"Subscription/sub-1/_history/1"} }, "Subscription/sub-1/_history/1"},
			"a create's absolute-path Location": {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return []string{"/fhir/Subscription/sub-1"} }, "Subscription/sub-1"},
			"a host in another case": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string {
				return []string{strings.Replace(b, "http://", "HTTP://", 1) + "/Subscription/sub-1"}
			}, "Subscription/sub-1"},
			"an update's 200":                {update, http.MethodPut, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, "Subscription/sub-1"},
			"an update's 200 with a version": {update, http.MethodPut, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1/_history/2"} }, "Subscription/sub-1/_history/2"},
			"an update's 201 with a version": {update, http.MethodPut, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/_history/1"} }, "Subscription/sub-1/_history/1"},

			"another host": {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return []string{"https://other.example/fhir/Subscription/sub-1"} }, ""},
			"another port": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string {
				return []string{strings.Replace(b, "127.0.0.1:", "127.0.0.1:1", 1) + "/Subscription/sub-1"}
			}, ""},
			"another scheme": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string {
				return []string{strings.Replace(b, "http://", "https://", 1) + "/Subscription/sub-1"}
			}, ""},
			"a sibling path": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "-old/Subscription/sub-1"} }, ""},
			"a sibling path sharing the base's spelling": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "x/Subscription/sub-1"} }, ""},
			"the base itself":     {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b} }, ""},
			"another type":        {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Task/sub-1"} }, ""},
			"another type's name": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/subscription/sub-1"} }, ""},
			"a query":             {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1?x=1"} }, ""},
			"an empty query":      {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1?"} }, ""},
			"a fragment":          {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1#x"} }, ""},
			"an empty fragment":   {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1#"} }, ""},
			"userinfo": {create, http.MethodPost, http.StatusCreated, "", func(b string) []string {
				return []string{strings.Replace(b, "http://", "http://u@", 1) + "/Subscription/sub-1"}
			}, ""},
			"an id that is not an id":                 {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/a_b"} }, ""},
			"an id too long":                          {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/" + strings.Repeat("a", 65)} }, ""},
			"an escaped id":                           {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub%2D1"} }, ""},
			"a version that is not one":               {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/_history/a_b"} }, ""},
			"no version":                              {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/_history"} }, ""},
			"another segment":                         {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/extra"} }, ""},
			"a trailing slash":                        {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1/"} }, ""},
			"two Locations":                           {create, http.MethodPost, http.StatusCreated, "", func(b string) []string { return []string{b + "/Subscription/sub-1", b + "/Subscription/sub-1"} }, ""},
			"an empty Location":                       {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return []string{""} }, ""},
			"a blank Location":                        {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return []string{"  "} }, ""},
			"an update with no Location":              {update, http.MethodPut, http.StatusOK, "", func(string) []string { return nil }, ""},
			"an update's Location not under the base": {update, http.MethodPut, http.StatusOK, "", func(string) []string { return []string{"https://other.example/fhir/Subscription/sub-1"} }, ""},
			"no Location":                             {create, http.MethodPost, http.StatusCreated, "", func(string) []string { return nil }, ""},

			"a create's 422":      {create, http.MethodPost, http.StatusUnprocessableEntity, subNotFound, func(b string) []string { return []string{b + "/Subscription/sub-1"} }, ""},
			"a read":              {request("GET Subscription/sub-1"), http.MethodGet, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, ""},
			"a delete":            {request("DELETE Subscription/sub-1"), http.MethodDelete, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, ""},
			"a search":            {func(*subProvider) subFrame { return searchFrame(t, payerEndpoint) }, http.MethodGet, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, ""},
			"an instance $status": {request("GET Subscription/sub-1/$status"), http.MethodGet, http.StatusOK, "", func(b string) []string { return []string{b + "/Subscription/sub-1"} }, ""},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				f := c.frame(p)
				sys.locations = c.locations(sys.srv.URL + "/fhir")
				sys.answer = func(r *http.Request) (readAnswer, bool) {
					// The gateway's own read before an update or a delete,
					// and a read, a search and a $status, are the store's.
					if r.Method != c.method || r.Method == http.MethodGet {
						return readAnswer{}, false
					}
					return readAnswer{status: c.status, header: fhirJSONHeader, body: []byte(c.body)}, true
				}
				if c.method == http.MethodGet {
					// The store answers; the hook adds the Location to it.
					sys.answer = func(r *http.Request) (readAnswer, bool) {
						a := sys.serve(r, nil)
						return a, true
					}
				}
				a := p.subscribe(t, f)
				if a.status/100 == 2 && a.status != c.status {
					t.Fatalf("answer %d %s, want %d", a.status, a.body, c.status)
				}
				if a.status/100 != 2 && a.status != c.status {
					t.Fatalf("answer %d %s, want %d carried", a.status, a.body, c.status)
				}
				got, framed := a.headers[shnsdk.FrameHeaderSubscriptionLocation]
				if framed {
					if _, _, err := shnsdk.ParseSubscriptionLocation(got); err != nil {
						t.Fatalf("framed %q, which the SDK refuses: %v", got, err)
					}
				}
				if got != c.want || framed != (c.want != "") {
					t.Fatalf("subscriptionLocation %q (framed %v), want %q", got, framed, c.want)
				}
				// A create's or an update's 2xx that frames none records why:
				// no Location at all, or one not under the base; nothing else
				// records either.
				var want []string
				if !framed && a.status/100 == 2 && (c.method == http.MethodPost || c.method == http.MethodPut) {
					want = []string{EditNotMadeSubscriptionLocationNotUnderBase}
					if ls := sys.locations; len(ls) == 0 || (len(ls) == 1 && strings.TrimSpace(ls[0]) == "") {
						want = []string{EditNotMadeSubscriptionLocationNoLocation}
					}
				}
				var recorded []string
				for _, e := range p.got.only(t).EditsNotMade {
					if strings.HasPrefix(e, "subscriptionLocation:") {
						recorded = append(recorded, e)
					}
				}
				if !slices.Equal(recorded, want) {
					t.Fatalf("recorded %v, want %v", recorded, want)
				}
			})
		}
	}
}

// An answer that is not a 2xx is carried as sent once it passes one check,
// the body read by its structure and never its label, at every level, on the
// interaction's forward and on the gateway's own read before a delete: FHIR
// JSON passes only as an OperationOutcome holding no resource, FHIR XML only
// as an OperationOutcome with no contained child; any other FHIR, under any
// label, is refused 502 on authority, and HTML and plain text are carried,
// whatever they are labelled.
func TestCDexSubscribe_AnErrorAnswerIsChecked(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`
	const xmlPatient = `<?xml version="1.0" encoding="UTF-8"?><Patient xmlns="http://hl7.org/fhir"><id value="p1"/><name><family value="Doe"/></name></Patient>`
	const xmlOutcome = `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><severity value="error"/><code value="not-found"/></issue></OperationOutcome>`
	for _, level := range allLevels {
		for name, c := range map[string]struct {
			status  int
			ct      string
			body    string
			carried bool
		}{
			"an OperationOutcome":                    {http.StatusNotFound, "application/fhir+json", subNotFound, true},
			"an OperationOutcome holding a resource": {http.StatusNotFound, "application/fhir+json", `{"resourceType":"OperationOutcome","contained":[` + patient + `],"issue":[]}`, false},
			"a Patient on a 404":                     {http.StatusNotFound, "application/fhir+json", patient, false},
			"a Patient with no media type":           {http.StatusInternalServerError, "", patient, false},
			"a proxy's HTML 502":                     {http.StatusBadGateway, "text/html", "<html><body>Bad Gateway</body></html>", true},
			// The body's structure decides, never its label.
			"an XML Patient":                                             {http.StatusNotFound, "application/fhir+xml", xmlPatient, false},
			"an XML Patient labelled text":                               {http.StatusNotFound, "text/plain", xmlPatient, false},
			"a JSON Patient under a malformed label":                     {http.StatusNotFound, "application/fhir+json; charset", patient, false},
			"a JSON Patient labelled text":                               {http.StatusNotFound, "text/plain", patient, false},
			"a JSON Patient labelled HTML":                               {http.StatusInternalServerError, "text/html", patient, false},
			"real HTML labelled FHIR JSON":                               {http.StatusBadGateway, "application/fhir+json", "<!DOCTYPE html><html><body><p>Bad Gateway<br></body></html>", true},
			"real plain text":                                            {http.StatusServiceUnavailable, "text/plain", "upstream down", true},
			"real plain text labelled FHIR XML":                          {http.StatusServiceUnavailable, "application/fhir+xml", "upstream down", true},
			"an XML OperationOutcome":                                    {http.StatusNotFound, "application/fhir+xml", xmlOutcome, true},
			"an XML OperationOutcome labelled JSON":                      {http.StatusNotFound, "application/json", xmlOutcome, true},
			"an XML OperationOutcome holding a resource":                 {http.StatusNotFound, "application/fhir+xml", strings.Replace(xmlOutcome, "<issue>", `<contained><Patient><name><family value="Doe"/></name></Patient></contained><issue>`, 1), false},
			"a UTF-16LE XML Patient":                                     {http.StatusNotFound, "application/fhir+xml", string(utf16Body(xmlPatient, true)), false},
			"a UTF-16BE XML Patient, unlabelled":                         {http.StatusNotFound, "", string(utf16Body(xmlPatient, false)), false},
			"a UTF-16LE XML OperationOutcome":                            {http.StatusNotFound, "application/fhir+xml", string(utf16Body(xmlOutcome, true)), false},
			"UTF-16LE JSON, no byte order mark":                          {http.StatusNotFound, "application/fhir+json", string(utf16Body(patient, true)[2:]), false},
			"UTF-16BE JSON, no byte order mark":                          {http.StatusNotFound, "application/fhir+json", string(utf16Body(patient, false)[2:]), false},
			"UTF-16LE XML, no byte order mark":                           {http.StatusNotFound, "application/fhir+xml", string(utf16Body(xmlPatient, true)[2:]), false},
			"UTF-16BE XML, no byte order mark, labelled text":            {http.StatusNotFound, "text/plain", string(utf16Body(xmlPatient, false)[2:]), false},
			"a DOCTYPE fixing the FHIR namespace":                        {http.StatusNotFound, "text/html", `<!DOCTYPE Patient [<!ATTLIST Patient xmlns CDATA #FIXED "http://hl7.org/fhir">]><Patient><name><family value="Doe"/></name></Patient>`, false},
			"a DOCTYPE declaring an entity":                              {http.StatusNotFound, "application/xml", `<!DOCTYPE Patient [<!ENTITY ns "http://hl7.org/fhir">]><Patient xmlns="&ns;"><name><family value="Doe"/></name></Patient>`, false},
			"an html DOCTYPE page":                                       {http.StatusBadGateway, "text/html", "<!DOCTYPE html>\n<html><head><title>502</title></head><body>Bad Gateway</body></html>", true},
			"a JSON object holding a Patient":                            {http.StatusInternalServerError, "application/json", `{"error":"x","patient":` + patient + `}`, false},
			"a JSON array holding a Patient":                             {http.StatusInternalServerError, "application/json", `[` + patient + `]`, false},
			"a proxy's JSON error":                                       {http.StatusBadGateway, "application/json", `{"error":"bad gateway"}`, true},
			"an XML OperationOutcome holding contained deep":             {http.StatusNotFound, "application/fhir+xml", strings.Replace(xmlOutcome, "<issue>", `<issue><details><contained><Patient><name><family value="Doe"/></name></Patient></contained></details>`, 1), false},
			"a Patient root in another namespace":                        {http.StatusNotFound, "application/xml", `<Patient xmlns="urn:x"><name><family value="Doe"/></name></Patient>`, false},
			"a Patient root in the FHIR namespace with a trailing slash": {http.StatusNotFound, "application/fhir+xml", `<Patient xmlns="http://hl7.org/fhir/"><name><family value="Doe"/></name></Patient>`, false},
			"a Patient root under an unbound prefix":                     {http.StatusNotFound, "text/plain", `<f:Patient><f:name><f:family value="Doe"/></f:name></f:Patient>`, false},
			"a Patient root under a DOCTYPE naming it":                   {http.StatusNotFound, "application/xml", `<!DOCTYPE f:Patient SYSTEM "http://example.org/p.dtd"><f:Patient><f:name><f:family value="Doe"/></f:name></f:Patient>`, false},
			"an XHTML page":                                              {http.StatusBadGateway, "text/html", `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>Bad Gateway</p></body></html>`, true},
			"a SOAP fault":                                               {http.StatusInternalServerError, "text/xml", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><soap:Fault><faultstring>down</faultstring></soap:Fault></soap:Body></soap:Envelope>`, true},
			"a JSON array holding a Patient, then text":                  {http.StatusInternalServerError, "application/json", `[` + patient + `] trailing`, false},
			"bracketed proxy text":                                       {http.StatusBadGateway, "text/plain", "[error] upstream timed out", true},
			// A JSON value holding no resource, then text that is no JSON
			// value, is not FHIR: carried.
			"a JSON array holding none, then text":                   {http.StatusBadGateway, "text/plain", "[1,2] trailing", true},
			"a JSON error, then a PHP warning":                       {http.StatusNotFound, "text/html", `{"error":"not found"}<br />` + "\n" + `<b>Warning</b>:  Cannot modify header information<br />`, true},
			"Drupal's XHTML+RDFa error page":                         {http.StatusNotFound, "text/html", "<!DOCTYPE html PUBLIC \"-//W3C//DTD XHTML+RDFa 1.0//EN\" \"http://www.w3.org/MarkUp/DTD/xhtml-rdfa-1.dtd\">\n<html xmlns=\"http://www.w3.org/1999/xhtml\"><body><p>Page not found<br></p></body></html>", true},
			"an XML error in ISO-8859-2":                             {http.StatusNotFound, "application/xml", "<?xml version=\"1.0\" encoding=\"ISO-8859-2\"?><error><message>Nie znaleziono \xb3</message></error>", true},
			"a JSON array holding none, then one holding a Patient":  {http.StatusInternalServerError, "application/json", `[{}] [` + patient + `]`, false},
			"an XML OperationOutcome holding a Patient element deep": {http.StatusNotFound, "application/fhir+xml", strings.Replace(xmlOutcome, "<issue>", `<issue><details><Patient><name><family value="Doe"/></name></Patient></details>`, 1), false},
			"a Patient root in no namespace":                         {http.StatusNotFound, "application/xml", `<Patient><name><family value="Doe"/></name></Patient>`, false},
			"a Patient root in no namespace, labelled text":          {http.StatusNotFound, "text/plain", `<Patient><name><family value="Doe"/></name></Patient>`, false},
			"an html root in no namespace":                           {http.StatusBadGateway, "text/html", `<html><body>Bad Gateway</body></html>`, true},
			"a patient root in lower case in no namespace":           {http.StatusNotFound, "application/xml", `<patient><name>x</name></patient>`, true},
			"a Patient root in the FHIR namespace":                   {http.StatusNotFound, "application/fhir+xml", `<Patient xmlns="http://hl7.org/fhir"><name><family value="Doe"/></name></Patient>`, false},
			"an XML OperationOutcome holding one, unlabelled":        {http.StatusNotFound, "", strings.Replace(xmlOutcome, "<issue>", `<contained><Patient><name><family value="Doe"/></name></Patient></contained><issue>`, 1), false},
		} {
			for _, request := range []string{"GET Subscription/sub-1", "DELETE Subscription/sub-1"} {
				t.Run(level.String()+"/"+name+"/"+request, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					sys.answer = func(r *http.Request) (readAnswer, bool) {
						return readAnswer{status: c.status, header: map[string]string{"Content-Type": c.ct}, body: []byte(c.body)}, true
					}
					a := p.subscribe(t, subFrame{request: request})
					if c.carried {
						if a.status != c.status || string(a.body) != c.body {
							t.Fatalf("answer %d %s, want %d carried as sent", a.status, a.body, c.status)
						}
						return
					}
					// The refused answer is the gateway's fixed OperationOutcome: its reason echoes only the answer's resource type, member names and FHIR type names, never a minted value, so "Doe" cannot match one by chance.
					if a.status != http.StatusBadGateway || bytes.Contains(a.body, []byte("Doe")) {
						t.Fatalf("answer %d %s, want 502 with nothing of the answer", a.status, a.body)
					}
					wantRefusal(t, p.got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalAuthority)
				})
			}
		}
	}
}

// Every refusal on the leg names its primitive, at every level, with nothing
// sent to the provider's system: the rejection rows.
func TestCDexSubscribe_RefusalsByPrimitive(t *testing.T) {
	for name, c := range map[string]struct {
		frame    func(t *testing.T) subFrame
		status   int
		rule     string
		msg      string
		notMade  string
		noScope  bool
		fidelity bool
	}{
		"not taken": {func(t *testing.T) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) }, http.StatusUnprocessableEntity, RefusalRouting, refusalCDexSubscribeNotTaken, "", true, false},
		"no subscriptionRequest": {func(t *testing.T) subFrame { f := createFrame(t, cdexSubscriptionJSON(nil)); f.request = ""; return f },
			http.StatusBadRequest, RefusalRouting, refusalCDexSubscribeNoRequest, "", false, false},
		"an id that is not a FHIR id": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/a_b"} }, http.StatusBadRequest, RefusalRouting, "not a FHIR id", "", false, false},
		// The interactions the leg never carries, by their own rules: a PATCH on fidelity,
		// history and type-level $status on
		// authority; a type-level history is no interaction at all.
		"PATCH":                          {func(*testing.T) subFrame { return subFrame{request: "PATCH Subscription/sub-1"} }, http.StatusMethodNotAllowed, RefusalFidelity, "PATCH is not carried", "", false, true},
		"PATCH at the type":              {func(*testing.T) subFrame { return subFrame{request: "PATCH Subscription"} }, http.StatusMethodNotAllowed, RefusalFidelity, "PATCH is not carried", "", false, true},
		"instance _history":              {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1/_history"} }, http.StatusForbidden, RefusalAuthority, "history is not carried", "", false, false},
		"instance _history with a query": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1/_history?_count=1"} }, http.StatusForbidden, RefusalAuthority, "history is not carried", "", false, false},
		"type-level $status":             {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/$status"} }, http.StatusForbidden, RefusalAuthority, "type-level $status is not carried", "", false, false},
		"type-level _history":            {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/_history"} }, http.StatusBadRequest, RefusalRouting, refusalCDexSubscribeRequest, "", false, false},
		// The search's endpoint travels only sealed: a search with no
		// subscriptionEndpoint names none, and is refused on authority.
		"a search with no subscriptionEndpoint": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription?status=active"} },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSearchNoSealedEndpoint, false, false},
		"an empty search with no subscriptionEndpoint": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription?"} },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSearchNoSealedEndpoint, false, false},
		"a search framed with a url": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.request = "GET Subscription?url=" + url.QueryEscape(payerEndpoint)
			return f
		}, http.StatusForbidden, RefusalAuthority, "never carries url", "", false, false},
		"a search with _summary": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.request += "&_summary=count"
			return f
		}, http.StatusForbidden, RefusalAuthority, "_summary", "", false, false},
		"a search whose _elements leaves out channel": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.request += "&_elements=status"
			return f
		}, http.StatusForbidden, RefusalAuthority, "must keep channel", "", false, false},
		"a search modifier": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.request = "GET Subscription?status:not=off"
			return f
		},
			http.StatusForbidden, RefusalAuthority, "modifier", "", false, false},
		"a read with an unknown parameter": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1?_format=json"} },
			http.StatusBadRequest, RefusalRouting, refusalCDexSubscribeRequest, "", false, false},
		"a vread with an unknown parameter": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1/_history/2?_format=json"} },
			http.StatusBadRequest, RefusalRouting, refusalCDexSubscribeRequest, "", false, false},
		"a read with _summary twice": {func(*testing.T) subFrame {
			return subFrame{request: "GET Subscription/sub-1?_summary=true&_summary=false"}
		},
			http.StatusBadRequest, RefusalRouting, refusalCDexSubscribeRequest, "", false, false},
		"a page whose _elements drops channel": {func(*testing.T) subFrame { return subFrame{request: "GET ?_getpages=abc&_elements=status"} },
			http.StatusForbidden, RefusalAuthority, "channel", "", false, false},
		"an If-Match on a read": {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1", ifMatch: `W/"1"`} },
			http.StatusBadRequest, RefusalIntegrity, refusalCDexSubscribeIfMatch, "", false, false},
		"an If-Match on a create": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.ifMatch = `W/"1"`
			return f
		},
			http.StatusBadRequest, RefusalIntegrity, refusalCDexSubscribeIfMatch, "", false, false},
		"an If-Match on a search": {func(t *testing.T) subFrame { f := searchFrame(t, payerEndpoint); f.ifMatch = `W/"1"`; return f },
			http.StatusBadRequest, RefusalIntegrity, refusalCDexSubscribeIfMatch, "", false, false},
		"a search without a sealed endpoint": {func(t *testing.T) subFrame { f := searchFrame(t, payerEndpoint); f.endpoint = ""; return f },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSearchNoSealedEndpoint, false, false},
		"a search with three sealed endpoints": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.endpoint += " " + f.endpoint + " " + f.endpoint
			return f
		},
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSearchNoSealedEndpoint, false, false},
		"a search with a value that is not sealed": {func(t *testing.T) subFrame { f := searchFrame(t, payerEndpoint); f.endpoint = "not-sealed"; return f },
			http.StatusForbidden, RefusalAuthority, "a sealed endpoint that is not one", EditNotMadeSearchNotSealedValue, false, false},
		"a page query outside its form": {func(*testing.T) subFrame { return subFrame{request: "GET ?_getpages=abc&_format=xml"} }, http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeRequest, "", false, false},
		"a read with a body":            {func(*testing.T) subFrame { return subFrame{request: "GET Subscription/sub-1", body: []byte(`{}`)} }, http.StatusBadRequest, RefusalIntegrity, refusalCDexSubscribeBody, "", false, false},
		"full-resource content": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"valueCode":"id-only"`, `"valueCode":"full-resource"`, 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "full-resource", "", false, false},
		"another topic": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, "http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task", "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic", 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "criteria", "", false, false},
		"no requester filter": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, "Task?requester:identifier=http://example.org/cdex/payer/payer-ids|Payer123&status=completed", "Task?status=completed", 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "requester:identifier", "", false, false},
		"another payer's identifier": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string { return strings.Replace(s, "|Payer123", "|Payer999", 1) }))
		}, http.StatusForbidden, RefusalAuthority, "not attested", "", false, false},
		"a filter the topic does not define": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string { return strings.Replace(s, "&status=completed", "&patient=Patient/1", 1) }))
		}, http.StatusForbidden, RefusalAuthority, "does not define", "", false, false},
		"no sealed endpoint": {func(t *testing.T) subFrame { f := createFrame(t, cdexSubscriptionJSON(nil)); f.endpoint = ""; return f },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSubscriptionNoSealedEndpoint, false, false},
		"two sealed endpoints on a create": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.endpoint += " " + f.endpoint
			return f
		},
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedEndpoint, EditNotMadeSubscriptionNoSealedEndpoint, false, false},
		"no sealed headers": {func(t *testing.T) subFrame { f := createFrame(t, cdexSubscriptionJSON(nil)); f.channel = ""; return f },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNoSealedHeaders, EditNotMadeSubscriptionNoSealedHeaders, false, false},
		"a malformed sealed endpoint": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.endpoint = "kid.!!"
			return f
		},
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNotSealed, EditNotMadeSubscriptionNotSealedValues, false, false},
		// Another payer's notification address is not a sealed value: framed
		// as one, it is refused, never written.
		"another payer's notification address as the sealed endpoint": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.endpoint = foreignAddress(t)
			return f
		},
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNotSealed, EditNotMadeSubscriptionNotSealedValues, false, false},
		"another payer's notification address as a search's sealed endpoint": {func(t *testing.T) subFrame {
			f := searchFrame(t, payerEndpoint)
			f.endpoint = foreignAddress(t)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "a sealed endpoint that is not one", EditNotMadeSearchNotSealedValue, false, false},
		"malformed sealed headers": {func(t *testing.T) subFrame { f := createFrame(t, cdexSubscriptionJSON(nil)); f.channel = "x"; return f },
			http.StatusForbidden, RefusalAuthority, refusalCDexSubscribeNotSealed, EditNotMadeSubscriptionNotSealedValues, false, false},
		// The body is read by its structure, never its label: FHIR XML is the
		// unreadable Subscription under any label, another resource the leg's
		// authority, and text the fidelity of the gateway's own edit.
		"an XML Subscription": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.contentType, f.body = "application/fhir+xml", []byte(`<Subscription xmlns="http://hl7.org/fhir"><status value="requested"/></Subscription>`)
			return f
		},
			http.StatusUnsupportedMediaType, RefusalFidelity, "FHIR JSON", EditNotMadeSubscriptionUnreadable, false, true},
		"an XML Subscription labelled FHIR JSON": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = []byte(`<Subscription xmlns="http://hl7.org/fhir"><status value="requested"/></Subscription>`)
			return f
		},
			http.StatusUnsupportedMediaType, RefusalFidelity, "FHIR JSON", EditNotMadeSubscriptionUnreadable, false, true},
		"plain text": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.contentType, f.body = "text/plain", []byte("a Subscription")
			return f
		},
			http.StatusUnsupportedMediaType, RefusalFidelity, "FHIR JSON", EditNotMadeSubscriptionUnreadable, false, true},
		"a UTF-16 JSON Subscription": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = utf16Body(string(f.body), true)
			return f
		},
			http.StatusBadRequest, RefusalFidelity, "unambiguously", EditNotMadeSubscriptionUnreadable, false, true},
		"a JSON Patient": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = []byte(`{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		"a JSON Patient labelled text": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.contentType, f.body = "text/plain", []byte(`{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		"a UTF-16BE JSON Subscription, no byte order mark": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = utf16Body(string(f.body), false)[2:]
			return f
		},
			http.StatusBadRequest, RefusalFidelity, "unambiguously", EditNotMadeSubscriptionUnreadable, false, true},
		"an XML Patient": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.contentType, f.body = "application/fhir+xml", []byte(`<Patient xmlns="http://hl7.org/fhir"><name><family value="Doe"/></name></Patient>`)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		"a JSON object holding a Patient": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = []byte(`{"note":"x","patient":{"resourceType":"Patient","name":[{"family":"Doe"}]}}`)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		"a JSON array holding the Subscription": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = append(append([]byte("["), f.body...), ']')
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		"a JSON Bundle": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = append(append([]byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":`), f.body...), []byte(`}]}`)...)
			return f
		},
			http.StatusForbidden, RefusalAuthority, "other than a Subscription", "", false, false},
		// The leg names no patient on either side: a Subscription holding a
		// resource, of any type, is refused on authority.
		"a contained Patient": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"status":"requested",`, `"status":"requested","contained":[{"resourceType":"Patient","id":"p1"}],`, 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "holds a resource", "", false, false},
		"a contained Organization": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"status":"requested",`, `"status":"requested","contained":[{"resourceType":"Organization","id":"o1"}],`, 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "holds a resource", "", false, false},
		"a resource-valued member": {func(t *testing.T) subFrame {
			return createFrame(t, cdexSubscriptionJSON(func(s string) string {
				return strings.Replace(s, `"status":"requested",`, `"status":"requested","note":{"resourceType":"Patient","id":"p1"},`, 1)
			}))
		}, http.StatusForbidden, RefusalAuthority, "holds a resource", "", false, false},
		// The payer's gateway sends no endpoint (E-18); a body that states
		// one anyway is read before E-19 writes over it.
		"an endpoint that is not a string": {func(t *testing.T) subFrame {
			f := endpointStatedFrame(t, cdexSubscriptionJSON(nil), payerEndpoint)
			f.body = bytes.Replace(f.body, []byte(`"endpoint":"`+payerEndpoint+`"`), []byte(`"endpoint":7`), 1)
			return f
		}, http.StatusBadRequest, RefusalFidelity, "re-key unambiguously", EditNotMadeSubscriptionUnreadable, false, true},
		"an endpoint in another case": {func(t *testing.T) subFrame {
			f := endpointStatedFrame(t, cdexSubscriptionJSON(nil), payerEndpoint)
			f.body = bytes.Replace(f.body, []byte(`"endpoint":"`), []byte(`"Endpoint":"`), 1)
			return f
		}, http.StatusBadRequest, RefusalFidelity, "re-key unambiguously", EditNotMadeSubscriptionUnreadable, false, true},
		"not one JSON object": {func(t *testing.T) subFrame {
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.body = []byte(`{"resourceType":"Subscription","id":"a","ID":"b"}`)
			return f
		}, http.StatusBadRequest, RefusalFidelity, "unambiguously", EditNotMadeSubscriptionUnreadable, false, true},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				sys := newSubStore(t)
				var opts []func(*Config)
				if c.noScope {
					opts = append(opts, func(cfg *Config) { cfg.ProviderCDexSubscriptionScope, cfg.ProviderCDexSubscriptionClient = "", nil })
				}
				p := newSubProvider(t, level, sys, opts...)
				a := p.subscribe(t, c.frame(t))
				if a.status != c.status || !strings.Contains(string(a.body), c.msg) {
					t.Fatalf("answer %d %s, want %d %q", a.status, a.body, c.status, c.msg)
				}
				if got := sys.calls(); len(got) != 0 {
					t.Fatalf("the provider's system got %+v, want nothing", got)
				}
				rec := p.got.only(t)
				if c.fidelity {
					if rec.Outcome != ExchangeRefused || rec.Rule != RefusalFidelity || rec.RefusalStage == "" || rec.Status != c.status {
						t.Fatalf("record %s %s stage %q %d, want refused on fidelity with its stage", rec.Outcome, rec.Rule, rec.RefusalStage, rec.Status)
					}
				} else {
					wantRefusal(t, rec, c.status, RefusedByProviderGateway, c.rule)
				}
				if c.notMade != "" && !slices.Equal(rec.EditsNotMade, []string{c.notMade}) {
					t.Fatalf("edits not made %v, want [%s]", rec.EditsNotMade, c.notMade)
				}
			})
		}
	}
}

// The subscription headers are defined on the subscribe leg alone: framed on
// the payer's read of its Task, they are refused 400, on message integrity.
func TestCDexSubscribe_TheHeadersOnAnotherLegAreRefused(t *testing.T) {
	for _, level := range allLevels {
		for _, h := range []string{shnsdk.FrameHeaderSubscriptionRequest, shnsdk.FrameHeaderSubscriptionEndpoint, shnsdk.FrameHeaderSubscriptionChannel, shnsdk.FrameHeaderIfMatch, shnsdk.FrameHeaderPrefer} {
			t.Run(level.String()+"/"+h, func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys, func(c *Config) { c.ProviderCDexReadScope, c.ProviderCDexReadClient = "system/Task.r", sys.srv.Client() })
				f := subFrame{leg: legCDexTaskStatus}
				switch h {
				case shnsdk.FrameHeaderSubscriptionRequest:
					f.request = "GET Subscription/sub-1"
				case shnsdk.FrameHeaderSubscriptionEndpoint:
					f.endpoint = "a.b"
				case shnsdk.FrameHeaderIfMatch:
					f.ifMatch = `W/"1"`
				case shnsdk.FrameHeaderPrefer:
					f.prefer = "return=minimal"
				default:
					f.channel = "a.b"
				}
				a := p.subscribe(t, f)
				if a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "not defined for this transaction type") {
					t.Fatalf("answer %d %s, want 400", a.status, a.body)
				}
				wantRefusal(t, p.got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalIntegrity)
			})
		}
	}
}

// A gateway that could not serve the leg refuses to start rather than
// declare it, and one that can declares it.
func TestNew_TheSubscriptionScopeNeedsWhatTheLegUses(t *testing.T) {
	for name, c := range map[string]struct {
		mut  func(c *Config)
		want string
	}{
		"a payer":          {func(c *Config) { c.Role = "payer" }, "role"},
		"no Task endpoint": {func(c *Config) { c.ProviderCDexTaskURL = "" }, "requires ProviderCDexTaskURL"},
		"no ingress base":  {func(c *Config) { c.IngressBaseURL = "" }, "ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE) requires the Da Vinci ingress"},
		"no ingress":       {func(c *Config) { c.IngressEnabled = false }, "ProviderCDexSubscriptionScope (PROVIDER_CDEX_SUBSCRIPTION_SCOPE) requires the Da Vinci ingress"},
		"no client":        {func(c *Config) { c.ProviderCDexSubscriptionClient = nil }, "ProviderCDexSubscriptionClient"},
		"an identity of another holder": {func(c *Config) { c.Identity.HolderID = "provider-other" }, `the identity names holder "provider-other", and this gateway is holder "provider-x": ` +
			`the notification keys are derived under one holder: Config.Identity.HolderID and Config.HolderID must name the same holder; the gateway app sets both from its registered bundle`},
		"a Task endpoint with a query": {func(c *Config) { c.ProviderCDexTaskURL = "https://ehr.example/fhir/Task?x=1" }, "PROVIDER_CDEX_SUBSCRIPTION_SCOPE"},
		"a Task endpoint not Task":     {func(c *Config) { c.ProviderCDexTaskURL = "https://ehr.example/fhir/submit" }, "PROVIDER_CDEX_SUBSCRIPTION_SCOPE"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := subscriptionScopeConfig(t, newSubStore(t))
			c.mut(&cfg)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New: %v, want an error containing %q", err, c.want)
			}
		})
	}
	cfg := Config{Role: "provider", ProviderCDexTaskURL: "https://ehr.example/fhir/Task", ProviderCDexSubscriptionScope: cdexSubscriptionScope}
	if !slices.Contains(cfg.TakenLegs(), legCDexTaskSubscribe) {
		t.Fatalf("TakenLegs %v, want the subscribe leg with the scope set", cfg.TakenLegs())
	}
	cfg.ProviderCDexSubscriptionScope = "  "
	if slices.Contains(cfg.TakenLegs(), legCDexTaskSubscribe) {
		t.Fatalf("TakenLegs %v, want no subscribe leg with a blank scope", cfg.TakenLegs())
	}
}

// subscriptionScopeConfig is a provider gateway's Config that takes the
// subscribe leg, which New accepts.
func subscriptionScopeConfig(t *testing.T, sys *subStore) Config {
	t.Helper()
	_, signPriv := genED25519(t)
	encPub, encPriv := genKeyPair(t)
	cfg := Config{Role: "provider", HolderID: "provider-x", Identity: shnsdk.Identity{HolderID: "provider-x", SignPriv: signPriv, EncPub: encPub, EncPriv: encPriv},
		AuthzURL: "http://stub.test", Reg: shnsdk.NewRegistry(), Validator: shnsdk.NewFakeValidator(), SoR: newCensusSoR(), Store: newCensusSoR(),
		ProviderCDexTaskURL: sys.taskURL(), ProviderCDexTaskClient: sys.srv.Client(), ProviderCDexSubscriptionScope: cdexSubscriptionScope,
		ProviderCDexSubscriptionClient: sys.srv.Client(), IngressBaseURL: cdexIngressBase}
	EnableIngressForTest(&cfg)
	pub, _ := genED25519(t)
	cfg.HubTransportPub = pub
	cfg.AuthzPub, _ = genED25519(t)
	return cfg
}

// The holder check is the subscription scope's alone: side by side, the same
// Config with the identity naming another holder than HolderID is refused at
// New with the scope set and starts without it, and the control, one holder
// with the scope, starts.
func TestNew_TheHolderCheckIsTheSubscriptionScopes(t *testing.T) {
	for name, c := range map[string]struct {
		mismatch, scope bool
		refused         bool
	}{
		"one holder, with the scope":        {false, true, false},
		"another holder, with the scope":    {true, true, true},
		"another holder, without the scope": {true, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := subscriptionScopeConfig(t, newSubStore(t))
			if c.mismatch {
				cfg.Identity.HolderID = "provider-other"
			}
			if !c.scope {
				cfg.ProviderCDexSubscriptionScope = ""
			}
			_, err := New(cfg)
			if c.refused != (err != nil) || (err != nil && !strings.Contains(err.Error(), "Config.Identity.HolderID and Config.HolderID must name the same holder")) {
				t.Fatalf("New: %v, want refused %v by the holder check", err, c.refused)
			}
		})
	}
}

// subscriptionTarget builds below the base as the parser admitted the path,
// and the base's own escaping is kept.
func TestSubscriptionTarget(t *testing.T) {
	base, _ := url.Parse("https://ehr.example/fhir%2Fx/r4")
	for _, c := range []struct {
		path, query string
		hasQuery    bool
		want        string
	}{
		{"Subscription", "", false, "https://ehr.example/fhir%2Fx/r4/Subscription"},
		{"Subscription/a.b/$status", "", false, "https://ehr.example/fhir%2Fx/r4/Subscription/a.b/$status"},
		{"Subscription", "url=a%2Cb&status=active", true, "https://ehr.example/fhir%2Fx/r4/Subscription?url=a%2Cb&status=active"},
		{"", "_getpages=x", true, "https://ehr.example/fhir%2Fx/r4?_getpages=x"},
	} {
		got, err := subscriptionTarget(base, c.path, c.query, c.hasQuery)
		if err != nil || got.String() != c.want {
			t.Errorf("%q: %v %v, want %s", c.path, got, err, c.want)
		}
	}
}

var _ = relay.EditSubscriptionEndpointRekey

// last returns the last record and clears them.
func (c *exchangeRecords) last(t *testing.T) ExchangeRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) == 0 {
		t.Fatal("no exchange record")
	}
	rec := c.recs[len(c.recs)-1]
	c.recs = nil
	wantClosed(t, rec)
	return rec
}

// utf16Body is s encoded as UTF-16 with its byte order mark, little-endian
// or big-endian.
func utf16Body(s string, little bool) []byte {
	out := []byte{0xFE, 0xFF}
	if little {
		out = []byte{0xFF, 0xFE}
	}
	for _, r := range s {
		if little {
			out = append(out, byte(r), byte(r>>8))
		} else {
			out = append(out, byte(r>>8), byte(r))
		}
	}
	return out
}
