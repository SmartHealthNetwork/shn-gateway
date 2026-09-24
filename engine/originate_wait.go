// originate_wait.go — what the provider gateway's OWN originator flows do when a
// payer pends a prior authorization.
//
// The payer's answer is the payer's answer. A pend is one of them, not a failure,
// and nothing here polls for a later one: a later decision comes only from an
// explicit `Claim/$inquire`, which this gateway performs on behalf of the
// participant whose workflow it is running.
//
// THE WAIT IS AN APPLICATION OPERATION, NOT A TRANSPORT RETRY. A caller that
// wants an answer within its own request may ask this gateway to follow the
// decision for a bounded time. Reaching that bound is NEVER an error: the result
// is "pended, and here is the continuation", which the caller resumes later with
// POST /scenario/pa/inquire. Prior Authorization asks clients not to inquire
// repetitively, and every inquiry is an audited exchange, so the wait is bounded
// three ways at once — a deadline, a delay schedule, and a hard cap on how many
// inquiries it may make.
//
// HERMETIC TIMING. The schedule lives in the two package-level variables below so
// a test can set it to milliseconds, exactly as the update leg's re-issue delay is
// set (nativepas_conflict.go). The DEADLINE comes from cfg.Clock, which is
// injected, so "the wait bound was reached" is a decision about the gateway's own
// clock rather than about how long a test happened to run. Sleeps use a timer
// together with the request context, so a cancelled request stops waiting at once.
// A hermetic row that sleeps on wall-clock seconds is a defect here, not a slow
// test.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The inquiry schedule. These are VARIABLES so a hermetic row can compress them
// to milliseconds; nothing outside a test assigns them.
//
// pasInquireFirstDelay leaves the payer a moment to do the work it pended for
// before the first follow-up. pasInquireBackoffCap bounds the exponential
// backoff, so a long wait is a few evenly spaced inquiries rather than one at the
// start and one at the end.
var (
	pasInquireFirstDelay = 2 * time.Second
	pasInquireBackoffCap = 5 * time.Second
)

// CompressPASInquireScheduleForTest shortens the inquiry schedule to first/cap.
//
// It exists because a harness in ANOTHER package drives whole scenarios through a
// real gateway against an in-process payer that decides instantly. Left at the
// shipped seconds, every such row would spend real wall-clock time asleep —
// measuring the machine instead of the rule, which this repo treats as a defect
// rather than a slow test. In-package rows compress the same two variables
// directly (fastPASInquire).
//
// It is a TEST AFFORDANCE, in the same family as EnableIngressForTest: build() and
// main MUST never call it, nothing reads it from the environment, and it changes
// only how long a wait sleeps — never how many inquiries it may make, never the
// bound, and never what any answer says. Call it before any gateway starts
// serving; the variables it writes are read by every in-flight wait.
func CompressPASInquireScheduleForTest(first, cap time.Duration) {
	pasInquireFirstDelay, pasInquireBackoffCap = first, cap
}

const (
	// PASWaitDefault is what an originator route waits when the caller asks for
	// nothing in particular: NOTHING. The payer's answer to the submit is the
	// answer, and a pend is one of them. Waiting is the caller's patience, and
	// a caller that has some says how much with ?wait=<seconds>.
	//
	// It used to be 30 seconds, which was wrong three ways at once: it turned a
	// payer that answers in a second into a thirty-second spin, it made a pend
	// look like a slow approval to anyone reading the clock, and — at PAS 2.2.1,
	// where the IG says an inquiry SHOULD NOT be used while waiting for final
	// results — it inquired on behalf of a caller that never asked to.
	PASWaitDefault = 0 * time.Second
	// PASWaitMax is the longest wait any caller may ask for, and it is set to
	// what the schedule below can actually deliver: the sixth and last inquiry
	// fires at pasInquireScheduleReach() (26s with the shipped schedule), so a
	// longer bound is a connection held open with no inquiry left to make.
	// TestPASWait_TheBoundIsReachable holds the two together.
	//
	// A caller that wants to hear about a decision hours from now is not asking
	// for a longer wait; it is asking to continue later, which is what the
	// continuation and POST /scenario/pa/inquire are for.
	PASWaitMax = 30 * time.Second
	// pasInquireMax is the hard cap on inquiries per wait. Prior Authorization
	// asks clients to avoid repetitive inquiries, and every inquiry is an
	// audited exchange on somebody else's system, so the cap holds even when the
	// schedule and the deadline would allow more.
	pasInquireMax = 6
)

// pasInquireScheduleReach is the instant the LAST inquiry a wait may make falls
// due, measured from the pend: the sum of the delays the schedule hands out
// before each of its pasInquireMax inquiries.
//
// It exists so the bound and the schedule are one fact rather than two numbers
// that agreed once. A wait longer than this reach makes no further inquiry — it
// only holds the caller's connection — and a hermetic row passing the maximum
// cannot tell the difference, which is how a 120-second maximum sat above a
// 26-second schedule without anything going red.
func pasInquireScheduleReach() time.Duration {
	total, delay := time.Duration(0), pasInquireFirstDelay
	for i := 0; i < pasInquireMax; i++ {
		total += delay
		delay = min(2*delay, pasInquireBackoffCap)
	}
	return total
}

// The decisions an originator flow reports. "pended" is a real, final ANSWER for
// the request that produced it — the payer has not decided yet — not an error and
// not an absence.
const (
	PASDecisionApproved = "approved"
	PASDecisionDenied   = "denied"
	PASDecisionPended   = "pended"
)

// ErrOrderChanged: the order in the participant's own system is no longer the one
// that was submitted. The inquiry is not sent: an inquiry about lines the payer
// never received would ask the wrong question, and silently following the new
// order would submit nothing while reporting on something else.
var ErrOrderChanged = errors.New("order changed since submission")

