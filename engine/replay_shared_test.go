package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// sharedReplayPair builds two payer Gateways that differ only in identity of the
// process — same holder, same keys, ONE ReplayStore — the shape of two replicas
// behind a shared record. Config mirrors relay_inbound_test.go's payer literal.
func sharedReplayPair(t *testing.T, authzPub ed25519.PublicKey, hubPub ed25519.PublicKey, clock func() time.Time) (*Gateway, *Gateway) {
	t.Helper()
	id, err := shnsdk.GenerateIdentity("payer")
	if err != nil {
		t.Fatal(err)
	}
	shared := NewInMemoryReplayStore()
	mk := func() *Gateway { return replayGateway(t, id, authzPub, hubPub, clock, shared) }
	return mk(), mk()
}

// replayGateway is one payer Gateway over the given ReplayStore. opts tweak the Config
// before construction (the handler-level rows below need an Audit Plane, a real HTTP
// client and a store-error metric hook; every other caller passes none).
func replayGateway(t *testing.T, id shnsdk.Identity, authzPub, hubPub ed25519.PublicKey, clock func() time.Time, replay ReplayStore, opts ...func(*Config)) *Gateway {
	t.Helper()
	sor := newCensusSoR()
	cfg := Config{
		Role:            "payer",
		HolderID:        "payer",
		Identity:        id,
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: hubPub,
		Reg:             shnsdk.NewRegistry(),
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Responder:       unusedResponder{},
		Clock:           clock,
		Client:          &http.Client{Transport: http.NewFileTransport(http.Dir(t.TempDir()))},
		Replay:          replay,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return mustNew(t, cfg)
}

func TestHubAssertion_SharedReplayRejectsAtOtherReplica(t *testing.T) {
	authzPub, _, _ := ed25519.GenerateKey(rand.Reader)
	hubPub, hubPriv, _ := ed25519.GenerateKey(rand.Reader)
	clock := func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	a, b := sharedReplayPair(t, authzPub, hubPub, clock)

	as := shnsdk.IssueAssertion("hub", "payer", hubPriv, clock(), time.Minute)
	raw, _ := json.Marshal(as)
	hdr := base64.StdEncoding.EncodeToString(raw)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil)
		r.Header.Set("X-Hub-Assertion", hdr)
		return r
	}
	if ok, _ := a.verifyHubAssertion(req()); !ok {
		t.Fatal("first presentation at A must verify")
	}
	if ok, _ := b.verifyHubAssertion(req()); ok {
		t.Fatal("same jti presented at B must be rejected (shared record)")
	}
}

func TestPatientAccess_SharedReplayRejectsAtOtherReplica(t *testing.T) {
	authzPub, authzPriv, _ := ed25519.GenerateKey(rand.Reader)
	hubPub, _, _ := ed25519.GenerateKey(rand.Reader)
	clock := func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	a, b := sharedReplayPair(t, authzPub, hubPub, clock)

	// Field set copied from the passing patient-access token in
	// test/adversarial (mintToken): the bindings patientAccessToken asserts are
	// Frame "patient-access", Operation "patient-access-read", Holder "phg".
	tok := signTestToken(shnsdk.Token{
		Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-1",
		Frame: "patient-access", Holder: "phg", CorrelationID: "corr-1",
		Expiry: clock().Add(time.Hour),
	}, authzPriv)
	raw, _ := json.Marshal(tok)
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/patient-access/Patient/p-1", nil)
		r.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
		return r
	}
	if _, ok, _ := a.patientAccessToken(req()); !ok {
		t.Fatal("first presentation at A must verify")
	}
	if _, ok, _ := b.patientAccessToken(req()); ok {
		t.Fatal("same correlationId presented at B must be rejected (shared record)")
	}
}

