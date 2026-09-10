package engine

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestInMemoryExchangeStore_BeginAppendGet(t *testing.T) {
	s := NewInMemoryExchangeStore(0, nil)
	ex := s.Begin(workstreamPA)
	if ex.ID == "" {
		t.Fatal("Begin: empty Exchange.ID")
	}
	if ex.Workstream != workstreamPA {
		t.Fatalf("Begin: workstream = %q, want %q", ex.Workstream, workstreamPA)
	}
	rec := LegRecord{Type: "crd-order-select", CorrelationID: "corr-child-1", Subjects: []string{"pci-1"}, Outcome: "approved"}
	if err := s.AppendLeg(ex.ID, rec); err != nil {
		t.Fatalf("AppendLeg: %v", err)
	}
	got, ok := s.Get(ex.ID)
	if !ok {
		t.Fatal("Get: exchange not found after AppendLeg")
	}
	if len(got.Legs) != 1 || got.Legs[0].CorrelationID != "corr-child-1" {
		t.Fatalf("Get: legs = %+v, want one leg with child corr", got.Legs)
	}
	if ex.ID == got.Legs[0].CorrelationID {
		t.Fatal("parent Exchange.ID must not equal child leg CorrelationID")
	}
}

func TestAppendLeg_UnknownExchangeFailsClosed(t *testing.T) {
	s := NewInMemoryExchangeStore(0, nil)
	if err := s.AppendLeg("no-such-exchange", LegRecord{Type: "crd-order-select"}); err == nil {
		t.Fatal("AppendLeg to unknown exchange: want error, got nil")
	}
}

func TestLegProject_DropsContent(t *testing.T) {
	leg := Leg{
		Type:     "crd-order-select",
		Physics:  paCatalog["crd-order-select"].Physics,
		Content:  Content{WorkstreamType: workstreamPA, Bytes: []byte(`{"clinical":"secret"}`)},
		Subjects: []string{"pci-1"},
	}
	rec := leg.Project("corr-child-1", "approved")
	if rec.Type != "crd-order-select" || rec.CorrelationID != "corr-child-1" || rec.Outcome != "approved" {
		t.Fatalf("Project: metadata not carried: %+v", rec)
	}
	if len(rec.Subjects) != 1 || rec.Subjects[0] != "pci-1" {
		t.Fatalf("Project: subjects not carried: %+v", rec.Subjects)
	}
}

func TestLegRecord_HasNoBytesField(t *testing.T) {
	assertNoBytes(t, reflect.TypeOf(LegRecord{}), "LegRecord")
	assertNoBytes(t, reflect.TypeOf(Exchange{}), "Exchange")
}

func assertNoBytes(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("%s is a []byte — clinical bytes must never reach the durable Exchange seam (AI-1)", path)
		}
		assertNoBytes(t, typ.Elem(), path+"[]")
	case reflect.Ptr:
		assertNoBytes(t, typ.Elem(), path)
	case reflect.Map:
		// A map value (or key) could smuggle a []byte/Content past a struct-only walk —
		// e.g. a future LegRecord.Metadata map[string][]byte. Recurse into both.
		assertNoBytes(t, typ.Key(), path+"[key]")
		assertNoBytes(t, typ.Elem(), path+"[val]")
	case reflect.Interface:
		// An interface field is inherently unguardable: its dynamic value could be a
		// []byte or Content. Fail closed — a stored interface must be a deliberate,
		// reviewed decision, never a silent hole in the non-aggregation guarantee.
		t.Fatalf("%s is an interface — unguardable; a stored interface could carry clinical bytes (AI-1)", path)
	case reflect.Struct:
		if typ == reflect.TypeOf(Content{}) {
			t.Fatalf("%s embeds engine.Content — content must never reach the durable Exchange seam (AI-1)", path)
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			assertNoBytes(t, f.Type, path+"."+f.Name)
		}
	}
}