// PASDecision is the result of the wait operation: what the payer said, the
// payer's own bytes for saying it, and the capability that continues it.
//
// It is the SAME shape whether the payer approved, denied or pended, and whether
// the wait ran to a decision or reached its bound. A caller therefore never has
// to tell "no decision yet" from "something went wrong" by reading a status code.
type PASDecision struct {
	// Decision is approved, denied or pended.
	Decision string
	// Parsed is the payer's determination as the shared parser read it.
	Parsed shnsdk.PriorAuthResult
	// PayerResponse is the payer's own bytes for the answer that produced this
	// decision — the submit's answer, or the inquiry's.
	PayerResponse []byte
	// Continuation is the capability that continues this authorization, and
	// ContinuationDurable says whether this deployment keeps it across a
	// restart. Both are empty/false when the payer decided on the submit and
	// there was nothing to continue.
	Continuation        string
	ContinuationDurable bool
	// Inquiries is how many inquiries the wait made. Zero means the payer's
	// first answer was the decision.
	Inquiries int
}

// classifyResolution classifies a PAS ClaimResponse at a resolution site as the
// payer's own determination: approved, denied or pended.
//
// It reads a pend FIRST, because the shared decision parser refuses a pended
// response rather than describing it — a pend is not a decision, and this
// function's job is to say which of the three the payer gave. An answer it cannot
// read at all yields "" and an unset result, which every caller treats as a
// failure to read the payer, never as a verdict.
func (g *Gateway) classifyResolution(respJSON []byte) (parsed shnsdk.PriorAuthResult, decision string) {
	if pended, needed, err := shnsdk.ParsePendedResponse(respJSON); err == nil && pended {
		return shnsdk.PriorAuthResult{Outcome: PASDecisionPended, NeededItems: needed}, PASDecisionPended
	}
	p, err := shnsdk.ParseClaimResponse(respJSON)
	if err != nil {
		return shnsdk.PriorAuthResult{}, ""
	}
	switch p.Outcome {
	case PASDecisionApproved, PASDecisionDenied:
		return p, p.Outcome
	}
	return shnsdk.PriorAuthResult{}, ""
}

// pasFollowInputs are the facts one originator submission needs, and the facts a
// continuation is made of. They travel as a struct because the continuation needs
// several of them AFTER the submit, and threading them back out of a positional
// return is how one of them quietly becomes the wrong value.
type pasFollowInputs struct {
	pci         string
	patientRef  string
	coverageRef string
	// coverage is the member's own Coverage record, read once at the origination
	// site: the PAS request carries it as the resolvable entry the Claim names.
	coverage []byte
	// insurer is the payer's own Organization record — see crdDtrResult.insurer.
	insurer      []byte
	member       string
	recipient    string
	orderRef     string
	orderJSON    []byte
	supplierJSON []byte
	// memberSystem is the namespace the participant's own system names this
	// member under, carried from the one reading of their Patient.
	memberSystem string
	source       *dtrBuildSource
	payer        shnsdk.PayerIdentifier
	// wait is how long the caller asked this gateway to follow the decision. Zero
	// is a perfectly good answer to "follow it for how long?": submit, report
	// what the payer said, and hand back the continuation.
	wait time.Duration
}

// submitClaimAndFollow is the originator's prior-authorization operation: submit,
// and then follow the decision for as long as the caller asked.
//
// It NEVER reports a payer determination as an error. The (status, msg, err)
// return is for the things that genuinely went wrong — a route this gateway
// cannot take, a bundle it could not build, a leg that failed, a 2xx it cannot
// read as a prior-authorization answer at all. An approval, a denial and a pend
// all come back as a PASDecision with status 0.
func (g *Gateway) submitClaimAndFollow(ctx context.Context, r *http.Request, in pasFollowInputs) (PASDecision, int, string, error) {
	sub, status, msg, err := g.submitPASClaim(ctx, r, in.pci, in.orderJSON, in.supplierJSON, in.source, in.coverage, in.insurer, in.patientRef, in.coverageRef, in.member, in.memberSystem, in.payer, in.recipient)
	if status != 0 {
		return PASDecision{}, status, msg, err
	}
	return g.followDecision(ctx, r, sub, in)
}

// followDecision classifies ONE payer answer — a submission's or an amendment's
// — and, when it is a pend, records the continuation and follows the decision for
// the rest of the caller's wait.
//
// It is shared by the single-shot tail and by the amendment sites deliberately:
// an amendment's re-pend is the same fact as a submission's pend, and a second
// implementation of "what do we do about a pend" is how the two would come to
// disagree.
func (g *Gateway) followDecision(ctx context.Context, r *http.Request, sub pasSubmission, in pasFollowInputs) (PASDecision, int, string, error) {
	parsed, decision := g.classifyResolution(sub.respJSON)
	if decision == "" {
		return PASDecision{PayerResponse: sub.respJSON}, http.StatusBadGateway, "claim response parse failed", nil
	}
	out := PASDecision{Decision: decision, Parsed: parsed, PayerResponse: sub.respJSON}
	if decision != PASDecisionPended {
		return out, 0, "", nil
	}
	cont, status, msg := g.recordContinuation(ctx, in, sub)
	if status != 0 {
		return out, status, msg, nil
	}
	out.Continuation, out.ContinuationDurable = cont.ID, g.continuationsAreDurable()
	return g.followPended(ctx, out, in.wait, g.inquiryFor(r, cont))
}

// inquiryFor binds one continuation's inquiry so the wait can run it without
// knowing anything about legs, routes or requests. The wait's own rules — the
// deadline, the schedule, the cap and the cancellation — are then testable on
// their own terms, which is the only way a row about them can be hermetic.
func (g *Gateway) inquiryFor(r *http.Request, cont Continuation) pasInquiry {
	return func(ctx context.Context) (PASDecision, int, string, error) {
		return g.inquireContinuation(ctx, r, cont)
	}
}

