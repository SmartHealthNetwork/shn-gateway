//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// workstream_pa_subscribe_answer_test.go — the notifier's gateway's answer
// check and ownership test on both subscribe legs (cdex-task-subscribe at a
// provider's gateway, pas-subscription-subscribe at a payer's), at every
// conformance level: a 2xx answer passes when it holds nothing but shapes
// that name no patient, whatever the interaction, and then each Subscription
// it carries is held to the subscriber, by the interaction's own test.

// subscribeTopic is a subscribe leg's notifier gateway, as these rows drive
// it: its leg, who refuses on it, how to build it, the subscriber's own
// Subscription and another's as its system stores them, and the subscriber's
// create and search as its gateway frames them.
type subscribeTopic struct {
	name, leg, refusedBy string
	notifier             func(t *testing.T, level ConformanceEnforcement, sys *subStore) (p *cdexRequestProvider, stored func(subscriber string) []byte)
	subscriber, other    string
	create, search       func(t *testing.T) subFrame
}

var subscribeTopics = []subscribeTopic{
	{
		name: "cdex-task", leg: legCDexTaskSubscribe, refusedBy: RefusedByProviderGateway,
		notifier: func(t *testing.T, level ConformanceEnforcement, sys *subStore) (*cdexRequestProvider, func(string) []byte) {
			p := newSubProvider(t, level, sys)
			return p.cdexRequestProvider, func(subscriber string) []byte { return p.storedFor(t, subscriber, cdexSubscriptionJSON(nil)) }
		},
		subscriber: "payer-x", other: "payer-y",
		create: func(t *testing.T) subFrame { return createFrame(t, cdexSubscriptionJSON(nil)) },
		search: func(t *testing.T) subFrame { return searchFrame(t, payerEndpoint) },
	},
	{
		name: "pas", leg: pastopic.SubscribeLeg, refusedBy: RefusedByPayerGateway,
		notifier: func(t *testing.T, level ConformanceEnforcement, sys *subStore) (*cdexRequestProvider, func(string) []byte) {
			p := newPASSubPayer(t, level, sys, nil)
			return p, func(subscriber string) []byte { return pasStoredFor(t, p, subscriber, pasSubscriptionJSON(nil)) }
		},
		subscriber: "provider-x", other: "provider-y",
		create: func(t *testing.T) subFrame { return pasCreateFrame(t, pasSubscriptionJSON(nil)) },
		search: func(t *testing.T) subFrame {
			sealed := pasProviderKeys(t).SealEndpointForSearch(pasBinding, pasProviderEndpoint)
			q, err := shnsdk.ParseSubscriptionSearch("url=" + url.QueryEscape(pasProviderEndpoint) + "&status=requested,active")
			if err != nil {
				t.Fatal(err)
			}
			return subFrame{request: q.Request.String(), endpoint: strings.Join(sealed, " ")}
		},
	},
}

// subscribeInteractions are a subscribe leg's interactions on Subscription
// sub-1, as the subscriber's gateway frames them, and the path the
// interaction's own forward reaches the notifier's system at.
func subscribeInteractions(t *testing.T, tp subscribeTopic) []struct {
	in    shnsdk.SubscriptionInteraction
	frame subFrame
	path  string
} {
	update := tp.create(t)
	update.request = "PUT Subscription/sub-1"
	return []struct {
		in    shnsdk.SubscriptionInteraction
		frame subFrame
		path  string
	}{
		{shnsdk.SubscriptionRead, subFrame{request: "GET Subscription/sub-1"}, "/fhir/Subscription/sub-1"},
		{shnsdk.SubscriptionVRead, subFrame{request: "GET Subscription/sub-1/_history/1"}, "/fhir/Subscription/sub-1/_history/1"},
		{shnsdk.SubscriptionCreate, tp.create(t), "/fhir/Subscription"},
		{shnsdk.SubscriptionUpdate, update, "/fhir/Subscription/sub-1"},
		{shnsdk.SubscriptionDelete, subFrame{request: "DELETE Subscription/sub-1"}, "/fhir/Subscription/sub-1"},
		{shnsdk.SubscriptionStatus, subFrame{request: "GET Subscription/sub-1/$status"}, "/fhir/Subscription/sub-1/$status"},
		{shnsdk.SubscriptionSearch, tp.search(t), "/fhir/Subscription"},
		{shnsdk.SubscriptionPage, subFrame{request: "GET ?_getpages=abc"}, "/fhir"},
	}
}