// advanceableClock is a manually advanced clock for the TTL rows. (Named around the
// package-level fixedClock var the native-PAS tests already own.)
func advanceableClock(t0 time.Time) (func() time.Time, func(time.Duration)) {
	cur := t0
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

func TestInMemoryExchangeStore_TTLEvicts(t *testing.T) {
	now, advance := advanceableClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewInMemoryExchangeStore(time.Hour, now)
	ex := s.Begin(workstreamPA)
	advance(time.Hour - time.Second)
	if _, ok := s.Get(ex.ID); !ok {
		t.Fatal("exchange missing inside TTL")
	}
	advance(time.Second) // created_at + TTL == now → expired (<=)
	if _, ok := s.Get(ex.ID); ok {
		t.Fatal("exchange visible at created_at+TTL")
	}
	if err := s.AppendLeg(ex.ID, LegRecord{Type: "crd"}); err == nil {
		t.Fatal("AppendLeg to an expired exchange must fail with the unknown-exchange error")
	}
}

func TestInMemoryExchangeStore_PurgeOnBeginThrottled(t *testing.T) {
	now, advance := advanceableClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewInMemoryExchangeStore(time.Minute, now)
	old := s.Begin(workstreamPA) // first Begin sweeps (nothing to sweep) and stamps lastPurge
	advance(2 * time.Minute)
	s.Begin(workstreamPA) // 2 min after the last sweep → sweeps; old is expired
	if _, held := s.exchanges[old.ID]; held {
		t.Fatal("expired exchange not purged on Begin")
	}
	sweptAt := now()
	old2 := s.Begin(workstreamPA)
	advance(30 * time.Second) // 30 s since the sweep: inside the 1-minute throttle
	s.Begin(workstreamPA)
	if s.lastPurge != sweptAt {
		t.Fatalf("purge ran inside the 1-minute throttle: lastPurge=%v sweptAt=%v", s.lastPurge, sweptAt)
	}
	if _, held := s.exchanges[old2.ID]; !held {
		t.Fatal("unexpired exchange dropped by a throttled Begin")
	}
	if _, ok := s.Get(old2.ID); !ok {
		t.Fatal("old2 is 30 s old with a 1-minute TTL and must still be visible")
	}
}

func TestInMemoryExchangeStore_GetReturnsSnapshot(t *testing.T) {
	now, _ := advanceableClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewInMemoryExchangeStore(time.Hour, now)
	ex := s.Begin(workstreamPA)
	_ = s.AppendLeg(ex.ID, LegRecord{Type: "crd", Subjects: []string{"p1"}})
	got, _ := s.Get(ex.ID)
	got.Legs[0].Subjects[0] = "mutated"
	got.Legs = append(got.Legs, LegRecord{Type: "extra"})
	again, _ := s.Get(ex.ID)
	if len(again.Legs) != 1 || again.Legs[0].Subjects[0] != "p1" {
		t.Fatal("Get must return a snapshot; caller mutation leaked into the store")
	}
}

func TestInMemoryExchangeStore_Reset(t *testing.T) {
	now, _ := advanceableClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewInMemoryExchangeStore(time.Hour, now)
	ex := s.Begin(workstreamPA)
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ex.ID); ok {
		t.Fatal("Reset left an exchange behind")
	}
}

// fakeExchangeStore records calls; AppendLeg fails when failAppend is set.
type fakeExchangeStore struct {
	mem        *inMemoryExchangeStore
	failAppend bool
	failReset  error // non-nil ⇒ Reset reports it, the shape a dead database gives
	resets     int
	begins     int
}

func (f *fakeExchangeStore) Begin(ws string) *Exchange { f.begins++; return f.mem.Begin(ws) }
func (f *fakeExchangeStore) AppendLeg(id string, rec LegRecord) error {
	if f.failAppend {
		return errors.New("store down")
	}
	return f.mem.AppendLeg(id, rec)
}
func (f *fakeExchangeStore) Get(id string) (*Exchange, bool) { return f.mem.Get(id) }
func (f *fakeExchangeStore) Reset() error {
	if f.failReset != nil {
		return f.failReset
	}
	f.resets++
	return f.mem.Reset()
}

