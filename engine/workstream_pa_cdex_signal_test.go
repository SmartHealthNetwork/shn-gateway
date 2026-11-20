//go:build !shn_coreonly

package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const (
	signalProviderBase = "https://provider.example/fhir"
	signalPayerIngress = "https://payer-gw.example"
	signalPayerBase    = "https://payer-gw.example/cdex/provider/provider"
)

// signalEvent is an id-only event notification naming Task t1, with focus as
// its focus reference and the rest of its addresses under
// signalProviderBase.
func signalEvent(focus string) string {
	return `{"resourceType":"Bundle","type":"history","entry":[` +
		`{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"` + signalProviderBase + `/Subscription/s1"}},` +
		`{"name":"topic","valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"},` +
		`{"name":"type","valueCode":"event-notification"},` +
		`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},` +
		`{"name":"focus","valueReference":{"reference":"` + focus + `"}}]}]},` +
		`"request":{"method":"GET","url":"` + signalProviderBase + `/Subscription/s1/$status?x=1"}},` +
		`{"fullUrl":"` + signalProviderBase + `/Task/t1","request":{"method":"PUT","url":"Task/t1"}}]}`
}

// delivered is E-17's delivery of body as a payer gateway whose ingress base
// is ingress makes it for the notifier "provider", with the base stated (or
// none when stated is "-"): the bytes delivered, the edits made, and the
// not-made reasons recorded.
func delivered(t *testing.T, ingress, stated, body string) (string, []relay.EditID, []string) {
	t.Helper()
	g := &Gateway{cfg: Config{IngressBaseURL: ingress}}
	x := &exchangeRecorder{}
	in := deliveryInput{notifier: "provider", statedBase: stated, hasStatedBase: stated != "-"}
	if stated == "-" {
		in.statedBase = ""
	}
	p, err := g.deliveredNotification(x, legCDexTaskSignal, []byte(body), "application/fhir+json", in)
	if err != nil {
		t.Fatal(err)
	}
	return string(relay.BytesForTest(p)), p.Edits(), x.rec.EditsNotMade
}

