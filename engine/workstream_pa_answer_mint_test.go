//go:build !shn_coreonly

package engine

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"golang.org/x/crypto/nacl/box"
)

// The room an answer that frames its own headers keeps under the sealed limit
// holds a leg token and a full involved list (maxInvolved tokens), each with
// every field long and escaped in the envelope's JSON: what is minted after
// the size is checked never takes such an answer over the limit.
func TestAnswerHeaderMintMarginHoldsTheTokens(t *testing.T) {
	long := strings.Repeat("x", 256)
	tok := signTestToken(shnsdk.Token{
		Operation: long, Scope: long, Subject: strings.Repeat("f", 64), Frame: long,
		CorrelationID: long, Holder: long, ConsentRef: long, PayloadHash: strings.Repeat("e", 64),
		Expiry: fakeAuthzNow, Involvement: long,
	}, fakeAuthzKey)
	js, err := tokenJSON(tok)
	if err != nil {
		t.Fatal(err)
	}
	involved := make([]shnsdk.InvolvedToken, maxInvolved)
	for i := range involved {
		involved[i] = shnsdk.InvolvedToken{Token: js, Involvement: long}
	}
	list, err := shnsdk.EncodeInvolved(involved)
	if err != nil {
		t.Fatal(err)
	}
	env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: long, Recipient: long}, Ciphertext: make([]byte, 1024)}
	bare, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken, env.Metadata.Involved = js, list
	minted, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if grew := len(minted) - len(bare); grew >= answerHeaderMintMargin {
		t.Fatalf("a token and a full involved list add %d bytes, not under answerHeaderMintMargin's %d", grew, answerHeaderMintMargin)
	}
}

// An answer whose own headers leave it no room under the sealed limit is
// sealed without them before its token is minted: it is delivered, authorized
// once, and only the headers it framed are recorded as over-answer-limit (a
// DTR answer's address headers among them). One with room carries them, also
// authorized once.
func TestAnAnswerNearTheLimitIsAuthorizedOnce(t *testing.T) {
	g, authz, _, _ := involvedGateway(t, false)
	pub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reg := shnsdk.NewRegistry()
	reg.Set("provider-gw", shnsdk.RegistryEntry{ID: "provider-gw", Role: "provider", EncPub: pub, MessageFrames: []string{shnsdk.MessageFrameV1}})
	g.cfg.Reg = reg
	answer := statedAnswer{status: http.StatusOK, headers: map[string]string{
		shnsdk.FrameHeaderETag: `"v1"`, shnsdk.FrameHeaderRetryAfter: "30",
		// Not carriable: recorded as such, never as over-answer-limit.
		shnsdk.FrameHeaderLastModified: "Sun, 01 Nov 2026 00:00:00 GMT\r\n",
	}, address: map[string]string{
		// A DTR answer's address headers, for the requester's gateway: left
		// out with the others, never at the cost of the answer.
		shnsdk.FrameHeaderOperationBase:        "https://payer.example/fhir/dtr",
		shnsdk.FrameHeaderOperationAddressTags: "/forms/a=" + strings.Repeat("A", 22),
	}}
	// sealedWithHeaders is the size of the answer's envelope, framed with its
	// headers, before any token: what the margin is checked against.
	sealedWithHeaders := func(n int) int {
		x := &exchangeRecorder{}
		frame := g.successFrame(context.WithValue(context.Background(), exchangeRecorderKey{}, x), "provider-gw", answer, "application/fhir+json", "")
		b, _, err := frame(make([]byte, n), true)
		if err != nil {
			t.Fatal(err)
		}
		env, err := shnsdk.Seal(shnsdk.Metadata{Sender: "provider", Recipient: "provider-gw"}, b, pub)
		if err != nil {
			t.Fatal(err)
		}
		out, err := shnsdk.EncodeEnvelope(env)
		if err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	// The largest body whose framed envelope stays inside the limit less the margin.
	lo, hi := 0, shnsdk.MaxResponseBytes
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if sealedWithHeaders(mid) <= shnsdk.MaxResponseBytes-answerHeaderMintMargin {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for name, c := range map[string]struct {
		size    int
		carried bool
	}{
		"with room":         {lo - 16<<10, true}, // the leg's own metadata is a little longer than the probe's
		"inside the margin": {lo + 4<<10, false},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(authz.calls())
			x := &exchangeRecorder{}
			r := httptest.NewRequest(http.MethodPost, "/inbound", nil).WithContext(context.WithValue(context.Background(), exchangeRecorderKey{}, x))
			p := relayedResponse(make([]byte, c.size))
			frame := g.successFrame(r.Context(), "provider-gw", answer, "application/fhir+json", "")
			out, status, msg := g.buildResponseLeg(r, "payer", "pas-submit", "pas-claim", "corr-1", p, answerKey("pas-claim", relay.OutcomeAnswered), frame, heldPCI(t, "MBR-COVERED"), "provider-gw", "")
			if status != 0 || len(out) == 0 {
				t.Fatalf("status %d %q", status, msg)
			}
			if got := len(authz.calls()) - before; got != 1 {
				t.Fatalf("authorized %d times, want once", got)
			}
			if len(out) > shnsdk.MaxResponseBytes {
				t.Fatalf("envelope %d bytes over the limit", len(out))
			}
			want := []string{"Last-Modified:" + AnswerHeaderValueNotCarriable}
			if !c.carried {
				want = append(want, "ETag:"+AnswerHeaderOverAnswerLimit, "Retry-After:"+AnswerHeaderOverAnswerLimit,
					shnsdk.FrameHeaderOperationAddressTags+":"+AnswerHeaderOverAnswerLimit, shnsdk.FrameHeaderOperationBase+":"+AnswerHeaderOverAnswerLimit)
			}
			if got := x.rec.AnswerHeadersNotCarried; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("recorded %v, want %v", got, want)
			}
		})
	}
}

