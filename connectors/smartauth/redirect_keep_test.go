package smartauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The headers a cross-origin hop keeps, as a request sets them.
var keptForTest = http.Header{
	"Accept":          {"application/fhir+json"},
	"Accept-Language": {"en-US"},
	"User-Agent":      {"shn-gateway-test"},
}

// setForTheOriginForTest are headers a request to a participant's own system
// carries that no other origin is sent: the ones the gateway sets for it (the
// requester, the leg's correlation id, a disclosed edit, a holder assertion),
// a Referer, and one no list names yet.
var setForTheOriginForTest = http.Header{
	"X-Shn-Requester":    {"payer-net-id|ids"},
	"X-Correlation-Id":   {"leg-0123"},
	"X-Shn-Edited":       {"identity-mapping"},
	"X-Holder-Assertion": {"assertion"},
	"Referer":            {"https://first.example/start"},
	"X-Future":           {"a header not named anywhere"},
}

// sendEveryHeader sends method to url through c carrying the configured
// headers, a bearer, setForTheOriginForTest and keptForTest.
func sendEveryHeader(t *testing.T, c *http.Client, method, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for _, h := range []http.Header{configuredForTest(), setForTheOriginForTest, keptForTest} {
		for k, v := range h {
			req.Header[k] = append([]string(nil), v...)
		}
	}
	req.Header.Set("Authorization", "Bearer AT-ORIGIN")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// requireOnlyKept fails unless h, as another origin received it, holds the
// kept headers as they were set and nothing else but the Accept-Encoding the
// transport adds to every request it sends.
func requireOnlyKept(t *testing.T, who string, h http.Header) {
	t.Helper()
	for k, v := range h {
		if want, kept := keptForTest[k]; kept {
			if !slices.Equal(v, want) {
				t.Errorf("%s received %s %q, want %q", who, k, v, want)
			}
			continue
		}
		if k == "Accept-Encoding" {
			continue
		}
		t.Errorf("%s received %s %q, want only Accept, Accept-Language and User-Agent", who, k, v)
	}
	for k := range keptForTest {
		if _, ok := h[k]; !ok {
			t.Errorf("%s lost %s", who, k)
		}
	}
}

// requireEverything fails unless h, as the participant's own origin received
// it, holds every header the request was sent with.
func requireEverything(t *testing.T, who string, h http.Header) {
	t.Helper()
	for _, set := range []http.Header{configuredForTest(), setForTheOriginForTest, keptForTest, {"Authorization": {"Bearer AT-ORIGIN"}}} {
		for k, v := range set {
			if k == "Referer" {
				// net/http sets a hop's Referer to the request before it.
				if h.Get(k) == "" {
					t.Errorf("%s received no Referer", who)
				}
				continue
			}
			if !slices.Equal(h[k], v) {
				t.Errorf("%s received %s %q, want %q", who, k, h[k], v)
			}
		}
	}
}

// The clients that reach a participant's system: ParticipantClient (no
// configured headers), ParticipantClientWith over a client with no policy and
// one with its own, and NewHTTPClient's policy.
func everyParticipantClient() map[string]func() *http.Client {
	m := configuredClients()
	m["ParticipantClient"] = func() *http.Client { return ParticipantClient(&http.Client{}) }
	m["CheckRedirect"] = func() *http.Client { return &http.Client{CheckRedirect: CheckRedirect} }
	return m
}

// A read (a GET or HEAD with no body) that a participant's system redirects to
// another origin, with any redirect code, is sent there with no header but
// Accept, Accept-Language and User-Agent: no bearer, no configured header, no
// header the gateway set for the participant's system (the requester, the
// correlation id, a disclosed edit), no Referer, and none named nowhere. The
// participant's own system received every one.
func TestParticipantClient_ACrossOriginReadCarriesOnlyTheKeptHeaders(t *testing.T) {
	for name, mk := range everyParticipantClient() {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				t.Run(fmt.Sprintf("%s/%s/%d", name, method, code), func(t *testing.T) {
					other := newHeaderServer(t, false, okAnswer)
					home := newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
						http.Redirect(w, r, other.URL+"/x", code)
					})
					sendEveryHeader(t, mk(), method, home.URL+"/start")
					got, away := home.requests(), other.requests()
					if len(got) != 1 || len(away) != 1 {
						t.Fatalf("requests: origin %d, other %d; want one each", len(got), len(away))
					}
					requireEverything(t, "the participant's own system", got[0])
					requireOnlyKept(t, "the other origin", away[0])
				})
			}
		}
	}
}

