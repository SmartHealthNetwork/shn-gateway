// workstream_pa_memstore.go — the PA workstream's state in the in-memory Store
// (memstore.go): the payer-side pended-claim ledger and the in-memory
// continuation store. The memory store holds it as the PA workstream's
// memState and reaches it through no PA name; LedgerOf and ContinuationsOf
// find it (workstreamStateOf).
//
// The ledger implements the optional PendLedger (package pend): the keyed lookup,
// the absorbing decided state, atomic decision-plus-keys writes and six-month
// retention, with the SAME shared state-machine helpers the durable backend runs.
// It also serves the Store seam's claim-update methods, which the memory store
// forwards to it (memClaimUpdates).
package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pend"
)

// newMemState: the PA workstream's state in an in-memory Store.
func (paWorkstream) newMemState() memState {
	return &memPAState{ledger: newMemPendLedger(), continuations: NewMemContinuations()}
}

// memPAState is the PA workstream's state in one in-memory Store. The ledger
// and the continuation store each keep their own mutex: a continuation read must
// not queue behind a pend-ledger write, and the two hold nothing in common but
// the store's lifetime.
type memPAState struct {
	ledger        *memPendLedger
	continuations *MemContinuations
}

// reset clears the ledger, then the continuation store. The continuation store
// is reset through its own entry point rather than reconstructed, which would
// drop an injected clock a retention row had set.
func (s *memPAState) reset() {
	s.ledger.reset()
	s.continuations.ResetContinuations()
}

// claimUpdates: the ledger serves the Store seam's claim-update methods.
func (s *memPAState) claimUpdates() memClaimUpdates { return s.ledger }

// memPAStateOf is the PA workstream's state in s, when s is (or embeds) an
// in-memory Store.
func memPAStateOf(s Store) (*memPAState, bool) {
	st, ok := workstreamStateOf(s, workstreamPA).(*memPAState)
	return st, ok
}

// checkStore refuses a Store that embeds an in-memory Store and declares part
// of PendLedger or ContinuationStore itself without being the whole
// interface: its own methods would be skipped for the embedded state's. New
// refuses it at construction, so the request path never meets one.
func (paWorkstream) checkStore(s Store) error {
	if _, ok := memPAStateOf(s); !ok {
		return nil
	}
	if m := partialPendLedger(s); m != "" {
		return fmt.Errorf("the configured Store %T declares %s but not the whole PendLedger, and embeds an in-memory Store whose own ledger would answer in its place: implement every PendLedger method", s, m)
	}
	if m := partialContinuationStore(s); m != "" {
		return fmt.Errorf("the configured Store %T declares %s but not the whole ContinuationStore, and embeds an in-memory Store whose own continuation store would answer in its place: implement every ContinuationStore method", s, m)
	}
	return nil
}

// The methods of PendLedger and of ContinuationStore, one interface each, so
// the guards below assert each by its exact signature. Each set is its whole
// interface, both ways, so a method either interface gains or changes fails
// the build here rather than going unchecked.
type (
	pendRecordPendedKeyed interface {
		RecordPendedKeyed(string, string, time.Time, PendKeys) (PendTransition, error)
	}
	pendLookupPended interface {
		LookupPended(string, PendKeys) (PendMatch, error)
	}
	pendRecordDecision interface {
		RecordDecision(string, string, string, time.Time, PendKeys) (PendTransition, error)
	}
	pendBeginClaimUpdateReason interface {
		BeginClaimUpdateReason(string, string) (bool, PendRefusal, error)
	}
	pendRecordOf interface {
		PendRecordOf(string, string) (PendRecord, bool, error)
	}
	continuationPut interface {
		PutContinuation(Continuation) (Continuation, error)
	}
	continuationRead interface {
		ReadContinuation(string, string) (Continuation, ContinuationLookup, error)
	}
	continuationDurable interface{ Durable() bool }
)

var (
	_ PendLedger = interface {
		pendRecordPendedKeyed
		pendLookupPended
		pendRecordDecision
		pendBeginClaimUpdateReason
		pendRecordOf
	}(nil)
	_ interface {
		pendRecordPendedKeyed
		pendLookupPended
		pendRecordDecision
		pendBeginClaimUpdateReason
		pendRecordOf
	} = PendLedger(nil)
	_ ContinuationStore = interface {
		continuationPut
		continuationRead
	}(nil)
	_ interface {
		continuationPut
		continuationRead
	} = ContinuationStore(nil)
)