// A store that cannot tell must refuse: neither gate may admit an unrecorded jti or
// correlation. The stand-in answers (replay=false, err) — a store that did NOT claim a
// replay but could not tell — which is precisely the shape a caller reading only the
// bool would let through. The refusal is REPORTED as an outage (unavailable=true), which
// is what turns it into a 503 at the route instead of a denial; the rows below pin both
// halves. One row per scope; both legs run through the SAME store outage a DSN
// deployment sees.
func TestReplayStoreOutage_HubAssertionAndPatientAccessFailClosed(t *testing.T) {
	authzPub, authzPriv, _ := ed25519.GenerateKey(rand.Reader)
	hubPub, hubPriv, _ := ed25519.GenerateKey(rand.Reader)
	clock := func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	id, err := shnsdk.GenerateIdentity("payer")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("hub assertion", func(t *testing.T) {
		as := shnsdk.IssueAssertion("hub", "payer", hubPriv, clock(), time.Minute)
		raw, _ := json.Marshal(as)
		req := func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", nil)
			r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(raw))
			return r
		}
		// Control: the same assertion verifies against a healthy record, so the refusal
		// below is the outage and not a malformed assertion.
		if ok, unavailable := replayGateway(t, id, authzPub, hubPub, clock, NewInMemoryReplayStore()).verifyHubAssertion(req()); !ok || unavailable {
			t.Fatalf("control: a valid assertion must verify against a healthy record (ok=%v unavailable=%v)", ok, unavailable)
		}
		down := &stubReplayStore{err: errors.New("store down")}
		ok, unavailable := replayGateway(t, id, authzPub, hubPub, clock, down).verifyHubAssertion(req())
		if ok {
			t.Fatal("a Hub assertion must be refused while the one-time-use record is unavailable")
		}
		if !unavailable {
			t.Fatal("the refusal must be reported as a store outage, not as a failed assertion (the route answers 503, not 403)")
		}
		if down.calls != 1 {
			t.Fatalf("record consulted %d times, want 1", down.calls)
		}
	})

	t.Run("patient access read", func(t *testing.T) {
		tok := signTestToken(shnsdk.Token{
			Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-1",
			Frame: "patient-access", Holder: "phg", CorrelationID: "corr-1",
			Expiry: clock().Add(time.Hour),
		}, authzPriv)
		raw, _ := json.Marshal(tok)
		req := func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/patient-access/Patient/p-1", nil)
			r.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
			return r
		}
		if _, ok, unavailable := replayGateway(t, id, authzPub, hubPub, clock, NewInMemoryReplayStore()).patientAccessToken(req()); !ok || unavailable {
			t.Fatalf("control: a valid token must verify against a healthy record (ok=%v unavailable=%v)", ok, unavailable)
		}
		down := &stubReplayStore{err: errors.New("store down")}
		_, ok, unavailable := replayGateway(t, id, authzPub, hubPub, clock, down).patientAccessToken(req())
		if ok {
			t.Fatal("a patient-access read must be refused while the one-time-use record is unavailable")
		}
		if !unavailable {
			t.Fatal("the refusal must be reported as a store outage, not as a bad token (the route answers 503, not 401)")
		}
		if down.calls != 1 {
			t.Fatalf("record consulted %d times, want 1", down.calls)
		}
	})
}

