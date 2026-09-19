// pasleg.go — what the engine knows about a PAS exchange that the payer content
// seam does not take, and the pend-ledger effects the submit and update legs
// derive from the payer's own answer.
//
// WHY A CONTEXT CARRIER. `LegResponder` takes the leg, the correlation and the
// subject: the authority outputs a content occupant needs to build and to key its
// own writes. The pend ledger needs one more — the VERIFIED requester holder, the
// envelope Sender the inbound handler authenticated — because every lookup key is
// namespaced by the requester that submitted the claim, and a key in no namespace
// would match in all of them. That fact is the engine's, never the connector's, so
// it travels beside the exchange on the context rather than through the content
// seam, exactly as the native certification capture does. Nothing outside this
// package can set it, and an injected `LegResponder` that never reads it keeps
// working unchanged.
//
// The same carrier collects the leg's OBSERVER NOTES. A payer gateway's legs raise
// facts an operator needs — a re-issue after the payer's own version conflict, a
// ledger transition the payer's later word forced, a pend no follow-up can name —
// and the responder has no observer of its own. It appends the event kinds here;
// the inbound handler emits them once the exchange is sealed, tied to its
// correlation.
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// pasLegKey is the context key for the per-exchange carrier.
type pasLegKey struct{}

// pasLeg carries the verified requester holder and the notes the leg raised.
type pasLeg struct {
	requester string
	notes     []string
}

// withPASLeg attaches a carrier for this exchange, returning the context and the
// carrier the caller reads its notes back from. requester is the VERIFIED
// envelope Sender.
func withPASLeg(ctx context.Context, requester string) (context.Context, *pasLeg) {
	leg := &pasLeg{requester: strings.TrimSpace(requester)}
	return context.WithValue(ctx, pasLegKey{}, leg), leg
}

// pasLegOf reads the carrier, or nil when the caller attached none (an injected
// responder, or a test driving the leg directly).
func pasLegOf(ctx context.Context) *pasLeg {
	leg, _ := ctx.Value(pasLegKey{}).(*pasLeg)
	return leg
}

// requesterHolderOf is the verified requester this exchange came from, or "".
func requesterHolderOf(ctx context.Context) string {
	if leg := pasLegOf(ctx); leg != nil {
		return leg.requester
	}
	return ""
}

// note records one observer event kind for the inbound handler to emit. A nil
// carrier drops it: a leg driven without one has no exchange to tie it to.
func (l *pasLeg) note(kind string) {
	if l == nil || kind == "" {
		return
	}
	l.notes = append(l.notes, kind)
}

// noteOn is note on the carrier this context holds.
func noteOn(ctx context.Context, kind string) { pasLegOf(ctx).note(kind) }

// RetryVersionConflictEvent is raised when an amendment was re-issued once after
// the payer's own store refused the write its resolution timer had already made.
// The message re-sent is the identical payload; the note says a second attempt
// happened, which an operator reading one exchange would otherwise not see.
const RetryVersionConflictEvent = "retry:version-conflict"

// pasAnswerKeys reads the lookup keys and the payer's own date out of a PAS
// response Bundle: the payer's `ClaimResponse.identifier`s, the submitted
// `Claim.identifier` it echoed in `request.identifier`, the pre-authorization
// reference and the item trace numbers. It is the SAME reader the inquiry leg
// uses, so a pend recorded at submit and a decision learned at inquiry can never
// be keyed differently.
func pasAnswerKeys(requesterHolder string, answer []byte) (PendKeys, time.Time) {
	keys := PendKeys{RequesterHolder: requesterHolder}
	var created time.Time
	for _, a := range readPASInquiryAnswers(requesterHolder, answer) {
		keys.RequestIDs = append(keys.RequestIDs, a.keys.RequestIDs...)
		keys.ClaimResponseIDs = append(keys.ClaimResponseIDs, a.keys.ClaimResponseIDs...)
		keys.ItemTraceNumbers = append(keys.ItemTraceNumbers, a.keys.ItemTraceNumbers...)
		if keys.PreAuthRef == "" {
			keys.PreAuthRef = a.keys.PreAuthRef
		}
		if created.IsZero() {
			created = a.created
		}
	}
	return keys, created
}

