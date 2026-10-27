//go:build !shn_coreonly

package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// leakingStoreErr is a driver error whose text quotes what a log must never carry: a user
// name, a host and the authentication failure itself.
var leakingStoreErr = fmt.Errorf("select ingress keys: %w", &pgconn.PgError{
	Severity: "FATAL", Code: "28P01",
	Message: `password authentication failed for user "gw_leak_user" on host db-leak.internal`,
})

const leakingStoreClass = "(store error: postgres 28P01)"

// leakingDB fails every read and every creator transaction with leakingStoreErr. With
// writes set, reads pass to keyDB and only the creator transaction fails.
type leakingDB struct {
	keyDB
	writes bool
}

func (d *leakingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.writes {
		return d.keyDB.Query(ctx, sql, args...)
	}
	return nil, leakingStoreErr
}

func (d *leakingDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, leakingStoreErr
}

// captureIngressLog collects the package log for one row.
func captureIngressLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// assertClassOnly fails unless the log holds want, with the store error's class, and none
// of the driver error's text.
func assertClassOnly(t *testing.T, logged, want string) {
	t.Helper()
	if !strings.Contains(logged, want+" "+leakingStoreClass) {
		t.Errorf("log lacks %q with the store error's class:\n%s", want, logged)
	}
	for _, leak := range []string{"gw_leak_user", "db-leak.internal", "password", "select ingress keys"} {
		if strings.Contains(logged, leak) {
			t.Errorf("log carries the driver error's text %q:\n%s", leak, logged)
		}
	}
}

// Every reload log line names the store error's class, never its text, keeping the stage
// it names: a cold replica's own reload, a warm replica's verification miss, the refresh
// loop's warm-up, an out-of-band reload, and the refresh tick's reload.
func TestIngressKeyReloadLogLinesNameTheClassNeverTheText(t *testing.T) {
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *IngressKeyStore)
		want string
	}{
		{"cold replica's reload", func(t *testing.T, s *IngressKeyStore) {
			if _, ok, err := s.VerificationKey("kid-unknown", now()); ok || err == nil {
				t.Fatalf("cold VerificationKey with the store failing = %v, %v; want a store error", ok, err)
			}
		}, "pgstore: ingress key reload failed"},
		{"warm verification miss's reload", func(t *testing.T, s *IngressKeyStore) {
			s.loaded = true // a successful read of an empty table
			if _, ok, err := s.VerificationKey("kid-unknown", now()); ok || err == nil {
				t.Fatalf("warm VerificationKey with the store failing = %v, %v; want a store error", ok, err)
			}
		}, "pgstore: ingress key reload failed"},
		{"warm-up reload", func(t *testing.T, s *IngressKeyStore) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // the warm-up runs, then the loop returns
			counted := 0
			s.RunRefresh(ctx, func(string) { counted++ })
			if counted != 1 {
				t.Fatalf("the warm-up's failure was counted %d times, want 1", counted)
			}
		}, "pgstore: ingress key warm-up reload failed"},
		{"out-of-band reload", func(t *testing.T, s *IngressKeyStore) {
			s.wakeTick(context.Background(), nil)
		}, "pgstore: ingress key reload (out of band) failed"},
		{"refresh tick's reload", func(t *testing.T, s *IngressKeyStore) {
			s.refreshTick(now(), nil)
		}, "pgstore: ingress key refresh failed: reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureIngressLog(t)
			s := NewIngressKeyStore(&leakingDB{}, "holder", now)
			tc.run(t, s)
			assertClassOnly(t, buf.String(), tc.want)
		})
	}
}

// The refresh tick's rotate stage logs the creator transaction's class, never its text,
// and so does a retry it skips straight after a token request's failed key write.
func TestIngressKeyPg_RefreshRotateLogLinesNameTheClassNeverTheText(t *testing.T) {
	pool := testPool(t)
	now, _ := fixedClock(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC))

	buf := captureIngressLog(t)
	s := NewIngressKeyStore(&leakingDB{keyDB: pool, writes: true}, "holder", now)
	s.refreshTick(now(), nil)
	assertClassOnly(t, buf.String(), "pgstore: ingress key refresh failed: rotate")

	buf = captureIngressLog(t)
	s = NewIngressKeyStore(&leakingDB{keyDB: pool, writes: true}, "holder", now)
	if _, _, err := s.SigningKey(now()); err == nil {
		t.Fatal("SigningKey created a key with Begin failing")
	}
	s.refreshTick(now(), nil)
	assertClassOnly(t, buf.String(), "pgstore: ingress key refresh failed: rotate skipped straight after a token request's failed key write")
}

// rotateError keeps the text and the chain callers have always read; only the log reads
// its fields.
func TestIngressKeyRotateErrorKeepsTheReturnedText(t *testing.T) {
	if got, want := (&rotateError{err: leakingStoreErr}).Error(), "rotate: "+leakingStoreErr.Error(); got != want {
		t.Errorf("rotateError = %q, want %q", got, want)
	}
	skipped := &rotateError{skipped: true, err: leakingStoreErr}
	if got, want := skipped.Error(), "rotate: a token request's key write failed moments ago, not retrying straight after it: "+leakingStoreErr.Error(); got != want {
		t.Errorf("skipped rotateError = %q, want %q", got, want)
	}
	var pgErr *pgconn.PgError
	if !errors.Is(skipped, leakingStoreErr) || !errors.As(skipped, &pgErr) || pgErr.Code != "28P01" {
		t.Errorf("rotateError does not unwrap to the store error")
	}
}