// pasInquiry is one follow-up ask: it returns the payer's determination, or the
// reason this gateway could not obtain one.
type pasInquiry func(ctx context.Context) (PASDecision, int, string, error)

// pasContractToken is the prior-authorization contract token for a line — the
// pin a resume leg selects against. The continuation stores the LINE, because
// that is the routing fact it is about; the contract is this leg's own and is
// never anything else.
func pasContractToken(line string) string { return "pa.pas@" + line }

// recordContinuation writes the continuation for a pended answer. Its facts come
// from the bytes SHN SENT and the bytes the payer ANSWERED with, read by the
// shared continuation reader — the same one a participant's own system uses for
// its own records. There is one reading of "what was submitted" in this system,
// not one per caller.
//
// The patient reference it stores is the one the PARTICIPANT'S OWN system knows,
// re-read here rather than carried down from the scenario: the inquiry re-reads
// the patient by that reference, and a reference that only resolves inside this
// gateway's own vocabulary would resolve to nothing months later.
func (g *Gateway) recordContinuation(ctx context.Context, in pasFollowInputs, sub pasSubmission) (Continuation, int, string) {
	store, ok := g.continuations()
	if !ok {
		// Unreachable in every shipped wiring (the in-memory Store ships one),
		// and still not something to paper over: without a continuation there is
		// nothing to continue, and saying so is better than answering "pended"
		// with a capability that names nothing.
		return Continuation{}, http.StatusInternalServerError, "this gateway keeps no prior-authorization continuations"
	}
	facts, err := shnsdk.NewPriorAuthContinuation(shnsdk.LineOf(sub.route.Token), in.recipient, in.member,
		sub.bundleJSON, sub.respJSON)
	if err != nil {
		return Continuation{}, http.StatusBadGateway, "read the continuation facts of the submitted request: " + err.Error()
	}
	patientRef, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, in.member)
	if err != nil {
		return Continuation{}, http.StatusBadGateway, "read the patient reference from the system of record: " + err.Error()
	}
	if !found {
		patientRef = in.patientRef
	}
	cont := ContinuationFacts(Continuation{
		Holder:       g.cfg.HolderID,
		CorrID:       sub.corr,
		SubjectPCI:   in.pci,
		SoRPatientID: patientRef,
		OrderRef:     in.orderRef,
		LastOutcome:  ContinuationOutcomePended,
	}, facts)
	// The item map pins the DATE OF SERVICE as well as the product, because that
	// is what makes a rescheduled order detectable. A submitted Claim that stated
	// one keeps it; one that stated none takes the date from the order it was
	// built from, which is where a service date comes from in the first place.
	// Without this the map holds no date, and the order-changed check would have
	// nothing to compare a reschedule against.
	orderDate := orderServiceDate(in.orderJSON)
	for i := range cont.Items {
		if cont.Items[i].ServiceDate == "" {
			cont.Items[i].ServiceDate = orderDate
		}
	}
	stored, err := store.PutContinuation(cont)
	if err != nil {
		return Continuation{}, http.StatusBadGateway, "record the prior-authorization continuation: " + err.Error()
	}
	return stored, 0, ""
}

// followPended runs the bounded wait: at most pasInquireMax inquiries, none of
// them after the deadline, each after the scheduled delay, and every one of them
// abandoned the moment the request context is done.
//
// Reaching the bound returns the pended decision it started with. That is the
// answer, not a timeout.
func (g *Gateway) followPended(ctx context.Context, pend PASDecision, wait time.Duration, inquire pasInquiry) (PASDecision, int, string, error) {
	deadline := g.cfg.Clock().Add(clampPASWait(wait))
	delay := pasInquireFirstDelay
	for pend.Inquiries < pasInquireMax {
		// The deadline is checked BEFORE the delay as well as after it, so a
		// wait of zero makes no inquiry at all rather than one.
		if !g.cfg.Clock().Before(deadline) {
			return pend, 0, "", nil
		}
		if err := sleepCtx(ctx, delay); err != nil {
			// The caller went away. Its continuation is recorded, so nothing is
			// lost; there is simply nobody to answer.
			return pend, 0, "", nil
		}
		if !g.cfg.Clock().Before(deadline) {
			return pend, 0, "", nil
		}
		next, status, msg, _ := inquire(ctx)
		pend.Inquiries++
		if status != 0 {
			// An inquiry that could not be made does not erase the answer the
			// payer already gave. The pend, and its continuation, stand — and
			// the failure is reported rather than reshaped into a decision.
			g.observe(ObserverEvent{Kind: "pa.inquiry-failed", Direction: "egress", LegType: "pas-claim-inquire",
				CorrelationID: pend.Continuation, Op: "pas-inquire", Detail: msg})
			return pend, 0, "", nil
		}
		next.Continuation, next.ContinuationDurable, next.Inquiries = pend.Continuation, pend.ContinuationDurable, pend.Inquiries
		if next.Decision != PASDecisionPended {
			return next, 0, "", nil
		}
		pend = next
		delay = min(2*delay, pasInquireBackoffCap)
	}
	return pend, 0, "", nil
}

// clampPASWait holds a caller's wait inside the operation's bounds. A negative
// wait is no wait; anything beyond the maximum is the maximum.
func clampPASWait(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > PASWaitMax:
		return PASWaitMax
	}
	return d
}

// pasWaitOf reads the wait a request asked for from the `wait` query parameter,
// in seconds, defaulting to PASWaitDefault when the caller asked for nothing.
// "0" is an explicit "do not wait" and is honoured; a value that is not a number
// is refused rather than silently read as the default, because a caller that
// mistyped its bound must not be given a different one without being told.
func pasWaitOf(r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return PASWaitDefault, true
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return 0, false
	}
	return clampPASWait(time.Duration(secs) * time.Second), true
}

