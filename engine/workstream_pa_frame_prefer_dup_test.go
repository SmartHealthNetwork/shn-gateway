//go:build !shn_coreonly

package engine

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawFrame is a message frame whose header segment is seg exactly as given,
// then body: a frame no encoder writes, as a sending gateway could still
// send one.
func rawFrame(seg string, body []byte) []byte {
	out := []byte{0x00, 0x01, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[2:6], uint32(len(seg)))
	return append(append(out, seg...), body...)
}

// A frame that names Prefer twice, in another case, with different values
// cannot be read one way only, at every level: an inbound request carrying
// one (a subscriber's read, on the subscribe leg that defines Prefer) is
// refused 400 on message integrity before its leg's handler runs, and an
// answer carrying one is refused as unreadable, on integrity. Prefer named
// once, or twice with one value, is read as any frame is: the read is
// answered, and such an answer is never refused as unreadable.
func TestFrame_PreferNamedTwiceIsUnreadable(t *testing.T) {
	const twice = `{"status":200,"headers":{"subscriptionRequest":"GET Subscription/sub-1","Prefer":"return=minimal","prefer":"return=representation"}}`
	const once = `{"status":200,"headers":{"subscriptionRequest":"GET Subscription/sub-1","Prefer":"return=minimal"}}`
	const sameTwice = `{"status":200,"headers":{"subscriptionRequest":"GET Subscription/sub-1","Prefer":"return=minimal","PREFER":"return=minimal"}}`
	for _, level := range allLevels {
		t.Run(level.String()+"/request", func(t *testing.T) {
			// On a subscribe leg, where Prefer is defined: a subscriber's
			// read framed by hand.
			read := func(seg string) (subAnswer, ExchangeRecord) {
				sys := newSubStore(t)
				p := newSubProvider(t, level, sys)
				sys.store["sub-1"] = p.storedFor(t, "payer-x", cdexSubscriptionJSON(nil))
				a := p.subscribe(t, subFrame{raw: rawFrame(seg, nil)})
				return a, p.got.only(t)
			}
			a, rec := read(twice)
			if a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "request frame decode failed") ||
				rec.Outcome != ExchangeRefused || rec.Rule != RefusalIntegrity || rec.RefusedBy != RefusedByProviderGateway {
				t.Fatalf("%d %s, record %s %s/%s, want 400 request frame decode failed, refused by the provider's gateway on integrity", a.status, a.body, rec.Outcome, rec.RefusedBy, rec.Rule)
			}
			for name, seg := range map[string]string{"once": once, "twice with one value": sameTwice} {
				if a, rec := read(seg); a.status != http.StatusOK || rec.Outcome != ExchangeAnswered {
					t.Fatalf("Prefer %s: %d %s, record %s %s/%s, want the read answered", name, a.status, a.body, rec.Outcome, rec.RefusedBy, rec.Rule)
				}
			}
		})
		t.Run(level.String()+"/answer", func(t *testing.T) {
			answer := func(seg string) (*httptest.ResponseRecorder, ExchangeRecord) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				env.payerReturns(LegResult{Response: testResponse(rawFrame(seg, []byte(`{"resourceType":"Bundle","type":"collection"}`)))})
				w := httptest.NewRecorder()
				env.originator.ingressRoute(RoutePAS)(w, httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle("00001", ""))))
				return w, got.only(t)
			}
			w, rec := answer(twice)
			wantRefusal(t, rec, w.Code, RefusedByProviderGateway, RefusalIntegrity)
			// Prefer once, or twice with one value, is read: the answer is then
			// judged as any other (a bare Bundle, which the stricter levels
			// refuse on conformance), never refused as unreadable.
			for name, seg := range map[string]string{"once": once, "twice with one value": sameTwice} {
				if w, rec := answer(seg); rec.Rule == RefusalIntegrity {
					t.Fatalf("an answer naming Prefer %s: %d, refused on integrity", name, w.Code)
				}
			}
		})
	}
}
