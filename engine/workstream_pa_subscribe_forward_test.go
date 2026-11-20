//go:build !shn_coreonly

package engine

import (
	"net/http"
	"slices"
	"testing"
)

// cdexSubscribeRequester is the RequesterHeader value every forward on the
// HRex Task topic's subscribe leg carries for the fixtures' payer: the payer,
// and the identifiers the registry attests for it.
const cdexSubscribeRequester = `payer-x; identifiers="http://example.org/cdex/payer/payer-ids|Payer123"`

// Every forward a provider's gateway makes on the HRex Task topic's subscribe
// leg names the payer and its attested identifiers (RequesterHeader), whatever
// else the forward carries: a create's, beside the edits it discloses; an
// update's and a delete's, beside a framed If-Match or none, and the gateway's
// own read before each; a read's; and a search's. The topic registers the
// header (subscribeRecipient.subscriber), so a forward that drops it is a
// change to the leg's message, at every level.
func TestCDexSubscribe_EveryForwardNamesTheRequester(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			check := func(t *testing.T, name string, sys *subStore, want int) {
				t.Helper()
				calls := sys.calls()
				if len(calls) != want {
					t.Fatalf("%s: %d calls to the provider's system, want %d: %+v", name, len(calls), want, calls)
				}
				for i, c := range calls {
					if v := c.header.Values(RequesterHeader); !slices.Equal(v, []string{cdexSubscribeRequester}) {
						t.Errorf("%s: call %d (%s %s) carried %s %q, want %q", name, i, c.method, c.requestURI, RequesterHeader, v, cdexSubscribeRequester)
					}
				}
			}
			t.Run("create", func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				if a := p.subscribe(t, createFrame(t, cdexSubscriptionJSON(nil))); a.status/100 != 2 {
					t.Fatalf("answer %d %s", a.status, a.body)
				}
				check(t, "create", sys, 1)
				if v := sys.calls()[0].header.Values(EditedHeader); len(v) == 0 {
					t.Errorf("the create disclosed no edit beside %s", RequesterHeader)
				}
			})
			for _, c := range []struct{ request, method string }{
				{"PUT Subscription/sub-1", http.MethodPut},
				{"DELETE Subscription/sub-1", http.MethodDelete},
			} {
				for _, framed := range []bool{true, false} {
					t.Run(c.request+"/ifMatch="+map[bool]string{true: "framed", false: "none"}[framed], func(t *testing.T) {
						sys := newSubStore(t)
						p := newSubProvider(t, level, sys)
						sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
						f := subFrame{request: c.request}
						if c.method == http.MethodPut {
							f = createFrame(t, cdexSubscriptionJSON(nil))
							f.request = c.request
						}
						if framed {
							f.ifMatch = `W/"3"`
						}
						if a := p.subscribe(t, f); a.status/100 != 2 {
							t.Fatalf("answer %d %s", a.status, a.body)
						}
						check(t, c.request, sys, 2)
					})
				}
			}
			t.Run("read", func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				if a := p.subscribe(t, subFrame{request: "GET Subscription/sub-1"}); a.status/100 != 2 {
					t.Fatalf("answer %d %s", a.status, a.body)
				}
				check(t, "read", sys, 1)
			})
			t.Run("search", func(t *testing.T) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				if a := p.subscribe(t, searchFrame(t, "https://payer-sys.example/hooks/cdex")); a.status/100 != 2 {
					t.Fatalf("answer %d %s", a.status, a.body)
				}
				check(t, "search", sys, 1)
			})
		})
	}
}