// An outage refusal that names its cause, driven through the REAL handlers. During a
// database failover every in-flight Hub-authenticated delivery and every patient-access
// read reaches the one-time-use record as an error; answering the ordinary denial
// (403/401) tells an honest caller its credential is bad. Both routes must answer the
// same 503 shape the token endpoint answers, and only for a store error — a replay keeps
// its denial. Neither request survives (the Hub has no retry and reports a non-2xx here
// as a failed forward); what 503 fixes is the CAUSE the sender records. Each row also pins the store-error
// metric: an outage is only visible to an operator if every refusing path counts it.
func TestReplayStoreOutage_InboundAndPatientAccessAnswer503(t *testing.T) {
	authzPub, authzPriv, _ := ed25519.GenerateKey(rand.Reader)
	hubPub, hubPriv, _ := ed25519.GenerateKey(rand.Reader)
	clock := func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	id, err := shnsdk.GenerateIdentity("payer")
	if err != nil {
		t.Fatal(err)
	}
	// serve drives one request through the payer's real mux over the given store and
	// returns the recorder plus the store names the metric hook was called with.
	serve := func(t *testing.T, replay ReplayStore, req *http.Request, opts ...func(*Config)) (*httptest.ResponseRecorder, []string) {
		t.Helper()
		var stores []string
		opts = append(opts, func(c *Config) { c.StoreErrorMetric = func(s string) { stores = append(stores, s) } })
		g := replayGateway(t, id, authzPub, hubPub, clock, replay, opts...)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec, stores
	}

	t.Run("hub-authenticated delivery", func(t *testing.T) {
		as := shnsdk.IssueAssertion("hub", "payer", hubPriv, clock(), time.Minute)
		raw, _ := json.Marshal(as)
		// A body that would decode: the request is refused at the hop-auth gate before
		// it is ever read, so what comes back names the record, never the envelope.
		req := func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader(`{"metadata":{}}`))
			r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(raw))
			return r
		}
		rec, stores := serve(t, &stubReplayStore{err: errors.New("store down")}, req())
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("store outage = %d, want 503 (a database outage must not be audited as a refused delivery); body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "one-time-use record unavailable") {
			t.Fatalf("body = %s, want the one-time-use record named", rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store (an outage answer must not be cached past the outage)", rec.Header().Get("Cache-Control"))
		}
		if len(stores) != 1 || stores[0] != storeErrReplay {
			t.Fatalf("store-error metric calls = %v, want exactly [%s]", stores, storeErrReplay)
		}
		// A replay keeps the denial it has always had…
		rec, stores = serve(t, &stubReplayStore{replay: true}, req())
		if rec.Code != http.StatusForbidden {
			t.Fatalf("replayed assertion = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "missing or invalid hub assertion") {
			t.Fatalf("replay body = %s, want the hub-assertion denial", rec.Body.String())
		}
		if len(stores) != 0 {
			t.Fatalf("a replay is not a store error, but the metric fired %v", stores)
		}
		// …and a healthy record opens the gate: the body is read and decoded, and the
		// refusal that comes back is the envelope's own (this stub envelope carries no
		// authority frame). The full 200 delivery is the control row in
		// test/engineconformance's hop-authority table, which drives a real sealed leg.
		rec, stores = serve(t, NewInMemoryReplayStore(), req())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "missing authority frame") {
			t.Fatalf("healthy record = %d %s, want the delivery past the hop-auth gate and into the envelope", rec.Code, rec.Body.String())
		}
		if len(stores) != 0 {
			t.Fatalf("healthy record fired the store-error metric %v", stores)
		}
	})

	t.Run("patient-access read", func(t *testing.T) {
		// An Audit Plane that accepts the append, and one EOB for the subject: the
		// healthy row must reach a real 200, so the 503 below is the record and not a
		// half-wired fixture.
		audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(audit.Close)
		withEOB := func(c *Config) {
			c.AuditURL = audit.URL
			c.Client = audit.Client()
			if err := c.Store.RecordEOB("p-1", "eob-1", []byte(`{"resourceType":"ExplanationOfBenefit","id":"eob-1"}`)); err != nil {
				t.Fatal(err)
			}
		}
		corr := 0
		req := func() *http.Request {
			corr++
			tok := signTestToken(shnsdk.Token{
				Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-1",
				Frame: "patient-access", Holder: "phg", CorrelationID: fmt.Sprintf("corr-%d", corr),
				Expiry: clock().Add(time.Hour),
			}, authzPriv)
			raw, _ := json.Marshal(tok)
			r := httptest.NewRequest(http.MethodGet, "/ExplanationOfBenefit?patient=p-1", nil)
			r.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
			return r
		}
		rec, stores := serve(t, &stubReplayStore{err: errors.New("store down")}, req(), withEOB)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("store outage = %d, want 503 (a database outage must not read as a bad token); body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "one-time-use record unavailable") {
			t.Fatalf("body = %s, want the one-time-use record named", rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
		}
		if strings.Contains(rec.Body.String(), "ExplanationOfBenefit") {
			t.Fatalf("the refusal disclosed EOB content: %s", rec.Body.String())
		}
		if len(stores) != 1 || stores[0] != storeErrReplay {
			t.Fatalf("store-error metric calls = %v, want exactly [%s]", stores, storeErrReplay)
		}
		rec, stores = serve(t, &stubReplayStore{replay: true}, req(), withEOB)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("replayed correlation = %d, want 401", rec.Code)
		}
		if len(stores) != 0 {
			t.Fatalf("a replay is not a store error, but the metric fired %v", stores)
		}
		rec, stores = serve(t, NewInMemoryReplayStore(), req(), withEOB)
		if rec.Code != http.StatusOK {
			t.Fatalf("healthy record = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(stores) != 0 {
			t.Fatalf("healthy record fired the store-error metric %v", stores)
		}
	})
}

// countingReplayStore records every key it is asked about, so a row can prove a caller
// refused an oversized key WITHOUT consulting the record.
type countingReplayStore struct {
	inner ReplayStore
	keys  []string
}