// continuations returns this gateway's continuation store: the configured Store's
// own, when it has one, and otherwise the in-memory default, built on first use.
//
// It never returns "none": a gateway that could originate a prior authorization
// but could not keep the continuation for it would answer "pended" with nothing
// to continue. The second return value is kept so the call sites read as the
// assertion they are, and so a Store that ever loses the seam fails loudly.
func (g *Gateway) continuations() (ContinuationStore, bool) {
	if cs, ok := ContinuationsOf(g.cfg.Store); ok {
		return cs, true
	}
	if cs := g.fallbackContinuations.Load(); cs != nil {
		return cs, true
	}
	fresh := NewMemContinuations()
	if !g.fallbackContinuations.CompareAndSwap(nil, fresh) {
		// Another request built it first. Its store is the one every later
		// reader will see, so this one's ids must never be handed out.
		return g.fallbackContinuations.Load(), true
	}
	return fresh, true
}

// continuationsAreDurable reports whether a continuation this gateway mints
// survives a restart. The Kit UI's notice and CONFIGURATION read this rather than
// inferring it from how the gateway happens to be wired.
func (g *Gateway) continuationsAreDurable() bool {
	cs, ok := g.continuations()
	return ok && DurableContinuations(cs)
}

// --- one inquiry ---

// inquireContinuation builds and sends ONE inquiry for a continuation and reads
// the payer's answer. It is the single place the originator asks a payer for a
// later decision, shared by the bounded wait and by the route that continues one
// later, so the two can never ask different questions.
func (g *Gateway) inquireContinuation(ctx context.Context, r *http.Request, cont Continuation) (PASDecision, int, string, error) {
	records, status, msg := g.inquiryRecords(ctx, cont)
	if status != 0 {
		return PASDecision{}, status, msg, nil
	}
	corr := g.cfg.CorrelationGen()
	// The inquiry runs at the line the SUBMISSION ran at, pinned on the
	// continuation and never re-negotiated. An inquiry is a follow-up about that
	// authorization, so asking about it at a line it was never sent at would ask
	// a different question — the same reason an amendment answers the pend it
	// references on the pend's own line.
	route, terr := g.selectResumeRoute(pasContractToken(cont.Line), cont.PayerHolder, "pas-claim-inquire")
	if terr != nil {
		return PASDecision{}, http.StatusBadGateway, terr.Error(), terr
	}
	body, sealed, err := g.buildPASInquiry(route.BuildLine, cont, records, corr)
	if err != nil {
		return PASDecision{}, http.StatusBadGateway, "build the prior-authorization inquiry: " + err.Error(), err
	}
	adapted, _, aerr := g.egressAdapt(ctx, route, body, ExchangeIdentity{CorrelationID: corr, LegType: "pas-claim-inquire", Counterpart: cont.PayerHolder})
	if aerr != nil {
		return PASDecision{}, http.StatusBadGateway, aerr.Error(), aerr
	}
	if !bytes.Equal(adapted, body) {
		// The inquiry this gateway authored cannot be carried to the payer's
		// line unchanged. The sealed payload is the bytes the builder produced,
		// so sending it after the chain changed them would transmit one message
		// under a seal that describes another. A byte comparison, not a length
		// one: a chain that swapped a value for another of the same width is the
		// case a length check would wave through.
		return PASDecision{}, http.StatusBadGateway, "the prior-authorization inquiry cannot be carried to the payer's line unchanged", nil
	}
	// An inquiry is certified against the INQUIRY profiles, never the submit
	// ones. Its Claim is profile-claim-inquiry, which states none of the
	// line-detail a submitted Claim.item must carry from 2.1 on — validating it
	// as a submission would report it invalid for a profile it was never meant
	// to meet, which is the same distinction the certification layer already
	// draws (species.go). Naming the profile also stops the validator guessing
	// from the bytes.
	inquiryProfile, _ := profileFor("PASRequestBundle", shnsdk.LineOf(route.Token), "pas-claim-inquire")
	if status, msg := g.validateFHIRForContract(ctx, body, "egress", "pa.pas", shnsdk.LineOf(route.Token), inquiryProfile); status != 0 {
		return PASDecision{}, status, msg, nil
	}
	answer, err := g.OriginateLeg(ctx, r, cont.PayerHolder, "pas-claim-inquire", cont.SubjectPCI, corr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, DeclaredVersion: route.Token, Route: routeInfoFor(route), Payload: sealed})
	if err != nil {
		return PASDecision{}, http.StatusBadGateway, err.Error(), err
	}
	if bad := validatePASInquiryAnswer(answer); bad.Status != 0 {
		return PASDecision{}, bad.Status, bad.Message, nil
	}
	decision, parsed, status, msg := decideFromInquiryAnswer(cont, answer)
	if status != 0 {
		return PASDecision{PayerResponse: answer}, status, msg, nil
	}
	out := PASDecision{Decision: decision, Parsed: parsed, PayerResponse: answer}
	if store, ok := g.continuations(); ok {
		// The continuation records what the payer has said SINCE, so a later
		// inquiry about a decided authorization resolves to the decision rather
		// than asking again. It also keeps the payer's own new identifiers, so a
		// second inquiry can name the authorization by everything it has.
		next := ContinuationFacts(cont, recordedInquiryFacts(cont, answer))
		next.LastOutcome = decision
		if _, err := store.PutContinuation(next); err != nil {
			// The payer's answer is not lost because the record of it was not
			// written: the decision is returned either way, and the failure is
			// reported rather than swallowed.
			g.observe(ObserverEvent{Kind: "pa.continuation-write-failed", Direction: "egress",
				LegType: "pas-claim-inquire", CorrelationID: corr, Op: "pas-inquire", Detail: err.Error()})
		}
	}
	return out, 0, "", nil
}

