package engine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A gateway's summary of the involved list it sends on one side of a leg
// (shnsdk.InvolvedSummary): how many entries the list has, and how many
// patients this side's involved pass left out, by reason, signed with this
// holder's own signing key, as its holder assertions are, and bound to the
// side's own token. It is sent only to a Hub that lists both
// shnsdk.HubAcceptsInvolved and shnsdk.HubAcceptsInvolvedSummary
// (Config.HubAcceptsInvolvedSummary), on a side whose leg takes the list
// (legInvolvement); otherwise Metadata.InvolvedSummary stays empty and the
// envelope is what it was without it.
//
// A summary stating no omission is this gateway's own statement that it left
// out no patient it knew of; the Hub records it as that statement, and the
// network never judges whether the list named every patient the message
// involves. A side with no list at all, its pass having left no one out,
// states listed 0 and no omission; one whose pass left patients out (a
// request whose patients could not be read at all among them) states each
// reason it left them out for.
//
// It is recording only: a summary that cannot be made is not sent, which the
// Hub reads as not knowing whether the list was complete, and nothing about
// it refuses, holds up or fails a leg.

// involvedTally counts, by reason, the patients one side's involved pass left
// out (omitInvolved), for that side's summary alone: a pass counts into the
// tally its own context carries (withInvolvedTally), never another side's or
// another call's.
type involvedTally struct {
	mu     sync.Mutex
	counts map[string]int
}

type involvedTallyKey struct{}

// withInvolvedTally is ctx with a new, empty tally, which replaces any tally
// ctx already carries.
func withInvolvedTally(ctx context.Context) (context.Context, *involvedTally) {
	t := &involvedTally{counts: map[string]int{}}
	return context.WithValue(ctx, involvedTallyKey{}, t), t
}

func involvedTallyFrom(ctx context.Context) *involvedTally {
	t, _ := ctx.Value(involvedTallyKey{}).(*involvedTally)
	return t
}

// add counts one patient left out for reason.
func (t *involvedTally) add(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[reason]++
}

// omissions is the tally as a summary states it: one entry per reason, each
// counted at least once, sorted by reason; empty when no one was left out.
func (t *involvedTally) omissions() []shnsdk.InvolvedOmission {
	out := []shnsdk.InvolvedOmission{}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for reason, n := range t.counts {
		if n > 0 {
			out = append(out, shnsdk.InvolvedOmission{Reason: reason, Count: n})
		}
	}
	slices.SortFunc(out, func(a, b shnsdk.InvolvedOmission) int { return strings.Compare(a.Reason, b.Reason) })
	return out
}

// involvedRequestSide is the request's involved list (involvedForRequest) and
// this gateway's summary of it (involvedSummary), bound to legTok, the
// request's own token.
func (g *Gateway) involvedRequestSide(ctx context.Context, r *http.Request, leg, frame, op, corrID, payloadHash string, payload []byte, legSubject string, legTok shnsdk.Token) (list, summary string) {
	ctx, tally := withInvolvedTally(ctx)
	list = g.involvedForRequest(ctx, r, leg, frame, op, corrID, payloadHash, payload, legSubject)
	if request, _ := legInvolvement(leg); !request {
		return list, ""
	}
	return list, g.involvedSummary(ctx, shnsdk.InvolvedSideRequest, leg, corrID, legTok, list, tally)
}

