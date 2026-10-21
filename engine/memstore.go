// memstore.go — the in-memory Store: the gateway's OWN business state (auth numbers,
// and each compiled-in workstream's own state). Split out of the retired
// StubHolderData (§4.1): the SoR half of that type was a hardcoded demo persona
// census and is gone; THIS half carries no persona content at all and stays as the
// production in-memory Store default (a deployment swaps in pgstore via
// SHN_STORE_DATABASE_URL).
//
// What a workstream keeps in memory is its own (newMemState): the PA workstream's
// pended-claim ledger and continuation store live in workstream_pa_memstore.go.
package engine

import (
	"maps"
	"slices"
	"sync"
)

// Demo holds the demographics a holder knows about a member, used to derive the
// substrate PCI (AI-5: PCI is computed from member + demographics, never the bare
// member ID). It is the SystemOfRecord's return shape; every implementation reads
// it out of its own backing store.
type Demo struct {
	BirthDate, FamilyName string
}

// MemStore is the in-memory Store implementation: auth numbers, and each
// compiled-in workstream's own state. Metadata/decision only (AI-1-compatible) —
// it never holds clinical content. Construct with NewMemStore.
//
// A durable (Postgres) Store plugs in behind the same seam with no gateway change
// (gateway/connectors/pgstore); its parity suite runs both implementations through
// the same table.
type MemStore struct {
	mu          sync.Mutex
	authNumbers map[string]string
	// states is each compiled-in workstream's own state, by workstream id,
	// built once by NewMemStore. Each state keeps its own lock: the store's mutex
	// guards the auth numbers alone.
	states map[string]memState
	// claims serves the Store seam's claim-update methods: the state of the
	// workstream that keeps claims.
	claims memClaimUpdates
}

// memState is one workstream's own state in an in-memory Store
// (workstream.newMemState).
type memState interface {
	// reset clears the state back to empty (Reset). Safe for concurrent use.
	reset()
}

// memClaimUpdates are the Store seam's claim-update methods, which a
// workstream's state serves for the memory store.
type memClaimUpdates interface {
	RecordPendedClaim(subjectPCI, correlationID string) error
	BeginClaimUpdate(subjectPCI, correlationID string) (bool, error)
	ReleaseClaimUpdate(subjectPCI, correlationID string) error
	FinalizeClaimUpdate(subjectPCI, correlationID string) error
}

// memWorkstreamStates is what an in-memory Store, or a type embedding one,
// answers: one workstream's own state.
type memWorkstreamStates interface {
	workstreamState(id string) memState
}

// NewMemStore returns a ready-to-use in-memory Store with an initialized
// auth-number store and each compiled-in workstream's state.
func NewMemStore() *MemStore {
	d := &MemStore{authNumbers: make(map[string]string), states: workstreamMemStates()}
	for _, id := range slices.Sorted(maps.Keys(d.states)) {
		if c, ok := d.states[id].(interface{ claimUpdates() memClaimUpdates }); ok {
			if d.claims != nil {
				panic("engine: more than one workstream keeps claims in the in-memory Store")
			}
			d.claims = c.claimUpdates()
		}
	}
	if d.claims == nil {
		panic("engine: no compiled-in workstream keeps claims in the in-memory Store")
	}
	return d
}

// workstreamState is workstream id's own state, or nil when it keeps none.
func (d *MemStore) workstreamState(id string) memState { return d.states[id] }

// workstreamStateOf is workstream id's own state in s, when s is (or embeds) an
// in-memory Store, and nil otherwise.
func workstreamStateOf(s Store, id string) memState {
	m, ok := s.(memWorkstreamStates)
	if !ok {
		return nil
	}
	return m.workstreamState(id)
}

// RecordPendedClaim records a keyless pended claim. See Store.
func (d *MemStore) RecordPendedClaim(subjectPCI, correlationID string) error {
	return d.claims.RecordPendedClaim(subjectPCI, correlationID)
}

// BeginClaimUpdate claims a pended claim for an update. See Store.
func (d *MemStore) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	return d.claims.BeginClaimUpdate(subjectPCI, correlationID)
}

// ReleaseClaimUpdate returns an in-progress claim to pended. See Store.
func (d *MemStore) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	return d.claims.ReleaseClaimUpdate(subjectPCI, correlationID)
}

// FinalizeClaimUpdate completes an update on the Store-only path. See Store.
func (d *MemStore) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	return d.claims.FinalizeClaimUpdate(subjectPCI, correlationID)
}

// StoreAuthNumber records the payer-issued pre-auth number for a service request
// reference. Safe for concurrent use.
func (d *MemStore) StoreAuthNumber(serviceRequestRef, preAuthRef string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.authNumbers[serviceRequestRef] = preAuthRef
	return nil
}

// AuthNumber returns a previously stored pre-auth number, or found=false. Safe
// for concurrent use.
func (d *MemStore) AuthNumber(serviceRequestRef string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, ok := d.authNumbers[serviceRequestRef]
	return ref, ok
}

// Reset clears all MUTABLE holder state — the auth-number store and each
// workstream's own state — back to clean synthetic state (the demo reset
// contract). Safe for concurrent use. Each workstream's state is reset under its
// own lock, after the auth numbers.
func (d *MemStore) Reset() {
	d.mu.Lock()
	d.authNumbers = make(map[string]string)
	d.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(d.states)) {
		d.states[id].reset()
	}
}
