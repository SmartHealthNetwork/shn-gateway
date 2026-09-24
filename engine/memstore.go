// memstore.go — the in-memory Store: the gateway's OWN business state (auth numbers,
// the pended-claim ledger, issued EOBs). Split out of the retired StubHolderData
// (§4.1): the SoR half of that type was a hardcoded demo persona census and is
// gone; THIS half carries no persona content at all and stays as the production
// in-memory Store default (a deployment swaps in pgstore via SHN_STORE_DATABASE_URL).
//
// It also implements the optional PendLedger (pendledger.go): the keyed lookup, the
// absorbing decided state, atomic decision-plus-EOB writes and six-month retention,
// with the SAME shared state-machine helpers the durable backend runs.
package engine

import (
	"sync"
	"time"
)

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
	// subjectPCI + "|" + correlationID. An absent key is "no such claim" — never
	// pended, or purged after its retention. Metadata only (FR-21/FR-6;
	// AI-1-compatible).
	pendedClaims map[string]*pendRow
	// pendedKeys is the lookup index: one (requester holder, kind, key) triple maps
	// to the pendedClaims keys it identifies. More than one is an ambiguous probe.
	pendedKeys map[pendIndexKey]map[string]bool
	// eobIDsByPCI is the payer-side PA-decision EOB store keyed by subject PCI
	// (UC-08 Patient Access API, FR-28), holding EOB ids in record order; eobByID
	// holds the bytes. Storing the id list rather than the bytes is what makes a
	// re-recorded EOB id ONE EOB here exactly as it is one row in the durable
	// store. Metadata/decision only — AI-1-compatible.
	eobIDsByPCI  map[string][]string
	eobByID      map[string][]byte
	eobOwnerByID map[string]string
	// now is the store's own clock (retention and arrival order). Injected so the
	// retention rows are deterministic; never reassigned outside tests.
	now func() time.Time
	// lastPurge throttles the lazy retention sweep (see maybePurgeLocked).
	lastPurge time.Time
	// continuations is the in-memory ContinuationStore (continuation.go). It is
	// held rather than embedded so its own mutex stays its own: a continuation
	// read must not queue behind a pend-ledger write, and the two hold nothing in
	// common but this store's lifetime.
	continuations *MemContinuations
}

// pendRow is one ledger row plus the index keys filed for it — the whole key,
// requester namespace included, as it was at index time (see indexLocked).
type pendRow struct {
	subjectPCI    string
	correlationID string
	rec           PendRecord
	keys          map[pendIndexKey]bool
}

// pendIndexKey is one lookup key in one requester's namespace.
type pendIndexKey struct {
	requesterHolder string
	kind            string
	key             string
}

// NewMemStore returns a ready-to-use in-memory Store with an initialized
// auth-number store, pended-claim ledger and EOB store.
func NewMemStore() *MemStore {
	d := &MemStore{now: time.Now}
	d.reset()
	return d
}

// pendedKey is the ledger key for a (subjectPCI, correlationID) pair.
func pendedKey(subjectPCI, correlationID string) string {
	return subjectPCI + "|" + correlationID
}

// RecordPendedClaim records a pended claim with no lookup keys — the Store seam's
// keyless pend. A claim the ledger already has as DECIDED is left alone: decided is
// absorbing, and only a dated, keyed re-pend (RecordPendedKeyed) can supersede a
// decision. Safe for concurrent use.
//
// On a claim the ledger already knows it sets the STATE AND NOTHING ELSE. It must
// not replace the record: dropping the requester the claim was submitted under
// would let a different requester re-pend it and take it over, and would strand the
// row's lookup keys under a requester that no longer matches — both of which the
// durable backend's conditional UPDATE never does. The two backends have to answer
// the same way, or the in-memory one is the permissive one and a suite passes on
// behaviour a deployment will not reproduce.
func (d *MemStore) RecordPendedClaim(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maybePurgeLocked()
	row, found := d.pendedClaims[pendedKey(subjectPCI, correlationID)]
	if found {
		if row.rec.State == PendStateDecided {
			return nil // already decided
		}
		row.rec.State = PendStatePended
		row.rec.LastTransition = d.now()
		return nil
	}
	d.upsertLocked(subjectPCI, correlationID, PendRecord{State: PendStatePended})
	return nil
}

