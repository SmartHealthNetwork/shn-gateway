package engine

// linecarry.go — the network's default across IG lines.
//
// By default a gateway carries every message as sent, whatever the two
// participants' IG lines, as if they were integrated directly: a request to a
// peer that shares no declared line, a request whose frame claims a line this
// gateway cannot answer at, and an answer stamped with another line than the
// leg routed. It records each (LineCarriedEvent and ExchangeRecord.LineCarried)
// and translates nothing it cannot certify. The only refusal on IG lines is the
// participant's own opt-in, Config.RequireSharedLine, which refuses such a pair
// as routing.

import (
	"context"
	"log"
	"regexp"
	"slices"
	"strings"
)

// LineCarriedEvent is the observer event a gateway raises when it carries a
// leg between two IG lines with nothing translated: the pair shares no
// declared line, or the answer is stamped with another line than the leg
// routed. Detail names the leg's lines; the event carries no content.
const LineCarriedEvent = "contract.line-carried"

// Where a leg was carried between two lines (ExchangeRecord.LineCarried).
const (
	// LineCarriedRequest: the request went to, or came from, a participant
	// that shares no declared line with this one, or, a pend's follow-up, was
	// carried at another line than the pend is pinned at.
	LineCarriedRequest = "request"
	// LineCarriedAnswer: the answer is stamped with another line than the leg
	// routed.
	LineCarriedAnswer = "answer"
)

// LineCarriedValues is the closed list ExchangeRecord.LineCarried draws from.
var LineCarriedValues = []string{LineCarriedRequest, LineCarriedAnswer}

// lineObserverKey carries the gateway's observer to a responder's code on
// an inbound leg, so a responder that carries across lines raises the same
// event as the gateway does.
type lineObserverKey struct{}

func withLineObserver(ctx context.Context, observe func(ObserverEvent)) context.Context {
	return context.WithValue(ctx, lineObserverKey{}, observe)
}

// noteLineCarried records that the leg was carried between two lines (where:
// LineCarriedRequest or LineCarriedAnswer) on the exchange record, and raises
// LineCarriedEvent through the observer the context carries.
func noteLineCarried(ctx context.Context, where string, e ObserverEvent) {
	exchangeOf(ctx).lineCarried(where)
	e.Kind = LineCarriedEvent
	log.Printf("gateway: %s: leg %s correlation %s: %s", LineCarriedEvent, e.LegType, e.CorrelationID, e.Detail)
	if observe, ok := ctx.Value(lineObserverKey{}).(func(ObserverEvent)); ok && observe != nil {
		observe(e)
	}
}

// noteLineCarried is noteLineCarried on a path where the gateway raises the
// event itself.
func (g *Gateway) noteLineCarried(ctx context.Context, where string, e ObserverEvent) {
	noteLineCarried(withLineObserver(ctx, g.observe), where, e)
}

func (x *exchangeRecorder) lineCarried(where string) {
	x.with(func() {
		if !slices.Contains(x.rec.LineCarried, where) {
			x.rec.LineCarried = append(x.rec.LineCarried, where)
		}
	})
}

// carriedRoute is the route a leg takes to a peer that shares no declared
// line when nothing reaches the peer's line: this gateway's own highest
// declared line, the line the message is built or written at, with no chain.
// ok is false when this build declares no line for the contract at all.
func carriedRoute(own []string, contract string) (legRoute, bool) {
	lines := contractLineSet(own, contract)
	if len(lines) == 0 {
		return legRoute{}, false
	}
	line := highestLine(lines)
	return legRoute{Token: contract + "@" + line, BuildLine: line}, true
}

// asSent is the route that carries a message exactly as it was built: at its
// build line, with no chain. It is the fallback when a walk to the peer's
// line would change bytes no certified translation can seal.
func (r legRoute) asSent() legRoute {
	contract, _, _ := strings.Cut(r.Token, "@")
	return legRoute{Token: contract + "@" + r.BuildLine, BuildLine: r.BuildLine, Pinned: r.Pinned}
}

// carriedDetail is the event's Detail for a request carried at token to a
// recipient declaring peer.
func carriedDetail(token, recipient string, peer []string) string {
	return "carried at " + token + "; recipient " + recipient + " declares " + declaredOrNone(peer)
}

// declaredOrNone renders a declared token list for an event's Detail.
func declaredOrNone(tokens []string) string {
	if len(tokens) == 0 {
		return "no line for the contract"
	}
	return strings.Join(tokens, ",")
}

// noteRequestLine records a request routed at a line its recipient does not
// declare: the pair shares no declared line, so it is carried at this
// gateway's own line. A recipient that declares nothing, or a version-neutral
// leg, is not a carry across lines.
func (g *Gateway) noteRequestLine(ctx context.Context, recipient, legType, correlationID, token string) {
	contract, line, ok := strings.Cut(token, "@")
	if !ok || line == "" {
		return
	}
	entry, found := g.cfg.Reg.Lookup(recipient)
	if !found || len(entry.ContractVersions) == 0 {
		return
	}
	peer := contractLineSet(entry.ContractVersions, contract)
	if peer[line] {
		return
	}
	g.noteLineCarried(ctx, LineCarriedRequest, ObserverEvent{
		Direction: "originate", LegType: legType, CorrelationID: correlationID, Counterpart: recipient,
		Detail: carriedDetail(token, recipient, sortedTokens(contract, peer)),
	})
}

// notePinCarried records a follow-up to a pend (an amendment or an inquiry)
// carried at token instead of the line the pend is pinned at, which nothing
// reaches any more (selectResumeRoute).
func (g *Gateway) notePinCarried(ctx context.Context, recipient, legType, correlationID, pinned, token string) {
	g.noteLineCarried(ctx, LineCarriedRequest, ObserverEvent{
		Direction: "originate", LegType: legType, CorrelationID: correlationID, Counterpart: recipient,
		Detail: "carried at " + token + "; the pend it follows up is pinned at " + pinned + ", which no lane or chain reaches",
	})
}

// lineTokenShape is a contract-version token as a frame carries one.
var lineTokenShape = regexp.MustCompile(`^[a-z][a-z0-9.]{0,31}@[0-9][0-9.]{0,15}$`)

// boundedLine is how an event or log line names a contract-version token a
// peer's frame stated: as it is when it has a token's shape, else as "sha256:"
// and its digest, so a peer cannot write arbitrary text into this gateway's
// records.
func boundedLine(token string) string {
	if lineTokenShape.MatchString(token) {
		return token
	}
	return "sha256:" + sha256hex([]byte(token))
}

// answeredAt names the line a carried request is answered at, for an event's
// Detail.
func answeredAt(token string) string {
	if token == "" {
		return "this gateway's own line"
	}
	return token
}
