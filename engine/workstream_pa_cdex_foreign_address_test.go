//go:build !shn_coreonly

package engine

import (
	"bytes"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A payer can learn another payer's notification address at a provider: an
// empty page of the other payer's search is delivered with its links as the
// provider's system wrote them, and those may name the address. The
// address names the other payer, its key id and its sealed endpoint, which
// opens only under that payer's gateway's key. These rows hold that a payer
// holding another payer's address cannot use it: nothing it frames makes the
// provider's gateway write, credential or route an address for the other
// payer, and the notification route refuses a credential issued for any
// other address. payer-x is the requesting payer throughout (the sender the
// Hub authenticated); payer-y is the other payer.

// foreignPayer is payer-y as the provider-x notifier knows it: its sealed
// endpoint and headers, as its own gateway sealed them, and the notification
// address and credential the provider's gateway wrote for it.
type foreignPayer struct {
	sealedEndpoint, sealedHeaders string
	address                       subscription.Address
	credential                    string
}

// foreignKeys are payer-y's gateway's subscription keys.
func foreignKeys(t testing.TB) *subscription.Keys {
	t.Helper()
	k, err := subscription.DeriveKeys("payer-y", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &k
}

// foreignAddress is payer-y's notification address at this provider.
func foreignAddress(t testing.TB) string {
	t.Helper()
	b := subscription.Binding{Subscriber: "payer-y", Notifier: "provider-x", Topic: cdextopic.TokenValue}
	se := foreignKeys(t).SealEndpoint(b, "https://payer-y-sys.example/notify")
	return subscription.Address{Ingress: cdexIngressBase, Topic: cdextopic.TokenValue, Subscriber: "payer-y", SealedEndpoint: se}.String()
}

func newForeignPayer(t testing.TB, provider subscription.Keys) foreignPayer {
	t.Helper()
	k := foreignKeys(t)
	b := subscription.Binding{Subscriber: "payer-y", Notifier: "provider-x", Topic: cdextopic.TokenValue}
	se := k.SealEndpoint(b, "https://payer-y-sys.example/notify")
	sh, err := k.SealHeaders(b, se, []string{"Authorization: Bearer payer-y-secret"})
	if err != nil {
		t.Fatal(err)
	}
	a := subscription.Address{Ingress: cdexIngressBase, Topic: cdextopic.TokenValue, Subscriber: "payer-y", SealedEndpoint: se}
	return foreignPayer{sealedEndpoint: se, sealedHeaders: sh, address: a, credential: provider.Credential(a, sh)}
}

// withChannelMember is f with member written as channel's first member of
// its body, as a gateway that does not seal it would send it
// (endpointStatedFrame writes channel.endpoint so).
func withChannelMember(t testing.TB, f subFrame, member string) subFrame {
	t.Helper()
	const channel = `"channel":{`
	if strings.Count(string(f.body), channel) != 1 {
		t.Fatalf("fixture: the body does not open channel once: %s", f.body)
	}
	f.body = []byte(strings.Replace(string(f.body), channel, channel+member+",", 1))
	return f
}

// A create or an update from payer-x that names payer-y's address, however it
// names it, is forwarded as payer-x's own: the provider's gateway writes the
// address for the sender the Hub authenticated, with a credential for that
// address alone, so the provider's system holds a Subscription that is
// payer-x's by the ownership test and never payer-y's, and payer-y's address
// and credential do not reach it. A notification to what it holds goes to
// payer-x, whose gateway cannot open payer-y's sealed values (the subscriber
// rows of TestOpenEndpoint_Rejects). At every level.
func TestCDexSubscribe_AnotherPayersAddressIsWrittenForTheSender(t *testing.T) {
	for _, level := range allLevels {
		for name, frame := range map[string]func(t *testing.T, y foreignPayer) subFrame{
			// A gateway that frames payer-y's sealed values as its own.
			"payer-y's sealed values framed": func(t *testing.T, y foreignPayer) subFrame {
				f := createFrame(t, cdexSubscriptionJSON(nil))
				f.endpoint, f.channel = y.sealedEndpoint, y.sealedHeaders
				return f
			},
			"payer-y's address as channel.endpoint": func(t *testing.T, y foreignPayer) subFrame {
				return endpointStatedFrame(t, cdexSubscriptionJSON(nil), y.address.String())
			},
			"payer-y's credential as channel.header": func(t *testing.T, y foreignPayer) subFrame {
				return withChannelMember(t, sealedCreateFrame(t, cdexSubscriptionJSON(nil), payerEndpoint),
					`"header":["`+subscription.AuthorizationHeader+`: `+y.credential+`","`+subscription.ChannelHeader+`: `+y.sealedHeaders+`"]`)
			},
		} {
			for _, request := range []string{"POST Subscription", "PUT Subscription/sub-1"} {
				t.Run(level.String()+"/"+name+"/"+request, func(t *testing.T) {
					sys := newSubStore(t)
					p := newSubProvider(t, level, sys)
					y := newForeignPayer(t, p.keys())
					f := frame(t, y)
					f.request = request
					if a := p.subscribe(t, f); a.status/100 != 2 {
						t.Fatalf("answer %d %s, want the Subscription forwarded as payer-x's", a.status, a.body)
					}
					calls := sys.calls()
					got := calls[len(calls)-1]
					s, rf := subscription.ParseSubscription(got.body, "application/fhir+json")
					if rf != nil {
						t.Fatal(rf)
					}
					if !p.keys().Owns(s, cdexIngressBase, "payer-x", cdextopic.Topic{}) {
						t.Fatalf("what the provider's system holds is not payer-x's by the ownership test: %s", got.body)
					}
					if p.keys().Owns(s, cdexIngressBase, "payer-y", cdextopic.Topic{}) {
						t.Fatalf("what the provider's system holds is payer-y's: %s", got.body)
					}
					addr, ok := subscription.ParseAddress(endpointOf(got.body), cdexIngressBase)
					if !ok || addr.Subscriber != "payer-x" || addr == y.address {
						t.Fatalf("endpoint %q, want an address for payer-x", endpointOf(got.body))
					}
					if bytes.Contains(got.body, []byte(y.address.String())) || bytes.Contains(got.body, []byte(y.credential)) {
						t.Fatalf("payer-y's address or credential reached the provider's system: %s", got.body)
					}
				})
			}
		}
	}
}

// An update framed with payer-y's notification address as its sealed endpoint
// is refused on authority, as a create is (TestCDexSubscribe_RefusalsByPrimitive):
// the address is not a sealed value. The update's read before it is the only
// call the provider's system gets; nothing is written. At every level.
func TestCDexSubscribe_AnUpdateFramedWithAnotherPayersAddressIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			f := createFrame(t, cdexSubscriptionJSON(nil))
			f.request, f.endpoint = "PUT Subscription/sub-1", foreignAddress(t)
			a := p.subscribe(t, f)
			if a.status != http.StatusForbidden || !strings.Contains(string(a.body), refusalCDexSubscribeNotSealed) {
				t.Fatalf("answer %d %s, want 403 naming %q", a.status, a.body, refusalCDexSubscribeNotSealed)
			}
			for _, c := range sys.calls() {
				if c.method != http.MethodGet {
					t.Fatalf("the provider's system got %s %s, want only the read", c.method, c.requestURI)
				}
			}
			if x := p.got.only(t); x.Outcome != ExchangeRefused || x.Rule != RefusalAuthority {
				t.Fatalf("record %s on %q, want refused on authority", x.Outcome, x.Rule)
			}
		})
	}
}