// recordedInquiryFacts folds an answer's payer-stated identifiers into the
// continuation's facts, through the same reader the requester-retained half uses.
// An answer it cannot read adds nothing and loses nothing.
func recordedInquiryFacts(cont Continuation, answer []byte) shnsdk.PriorAuthContinuation {
	facts := cont.SDKContinuation()
	_ = facts.Record(answer)
	return facts
}

// decideFromInquiryAnswer reads the payer's answer for THIS authorization. The
// match is by the continuation's own keys — item trace numbers, the payer's
// identifiers, the authorization reference — so an answer carrying several
// authorizations decides the right one, and an answer carrying none says so.
//
// Zero matches and more than one match are REPORTED, never resolved by taking the
// first: an inquiry that matched nothing has not been answered, and one that
// matched twice has not been answered unambiguously.
func decideFromInquiryAnswer(cont Continuation, answer []byte) (string, shnsdk.PriorAuthResult, int, string) {
	result, err := shnsdk.InquiryDecision(answer, cont.SDKContinuation())
	if err != nil {
		return "", shnsdk.PriorAuthResult{}, http.StatusBadGateway, "read the prior-authorization inquiry answer: " + err.Error()
	}
	switch result.Outcome {
	case PASDecisionApproved, PASDecisionDenied, PASDecisionPended:
		return result.Outcome, result, 0, ""
	}
	return "", shnsdk.PriorAuthResult{}, http.StatusBadGateway,
		fmt.Sprintf("the prior-authorization inquiry answer reports %q, which is not a determination", result.Outcome)
}

// --- the inquiry the originator authors ---

// buildPASInquiry builds the inquiry request Bundle for a continuation at a line,
// through the SHARED builder a participant's own system uses. Each of the
// participant's records travels as its own bytes, and every one of them is
// declared as an embed, so the bytes on the wire are provably the participant's
// and not a rendering of them.
//
// The party this inquiry names is the party the SUBMISSION named, because both
// come from one rule (shnsdk.SelectPASProvider) over the same order. Prior
// Authorization says a payer SHALL match an inquiry on the member or subscriber
// id PLUS the ordering and/or rendering provider identifier, so the two halves
// have to agree: a submission that identified nobody — which is what a
// Claim.provider carrying display text alone does, at every line — leaves the
// payer nothing to match, and a submission and an inquiry naming DIFFERENT real
// parties fail the same match one step later.
func (g *Gateway) buildPASInquiry(line string, cont Continuation, records shnsdk.PASInquiryRecords, corr string) ([]byte, relay.Payload, error) {
	var sealed relay.Payload
	inq, err := shnsdk.BuildPASInquiryBundle(line, cont.SDKContinuation().InquiryInputs(
		"inquiry-"+corr,
		shnsdk.PASIdentifier{System: shnsdk.PASInquiryIdentifierSystem, Value: corr},
		records, g.cfg.Clock()))
	if err != nil {
		return nil, sealed, err
	}
	embeds := make([]relay.Embed, 0, len(inq.Copied))
	for _, c := range inq.Copied {
		src, ok := inquiryRecordBody(records, c.Input)
		if !ok {
			return nil, sealed, fmt.Errorf("the inquiry copies an unknown input %q", c.Input)
		}
		embeds = append(embeds, relay.Embed{Source: src, Start: c.Start, End: c.End, At: c.At})
	}
	sealed, err = relay.Authored(relay.BuilderSDKPASInquiry, inq.Body, "application/fhir+json", embeds...)
	if err != nil {
		return nil, sealed, err
	}
	return inq.Body, sealed, nil
}

// inquiryRecordBody names the participant record an embed copied from. The set is
// CLOSED: a builder that started copying a fifth input would be refused here
// rather than sending an unverified span.
func inquiryRecordBody(records shnsdk.PASInquiryRecords, input string) (relay.Body, bool) {
	switch input {
	case "Patient":
		return relay.NewBody(records.Patient, relay.OriginUpstreamResponse), true
	case "Coverage":
		return relay.NewBody(records.Coverage, relay.OriginUpstreamResponse), true
	case "Provider":
		return relay.NewBody(records.Provider, relay.OriginUpstreamResponse), true
	case "Insurer":
		return relay.NewBody(records.Insurer, relay.OriginUpstreamResponse), true
	}
	return relay.Body{}, false
}