// BeginClaimUpdate ATOMICALLY claims a pended claim for a ClaimUpdate: if it is
// currently pended it transitions it to in-progress and returns true; otherwise
// (never pended, already decided, or another update already in progress) it
// returns false. This single test-and-set is the FR-6 current-state authority check
// AND the mutual-exclusion that serializes concurrent updates for the same claim —
// only one update can be in flight. The caller must pair it with a decision
// (RecordDecision, or FinalizeClaimUpdate on a store-only path) or
// ReleaseClaimUpdate. Safe for concurrent use.
func (d *MemStore) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	ok, _, err := d.BeginClaimUpdateReason(subjectPCI, correlationID)
	return ok, err
}

// BeginClaimUpdateReason is BeginClaimUpdate with the refusal reason. See
// PendLedger.
func (d *MemStore) BeginClaimUpdateReason(subjectPCI, correlationID string) (bool, PendRefusal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, found := d.pendedClaims[pendedKey(subjectPCI, correlationID)]
	cur := PendState("")
	if found {
		cur = row.rec.State
	}
	next, ok, why := PendBegin(cur, found)
	if ok {
		row.rec.State = next
		row.rec.LastTransition = d.now()
	}
	return ok, why, nil
}

// ReleaseClaimUpdate returns an in-progress claim to pended (a ClaimUpdate did NOT
// decide it — e.g. still insufficient or a validation error — so a later, complete
// amendment can still transition it). On a DECIDED claim it is a no-op reporting
// "already decided": a rollback after the decision never reverts it. Safe for
// concurrent use.
func (d *MemStore) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, found := d.pendedClaims[pendedKey(subjectPCI, correlationID)]
	if !found || row.rec.State != PendStateInProgress {
		return nil
	}
	row.rec.State = PendStatePended
	row.rec.LastTransition = d.now()
	return nil
}

// FinalizeClaimUpdate completes the pended→approved transition on the Store-only
// path: it removes the claim so a replayed update for it finds nothing (replay
// protection). On a DECIDED claim it is a no-op reporting "already decided" — the
// ledger keeps that row for its retention period so a follow-up still resolves.
// Safe for concurrent use.
func (d *MemStore) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if row, found := d.pendedClaims[k]; found && row.rec.State == PendStateDecided {
		return nil // already decided
	}
	d.deleteLocked(k)
	return nil
}

// --- PendLedger ---

var _ PendLedger = (*MemStore)(nil)

// The in-memory Store answers the pre-forward correlation checks (eobowner.go).
var (
	_ EOBOwnerLookup        = (*MemStore)(nil)
	_ PendCorrelationLookup = (*MemStore)(nil)
)

