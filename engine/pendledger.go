// pendledger.go — the payer-side pend ledger: the keyed lookup, the state machine
// and the retention rule a payer gateway needs to answer a follow-up about an
// authorization it pended.
//
// WHY IT IS A SEPARATE, OPTIONAL INTERFACE. Store's pended-claim methods key a
// claim by (subjectPCI, correlationID) — the gateway's own correlation, which a
// requester asking about the authorization later does not have. A follow-up
// (`Claim/$inquire`, or an amendment arriving on a fresh correlation) carries the
// PAYER's identifiers instead: the `ClaimResponse.request.identifier` the payer
// echoed, its own `ClaimResponse.identifier`, the `preAuthRef`, or the request's
// item trace numbers. The ledger indexes exactly those, namespaced by the requester
// that submitted the claim, so a follow-up resolves to the right authorization and
// to nobody else's.
//
// Store itself does NOT change: a Store written without the ledger keeps working,
// and the legs reach the ledger with LedgerOf. A store with no ledger follows
// FallbackDecision.
//
// AI-1 holds: every field here is metadata or a decision (a state, an outcome, a
// date, an identifier, one PA-decision EOB the payer issued about its own claim).
// No clinical content is stored, and nothing here is a cross-holder record.
package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PendState is the ledger's state for one authorization.
type PendState string

const (
	// PendStatePended is "awaiting supplemental data" — the payer pended it.
	PendStatePended PendState = "pended"
	// PendStateInProgress is "an amendment is mid-adjudication for it".
	PendStateInProgress PendState = "in_progress"
	// PendStateDecided is ABSORBING: the payer answered approved or denied, and no
	// Store transition moves it. Only a payer re-pend the payer dated LATER than
	// the decision can (see PendRePend).
	PendStateDecided PendState = "decided"
)

// The decision outcomes the ledger records. A pend is not a decision: it is
// recorded with RecordPendedKeyed, never as an outcome here.
const (
	PendOutcomeApproved = "approved"
	PendOutcomeDenied   = "denied"
)

// The observer event kinds the ledger's transitions raise. The leg emits them; the
// ledger decides which one applies and reports it in a PendTransition.
const (
	// DecisionConflictEvent: a second, DIFFERENT outcome arrived for a claim the
	// ledger already has as decided. The decision the payer dated later is kept.
	DecisionConflictEvent = "decision.conflict"
	// DecisionSupersededEvent: the payer re-pended a claim the ledger had as
	// decided, dating the re-pend after the decision. The payer's later word wins.
	DecisionSupersededEvent = "decision.superseded"
	// DecisionStaleAnswerEvent: the payer re-pended a decided claim, but dated the
	// re-pend no later than the decision. The decision stands; the requester still
	// receives the payer's bytes.
	DecisionStaleAnswerEvent = "decision.stale-answer"
	// PendNoLookupKeysEvent: a pend was recorded for an authorization that no
	// response has given an identifier to match a follow-up on
	// (PendTransition.Keys is zero — the union, so a re-pend that adds no key to an
	// already-keyed authorization does NOT raise this). See PendTransition.Keys for
	// what that costs and what it does not.
	PendNoLookupKeysEvent = "pend.no-lookup-keys"
)

// PendRefusal says why an amendment could not bind a claim. The ledger records
// and never gates: the amendment reaches the payer either way, and the reason is
// noted for the operator (pend.amendment-unbound:<reason>), so the reasons stay
// distinguishable from one another.
type PendRefusal string

const (
	PendRefusalNone       PendRefusal = ""
	PendRefusalNotPended  PendRefusal = "not-pended"
	PendRefusalInProgress PendRefusal = "update-in-progress"
	PendRefusalDecided    PendRefusal = "already-decided"
)

// The lookup key kinds. A key is stored and matched as (kind, value), so a
// preAuthRef can never collide with an identifier that happens to read the same.
const (
	PendKeyRequestIdentifier       = "request-identifier"
	PendKeyClaimResponseIdentifier = "claimresponse-identifier"
	PendKeyPreAuthRef              = "preauthref"
	PendKeyItemTraceNumber         = "item-tracenumber"
)

