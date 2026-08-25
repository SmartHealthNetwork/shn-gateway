// memstore.go — the in-memory Store: the gateway's OWN business state (auth numbers,
// the pended-claim ledger, issued EOBs). Split out of the retired StubHolderData
// (§4.1): the SoR half of that type was a hardcoded demo persona census and is
// gone; THIS half carries no persona content at all and stays as the production
// in-memory Store default (a deployment swaps in pgstore via SHN_STORE_DATABASE_URL).
package engine

import "sync"

// Demo holds the demographics a holder knows about a member, used to derive the
// substrate PCI (AI-5: PCI is computed from member + demographics, never the bare
// member ID). It is the SystemOfRecord's return shape; every implementation reads
// it out of its own backing store.
type Demo struct {
	BirthDate, FamilyName string
}

// MemStore is the in-memory Store implementation: auth numbers, the payer-side
// pended-claim ledger, and the PA-decision EOB store. Metadata/decision only
// (AI-1-compatible) — it never holds clinical content. Construct with NewMemStore.
//
// A durable (Postgres) Store plugs in behind the same seam with no gateway change
// (gateway/connectors/pgstore); its parity suite runs both implementations through
// the same table.
type MemStore struct {
	mu          sync.Mutex
	authNumbers map[string]string
	// pendedClaims is the payer-side pended-claim ledger keyed by
	// subjectPCI + "|" + correlationID. The value is the status: claimPended (awaiting
	// supplemental data) or claimInProgress (a ClaimUpdate is being adjudicated for
	// it). An absent key is "no such claim" (never pended, or already approved).
	// Metadata only (FR-21/FR-6; AI-1-compatible).
	pendedClaims map[string]claimStatus
	// eobsByPCI is the payer-side PA-decision EOB store keyed by subject PCI
	// (UC-08 Patient Access API, FR-28). Metadata/decision only — AI-1-compatible.
	eobsByPCI map[string][][]byte
	eobByID   map[string][]byte
}

type claimStatus int

const (
	claimPended     claimStatus = iota + 1 // awaiting supplemental data
	claimInProgress                        // a ClaimUpdate is mid-adjudication
)

// NewMemStore returns a ready-to-use in-memory Store with an initialized
// auth-number store, pended-claim ledger and EOB store.
func NewMemStore() *MemStore {
	return &MemStore{
		authNumbers:  make(map[string]string),
		pendedClaims: make(map[string]claimStatus),
		eobsByPCI:    make(map[string][][]byte),
		eobByID:      make(map[string][]byte),
	}
}

// pendedKey is the ledger key for a (subjectPCI, correlationID) pair.
func pendedKey(subjectPCI, correlationID string) string {
	return subjectPCI + "|" + correlationID
}

// RecordPendedClaim records a pended claim. Safe for concurrent use.
func (d *MemStore) RecordPendedClaim(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendedClaims[pendedKey(subjectPCI, correlationID)] = claimPended
	return nil
}

// BeginClaimUpdate ATOMICALLY claims a pended claim for a ClaimUpdate: if it is
// currently pended it transitions it to in-progress and returns true; otherwise
// (never pended, already approved, or another update already in progress) it
// returns false. This single test-and-set is the FR-6 current-state authority check
// AND the mutual-exclusion that serializes concurrent updates for the same claim —
// only one update can be in flight. The caller must pair it with FinalizeClaimUpdate
// (on approval) or ReleaseClaimUpdate (on any non-approval). Safe for concurrent use.
func (d *MemStore) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if d.pendedClaims[k] != claimPended {
		return false, nil
	}
	d.pendedClaims[k] = claimInProgress
	return true, nil
}

// ReleaseClaimUpdate returns an in-progress claim to pended (a ClaimUpdate did NOT
// approve — e.g. still insufficient or a validation error — so a later, complete
// amendment can still transition it). Safe for concurrent use.
func (d *MemStore) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if d.pendedClaims[k] == claimInProgress {
		d.pendedClaims[k] = claimPended
	}
	return nil
}

// FinalizeClaimUpdate completes the pended→approved transition: it removes the
// claim so a replayed update for it finds nothing (replay protection). Safe for
// concurrent use.
func (d *MemStore) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pendedClaims, pendedKey(subjectPCI, correlationID))
	return nil
}

// RecordEOB stores a PA-decision EOB for a patient (keyed by subject PCI) and
// indexes it by EOB id for read-by-id. Stores a COPY of the bytes. Safe for
// concurrent use.
func (d *MemStore) RecordEOB(subjectPCI, eobID string, eobJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]byte, len(eobJSON))
	copy(cp, eobJSON)
	d.eobsByPCI[subjectPCI] = append(d.eobsByPCI[subjectPCI], cp)
	d.eobByID[eobID] = cp
	return nil
}

// EOBsForPatient returns all stored EOBs for a patient PCI (search), or ok=false
// when none are stored. Returns defensive copies (a fresh slice of fresh byte
// slices) so a caller cannot mutate stored state. Safe for concurrent use.
func (d *MemStore) EOBsForPatient(subjectPCI string) ([][]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.eobsByPCI[subjectPCI]
	if !ok || len(v) == 0 {
		return nil, false
	}
	out := make([][]byte, len(v))
	for i, b := range v {
		cp := make([]byte, len(b))
		copy(cp, b)
		out[i] = cp
	}
	return out, true
}

// EOBByID returns one stored EOB by its id (read), or ok=false. Returns a
// defensive copy so a caller cannot mutate stored bytes. Safe for concurrent use.
func (d *MemStore) EOBByID(eobID string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.eobByID[eobID]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, true
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

// Reset clears all MUTABLE holder state — the auth-number store, the pended-claim
// ledger and the EOB store — back to clean synthetic state (the demo reset
// contract). Safe for concurrent use.
func (d *MemStore) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.authNumbers = make(map[string]string)
	d.pendedClaims = make(map[string]claimStatus)
	d.eobsByPCI = make(map[string][][]byte)
	d.eobByID = make(map[string][]byte)
}