// A chain within the participant's origin keeps every header on every hop.
func TestParticipantClient_AChainWithinTheOriginKeepsEveryHeader(t *testing.T) {
	for name, mk := range everyParticipantClient() {
		t.Run(name, func(t *testing.T) {
			var home *headerServer
			home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/a" {
					http.Redirect(w, r, home.URL+"/b", http.StatusFound)
					return
				}
				okAnswer(r, w)
			})
			sendEveryHeader(t, mk(), http.MethodGet, home.URL+"/a")
			got := home.requests()
			if len(got) != 2 {
				t.Fatalf("the origin received %d requests, want 2", len(got))
			}
			for i, h := range got {
				requireEverything(t, fmt.Sprintf("hop %d", i), h)
			}
		})
	}
}

// A chain that leaves the origin and comes back carries only the kept headers
// on the hop away and on the hop back, and on a hop within the other origin.
func TestParticipantClient_AChainBackToTheOriginCarriesOnlyTheKeptHeaders(t *testing.T) {
	for name, mk := range everyParticipantClient() {
		t.Run(name, func(t *testing.T) {
			var home, other *headerServer
			other = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/x" {
					http.Redirect(w, r, other.URL+"/y", http.StatusFound)
					return
				}
				http.Redirect(w, r, home.URL+"/back", http.StatusFound)
			})
			home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, other.URL+"/x", http.StatusFound)
					return
				}
				okAnswer(r, w)
			})
			sendEveryHeader(t, mk(), http.MethodGet, home.URL+"/start")
			got, away := home.requests(), other.requests()
			if len(got) != 2 || len(away) != 2 {
				t.Fatalf("requests: origin %d, other %d; want 2 each", len(got), len(away))
			}
			requireEverything(t, "the first request", got[0])
			requireOnlyKept(t, "the hop away", away[0])
			requireOnlyKept(t, "the hop within the other origin", away[1])
			requireOnlyKept(t, "the hop back to the origin", got[1])
		})
	}
}

// A header the deployment configured is dropped on a hop to another origin
// even when it has a kept header's name: it was configured for the first
// origin. The participant's own system received it as configured.
func TestParticipantClientWith_AConfiguredHeaderNamedLikeAKeptOneIsDropped(t *testing.T) {
	configured := http.Header{"User-Agent": {"partner-routing-agent"}, "accept-language": {"x-tenant-7"}}
	other := newHeaderServer(t, false, okAnswer)
	home := newHeaderServer(t, false, redirectTo(other.URL+"/x"))
	req, _ := http.NewRequest(http.MethodGet, home.URL, nil)
	req.Header.Set("User-Agent", "partner-routing-agent")
	req.Header.Set("Accept-Language", "x-tenant-7")
	req.Header.Set("Accept", "application/json")
	resp, err := ParticipantClientWith(&http.Client{}, configured).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	first, away := home.requests()[0], other.requests()[0]
	if first.Get("User-Agent") != "partner-routing-agent" || first.Get("Accept-Language") != "x-tenant-7" {
		t.Fatalf("the participant's own system received %v, want the configured values", first)
	}
	if ua := away.Get("User-Agent"); ua == "partner-routing-agent" || !strings.HasPrefix(ua, "Go-http-client/") {
		t.Errorf("the other origin received User-Agent %q, want the transport's own", ua)
	}
	if v, ok := away["Accept-Language"]; ok {
		t.Errorf("the other origin received Accept-Language %q, want none", v)
	}
	if away.Get("Accept") != "application/json" {
		t.Errorf("the other origin received Accept %q, want the request's", away.Get("Accept"))
	}
}

