package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// resettableExchangeStore is the store seam under test: engine.ExchangeStore plus the
// admin Reset that Gateway.Reset calls through engine's (unexported) resettableStore.
// Both backends satisfy it, so the parity table can exercise Reset on both.
type resettableExchangeStore interface {
	engine.ExchangeStore
	Reset() error
}

func leg(typ string) engine.LegRecord {
	return engine.LegRecord{Type: typ, CorrelationID: "corr-" + typ, Subjects: []string{"p1"},
		Physics: engine.LegPhysics{Kind: engine.KindRequestResponse, Effect: engine.EffectReadOnly, Timing: engine.TimingSync, Locality: engine.LocalitySubstrate},
		Outcome: "ok"}
}

// countRows is the direct read of what the store left behind; a discarded Scan error
// would leave n == 0 and pass a "row is gone" assertion vacuously.
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// exchangeRows is the mem↔pg parity table: every row must hold on BOTH backends (the
// mirror may be neither stricter nor looser than the oracle). mk builds a fresh store
// over the given clock with a one-hour TTL.
func exchangeRows(t *testing.T, mk func(now func() time.Time) resettableExchangeStore) {
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	t.Run("patient-agnostic leg (nil Subjects) round-trips", func(t *testing.T) {
		// ingress.go records the DTR $questionnaire-package leg with Subjects: subjectsOf("")
		// == nil. pgx encodes a nil []string as SQL NULL; the pg store must coalesce it, and
		// both backends must hand it back the same way (empty ⇒ nil).
		now, _ := fixedClock(t0)
		s := mk(now)
		ex := s.Begin("da-vinci-pa")
		dtr := leg("dtr")
		dtr.Subjects = nil
		if err := s.AppendLeg(ex.ID, dtr); err != nil {
			t.Fatalf("nil-Subjects leg rejected: %v", err)
		}
		got, ok := s.Get(ex.ID)
		if !ok || len(got.Legs) != 1 {
			t.Fatalf("Get = %+v %v", got, ok)
		}
		if !reflect.DeepEqual(got.Legs[0], dtr) {
			t.Fatalf("leg not round-tripped: got %+v want %+v", got.Legs[0], dtr)
		}
		if got.Legs[0].Subjects != nil {
			t.Fatal("empty subjects must come back as nil on both backends")
		}
	})
	t.Run("exact TTL boundary: created_at + TTL == now is expired", func(t *testing.T) {
		now, advance := fixedClock(t0)
		s := mk(now)
		ex := s.Begin("da-vinci-pa")
		advance(time.Hour - time.Second)
		if _, ok := s.Get(ex.ID); !ok {
			t.Fatal("one second before the boundary must still be visible")
		}
		advance(time.Second)
		if _, ok := s.Get(ex.ID); ok {
			t.Fatal("created_at + TTL == now must be expired")
		}
		if err := s.AppendLeg(ex.ID, leg("crd")); err == nil || !strings.Contains(err.Error(), "unknown exchange") {
			t.Fatalf("append at the boundary = %v, want unknown exchange", err)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		now, _ := fixedClock(t0)
		s := mk(now)
		if _, ok := s.Get("nope"); ok {
			t.Fatal("unknown id visible")
		}
		if err := s.AppendLeg("nope", leg("crd")); err == nil || !strings.Contains(err.Error(), "unknown exchange") {
			t.Fatalf("append to unknown = %v", err)
		}
	})
	t.Run("Begin then Get with no legs", func(t *testing.T) {
		now, _ := fixedClock(t0)
		s := mk(now)
		ex := s.Begin("da-vinci-pa")
		got, ok := s.Get(ex.ID)
		if !ok || got.ID != ex.ID || got.Workstream != "da-vinci-pa" || got.Legs == nil || len(got.Legs) != 0 {
			t.Fatalf("Get = %+v %v (Legs must be an empty, non-nil slice on both backends)", got, ok)
		}
	})
	t.Run("Get is a snapshot", func(t *testing.T) {
		now, _ := fixedClock(t0)
		s := mk(now)
		ex := s.Begin("da-vinci-pa")
		if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
			t.Fatal(err)
		}
		got, ok := s.Get(ex.ID)
		if !ok {
			t.Fatal("Get after AppendLeg reported absent")
		}
		got.Legs[0].Subjects[0] = "mutated"
		got.Legs = append(got.Legs, leg("dtr"))
		again, ok := s.Get(ex.ID)
		if !ok {
			t.Fatal("second Get reported absent")
		}
		if len(again.Legs) != 1 || again.Legs[0].Subjects[0] != "p1" {
			t.Fatal("Get must return a snapshot")
		}
	})
	t.Run("Reset empties the holder", func(t *testing.T) {
		now, _ := fixedClock(t0)
		s := mk(now)
		ex := s.Begin("da-vinci-pa")
		if err := s.Reset(); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.Get(ex.ID); ok {
			t.Fatal("Reset left an exchange")
		}
	})
}

