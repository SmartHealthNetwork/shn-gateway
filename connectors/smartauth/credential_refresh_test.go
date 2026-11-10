package smartauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTokenServer answers each token request with the next bearer, AT-1,
// AT-2, …, and counts them; fail makes the n-th request (1-based) fail.
func countingTokenServer(t *testing.T, hits *int32, fail int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		if n == fail {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("AT-%d", n), "token_type": "bearer", "expires_in": 300})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func refreshClient(t *testing.T, tokenURL string) *http.Client {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	hc, err := NewHTTPClient(Config{TokenURL: tokenURL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384", Key: key, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

// sentRequest is what the resource server received.
type sentRequest struct {
	header http.Header
	body   []byte
}

// resourceServer answers each request with answer(bearer) and keeps what it
// received.
func resourceServer(t *testing.T, answer func(bearer string, w http.ResponseWriter)) (*httptest.Server, func() []sentRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, sentRequest{header: r.Header.Clone(), body: b})
		mu.Unlock()
		answer(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sentRequest { mu.Lock(); defer mu.Unlock(); return slices.Clone(got) }
}

// unauthorizedFor answers 401 to bearer stale, with challenge as its
// WWW-Authenticate ("" for none), and 200 to any other.
func unauthorizedFor(stale, challenge string) func(string, http.ResponseWriter) {
	return func(bearer string, w http.ResponseWriter) {
		if bearer == stale {
			if challenge != "" {
				w.Header().Set("WWW-Authenticate", challenge)
			}
			http.Error(w, `{"resourceType":"OperationOutcome"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

func post(t *testing.T, ctx context.Context, hc *http.Client, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set("X-Correlation-Id", "corr-812")
	req.Header.Set("X-Participant", "a")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A 401 from the resource server to the cached bearer drops that bearer, and
// the request is sent once more with a fresh one: byte for byte the same
// request, the Authorization header alone changed. The observation counts the
// resend and the challenge the 401 carried.
func TestBearerTransport_A401RefreshesTheCredentialAndResendsTheRequestOnce(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	rs, sent := resourceServer(t, unauthorizedFor("AT-1", `Bearer realm="payer", error="invalid_token", error_description="expired"`))
	hc := refreshClient(t, ts.URL)
	body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	ctx, obs := WithCredentialRefreshObservation(context.Background())
	resp := post(t, ctx, hc, rs.URL, body)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != `{"ok":true}` {
		t.Fatalf("answer %d %s, want the resend's answer", resp.StatusCode, b)
	}
	got := sent()
	if len(got) != 2 || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("%d sends and %d token fetches, want 2 and 2", len(got), tokens)
	}
	if got[0].header.Get("Authorization") != "Bearer AT-1" || got[1].header.Get("Authorization") != "Bearer AT-2" {
		t.Fatalf("bearers %q then %q, want AT-1 then AT-2", got[0].header.Get("Authorization"), got[1].header.Get("Authorization"))
	}
	if !bytes.Equal(got[0].body, body) || !bytes.Equal(got[1].body, body) {
		t.Fatalf("bodies %q and %q, want both exactly %q", got[0].body, got[1].body, body)
	}
	first, second := got[0].header.Clone(), got[1].header.Clone()
	first.Del("Authorization")
	second.Del("Authorization")
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("headers differ beyond Authorization:\n%v\n%v", first, second)
	}
	if want := []string{ChallengeInvalidToken}; !slices.Equal(obs.Resends(), want) {
		t.Fatalf("resends %q, want %q", obs.Resends(), want)
	}
}

// Exactly one resend: a 401 to the fresh bearer too is the participant's
// answer, relayed as it is, and nothing is sent a third time.
func TestBearerTransport_ASecond401IsTheAnswer(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	rs, sent := resourceServer(t, func(_ string, w http.ResponseWriter) {
		http.Error(w, `{"second":"no"}`, http.StatusUnauthorized)
	})
	hc := refreshClient(t, ts.URL)
	ctx, obs := WithCredentialRefreshObservation(context.Background())
	resp := post(t, ctx, hc, rs.URL, []byte(`{}`))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), `"second":"no"`) {
		t.Fatalf("answer %d %s, want the second 401 as sent", resp.StatusCode, b)
	}
	if n := len(sent()); n != 2 || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("%d sends and %d token fetches, want 2 and 2 (no loop)", n, tokens)
	}
	if want := []string{ChallengeNone}; !slices.Equal(obs.Resends(), want) {
		t.Fatalf("resends %q, want %q", obs.Resends(), want)
	}
}

// Only a 401 refreshes: a 403, or any other status, is the answer, and the
// cached bearer stays.
func TestBearerTransport_OnlyA401Refreshes(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var tokens int32
			ts := countingTokenServer(t, &tokens, 0)
			rs, sent := resourceServer(t, func(_ string, w http.ResponseWriter) { http.Error(w, "no", status) })
			hc := refreshClient(t, ts.URL)
			ctx, obs := WithCredentialRefreshObservation(context.Background())
			for range 2 {
				resp := post(t, ctx, hc, rs.URL, []byte(`{}`))
				resp.Body.Close()
				if resp.StatusCode != status {
					t.Fatalf("answer %d, want %d", resp.StatusCode, status)
				}
			}
			if n := len(sent()); n != 2 || atomic.LoadInt32(&tokens) != 1 || len(obs.Resends()) != 0 {
				t.Fatalf("%d sends, %d token fetches, resends %q: want 2, 1, none", n, tokens, obs.Resends())
			}
		})
	}
}

// A request whose body cannot be read again is not resent: its 401 is the
// answer. The stale bearer is dropped all the same, so the next request goes
// with a fresh one.
func TestBearerTransport_AnUnreplayableBodyIsNotResent(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	rs, sent := resourceServer(t, unauthorizedFor("AT-1", ""))
	hc := refreshClient(t, ts.URL)
	ctx, obs := WithCredentialRefreshObservation(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rs.URL, io.NopCloser(strings.NewReader(`{"once":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("fixture: the body is replayable")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(sent()) != 1 || len(obs.Resends()) != 0 {
		t.Fatalf("answer %d, %d sends, resends %q: want the 401, 1 send, none", resp.StatusCode, len(sent()), obs.Resends())
	}
	resp = post(t, context.Background(), hc, rs.URL, []byte(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("next request %d with %d token fetches, want 200 with a fresh bearer", resp.StatusCode, tokens)
	}
}

// Many requests refused together with the same stale bearer share one fresh
// one: one token fetch, and each request sent exactly twice, its first send
// refused and its resend answered.
func TestBearerTransport_Concurrent401sShareOneRefresh(t *testing.T) {
	const n = 8
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	hc := refreshClient(t, ts.URL)
	// Hold every first send until all n have arrived with AT-1, so each is
	// refused with the same stale bearer before any resend.
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	var refused atomic.Int32
	rs, sent := resourceServer(t, func(bearer string, w http.ResponseWriter) {
		if bearer == "AT-1" {
			arrived.Done()
			<-release
			refused.Add(1)
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	go func() { arrived.Wait(); close(release) }()
	var wg sync.WaitGroup
	codes := make([]int, n)
	resends := make([][]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, obs := WithCredentialRefreshObservation(context.Background())
			resp := post(t, ctx, hc, rs.URL, []byte(fmt.Sprintf(`{"i":%d}`, i)))
			codes[i] = resp.StatusCode
			resp.Body.Close()
			resends[i] = obs.Resends()
		}()
	}
	wg.Wait()
	for i := range n {
		if codes[i] != http.StatusOK || !slices.Equal(resends[i], []string{ChallengeNone}) {
			t.Fatalf("request %d: answer %d, resends %q; want 200 after one resend", i, codes[i], resends[i])
		}
	}
	got := sent()
	fresh := 0
	for _, r := range got {
		if r.header.Get("Authorization") == "Bearer AT-2" {
			fresh++
		}
	}
	if len(got) != 2*n || refused.Load() != n || fresh != n || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("%d sends (%d refused, %d with the fresh bearer) over %d token fetches; want %d, %d, %d and 2", len(got), refused.Load(), fresh, tokens, 2*n, n, n)
	}
}

// When the fresh bearer cannot be obtained, the participant's 401 is the
// answer, as it arrived: nothing is sent a second time, no resend is recorded,
// and the request is not marked a token acquisition failure (it was sent and
// answered). The next request fetches again.
func TestBearerTransport_ARefreshThatFailsLeavesThe401AsTheAnswer(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 2)
	rs, sent := resourceServer(t, func(bearer string, w http.ResponseWriter) {
		if bearer == "AT-1" {
			http.Error(w, `{"theirs":"401"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	hc := refreshClient(t, ts.URL)
	ctx, acq := WithTokenAcquisitionObservation(context.Background())
	ctx, obs := WithCredentialRefreshObservation(ctx)
	resp := post(t, ctx, hc, rs.URL, []byte(`{}`))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || string(b) != "{\"theirs\":\"401\"}\n" {
		t.Fatalf("answer %d %q, want the participant's 401 as it arrived", resp.StatusCode, b)
	}
	if len(sent()) != 1 || len(obs.Resends()) != 0 || acq.Failed() {
		t.Fatalf("%d sends, resends %q, acquisition failed %v: want 1, none, false", len(sent()), obs.Resends(), acq.Failed())
	}
	resp = post(t, context.Background(), hc, rs.URL, []byte(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&tokens) != 3 {
		t.Fatalf("next request %d after %d fetches, want 200 with a third fetch", resp.StatusCode, tokens)
	}
}

// A request that ends after its 401 and before a resend is not resent.
func TestBearerTransport_ARequestThatEndsIsNotResent(t *testing.T) {
	var tokens int32
	ts := countingTokenServer(t, &tokens, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, obs := WithCredentialRefreshObservation(ctx)
	rs, sent := resourceServer(t, func(_ string, w http.ResponseWriter) {
		cancel()
		http.Error(w, "expired", http.StatusUnauthorized)
	})
	hc := refreshClient(t, ts.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rs.URL, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("answer %d, want the 401 or the request's end", resp.StatusCode)
		}
	}
	if len(sent()) != 1 || len(obs.Resends()) != 0 || atomic.LoadInt32(&tokens) != 1 {
		t.Fatalf("%d sends, resends %q, %d fetches: want 1, none, 1", len(sent()), obs.Resends(), tokens)
	}
}

// Dropping a token generation that is no longer the one held keeps the newer
// one: a request refused with an old bearer never discards a fresh one another
// request already fetched, even when the authorization server issued the same
// string again.
func TestTokenSource_InvalidateKeepsANewerToken(t *testing.T) {
	var tokens int32
	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokens, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT-same", "token_type": "bearer", "expires_in": 300})
	}))
	t.Cleanup(same.Close)
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	src := &TokenSource{Config: Config{TokenURL: same.URL, ClientID: "gw", Alg: "ES384", Key: key, Clock: time.Now}}
	ctx := context.Background()
	_, first, err := src.token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.invalidate(ctx, first); err != nil {
		t.Fatal(err)
	}
	tok, second, err := src.token(ctx)
	if err != nil || tok != "AT-same" || second == first || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("after invalidating generation %d: %q generation %d, %d fetches", first, tok, second, tokens)
	}
	// A late 401 to the first generation keeps the second, though its string is the same.
	if err := src.invalidate(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, gen, _ := src.token(ctx); gen != second || atomic.LoadInt32(&tokens) != 2 {
		t.Fatalf("a stale invalidate dropped the newer token: generation %d, %d fetches", gen, tokens)
	}
}

// The challenge a 401 carried, as observed, never used to decide: a Bearer
// challenge naming invalid_token, any other challenge, or none.
func TestCredentialChallenge(t *testing.T) {
	for _, c := range []struct {
		values []string
		want   string
	}{
		{nil, ChallengeNone},
		{[]string{`Bearer error="invalid_token"`}, ChallengeInvalidToken},
		{[]string{`Bearer realm="p", error="invalid_token", error_description="x"`}, ChallengeInvalidToken},
		{[]string{`bearer error=invalid_token`}, ChallengeInvalidToken},
		{[]string{`Basic realm="x"`, `Bearer error="invalid_token"`}, ChallengeInvalidToken},
		{[]string{`Bearer error="insufficient_scope"`}, ChallengeOther},
		{[]string{`Bearer realm="p", error_description="invalid_token"`}, ChallengeOther},
		{[]string{`Basic realm="x"`}, ChallengeOther},
		{[]string{``}, ChallengeOther},
	} {
		h := http.Header{}
		for _, v := range c.values {
			h.Add("WWW-Authenticate", v)
		}
		if got := credentialChallenge(h); got != c.want {
			t.Errorf("%q: %q, want %q", c.values, got, c.want)
		}
	}
}