// pasRequestKeys reads the identifiers the REQUESTER put on its own submission:
// the `Claim.identifier` values and the item trace numbers. They belong in the
// index because a follow-up is made by the requester from its own records, so an
// authorization stays findable by what the requester retained even when the payer
// echoed none of it. Nothing here is minted: every value is read from the message
// that was actually sent.
func pasRequestKeys(requestFHIR []byte) PendKeys {
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Identifier   []struct {
					System string `json:"system"`
					Value  string `json:"value"`
				} `json:"identifier"`
				Item []struct {
					Extension []json.RawMessage `json:"extension"`
				} `json:"item"`
			} `json:"resource"`
		} `json:"entry"`
	}
	var keys PendKeys
	if decodeMessage(requestFHIR, &b) != nil {
		return keys
	}
	for _, e := range b.Entry {
		if e.Resource.ResourceType != "Claim" {
			continue
		}
		for _, id := range e.Resource.Identifier {
			if k := identifierKey(id.System, id.Value); k != "" {
				keys.RequestIDs = append(keys.RequestIDs, k)
			}
		}
		for _, it := range e.Resource.Item {
			if k := identifierExtensionKey(it.Extension, pasExtItemTraceNumberURL); k != "" {
				keys.ItemTraceNumbers = append(keys.ItemTraceNumbers, k)
			}
		}
	}
	return keys
}

// mergePendKeys unions two key sets under one requester namespace.
func mergePendKeys(requesterHolder string, a, b PendKeys) PendKeys {
	out := PendKeys{RequesterHolder: requesterHolder, PreAuthRef: a.PreAuthRef}
	if out.PreAuthRef == "" {
		out.PreAuthRef = b.PreAuthRef
	}
	out.RequestIDs = append(append([]string{}, a.RequestIDs...), b.RequestIDs...)
	out.ClaimResponseIDs = append(append([]string{}, a.ClaimResponseIDs...), b.ClaimResponseIDs...)
	out.ItemTraceNumbers = append(append([]string{}, a.ItemTraceNumbers...), b.ItemTraceNumbers...)
	return out
}

// recordPASPend is the ledger effect of a payer PEND: the authorization is
// recorded (or re-pended) under the union of the keys the payer's answer and the
// requester's own submission state. A Store with no pend ledger, or an exchange
// with no verified requester to namespace the keys under, falls back to the
// keyless Store pend: the claim is still recorded so an amendment on the same
// correlation binds, and only discoverability by a later inquiry is limited.
func recordPASPend(ctx context.Context, store Store, subjectPCI, corrID string, keys PendKeys, created time.Time) func() error {
	ledger, hasLedger := LedgerOf(store)
	requester := requesterHolderOf(ctx)
	if !hasLedger || requester == "" {
		return func() error { return store.RecordPendedClaim(subjectPCI, corrID) }
	}
	leg := pasLegOf(ctx)
	return func() error {
		tr, err := ledger.RecordPendedKeyed(subjectPCI, corrID, created, keys)
		if err != nil {
			return err
		}
		if tr.Event != "" {
			leg.note(tr.Event)
		}
		if tr.Keys == 0 {
			// Nothing any follow-up can name this authorization by. The payer's
			// bytes still relayed and the pend is still recorded, so an amendment on
			// the same correlation still binds; only discoverability by a later
			// inquiry is limited. An operator sees it here rather than at that
			// inquiry, and closing it can only ever ADD discoverability.
			leg.note(PendNoLookupKeysEvent)
		}
		return nil
	}
}

// recordPASDecision is the ledger effect of a payer DECISION: the outcome the
// payer stated, dated as the payer dated it, with its decision EOB written in the
// same write. `decided` is absorbing, so a denial finalizes exactly as an approval
// does and the authorization is never re-pended by anything but a later payer
// re-pend. A Store with no ledger follows FallbackDecision, and its EOB is written
// on its own as it always was.
func recordPASDecision(ctx context.Context, store Store, subjectPCI, corrID, outcome string, decidedAt time.Time, eob *EOBRecord) func() error {
	ledger, hasLedger := LedgerOf(store)
	if !hasLedger {
		return func() error {
			if eob != nil {
				if err := store.RecordEOB(eob.SubjectPCI, eob.EOBID, eob.JSON); err != nil {
					return err
				}
			}
			return FallbackDecision(store, subjectPCI, corrID)
		}
	}
	leg := pasLegOf(ctx)
	return func() error {
		tr, err := ledger.RecordDecision(subjectPCI, corrID, outcome, decidedAt, eob)
		if err != nil {
			return err
		}
		if tr.Event != "" {
			leg.note(tr.Event)
		}
		return nil
	}
}
