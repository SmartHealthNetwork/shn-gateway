package pgstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// creatorBarrierDB allows both initial empty reloads to finish before either
// creator can begin its real transaction.
type creatorBarrierDB struct {
	keyDB
	arrived chan struct{}
	release chan struct{}
}

var errAdoptionFault = errors.New("injected transaction failure")

// adoptionFaultDB keeps real transaction effects while failing one boundary.
// The acknowledgment-loss case commits for real before returning its error.
type adoptionFaultDB struct {
	keyDB
	fault               string
	calls               map[string]int
	ctx                 context.Context
	deadline            time.Time
	badDeadline         bool
	rowsOpen            bool
	commandWithOpenRows bool
	beforeCommit        func()
	beforeLock          func(pgx.Tx, context.Context)
}

func (d *adoptionFaultDB) note(ctx context.Context, call string) error {
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[call]++
	deadline, ok := ctx.Deadline()
	if call == "begin" {
		d.ctx = ctx
		d.deadline = deadline
	}
	if !ok || !deadline.Equal(d.deadline) || ctx != d.ctx {
		d.badDeadline = true
	}
	if d.fault == call {
		return errAdoptionFault
	}
	return nil
}

func (d *adoptionFaultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := d.note(ctx, "begin"); err != nil {
		return nil, err
	}
	tx, err := d.keyDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &adoptionFaultTx{Tx: tx, db: d}, nil
}

type adoptionFaultTx struct {
	pgx.Tx
	db *adoptionFaultDB
}

func (tx *adoptionFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	call := ""
	switch {
	case strings.HasPrefix(sql, "SET TRANSACTION"):
		call = "isolation"
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		call = "lock"
	case strings.HasPrefix(sql, "DELETE"):
		call = "delete"
	case strings.HasPrefix(sql, "INSERT"):
		call = "insert"
	}
	if tx.db.rowsOpen {
		tx.db.commandWithOpenRows = true
	}
	if err := tx.db.note(ctx, call); err != nil {
		return pgconn.CommandTag{}, err
	}
	if call == "lock" && tx.db.beforeLock != nil {
		tx.db.beforeLock(tx.Tx, ctx)
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestIngressKeyPg_HolderLockDeadlineAdoption(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	s := NewIngressKeyStore(pool, "locked-holder", now)
	old, key, err := s.SigningKey(now())
	if err != nil {
		t.Fatal(err)
	}
	advance(24 * time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adoptionRollback(t, lock) })
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ingressKeyLockID(s.holderID)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	db := &adoptionFaultDB{keyDB: pool, beforeLock: func(pgx.Tx, context.Context) { close(entered) }}
	s.pool = db
	done := make(chan error, 1)
	var workers sync.WaitGroup
	start := time.Now()
	workers.Add(1)
	go func() { defer workers.Done(); done <- s.rotateIfDue(now()) }()
	t.Cleanup(func() {
		adoptionRollback(t, lock)
		adoptionDrain(t, &workers)
	})
	mustSignal(t, entered, "creator attempting owned holder lock")
	// These complete while the holder lock is still held: the cache mutex must
	// never span transaction I/O, and another holder has a different lock identity.
	verified := make(chan coldAnswer, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		pub, ok, err := s.VerificationKey(old, now())
		verified <- coldAnswer{pub, ok, err}
	}()
	select {
	case answer := <-verified:
		if !answer.ok || answer.err != nil || !answer.pub.Equal(&key.PublicKey) {
			t.Fatalf("cached verification while creation waits=%+v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("cached verification blocked on creator")
	}
	other := NewIngressKeyStore(pool, "independent-holder", now)
	if ingressKeyLockID(s.holderID) == ingressKeyLockID(other.holderID) {
		t.Fatal("fixture holder identities collide")
	}
	otherDone := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); otherDone <- other.rotateIfDue(now()) }()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent holder blocked on unrelated lock")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("same-holder creator escaped held lock")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("creator exceeded its shared deadline")
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("holder lock deadline elapsed=%s", elapsed)
	}
	if db.badDeadline || db.calls["select"] != 0 || db.calls["rollback"] != 1 {
		t.Fatalf("timeout crossed deadline or continued transaction: %v", db.calls)
	}
	if s.signing != old || len(s.keys) != 1 {
		t.Fatal("timed-out creator published a candidate")
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1`, s.holderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("timed-out creator left %d rows", count)
	}
}

func TestIngressKeyPg_ReadCommittedAfterLockAdoption(t *testing.T) {
	pool := testPool(t)
	config := pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	strictPool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(strictPool.Close)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	const holder = "snapshot-holder"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adoptionRollback(t, owner) })
	if _, err := owner.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ingressKeyLockID(holder)); err != nil {
		t.Fatal(err)
	}
	beforeLock := make(chan struct{})
	mayLock := make(chan struct{})
	var release sync.Once
	db := &adoptionFaultDB{keyDB: strictPool, beforeLock: func(tx pgx.Tx, ctx context.Context) {
		// Take an actual snapshot before the owner's row commits. Read Committed
		// must take a new one for selection after acquiring the advisory lock.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1`, holder).Scan(&n); err != nil {
			t.Error(err)
		}
		if n != 0 {
			t.Error("pre-lock fixture snapshot was not empty")
		}
		close(beforeLock)
		select {
		case <-mayLock:
		case <-ctx.Done():
		}
	}}
	s := NewIngressKeyStore(db, holder, now)
	done := make(chan error, 1)
	go func() { done <- s.rotateIfDue(now()) }()
	t.Cleanup(func() {
		release.Do(func() { close(mayLock) })
		adoptionRollback(t, owner)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("snapshot creator did not drain")
		}
	})
	mustSignal(t, beforeLock, "pre-lock statement snapshot")
	const winner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := owner.Exec(ctx, `INSERT INTO gw_ingress_key (holder_id,kid,private_key_pem,created_at,not_after) VALUES ($1,$2,$3,$4,$5)`, holder, winner, adoptionPEM(t, elliptic.P384()), now(), now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(mayLock) })
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot creator did not finish")
	}
	if s.signing != winner || db.calls["insert"] != 0 {
		t.Fatalf("post-lock selection reused stale snapshot: key=%s commands=%v", s.signing, db.calls)
	}
}