// involvedAnswerSide is the answer's involved list (involvedForAnswer) and
// this gateway's summary of it (involvedSummary), bound to legTok, the
// answer's own token.
//
// The answer's list names the payer's own binding of the member, so its
// summary is sent only once that binding is settled or has failed
// (involvedCollector.bindingOutcome):
//   - settled, the binding reflected in the list (its patient named when it
//     differs from the token's, or none needed, or no member identified):
//     the summary states the list and the pass's omissions;
//   - failed: each member whose binding failed is stated left out too, under
//     its reason, never under a guessed identity;
//   - never attempted (the leg refused before it bound anyone), resolved but
//     refused before its patient entered the list, or a leg naming several
//     members that stopped before it bound them all: no summary, which the
//     Hub reads as not knowing whether the list was complete. For a Hub that
//     reads summaries, the exchange record says why
//     (InvolvedSummaryBindingUnsettled).
func (g *Gateway) involvedAnswerSide(r *http.Request, leg, frame, op, corrID, payloadHash, legSubject string, legTok shnsdk.Token) (list, summary string) {
	ctx, tally := withInvolvedTally(r.Context())
	list = g.involvedForAnswer(r.WithContext(ctx), leg, frame, op, corrID, payloadHash, legSubject)
	if _, answer := legInvolvement(leg); !answer {
		return list, ""
	}
	failed, settled := involvedCollectorFrom(r.Context()).bindingOutcome()
	if !settled {
		if g.readsInvolvedSummaries() {
			exchangeOf(r.Context()).involvedSummaryNotSent(InvolvedSummaryBindingUnsettled)
		}
		return list, ""
	}
	for reason, n := range failed {
		for range n {
			tally.add(reason)
		}
	}
	return list, g.involvedSummary(r.Context(), shnsdk.InvolvedSideAnswer, leg, corrID, legTok, list, tally)
}

// readsInvolvedSummaries reports whether the network's Hub reads this
// gateway's summaries: its discovery descriptor lists both involved and
// involved-summary.
func (g *Gateway) readsInvolvedSummaries() bool {
	return g.cfg.HubAcceptsInvolved && g.cfg.HubAcceptsInvolvedSummary
}

// InvolvedSummaryNotSentEvent is the observer event a leg raises when this
// gateway would have sent a summary of its involved list and did not, so that
// an operator can see why the Hub recorded the list's completeness as
// unknown. Detail is a fixed reason (InvolvedSummaryOverLimit,
// InvolvedSummaryUnsigned); it carries no identifier and nothing of the
// summary.
const InvolvedSummaryNotSentEvent = "involved.summary-not-sent"

// The reasons a summary of a list this gateway sends to a Hub that reads
// summaries is not sent: InvolvedSummaryNotSentEvent gives the first two, and
// ExchangeRecord.InvolvedSummaryNotSent any of them
// (InvolvedSummaryNotSentReasons).
const (
	// InvolvedSummaryOverLimit: the envelope with the summary was over its
	// size limit, so it was sent without it (withoutOverLimitSummary).
	InvolvedSummaryOverLimit = "involved-summary-over-limit"
	// InvolvedSummaryUnsigned: the summary could not be signed or encoded.
	InvolvedSummaryUnsigned = "involved-summary-unsigned"
	// InvolvedSummaryBindingUnsettled: an answer's summary was not made,
	// because the answering gateway's own binding of the member had not
	// settled (involvedAnswerSide): it never ran, the member names no one, it
	// resolved but the leg was refused before its patient entered the list, or
	// the request named several members and the leg stopped before binding
	// them all. The network's guarantee is unchanged: the Hub reads a missing
	// summary as not knowing whether the list was complete. Recorded only; no
	// observer event is raised for it.
	InvolvedSummaryBindingUnsettled = "involved-summary-binding-unsettled"
)

// InvolvedSummaryNotSentReasons is the closed list
// ExchangeRecord.InvolvedSummaryNotSent draws from.
var InvolvedSummaryNotSentReasons = []string{InvolvedSummaryOverLimit, InvolvedSummaryUnsigned, InvolvedSummaryBindingUnsettled}

