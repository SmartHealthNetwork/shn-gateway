//go:build !shn_coreonly

package pgstore

// audit_outbox_test.go: the durable audit outbox (gw_audit_outbox). Every row
// is pg-gated (testPool skips without SHN_TEST_DATABASE_URL) and runs in the
// serial `pg` job.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
)

func outbox(t *testing.T, holder string) *AuditOutbox {
	t.Helper()
	return NewAuditOutbox(testPool(t), holder)
}

// recordID is a 26-character recordId for row i.
func recordID(i int) string { return fmt.Sprintf("01JZX5A7B8C9D0E1F2G3%06d", i) }

func entry(i int) auditappend.Entry {
	return auditappend.Entry{RecordID: recordID(i), Body: []byte(fmt.Sprintf(`{"n":%d}`, i))}
}

// TestPgAuditOutbox_QueueClaimDeferDelete: records come out oldest first, a
// claimed row is held for its lease, a deferred row waits for its time, and a
// deleted row is gone.
func TestPgAuditOutbox_QueueClaimDeferDelete(t *testing.T) {
	ctx := context.Background()
	o := outbox(t, "provider-a")
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if dropped, err := o.Enqueue(ctx, entry(i), now, auditappend.Bound); err != nil || dropped != 0 {
			t.Fatalf("Enqueue %d: dropped %d, %v", i, dropped, err)
		}
	}
	e, ok, err := o.Claim(ctx, now, time.Minute)
	if err != nil || !ok || e.RecordID != recordID(1) || !bytes.Equal(e.Body, entry(1).Body) {
		t.Fatalf("first claim: %+v %v %v, want record 1 with its exact bytes", e, ok, err)
	}
	// Held: the next claim takes the next record, not the held one.
	e2, ok, err := o.Claim(ctx, now, time.Minute)
	if err != nil || !ok || e2.RecordID != recordID(2) {
		t.Fatalf("second claim: %+v %v %v, want record 2", e2, ok, err)
	}
	if err := o.Defer(ctx, e2.Seq, now.Add(time.Hour), auditappend.RetryUnavailable); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx, e.Seq); err != nil {
		t.Fatal(err)
	}
	e3, ok, _ := o.Claim(ctx, now, time.Minute)
	if !ok || e3.RecordID != recordID(3) {
		t.Fatalf("third claim: %+v, want record 3", e3)
	}
	if err := o.Delete(ctx, e3.Seq); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx, e3.Seq); !errors.Is(err, auditappend.ErrGone) {
		t.Fatalf("a second Delete of one row = %v, want ErrGone", err)
	}
	if _, ok, _ := o.Claim(ctx, now, time.Minute); ok {
		t.Fatal("a claim found a row while every row is held or deferred")
	}
	// The deferred row comes due, with its attempt counted.
	later, ok, _ := o.Claim(ctx, now.Add(2*time.Hour), time.Minute)
	if !ok || later.Seq != e2.Seq || later.Attempts != 1 {
		t.Fatalf("after its time: %+v, want record 2 with one attempt", later)
	}
	if n, err := o.Depth(ctx); err != nil || n != 1 {
		t.Fatalf("Depth = %d, %v, want 1", n, err)
	}
}

// TestPgAuditOutbox_ClaimsTheEarliestDue: a record deferred past a newer
// one does not hold the newer one back.
func TestPgAuditOutbox_ClaimsTheEarliestDue(t *testing.T) {
	ctx := context.Background()
	o := outbox(t, "provider-a")
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		if _, err := o.Enqueue(ctx, entry(i), now, auditappend.Bound); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _ := o.Claim(ctx, now, time.Minute)
	if err := o.Defer(ctx, first.Seq, now.Add(time.Hour), auditappend.RetryUnavailable); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	e, ok, _ := o.Claim(ctx, later, time.Minute)
	if !ok || e.RecordID != recordID(2) {
		t.Fatalf("claim: %+v, want record 2, due before the deferred record 1", e)
	}
}

// TestPgAuditOutbox_BoundDropsTheOldest: past the bound the oldest rows go,
// counted, and the newest stay.
func TestPgAuditOutbox_BoundDropsTheOldest(t *testing.T) {
	ctx := context.Background()
	o := outbox(t, "provider-a")
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if _, err := o.Enqueue(ctx, entry(i), now, 3); err != nil {
			t.Fatal(err)
		}
	}
	dropped, err := o.Enqueue(ctx, entry(4), now, 3)
	if err != nil || dropped != 1 {
		t.Fatalf("the fourth record dropped %d, %v, want 1", dropped, err)
	}
	e, _, _ := o.Claim(ctx, now, time.Minute)
	if e.RecordID != recordID(2) {
		t.Fatalf("oldest left is %s, want record 2: the oldest is what drops", e.RecordID)
	}
}

// TestPgAuditOutbox_SurvivesARestartAndHoldsPerHolder: a new outbox over the
// same database finds the queued rows; another holder's rows are not its own.
func TestPgAuditOutbox_SurvivesARestartAndHoldsPerHolder(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	a := NewAuditOutbox(pool, "provider-a")
	if _, err := a.Enqueue(ctx, entry(1), now, auditappend.Bound); err != nil {
		t.Fatal(err)
	}
	restarted := NewAuditOutbox(pool, "provider-a")
	if e, ok, _ := restarted.Claim(ctx, now, time.Minute); !ok || e.RecordID != recordID(1) {
		t.Fatalf("after a restart: %+v %v, want the queued record", e, ok)
	}
	other := NewAuditOutbox(pool, "payer-b")
	if n, _ := other.Depth(ctx); n != 0 {
		t.Fatalf("another holder's outbox holds %d rows", n)
	}
	if _, ok, _ := other.Claim(ctx, now.Add(time.Hour), time.Minute); ok {
		t.Fatal("another holder claimed this holder's row")
	}
}

// TestPgAuditOutbox_ReplicasNeverClaimOneRowTogether: two outboxes over one
// database (two replicas of one gateway) claim different rows.
func TestPgAuditOutbox_ReplicasNeverClaimOneRowTogether(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	a := NewAuditOutbox(pool, "provider-a")
	b := NewAuditOutbox(pool, "provider-a")
	for i := 1; i <= 2; i++ {
		if _, err := a.Enqueue(ctx, entry(i), now, auditappend.Bound); err != nil {
			t.Fatal(err)
		}
	}
	ea, okA, _ := a.Claim(ctx, now, time.Minute)
	eb, okB, _ := b.Claim(ctx, now, time.Minute)
	if !okA || !okB || ea.Seq == eb.Seq {
		t.Fatalf("replicas claimed %+v and %+v: want two different rows", ea, eb)
	}
	if _, ok, _ := b.Claim(ctx, now, time.Minute); ok {
		t.Fatal("a third claim found a row while both are held")
	}
}

// TestPgAuditOutbox_RefusesWhatItMustNotHold: a record over MaxBody is
// refused before any row is written, and one recordId is queued once.
func TestPgAuditOutbox_RefusesWhatItMustNotHold(t *testing.T) {
	ctx := context.Background()
	o := outbox(t, "provider-a")
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	big := auditappend.Entry{RecordID: recordID(1), Body: bytes.Repeat([]byte("a"), auditappend.MaxBody+1)}
	if _, err := o.Enqueue(ctx, big, now, auditappend.Bound); err == nil {
		t.Fatal("a record over MaxBody was queued")
	}
	for range 2 {
		if _, err := o.Enqueue(ctx, entry(2), now, auditappend.Bound); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := o.Depth(ctx); n != 1 {
		t.Fatalf("Depth = %d after the same recordId twice, want 1", n)
	}
}