func adoptionRollback(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := storeCtx()
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("cleanup rollback: %v", err)
	}
}

func adoptionDrain(t *testing.T, workers *sync.WaitGroup) {
	t.Helper()
	drained := make(chan struct{})
	go func() { workers.Wait(); close(drained) }()
	mustSignal(t, drained, "adoption goroutines drained")
}

func (tx *adoptionFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := tx.db.note(ctx, "select"); err != nil {
		return nil, err
	}
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	tx.db.rowsOpen = true
	return &adoptionFaultRows{Rows: rows, db: tx.db}, nil
}

func (tx *adoptionFaultTx) Commit(ctx context.Context) error {
	if tx.db.rowsOpen {
		tx.db.commandWithOpenRows = true
	}
	if tx.db.beforeCommit != nil {
		tx.db.beforeCommit()
	}
	if err := tx.db.note(ctx, "commit"); err != nil {
		return err
	}
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	if tx.db.fault == "ack_lost" {
		return errAdoptionFault
	}
	return nil
}

func (tx *adoptionFaultTx) Rollback(ctx context.Context) error {
	if err := tx.db.note(ctx, "rollback"); err != nil {
		return err
	}
	return tx.Tx.Rollback(ctx)
}

type adoptionFaultRows struct {
	pgx.Rows
	db *adoptionFaultDB
}

func (r *adoptionFaultRows) Scan(dest ...any) error {
	r.db.calls["scan"]++
	if r.db.fault == "scan" {
		return errAdoptionFault
	}
	return r.Rows.Scan(dest...)
}
func (r *adoptionFaultRows) Err() error {
	r.db.calls["rows_error"]++
	if r.db.fault == "rows_error" {
		return errAdoptionFault
	}
	return r.Rows.Err()
}
func (r *adoptionFaultRows) Close() { r.db.rowsOpen = false; r.Rows.Close() }