// A search from payer-x that names payer-y's address in its query is refused
// on authority before anything reaches the provider's system: the search's
// url is written by this gateway from the sealed endpoint, never framed. At
// every level.
func TestCDexSubscribe_ASearchByAnotherPayersAddressIsRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sys := newSubStore(t)
			p := newSubProvider(t, level, sys)
			y := newForeignPayer(t, p.keys())
			f := searchFrame(t, payerEndpoint)
			f.request = "GET Subscription?url=" + url.QueryEscape(y.address.String())
			a := p.subscribe(t, f)
			if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "never carries url") {
				t.Fatalf("answer %d %s, want 403 naming the url", a.status, a.body)
			}
			if got := sys.calls(); len(got) != 0 {
				t.Fatalf("the provider's system got %+v", got)
			}
			if x := p.got.only(t); x.Outcome != ExchangeRefused || x.Rule != RefusalAuthority {
				t.Fatalf("record %s on %q, want refused on authority", x.Outcome, x.Rule)
			}
		})
	}
}

// The notification route accepts a credential only for the address it is
// posted to: payer-y's address with a credential this gateway issued for
// another address, payer-x's own, or the one it writes when payer-x frames
// payer-y's sealed values, is refused 401 on authentication before its body
// is read: the control, payer-y's own credential, gets past the credential
// check, and the same body is then read and refused 400 on routing. At every
// level.
func TestCDexNotify_ACredentialForAnotherAddressIsRefused(t *testing.T) {
	for _, level := range allLevels {
		for name, row := range map[string]struct {
			credential func(p *subProvider, y foreignPayer) (auth, channel string)
			refused    bool
		}{
			"payer-y's own credential": {func(_ *subProvider, y foreignPayer) (string, string) { return y.credential, y.sealedHeaders }, false},
			"payer-x's credential for its own address": {func(p *subProvider, _ foreignPayer) (string, string) {
				se := payerKeys(t).SealEndpoint(payerBinding, payerEndpoint)
				sh, err := payerKeys(t).SealHeaders(payerBinding, se, nil)
				if err != nil {
					t.Fatal(err)
				}
				return p.keys().Credential(subscription.Address{Ingress: cdexIngressBase, Topic: cdextopic.TokenValue, Subscriber: "payer-x", SealedEndpoint: se}, sh), sh
			}, true},
			"payer-x's credential for payer-y's sealed values": {func(p *subProvider, y foreignPayer) (string, string) {
				a := y.address
				a.Subscriber = "payer-x"
				return p.keys().Credential(a, y.sealedHeaders), y.sealedHeaders
			}, true},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newSubProvider(t, level, newSubStore(t))
				y := newForeignPayer(t, p.keys())
				auth, channel := row.credential(p, y)
				r := httptest.NewRequest(http.MethodPost, strings.TrimPrefix(y.address.String(), cdexIngressBase), strings.NewReader(`{}`))
				r.Header.Set("Content-Type", "application/fhir+json")
				r.Header.Set(subscription.AuthorizationHeader, auth)
				r.Header.Set(subscription.ChannelHeader, channel)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RouteCDexNotify)).ServeHTTP(rec, r)
				x := p.got.only(t)
				if !row.refused {
					// The credential verifies, so the body is read: {} is not a
					// notification, refused 400 on routing.
					if rec.Code != http.StatusBadRequest || x.Rule != RefusalRouting {
						t.Fatalf("%d %s (record %q), want the credential to verify and the body refused 400 on routing", rec.Code, rec.Body.String(), x.Rule)
					}
					return
				}
				if rec.Code != http.StatusUnauthorized || x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByProviderGateway || x.Rule != RefusalAuthentication {
					t.Fatalf("%d %s, record %s by %q on %q, want 401 refused by the provider's gateway on authentication", rec.Code, rec.Body.String(), x.Outcome, x.RefusedBy, x.Rule)
				}
			})
		}
	}
}