// partialPendLedger is the first PendLedger method s declares when it is not
// the whole PendLedger, or "". The in-memory Store declares none, so a match is
// the store's own. Each is a
// typed assertion rather than a lookup by name, which would keep every
// exported method of every type in the binary.
func partialPendLedger(s Store) string {
	if _, whole := s.(PendLedger); whole {
		return ""
	}
	switch {
	case implements[pendRecordPendedKeyed](s):
		return "RecordPendedKeyed"
	case implements[pendLookupPended](s):
		return "LookupPended"
	case implements[pendRecordDecision](s):
		return "RecordDecision"
	case implements[pendBeginClaimUpdateReason](s):
		return "BeginClaimUpdateReason"
	case implements[pendRecordOf](s):
		return "PendRecordOf"
	}
	return ""
}

// partialContinuationStore is the first ContinuationStore method s declares,
// with Durable (which DurableContinuations reads), when it is not the whole
// ContinuationStore, or "".
func partialContinuationStore(s Store) string {
	if _, whole := s.(ContinuationStore); whole {
		return ""
	}
	switch {
	case implements[continuationPut](s):
		return "PutContinuation"
	case implements[continuationRead](s):
		return "ReadContinuation"
	case implements[continuationDurable](s):
		return "Durable"
	}
	return ""
}

// implements reports whether s has I's methods.
func implements[I any](s Store) bool {
	_, ok := s.(I)
	return ok
}

// memPendLedger is the in-memory pended-claim ledger. Metadata/decision only
// (FR-21/FR-6; AI-1-compatible): it never holds clinical content.
type memPendLedger struct {
	mu sync.Mutex
	// pendedClaims is the payer-side pended-claim ledger keyed by
	// subjectPCI + "|" + correlationID. An absent key is "no such claim" — never
	// pended, or purged after its retention.
	pendedClaims map[string]*pendRow
	// pendedKeys is the lookup index: one (requester holder, kind, key) triple maps
	// to the pendedClaims keys it identifies. More than one is an ambiguous probe.
	pendedKeys map[pendIndexKey]map[string]bool
	// now is the ledger's own clock (retention and arrival order). Injected so the
	// retention rows are deterministic; never reassigned outside tests.
	now func() time.Time
	// lastPurge throttles the lazy retention sweep (see maybePurgeLocked).
	lastPurge time.Time
}

// newMemPendLedger returns an empty ledger on the wall clock.
func newMemPendLedger() *memPendLedger {
	l := &memPendLedger{now: time.Now}
	l.resetLocked()
	return l
}

// reset clears the ledger and its lookup index. Safe for concurrent use.
func (l *memPendLedger) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resetLocked()
}