// E-17 decides by the base the provider's gateway framed, by the
// segment-boundary match, never by text: every absolute address in its places
// under that base is re-keyed to the payer gateway's base for the provider,
// its query kept; a sibling path that merely shares the base's spelling,
// another host, a fragment, and an address in no place of E-17's, are carried
// as sent, each recorded once where it is an address in E-17's places; a
// relative reference and a urn:uuid are no address into the server, carried
// and not recorded.
func TestCDexSignal_E17DecidesByTheFramedBase(t *testing.T) {
	under := signalEvent(signalProviderBase + "/Task/t1")
	all := strings.ReplaceAll(under, signalProviderBase, signalPayerBase)
	for name, row := range map[string]struct {
		body, want string
		notMade    []string
	}{
		"every address under the base": {under, all, nil},
		"a sibling path that shares the base's spelling": {signalEvent("https://provider.example/fhirX/Task/t1"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", "https://provider.example/fhirX/Task/t1\"}}]}", 1), []string{EditNotMadeNotificationNotUnderBase}},
		"another host": {signalEvent("https://other.example/fhir/Task/t1"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", "https://other.example/fhir/Task/t1\"}}]}", 1), []string{EditNotMadeNotificationNotUnderBase}},
		"another scheme": {signalEvent("http://provider.example/fhir/Task/t1"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", "http://provider.example/fhir/Task/t1\"}}]}", 1), []string{EditNotMadeNotificationNotUnderBase}},
		"a fragment": {signalEvent(signalProviderBase + "/Task/t1#x"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", signalProviderBase+"/Task/t1#x\"}}]}", 1), []string{EditNotMadeNotificationNotUnderBase}},
		"a relative focus": {signalEvent("Task/t1"), strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", "Task/t1\"}}]}", 1), nil},
		"a dot segment above the base": {signalEvent(fmt.Sprintf("%s/../provider-y/Task/t1", signalProviderBase)),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", fmt.Sprintf("%s/../provider-y/Task/t1\"}}]}", signalProviderBase), 1), []string{EditNotMadeNotificationNotUnderBase}},
		"a percent-encoded dot segment above the base": {signalEvent(signalProviderBase + "/%2e%2E/provider-y/Task/t1"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", signalProviderBase+"/%2e%2E/provider-y/Task/t1\"}}]}", 1), []string{EditNotMadeNotificationNotUnderBase}},
		"a percent-encoded escaped path is carried as written": {signalEvent(signalProviderBase + "/Task/t%2D1"),
			strings.Replace(all, signalPayerBase+"/Task/t1\"}}]}", signalPayerBase+"/Task/t%2D1\"}}]}", 1), nil},
		"a member named in another case": {strings.Replace(under, `{"fullUrl":"`+signalProviderBase+`/Task/t1"`, `{"FullUrl":"`+signalProviderBase+`/Task/t1"`, 1),
			strings.Replace(all, `{"fullUrl":"`+signalPayerBase+`/Task/t1"`, `{"FullUrl":"`+signalPayerBase+`/Task/t1"`, 1), nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, edits, notMade := delivered(t, signalPayerIngress, signalProviderBase, row.body)
			if got != row.want {
				t.Fatalf("delivered\n%s\nwant\n%s", got, row.want)
			}
			if !slices.Equal(edits, []relay.EditID{relay.EditCDexTaskNotificationAddressRekey}) {
				t.Fatalf("edits %v", edits)
			}
			if !slices.Equal(notMade, row.notMade) {
				t.Fatalf("not made %v, want %v", notMade, row.notMade)
			}
		})
	}
	// The urn:uuid fullUrl and the relative request.url are carried.
	got, _, _ := delivered(t, signalPayerIngress, signalProviderBase, under)
	if !strings.Contains(got, `"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b"`) || !strings.Contains(got, `"url":"Task/t1"`) {
		t.Fatalf("a urn:uuid or relative address was changed: %s", got)
	}
}

// What E-17 cannot decide is carried as sent and recorded, never guessed: a
// signal request with no stated base (recorded for every such notification,
// one with no address in it too), a payer gateway with no base of its own for
// the provider, and a notification relay cannot read one way only.
func TestCDexSignal_E17NotMade(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	handshake := `{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},{"name":"type","valueCode":"handshake"}]}}]}`
	for name, row := range map[string]struct {
		ingress, stated, body string
		notMade               []string
	}{
		"no stated base":                {signalPayerIngress, "-", event, []string{EditNotMadeNotificationNoStatedBase}},
		"an empty stated base":          {signalPayerIngress, "", event, []string{EditNotMadeNotificationNoStatedBase}},
		"no stated base, no address":    {signalPayerIngress, "-", handshake, []string{EditNotMadeNotificationNoStatedBase}},
		"no base for the provider":      {"", signalProviderBase, event, []string{EditNotMadeNotificationNoBase}},
		"a stated base that is no base": {signalPayerIngress, "not a url", event, []string{EditNotMadeNotificationNotUnderBase}},
	} {
		t.Run(name, func(t *testing.T) {
			got, edits, notMade := delivered(t, row.ingress, row.stated, row.body)
			if got != row.body || len(edits) != 0 {
				t.Fatalf("delivered %s with %v, want it as sent", got, edits)
			}
			if !slices.Equal(notMade, row.notMade) {
				t.Fatalf("not made %v, want %v", notMade, row.notMade)
			}
		})
	}
}

// The leg's registered edits on the recipient's request are each made by the
// delivery, through relay, and disclosed. This is a TEMPORARY stand-in until
// the PAS notified-return re-key (E-22) lands on pas-subscription-notify: a
// registered edit (E-11's id, its row re-registered on cdex-task-signal for
// this row only, its maker replacing the Bundle's type) is made beside E-17
// by deliveredNotification, through relay.ApplyChanges, and named in
// X-SHN-Edited by setEditedHeader, as the delivery writes it. What it does
// not prove: relay's transmit ownership check for that edit on the leg (the
// ownership table is registered once, when the engine loads, and takes no
// row from a test), so the payload is never sent through sendVia here. A
// registered edit with no maker is this gateway's own fault: nothing is
// delivered.
func TestCDexSignal_TheLegsRegisteredEditsAreMadeAndDisclosed(t *testing.T) {
	savedRows, savedMakers := relayEdits, deliveryEdits
	t.Cleanup(func() { relayEdits, deliveryEdits = savedRows, savedMakers })
	var standIn relay.Edit
	for _, e := range relayEdits {
		if e.ID == relay.EditCDexTaskReturnRekey {
			standIn = e
		}
	}
	standIn.Legs = []string{legCDexTaskSignal}
	relayEdits = append(slices.Clone(savedRows), standIn)
	deliveryEdits = map[relay.EditID]deliveryEdit{
		relay.EditCDexTaskNotificationAddressRekey: savedMakers[relay.EditCDexTaskNotificationAddressRekey],
		relay.EditCDexTaskReturnRekey: func(_ *Gateway, _ *exchangeRecorder, d *relay.Document, _ deliveryInput) ([]relay.Op, error) {
			n, ok := d.Member(d.Root(), "type")
			if !ok {
				t.Fatal("no type")
			}
			return []relay.Op{d.Replace(n, []byte(`"stand-in"`))}, nil
		},
	}
	got, edits, _ := delivered(t, signalPayerIngress, signalProviderBase, signalEvent(signalProviderBase+"/Task/t1"))
	if !strings.Contains(got, `"type":"stand-in"`) || !strings.Contains(got, signalPayerBase+"/Task/t1") {
		t.Fatalf("delivered %s, want both edits made", got)
	}
	if !slices.Contains(edits, relay.EditCDexTaskReturnRekey) || !slices.Contains(edits, relay.EditCDexTaskNotificationAddressRekey) {
		t.Fatalf("edits %v", edits)
	}
	// The delivery's disclosure names both, as sendVia writes it.
	g := &Gateway{cfg: Config{IngressBaseURL: signalPayerIngress}}
	p, err := g.deliveredNotification(&exchangeRecorder{}, legCDexTaskSignal, []byte(signalEvent(signalProviderBase+"/Task/t1")), "application/fhir+json",
		deliveryInput{notifier: "provider", statedBase: signalProviderBase, hasStatedBase: true})
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	setEditedHeader(h, p)
	if got := h.Values(EditedHeader); !slices.Equal(got, []string{"cdex-task-return-rekey", "cdex-task-notification-address-rekey"}) {
		t.Fatalf("X-SHN-Edited %q", got)
	}
	// Rejection row: a registered edit with no maker delivers nothing.
	delete(deliveryEdits, relay.EditCDexTaskReturnRekey)
	if _, err := g.deliveredNotification(&exchangeRecorder{}, legCDexTaskSignal, []byte(`{}`), "application/fhir+json", deliveryInput{}); err == nil {
		t.Fatal("a registered edit with no maker: delivered")
	}
}

// Every registered edit a recipient makes on the request of a topic's signal
// leg has its maker, so the delivery makes each one the registry declares;
// and every maker's edit is one the delivery discloses (headerDisclosedEdits),
// so no edit of a notification reaches the subscriber's endpoint unnamed.
func TestDeliveryEdits_EveryRegisteredEditHasAMaker(t *testing.T) {
	var signals int
	for leg, spec := range paCatalog {
		if !spec.Signal {
			continue
		}
		signals++
		for _, id := range deliveryEditIDs(leg) {
			if _, ok := deliveryEdits[id]; !ok {
				t.Errorf("%s: registered edit %s has no maker", leg, id)
			}
		}
	}
	if signals == 0 || !slices.Equal(deliveryEditIDs(legCDexTaskSignal), []relay.EditID{relay.EditCDexTaskNotificationAddressRekey}) {
		t.Fatalf("%d signal legs; cdex-task-signal's edits %v, want E-17", signals, deliveryEditIDs(legCDexTaskSignal))
	}
	if got := deliveryEditIDs(legPASSubscriptionNotify); !slices.Equal(got, []relay.EditID{relay.EditPASNotifiedReturnRekey}) {
		t.Fatalf("pas-subscription-notify's edits %v, want E-22", got)
	}
	if got := deliveryEditIDs(legPASSubscriptionSignal); len(got) != 0 {
		t.Fatalf("pas-subscription-signal's edits %v, want none", got)
	}
	for id := range deliveryEdits {
		if !slices.Contains(headerDisclosedEdits, id) {
			t.Errorf("maker %s's edit is not disclosed in %s", id, EditedHeader)
		}
		var registered bool
		for _, e := range relayEdits {
			registered = registered || (e.ID == id && e.Role == relay.RoleRecipient && e.Direction == relay.DirectionRequest)
		}
		if !registered {
			t.Errorf("maker %s is no registered recipient request edit", id)
		}
	}
}

// The subscriber's header values are presented as HTTP fields, split at the
// first colon, the value trimmed; one that cannot be is refused, never sent
// altered.
func TestDeliveryHeaders(t *testing.T) {
	h, ok := deliveryHeaders([]string{"Authorization: Bearer s3cret", "X-Tenant:a", "X-Tenant:  b\t", "X-Url: https://x.example:8443/a"})
	if !ok || h.Get("Authorization") != "Bearer s3cret" || !slices.Equal(h.Values("X-Tenant"), []string{"a", "b"}) || h.Get("X-Url") != "https://x.example:8443/a" {
		t.Fatalf("%v %v", h, ok)
	}
	if h, ok := deliveryHeaders(nil); !ok || len(h) != 0 {
		t.Fatalf("no values: %v %v", h, ok)
	}
	for _, bad := range []string{"no colon", ": no name", "Bad Name: x", "X: a\r\nInjected: y", "X: a\x00b"} {
		if _, ok := deliveryHeaders([]string{"Authorization: Bearer s3cret", bad}); ok {
			t.Errorf("%q: presented", bad)
		}
	}
}

// A request frame carries a notification's sealed endpoint and headers and
// the notifier's stated base on a signal leg, the interaction and If-Match on
// a subscribe leg alone, and the stated base on no other leg's request: a
// frame naming one where the leg defines none is refused 400.
func TestInboundFrameSubscription_TheSignalLegsHeaders(t *testing.T) {
	frame := func(h map[string]string) []byte {
		b, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, h, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	signal := map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: "e", shnsdk.FrameHeaderSubscriptionChannel: "c", shnsdk.FrameHeaderStatedBase: "https://p.example/fhir"}
	f, status, _ := inboundFrameSubscription(legCDexTaskSignal, frame(signal))
	if status != 0 || f.endpoint != "e" || f.channel != "c" || f.statedBase != "https://p.example/fhir" || !f.hasStatedBase {
		t.Fatalf("signal frame %+v %d", f, status)
	}
	for name, row := range map[string]struct {
		leg string
		h   map[string]string
	}{
		"a stated base on the subscribe leg's request": {legCDexTaskSubscribe, map[string]string{shnsdk.FrameHeaderSubscriptionRequest: "GET Subscription/s1", shnsdk.FrameHeaderStatedBase: "https://p.example/fhir"}},
		"a stated base on another leg":                 {"pas-claim", map[string]string{shnsdk.FrameHeaderStatedBase: "https://p.example/fhir"}},
		"an interaction on the signal leg":             {legCDexTaskSignal, map[string]string{shnsdk.FrameHeaderSubscriptionRequest: "GET Subscription/s1"}},
		"an If-Match on the signal leg":                {legCDexTaskSignal, map[string]string{shnsdk.FrameHeaderIfMatch: `W/"1"`}},
		"a sealed endpoint on another leg":             {"pas-claim", map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: "e"}},
	} {
		if _, status, _ := inboundFrameSubscription(row.leg, frame(row.h)); status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, status)
		}
	}
	// The subscribe leg keeps its own headers.
	if _, status, _ := inboundFrameSubscription(legCDexTaskSubscribe, frame(map[string]string{shnsdk.FrameHeaderSubscriptionRequest: "GET Subscription/s1",
		shnsdk.FrameHeaderSubscriptionEndpoint: "e", shnsdk.FrameHeaderIfMatch: `W/"1"`})); status != 0 {
		t.Fatalf("subscribe frame refused %d", status)
	}
}

// signalPayer is a payer's gateway receiving a provider's notification from
// the Hub, at one conformance level, recording its exchanges, with its own
// system's notification endpoint served over https.
type signalPayer struct {
	g         *Gateway
	provider  inboundTestRequester
	hubPriv   []byte
	authzPriv []byte
	got       *exchangeRecords
	endpoint  *httptest.Server
	mu        sync.Mutex
	received  []*http.Request
	bodies    [][]byte
	answer    func(w http.ResponseWriter)
}

func newSignalPayer(t *testing.T, level ConformanceEnforcement) *signalPayer {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	hubPub, hubPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	provEncPub, provEncPriv := genKeyPair(t)
	provSignPub, _ := genED25519(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	p := &signalPayer{hubPriv: hubPriv, authzPriv: authzPriv, provider: inboundTestRequester{ID: "provider-x", EncPub: provEncPub, EncPriv: provEncPriv}}
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
		w.WriteHeader(http.StatusNoContent)
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
	reg.Set("provider-x", shnsdk.RegistryEntry{ID: "provider-x", Role: "provider", EncPub: provEncPub, SignPub: provSignPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	sor := newCensusSoR()
	cfg := Config{
		Role:                           "payer",
		HolderID:                       "payer-x",
		Identity:                       shnsdk.Identity{HolderID: "payer-x", SignPriv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:                       "http://stub.test",
		AuthzPub:                       authzPub,
		HubTransportPub:                hubPub,
		Reg:                            reg,
		Validator:                      shnsdk.NewFakeValidator(),
		SoR:                            sor,
		Store:                          sor,
		Clock:                          clock,
		Client:                         &http.Client{Transport: &inboundAuthzStub{authzPriv: authzPriv, clock: clock, holder: "payer-x"}},
		Responder:                      NewNativeResponder(http.DefaultClient, "http://payer.test", clock),
		IngressBaseURL:                 signalPayerIngress,
		SubscriptionNotificationClient: client,
	}
	g := mustNew(t, cfg)
	g.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	g.cfg.ExchangeObserved = got.observe
	p.g, p.got = g, &got
	return p
}

// sealed are the payer's sealed endpoint and headers for the notifier
// provider-x, as its gateway sealed them at subscribe time.
func (p *signalPayer) sealed(t *testing.T, notifier, endpoint string, headers []string) (string, string) {
	t.Helper()
	k := p.g.paState().subscriberKeys
	b := subscription.Binding{Subscriber: "payer-x", Notifier: notifier, Topic: cdextopic.TokenValue}
	se := k.SealEndpoint(b, endpoint)
	sh, err := k.SealHeaders(b, se, headers)
	if err != nil {
		t.Fatal(err)
	}
	return se, sh
}

// signal delivers body on cdex-task-signal as the Hub would, framed with
// headers, under a token naming no patient: the leg names none.
func (p *signalPayer) signal(t *testing.T, headers map[string]string, body []byte) subAnswer {
	t.Helper()
	spec, _ := legSpecFor(legCDexTaskSignal)
	clock := p.g.cfg.Clock()
	const corr = "corr-signal"
	payload, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, body)
	if err != nil {
		t.Fatal(err)
	}
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.provider.ID, Recipient: p.g.cfg.HolderID, TransactionType: spec.WireName,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Frame: spec.ReqFrame, Holder: p.provider.ID,
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
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.provider, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return subAnswer{status: hdr.Status, framed: true, headers: hdr.Headers, body: answer}
}

// The payer's gateway's own checks of a notification it is delivered, each a
// network rule at every conformance level: valid notification − one mutation
// → refused on its primitive, recorded as the payer gateway's refusal, nothing
// reaching its own endpoint. The control is delivered, with E-17 made and the
// payer's own header value, and the endpoint's answer carried.
func TestCDexSignal_ThePayersGatewaysChecks(t *testing.T) {
	event := []byte(signalEvent(signalProviderBase + "/Task/t1"))
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		type row struct {
			frame  func(p *signalPayer) map[string]string
			body   []byte
			answer func(w http.ResponseWriter)
			status int
			rule   string
			msg    string
		}
		valid := func(p *signalPayer) map[string]string {
			se, sh := p.sealed(t, "provider-x", payerEndpoint, []string{"Authorization: Bearer payer-secret"})
			return map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
				shnsdk.FrameHeaderStatedBase: signalProviderBase, "Content-Type": "application/fhir+json"}
		}
		without := func(name string) func(p *signalPayer) map[string]string {
			return func(p *signalPayer) map[string]string { f := valid(p); delete(f, name); return f }
		}
		sealedAs := func(notifier, endpoint string, headers []string) func(p *signalPayer) map[string]string {
			return func(p *signalPayer) map[string]string {
				f := valid(p)
				f[shnsdk.FrameHeaderSubscriptionEndpoint], f[shnsdk.FrameHeaderSubscriptionChannel] = p.sealed(t, notifier, endpoint, headers)
				return f
			}
		}
		// sealedFor is the frame with the endpoint and headers sealed for
		// subscriber by k, this gateway's own keys when k is nil.
		sealedFor := func(k *subscription.Keys, subscriber string) func(p *signalPayer) map[string]string {
			return func(p *signalPayer) map[string]string {
				f := valid(p)
				keys := p.g.paState().subscriberKeys
				if k != nil {
					keys = *k
				}
				b := subscription.Binding{Subscriber: subscriber, Notifier: "provider-x", Topic: cdextopic.TokenValue}
				se := keys.SealEndpoint(b, payerEndpoint)
				sh, err := keys.SealHeaders(b, se, []string{"Authorization: Bearer payer-secret"})
				if err != nil {
					t.Fatal(err)
				}
				f[shnsdk.FrameHeaderSubscriptionEndpoint], f[shnsdk.FrameHeaderSubscriptionChannel] = se, sh
				return f
			}
		}
		patient := func(status int) func(w http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"p1"}`))
			}
		}
		patientAt := func(old, new string) []byte {
			b := strings.Replace(string(event), old, new, 1)
			if b == string(event) {
				t.Fatalf("no %s in the event", old)
			}
			return []byte(b)
		}
		const pat = `{"resourceType":"Patient","id":"p1"}`
		content := []byte(strings.Replace(string(event), `{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1"},`, 1))
		for name, r := range map[string]row{
			"control":                          {valid, event, nil, http.StatusNoContent, "", ""},
			"no sealed endpoint":               {without(shnsdk.FrameHeaderSubscriptionEndpoint), event, nil, http.StatusForbidden, RefusalAuthority, "carries no sealed endpoint or headers"},
			"no sealed headers":                {without(shnsdk.FrameHeaderSubscriptionChannel), event, nil, http.StatusForbidden, RefusalAuthority, "carries no sealed endpoint or headers"},
			"another notifier's sealed values": {sealedAs("provider-y", payerEndpoint, []string{"Authorization: Bearer payer-secret"}), event, nil, http.StatusForbidden, RefusalAuthority, ""},
			// A value another payer's gateway sealed, or this gateway sealed for
			// another payer, opens for no one else: an address naming it that
			// reached this payer is refused, never delivered (§7.2). The first
			// is refused on its key id, which is not this gateway's; the second
			// under this gateway's own key, on the seal's binding.
			"another payer's sealed values":                        {sealedFor(foreignKeys(t), "payer-y"), event, nil, http.StatusForbidden, RefusalAuthority, ""},
			"values this gateway sealed for another payer":         {sealedFor(nil, "payer-y"), event, nil, http.StatusForbidden, RefusalAuthority, ""},
			"an endpoint that is not https":                        {sealedAs("provider-x", "http://payer-sys.example/notify", nil), event, nil, http.StatusUnprocessableEntity, RefusalIntegrity, ""},
			"a header that is not an HTTP field":                   {sealedAs("provider-x", payerEndpoint, []string{"no colon here"}), event, nil, http.StatusUnprocessableEntity, RefusalFidelity, ""},
			"content on the signal leg":                            {valid, content, nil, http.StatusForbidden, RefusalAuthority, ""},
			"a Patient as a root Bundle member":                    {valid, patientAt(`"type":"history",`, `"type":"history","x":`+pat+`,`), nil, http.StatusForbidden, RefusalAuthority, "id-only"},
			"a Patient as another member of entry[1]":              {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1","x":`+pat+`,`), nil, http.StatusForbidden, RefusalAuthority, "id-only"},
			"a Patient in entry[0].request":                        {valid, patientAt(`"request":{"method":"GET",`, `"request":{"x":`+pat+`,"method":"GET",`), nil, http.StatusForbidden, RefusalAuthority, "id-only"},
			"a Patient in a focus part's valueReference":           {valid, patientAt(`{"name":"focus","valueReference":{`, `{"name":"focus","valueReference":{"x":`+pat+`,`), nil, http.StatusForbidden, RefusalAuthority, "id-only"},
			"a Patient in the status's meta":                       {valid, patientAt(`"resource":{"resourceType":"Parameters",`, `"resource":{"resourceType":"Parameters","meta":{"x":`+pat+`},`), nil, http.StatusForbidden, RefusalAuthority, "id-only"},
			"not a notification":                                   {valid, []byte(`{"resourceType":"Bundle","type":"history"}`), nil, http.StatusForbidden, RefusalAuthority, ""},
			"nesting deeper than relay reads":                      {valid, patientAt(`"type":"history",`, `"type":"history","x":`+strings.Repeat(`{"a":`, 70)+`0`+strings.Repeat(`}`, 70)+`,`), nil, http.StatusForbidden, RefusalAuthority, "closed notification shape"},
			"a focus with userinfo":                                {valid, []byte(signalEvent("https://u@provider.example/fhir/Task/t1")), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"a focus whose host ends in a dot":                     {valid, []byte(signalEvent("https://provider.example./fhir/Task/t1")), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"a string relay cannot read":                           {valid, patientAt(`{"name":"event-number","valueString":"1"}`, `{"name":"event-number","valueString":"\ud800"}`), nil, http.StatusBadRequest, RefusalFidelity, "read one way only"},
			"an additional-context Patient":                        {valid, patientAt(`{"name":"focus",`, `{"name":"additional-context","valueReference":{"reference":"Patient/p1"}},{"name":"focus",`), nil, http.StatusForbidden, RefusalAuthority, "additional-context"},
			"an additional-context Task":                           {valid, patientAt(`{"name":"focus",`, `{"name":"additional-context","valueReference":{"reference":"`+signalProviderBase+`/Task/t2"}},{"name":"focus",`), nil, http.StatusForbidden, RefusalAuthority, "additional-context"},
			"an additional-context naming nothing":                 {valid, patientAt(`{"name":"focus",`, `{"name":"additional-context"},{"name":"focus",`), nil, http.StatusForbidden, RefusalAuthority, "additional-context"},
			"a part naming a patient":                              {valid, patientAt(`{"name":"focus",`, `{"name":"patient","valueReference":{"reference":"Patient/p1"}},{"name":"focus",`), nil, http.StatusForbidden, RefusalAuthority, "closed notification shape"},
			"an extension":                                         {valid, patientAt(`"type":"history",`, `"type":"history","extension":[{"url":"http://example.org/x","valueString":"y"}],`), nil, http.StatusForbidden, RefusalAuthority, "closed notification shape"},
			"a signed notification":                                {valid, []byte(strings.TrimSuffix(signalEvent(signalProviderBase+"/Task/t1"), "}") + `,"signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-13T00:00:00Z","who":{"reference":"Organization/provider"}}}`), nil, http.StatusForbidden, RefusalAuthority, "signature"},
			"a sibling path on the provider's server":              {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"https://provider.example/fhirX/Task/t1",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "not under the provider's stated FHIR base"},
			"identity members on the provider's server, delivered": {valid, []byte(withIdentities(signalEvent(signalProviderBase+"/Task/t1"), signalProviderBase)), nil, http.StatusNoContent, "", ""},
			"a fragment on the provider's address":                 {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1#x",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "not under the provider's stated FHIR base"},
			"an escaped base path":                                 {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"https://provider.example/%66hir/Task/t1",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "not under the provider's stated FHIR base"},
			"a host-less https address":                            {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"https:provider.example/fhir/Task/t1",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"a backslashed address":                                {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"https:\\\\provider.example\\fhir\\Task\\t1",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"an address with a leading space":                      {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":" `+signalProviderBase+`/Task/t1",`), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"a network-path link url":                              {valid, patientAt(`"type":"history",`, `"type":"history","link":[{"relation":"self","url":"//provider.example/fhir/Subscription/s1/$status"}],`), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"a tab in a response.location":                         {valid, patientAt(`{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1","response":{"status":"200","location":"https://provider.example/fhir/Task/t1/_his\ttory/2"},`), nil, http.StatusUnprocessableEntity, RefusalRouting, "cannot be re-keyed"},
			"not FHIR":                                             {valid, []byte(`notify`), nil, http.StatusForbidden, RefusalAuthority, ""},
			"an answer holding a Patient":                          {valid, event, patient(http.StatusOK), http.StatusBadGateway, RefusalAuthority, ""},
			"an error answer holding a Patient":                    {valid, event, patient(http.StatusBadRequest), http.StatusBadGateway, RefusalAuthority, ""},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newSignalPayer(t, level)
				p.answer = r.answer
				a := p.signal(t, r.frame(p), r.body)
				if a.status != r.status || !strings.Contains(string(a.body), r.msg) {
					t.Fatalf("answered %d %s, want %d naming %q", a.status, a.body, r.status, r.msg)
				}
				x := p.got.only(t)
				if r.rule == "" {
					if len(p.bodies) != 1 || !strings.Contains(string(p.bodies[0]), signalPayerIngress+"/cdex/provider/provider-x/Task/t1") {
						t.Fatalf("delivered %q", p.bodies)
					}
					if got := p.received[0].Header.Get("Authorization"); got != "Bearer payer-secret" {
						t.Fatalf("Authorization %q", got)
					}
					if x.Outcome == ExchangeRefused || x.Exchange != legCDexTaskSignal {
						t.Fatalf("control record %+v", x)
					}
					return
				}
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway || x.Rule != r.rule {
					t.Fatalf("record %s by %q on %q, want refused by the payer's gateway on %q", x.Outcome, x.RefusedBy, x.Rule, r.rule)
				}
				// Each fidelity refusal here is of a notification the gateway
				// cannot build its delivery of: stage build.
				if r.rule == RefusalFidelity && x.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("recorded at stage %q, want %q", x.RefusalStage, vocab.RefusalStageBuild)
				}
				if strings.Contains(string(a.body), "payer-secret") || strings.Contains(string(a.body), "payer-sys.example") {
					t.Fatalf("the refusal names the payer's own values: %s", a.body)
				}
				if r.answer == nil && len(p.bodies) != 0 {
					t.Fatalf("the endpoint got %d notifications", len(p.bodies))
				}
			})
		}
	}
}

// A registered edit the payer's gateway cannot make on a notification it can
// read is this gateway's own fault, whoever sent the notification: refused
// 502 on the fidelity of its own edit, at stage verify, nothing delivered, at
// every conformance level. Only the notifier's message is answered 4xx.
func TestCDexSignal_AFaultInTheGatewaysOwnEditIs502(t *testing.T) {
	saved := deliveryEdits
	t.Cleanup(func() { deliveryEdits = saved })
	deliveryEdits = map[relay.EditID]deliveryEdit{
		relay.EditCDexTaskNotificationAddressRekey: func(*Gateway, *exchangeRecorder, *relay.Document, deliveryInput) ([]relay.Op, error) {
			return nil, fmt.Errorf("an edit failing its own verification")
		},
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newSignalPayer(t, level)
			se, sh := p.sealed(t, "provider-x", payerEndpoint, []string{"Authorization: Bearer payer-secret"})
			a := p.signal(t, map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
				shnsdk.FrameHeaderStatedBase: signalProviderBase, "Content-Type": "application/fhir+json"}, []byte(signalEvent(signalProviderBase+"/Task/t1")))
			if a.status != http.StatusBadGateway || !strings.Contains(string(a.body), refusalSignalEdit) {
				t.Fatalf("answered %d %s, want 502 %q", a.status, a.body, refusalSignalEdit)
			}
			x := p.got.only(t)
			if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway || x.Rule != RefusalFidelity || x.RefusalStage != vocab.RefusalStageVerify {
				t.Fatalf("record %s by %q on %q at %q, want refused by the payer's gateway on %q at %q", x.Outcome, x.RefusedBy, x.Rule, x.RefusalStage, RefusalFidelity, vocab.RefusalStageVerify)
			}
			if len(p.bodies) != 0 {
				t.Fatalf("the endpoint got %d notifications", len(p.bodies))
			}
		})
	}
}

// credentialBeforeBody are the ingress routes whose credential travels in the
// address and headers alone, so they check it before reading a body: an
// unauthenticated sender cannot make the gateway read up to its limit. The
// body-reading rows every other route shares run on them with a credential
// that verifies (TestCDexNotify_TheCredentialComesBeforeTheBody,
// TestPASNotify_TheCredentialComesBeforeTheBody).
var credentialBeforeBody = map[string]bool{RouteCDexNotify: true, RoutePASNotify: true}

// notifyRequest is a POST of body to the notification address the provider's
// gateway p writes for payer-x, with its credential when authed.
func notifyRequest(t *testing.T, p *subProvider, body []byte, authed bool) *http.Request {
	t.Helper()
	se := payerKeys(t).SealEndpoint(payerBinding, payerEndpoint)
	sh, err := payerKeys(t).SealHeaders(payerBinding, se, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := subscription.Address{Ingress: cdexIngressBase, Topic: cdextopic.TokenValue, Subscriber: "payer-x", SealedEndpoint: se}
	r := httptest.NewRequest(http.MethodPost, strings.TrimPrefix(a.String(), cdexIngressBase), bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/fhir+json")
	if authed {
		r.Header.Set(subscription.AuthorizationHeader, p.keys().Credential(a, sh))
		r.Header.Set(subscription.ChannelHeader, sh)
	}
	return r
}

// The notification route checks its credential before it reads a body: an
// unauthenticated body over the network's limit, or in a coding the gateway
// does not decode, is refused 401 on authentication, never 413 or 415, and
// nothing of it is read past what the transport took. With a credential that
// verifies, the body is read as every ingress route reads one: over the
// limit 413, recorded routing; in another coding 415, recorded integrity; a
// gzip body that inflates past the limit 413. Each at every level.
func TestCDexNotify_TheCredentialComesBeforeTheBody(t *testing.T) {
	over := bytes.Repeat([]byte(" "), int(shnsdk.MaxRequestBytes)+1)
	inflating := func() []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write(over)
		_ = zw.Close()
		return b.Bytes()
	}()
	for _, level := range allLevels {
		for name, row := range map[string]struct {
			authed       bool
			body         []byte
			coding       string
			status       int
			rule, reason string
		}{
			"unauthenticated, over the limit":    {false, over, "", http.StatusUnauthorized, RefusalAuthentication, ""},
			"unauthenticated, in another coding": {false, []byte(`{}`), "br", http.StatusUnauthorized, RefusalAuthentication, ""},
			"unauthenticated, inflating past it": {false, inflating, "gzip", http.StatusUnauthorized, RefusalAuthentication, ""},
			"authenticated, over the limit":      {true, over, "", http.StatusRequestEntityTooLarge, RefusalRouting, errRequestTooLarge},
			"authenticated, in another coding":   {true, []byte(`{}`), "br", http.StatusUnsupportedMediaType, RefusalIntegrity, errUnsupportedCoding},
			"authenticated, inflating past it":   {true, inflating, "gzip", http.StatusRequestEntityTooLarge, RefusalRouting, errRequestTooLarge},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newSubProvider(t, level, newSubStore(t))
				r := notifyRequest(t, p, row.body, row.authed)
				if row.coding != "" {
					r.Header.Set("Content-Encoding", row.coding)
				}
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RouteCDexNotify)).ServeHTTP(rec, r)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.reason) {
					t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), row.status, row.reason)
				}
				x := p.got.only(t)
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByProviderGateway || x.Rule != row.rule {
					t.Fatalf("record %s by %q on %q, want refused by the provider's gateway on %q", x.Outcome, x.RefusedBy, x.Rule, row.rule)
				}
			})
		}
	}
}