// MaxPendKeyBytes bounds one lookup key. The value is payer- or requester-supplied
// and is part of the durable index's primary key, so an oversized value would
// overflow the btree index row on a healthy database (the same reason
// MaxReplayKeyBytes exists). Every backend refuses an oversized key identically,
// and the durable schema carries the matching CHECK as the backstop.
const MaxPendKeyBytes = 512

// PendLedgerRetention is how long a ledger row survives its last transition. Prior
// Authorization keeps a pended authorization queryable for at least six months, so
// this is the LONGEST six-month span (184 days: Mar 1 → Sep 1), never a shorter
// approximation — the ledger must not purge earlier than the IG requires. Disclosed
// in docs/CONFIGURATION.md.
const PendLedgerRetention = 184 * 24 * time.Hour

// PendPurgeInterval throttles the lazy retention sweep (the exchange store's
// maybePurge pattern) and PendPurgeMax bounds ONE sweep, so a backlog is worked off
// over many calls instead of in one unbounded DELETE.
const (
	PendPurgeInterval = time.Minute
	PendPurgeMax      = 100
)

// The ledger's guards. Each is refused before any state changes, so a refused call
// leaves the ledger exactly as it was.
var (
	// ErrPendRequesterRequired: the requester holder namespaces every key, so a
	// missing one would put a key in no namespace at all (or match in all of them).
	ErrPendRequesterRequired = errors.New("engine: pend ledger: a requester holder is required")
	// ErrPendRequesterMismatch: a pend row belongs to the requester that submitted
	// the claim; another requester must not re-pend or re-key it.
	ErrPendRequesterMismatch = errors.New("engine: pend ledger: the claim was submitted by another requester")
	// ErrPendKeyTooLong: see MaxPendKeyBytes.
	ErrPendKeyTooLong = errors.New("engine: pend ledger: lookup key is too long")
	// ErrPendOutcomeInvalid: RecordDecision records a DECISION. A pend is not one.
	ErrPendOutcomeInvalid = errors.New("engine: pend ledger: outcome is not a decision")
	// ErrPendEOBInvalid: the EOB is written in the SAME transaction as the
	// decision, so a malformed one refuses the decision rather than landing half.
	ErrPendEOBInvalid = errors.New("engine: pend ledger: EOB record is not writable")
	// ErrEOBSubjectMismatch prevents an EOB ID already filed for one patient from
	// exposing replacement bytes through that patient's Patient Access list.
	ErrEOBSubjectMismatch = errors.New("engine: EOB id belongs to another patient")
)

// PendKeys are the facts a follow-up can name an authorization by, as the payer's
// response carried them. Every value is "system|value" for an Identifier, and the
// requester holder namespaces all of them.
//
// It holds LOOKUP KEYS AND NOTHING ELSE. In particular there is no field for the
// INQUIRY's own Claim.identifier: matching on it would resolve a follow-up by a
// fact the inquiry minted for itself rather than by anything the payer answered,
// and a ledger cannot match on a fact it has no field for
// (TestPendKeys_FieldSetIsPinned keeps it that way). RequestIDs are the identifiers
// of the claim that was SUBMITTED — the ones the payer echoes in
// `ClaimResponse.request.identifier`, and the ones a requester retained from its
// own submission — never the identifiers minted on a fresh inquiry Claim.
type PendKeys struct {
	// RequesterHolder is the verified envelope Sender: the namespace of every key.
	RequesterHolder string
	// RequestIDs are the submitted Claim.identifier values the payer echoes in
	// ClaimResponse.request.identifier, as "system|value".
	RequestIDs []string
	// ClaimResponseIDs are the payer's own ClaimResponse.identifier values, as
	// "system|value".
	ClaimResponseIDs []string
	// PreAuthRef is the payer's pre-authorization reference.
	PreAuthRef string
	// ItemTraceNumbers are the request's item traceNumber values, as
	// "system|value".
	ItemTraceNumbers []string
}

// PendKeyRef is one stored lookup key: its kind and its value.
type PendKeyRef struct {
	Kind string
	Key  string
}

