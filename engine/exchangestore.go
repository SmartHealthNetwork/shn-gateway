// exchangestore.go — the Layer-2 Exchange correlation seam (AI-1).
// The in-memory impl is the current default; a durable/expiring/shared impl is a planned
// future drop-in behind this interface. The store persists a METADATA-ONLY LegRecord — never
// Content, never bytes — so a durable store cannot silently become a longitudinal clinical store (AI-1).
package engine

import (
	"fmt"
	"sync"
)

// ExchangeStore groups the legs of one correlated business interaction. Begin mints the
// parent Exchange.ID; AppendLeg records a completed leg's metadata projection; Get reads back.
//
// Aliasing contract: Begin/Get return the store's live *Exchange. A caller MUST NOT read the
// returned pointer's mutable state (notably .Legs) concurrently with a store mutation on the
// same exchange — it is safe only because the current in-memory impl runs one synchronous
// ingress call per exchange (callers use Begin's result for its .ID, never to read .Legs).
// A durable/shared impl that admits concurrent readers must return a snapshot copy instead.
type ExchangeStore interface {
	Begin(workstream string) *Exchange
	AppendLeg(exchangeID string, rec LegRecord) error
	Get(exchangeID string) (*Exchange, bool)
}

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
	exchanges map[string]*Exchange
}

// NewInMemoryExchangeStore is the default ExchangeStore. Reset-cleared, no TTL — a
// durable/expiring/shared backend is a planned future drop-in behind ExchangeStore.
func NewInMemoryExchangeStore() *inMemoryExchangeStore {
	return &inMemoryExchangeStore{exchanges: map[string]*Exchange{}}
}

func (s *inMemoryExchangeStore) Begin(workstream string) *Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := &Exchange{ID: newCorrelationID(), Workstream: workstream}
	s.exchanges[ex.ID] = ex
	return ex
}

func (s *inMemoryExchangeStore) AppendLeg(exchangeID string, rec LegRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.exchanges[exchangeID]
	if !ok {
		return fmt.Errorf("ExchangeStore: append to unknown exchange %q", exchangeID)
	}
	ex.Legs = append(ex.Legs, rec)
	return nil
}

func (s *inMemoryExchangeStore) Get(exchangeID string) (*Exchange, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.exchanges[exchangeID]
	return ex, ok
}

// snapshot returns value copies of all exchanges (test observability of the non-aggregation seam).
func (s *inMemoryExchangeStore) snapshot() []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Exchange, 0, len(s.exchanges))
	for _, ex := range s.exchanges {
		cp := *ex
		cp.Legs = append([]LegRecord(nil), ex.Legs...)
		out = append(out, cp)
	}
	return out
}