// inquiryRecords reads the four records an inquiry carries from the
// PARTICIPANT'S OWN system of record, and runs the order-changed check on the way.
//
// Nothing here is minted. A record the participant's system cannot supply is a
// refusal that NAMES the record, because an inquiry missing the patient, the
// coverage or the provider is one the payer is required to refuse anyway, and
// saying "the inquiry failed" would leave an operator guessing which of the four
// it was.
func (g *Gateway) inquiryRecords(ctx context.Context, cont Continuation) (shnsdk.PASInquiryRecords, int, string) {
	sor := ReadSystemOfRecord(g.cfg.SoR)
	var out shnsdk.PASInquiryRecords

	// The order first: it is the one record that is NOT sent, and re-reading it
	// is how a changed order is detected rather than followed.
	order, found, err := sor.ResolveByReferenceContext(ctx, cont.OrderRef)
	if err != nil {
		return out, http.StatusBadGateway, "read the order from the system of record: " + err.Error()
	}
	if !found {
		return out, http.StatusConflict, ErrOrderChanged.Error() + ": the order is no longer in the system of record"
	}
	if changed, detail := pasOrderChanged(order, cont); changed {
		return out, http.StatusConflict, ErrOrderChanged.Error() + ": " + detail
	}

	if out.Patient, found, err = sor.ResolveByReferenceContext(ctx, cont.SoRPatientID); err != nil {
		return out, http.StatusBadGateway, "read the patient from the system of record: " + err.Error()
	} else if !found {
		return out, http.StatusBadGateway, "the system of record has no patient " + cont.SoRPatientID
	}

	// Through memberCoverage, the one reader of a member's Coverage: a member whose
	// records name two payers is refused there rather than routed on whichever the
	// system of record happened to list first.
	coverage, hasCoverage, status, msg := g.memberCoverage(ctx, cont.MemberID)
	if status != 0 {
		return out, status, msg
	}
	if !hasCoverage {
		return out, http.StatusBadGateway, "the system of record has no coverage for member " + cont.MemberID
	}
	out.Coverage = coverage

	// The participant's system may name this patient by its OWN record id rather
	// than by the member id. Every other originated leg re-keys that one binding
	// at the participant boundary (originCRDRecords, through the same helper), and
	// the submission this inquiry continues named the member. An inquiry that kept
	// the local record id would ask the payer about a member the payer has never
	// heard of — measured: the payer refuses it outright, "unknown member". Only
	// the patient's own name changes; nothing the record asserts does.
	if sorID, ok := strings.CutPrefix(cont.SoRPatientID, "Patient/"); ok && sorID != cont.MemberID {
		if out.Patient, err = namePatientByMember(out.Patient, sorID, cont.MemberID); err != nil {
			return out, http.StatusBadGateway, "name the patient by the member id: " + err.Error()
		}
		if out.Coverage, err = namePatientByMember(out.Coverage, sorID, cont.MemberID); err != nil {
			return out, http.StatusBadGateway, "name the patient in the coverage by the member id: " + err.Error()
		}
	}

	// The provider and the payer organization are named BY the participant's own
	// records — the order names its provider, the coverage names its payer — and
	// each is then read from the same system. No identity is chosen here.
	if _, out.Provider, status, msg = g.pasProvider(ctx, order); status != 0 {
		return out, status, msg
	}

	// Through memberPayerOrganization, the ONE reader of "which organization is
	// this coverage's payer". This site used to resolve the payor reference
	// itself, which read a coverage that carries its payer organization INSIDE
	// itself — a perfectly ordinary FHIR shape, and the one the demo roster's own
	// records use — as a coverage naming a record the system does not hold. Two
	// readings of one fact, and this one was wrong.
	if coveragePayorRef(out.Coverage) == "" {
		return out, http.StatusBadGateway, "the coverage names no payer organization, and an inquiry must name one"
	}
	insurer, status, msg := g.memberPayerOrganization(ctx, out.Coverage)
	if status != 0 {
		return out, status, msg
	}
	out.Insurer = insurer
	return out, 0, ""
}

// pasOrderChanged compares the order as the participant's system holds it NOW
// against the item map of what was submitted.
//
// The item map is the record of what the payer was actually asked about. If the
// order has since become a different request, an inquiry built from it would ask
// about lines the payer never saw, and an inquiry built from the item map would
// report a decision about a request the participant no longer has. Neither is
// answerable, so the change is reported and the inquiry is not sent.
//
// EVERY LINE IS COMPARED, on BOTH facts the item map holds. A submission's lines
// all derive from this one order, so each of them must still match it: checking
// only that SOME line matches would wave through a two-line submission whose
// order now accounts for one of them, and checking only the product code would
// wave through a rescheduled order — the same device, a different date of
// service, which is a different request to a payer. Both of those were live
// holes before this comparison was written per-line and on both facts.
func pasOrderChanged(order []byte, cont Continuation) (bool, string) {
	nowCode, nowDate, err := orderItemFacts(order)
	if err != nil {
		return true, "the order in the system of record " + err.Error()
	}
	if len(cont.Items) == 0 {
		// Nothing pins what was submitted, so nothing can say the order still
		// matches it. "Cannot say" is not "unchanged".
		return true, "the continuation records no submitted line to compare the order against"
	}
	for _, it := range SortedContinuationItems(cont.Items) {
		if it.ProductCode != nowCode {
			return true, fmt.Sprintf("line %d was submitted for %s and the order now asks for %s",
				it.Sequence, it.ProductCode, nowCode)
		}
		if it.ServiceDate != nowDate {
			return true, fmt.Sprintf("line %d was submitted for service on %s and the order now asks for %s",
				it.Sequence, blankAs(it.ServiceDate, "no stated date"), blankAs(nowDate, "no stated date"))
		}
	}
	return false, ""
}

// blankAs renders an absent value as words, so a report never reads "… on  and
// the order now asks for 2028-12-31".
func blankAs(v, absent string) string {
	if v == "" {
		return absent
	}
	return v
}

// orderItemFacts reads the two facts the item map pins about an order: its
// product coding and its date of service. BOTH sides of the change check read
// them through this one function, so "as submitted" and "as it stands now" are
// always like for like — a comparison whose two halves were derived differently
// would report a change whenever the derivations differed rather than when the
// order did.
func orderItemFacts(order []byte) (code, serviceDate string, err error) {
	system, c, _, perr := shnsdk.ParseOrderProductCoding(order)
	if perr != nil {
		return "", "", errors.New("has no readable product coding")
	}
	code = codingKey(system, c)
	if code == "" {
		return "", "", errors.New("has no product coding")
	}
	return code, orderServiceDate(order), nil
}

// orderServiceDate reads the date of service an order states, as a FHIR date.
// An order that states none yields the empty string, which is a fact about the
// order and compares equal to the same absence recorded at submission.
func orderServiceDate(order []byte) string {
	var probe struct {
		OccurrenceDateTime string `json:"occurrenceDateTime"`
		OccurrencePeriod   struct {
			Start string `json:"start"`
		} `json:"occurrencePeriod"`
	}
	if json.Unmarshal(order, &probe) != nil {
		return ""
	}
	for _, v := range []string{probe.OccurrenceDateTime, probe.OccurrencePeriod.Start} {
		if len(v) >= 10 {
			return v[:10]
		}
	}
	return ""
}