// The bearer client (NewHTTPClient) sends its bearer only within the origin:
// a cross-origin hop, and the hop back, carry no Authorization and no other
// header but the kept ones.
func TestNewHTTPClient_ACrossOriginHopCarriesOnlyTheKeptHeaders(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-keep","token_type":"bearer","expires_in":300}`)
	}))
	t.Cleanup(tok.Close)
	hc, err := NewHTTPClient(Config{TokenURL: tok.URL, ClientID: "c1", ClientSecret: "s1", Scope: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var home *headerServer
	other := newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, home.URL+"/back", http.StatusFound)
	})
	home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
			return
		}
		okAnswer(r, w)
	})
	req, _ := http.NewRequest(http.MethodGet, home.URL+"/start", nil)
	for k, v := range setForTheOriginForTest {
		req.Header[k] = v
	}
	for k, v := range keptForTest {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got, away := home.requests(), other.requests()
	if len(got) != 2 || len(away) != 1 {
		t.Fatalf("requests: origin %d, other %d; want 2 and 1", len(got), len(away))
	}
	if got[0].Get("Authorization") != "Bearer tok-keep" || got[0].Get("X-Shn-Requester") == "" {
		t.Fatalf("the first request carried %v, want the bearer and every header", got[0])
	}
	requireOnlyKept(t, "the other origin", away[0])
	requireOnlyKept(t, "the hop back to the origin", got[1])
}

// The CDS client JWT is minted for a hop within the origin, for that hop's
// URL, and for no hop of a chain that has left it: the hop away and the hop
// back carry only the kept headers.
func TestCDSClientJWT_AChainThatLeftTheOriginCarriesOnlyTheKeptHeaders(t *testing.T) {
	key, err := NewCDSClientKey("ES384", cdsTestKey(t, "ES384"), "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	stub := &cdsStub{t: t, jwks: key.JWKSet(), alg: "ES384", now: func() time.Time { return now }}
	var home *headerServer
	other := newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
		http.Redirect(w, r, home.URL+"/cds-services", http.StatusFound)
	})
	home = newHeaderServer(t, false, func(r *http.Request, w http.ResponseWriter) {
		switch r.URL.Path {
		case "/moved":
			if !stub.verify(r, home.URL+r.URL.Path) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, home.URL+"/away", http.StatusFound)
		case "/away":
			if !stub.verify(r, home.URL+r.URL.Path) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		default:
			_, _ = io.WriteString(w, `{"services":[]}`)
		}
	})
	hc, err := NewClientJWTHTTPClient(key, ClientJWTConfig{Issuer: fixedIssuer(testIssuer), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, home.URL+"/moved", nil)
	for k, v := range setForTheOriginForTest {
		req.Header[k] = v
	}
	for k, v := range keptForTest {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the listing", resp.StatusCode)
	}
	got, away := home.requests(), other.requests()
	if len(got) != 3 || len(away) != 1 {
		t.Fatalf("requests: origin %d, other %d; want 3 and 1", len(got), len(away))
	}
	if seen := stub.jwts(); len(seen) != 2 || seen[0].url != home.URL+"/moved" || seen[1].url != home.URL+"/away" {
		t.Fatalf("JWTs verified %v, want one for /moved and one for its same-origin hop /away", seen)
	}
	requireOnlyKept(t, "the other origin", away[0])
	requireOnlyKept(t, "the hop back to the origin", got[2])
}

// The kept headers are exactly Accept, Accept-Language and User-Agent: adding
// a name lets every other origin a participant's system redirects to see it,
// so it is a reviewed change to this row as well as to the list.
func TestCrossOriginHeaders_AreExactlyTheThree(t *testing.T) {
	if want := []string{"Accept", "Accept-Language", "User-Agent"}; !slices.Equal(crossOriginHeaders, want) {
		t.Fatalf("crossOriginHeaders %q, want exactly %q", crossOriginHeaders, want)
	}
}