func TestExchangeParity_Mem(t *testing.T) {
	exchangeRows(t, func(now func() time.Time) resettableExchangeStore {
		return engine.NewInMemoryExchangeStore(time.Hour, now)
	})
}

func TestExchangeParity_Pg(t *testing.T) {
	pool := testPool(t)
	n := 0
	exchangeRows(t, func(now func() time.Time) resettableExchangeStore {
		n++
		// a distinct holder per row keeps rows independent without re-creating the schema
		return NewExchangeStore(pool, "h-"+string(rune('a'+n)), time.Hour, now)
	})
}

// exchangeRows' mk hardcodes a one-hour TTL, so a zero-TTL parity row would need
// mk threaded with a ttl parameter and every existing call site in exchangeRows
// updated to pass one explicitly — a wider diff than this standalone pair, which
// exercises the same fact (mirror == oracle at the constructor boundary) without
// touching the table. A non-positive TTL must mean "use the default," not
// "expire immediately": engine.NewInMemoryExchangeStore already normalizes
// ttl<=0 to its defaultExchangeTTL, and NewExchangeStore must match it.
func TestExchangeZeroTTL_Mem(t *testing.T) {
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := engine.NewInMemoryExchangeStore(0, now)
	ex := s.Begin("da-vinci-pa")
	if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ex.ID); !ok {
		t.Fatal("zero TTL must default, not evict the exchange at birth")
	}
}

func TestExchangeZeroTTL_Pg(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewExchangeStore(pool, "holder", 0, now)
	ex := s.Begin("da-vinci-pa")
	if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ex.ID); !ok {
		t.Fatal("zero TTL must default like the in-memory store, not evict the exchange at birth")
	}
}