// involvedSummary is the encoded summary for Metadata.InvolvedSummary of side's
// list (encoded as Metadata.Involved carries it) and tally, bound to legTok,
// or "" when the Hub does not read one or it cannot be made. One that cannot
// be made is recorded on ctx's exchange record (InvolvedSummaryUnsigned).
func (g *Gateway) involvedSummary(ctx context.Context, side, leg, corrID string, legTok shnsdk.Token, list string, tally *involvedTally) string {
	if !g.readsInvolvedSummaries() {
		return ""
	}
	listed := 0
	if list != "" {
		entries, err := shnsdk.DecodeInvolved(list)
		if err != nil {
			log.Printf("gateway: %s %s: no involved summary sent: its list does not decode (correlation %s)", leg, side, boundedID(corrID))
			g.observe(ObserverEvent{Kind: InvolvedSummaryNotSentEvent, LegType: leg, CorrelationID: corrID, Detail: InvolvedSummaryUnsigned})
			exchangeOf(ctx).involvedSummaryNotSent(InvolvedSummaryUnsigned)
			return ""
		}
		listed = len(entries)
	}
	sign := g.signInvolvedSummary
	if sign == nil {
		sign = signInvolvedSummary
	}
	encoded, err := sign(shnsdk.InvolvedSummary{
		Side:    side,
		Leg:     shnsdk.InvolvedSummaryLeg(legTok),
		Listed:  listed,
		Omitted: tally.omissions(),
	}, g.cfg.Identity.SignPriv, legTok)
	if err != nil {
		log.Printf("gateway: %s %s: no involved summary sent: %v (correlation %s)", leg, side, err, boundedID(corrID))
		g.observe(ObserverEvent{Kind: InvolvedSummaryNotSentEvent, LegType: leg, CorrelationID: corrID, Detail: InvolvedSummaryUnsigned})
		exchangeOf(ctx).involvedSummaryNotSent(InvolvedSummaryUnsigned)
		return ""
	}
	return encoded
}

// errInvolvedSummaryUnsigned is a summary that cannot be signed: no usable
// signing key, or no token signature to bind it to.
var errInvolvedSummaryUnsigned = errors.New("it cannot be signed")

// signInvolvedSummary signs s with priv, this holder's signing key, and
// encodes it, or fails when priv is not a signing key or legTok carries no
// signature to bind s to.
func signInvolvedSummary(s shnsdk.InvolvedSummary, priv ed25519.PrivateKey, legTok shnsdk.Token) (string, error) {
	if len(priv) != ed25519.PrivateKeySize || len(legTok.Signature) == 0 {
		return "", errInvolvedSummaryUnsigned
	}
	return shnsdk.EncodeInvolvedSummary(shnsdk.SignInvolvedSummary(s, priv))
}

// withoutOverLimitSummary re-encodes env without its involved summary when
// encoded, env as encoded with it, is over limit: a summary never costs a leg
// its envelope. When dropping it is what brings the envelope within the
// limit, the drop is logged, raised as InvolvedSummaryNotSentEvent
// (InvolvedSummaryOverLimit) and recorded on ctx's exchange record; an
// envelope still over the limit without it is refused as too large by its
// caller, as it was before summaries, and the summary is not why. It returns
// encoded unchanged otherwise, or when env carries no summary.
func (g *Gateway) withoutOverLimitSummary(ctx context.Context, env *shnsdk.Envelope, encoded []byte, limit int) ([]byte, error) {
	if len(encoded) <= limit || env.Metadata.InvolvedSummary == "" {
		return encoded, nil
	}
	env.Metadata.InvolvedSummary = ""
	out, err := shnsdk.EncodeEnvelope(*env)
	if err != nil || len(out) > limit {
		return out, err
	}
	log.Printf("gateway: %s: no involved summary sent: %s (correlation %s)", env.Metadata.TransactionType, InvolvedSummaryOverLimit, boundedID(env.Metadata.CorrelationID))
	g.observe(ObserverEvent{Kind: InvolvedSummaryNotSentEvent, LegType: env.Metadata.TransactionType, CorrelationID: env.Metadata.CorrelationID, Detail: InvolvedSummaryOverLimit})
	exchangeOf(ctx).involvedSummaryNotSent(InvolvedSummaryOverLimit)
	return out, nil
}
