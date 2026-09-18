package smartauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// tokenServer is a hermetic SMART token endpoint: it verifies the ES384
// client_assertion (against pub, aud must equal its own URL) and returns AT-123.
// hits counts how many times it was actually called (to prove caching).
func tokenServer(t *testing.T, pub *ecdsa.PublicKey, hits *int32) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "client_credentials" {
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		_, err := jwt.Parse(r.FormValue("client_assertion"),
			func(*jwt.Token) (any, error) { return pub, nil },
			jwt.WithValidMethods([]string{"ES384"}), jwt.WithExpirationRequired(),
			jwt.WithAudience(srv.URL))
		if err != nil {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT-123", "token_type": "bearer", "expires_in": 300,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenSource_SignsExchangesAndCaches(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	var hits int32
	srv := tokenServer(t, &key.PublicKey, &hits)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	ts := &TokenSource{Config: Config{
		TokenURL: srv.URL, ClientID: "gw-payer", Scope: "system/*.read",
		Alg: "ES384", Key: key, Clock: testClock(now), HTTPClient: srv.Client(),
	}}
	tok, err := ts.Token(context.Background())
	if err != nil || tok != "AT-123" {
		t.Fatalf("Token = %q, %v; want AT-123, nil", tok, err)
	}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cached)", got)
	}
}

func TestTokenSource_EarlyRefresh(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	var hits int32
	srv := tokenServer(t, &key.PublicKey, &hits)
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	clk := &mutClock{t: base}
	ts := &TokenSource{Config: Config{
		TokenURL: srv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: clk.now, HTTPClient: srv.Client(),
	}}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.set(base.Add(239 * time.Second)) // expires_in=300, skew=60 → still cached
	_, _ = ts.Token(context.Background())
	clk.set(base.Add(241 * time.Second)) // now within skew of expiry → refresh
	_, _ = ts.Token(context.Background())
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits=%d, want 2 (initial + one refresh past skew)", got)
	}
}

func TestTokenSource_FailClosed(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	ts := &TokenSource{Config: Config{
		TokenURL: srv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: testClock(time.Now()), HTTPClient: srv.Client(),
	}}
	if tok, err := ts.Token(context.Background()); err == nil || tok != "" {
		t.Fatalf("Token = %q, %v; want \"\", error (fail-closed)", tok, err)
	}
}

func TestTokenSource_ConcurrentSingleFlight(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	var hits int32
	srv := tokenServer(t, &key.PublicKey, &hits)
	ts := &TokenSource{Config: Config{
		TokenURL: srv.URL, ClientID: "gw", Scope: "system/*.read", Alg: "ES384",
		Key: key, Clock: testClock(time.Now()), HTTPClient: srv.Client(),
	}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = ts.Token(context.Background()) }()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("concurrent Token() hit endpoint %d times, want 1", got)
	}
}

type mutClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mutClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *mutClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

// secretServer is a hermetic client_secret_post token endpoint: it requires the
// form to carry the id+secret pair and NO assertion fields, and returns AT-SEC.
func secretServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "client_credentials" ||
			r.FormValue("client_id") != "gw-payer" ||
			r.FormValue("client_secret") != "s3cret" ||
			r.FormValue("client_assertion") != "" ||
			r.FormValue("client_assertion_type") != "" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT-SEC", "token_type": "bearer", "expires_in": 300,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenSource_ClientSecretPost(t *testing.T) {
	var hits int32
	srv := secretServer(t, &hits)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	clk := &mutClock{t: now}
	ts := &TokenSource{Config: Config{
		TokenURL: srv.URL, ClientID: "gw-payer", ClientSecret: "s3cret",
		Scope: "system/*.read", Clock: clk.now, HTTPClient: srv.Client(),
	}}
	tok, err := ts.Token(context.Background())
	if err != nil || tok != "AT-SEC" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	// Second call inside the expiry window must come from cache.
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("want 1 endpoint hit (cache), got %d", got)
	}
	// Past expiry (expires_in=300, skew=60) secret mode re-mints like jwt mode.
	clk.set(now.Add(241 * time.Second))
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("want re-mint past expiry, got %d hits", got)
	}
}