func TestExchangePg_BeginAppendGetRoundTrip(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewExchangeStore(pool, "holder", time.Hour, now)
	ex := s.Begin("da-vinci-pa")
	if ex == nil || ex.ID == "" {
		t.Fatal("Begin must return an exchange with an id")
	}
	if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLeg(ex.ID, leg("dtr")); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(ex.ID)
	if !ok || got.Workstream != "da-vinci-pa" || len(got.Legs) != 2 || got.Legs[0].Type != "crd" || got.Legs[1].Type != "dtr" {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if got.Legs[0].Physics != leg("crd").Physics || got.Legs[0].Subjects[0] != "p1" || got.Legs[0].Outcome != "ok" {
		t.Fatalf("leg fields not round-tripped: %+v", got.Legs[0])
	}
	if got.Legs[0].CorrelationID != "corr-crd" || got.Legs[1].CorrelationID != "corr-dtr" {
		t.Fatalf("correlation ids not round-tripped: %+v", got.Legs)
	}
	// Get is a snapshot.
	got.Legs[0].Subjects[0] = "mutated"
	again, ok := s.Get(ex.ID)
	if !ok {
		t.Fatal("second Get reported absent")
	}
	if again.Legs[0].Subjects[0] != "p1" {
		t.Fatal("Get must return a snapshot")
	}
	// Visible from a second instance (the durable half of the seam).
	other := NewExchangeStore(pool, "holder", time.Hour, now)
	if _, ok := other.Get(ex.ID); !ok {
		t.Fatal("exchange not visible from a second instance")
	}
	// …and never from another holder.
	stranger := NewExchangeStore(pool, "other-holder", time.Hour, now)
	if _, ok := stranger.Get(ex.ID); ok {
		t.Fatal("exchange leaked across holders")
	}
}

func TestExchangePg_UnknownAndExpiredFailClosed(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewExchangeStore(pool, "holder", time.Hour, now)
	if err := s.AppendLeg("nope", leg("crd")); err == nil || !strings.Contains(err.Error(), "unknown exchange") {
		t.Fatalf("append to unknown = %v", err)
	}
	ex := s.Begin("da-vinci-pa")
	advance(time.Hour) // created_at + TTL == now → expired
	if _, ok := s.Get(ex.ID); ok {
		t.Fatal("expired exchange visible")
	}
	if err := s.AppendLeg(ex.ID, leg("crd")); err == nil {
		t.Fatal("append to expired exchange must fail")
	}
}

// The sweep on Begin is THROTTLED (one purge per exchangePurgeInterval) and the purge
// takes the legs with it (ON DELETE CASCADE). Both halves are asserted: an expired
// exchange survives a Begin inside the throttle window and is gone after one outside it.
func TestExchangePg_PurgeOnBeginThrottledCascadesLegs(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	// TTL well under exchangePurgeInterval so an exchange can be expired while the
	// throttle is still armed — the state the throttle half needs.
	s := NewExchangeStore(pool, "holder", 10*time.Second, now)
	advance(2 * exchangePurgeInterval)
	old := s.Begin("da-vinci-pa") // this Begin's purge arms the throttle at now()
	if err := s.AppendLeg(old.ID, leg("crd")); err != nil {
		t.Fatal(err)
	}
	advance(exchangePurgeInterval / 2) // old is expired (TTL 10s) but the throttle is armed
	if _, ok := s.Get(old.ID); ok {
		t.Fatal("expired exchange must not be readable, purged or not")
	}
	s.Begin("da-vinci-pa")
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange WHERE exchange_id=$1`, old.ID); n != 1 {
		t.Fatalf("expired exchange rows after a throttled Begin = %d, want 1 (the sweep must not run inside the throttle)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange_leg WHERE exchange_id=$1`, old.ID); n != 1 {
		t.Fatalf("leg rows after a throttled Begin = %d, want 1", n)
	}
	advance(exchangePurgeInterval/2 + time.Second) // now ≥ one interval since the last purge
	s.Begin("da-vinci-pa")
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange WHERE exchange_id=$1`, old.ID); n != 0 {
		t.Fatalf("expired exchange rows after the sweep = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange_leg WHERE exchange_id=$1`, old.ID); n != 0 {
		t.Fatal("legs of a purged exchange survived (ON DELETE CASCADE)")
	}
}

func TestExchangePg_ConcurrentAppendsGetDistinctSeq(t *testing.T) {
	pool := testPool(t)
	s := NewExchangeStore(pool, "holder", time.Hour, time.Now)
	ex := s.Begin("da-vinci-pa")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent appends failed (a duplicate seq violates the leg primary key): %v", errs)
	}
	got, ok := s.Get(ex.ID)
	if !ok {
		t.Fatal("Get reported absent")
	}
	if len(got.Legs) != 16 {
		t.Fatalf("legs = %d, want 16 (row lock serializes seq)", len(got.Legs))
	}
	// seq is 1..16 with no gap: the row lock makes MAX(seq)+1 deterministic.
	if n := countRows(t, pool, `SELECT count(DISTINCT seq) FROM gw_exchange_leg WHERE exchange_id=$1`, ex.ID); n != 16 {
		t.Fatalf("distinct seq values = %d, want 16", n)
	}
	if n := countRows(t, pool, `SELECT COALESCE(MAX(seq),0) FROM gw_exchange_leg WHERE exchange_id=$1`, ex.ID); n != 16 {
		t.Fatalf("MAX(seq) = %d, want 16 (contiguous from 1)", n)
	}
}