func (c *countingReplayStore) CheckAndRecord(scope, clientID, key string, now, expiresAt time.Time) (bool, error) {
	c.keys = append(c.keys, key)
	return c.inner.CheckAndRecord(scope, clientID, key, now, expiresAt)
}

// TestReplayKeyLengthBound_RefusedByEveryCallerBeforeTheStore: the one-time-use key is
// caller-supplied (a jti, a correlationId) and is part of the durable record's primary
// key, so a multi-kilobyte value overflows the btree index row on a HEALTHY database —
// which the store reports as a failure and the route answers as a 503 with a counted
// store error. An oversized credential field must instead be refused as what it is: a bad
// credential, with each caller's ordinary denial, before the store is consulted at all.
//
// One row per caller, driven through the real handlers, plus the in-memory store's own
// refusal (its Postgres twin is TestReplayPg's parity table).
func TestReplayKeyLengthBound_RefusedByEveryCallerBeforeTheStore(t *testing.T) {
	authzPub, authzPriv, _ := ed25519.GenerateKey(rand.Reader)
	hubPub, hubPriv, _ := ed25519.GenerateKey(rand.Reader)
	clock := func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	id, err := shnsdk.GenerateIdentity("payer")
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("j", MaxReplayKeyBytes+1) // 513 bytes: one past the bound

	serve := func(t *testing.T, req *http.Request) (*httptest.ResponseRecorder, *countingReplayStore, []string) {
		t.Helper()
		var stores []string
		rec := &countingReplayStore{inner: NewInMemoryReplayStore()}
		g := replayGateway(t, id, authzPub, hubPub, clock, rec, func(c *Config) {
			c.StoreErrorMetric = func(s string) { stores = append(stores, s) }
		})
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, req)
		return w, rec, stores
	}

	t.Run("hub assertion", func(t *testing.T) {
		// SIGNED over the oversized jti (the signing payload is the assertion with Sig
		// nil), so the assertion itself is valid and the length bound is the only thing
		// that can refuse it — a tampered jti would fail the signature first and prove
		// nothing.
		as := shnsdk.IssueAssertion("hub", "payer", hubPriv, clock(), time.Minute)
		as.JTI = oversized
		as.Sig = nil
		payload, _ := json.Marshal(as)
		as.Sig = ed25519.Sign(hubPriv, payload)
		raw, _ := json.Marshal(as)
		r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader(`{"metadata":{}}`))
		r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(raw))
		w, rec, stores := serve(t, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("oversized hub jti = %d, want 403 (the ordinary denial); body=%s", w.Code, w.Body.String())
		}
		if len(rec.keys) != 0 {
			t.Fatalf("the one-time-use record was consulted with %d oversized keys, want 0", len(rec.keys))
		}
		if len(stores) != 0 {
			t.Fatalf("an oversized jti counted a store error %v — it is a bad credential, not an outage", stores)
		}
	})

	t.Run("patient access read", func(t *testing.T) {
		tok := signTestToken(shnsdk.Token{
			Operation: "patient-access-read", Scope: "patient-access-only", Subject: "p-1",
			Frame: "patient-access", Holder: "phg", CorrelationID: oversized,
			Expiry: clock().Add(time.Hour),
		}, authzPriv)
		raw, _ := json.Marshal(tok)
		r := httptest.NewRequest(http.MethodGet, "/ExplanationOfBenefit", nil)
		r.Header.Set("Authorization", "Bearer "+base64.StdEncoding.EncodeToString(raw))
		w, rec, stores := serve(t, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("oversized correlationId = %d, want 401 (the ordinary denial); body=%s", w.Code, w.Body.String())
		}
		if len(rec.keys) != 0 {
			t.Fatalf("the one-time-use record was consulted with %d oversized keys, want 0", len(rec.keys))
		}
		if len(stores) != 0 {
			t.Fatalf("an oversized correlationId counted a store error %v", stores)
		}
	})

	// The in-memory store refuses one too (defence in depth; the Postgres oracle refuses
	// it identically — pgstore's parity table carries that half).
	if replay, err := NewInMemoryReplayStore().CheckAndRecord(ReplayScopeIngressJTI, "c", oversized, clock(), clock().Add(time.Minute)); !replay || err == nil {
		t.Fatalf("in-memory store on an oversized key = replay:%v err:%v; want true and an error", replay, err)
	}
}