// answerShape is a 2xx answer the notifier's system gives: its body, from the
// subscriber's own stored Subscription and another's, and what it carries for
// the rules: the subscriber's Subscriptions and others', whether it is a
// $status Bundle and, if so, whether it names the Subscription asked for, and
// whether it holds a resource or is no FHIR this network delivers.
type answerShape struct {
	name        string
	body        func(own, other []byte) string
	owned, not  int
	status      bool
	namesAsked  bool
	unreadOrHas bool
}

func answerShapes() []answerShape {
	const patient = `{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`
	const outcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`
	searchset := func(entries ...string) string {
		return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
	}
	entry := func(b []byte) string { return `{"resource":` + string(b) + `}` }
	statusOf := func(id string) string {
		return searchset(`{"resource":{"resourceType":"Parameters","parameter":[{"name":"subscription","valueReference":{"reference":"Subscription/` + id + `"}},` +
			`{"name":"status","valueCode":"active"},{"name":"type","valueCode":"query-status"}]}}`)
	}
	return []answerShape{
		{name: "the subscriber's Subscription", body: func(own, _ []byte) string { return string(own) }, owned: 1},
		{name: "another's Subscription", body: func(_, other []byte) string { return string(other) }, not: 1},
		{name: "an OperationOutcome", body: func(_, _ []byte) string { return outcome }},
		{name: "no body", body: func(_, _ []byte) string { return "" }},
		{name: "a searchset of the subscriber's and an outcome", body: func(own, _ []byte) string { return searchset(entry(own), entry([]byte(outcome))) }, owned: 1},
		{name: "a searchset of the subscriber's twice", body: func(own, _ []byte) string { return searchset(entry(own), entry(own)) }, owned: 2},
		{name: "a searchset of the subscriber's and another's", body: func(own, other []byte) string { return searchset(entry(own), entry(other)) }, owned: 1, not: 1},
		{name: "an empty searchset", body: func(_, _ []byte) string { return `{"resourceType":"Bundle","type":"searchset"}` }},
		{name: "a $status Bundle naming the Subscription asked for", body: func(_, _ []byte) string { return statusOf("sub-1") }, status: true, namesAsked: true},
		{name: "a $status Bundle naming another", body: func(_, _ []byte) string { return statusOf("sub-2") }, status: true},
		{name: "a Patient", body: func(_, _ []byte) string { return patient }, unreadOrHas: true},
		{name: "a Subscription holding a Patient", body: func(own, _ []byte) string {
			return strings.Replace(string(own), `"resourceType":"Subscription",`, `"resourceType":"Subscription","contained":[`+patient+`],`, 1)
		}, unreadOrHas: true},
		{name: "an OperationOutcome holding a Patient", body: func(_, _ []byte) string {
			return strings.Replace(outcome, `"issue"`, `"contained":[`+patient+`],"issue"`, 1)
		}, unreadOrHas: true},
		{name: "a searchset holding a Patient", body: func(own, _ []byte) string { return searchset(entry(own), entry([]byte(patient))) }, unreadOrHas: true},
	}
}

// wantAnswer is what the notifier's gateway does with shape on in, by the
// rules: delivered (0), or refused with the status given.
func wantAnswer(in shnsdk.SubscriptionInteraction, s answerShape) int {
	namesID := in != shnsdk.SubscriptionCreate && in != shnsdk.SubscriptionSearch && in != shnsdk.SubscriptionPage
	switch {
	case s.unreadOrHas:
		return http.StatusBadGateway
	case s.status && (!namesID || !s.namesAsked):
		return http.StatusBadGateway
	}
	n := s.owned + s.not
	switch in {
	case shnsdk.SubscriptionRead, shnsdk.SubscriptionVRead:
		if n == 1 && s.not == 0 {
			return 0
		}
		return http.StatusNotFound
	case shnsdk.SubscriptionCreate, shnsdk.SubscriptionUpdate:
		if n == 0 || (n == 1 && s.not == 0) {
			return 0
		}
		return http.StatusNotFound
	case shnsdk.SubscriptionDelete, shnsdk.SubscriptionStatus:
		if s.not == 0 {
			return 0
		}
		return http.StatusNotFound
	default: // a search or a page
		if s.not == 0 {
			return 0
		}
		return http.StatusForbidden
	}
}

// The answer matrix, on both topics, at every level: each shape on each
// interaction is delivered byte for byte, framed with the notifier's base and
// under no patient, or refused by the rules, with nothing of it delivered.
// A shape that names no patient passes the answer check whatever the
// interaction; then a read or vread's answer carries exactly one
// Subscription, the subscriber's, or it is the network's 404 (an
// OperationOutcome or no body included); a create's or an update's carries at
// most one, the subscriber's; a delete's or a $status's carries only the
// subscriber's, or the network's 404; a search's or a page's only the
// subscriber's, or 403. A $status Bundle passes the check only on an
// interaction naming the Subscription by id, naming that one: on a read or a
// vread, carrying no Subscription, it is then the network's 404. A shape
// holding a resource is refused 502 on every interaction.
func TestSubscribe_TheAnswerMatrix(t *testing.T) {
	for _, tp := range subscribeTopics {
		for _, level := range allLevels {
			t.Run(tp.name+"/"+level.String(), func(t *testing.T) {
				sys := newSubStore(t)
				p, stored := tp.notifier(t, level, sys)
				own, other := stored(tp.subscriber), stored(tp.other)
				for _, it := range subscribeInteractions(t, tp) {
					for _, s := range answerShapes() {
						name := string(it.in) + "/" + s.name
						body := s.body(own, other)
						sys.store["sub-1"] = own
						sys.answer = func(r *http.Request) (readAnswer, bool) {
							// The gateway's own read before an update, a delete or a
							// $status is answered by the store.
							if r.Method == http.MethodGet && r.URL.Path == "/fhir/Subscription/sub-1" && it.in != shnsdk.SubscriptionRead {
								return readAnswer{}, false
							}
							if r.URL.Path != it.path {
								t.Errorf("%s: the forward reached %s %s, want %s", name, r.Method, r.URL.Path, it.path)
							}
							return readAnswer{status: http.StatusOK, header: fhirJSONHeader, body: []byte(body)}, true
						}
						p.got.mu.Lock()
						p.got.recs = nil
						p.got.mu.Unlock()
						f := it.frame
						f.leg = tp.leg
						a := p.subscribe(t, f)
						want := wantAnswer(it.in, s)
						if want == 0 {
							if !a.framed || a.status != http.StatusOK || a.body == nil && body != "" || string(a.body) != body || a.subject != "" {
								t.Errorf("%s: %d (framed %v, subject %q) %s, want 200 delivered as sent", name, a.status, a.framed, a.subject, a.body)
							}
							if got := a.headers[shnsdk.FrameHeaderStatedBase]; got != sys.srv.URL+"/fhir" {
								t.Errorf("%s: statedBase %q, want the notifier's base", name, got)
							}
							continue
						}
						if a.status != want || bytes.Contains(a.body, []byte("Doe")) || bytes.Contains(a.body, []byte("/notify/")) {
							t.Errorf("%s: %d %s, want %d with nothing of the answer", name, a.status, a.body, want)
							continue
						}
						wantRefusal(t, p.got.only(t), want, tp.refusedBy, RefusalAuthority)
					}
				}
			})
		}
	}
}

// The subscriber's return preference, on both topics, at every level: framed
// as Prefer, it reaches the notifier's system as sent on the interaction's
// own forward, and never on the gateway's own read before an update, a delete
// or a $status. A create the subscriber asked to be answered minimally,
// answered 201 with no body, is delivered with its location framed, for the
// subscriber's gateway to write its Location (E-23).
func TestSubscribe_ThePreferenceIsCarriedOnTheInteractionsOwnForward(t *testing.T) {
	for _, tp := range subscribeTopics {
		for _, level := range allLevels {
			t.Run(tp.name+"/"+level.String(), func(t *testing.T) {
				sys := newSubStore(t)
				p, stored := tp.notifier(t, level, sys)
				own := stored(tp.subscriber)
				for _, it := range subscribeInteractions(t, tp) {
					for _, pref := range []string{"return=minimal", "return=representation", "return=OperationOutcome", "RETURN=other; x=1"} {
						name := string(it.in) + "/" + pref
						sys.store["sub-1"] = own
						sys.answer = func(r *http.Request) (readAnswer, bool) {
							if r.Method == http.MethodGet && r.URL.Path == "/fhir/Subscription/sub-1" && it.in != shnsdk.SubscriptionRead {
								return readAnswer{}, false
							}
							if it.in == shnsdk.SubscriptionCreate && pref == "return=minimal" {
								return readAnswer{status: http.StatusCreated, header: map[string]string{"Location": sys.srv.URL + "/fhir/Subscription/sub-9/_history/1"}}, true
							}
							return readAnswer{status: http.StatusOK, header: fhirJSONHeader, body: own}, true
						}
						before := len(sys.calls())
						f := it.frame
						f.leg, f.prefer = tp.leg, pref
						a := p.subscribe(t, f)
						calls := sys.calls()[before:]
						if len(calls) == 0 {
							t.Fatalf("%s: nothing reached the notifier's system (%d %s)", name, a.status, a.body)
						}
						for _, c := range calls[:len(calls)-1] {
							if c.header.Values("Prefer") != nil {
								t.Errorf("%s: the gateway's own %s %s carried Prefer %q", name, c.method, c.requestURI, c.header.Values("Prefer"))
							}
						}
						if got := calls[len(calls)-1].header.Values("Prefer"); len(got) != 1 || got[0] != pref {
							t.Errorf("%s: the interaction's forward carried Prefer %q, want %q as sent", name, got, pref)
						}
						if it.in == shnsdk.SubscriptionCreate && pref == "return=minimal" {
							if a.status != http.StatusCreated || len(a.body) != 0 || a.headers[shnsdk.FrameHeaderSubscriptionLocation] != "Subscription/sub-9/_history/1" {
								t.Errorf("%s: %d %q %v, want 201 with no body and its location framed", name, a.status, a.body, a.headers)
							}
						}
					}
				}
				// No Prefer framed, none sent.
				before := len(sys.calls())
				p.subscribe(t, subFrame{request: "GET Subscription/sub-1", leg: tp.leg})
				for _, c := range sys.calls()[before:] {
					if c.header.Values("Prefer") != nil {
						t.Errorf("a read framed with no Prefer reached the system with %q", c.header.Values("Prefer"))
					}
				}
			})
		}
	}
}

// A Prefer framed that is not exactly one return preference is refused 400
// on integrity at the notifier's gateway, on both topics and at every level,
// with nothing sent to its system: no other preference crosses the network.
func TestSubscribe_APreferThatIsNotOneReturnPreferenceIsRefused(t *testing.T) {
	for _, tp := range subscribeTopics {
		for _, level := range allLevels {
			for _, v := range []string{"respond-async", "handling=strict", "return=minimal, respond-async", "respond-async, return=minimal",
				" return=minimal", "return=minimal,", "returned=minimal"} {
				t.Run(tp.name+"/"+level.String()+"/"+v, func(t *testing.T) {
					sys := newSubStore(t)
					p, stored := tp.notifier(t, level, sys)
					sys.store["sub-1"] = stored(tp.subscriber)
					a := p.subscribe(t, subFrame{request: "GET Subscription/sub-1", prefer: v, leg: tp.leg})
					if a.status != http.StatusBadRequest || !strings.Contains(string(a.body), refusalCDexSubscribePrefer) {
						t.Fatalf("%d %s, want 400 %q", a.status, a.body, refusalCDexSubscribePrefer)
					}
					wantRefusal(t, p.got.only(t), http.StatusBadRequest, tp.refusedBy, RefusalIntegrity)
					if calls := sys.calls(); len(calls) != 0 {
						t.Fatalf("the notifier's system got %+v, want nothing", calls)
					}
				})
			}
		}
	}
}