func TestExchangePg_ResetDeletesOnlyThisHolder(t *testing.T) {
	pool := testPool(t)
	a := NewExchangeStore(pool, "a", time.Hour, time.Now)
	b := NewExchangeStore(pool, "b", time.Hour, time.Now)
	exA, exB := a.Begin("da-vinci-pa"), b.Begin("da-vinci-pa")
	if err := a.AppendLeg(exA.ID, leg("crd")); err != nil {
		t.Fatal(err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get(exA.ID); ok {
		t.Fatal("Reset left holder a's exchange")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange_leg WHERE holder_id='a'`); n != 0 {
		t.Fatalf("legs after Reset = %d, want 0 (ON DELETE CASCADE)", n)
	}
	if _, ok := b.Get(exB.ID); !ok {
		t.Fatal("Reset touched holder b")
	}
}

func TestExchangePg_ClosedPoolIsBestEffort(t *testing.T) {
	pool := testPool(t)
	s := NewExchangeStore(pool, "holder", time.Hour, time.Now)
	// The best-effort rows below log by design; capture the process logger so the
	// package's test output stays clean and the log line itself is asserted.
	// Swapping the process-global logger is safe here: no test in this package is parallel.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var stores []string
	s.SetErrorHook(func(store string) { stores = append(stores, store) })
	pool.Close()
	ex := s.Begin("da-vinci-pa") // insert fails, logged and counted; an Exchange is still returned
	if ex == nil || ex.ID == "" {
		t.Fatal("Begin must always return an exchange")
	}
	if !strings.Contains(logBuf.String(), ex.ID) {
		t.Fatalf("a failed Begin insert must be logged with the exchange id; log = %q", logBuf.String())
	}
	// Nothing else reports this insert: the gateway only counts what AppendLeg returns,
	// so an outage that never gets as far as a leg would be invisible without this.
	if len(stores) != 1 || stores[0] != storeNameExchange {
		t.Fatalf("store-error hook calls after a failed Begin insert = %v, want exactly [%s]", stores, storeNameExchange)
	}
	err := s.AppendLeg(ex.ID, leg("crd"))
	if err == nil {
		t.Fatal("AppendLeg over a closed pool must return an error for recordLeg to count")
	}
	// …and it is NOT counted twice: AppendLeg returns its error to the gateway, which
	// counts it there.
	if len(stores) != 1 {
		t.Fatalf("store-error hook calls = %v, want the Begin insert only (AppendLeg is counted by its caller)", stores)
	}
	if strings.Contains(err.Error(), "unknown exchange") {
		t.Fatalf("a DB outage must not be reported as a caller error: %v", err)
	}
	if _, ok := s.Get(ex.ID); ok {
		t.Fatal("Get over a closed pool must report absent")
	}
	if err := s.Reset(); err == nil {
		t.Fatal("Reset over a closed pool must return the error (Gateway.Reset logs it)")
	}
}

// A failed random source must stop the exchange, on BOTH backends. The in-memory mirror
// (engine.newCorrelationID) panics rather than emit a weak or empty correlation id; this
// store used to log and continue with an all-zero id, whose primary-key collision was
// then swallowed by the best-effort insert — so unrelated legs from unrelated exchanges
// would alias one row. The oracle must never be weaker than the mirror it stands in for.
// Its twin is TestExchangeMem_BeginRefusesAFailedRandomSource in gateway/engine.
func TestExchangePg_BeginRefusesAFailedRandomSource(t *testing.T) {
	pool := testPool(t)
	s := NewExchangeStore(pool, "holder", time.Hour, time.Now)
	restore := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = restore })

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("Begin continued past a failed random source: an all-zero exchange id aliases unrelated legs onto one row")
			}
			if msg := fmt.Sprint(r); !strings.Contains(msg, "crypto/rand failed generating") {
				t.Fatalf("panic = %q, want the in-memory mirror's message shape", msg)
			}
		}()
		_ = s.Begin("da-vinci-pa")
	}()
	if n := countRows(t, pool, `SELECT count(*) FROM gw_exchange WHERE holder_id='holder'`); n != 0 {
		t.Fatalf("exchange rows = %d, want 0 (nothing may be written for an id that was never minted)", n)
	}
}

// countingExchangeDB is a hermetic exchangeDB: every round trip is counted and answers
// with fail (nil ⇒ the call succeeds). Counting is the whole point — the breaker's claim
// is about round trips NOT made, which no outcome can show on its own.
type countingExchangeDB struct {
	mu     sync.Mutex
	calls  int
	fail   error
	panics bool // a round trip that panics instead of returning (the pgx driver can)
	// duringCall runs while the round trip is "in" the database, before it returns or
	// panics. A row sets it to advance the injected clock, which is the only way to model
	// a call that takes real time against a fake pool.
	duringCall func()
}

func (d *countingExchangeDB) note() error {
	d.mu.Lock()
	panics := d.panics
	d.calls++
	err := d.fail
	during := d.duringCall
	d.mu.Unlock()
	if during != nil {
		during()
	}
	if panics {
		panic("countingExchangeDB: round trip panicked")
	}
	return err
}

func (d *countingExchangeDB) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *countingExchangeDB) setFail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = err
}

func (d *countingExchangeDB) setPanics(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.panics = v
}

func (d *countingExchangeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.note()
}

func (d *countingExchangeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, d.note()
}

func (d *countingExchangeDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{d.note()}
}

func (d *countingExchangeDB) Begin(context.Context) (pgx.Tx, error) { return nil, d.note() }

// errRow is a pgx.Row that only ever reports its error (the breaker rows never read a
// value; a healthy read has its own pg-gated rows above).
type errRow struct{ err error }

func (r errRow) Scan(...any) error {
	if r.err != nil {
		return r.err
	}
	return pgx.ErrNoRows
}

var errExchangeDBDown = errors.New("countingExchangeDB: unreachable")

// The exchange seam is best-effort, and during a database outage its cost is what makes
// an unrelated request slow: two synchronous calls per CRD/DTR/PAS request, each bounded
// only by storeTimeout, on a path (a direct-bearer relay) that needs no database at all.
// After a failure the seam therefore skips the database for exchangeBreakerOpen and
// answers exactly what it would have answered on a failure, then lets ONE probe through.
//
// Hermetic and deterministic: a counting fake for the database, an injected clock, no
// sleeps. The whole row advances less than exchangePurgeInterval, so the once-a-minute
// purge never fires and every counted call is one the row asked for.
func TestExchangePg_BreakerSkipsTheDatabaseAfterAFailure(t *testing.T) {
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	db := &countingExchangeDB{fail: errExchangeDBDown}
	s := NewExchangeStore(db, "holder", time.Hour, now)
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var stores []string
	s.SetErrorHook(func(store string) { stores = append(stores, store) })

	// The failure that opens the breaker: one round trip, counted once, logged once.
	ex := s.Begin("da-vinci-pa")
	if ex == nil || ex.ID == "" {
		t.Fatal("Begin must always return an exchange")
	}
	if db.count() != 1 {
		t.Fatalf("calls after the first Begin = %d, want 1", db.count())
	}
	if len(stores) != 1 {
		t.Fatalf("store-error hook calls = %v, want exactly one for the failed insert", stores)
	}
	if n := strings.Count(logBuf.String(), "skipping its calls"); n != 1 {
		t.Fatalf("breaker-open log lines = %d, want 1; log=%q", n, logBuf.String())
	}

	// Inside the window: no round trips at all, and the same best-effort outcomes.
	logBuf.Reset()
	for i := 0; i < 3; i++ {
		advance(time.Second) // still inside exchangeBreakerOpen
		if e := s.Begin("da-vinci-pa"); e == nil || e.ID == "" {
			t.Fatal("a skipped Begin must still hand back an exchange")
		}
		err := s.AppendLeg(ex.ID, leg("crd"))
		if err == nil {
			t.Fatal("a skipped AppendLeg must return a store error for the gateway to count")
		}
		if strings.Contains(err.Error(), "unknown exchange") {
			t.Fatalf("a skipped AppendLeg must not be reported as a caller error: %v", err)
		}
		if _, ok := s.Get(ex.ID); ok {
			t.Fatal("a skipped Get must report absent")
		}
	}
	if db.count() != 1 {
		t.Fatalf("round trips inside the open window = %d, want 0 beyond the first call", db.count()-1)
	}
	// Accounting is unchanged by the skip: one count per skipped Begin, exactly as a
	// failed insert produced — and no per-request log line.
	if len(stores) != 4 {
		t.Fatalf("store-error hook calls = %d, want 4 (the failed insert plus one per skipped Begin)", len(stores))
	}
	if logBuf.Len() != 0 {
		t.Fatalf("the open breaker logged per request: %q", logBuf.String())
	}

	// The window elapses: exactly ONE probe goes through, and a failing probe re-opens.
	advance(exchangeBreakerOpen)
	s.Begin("da-vinci-pa")
	if db.count() != 2 {
		t.Fatalf("calls after the window elapsed = %d, want 2 (one half-open probe)", db.count())
	}
	s.Begin("da-vinci-pa")
	if _, ok := s.Get(ex.ID); ok {
		t.Fatal("a skipped Get must report absent")
	}
	if db.count() != 2 {
		t.Fatalf("calls after a FAILED probe = %d, want 2 (the window re-opened)", db.count())
	}

	// The database comes back: the next probe succeeds, the breaker closes (one log
	// line), and the seam goes back to calling it on every request.
	logBuf.Reset()
	db.setFail(nil)
	advance(exchangeBreakerOpen)
	s.Begin("da-vinci-pa")
	if db.count() != 3 {
		t.Fatalf("calls at the second probe = %d, want 3", db.count())
	}
	if n := strings.Count(logBuf.String(), "reachable again"); n != 1 {
		t.Fatalf("breaker-close log lines = %d, want 1; log=%q", n, logBuf.String())
	}
	before := len(stores)
	s.Begin("da-vinci-pa")
	s.Begin("da-vinci-pa")
	if db.count() != 5 {
		t.Fatalf("calls with the breaker closed = %d, want 5 (every Begin reaches the database again)", db.count())
	}
	if len(stores) != before {
		t.Fatalf("a healthy Begin counted a store error: %v", stores[before:])
	}
}

// The fail-fast window is measured from when the round trip COMPLETED, not from the clock
// read its caller took before starting it. A call that burned the whole storeTimeout — the
// bound every call on this seam has, and exactly what an unreachable database costs — would
// otherwise open a window of exchangeBreakerOpen minus that timeout: three seconds, not the
// five the log line, the type doc and gateway/docs/DEPLOYMENT.md all state. The seam would
// go back to putting a two-second round trip in front of every request two seconds early,
// which is the cost the breaker exists to stop.
//
// The same stamp has to hold for a round trip that PANICS: reportPanic is the path that
// releases the half-open slot, and a window stamped from before the call would be short
// there too.
func TestExchangePg_BreakerWindowRunsFromTheRoundTripsCompletion(t *testing.T) {
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// assertWindow drives one guarded call that occupies the whole store timeout, then
	// pins both edges of the window it opened against the call's COMPLETION.
	assertWindow := func(t *testing.T, db *countingExchangeDB, s *ExchangeStore, advance func(time.Duration), open func()) {
		t.Helper()
		open()
		if db.count() != 1 {
			t.Fatalf("calls for the failure that opens the breaker = %d, want 1", db.count())
		}
		// The call started at t0 and came back at t0+storeTimeout, so the window runs to
		// completion+exchangeBreakerOpen. A hair before that: still skipped, no round trip.
		advance(exchangeBreakerOpen - 100*time.Millisecond)
		s.Begin("da-vinci-pa")
		if db.count() != 1 {
			t.Fatalf("a call %v after the round trip completed reached the database — the window was stamped from before the call, so it is %v short",
				exchangeBreakerOpen-100*time.Millisecond, storeTimeout)
		}
		// …and exactly at the window's end, one half-open probe goes through.
		advance(100 * time.Millisecond)
		s.Begin("da-vinci-pa")
		if db.count() != 2 {
			t.Fatalf("calls at completion+%v = %d, want 2 (the half-open probe)", exchangeBreakerOpen, db.count())
		}
	}

	t.Run("failed round trip", func(t *testing.T) {
		now, advance := fixedClock(t0)
		db := &countingExchangeDB{fail: errExchangeDBDown, duringCall: func() { advance(storeTimeout) }}
		s := NewExchangeStore(db, "holder", time.Hour, now)
		log.SetOutput(&bytes.Buffer{}) // the breaker logs by design; keep the package output clean
		t.Cleanup(func() { log.SetOutput(os.Stderr) })
		assertWindow(t, db, s, advance, func() { s.Begin("da-vinci-pa") })
	})

	t.Run("panicking round trip", func(t *testing.T) {
		now, advance := fixedClock(t0)
		db := &countingExchangeDB{panics: true, duringCall: func() { advance(storeTimeout) }}
		s := NewExchangeStore(db, "holder", time.Hour, now)
		log.SetOutput(&bytes.Buffer{})
		t.Cleanup(func() { log.SetOutput(os.Stderr) })
		assertWindow(t, db, s, advance, func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic must be re-raised for the caller, not swallowed by the breaker")
				}
				db.setPanics(false) // the probe below reports health, not another panic
			}()
			s.Begin("da-vinci-pa")
		})
	})
}

// A caller error must NOT open the breaker: appending to an exchange this holder does not
// have says nothing about whether the database is reachable, and treating it as an outage
// would stop recording every OTHER exchange's legs for five seconds.
func TestExchangePg_CallerErrorDoesNotOpenTheBreaker(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewExchangeStore(pool, "holder", time.Hour, now)
	if err := s.AppendLeg("no-such-exchange", leg("crd")); err == nil || !strings.Contains(err.Error(), "unknown exchange") {
		t.Fatalf("append to an unknown exchange = %v, want the caller error", err)
	}
	ex := s.Begin("da-vinci-pa")
	if err := s.AppendLeg(ex.ID, leg("crd")); err != nil {
		t.Fatalf("the next append must still reach the database: %v", err)
	}
	if got, ok := s.Get(ex.ID); !ok || len(got.Legs) != 1 {
		t.Fatalf("Get after the caller error = %v,%v; want the leg that was appended", got, ok)
	}
}

// A round trip that PANICS must still release the half-open slot. The slot is marked
// before the call and cleared only when the call reports, so without the panic path
// probing stays true with the window already elapsed — breakerAllows then refuses every
// later call, and one panic takes the correlation seam out for the life of the process.
// A panicking call is not evidence of health, so the window re-opens and the next probe
// arrives one window later; the panic itself is re-raised for the caller.
func TestExchangePg_PanicDuringAProbeReleasesTheHalfOpenSlot(t *testing.T) {
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	now, advance := fixedClock(t0)
	db := &countingExchangeDB{fail: errExchangeDBDown}
	s := NewExchangeStore(db, "holder", time.Hour, now)
	log.SetOutput(&bytes.Buffer{}) // the breaker logs by design; keep the package output clean
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s.Begin("da-vinci-pa") // the failure that opens the breaker
	if db.count() != 1 {
		t.Fatalf("calls after the first Begin = %d, want 1", db.count())
	}

	// The window elapses; this call is the half-open probe, and it panics.
	advance(exchangeBreakerOpen)
	db.setPanics(true)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic must be re-raised for the caller, not swallowed by the breaker")
			}
		}()
		s.Begin("da-vinci-pa")
	}()
	if db.count() != 2 {
		t.Fatalf("calls after the panicking probe = %d, want 2", db.count())
	}
	db.setPanics(false)

	// One probe per window still holds: nothing goes through until the window elapses…
	advance(exchangeBreakerOpen - time.Second)
	s.Begin("da-vinci-pa")
	if _, ok := s.Get("whatever"); ok {
		t.Fatal("a skipped Get must report absent")
	}
	if db.count() != 2 {
		t.Fatalf("calls inside the window that followed the panic = %d, want 2 (the panic must not open the gate either)", db.count())
	}
	// …and then the next probe really runs, which is what a stranded slot would prevent.
	advance(time.Second)
	db.setFail(nil)
	s.Begin("da-vinci-pa")
	if db.count() != 3 {
		t.Fatalf("calls after the window following the panic = %d, want 3 — the half-open slot was never released, so the breaker is open for good", db.count())
	}
	// The successful probe closed it: the seam calls the database again on every request.
	s.Begin("da-vinci-pa")
	if db.count() != 4 {
		t.Fatalf("calls with the breaker closed = %d, want 4", db.count())
	}
}