// Refs returns the keys to index or probe with, in a stable order, deduplicated and
// bounds-checked. An empty value is not a key and is dropped; an oversized one is
// refused with ErrPendKeyTooLong.
//
// It is the ONLY place a PendKeys field becomes a stored key, so the index can
// never hold a fact this function did not produce — the durable backend asserts
// exactly that (one indexed row per Ref, no more).
func (k PendKeys) Refs() ([]PendKeyRef, error) {
	var out []PendKeyRef
	seen := map[PendKeyRef]bool{}
	add := func(kind, raw string) error {
		v := strings.TrimSpace(raw)
		if v == "" {
			return nil
		}
		if len(v) > MaxPendKeyBytes {
			return fmt.Errorf("%w: %s is %d bytes (max %d)", ErrPendKeyTooLong, kind, len(v), MaxPendKeyBytes)
		}
		ref := PendKeyRef{Kind: kind, Key: v}
		if seen[ref] {
			return nil
		}
		seen[ref] = true
		out = append(out, ref)
		return nil
	}
	for _, v := range k.RequestIDs {
		if err := add(PendKeyRequestIdentifier, v); err != nil {
			return nil, err
		}
	}
	for _, v := range k.ClaimResponseIDs {
		if err := add(PendKeyClaimResponseIdentifier, v); err != nil {
			return nil, err
		}
	}
	if err := add(PendKeyPreAuthRef, k.PreAuthRef); err != nil {
		return nil, err
	}
	for _, v := range k.ItemTraceNumbers {
		if err := add(PendKeyItemTraceNumber, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ProbePendKeys is Refs for a LOOKUP: an oversized key can never have been recorded
// (RecordPendedKeyed refuses it), so a probe carrying one simply matches nothing
// instead of failing the follow-up.
func ProbePendKeys(k PendKeys) []PendKeyRef {
	trimmed := k
	trimmed.RequestIDs = boundedKeys(k.RequestIDs)
	trimmed.ClaimResponseIDs = boundedKeys(k.ClaimResponseIDs)
	trimmed.ItemTraceNumbers = boundedKeys(k.ItemTraceNumbers)
	if len(strings.TrimSpace(k.PreAuthRef)) > MaxPendKeyBytes {
		trimmed.PreAuthRef = ""
	}
	refs, err := trimmed.Refs()
	if err != nil { // unreachable: every oversized value was dropped above
		return nil
	}
	return refs
}

func boundedKeys(in []string) []string {
	var out []string
	for _, v := range in {
		if len(strings.TrimSpace(v)) <= MaxPendKeyBytes {
			out = append(out, v)
		}
	}
	return out
}

// EOBRecord is one PA-decision EOB, written in the SAME write as the decision it
// belongs to. It carries what Store.RecordEOB(subjectPCI, eobID, eobJSON) takes, so
// the durable backend can write the decision and the EOB in one transaction.
type EOBRecord struct {
	SubjectPCI string
	EOBID      string
	JSON       []byte
}

// Validate refuses an EOB the ledger cannot write. A nil receiver is "this decision
// has no EOB", which is valid (an update leg records a decision without one).
// The subject must be the decision's subject: an EOB filed against another patient
// would be a cross-patient write, not a payer recording its own claim.
func (e *EOBRecord) Validate(subjectPCI string) error {
	if e == nil {
		return nil
	}
	if e.EOBID == "" {
		return fmt.Errorf("%w: no EOB id", ErrPendEOBInvalid)
	}
	if len(e.JSON) == 0 {
		return fmt.Errorf("%w: no EOB bytes", ErrPendEOBInvalid)
	}
	if e.SubjectPCI != subjectPCI {
		return fmt.Errorf("%w: EOB subject %q is not the decision's subject %q", ErrPendEOBInvalid, e.SubjectPCI, subjectPCI)
	}
	return nil
}

// PendRecord is one ledger row, as a reader sees it. Metadata only.
type PendRecord struct {
	State           PendState
	RequesterHolder string
	// Outcome is the recorded decision (PendOutcome*), empty unless State is
	// PendStateDecided.
	Outcome string
	// DecidedAt is the date the PAYER gave the decision (ClaimResponse.created),
	// not the time the row was written. It is what the supersede and conflict rules
	// compare.
	DecidedAt time.Time
	// LastTransition is the store's own clock at the last ledger WRITE for this
	// authorization — a pend, a re-pend or a decision, whether or not it changed
	// the state — except a re-pend that leaves a live amendment hold alone, which
	// keeps the hold's time so a stranded hold lapses (PendInProgressStale).
	// Retention counts from here, so an authorization the payer is still
	// speaking about stays answerable, and a payer's own clock can neither shorten
	// nor extend the period.
	LastTransition time.Time
}

// PendTransition reports what a ledger write did, so the leg can emit the right
// observer event without re-reading the row (and racing another leg).
type PendTransition struct {
	// From is the state before the write; "" when there was no row.
	From PendState
	// To is the state after the write.
	To PendState
	// Event is the observer event kind this transition raises
	// (Decision*Event), or "" for an ordinary transition.
	Event string
	// Changed is false when the write was idempotent — the same decision recorded
	// twice, or a stale answer that left the ledger alone.
	Changed bool
	// Keys is how many lookup keys the authorization HAS after this write — the
	// union of every response recorded for it, not this response's contribution. A
	// re-pend whose response carries no identifier still leaves an authorization
	// that a follow-up can name by the keys an earlier response gave, so the number
	// that answers "can anything find this?" is the union.
	//
	// ZERO is therefore a DISCLOSED LIMITATION, not a failure: no response for this
	// authorization has carried an identifier — no echoed request identifier, no
	// ClaimResponse identifier, no preAuthRef, no item trace number — so there is
	// nothing for a later inquiry to name it by. What is NOT lost: the payer's
	// bytes still relay to the requester unchanged, and the pend is still recorded,
	// so an amendment on the same correlation still binds. Only discoverability by
	// inquiry is limited. A leg emits PendNoLookupKeysEvent when it sees this, so an
	// operator can see it rather than discovering it at the next inquiry. Surfacing
	// such a response more fully is a known limitation, and closing it can only ever
	// ADD discoverability: the bytes relayed and the pend recorded are the same
	// either way. The gateway will not invent an identifier the payer did not send.
	Keys int
}

// PendLedger is the keyed pend ledger a payer gateway keeps for the authorizations
// it pends and decides. It is OPTIONAL: Store does not change, and a caller reaches
// it with LedgerOf.
//
// Every write returns the TRANSITION it performed, and the amendment check returns
// the REASON it did not bind, because the rules this ledger implements are stated in
// terms of both: which of the decision events a write raised, and whether an
// amendment did not bind because the authorization was already decided, held by
// another amendment, or not pended here at all. A caller
// cannot re-derive either by reading the row back — another leg may have moved it
// in between — so the write reports it.
type PendLedger interface {
	// RecordPendedKeyed records (or re-pends) an authorization and indexes its
	// lookup keys under k.RequesterHolder. created is the payer's
	// `ClaimResponse.created` for THIS response — an input to the timestamp rule
	// below, never a lookup key. A re-pend UNIONS the new response's keys with the
	// ones already recorded: the original submission's identifiers keep identifying
	// the authorization.
	RecordPendedKeyed(subjectPCI, corrID string, created time.Time, k PendKeys) (PendTransition, error)
	// LookupPended resolves a follow-up's keys to the authorization they name,
	// within requesterHolder's namespace and nowhere else. A decided claim is still
	// found — an inquiry about it must resolve to the decision, not to "no such
	// authorization". Two different authorizations matching one probe is
	// ambiguous=true with found=false and no ledger change: the ledger will not
	// guess which one the requester meant.
	LookupPended(requesterHolder string, k PendKeys) (subjectPCI, corrID string, found, ambiguous bool, err error)
	// RecordDecision records the payer's terminal answer and its EOB as ONE write:
	// if the EOB cannot be written, there is no decision. decidedAt is the payer's
	// ClaimResponse.created. Idempotent for the same outcome; a different outcome
	// keeps the one the payer dated later and reports DecisionConflictEvent.
	RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time, eob *EOBRecord) (PendTransition, error)
	// BeginClaimUpdateReason is Store.BeginClaimUpdate with the refusal reason: the
	// atomic test-and-set that binds a pended authorization for one amendment, and
	// says why when it will not.
	BeginClaimUpdateReason(subjectPCI, corrID string) (claimed bool, why PendRefusal, err error)
	// PendRecordOf reads one ledger row. found=false when there is none; the error
	// is reserved for a store failure, so "absent" and "unavailable" stay
	// distinguishable (unlike Store's read methods, which cannot).
	PendRecordOf(subjectPCI, corrID string) (PendRecord, bool, error)
}

// LedgerOf reports the Store's pend ledger, if it has one. This is the assertion
// the PAS legs branch on: with a ledger they record keyed pends, keyed lookups and
// atomic decisions; without one they follow FallbackDecision.
func LedgerOf(s Store) (PendLedger, bool) {
	l, ok := s.(PendLedger)
	if !ok {
		return nil, false
	}
	return l, true
}

// FallbackDecision is the ledger effect of a terminal payer decision — approved OR
// DENIED — on a Store with no PendLedger: today's FinalizeClaimUpdate. A denial
// finalizes exactly as an approval does, so a denied claim is never re-pended.
//
// There is deliberately no inquiry entry point beside it: on such a store an
// inquiry changes no ledger state at all, because there is no ledger to change.
func FallbackDecision(s Store, subjectPCI, corrID string) error {
	return s.FinalizeClaimUpdate(subjectPCI, corrID)
}

// --- the state machine, shared by every backend ---
//
// These helpers are pure: given the row a backend read (under its own lock or row
// lock) they return the row to write and the transition to report. All three
// backends run the SAME functions, so "the three backends agree" is true by
// construction rather than by three parallel implementations that must be kept in
// step.
//
// THE TIMESTAMP RULE, in one place. Both rules below compare a date the PAYER put
// in its response against a date the ledger recorded, and both read an absent or
// equal date the same way: NOT LATER. An answer that is not later than what the
// ledger already holds does not displace it — an undated conflicting decision does
// not replace a dated one, and an undated re-pend does not reopen a decision.
// `ClaimResponse.created` is 1..1, so an undated answer is a malformed response that
// FR-G28 validation refuses before it reaches the ledger at all; if one ever does,
// not acting on it is the safe direction, and the payer's bytes still relay either
// way. The one place arrival order decides anything is between two decisions the
// payer dated the SAME instant, where there is nothing else to go on.

// PendInProgressStale is how long an amendment holds an authorization in
// progress. No leg lasts nearly this long (the Hub's forward and a requester's
// leg each wait 30 seconds), so a row still held after it was stranded: the
// gateway serving the amendment stopped, or lost its store, before the release.
// Past it, the hold lapses: a re-pend moves the row again and a new amendment
// binds it. A re-pend never refreshes a live hold, so a stranded row ages out.
const PendInProgressStale = 5 * time.Minute

// inProgressHeld reports whether cur is an amendment's live hold at now.
func inProgressHeld(cur PendRecord, now time.Time) bool {
	return cur.State == PendStateInProgress && now.Sub(cur.LastTransition) < PendInProgressStale
}

// PendBegin is the Begin transition: pended → in_progress, a lapsed hold
// (PendInProgressStale) → in_progress, and a refusal with its reason from
// anywhere else.
func PendBegin(cur PendRecord, found bool, now time.Time) (next PendState, ok bool, why PendRefusal) {
	if !found {
		return "", false, PendRefusalNotPended
	}
	switch cur.State {
	case PendStatePended:
		return PendStateInProgress, true, PendRefusalNone
	case PendStateInProgress:
		if inProgressHeld(cur, now) {
			return cur.State, false, PendRefusalInProgress
		}
		return PendStateInProgress, true, PendRefusalNone
	case PendStateDecided:
		return cur.State, false, PendRefusalDecided
	}
	// An unknown state is not a pended claim. A backend never writes one; this keeps
	// the function total rather than trusting that.
	return cur.State, false, PendRefusalNotPended
}

// PendRePend is RecordPendedKeyed's state effect. It sets the row's
// LastTransition to now, except on a live amendment hold, which it leaves as it
// is.
//
// On a decided claim the payer's later word wins and its earlier word does not: the
// row returns to pended only when the payer dated the re-pend after the decision,
// and otherwise the decision stands. Either way the requester receives the payer's
// bytes — that is the leg's job, not the ledger's.
//
// A re-pend never reopens an amendment in progress: the backend records its keys,
// but only that amendment's own outcome (its release, its re-pend, or the payer's
// decision) moves the row. A resent submission the payer pended again must not let
// a second amendment bind while the first is still with the payer. A hold past
// PendInProgressStale has lapsed and is re-pended like a pended row.
func PendRePend(cur PendRecord, found bool, created, now time.Time) (next PendRecord, tr PendTransition) {
	if !found {
		return PendRecord{State: PendStatePended, LastTransition: now}, PendTransition{To: PendStatePended, Changed: true}
	}
	if inProgressHeld(cur, now) {
		return cur, PendTransition{From: PendStateInProgress, To: PendStateInProgress}
	}
	next = cur
	next.LastTransition = now
	if cur.State != PendStateDecided {
		next.State = PendStatePended
		return next, PendTransition{From: cur.State, To: PendStatePended, Changed: cur.State != PendStatePended}
	}
	if created.After(cur.DecidedAt) {
		next.State = PendStatePended
		// The decision is superseded, so it is no longer the row's answer: a later
		// re-pend must be judged against the NEXT decision, not against this one.
		next.Outcome, next.DecidedAt = "", time.Time{}
		return next, PendTransition{From: PendStateDecided, To: PendStatePended, Event: DecisionSupersededEvent, Changed: true}
	}
	// Not later (earlier, the same instant, or undated): the decision stands.
	return next, PendTransition{From: PendStateDecided, To: PendStateDecided, Event: DecisionStaleAnswerEvent}
}

// PendDecide is RecordDecision's state effect. decided is absorbing: a decision for
// an already-decided claim never reopens it, it only decides which outcome the row
// keeps — the one the payer dated later, and on an exact tie the one that arrived
// later.
func PendDecide(cur PendRecord, found bool, outcome string, decidedAt time.Time) (next PendRecord, tr PendTransition) {
	if !found {
		return PendRecord{State: PendStateDecided, Outcome: outcome, DecidedAt: decidedAt},
			PendTransition{To: PendStateDecided, Changed: true}
	}
	if cur.State != PendStateDecided {
		next = cur
		next.State, next.Outcome, next.DecidedAt = PendStateDecided, outcome, decidedAt
		return next, PendTransition{From: cur.State, To: PendStateDecided, Changed: true}
	}
	if cur.Outcome == outcome {
		// Idempotent: the same answer relayed twice (duplicate terminal inquiries)
		// is one decision.
		return cur, PendTransition{From: PendStateDecided, To: PendStateDecided}
	}
	tr = PendTransition{From: PendStateDecided, To: PendStateDecided, Event: DecisionConflictEvent}
	if decidedAt.Before(cur.DecidedAt) {
		// Not later — including an undated answer, whose zero date is before any
		// recorded one. The recorded decision stands.
		return cur, tr
	}
	next = cur
	next.Outcome, next.DecidedAt = outcome, decidedAt
	tr.Changed = true
	return next, tr
}

// ValidatePendDecision refuses an outcome that is not a decision. A pend is
// recorded with RecordPendedKeyed; recording it as an outcome here would make
// "decided" mean "pended" on one backend and not another.
func ValidatePendDecision(outcome string) error {
	switch outcome {
	case PendOutcomeApproved, PendOutcomeDenied:
		return nil
	}
	return fmt.Errorf("%w: %q (want %q or %q)", ErrPendOutcomeInvalid, outcome, PendOutcomeApproved, PendOutcomeDenied)
}

// ValidatePendKeys runs the guards every backend applies before it touches state:
// the requester namespace, and the key bound. It returns the keys to index.
func ValidatePendKeys(k PendKeys) ([]PendKeyRef, error) {
	if strings.TrimSpace(k.RequesterHolder) == "" {
		return nil, ErrPendRequesterRequired
	}
	return k.Refs()
}

// DuePendPurge reports whether a lazy retention sweep is due, and the instant rows
// must have last transitioned before to be purged. Shared so both durable and
// in-memory backends throttle and cut at the same points.
func DuePendPurge(now, lastPurge time.Time) (due bool, cutoff time.Time) {
	if now.Sub(lastPurge) < PendPurgeInterval {
		return false, time.Time{}
	}
	return true, now.Add(-PendLedgerRetention)
}