// RecordPendedKeyed records (or re-pends) an authorization and indexes its lookup
// keys. See PendLedger.
func (d *MemStore) RecordPendedKeyed(subjectPCI, corrID string, created time.Time, k PendKeys) (PendTransition, error) {
	refs, err := ValidatePendKeys(k)
	if err != nil {
		return PendTransition{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maybePurgeLocked()
	row, found := d.pendedClaims[pendedKey(subjectPCI, corrID)]
	var cur PendRecord
	if found {
		cur = row.rec
		if cur.RequesterHolder != "" && cur.RequesterHolder != k.RequesterHolder {
			return PendTransition{}, ErrPendRequesterMismatch
		}
	}
	next, tr := PendRePend(cur, found, created)
	next.RequesterHolder = k.RequesterHolder
	next.LastTransition = d.now()
	d.upsertLocked(subjectPCI, corrID, next)
	d.indexLocked(subjectPCI, corrID, k.RequesterHolder, refs)
	// The UNION the authorization now has, not this response's contribution: a
	// re-pend whose response carries no identifier leaves an authorization that is
	// still perfectly findable by the keys an earlier response gave, and reporting
	// zero for it would raise "nothing to match on" about a claim that has plenty.
	// Read under the same lock as the write, so the number is the one that landed.
	tr.Keys = len(d.pendedClaims[pendedKey(subjectPCI, corrID)].keys)
	return tr, nil
}

// LookupPended resolves a follow-up's keys within one requester's namespace. See
// PendLedger.
func (d *MemStore) LookupPended(requesterHolder string, k PendKeys) (string, string, bool, bool, error) {
	if requesterHolder == "" {
		return "", "", false, false, ErrPendRequesterRequired
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	hits := map[string]bool{}
	for _, ref := range ProbePendKeys(k) {
		for key := range d.pendedKeys[pendIndexKey{requesterHolder: requesterHolder, kind: ref.Kind, key: ref.Key}] {
			hits[key] = true
		}
	}
	if len(hits) == 0 {
		return "", "", false, false, nil
	}
	if len(hits) > 1 {
		// Two authorizations share a key: the ledger will not guess which one the
		// requester meant, and it changes nothing.
		return "", "", false, true, nil
	}
	for key := range hits {
		row := d.pendedClaims[key]
		if row == nil { // an index entry outliving its row cannot happen; stay total
			return "", "", false, false, nil
		}
		return row.subjectPCI, row.correlationID, true, false, nil
	}
	return "", "", false, false, nil
}

// RecordDecision records the payer's terminal answer and its EOB as one write. See
// PendLedger.
//
// The decision and the EOB land together or not at all. In memory that means both
// guards run BEFORE any state changes and both writes then happen under one hold of
// the mutex; the durable backend runs the same two writes in one transaction.
func (d *MemStore) RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, eob *EOBRecord) (PendTransition, error) {
	if err := ValidatePendDecision(outcome); err != nil {
		return PendTransition{}, err
	}
	if err := eob.Validate(subjectPCI); err != nil {
		return PendTransition{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if eob != nil {
		if err := d.recordEOBLocked(eob.SubjectPCI, eob.EOBID, eob.JSON); err != nil {
			return PendTransition{}, err
		}
	}
	row, found := d.pendedClaims[pendedKey(subjectPCI, corrID)]
	var cur PendRecord
	if found {
		cur = row.rec
	}
	next, tr := PendDecide(cur, found, outcome, decidedAt)
	next.RequesterHolder = cur.RequesterHolder
	next.LastTransition = d.now()
	d.upsertLocked(subjectPCI, corrID, next)
	return tr, nil
}

// PendRecordOf reads one ledger row. See PendLedger.
func (d *MemStore) PendRecordOf(subjectPCI, corrID string) (PendRecord, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row, found := d.pendedClaims[pendedKey(subjectPCI, corrID)]
	if !found {
		return PendRecord{}, false, nil
	}
	return row.rec, true, nil
}

// --- ContinuationStore (continuation.go) ---
//
// The in-memory Store ships the in-memory continuation store, so the default
// deployment has one without wiring. It is the NON-DURABLE half of the disclosed
// posture: what it minted before a restart is gone, and it answers a later
// continuation with ContinuationLost rather than "unknown".

var _ ContinuationStore = (*MemStore)(nil)

// PutContinuation writes a continuation. See ContinuationStore.
func (d *MemStore) PutContinuation(c Continuation) (Continuation, error) {
	return d.continuations.PutContinuation(c)
}

// ReadContinuation resolves {holder, id}. See ContinuationStore.
func (d *MemStore) ReadContinuation(holder, id string) (Continuation, ContinuationLookup, error) {
	return d.continuations.ReadContinuation(holder, id)
}

// Durable reports that this store loses its continuations on a restart.
func (d *MemStore) Durable() bool { return false }

// --- ledger internals (all callers hold d.mu) ---

// upsertLocked writes rec for (subjectPCI, correlationID), creating the row if it
// is new and preserving the keys already indexed for it.
func (d *MemStore) upsertLocked(subjectPCI, correlationID string, rec PendRecord) {
	k := pendedKey(subjectPCI, correlationID)
	row, found := d.pendedClaims[k]
	if !found {
		row = &pendRow{subjectPCI: subjectPCI, correlationID: correlationID, keys: map[pendIndexKey]bool{}}
		d.pendedClaims[k] = row
	}
	if rec.LastTransition.IsZero() {
		rec.LastTransition = d.now()
	}
	row.rec = rec
}

// indexLocked adds refs to the lookup index for this row, unioning them with the
// keys a previous response already contributed.
func (d *MemStore) indexLocked(subjectPCI, correlationID, requesterHolder string, refs []PendKeyRef) {
	k := pendedKey(subjectPCI, correlationID)
	row := d.pendedClaims[k]
	if row == nil {
		return
	}
	for _, ref := range refs {
		// The row remembers the WHOLE index key, requester included, so the entry
		// can be removed by the namespace it was filed under. Re-deriving the
		// namespace from the record at delete time would strand the entry whenever
		// the record's requester had since changed or been cleared — a stale entry
		// that answers a later lookup with ambiguous=true for a claim that is gone.
		idx := pendIndexKey{requesterHolder: requesterHolder, kind: ref.Kind, key: ref.Key}
		row.keys[idx] = true
		if d.pendedKeys[idx] == nil {
			d.pendedKeys[idx] = map[string]bool{}
		}
		d.pendedKeys[idx][k] = true
	}
}

// deleteLocked removes a row and every index entry that pointed at it — the
// in-memory equivalent of the durable index's ON DELETE CASCADE.
func (d *MemStore) deleteLocked(k string) {
	row, found := d.pendedClaims[k]
	if !found {
		return
	}
	for idx := range row.keys {
		delete(d.pendedKeys[idx], k)
		if len(d.pendedKeys[idx]) == 0 {
			delete(d.pendedKeys, idx)
		}
	}
	delete(d.pendedClaims, k)
}

// maybePurgeLocked runs the lazy retention sweep: at most once per
// PendPurgeInterval, and at most PendPurgeMax rows per call, so a backlog is worked
// off over many calls. It mirrors the durable store's bounded purge exactly, which
// is what keeps the two backends' observable retention the same.
func (d *MemStore) maybePurgeLocked() {
	now := d.now()
	due, cutoff := DuePendPurge(now, d.lastPurge)
	if !due {
		return
	}
	d.lastPurge = now
	purged := 0
	for k, row := range d.pendedClaims {
		if purged >= PendPurgeMax {
			return
		}
		if row.rec.LastTransition.After(cutoff) {
			continue
		}
		d.deleteLocked(k)
		purged++
	}
}

// recordEOBLocked stores a COPY of the bytes under eobID, appending the id to the
// patient's list only the first time that id is seen — a re-recorded id is ONE EOB,
// exactly as it is one row in the durable store.
func (d *MemStore) recordEOBLocked(subjectPCI, eobID string, eobJSON []byte) error {
	if owner, seen := d.eobOwnerByID[eobID]; seen && owner != subjectPCI {
		return ErrEOBSubjectMismatch
	}
	cp := make([]byte, len(eobJSON))
	copy(cp, eobJSON)
	if _, seen := d.eobOwnerByID[eobID]; !seen {
		d.eobIDsByPCI[subjectPCI] = append(d.eobIDsByPCI[subjectPCI], eobID)
	}
	d.eobOwnerByID[eobID] = subjectPCI
	d.eobByID[eobID] = cp
	return nil
}

// RecordEOB stores a PA-decision EOB for a patient (keyed by subject PCI) and
// indexes it by EOB id for read-by-id. Stores a COPY of the bytes. Re-recording an
// id replaces its bytes and does not add a second EOB. Safe for concurrent use.
func (d *MemStore) RecordEOB(subjectPCI, eobID string, eobJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recordEOBLocked(subjectPCI, eobID, eobJSON)
}

// EOBOwner reports the patient an EOB id is filed for. See EOBOwnerLookup.
func (d *MemStore) EOBOwner(eobID string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	owner, found := d.eobOwnerByID[eobID]
	return owner, found, nil
}

// PendedForOtherSubject reports a patient other than subjectPCI with an undecided
// authorization under corrID. See PendCorrelationLookup. When more than one
// qualifies the least PCI is reported, so the answer does not depend on map order.
func (d *MemStore) PendedForOtherSubject(corrID, subjectPCI string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	other, found := "", false
	for _, row := range d.pendedClaims {
		if row.correlationID != corrID || row.subjectPCI == subjectPCI || row.rec.State == PendStateDecided {
			continue
		}
		if !found || row.subjectPCI < other {
			other, found = row.subjectPCI, true
		}
	}
	return other, found, nil
}

// EOBsForPatient returns all stored EOBs for a patient PCI (search), or ok=false
// when none are stored. Returns defensive copies (a fresh slice of fresh byte
// slices) so a caller cannot mutate stored state. Safe for concurrent use.
func (d *MemStore) EOBsForPatient(subjectPCI string) ([][]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.eobIDsByPCI[subjectPCI]
	if len(ids) == 0 {
		return nil, false
	}
	out := make([][]byte, 0, len(ids))
	for _, id := range ids {
		b := d.eobByID[id]
		cp := make([]byte, len(b))
		copy(cp, b)
		out = append(out, cp)
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
// ledger with its lookup index, and the EOB store — back to clean synthetic state
// (the demo reset contract). Safe for concurrent use.
func (d *MemStore) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reset()
}

// reset initializes every map. Called by the constructor (no lock needed yet) and
// by Reset (which holds the lock).
func (d *MemStore) reset() {
	d.authNumbers = make(map[string]string)
	d.pendedClaims = make(map[string]*pendRow)
	d.pendedKeys = make(map[pendIndexKey]map[string]bool)
	d.eobIDsByPCI = make(map[string][]string)
	d.eobByID = make(map[string][]byte)
	d.eobOwnerByID = make(map[string]string)
	d.lastPurge = time.Time{}
	// The continuation store keeps its OWN mutex, so it is reset through its own
	// entry point rather than reconstructed here — reconstructing it would drop
	// an injected clock a retention row had set.
	if d.continuations == nil {
		d.continuations = NewMemContinuations()
		return
	}
	d.continuations.ResetContinuations()
}
