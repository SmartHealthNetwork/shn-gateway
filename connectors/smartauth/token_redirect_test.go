package smartauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingOrigin is an https server that counts every request it receives,
// answering a token as a token endpoint would: a hop it receives is a
// credential that reached it.
func countingOrigin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"AT-LEAKED","token_type":"bearer","expires_in":300}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// tokenConfigs are the two ways a gateway authenticates its token request:
// client_secret_post (the secret in the form) and private_key_jwt (a signed
// assertion in the form).
func tokenConfigs(t *testing.T, tokenURL string, hc *http.Client) map[string]Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Config{
		"client_secret_post": {TokenURL: tokenURL, ClientID: "gw", ClientSecret: "OPAQUE-SECRET", Scope: "system/*.read", Clock: time.Now, HTTPClient: hc},
		"private_key_jwt":    {TokenURL: tokenURL, ClientID: "gw", Alg: "ES384", Key: key, Scope: "system/*.read", Clock: time.Now, HTTPClient: hc},
	}
}

// A token endpoint that answers the token request with a redirect is never
// followed, whatever its origin and whatever its code: a cross-origin 307 or
// 308 (which would replay the form, the client's credential), a 302, and a
// redirect within the endpoint's own origin. The origin it names receives no
// request at all. The fetch fails as a token endpoint that does not answer
// fails (TokenTransportError), its error naming the status and the origin
// only, never the Location's path or query; the hop is recorded as
// token-redirect-refused; and nothing else is sent.
func TestTokenFetch_ARedirectIsNeverFollowed(t *testing.T) {
	other, otherHits := countingOrigin(t)
	var target string
	var code int
	var tokenHits atomic.Int32
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		_, _ = io.ReadAll(r.Body)
		http.Redirect(w, r, target, code)
	}))
	t.Cleanup(tokenSrv.Close)
	cases := []struct {
		name   string
		code   int
		target func() string
		origin string
	}{
		{"a cross-origin 307", http.StatusTemporaryRedirect, func() string { return other.URL + "/collect?secret=x" }, other.URL},
		{"a cross-origin 308", http.StatusPermanentRedirect, func() string { return other.URL + "/collect?secret=x" }, other.URL},
		{"a cross-origin 302", http.StatusFound, func() string { return other.URL + "/collect?secret=x" }, other.URL},
		{"a 307 within the endpoint's origin", http.StatusTemporaryRedirect, func() string { return tokenSrv.URL + "/token2?x=1" }, tokenSrv.URL},
	}
	for _, tc := range cases {
		for auth, cfg := range tokenConfigs(t, tokenSrv.URL+"/token", tokenSrv.Client()) {
			t.Run(tc.name+"/"+auth, func(t *testing.T) {
				otherHits.Store(0)
				tokenHits.Store(0)
				target, code = tc.target(), tc.code
				ts := &TokenSource{Config: cfg}
				ctx, obs := WithRedirectObservation(context.Background())
				_, _, err := ts.token(ctx)
				var transport *TokenTransportError
				var redirect *TokenRedirectError
				if !errors.As(err, &transport) || !errors.Is(err, ErrTokenRedirect) || !errors.As(err, &redirect) {
					t.Fatalf("err %v, want a token transport failure that is a refused redirect", err)
				}
				if redirect.StatusCode != tc.code || redirect.Origin != tc.origin {
					t.Fatalf("redirect %+v, want status %d and origin %s", redirect, tc.code, tc.origin)
				}
				if msg := err.Error(); strings.Contains(msg, "collect") || strings.Contains(msg, "secret=") || strings.Contains(msg, "token2") || !strings.Contains(msg, fmt.Sprint(tc.code)) {
					t.Fatalf("error %q, want the status and origin only", msg)
				}
				if otherHits.Load() != 0 || tokenHits.Load() != 1 {
					t.Fatalf("requests: the other origin %d, the token endpoint %d; want 0 and 1", otherHits.Load(), tokenHits.Load())
				}
				if want := []string{RedirectTokenRefused}; !slices.Equal(obs.Hops(), want) {
					t.Fatalf("hops %q, want %q", obs.Hops(), want)
				}
			})
		}
	}
}

