package checks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newReachableRunner builds a Runner with a single reachable target against srv,
// so Run() completes quickly and deterministically.
func newReachableRunner(srv *httptest.Server) *Runner {
	return NewRunner([]Target{{ID: "svc", Kind: KindReachable, URL: srv.URL}}, srv.Client(), time.Now)
}

// req builds a GET/POST request against path with the given RemoteAddr and
// (optional) bearer token header.
func req(method, remoteAddr, bearer string) *http.Request {
	r := httptest.NewRequest(method, "/internal/checks", nil)
	r.RemoteAddr = remoteAddr
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// ---- token-gated access ----

func TestHandler_TokenSet_NoHeader401(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "10.1.2.3:5555", ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandler_TokenSet_WrongToken401(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "10.1.2.3:5555", "not-the-secret"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandler_TokenSet_RightTokenWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)
	if _, err := rn.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	h := Handler(rn, "secret")
	w := httptest.NewRecorder()
	// Non-loopback RemoteAddr proves the token gate, not the loopback gate, is
	// what's granting access here.
	h.ServeHTTP(w, req(http.MethodGet, "10.1.2.3:5555", "secret"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ---- loopback-gated access (token unset) ----

func TestHandler_TokenUnset_LoopbackWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)
	if _, err := rn.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "127.0.0.1:54321", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_TokenUnset_IPv6LoopbackWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)
	if _, err := rn.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "[::1]:54321", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_TokenUnset_NonLoopback403(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "10.1.2.3:5555", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestHandler_TokenUnset_UnparseableRemoteAddr403(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "")
	w := httptest.NewRecorder()
	// No port at all — net.SplitHostPort fails, must not be treated as loopback.
	h.ServeHTTP(w, req(http.MethodGet, "127.0.0.1", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// ---- GET semantics ----

func TestHandler_GET_BeforeFirstRun503(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "127.0.0.1:1", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "checks have not completed yet" {
		t.Fatalf("body = %v, want error=%q", body, "checks have not completed yet")
	}
}

func TestHandler_GET_AfterRun200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)
	if _, err := rn.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodGet, "127.0.0.1:1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Results   []Result `json:"results"`
		CheckedAt string   `json:"checkedAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Results) != 1 || body.Results[0].ID != "svc" {
		t.Fatalf("results = %+v, want one result for target svc", body.Results)
	}
	if body.CheckedAt == "" {
		t.Fatalf("checkedAt missing from response")
	}
}

// ---- POST semantics ----

func TestHandler_POST_Runs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)

	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodPost, "127.0.0.1:1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Results []Result `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Results) != 1 {
		t.Fatalf("results = %+v, want 1", body.Results)
	}
}

func TestHandler_POST_Busy409(t *testing.T) {
	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rn := newReachableRunner(srv)

	h := Handler(rn, "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req(http.MethodPost, "127.0.0.1:1", ""))
	}()
	<-handlerStarted // by now rn.busy is set

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodPost, "127.0.0.1:1", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "checks already running" {
		t.Fatalf("body = %v, want error=%q", body, "checks already running")
	}

	close(release)
	<-done
}

func TestHandler_POST_CooldownReturnsCached200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	rn := newReachableRunner(srv)
	if _, err := rn.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodPost, "127.0.0.1:1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cooldown-cached)", w.Code)
	}
}

// ---- method gating ----

func TestHandler_MethodNotAllowed(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodDelete, "127.0.0.1:1", ""))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

// The auth gate must apply BEFORE the method check — an unauthorized DELETE
// must not leak a 405 (which would confirm the endpoint exists/accepts other
// verbs) to an unauthenticated caller; it must look identical to any other
// unauthorized request.
func TestHandler_AuthCheckedBeforeMethod(t *testing.T) {
	rn := NewRunner(nil, http.DefaultClient, time.Now)
	h := Handler(rn, "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req(http.MethodDelete, "10.1.2.3:1", ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth gate must precede method check)", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("content-type = %q, want json", w.Header().Get("Content-Type"))
	}
}