// pasProvider resolves the provider a prior-authorization request about this
// order names — the ONE selection, shared with the submission itself.
//
// The rule lives in the SDK (shnsdk.SelectPASProvider), beside both builders,
// because a submission and the inquiry about it must name the SAME party: a
// submission naming the ordering PractitionerRole while the inquiry named the
// rendering Organization fails the payer's match for exactly the reason a
// party-less submission fails, only later and harder to find. This gateway
// supplies the one thing the rule cannot have — a reader into the PARTICIPANT'S
// OWN system — and maps its refusals onto this leg's status.
//
// That refusal matters more than it looks. Before it existed, an order naming
// nobody carryable reached the inquiry builder and came back as a generic type
// complaint from deep inside it, with no reference to look at — and the one
// persona seeded to exercise this path named a Practitioner, so the path was
// broken and unreadable at the same time.
// held are records this flow ALREADY holds for the order it is about — the
// dispatched supplier is the one case: the dispatch step read (or, on the demo
// lane whose order this gateway authored, built) that Organization before the
// prior authorization began, and the request carries those same bytes. Looking
// there first is not a fallback identity: the reference must still name that
// record, and anything else goes to the participant's system as before.
func (g *Gateway) pasProvider(ctx context.Context, order []byte, held ...[]byte) (ref string, resource []byte, status int, msg string) {
	ref, resource, err := shnsdk.SelectPASProvider(order, func(reference string) ([]byte, bool, error) {
		for _, rec := range held {
			if len(rec) > 0 && shnsdk.PASResourceTypeOf(rec)+"/"+pasResourceID(rec) == reference {
				return rec, true, nil
			}
		}
		return ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, reference)
	})
	if err != nil {
		return "", nil, http.StatusBadGateway, err.Error()
	}
	return ref, resource, 0, ""
}

// pasMemberSystem reads the namespace the PARTICIPANT'S OWN system names this
// member under, off the Patient that system holds.
//
// It is the other half of what a payer matches a prior-authorization inquiry on.
// A request whose Patient carries only an id is stored by the payer under a
// member it cannot key on, and no conformant inquiry finds it again — the same
// defect as a Claim.provider carrying display text alone, one element over. So
// the system is READ, never assumed: a participant whose records name members
// under their own namespace sends that one, and a participant whose Patient
// carries no member identifier at all is refused here rather than sending a
// request nobody can match.
// The namespace is NOT re-read here. It rides from the one reading of the
// member's Patient the flow already made (crdOriginRecords), because reading one
// identity twice is what the single-read invariant forbids — and two readings
// are two chances to disagree.
func (g *Gateway) pasMemberSystem(system, member string) (string, int, string) {
	if strings.TrimSpace(system) == "" {
		return "", http.StatusBadGateway,
			"the system of record's patient for member " + member +
				" carries no identifier naming them, and a payer matches a prior authorization on the member id"
	}
	return system, 0, ""
}

// pasMemberSystemOrFail is pasMemberSystem for a scenario handler: it writes the
// refusal itself, in the same words at every originator flow.
func (g *Gateway) pasMemberSystemOrFail(w http.ResponseWriter, system, member string) (string, bool) {
	out, status, msg := g.pasMemberSystem(system, member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return "", false
	}
	return out, true
}

// memberIdentifierSystemOfRecord reads the namespace a Patient record names a
// member under, or "" when it names them under none.
func memberIdentifierSystemOfRecord(patient []byte, member string) string {
	var probe struct {
		Identifier []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
	}
	if json.Unmarshal(patient, &probe) != nil {
		return ""
	}
	for _, id := range probe.Identifier {
		if id.Value == member && strings.TrimSpace(id.System) != "" {
			return id.System
		}
	}
	return ""
}

// pasResourceID reads a resource's own id, or "" when it states none.
func pasResourceID(resource []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resource, &probe) != nil {
		return ""
	}
	return probe.ID
}

// pasProviderOrFail is pasProvider for a scenario handler: it writes the refusal
// itself, so every originator flow refuses an order naming no carryable provider
// in the same words rather than each inventing its own.
func (g *Gateway) pasProviderOrFail(w http.ResponseWriter, r *http.Request, order []byte) ([]byte, bool) {
	_, provider, status, msg := g.pasProvider(r.Context(), order)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return nil, false
	}
	return provider, true
}

// coveragePayorRef reads the payer organization a Coverage names.
func coveragePayorRef(coverage []byte) string {
	var probe struct {
		Payor []struct {
			Reference string `json:"reference"`
		} `json:"payor"`
	}
	if json.Unmarshal(coverage, &probe) != nil {
		return ""
	}
	for _, p := range probe.Payor {
		if p.Reference != "" {
			return p.Reference
		}
	}
	return ""
}

// The continuation's provider identifier is NOT derived here any more. It is
// read off the SUBMITTED BUNDLE by shnsdk.NewPriorAuthContinuation, like every
// other fact a continuation holds, so the party recorded and the party on the
// wire are the same one. The derivation this replaced took an NPI stated inline
// on the order's requester reference FIRST, so an order whose requester carried
// an inline NPI and whose performer was the carryable Organization recorded one
// party and named another — a disagreement nothing downstream would have caught.

// --- the route that continues a decision later ---

// paInquireReq is what POST /scenario/pa/inquire takes: the capability, and
// optionally how long to follow the decision this time.
type paInquireReq struct {
	Continuation string `json:"continuation"`
	// WaitSeconds is optional. Absent means the route's own default; zero means
	// ask once and report what comes back.
	WaitSeconds *int `json:"waitSeconds,omitempty"`
}

