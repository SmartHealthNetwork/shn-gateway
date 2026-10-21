// workstream_pa_pendledger.go — the engine's side of the payer pend ledger, which moved to
// package pend (github.com/SmartHealthNetwork/shn-gateway/pa/pend). The engine
// keeps its names as deprecated aliases and forwarders until v1.0.0, so a
// store whose method signatures name the engine's types still implements the
// ledger. LedgerOf and FallbackDecision stay here: they read the engine's
// Store.
package engine

import (
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/pa/pend"
)

// LedgerOf reports the Store's pend ledger, if it has one: the Store itself when
// it implements PendLedger (the durable store), or the PA workstream's ledger
// when it is, or embeds, an in-memory Store. A store that embeds one and
// declares only part of PendLedger is answered with none rather than the
// embedded ledger in its place; New refuses such a store (checkStore). This is
// the assertion the PAS legs branch on: with a ledger they record keyed pends,
// keyed lookups and atomic decisions; without one they follow FallbackDecision.
func LedgerOf(s Store) (PendLedger, bool) {
	if l, ok := s.(PendLedger); ok {
		return l, true
	}
	if st, ok := memPAStateOf(s); ok && partialPendLedger(s) == "" {
		return st.ledger, true
	}
	return nil, false
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

// Deprecated: use pend.PendState (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendState = pend.PendState

// Deprecated: use pend.PendRefusal (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendRefusal = pend.PendRefusal

// Deprecated: use pend.PendKeys (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendKeys = pend.PendKeys

// Deprecated: use pend.PendKeyRef (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendKeyRef = pend.PendKeyRef

// Deprecated: use pend.PendMatchVerdict (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendMatchVerdict = pend.PendMatchVerdict

// Deprecated: use pend.PendMatch (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendMatch = pend.PendMatch

// Deprecated: use pend.PendCandidate (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendCandidate = pend.PendCandidate

// Deprecated: use pend.PendRecord (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendRecord = pend.PendRecord

// Deprecated: use pend.PendTransition (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendTransition = pend.PendTransition

// Deprecated: use pend.PendLedger (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
type PendLedger = pend.PendLedger

// Deprecated: use pend.PendStatePended (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendStatePended = pend.PendStatePended

// Deprecated: use pend.PendStateInProgress (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendStateInProgress = pend.PendStateInProgress

// Deprecated: use pend.PendStateDecided (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendStateDecided = pend.PendStateDecided

// Deprecated: use pend.PendOutcomeApproved (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendOutcomeApproved = pend.PendOutcomeApproved

// Deprecated: use pend.PendOutcomeDenied (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendOutcomeDenied = pend.PendOutcomeDenied

// Deprecated: use pend.DecisionConflictEvent (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const DecisionConflictEvent = pend.DecisionConflictEvent

// Deprecated: use pend.DecisionSupersededEvent (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const DecisionSupersededEvent = pend.DecisionSupersededEvent

// Deprecated: use pend.DecisionStaleAnswerEvent (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const DecisionStaleAnswerEvent = pend.DecisionStaleAnswerEvent

// Deprecated: use pend.PendNoLookupKeysEvent (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendNoLookupKeysEvent = pend.PendNoLookupKeysEvent

// Deprecated: use pend.PendRefusalNone (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendRefusalNone = pend.PendRefusalNone

// Deprecated: use pend.PendRefusalNotPended (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendRefusalNotPended = pend.PendRefusalNotPended

// Deprecated: use pend.PendRefusalInProgress (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendRefusalInProgress = pend.PendRefusalInProgress

// Deprecated: use pend.PendRefusalDecided (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendRefusalDecided = pend.PendRefusalDecided

// Deprecated: use pend.PendKeyRequestIdentifier (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendKeyRequestIdentifier = pend.PendKeyRequestIdentifier

// Deprecated: use pend.PendKeyClaimResponseIdentifier (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendKeyClaimResponseIdentifier = pend.PendKeyClaimResponseIdentifier

// Deprecated: use pend.PendKeyPreAuthRef (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendKeyPreAuthRef = pend.PendKeyPreAuthRef

// Deprecated: use pend.PendKeyItemTraceNumber (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendKeyItemTraceNumber = pend.PendKeyItemTraceNumber

// Deprecated: use pend.MaxPendKeyBytes (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const MaxPendKeyBytes = pend.MaxPendKeyBytes

// Deprecated: use pend.PendLedgerRetention (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendLedgerRetention = pend.PendLedgerRetention

// Deprecated: use pend.PendPurgeInterval (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendPurgeInterval = pend.PendPurgeInterval

// Deprecated: use pend.PendPurgeMax (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendPurgeMax = pend.PendPurgeMax

// Deprecated: use pend.PendMatchFound (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchFound = pend.PendMatchFound

// Deprecated: use pend.PendMatchNoStrongKey (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchNoStrongKey = pend.PendMatchNoStrongKey

// Deprecated: use pend.PendMatchNone (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchNone = pend.PendMatchNone

// Deprecated: use pend.PendMatchAmbiguous (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchAmbiguous = pend.PendMatchAmbiguous

// Deprecated: use pend.PendMatchDisagrees (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchDisagrees = pend.PendMatchDisagrees

// Deprecated: use pend.PendMatchRequesterKeyOnly (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendMatchRequesterKeyOnly = pend.PendMatchRequesterKeyOnly

// Deprecated: use pend.PendInProgressStale (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
const PendInProgressStale = pend.PendInProgressStale

// Deprecated: use pend.ErrPendRequesterRequired (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
var ErrPendRequesterRequired = pend.ErrPendRequesterRequired

// Deprecated: use pend.ErrPendRequesterMismatch (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
var ErrPendRequesterMismatch = pend.ErrPendRequesterMismatch

// Deprecated: use pend.ErrPendKeyTooLong (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
var ErrPendKeyTooLong = pend.ErrPendKeyTooLong

// Deprecated: use pend.ErrPendOutcomeInvalid (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
var ErrPendOutcomeInvalid = pend.ErrPendOutcomeInvalid

// Deprecated: use pend.ProbePendKeys (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func ProbePendKeys(k PendKeys) []PendKeyRef {
	return pend.ProbePendKeys(k)
}