func TestNewHTTPClient_ModeExclusion(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	// Both modes → construction error.
	_, err := NewHTTPClient(Config{TokenURL: "https://t", ClientID: "c",
		Alg: "ES384", Key: key, ClientSecret: "s"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want both-modes error, got %v", err)
	}
	// Neither mode → construction error (fail at boot, not first read).
	if _, err = NewHTTPClient(Config{TokenURL: "https://t", ClientID: "c"}); err == nil {
		t.Fatal("want missing-credentials error")
	}
	// Secret-only → constructs.
	if _, err = NewHTTPClient(Config{TokenURL: "https://t", ClientID: "c",
		ClientSecret: "s"}); err != nil {
		t.Fatalf("secret-only should construct: %v", err)
	}
}

// TestTokenSource_ExpiresInAsString: a real partner token endpoint (Azure AD's
// v1 issuer behind a payer's proxy) answers `"expires_in":"3599"` as a JSON
// string. RFC 6749 says number, but the partner's bytes are what they are and
// the gateway must read them: the value is accepted and used as the TTL. A
// string that is not a number is still a decode error naming the field (the
// rejection row), and a missing/zero value still falls back to the default TTL.
func TestTokenSource_ExpiresInAsString(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	serve := func(t *testing.T, body string) *TokenSource {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return &TokenSource{Config: Config{
			TokenURL: srv.URL, ClientID: "gw-payer", ClientSecret: "s3cret",
			Clock: testClock(now), HTTPClient: srv.Client(),
		}}
	}

	t.Run("numeric string is accepted as the TTL and the departure is reported once", func(t *testing.T) {
		ts := serve(t, `{"token_type":"Bearer","expires_in":"3599","access_token":"AT-STR"}`)
		var notes []string
		ts.Observer = func(note string) { notes = append(notes, note) }
		for i := 0; i < 2; i++ {
			tok, ttl, err := ts.fetch(context.Background())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if tok != "AT-STR" || ttl != 3599*time.Second {
				t.Fatalf("token=%q ttl=%s, want AT-STR / 3599s", tok, ttl)
			}
		}
		if len(notes) != 1 {
			t.Fatalf("want exactly one departure note across two fetches, got %d: %v", len(notes), notes)
		}
		if !strings.Contains(notes[0], "expires_in") || !strings.Contains(notes[0], "JSON string") || strings.Contains(notes[0], "s3cret") || strings.Contains(notes[0], "AT-STR") {
			t.Fatalf("note must name the departure and carry no secret or token: %q", notes[0])
		}
	})

	t.Run("a numeric expires_in reports nothing", func(t *testing.T) {
		ts := serve(t, `{"access_token":"AT-NUM","expires_in":300}`)
		ts.Observer = func(note string) { t.Errorf("unexpected note for a conformant response: %q", note) }
		if _, _, err := ts.fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("number still works", func(t *testing.T) {
		ts := serve(t, `{"access_token":"AT-NUM","expires_in":300}`)
		_, ttl, err := ts.fetch(context.Background())
		if err != nil || ttl != 300*time.Second {
			t.Fatalf("ttl=%s err=%v, want 300s", ttl, err)
		}
	})

	t.Run("absent falls back to the default TTL", func(t *testing.T) {
		ts := serve(t, `{"access_token":"AT-NONE"}`)
		_, ttl, err := ts.fetch(context.Background())
		if err != nil || ttl != defaultAssertionTTL {
			t.Fatalf("ttl=%s err=%v, want default", ttl, err)
		}
	})

	t.Run("non-numeric string is refused, naming expires_in", func(t *testing.T) {
		ts := serve(t, `{"access_token":"AT-BAD","expires_in":"soon"}`)
		if _, _, err := ts.fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "expires_in") {
			t.Fatalf("want decode error naming expires_in, got %v", err)
		}
	})
}