// Through the bearer transport, a token endpoint's redirect fails the
// operation as a token acquisition failure: the operation is never sent, and
// nothing reaches the origin the redirect named. The deployment's own token
// client is not changed, even one whose policy follows every redirect.
func TestTokenFetch_ARedirectFailsTheOperationAsATokenFailure(t *testing.T) {
	other, otherHits := countingOrigin(t)
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/token", http.StatusPermanentRedirect)
	}))
	t.Cleanup(tokenSrv.Close)
	system := newHopServer(t, okAnswer)
	permissive := tokenSrv.Client()
	permissive.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	for auth, cfg := range tokenConfigs(t, tokenSrv.URL+"/token", permissive) {
		t.Run(auth, func(t *testing.T) {
			otherHits.Store(0)
			hc, err := NewHTTPClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPost, system.URL+"/Claim/$submit", strings.NewReader(`{}`))
			resp, err := hc.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if !IsTokenAcquisitionError(err) || !errors.Is(err, ErrTokenRedirect) {
				t.Fatalf("err %v, want a token acquisition failure caused by the refused redirect", err)
			}
			if otherHits.Load() != 0 || len(system.seen()) != 0 {
				t.Fatalf("the other origin received %d requests and the system %d; want none", otherHits.Load(), len(system.seen()))
			}
			if permissive.CheckRedirect == nil {
				t.Fatal("the deployment's token client lost its own policy")
			}
		})
	}
}

// A 3xx net/http would not follow is refused all the same, by its status: a
// 300, a redirect with no Location, and one whose Location net/http cannot
// parse. The fetch fails as a token endpoint that does not answer fails, the
// error carries no part of the Location's path or query, and the hop is
// recorded as token-redirect-refused.
func TestTokenFetch_A3xxNetHTTPWouldNotFollowIsRefusedToo(t *testing.T) {
	var code int
	var location string
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if location != "" {
			w.Header()["Location"] = []string{location}
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(tokenSrv.Close)
	for _, tc := range []struct {
		name     string
		code     int
		location string
	}{
		{"a 300", http.StatusMultipleChoices, ""},
		{"a 302 with no Location", http.StatusFound, ""},
		{"a 307 whose Location cannot be parsed", http.StatusTemporaryRedirect, "https://other.example/%zz/collect?secret=x"},
	} {
		for auth, cfg := range tokenConfigs(t, tokenSrv.URL+"/token", tokenSrv.Client()) {
			t.Run(tc.name+"/"+auth, func(t *testing.T) {
				code, location = tc.code, tc.location
				ctx, obs := WithRedirectObservation(context.Background())
				_, _, err := (&TokenSource{Config: cfg}).token(ctx)
				var transport *TokenTransportError
				var redirect *TokenRedirectError
				if !errors.As(err, &transport) || !errors.As(err, &redirect) || redirect.StatusCode != tc.code {
					t.Fatalf("err %v, want a token transport failure that is a refused %d", err, tc.code)
				}
				if msg := err.Error(); strings.Contains(msg, "collect") || strings.Contains(msg, "secret=") || strings.Contains(msg, "%zz") {
					t.Fatalf("error %q carries the Location's path or query", msg)
				}
				if want := []string{RedirectTokenRefused}; !slices.Equal(obs.Hops(), want) {
					t.Fatalf("hops %q, want %q", obs.Hops(), want)
				}
			})
		}
	}
}

// The deployment's own token client is not changed by a fetch: sent on its
// own, it still follows the redirect its policy allows, while the token
// request sent through it follows none.
func TestTokenFetch_LeavesTheDeploymentsClientUnchanged(t *testing.T) {
	other, otherHits := countingOrigin(t)
	tokenSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/token", http.StatusPermanentRedirect)
	}))
	t.Cleanup(tokenSrv.Close)
	own := tokenSrv.Client()
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	ownTransport := own.Transport
	cfg := tokenConfigs(t, tokenSrv.URL+"/token", own)["client_secret_post"]
	if _, _, err := (&TokenSource{Config: cfg}).token(context.Background()); !errors.Is(err, ErrTokenRedirect) || otherHits.Load() != 0 {
		t.Fatalf("err %v, other origin hits %d; want a refused redirect and none", err, otherHits.Load())
	}
	if own.Transport != ownTransport {
		t.Fatal("the deployment's client's transport was replaced")
	}
	req, _ := http.NewRequest(http.MethodPost, tokenSrv.URL+"/token", strings.NewReader("x=1"))
	resp, err := own.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if otherHits.Load() != 1 {
		t.Fatalf("the deployment's client, sent on its own, reached the other origin %d times; want its own policy kept (1)", otherHits.Load())
	}
}