func adoptionPEM(t *testing.T, curve elliptic.Curve) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestIngressKeyPg_TransactionFaultAdoption(t *testing.T) {
	for _, fault := range []string{"begin", "isolation", "lock", "select", "scan", "rows_error", "delete", "insert", "commit", "ack_lost"} {
		t.Run(fault, func(t *testing.T) {
			pool := testPool(t)
			now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
			material := adoptionPEM(t, elliptic.P384())
			// An old sibling requires a scan but cannot win signing. Failed cleanup
			// must restore the expired row as well as preserve this live sibling.
			insertRawKeyRow(t, pool, "fault-holder", "11111111111111111111111111111111", material, now().Add(-24*time.Hour), now().Add(time.Minute))
			insertRawKeyRow(t, pool, "fault-holder", "22222222222222222222222222222222", material, now().Add(-48*time.Hour), now())
			db := &adoptionFaultDB{keyDB: pool, fault: fault}
			s := NewIngressKeyStore(db, "fault-holder", now)
			db.beforeCommit = func() {
				s.mu.RLock()
				defer s.mu.RUnlock()
				if len(s.keys) != 0 || s.signing != "" {
					t.Error("published a candidate before commit")
				}
			}
			if err := s.rotateIfDue(now()); !errors.Is(err, errAdoptionFault) {
				t.Fatalf("fault %s returned %v", fault, err)
			}
			if len(s.keys) != 0 || s.signing != "" {
				t.Fatal("failed transaction published a key")
			}
			if db.calls[fault] == 0 && fault != "ack_lost" {
				t.Fatalf("did not reach %s", fault)
			}
			if fault != "begin" && db.calls["rollback"] != 1 {
				t.Fatalf("rollback calls=%d", db.calls["rollback"])
			}
			if db.badDeadline || time.Until(db.deadline) > storeTimeout {
				t.Fatal("transaction commands did not share the one store deadline")
			}
			if db.commandWithOpenRows {
				t.Fatal("transaction command ran before rows closed")
			}
			var expired int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1 AND kid=$2`, s.holderID, "22222222222222222222222222222222").Scan(&expired); err != nil {
				t.Fatal(err)
			}
			if fault == "ack_lost" {
				if expired != 0 {
					t.Fatal("lost acknowledgment did not actually commit cleanup")
				}
				var committed string
				if err := pool.QueryRow(t.Context(), `SELECT kid FROM gw_ingress_key WHERE holder_id=$1 AND created_at=$2`, s.holderID, now()).Scan(&committed); err != nil {
					t.Fatal(err)
				}
				s.pool = pool
				if err := s.rotateIfDue(now()); err != nil {
					t.Fatal(err)
				}
				if s.signing != committed {
					t.Fatalf("later operation failed to adopt actual committed winner: %s != %s", s.signing, committed)
				}
			} else if expired != 1 {
				t.Fatal("failed transaction did not roll back cleanup")
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1`, s.holderID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("failed operation left unexpected rows: %d", count)
			}
		})
	}
}