// A redirect the payer's endpoint answers with is carried as its answer: a
// 303 within its origin, which would turn the POST into a GET, is not
// followed, and the 303 with its body goes back to the provider as the
// endpoint answered; a 307 to another origin is refused on routing.
func TestCDexSignal_AnEndpointsRedirectIsItsAnswer(t *testing.T) {
	event := []byte(signalEvent(signalProviderBase + "/Task/t1"))
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementStrict} {
		p := newSignalPayer(t, level)
		p.answer = func(w http.ResponseWriter) {
			w.Header().Set("Location", "/elsewhere")
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusSeeOther)
			_, _ = w.Write([]byte("see elsewhere"))
		}
		se, sh := p.sealed(t, "provider-x", payerEndpoint, nil)
		frame := map[string]string{shnsdk.FrameHeaderSubscriptionEndpoint: se, shnsdk.FrameHeaderSubscriptionChannel: sh,
			shnsdk.FrameHeaderStatedBase: signalProviderBase, "Content-Type": "application/fhir+json"}
		a := p.signal(t, frame, event)
		if a.status != http.StatusSeeOther || string(a.body) != "see elsewhere" || len(p.bodies) != 1 {
			t.Fatalf("%s: answered %d %q after %d deliveries, want the endpoint's 303 as answered", level, a.status, a.body, len(p.bodies))
		}
		q := newSignalPayer(t, level)
		q.answer = func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://other.example/notify")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		se, sh = q.sealed(t, "provider-x", payerEndpoint, nil)
		frame[shnsdk.FrameHeaderSubscriptionEndpoint], frame[shnsdk.FrameHeaderSubscriptionChannel] = se, sh
		if a := q.signal(t, frame, event); a.status == http.StatusTemporaryRedirect || a.status < 400 {
			t.Fatalf("%s: a cross-origin redirect answered %d %q, want it refused", level, a.status, a.body)
		}
		if x := q.got.only(t); x.Outcome != ExchangeRefused || x.Rule != RefusalRouting {
			t.Fatalf("%s: record %s on %q, want refused on routing", level, x.Outcome, x.Rule)
		}
	}
}