// An answer that frames no headers, at the limit as sealed, that its token and
// involved list then take over it, is refused (too large, delivered) after
// one authorization, and never sealed again: once minted, an answer is not
// re-sealed. The largest such answer is found by the calls each attempt makes:
// a refusal before minting makes none.
func TestAnAnswerItsTokenTakesOverIsRefusedAfterOneAuthorization(t *testing.T) {
	g, authz, _, _ := involvedGateway(t, false)
	pub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reg := shnsdk.NewRegistry()
	reg.Set("provider-gw", shnsdk.RegistryEntry{ID: "provider-gw", Role: "provider", EncPub: pub, MessageFrames: []string{shnsdk.MessageFrameV1}})
	g.cfg.Reg = reg
	answer := statedAnswer{status: http.StatusOK}
	leg := func(n int) (int, string, int) {
		before := len(authz.calls())
		x := &exchangeRecorder{}
		r := httptest.NewRequest(http.MethodPost, "/inbound", nil).WithContext(context.WithValue(context.Background(), exchangeRecorderKey{}, x))
		frame := g.successFrame(r.Context(), "provider-gw", answer, "application/fhir+json", "")
		_, status, msg := g.buildResponseLeg(r, "payer", "pas-submit", "pas-claim", "corr-1", relayedResponse(make([]byte, n)), answerKey("pas-claim", relay.OutcomeAnswered), frame, heldPCI(t, "MBR-COVERED"), "provider-gw", "")
		return status, msg, len(authz.calls()) - before
	}
	// The largest body that passes the check before minting (it is authorized).
	lo, hi := 0, shnsdk.MaxResponseBytes
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if _, _, calls := leg(mid); calls > 0 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	status, msg, calls := leg(lo)
	if status != http.StatusBadGateway || msg != errResponseTooLarge || calls != 1 {
		t.Fatalf("at the limit as sealed: status %d %q after %d authorizations, want 502 %q after 1", status, msg, calls, errResponseTooLarge)
	}
	if status, msg, calls := leg(lo - 4<<10); status != 0 || calls != 1 {
		t.Fatalf("with room: status %d %q after %d authorizations, want delivered after 1", status, msg, calls)
	}
}