func TestIngressKeyPg_ReadableWinnerAdoption(t *testing.T) {
	pool := testPool(t)
	now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	good := adoptionPEM(t, elliptic.P384())
	const holder = "winner-holder"
	const winner = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, row := range []struct {
		kid, pem        string
		created, expiry time.Time
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", good, now().Add(-time.Hour), now().Add(time.Hour)},
		{winner, good, now().Add(-time.Hour), now().Add(time.Hour)},
		{"cccccccccccccccccccccccccccccccc", "invalid key bytes", now(), now().Add(time.Hour)},
		{"dddddddddddddddddddddddddddddddd", adoptionPEM(t, elliptic.P256()), now(), now().Add(time.Hour)},
		{"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", good, now(), now()},
		{"11111111111111111111111111111111", good, now().Add(-24 * time.Hour), now().Add(time.Minute)},
	} {
		insertRawKeyRow(t, pool, holder, row.kid, row.pem, row.created, row.expiry)
	}
	insertRawKeyRow(t, pool, "other-holder", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", good, now().Add(-48*time.Hour), now())
	db := &adoptionFaultDB{keyDB: pool}
	s := NewIngressKeyStore(db, holder, now)
	if err := s.rotateIfDue(now()); err != nil {
		t.Fatal(err)
	}
	if s.signing != winner || db.calls["insert"] != 0 || db.calls["delete"] != 0 {
		t.Fatalf("readable tie winner=%s commands=%v", s.signing, db.calls)
	}
	if db.calls["commit"] != 1 || db.commandWithOpenRows || db.badDeadline {
		t.Fatalf("adoption did not commit with closed rows under one deadline: %v", db.calls)
	}
	// The same ordering must hold through the cache-adoption path, which avoids a
	// creator transaction. Include an expired-but-newer cache row to fence expiry.
	cached := NewIngressKeyStore(pool, holder, now)
	if err := cached.reload(now()); err != nil {
		t.Fatal(err)
	}
	expired := s.keys[winner]
	expired.created = now()
	expired.notAfter = now()
	cached.keys["ffffffffffffffffffffffffffffffff"] = expired
	for range 50 {
		if got := cached.newestLiveLocked(now()); got != winner {
			t.Fatalf("cache winner=%s", got)
		}
	}
	if kid, _, err := cached.SigningKey(now()); err != nil || kid != winner {
		t.Fatalf("cache adoption=%s %v", kid, err)
	}
	if _, ok, err := cached.VerificationKey("11111111111111111111111111111111", now()); !ok || err != nil {
		t.Fatalf("old sibling not retained: %v %v", ok, err)
	}
	advance(2 * time.Hour)
	if err := s.refreshOnce(now()); err != nil {
		t.Fatal(err)
	}
	var own, other int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1`, holder).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id='other-holder'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if own != 1 || other != 1 {
		t.Fatalf("cleanup crossed holder scope or retained expired own rows: own=%d other=%d", own, other)
	}
}

func TestIngressKeyPg_FailureBeforeReadableAdoption(t *testing.T) {
	for _, fault := range []string{"scan", "rows_error", "commit", "ack_lost"} {
		t.Run(fault, func(t *testing.T) {
			pool := testPool(t)
			now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
			writer := NewIngressKeyStore(pool, "existing-holder", now)
			winner, _, err := writer.SigningKey(now())
			if err != nil {
				t.Fatal(err)
			}
			db := &adoptionFaultDB{keyDB: pool, fault: fault}
			s := NewIngressKeyStore(db, writer.holderID, now)
			db.beforeCommit = func() {
				s.mu.RLock()
				defer s.mu.RUnlock()
				if s.signing != "" || len(s.keys) != 0 {
					t.Error("readable candidate published before commit")
				}
			}
			if err := s.rotateIfDue(now()); !errors.Is(err, errAdoptionFault) {
				t.Fatalf("adoption failure=%v", err)
			}
			if s.signing != "" || len(s.keys) != 0 || db.calls["insert"] != 0 {
				t.Fatal("failed readable adoption published or inserted a key")
			}
			if db.rowsOpen || db.commandWithOpenRows || db.calls["rollback"] != 1 {
				t.Fatal("failed adoption left rows open or did not roll back")
			}
			s.pool = pool
			if err := s.rotateIfDue(now()); err != nil || s.signing != winner {
				t.Fatalf("readable adoption retry=%s %v", s.signing, err)
			}
		})
	}
}

func TestIngressKeyPg_SigningFailureLeavesReplayUnspentAdoption(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	db := &adoptionFaultDB{keyDB: pool, fault: "ack_lost"}
	keys := NewIngressKeyStore(db, "replay-holder", now)
	key, pub := ackLostClientKey(t)
	srv := adoptionReplica(t, pool, keys, pub, now)
	assertion := adoptionAssertion(t, key, now(), "unspent-on-signing-error")
	if code, bearer := adoptionToken(t, srv, assertion); code != 503 || bearer != "" {
		t.Fatalf("signing failure=%d %q", code, bearer)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_replay WHERE holder_id=$1`, keys.holderID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || len(keys.keys) != 0 || keys.signing != "" {
		t.Fatal("failed signing spent replay or published a key")
	}
	// A healthy sibling adopts the committed key and can spend that same assertion.
	other := NewIngressKeyStore(pool, keys.holderID, now)
	sibling := adoptionReplica(t, pool, other, pub, now)
	code, bearer := adoptionToken(t, sibling, assertion)
	if code != 200 || bearer == "" {
		t.Fatalf("unspent assertion retry=%d %q", code, bearer)
	}
	if got := adoptionDiscovery(t, sibling, bearer); got != 200 {
		t.Fatalf("retry bearer authentication=%d", got)
	}
	if code, _ := adoptionToken(t, srv, assertion); code != 401 {
		t.Fatalf("spent assertion across replicas=%d", code)
	}
}

func (b *creatorBarrierDB) Begin(ctx context.Context) (pgx.Tx, error) {
	b.arrived <- struct{}{}
	select {
	case <-b.release:
		return b.keyDB.Begin(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type adoptionReloadDB struct {
	keyDB
	hold    atomic.Bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *adoptionReloadDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.hold.Load() {
		d.once.Do(func() { close(d.started) })
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return d.keyDB.Query(ctx, sql, args...)
}

func adoptionReplica(t *testing.T, pool *pgxpool.Pool, keys *IngressKeyStore, pub []byte, now func() time.Time) *httptest.Server {
	t.Helper()
	id, err := shnsdk.GenerateIdentity(keys.holderID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := engine.New(engine.Config{
		Role: "provider", HolderID: keys.holderID, Identity: id,
		SoR: ackLostSoR{}, Store: engine.NewMemStore(), Clock: now,
		IngressEnabled: true, IngressBaseURL: ackLostIngressBase,
		IngressClients: map[string]engine.IngressClientRegistration{
			"br-provider": {Alg: "ES384", PublicKeyPEM: pub, Scopes: []string{"system/Davinci.write"}},
		},
		IngressKeys: keys, Replay: NewReplayStore(pool, keys.holderID, now),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func adoptionAssertion(t *testing.T, key *ecdsa.PrivateKey, now time.Time, jti string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodES384, jwt.MapClaims{
		"iss": "br-provider", "sub": "br-provider", "aud": ackLostIngressBase + "/oauth/token",
		"jti": jti, "iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adoptionToken(t *testing.T, srv *httptest.Server, assertion string) (int, string) {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.PostForm(srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("token JSON: %v", err)
	}
	if resp.StatusCode != 200 && doc.AccessToken != "" {
		t.Fatal("failed token response issued a bearer")
	}
	return resp.StatusCode, doc.AccessToken
}

func adoptionDiscovery(t *testing.T, srv *httptest.Server, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/cds-services", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		var doc struct{ Services []struct{ ID, Hook string } }
		if err := json.Unmarshal(body, &doc); err != nil || len(doc.Services) != 1 || doc.Services[0].ID != "order-select-crd" || doc.Services[0].Hook != "order-select" {
			t.Fatalf("authenticated discovery did not return the advertised service: %s (%v)", body, err)
		}
	}
	return resp.StatusCode
}

// Losing the shared creator decision must fail the very first authenticated call,
// even after the old throttle window has elapsed. No later reload can rescue it.
func TestIngressKeyPg_ConcurrentCreatorAdoption(t *testing.T) {
	for _, tc := range []struct {
		name               string
		signA, signB, held bool
		elapsed            time.Duration
	}{
		{"refresh_refresh_at_seven_seconds", false, false, false, 7 * time.Second},
		{"signing_signing", true, true, false, 7 * time.Second},
		{"mixed", false, true, false, 7 * time.Second},
		{"immediate_throttled_held_reload", false, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			now, advance := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
			counted := &countingDB{keyDB: pool}
			barrier := &creatorBarrierDB{keyDB: counted, arrived: make(chan struct{}, 2), release: make(chan struct{})}
			dbB := &adoptionReloadDB{keyDB: barrier, started: make(chan struct{}), release: make(chan struct{})}
			a := NewIngressKeyStore(barrier, "adoption-holder", now)
			b := NewIngressKeyStore(dbB, "adoption-holder", now)
			a.refreshing.Store(true)
			b.refreshing.Store(true)
			var release sync.Once
			done := make(chan error, 2)
			var workers sync.WaitGroup
			t.Cleanup(func() {
				release.Do(func() { close(barrier.release) })
				adoptionDrain(t, &workers)
			})
			for i, s := range []*IngressKeyStore{a, b} {
				sign := []bool{tc.signA, tc.signB}[i]
				workers.Add(1)
				go func() {
					defer workers.Done()
					if sign {
						_, _, err := s.SigningKey(now())
						done <- err
					} else {
						done <- s.refreshOnce(now())
					}
				}()
			}
			mustSignal(t, barrier.arrived, "first creator reached Begin")
			mustSignal(t, barrier.arrived, "second creator reached Begin")
			if !a.loaded || !b.loaded || len(a.keys) != 0 || len(b.keys) != 0 {
				t.Fatal("both reloads must finish empty before creation")
			}
			release.Do(func() { close(barrier.release) })
			for range 2 {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("creator did not finish")
				}
			}
			advance(tc.elapsed)
			if tc.held {
				dbB.hold.Store(true)
				reloaded := make(chan struct{})
				go func() { defer close(reloaded); _ = b.reload(now()) }()
				t.Cleanup(func() { close(dbB.release); mustSignal(t, reloaded, "later reload drained") })
				mustSignal(t, dbB.started, "later reload held")
				if !b.throttled(now()) {
					t.Fatal("miss throttle is not armed")
				}
			}
			clientKey, pub := ackLostClientKey(t)
			srvA := adoptionReplica(t, pool, a, pub, now)
			srvB := adoptionReplica(t, pool, b, pub, now)
			assertion := adoptionAssertion(t, clientKey, now(), "creator-first")
			code, bearer := adoptionToken(t, srvA, assertion)
			if code != 200 || bearer == "" {
				t.Fatalf("mint = %d %q", code, bearer)
			}
			queries := atomic.LoadInt32(&counted.queries)
			first := adoptionDiscovery(t, srvB, bearer)
			var n int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gw_ingress_key WHERE holder_id=$1`, a.holderID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			t.Logf("completed creators: A=%s B=%s rows=%d elapsed=%s first authenticated HTTP=%d", a.signing, b.signing, n, tc.elapsed, first)
			if first != http.StatusOK {
				t.Errorf("first valid cross-replica call = %d, want 200", first)
			}
			if n != 1 || a.signing != b.signing || !a.keys[a.signing].key.Equal(b.keys[b.signing].key) {
				t.Errorf("creators must cache one identical committed winner (rows=%d)", n)
			}
			if atomic.LoadInt32(&counted.queries) != queries {
				t.Fatal("first call queried instead of using the adopted cache key")
			}
			if t.Failed() {
				return
			}
			if code, _ := adoptionToken(t, srvB, assertion); code != 401 {
				t.Fatalf("cross-replica assertion replay = %d", code)
			}
			parts := strings.Split(bearer, ".")
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatal(err)
			}
			sig[0] ^= 1
			if got := adoptionDiscovery(t, srvB, parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(sig)); got != 401 {
				t.Fatalf("signature mutation = %d", got)
			}
			var header map[string]any
			hb, err := base64.RawURLEncoding.DecodeString(parts[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(hb, &header); err != nil {
				t.Fatal(err)
			}
			header["kid"] = "ffffffffffffffffffffffffffffffff"
			claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var claims jwt.MapClaims
			if err := json.Unmarshal(claimsBytes, &claims); err != nil {
				t.Fatal(err)
			}
			// Re-sign after changing only the kid, so refusal proves the unknown-key
			// guard rather than merely repeating the invalid-signature control.
			unknown := jwt.NewWithClaims(jwt.SigningMethodES384, claims)
			unknown.Header = header
			unknownBearer, err := unknown.SignedString(a.keys[a.signing].key)
			if err != nil {
				t.Fatal(err)
			}
			if got := adoptionDiscovery(t, srvB, unknownBearer); got != 401 {
				t.Fatalf("unknown valid-shape kid = %d", got)
			}
			// Expire only the key; the same JWT's exp remains five minutes ahead.
			b.mu.Lock()
			expired := b.keys[b.signing]
			expired.notAfter = now()
			b.keys[b.signing] = expired
			b.mu.Unlock()
			if got := adoptionDiscovery(t, srvB, bearer); got != 401 {
				t.Fatalf("key expiry independent of JWT expiry = %d", got)
			}
		})
	}
}