// resetLocked initializes every map. Called by the constructor (no lock needed
// yet) and by reset (which holds the lock).
func (l *memPendLedger) resetLocked() {
	l.pendedClaims = make(map[string]*pendRow)
	l.pendedKeys = make(map[pendIndexKey]map[string]bool)
	l.lastPurge = time.Time{}
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
func (l *memPendLedger) RecordPendedClaim(subjectPCI, correlationID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybePurgeLocked()
	row, found := l.pendedClaims[pendedKey(subjectPCI, correlationID)]
	if found {
		if row.rec.State == PendStateDecided {
			return nil // already decided
		}
		if pend.InProgressHeld(row.rec, l.now()) {
			return nil // never reopens a live amendment hold (PendRePend)
		}
		row.rec.State = PendStatePended
		row.rec.LastTransition = l.now()
		return nil
	}
	l.upsertLocked(subjectPCI, correlationID, PendRecord{State: PendStatePended})
	return nil
}

// BeginClaimUpdate ATOMICALLY claims a pended claim for a ClaimUpdate: if it is
// currently pended (or its amendment hold has lapsed, PendInProgressStale) it
// transitions it to in-progress and returns true; otherwise (never pended,
// already decided, or another update's live hold) it returns false. The
// test-and-set decides which amendment OWNS the row, never whether an amendment
// reaches the payer: one that does not bind is forwarded all the same. The owner
// pairs it with a decision (RecordDecision, or FinalizeClaimUpdate on a
// store-only path) or ReleaseClaimUpdate. Safe for concurrent use.
func (l *memPendLedger) BeginClaimUpdate(subjectPCI, correlationID string) (bool, error) {
	ok, _, err := l.BeginClaimUpdateReason(subjectPCI, correlationID)
	return ok, err
}

// BeginClaimUpdateReason is BeginClaimUpdate with the refusal reason. See
// PendLedger.
func (l *memPendLedger) BeginClaimUpdateReason(subjectPCI, correlationID string) (bool, PendRefusal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, found := l.pendedClaims[pendedKey(subjectPCI, correlationID)]
	var cur PendRecord
	if found {
		cur = row.rec
	}
	next, ok, why := PendBegin(cur, found, l.now())
	if ok {
		row.rec.State = next
		row.rec.LastTransition = l.now()
	}
	return ok, why, nil
}

// ReleaseClaimUpdate returns an in-progress claim to pended (a ClaimUpdate did NOT
// decide it — e.g. still insufficient or a validation error — so a later, complete
// amendment can still transition it). On a DECIDED claim it is a no-op reporting
// "already decided": a rollback after the decision never reverts it. Safe for
// concurrent use.
func (l *memPendLedger) ReleaseClaimUpdate(subjectPCI, correlationID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, found := l.pendedClaims[pendedKey(subjectPCI, correlationID)]
	if !found || row.rec.State != PendStateInProgress {
		return nil
	}
	row.rec.State = PendStatePended
	row.rec.LastTransition = l.now()
	return nil
}

// FinalizeClaimUpdate completes the pended→approved transition on the Store-only
// path: it removes the claim so a replayed update for it finds nothing (replay
// protection). On a DECIDED claim it is a no-op reporting "already decided" — the
// ledger keeps that row for its retention period so a follow-up still resolves.
// Safe for concurrent use.
func (l *memPendLedger) FinalizeClaimUpdate(subjectPCI, correlationID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := pendedKey(subjectPCI, correlationID)
	if row, found := l.pendedClaims[k]; found && row.rec.State == PendStateDecided {
		return nil // already decided
	}
	l.deleteLocked(k)
	return nil
}

// --- PendLedger ---

var _ PendLedger = (*memPendLedger)(nil)

// RecordPendedKeyed records (or re-pends) an authorization and indexes its lookup
// keys. See PendLedger.
func (l *memPendLedger) RecordPendedKeyed(subjectPCI, corrID string, created time.Time, k PendKeys) (PendTransition, error) {
	refs, err := ValidatePendKeys(k)
	if err != nil {
		return PendTransition{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybePurgeLocked()
	row, found := l.pendedClaims[pendedKey(subjectPCI, corrID)]
	var cur PendRecord
	if found {
		cur = row.rec
		if cur.RequesterHolder != "" && cur.RequesterHolder != k.RequesterHolder {
			return PendTransition{}, ErrPendRequesterMismatch
		}
	}
	next, tr := PendRePend(cur, found, created, l.now())
	next.RequesterHolder = k.RequesterHolder
	l.upsertLocked(subjectPCI, corrID, next)
	l.indexLocked(subjectPCI, corrID, k.RequesterHolder, refs)
	// The UNION the authorization now has, not this response's contribution: a
	// re-pend whose response carries no identifier leaves an authorization that is
	// still perfectly findable by the keys an earlier response gave, and reporting
	// zero for it would raise "nothing to match on" about a claim that has plenty.
	// Read under the same lock as the write, so the number is the one that landed.
	tr.Keys = len(l.pendedClaims[pendedKey(subjectPCI, corrID)].keys)
	return tr, nil
}

// LookupPended resolves a follow-up's keys within one requester's namespace. See
// PendLedger.
func (l *memPendLedger) LookupPended(requesterHolder string, k PendKeys) (PendMatch, error) {
	if requesterHolder == "" {
		return PendMatch{}, ErrPendRequesterRequired
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	hits := map[string]bool{}
	echoed := false
	for _, ref := range StrongPendProbe(k) {
		for key := range l.pendedKeys[pendIndexKey{requesterHolder: requesterHolder, kind: ref.Kind, key: ref.Key}] {
			if ref.Kind == PendKeyClaimResponseIdentifier && EchoedClaimIdentifier(ref.Key, l.heldLocked(key, requesterHolder)) {
				echoed = true
				continue
			}
			hits[key] = true
		}
	}
	var (
		candidates []PendCandidate
		held       []PendKeyRef
	)
	for key := range hits {
		row := l.pendedClaims[key]
		if row == nil { // an index entry outliving its row cannot happen; stay total
			continue
		}
		candidates = append(candidates, PendCandidate{SubjectPCI: row.subjectPCI, CorrelationID: row.correlationID})
		for idx := range row.keys {
			if idx.requesterHolder == requesterHolder {
				held = append(held, PendKeyRef{Kind: idx.kind, Key: idx.key})
			}
		}
	}
	return ResolvePendMatch(k, candidates, held, echoed), nil
}

// heldLocked is the keys the authorization filed under key holds in the
// requester's namespace.
func (l *memPendLedger) heldLocked(key, requesterHolder string) []PendKeyRef {
	row := l.pendedClaims[key]
	if row == nil {
		return nil
	}
	var held []PendKeyRef
	for idx := range row.keys {
		if idx.requesterHolder == requesterHolder {
			held = append(held, PendKeyRef{Kind: idx.kind, Key: idx.key})
		}
	}
	return held
}

// RecordDecision records the payer's terminal answer and its keys as one write.
// See PendLedger.
//
// The decision and its keys land together or not at all. In memory that means
// every guard runs BEFORE any state changes and both writes then happen under one
// hold of the mutex; the durable backend runs the same two writes in one
// transaction.
func (l *memPendLedger) RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, k PendKeys) (PendTransition, error) {
	if err := ValidatePendDecision(outcome); err != nil {
		return PendTransition{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row, found := l.pendedClaims[pendedKey(subjectPCI, corrID)]
	var cur PendRecord
	if found {
		cur = row.rec
		if cur.RequesterHolder != "" && k.RequesterHolder != "" && cur.RequesterHolder != k.RequesterHolder {
			return PendTransition{}, ErrPendRequesterMismatch
		}
	}
	next, tr := PendDecide(cur, found, outcome, decidedAt)
	// Only the decision the ledger keeps indexes its authorization number
	// (PendDecisionKept): a losing or older answer adds none.
	kept := PendDecisionKept(next, outcome, decidedAt)
	next.RequesterHolder = cur.RequesterHolder
	if !found {
		next.RequesterHolder = k.RequesterHolder
	}
	next.LastTransition = l.now()
	l.upsertLocked(subjectPCI, corrID, next)
	if refs := DecisionPendKeys(found, kept, next.RequesterHolder, k); len(refs) > 0 {
		l.indexLocked(subjectPCI, corrID, next.RequesterHolder, refs)
	}
	return tr, nil
}

// PendRecordOf reads one ledger row. See PendLedger.
func (l *memPendLedger) PendRecordOf(subjectPCI, corrID string) (PendRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, found := l.pendedClaims[pendedKey(subjectPCI, corrID)]
	if !found {
		return PendRecord{}, false, nil
	}
	return row.rec, true, nil
}

// --- ledger internals (all callers hold l.mu) ---

// upsertLocked writes rec for (subjectPCI, correlationID), creating the row if it
// is new and preserving the keys already indexed for it.
func (l *memPendLedger) upsertLocked(subjectPCI, correlationID string, rec PendRecord) {
	k := pendedKey(subjectPCI, correlationID)
	row, found := l.pendedClaims[k]
	if !found {
		row = &pendRow{subjectPCI: subjectPCI, correlationID: correlationID, keys: map[pendIndexKey]bool{}}
		l.pendedClaims[k] = row
	}
	if rec.LastTransition.IsZero() {
		rec.LastTransition = l.now()
	}
	row.rec = rec
}

// indexLocked adds refs to the lookup index for this row, unioning them with the
// keys a previous response already contributed.
func (l *memPendLedger) indexLocked(subjectPCI, correlationID, requesterHolder string, refs []PendKeyRef) {
	k := pendedKey(subjectPCI, correlationID)
	row := l.pendedClaims[k]
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
		if l.pendedKeys[idx] == nil {
			l.pendedKeys[idx] = map[string]bool{}
		}
		l.pendedKeys[idx][k] = true
	}
}

// deleteLocked removes a row and every index entry that pointed at it — the
// in-memory equivalent of the durable index's ON DELETE CASCADE.
func (l *memPendLedger) deleteLocked(k string) {
	row, found := l.pendedClaims[k]
	if !found {
		return
	}
	for idx := range row.keys {
		delete(l.pendedKeys[idx], k)
		if len(l.pendedKeys[idx]) == 0 {
			delete(l.pendedKeys, idx)
		}
	}
	delete(l.pendedClaims, k)
}

// maybePurgeLocked runs the lazy retention sweep: at most once per
// PendPurgeInterval, and at most PendPurgeMax rows per call, so a backlog is worked
// off over many calls. It mirrors the durable store's bounded purge exactly, which
// is what keeps the two backends' observable retention the same.
func (l *memPendLedger) maybePurgeLocked() {
	now := l.now()
	due, cutoff := DuePendPurge(now, l.lastPurge)
	if !due {
		return
	}
	l.lastPurge = now
	purged := 0
	for k, row := range l.pendedClaims {
		if purged >= PendPurgeMax {
			return
		}
		if row.rec.LastTransition.After(cutoff) {
			continue
		}
		l.deleteLocked(k)
		purged++
	}
}