// paInquireResp is the wait operation's result on the wire: the decision, the
// continuation it belongs to, and the payer's own answer.
type paInquireResp struct {
	Decision            string          `json:"decision"`
	Continuation        string          `json:"continuation,omitempty"`
	ContinuationDurable bool            `json:"continuationDurable"`
	AuthNumber          string          `json:"authNumber,omitempty"`
	ValidUntil          string          `json:"validUntil,omitempty"`
	Denied              bool            `json:"denied,omitempty"`
	Rationale           string          `json:"rationale,omitempty"`
	PendedItems         []string        `json:"pendedItems,omitempty"`
	Inquiries           int             `json:"inquiries"`
	PayerResponse       json.RawMessage `json:"payerResponse,omitempty"`
}

// handlePAInquire is POST /scenario/pa/inquire: continue a prior-authorization
// decision this gateway pended earlier.
//
// The continuation id IS the capability, bound to {holder, continuationID}. An id
// this holder did not mint answers 404 and says nothing more — an operator
// surface that distinguished "not yours" from "no such thing" would confirm which
// ids exist. An id this gateway minted and could not keep answers 410 and says so.
//
// The trust boundary is the one the rest of /scenario/* already has: these routes
// are never publicly routed (the public ingress 404s them and the Kit binds them
// to localhost). Operator authentication for this surface is the console's own
// tracked follow-up and is deliberately not invented here.
func (g *Gateway) handlePAInquire(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	var req paInquireReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body failed"})
			return
		}
	}
	if req.Continuation == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "continuation is required"})
		return
	}
	store, ok := g.continuations()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "this gateway keeps no prior-authorization continuations"})
		return
	}
	cont, look, err := store.ReadContinuation(g.cfg.HolderID, req.Continuation)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the continuation store is unavailable"})
		return
	}
	if look != ContinuationFound {
		status, msg := ContinuationRefusal(look)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	wait := PASWaitDefault
	if req.WaitSeconds != nil {
		if *req.WaitSeconds < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "waitSeconds must not be negative"})
			return
		}
		wait = clampPASWait(time.Duration(*req.WaitSeconds) * time.Second)
	}
	out, status, msg, err := g.inquireContinuation(r.Context(), r, cont)
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	out.Continuation, out.ContinuationDurable = cont.ID, DurableContinuations(store)
	out.Inquiries = 1
	if out.Decision == PASDecisionPended && wait > 0 {
		// Still pended: keep following it for the rest of the caller's wait,
		// under the same bounds the submit's own wait runs under.
		out, status, msg, err = g.followPended(r.Context(), out, wait, g.inquiryFor(r, cont))
		if status != 0 {
			if g.relayOriginationError(w, err) {
				return
			}
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	writeJSON(w, http.StatusOK, paInquireRespOf(out))
}

// applyTo projects a payer determination onto a scenario response, leaving
// everything the handler already filled in alone.
//
// Every originator surface goes through this one function, so an approval, a
// denial and a pend are reported with the same words everywhere. A handler that
// spelled a denial its own way would leave a consumer reading one route's
// vocabulary and missing another's.
func (d PASDecision) applyTo(resp uc03Resp) uc03Resp {
	resp.Decision = d.Decision
	resp.AuthNumber = d.Parsed.PreAuthRef
	resp.ValidUntil = d.Parsed.ValidUntil
	resp.Denied = d.Decision == PASDecisionDenied
	resp.Pended = d.Decision == PASDecisionPended
	resp.Continuation = d.Continuation
	if d.Continuation != "" {
		// Stated only where there IS a continuation, and stated explicitly when
		// it is false — that is the disclosure, and a disclosure that is
		// indistinguishable from silence is not one.
		durable := d.ContinuationDurable
		resp.ContinuationDurable = &durable
	}
	if d.Parsed.Denial != nil {
		resp.Rationale = d.Parsed.Denial.Rationale
	}
	if codes := neededItemCodes(d.Parsed.NeededItems); len(codes) > 0 {
		resp.PendedItems = codes
	}
	return resp
}

// applyToUC05 is applyTo for the federated-retrieval surface, which carries a
// facility attribution of its own. It is the SAME projection — one vocabulary for
// a determination, wherever it is reported.
func (d PASDecision) applyToUC05(resp uc05Resp) uc05Resp {
	projected := d.applyTo(uc03Resp{PendedItems: resp.PendedItems})
	resp.Decision = projected.Decision
	resp.AuthNumber = projected.AuthNumber
	resp.ValidUntil = projected.ValidUntil
	resp.Denied = projected.Denied
	resp.Rationale = projected.Rationale
	resp.Pended = projected.Pended
	resp.Continuation = projected.Continuation
	resp.ContinuationDurable = projected.ContinuationDurable
	resp.PendedItems = projected.PendedItems
	return resp
}

// paInquireRespOf projects a decision onto the route's answer shape.
func paInquireRespOf(d PASDecision) paInquireResp {
	out := paInquireResp{
		Decision:            d.Decision,
		Continuation:        d.Continuation,
		ContinuationDurable: d.ContinuationDurable,
		AuthNumber:          d.Parsed.PreAuthRef,
		ValidUntil:          d.Parsed.ValidUntil,
		Denied:              d.Decision == PASDecisionDenied,
		Inquiries:           d.Inquiries,
		PendedItems:         neededItemCodes(d.Parsed.NeededItems),
	}
	if d.Parsed.Denial != nil {
		out.Rationale = d.Parsed.Denial.Rationale
	}
	if json.Valid(d.PayerResponse) {
		out.PayerResponse = json.RawMessage(d.PayerResponse)
	}
	return out
}