// Deprecated: use pend.StrongPendProbe (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func StrongPendProbe(k PendKeys) []PendKeyRef {
	return pend.StrongPendProbe(k)
}

// Deprecated: use pend.ResolvePendMatch (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func ResolvePendMatch(stated PendKeys, candidates []PendCandidate, held []PendKeyRef, echoed bool) PendMatch {
	return pend.ResolvePendMatch(stated, candidates, held, echoed)
}

// Deprecated: use pend.PendDecisionKept (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func PendDecisionKept(next PendRecord, outcome string, decidedAt time.Time) bool {
	return pend.PendDecisionKept(next, outcome, decidedAt)
}

// Deprecated: use pend.PendKeysAgree (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func PendKeysAgree(held []PendKeyRef, stated PendKeys) (bool, string) {
	return pend.PendKeysAgree(held, stated)
}

// Deprecated: use pend.DecisionPendKeys (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func DecisionPendKeys(found, kept bool, requesterHolder string, k PendKeys) []PendKeyRef {
	return pend.DecisionPendKeys(found, kept, requesterHolder, k)
}

// Deprecated: use pend.EchoedClaimIdentifier (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func EchoedClaimIdentifier(claimResponseKey string, held []PendKeyRef) bool {
	return pend.EchoedClaimIdentifier(claimResponseKey, held)
}

// Deprecated: use pend.PendBegin (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func PendBegin(cur PendRecord, found bool, now time.Time) (next PendState, ok bool, why PendRefusal) {
	return pend.PendBegin(cur, found, now)
}

// Deprecated: use pend.PendRePend (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func PendRePend(cur PendRecord, found bool, created, now time.Time) (next PendRecord, tr PendTransition) {
	return pend.PendRePend(cur, found, created, now)
}

// Deprecated: use pend.PendDecide (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func PendDecide(cur PendRecord, found bool, outcome string, decidedAt time.Time) (next PendRecord, tr PendTransition) {
	return pend.PendDecide(cur, found, outcome, decidedAt)
}

// Deprecated: use pend.ValidatePendDecision (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func ValidatePendDecision(outcome string) error {
	return pend.ValidatePendDecision(outcome)
}

// Deprecated: use pend.ValidatePendKeys (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func ValidatePendKeys(k PendKeys) ([]PendKeyRef, error) {
	return pend.ValidatePendKeys(k)
}

// Deprecated: use pend.DuePendPurge (package github.com/SmartHealthNetwork/shn-gateway/pa/pend).
func DuePendPurge(now, lastPurge time.Time) (due bool, cutoff time.Time) {
	return pend.DuePendPurge(now, lastPurge)
}
