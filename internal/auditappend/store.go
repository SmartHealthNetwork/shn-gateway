package auditappend

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Bound is how many records the outbox holds for one holder. A record
// queued beyond it drops the oldest, counted: while the Audit Plane is
// unreachable the outbox never grows without limit. It is behaviour, not a
// setting.
const Bound = 10000

// MaxBody bounds one record's signed append request, and so the unsigned
// record the outbox keeps. The content guard bounds every member, so a real
// record is far smaller; the outbox's own check (and gw_audit_outbox's CHECK)
// is the backstop.
const MaxBody = 64 << 10

// ErrGone is a Delete of a record the outbox no longer holds: overflow took
// it while it was being sent. The appender counts the record once, under the
// overflow that took it, not again as sent or dropped.
var ErrGone = errors.New("auditappend: the record is no longer queued")

// Entry is one queued record: the record's JSON as the content guard passed
// it, unsigned. Each send signs it with the key the gateway runs with then.
type Entry struct {
	Seq      int64
	RecordID string
	Body     []byte
	Attempts int
}

// Store is the outbox. Its implementations are the in-memory one here and
// the gateway's Postgres store (gw_audit_outbox), which survives a restart.
type Store interface {
	// Enqueue adds a record and drops the oldest beyond bound, in one step,
	// returning how many it dropped.
	Enqueue(ctx context.Context, e Entry, now time.Time, bound int) (dropped int, err error)
	// Claim returns the record due earliest at now (the oldest among equals)
	// and holds it until now+lease, so no other sender takes it meanwhile; ok
	// is false when none is due.
	Claim(ctx context.Context, now time.Time, lease time.Duration) (e Entry, ok bool, err error)
	// Delete removes a record: the Plane accepted it, or it was dropped. It
	// returns ErrGone when the record is no longer queued.
	Delete(ctx context.Context, seq int64) error
	// Defer makes a record due again at next, counting the attempt.
	Defer(ctx context.Context, seq int64, next time.Time, status string) error
	// Depth is how many records are queued.
	Depth(ctx context.Context) (int, error)
	// Durable reports whether queued records survive a restart.
	Durable() bool
}

// MemStore is the outbox of a gateway with no Postgres store: bounded, oldest
// dropped first, and lost at restart (the appender counts what a stop leaves).
type MemStore struct {
	mu      sync.Mutex
	next    int64
	entries []memEntry
}

type memEntry struct {
	Entry
	due time.Time
}

// NewMemStore returns an empty in-memory outbox.
func NewMemStore() *MemStore { return &MemStore{} }

func (s *MemStore) Enqueue(_ context.Context, e Entry, now time.Time, bound int) (int, error) {
	if len(e.Body) > MaxBody {
		return 0, errors.New("auditappend: record over its bound")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	e.Seq = s.next
	s.entries = append(s.entries, memEntry{Entry: e, due: now})
	dropped := 0
	if over := len(s.entries) - bound; over > 0 {
		dropped = over
		s.entries = append(s.entries[:0:0], s.entries[over:]...)
	}
	return dropped, nil
}

func (s *MemStore) Claim(_ context.Context, now time.Time, lease time.Duration) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	for i := range s.entries {
		if !s.entries[i].due.After(now) && (best < 0 || s.entries[i].due.Before(s.entries[best].due)) {
			best = i
		}
	}
	if best < 0 {
		return Entry{}, false, nil
	}
	s.entries[best].due = now.Add(lease)
	return s.entries[best].Entry, true, nil
}

func (s *MemStore) Delete(_ context.Context, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].Seq == seq {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return nil
		}
	}
	return ErrGone
}

func (s *MemStore) Defer(_ context.Context, seq int64, next time.Time, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].Seq == seq {
			s.entries[i].due = next
			s.entries[i].Attempts++
			return nil
		}
	}
	return nil
}

func (s *MemStore) Depth(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), nil
}

func (s *MemStore) Durable() bool { return false }
