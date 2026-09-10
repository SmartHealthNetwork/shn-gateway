// exchangestore.go — the Layer-2 Exchange correlation seam (AI-1).
// The in-memory impl is the default when no store is configured; a durable/shared impl is a
// drop-in behind this interface. The store persists a METADATA-ONLY LegRecord — never
// Content, never bytes — so a durable store cannot silently become a longitudinal clinical store (AI-1).
package engine

import (
	"fmt"
	"sync"
	"time"
)

// ExchangeStore is the correlation seam (metadata only: never bytes). Begin opens
// an exchange, AppendLeg records one leg's metadata, Get returns a SNAPSHOT copy of
// the exchange (callers may mutate it freely) or false when the exchange is unknown
// or has passed its TTL. Every implementation evicts an exchange once
// created_at + TTL <= now. The seam is best-effort on the product path: a store
// failure is logged and counted by the gateway, never surfaced to the caller.
type ExchangeStore interface {
	Begin(workstream string) *Exchange
	AppendLeg(exchangeID string, rec LegRecord) error
	Get(exchangeID string) (*Exchange, bool)
}

// resettableStore is implemented by stores that can drop every exchange for the
// holder (admin reset). Gateway.Reset calls it and never reassigns the store.
type resettableStore interface {
	Reset() error
}

// defaultExchangeTTL applies when Config.ExchangeTTL is zero.
const defaultExchangeTTL = 168 * time.Hour

// purgeInterval throttles the lazy sweep of expired exchanges on Begin.
const purgeInterval = time.Minute

// LegRecord is the metadata-only projection of a completed leg that the store persists.
// It carries NO Content and NO bytes — by construction, never by discipline.
type LegRecord struct {
	Type          string
	CorrelationID string
	Subjects      []string
	Physics       LegPhysics
	Outcome       string // a non-clinical metadata label (e.g. approved | pa-required | pended | denied | ok | error | complete) — gates nothing; NEVER clinical content
}

type inMemoryExchangeStore struct {
	mu        sync.Mutex
	ttl       time.Duration
	now       func() time.Time
	exchanges map[string]*memExchange
	lastPurge time.Time
}

type memExchange struct {
	ex        Exchange
	createdAt time.Time
}

// NewInMemoryExchangeStore is the default ExchangeStore: process-local, bounded by ttl
// (zero selects defaultExchangeTTL), swept lazily on Begin. Correct only at ONE replica —
// a shared/durable backend is the drop-in behind ExchangeStore.
func NewInMemoryExchangeStore(ttl time.Duration, clock func() time.Time) *inMemoryExchangeStore {
	if ttl <= 0 {
		ttl = defaultExchangeTTL
	}
	if clock == nil {
		clock = time.Now
	}
	return &inMemoryExchangeStore{ttl: ttl, now: clock, exchanges: map[string]*memExchange{}}
}

func (s *inMemoryExchangeStore) expired(m *memExchange, now time.Time) bool {
	return !m.createdAt.Add(s.ttl).After(now) // created_at + TTL <= now
}

func (s *inMemoryExchangeStore) Begin(workstream string) *Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	ex := &memExchange{ex: Exchange{ID: newCorrelationID(), Workstream: workstream}, createdAt: now}
	s.exchanges[ex.ex.ID] = ex
	return &Exchange{ID: ex.ex.ID, Workstream: workstream}
}

func (s *inMemoryExchangeStore) purgeLocked(now time.Time) {
	if !s.lastPurge.IsZero() && now.Sub(s.lastPurge) < purgeInterval {
		return
	}
	for id, m := range s.exchanges {
		if s.expired(m, now) {
			delete(s.exchanges, id)
		}
	}
	s.lastPurge = now
}

func (s *inMemoryExchangeStore) AppendLeg(exchangeID string, rec LegRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.exchanges[exchangeID]
	if !ok || s.expired(m, s.now()) {
		return fmt.Errorf("ExchangeStore: append to unknown exchange %q", exchangeID)
	}
	m.ex.Legs = append(m.ex.Legs, cloneLeg(rec))
	return nil
}

func (s *inMemoryExchangeStore) Get(exchangeID string) (*Exchange, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.exchanges[exchangeID]
	if !ok || s.expired(m, s.now()) {
		return nil, false
	}
	return cloneExchange(&m.ex), true
}

func (s *inMemoryExchangeStore) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = map[string]*memExchange{}
	return nil
}

func cloneLeg(l LegRecord) LegRecord {
	c := l
	if l.Subjects != nil {
		c.Subjects = append([]string(nil), l.Subjects...)
	}
	return c
}

func cloneExchange(e *Exchange) *Exchange {
	c := &Exchange{ID: e.ID, Workstream: e.Workstream, Legs: make([]LegRecord, 0, len(e.Legs))}
	for _, l := range e.Legs {
		c.Legs = append(c.Legs, cloneLeg(l))
	}
	return c
}

// snapshot returns value copies of every UNEXPIRED exchange (test observability of the
// non-aggregation seam).
func (s *inMemoryExchangeStore) snapshot() []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Exchange, 0, len(s.exchanges))
	for _, m := range s.exchanges {
		if s.expired(m, now) {
			continue
		}
		out = append(out, *cloneExchange(&m.ex))
	}
	return out
}