func TestGatewayReset_CallsStoreResetNeverReassigns(t *testing.T) {
	fake := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, time.Now)}
	_, pub := newTestClientKey(t)
	_, signPriv := genED25519(t)
	sor := newCensusSoR()
	g := mustNew(t, Config{
		Role: "provider", HolderID: "provider", PayerRouter: payerRouterFor(t, "payer"),
		Identity:       shnsdk.Identity{HolderID: "provider", SignPriv: signPriv},
		IngressEnabled: true, IngressBaseURL: testIngressBaseURL,
		IngressClients: map[string]IngressClientRegistration{"c": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{ingressScope}}},
		Reg:            shnsdk.NewRegistry(), Validator: shnsdk.NewFakeValidator(), SoR: sor, Store: sor,
		Clock: ingressFixedClock(), HubURL: "http://hub.test",
		Exchanges: fake,
	})
	if err := g.Reset(); err != nil {
		t.Fatal(err)
	}
	if fake.resets != 1 {
		t.Fatalf("store Reset calls = %d, want 1", fake.resets)
	}
	g.exchanges.Begin(workstreamPA)
	if fake.begins != 1 {
		t.Fatal("Begin after Reset did not land in the configured store — Reset reassigned it")
	}
}

// routableCRDReqJSON is crdReqJSON's conformant order-select request with an INLINE
// Coverage.payor identifier, so the ingress routes (recipientForWith) to the test
// payer instead of failing closed at 422 before any exchange is opened. Everything
// downstream of routing is crdTestSystem's stub substrate.
func routableCRDReqJSON() []byte {
	const ref = "Patient/MBR-COVERED"
	base := crdReqJSON("MBR-COVERED", ref, ref)
	payor := `"payor":[{"identifier":{"system":"` + shnsdk.CMSPayerIdentity.System +
		`","value":"` + shnsdk.CMSPayerIdentity.Value + `"}}],`
	out := strings.Replace(string(base), `"resourceType":"Coverage","id":"c1",`,
		`"resourceType":"Coverage","id":"c1",`+payor, 1)
	return []byte(out)
}

func TestRecordLeg_StoreFailureIsLoggedCountedNotFatal(t *testing.T) {
	// crdTestSystem (originate_test.go) + EnableIngressForTest (auth bypass) is the
	// fixture observer_test.go uses to drive a real CRD ingress call to 200.
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded, Questionnaires: []string{"http://example.org/q"}})
	cfg := gw.cfg
	EnableIngressForTest(&cfg)
	fake := &fakeExchangeStore{mem: NewInMemoryExchangeStore(time.Hour, cfg.Clock), failAppend: true}
	cfg.Exchanges = fake
	var metricStores []string
	cfg.StoreErrorMetric = func(store string) { metricStores = append(metricStores, store) }
	gw2 := mustNew(t, cfg)

	// Swaps the process-global logger: relies on gateway/engine having no t.Parallel()
	// test (true today) — a parallel test in this package would race this buffer.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	req := httptest.NewRequest(http.MethodPost, "/cds-services/order-select-crd", bytes.NewReader(routableCRDReqJSON()))
	rec := httptest.NewRecorder()
	gw2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CRD with a failing exchange store = %d, want 200 (best-effort seam); body=%s", rec.Code, rec.Body.String())
	}
	// The CRD 200 path records exactly one leg (ingress.go: the AppendLeg after wrapCards).
	if n := strings.Count(logBuf.String(), "gateway: exchange store: append leg"); n != 1 {
		t.Fatalf("exchange store error logged %d times, want 1:\n%s", n, logBuf.String())
	}
	if len(metricStores) != 1 || metricStores[0] != storeErrExchange {
		t.Fatalf("StoreErrorMetric calls = %v, want [exchange]", metricStores)
	}
}

// The in-memory mirror's half of the failed-random-source pair (its oracle twin is
// TestExchangePg_BeginRefusesAFailedRandomSource in connectors/pgstore): a correlation
// id is what binds a leg to its exchange, so a weak or empty one is never emitted.
func TestExchangeMem_BeginRefusesAFailedRandomSource(t *testing.T) {
	s := NewInMemoryExchangeStore(time.Hour, func() time.Time { return time.Unix(0, 0) })
	restore := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = restore })
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Begin continued past a failed random source")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "crypto/rand failed generating") {
			t.Fatalf("panic = %q, want the correlation-id message shape", msg)
		}
	}()
	_ = s.Begin("da-vinci-pa")
}
