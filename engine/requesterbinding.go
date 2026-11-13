package engine

// requesterbinding.go — the requester-side binding record
// (PARTICIPANT_PROTOCOL §6.1c): on a leg whose request names no patient (its
// row's subject: from-answer), the patient is bound from the answer. The
// responder's gateway binds it, and its answer's token names that binding.
// The requester's gateway binds the same patient through its own records and
// reports that binding to the Hub (POST {hub}/requester-binding), after it
// has delivered the answer, so the Hub records the leg under it beside the
// responder's.
//
// The report never holds up or changes the answer: it runs once the answer
// is written (afterAnswer), and the call's exchange record waits for it, so
// the record and its access line say what became of it. It is sent once,
// and sent again only once, and only when the Hub states it never took it (a
// 503 with X-SHN-Delivered: no, resendRequesterBinding), so the Hub's 409
// stays strict, and its outcome is kept apart (RequesterBindingOutcomes):
// accepted (the Hub's 204); refused (any other answer of the Hub's, with its
// status, or a refusal the Authorization Framework states for the binding's
// token, when nothing is sent); unknown (any byte of the record was written
// and no answer came: a lost 204 is never read as not reported); not-sent
// (nothing reached the Hub, with why); and other (a report that ended
// abnormally).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// What became of a requester-side binding record (RequesterBindingRecord.Outcome).
const (
	// RequesterBindingAccepted: the Hub answered 204.
	RequesterBindingAccepted = "accepted"
	// RequesterBindingRefused: the Hub answered anything else (a 503 among
	// them: final, but for one stating it never took the record, which is
	// sent again once, and is final the second time), or the Authorization
	// Framework stated a
	// refusal of the binding's token (its 403, its 502 naming a rule, or an
	// earlier deploy's 400) and nothing was sent. Status and RefusedBy say
	// which.
	RequesterBindingRefused = "refused"
	// RequesterBindingUnknown: a byte of the record was written to the Hub and
	// no answer came back (a transport failure, the budget running out, or a
	// stop): the Hub may have recorded it. A cut before any byte was written
	// is other.
	RequesterBindingUnknown = "unknown"
	// RequesterBindingNotSent: nothing reached the Hub and nothing refused
	// the record: NotSent says why.
	RequesterBindingNotSent = "not-sent"
)

// RequesterBindingOutcomes is the closed list RequesterBindingRecord.Outcome
// draws from; "other" is a report that ended abnormally: a panic, or a cut (a
// stop, or the budget run out) before any byte of the record was written.
var RequesterBindingOutcomes = []string{RequesterBindingAccepted, RequesterBindingRefused, RequesterBindingUnknown, RequesterBindingNotSent, ExchangeOther}

// Why a record was not sent (RequesterBindingRecord.NotSent).
const (
	// RequesterNotSentAuthorization: no token for the binding was minted, and
	// the Authorization Framework stated no refusal: it could not be reached,
	// answered without stating one (a bare 5xx, a 429), or its answer carried
	// no token that verifies for the answer and the binding, or none with the
	// involvement asked for.
	RequesterNotSentAuthorization = "authorization-failed"
	// RequesterNotSentHubUnreachable: the connection to the Hub failed before
	// any byte of the record was written to it.
	RequesterNotSentHubUnreachable = "hub-unreachable"
	// RequesterNotSentUnbuildable: the record, or the request carrying it,
	// could not be built here: nothing was dialed.
	RequesterNotSentUnbuildable = "record-unbuildable"
)

// RequesterNotSentReasons is the closed list RequesterBindingRecord.NotSent
// draws from.
var RequesterNotSentReasons = []string{RequesterNotSentAuthorization, RequesterNotSentHubUnreachable, RequesterNotSentUnbuildable}

// Why a requester could not bind the patient an answer disclosed
// (RequesterBindingRecord.Unbound). It stays on the requester's own record:
// the Hub is told only that the binding is unavailable.
const (
	// RequesterUnboundUnreadable: the answer could not be read as the
	// resource its leg answers with.
	RequesterUnboundUnreadable = "unreadable-answer"
	// RequesterUnboundNoMember: the answer names no one member number for
	// its patient (none, two that differ, or only a reference into the
	// responder's own system, which this gateway cannot read).
	RequesterUnboundNoMember = "no-member"
	// RequesterUnboundUnknownMember: this participant's records do not hold
	// the member, and it requires known members (RequireKnownMembers).
	RequesterUnboundUnknownMember = "unknown-member"
	// RequesterUnboundUnidentifiable: the member number is shared with
	// another person the answer carries, and no Patient of the member's own
	// identifies one patient.
	RequesterUnboundUnidentifiable = "unidentifiable"
	// RequesterUnboundSystemOfRecord: this participant's system of record
	// could not be read.
	RequesterUnboundSystemOfRecord = "system-of-record-unavailable"
)

// RequesterUnboundReasons is the closed list RequesterBindingRecord.Unbound
// draws from.
var RequesterUnboundReasons = []string{RequesterUnboundUnreadable, RequesterUnboundNoMember, RequesterUnboundUnknownMember, RequesterUnboundUnidentifiable, RequesterUnboundSystemOfRecord}

// RequesterBindingRecord is what this gateway, as the requester, reported to
// the Hub of its binding of the patient an answer disclosed, and what became
// of it (ExchangeRecord.RequesterBinding). Metadata only: never a patient
// identifier or a token.
type RequesterBindingRecord struct {
	// Binding is shnsdk.RequesterBindingBound or
	// shnsdk.RequesterBindingUnavailable.
	Binding string
	// Involvement is the binding's involvement when bound:
	// shnsdk.InvolvementRequesterHeld (a member this participant's records
	// hold) or shnsdk.InvolvementRequesterDerived.
	Involvement string
	// Unbound is why the binding is unavailable, from the workstream's closed
	// list (RequesterUnboundReasons); it stays here and never reaches the Hub.
	Unbound string
	// Outcome is RequesterBindingOutcomes' value.
	Outcome string
	// RefusedBy is who refused the record (RefusedByHub, or
	// RefusedByAuthorizationFramework when it refused the binding's token);
	// set exactly when Outcome is refused.
	RefusedBy string
	// Status is the refusing answer's status; set exactly when Outcome is
	// refused.
	Status int
	// NotSent is why nothing was sent (RequesterNotSentReasons); set exactly
	// when Outcome is not-sent.
	NotSent string
	// Resent says a second send of the record was begun, after the Hub
	// stated on its 503 that it never took the first
	// (resendRequesterBinding): Outcome is then the second send's, not-sent
	// when none of it reached the Hub.
	Resent bool
}

// requesterBindingBudget bounds the whole report: the binding, the token for
// it and the POST, with its one resend and the wait before it. The report runs after the answer is written, so it
// holds up no one; the bound keeps a call's work from outliving it unseen.
const requesterBindingBudget = 10 * time.Second

// afterAnswerStopWait bounds each of the two waits a stop makes for reports
// in flight (stopAfterAnswers): one for them to finish, then, once they are
// cut, one for them to settle. The step is at most twice it
// (DEPLOYMENT.md, "Stopping the gateway").
const afterAnswerStopWait = 500 * time.Millisecond

// afterStopper is how a stop cuts the reports in flight: ctx is cancelled to
// cut them, forced closed to write the records still waiting for one, and
// records counts the exchange records waiting for a report.
type afterStopper struct {
	once       sync.Once
	ctx        context.Context
	cancel     context.CancelFunc
	forced     chan struct{}
	forcedOnce sync.Once
	records    sync.WaitGroup
}

// afterStop is the gateway's afterStopper, made on first use, so a Gateway
// built without New has one too.
func (g *Gateway) afterStop() *afterStopper {
	st := &g.afterStopping
	st.once.Do(func() {
		st.ctx, st.cancel = context.WithCancel(context.Background())
		st.forced = make(chan struct{})
	})
	return st
}

// stopAfterAnswers is a stop's step for the reports in flight: it waits up to
// afterAnswerStopWait for them, then cuts them (each settles by what it had
// done: unknown once a byte of the record was written, other before) and
// waits as long again, then has every exchange record still waiting written
// with what its report noted. Every read's record and access line is
// written, whatever the reports do.
func (g *Gateway) stopAfterAnswers() {
	st := g.afterStop()
	if !waitFor(&g.afterAnswers, afterAnswerStopWait) {
		st.cancel()
		if !waitFor(&g.afterAnswers, afterAnswerStopWait) {
			st.forcedOnce.Do(func() { close(st.forced) })
		}
	}
	st.cancel()
	st.records.Wait()
}

// waitFor reports whether wg is done within d.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// legTokens are a leg's two tokens as its envelopes carried them, and the
// answer's token as verified: what a requester-side binding record presents.
type legTokens struct {
	request, response string
	responseToken     shnsdk.Token
}

type legTokensKey struct{}

// withLegTokensSink returns ctx carrying sink: the round trip stores the
// leg's tokens in it once the answer's token has verified.
func withLegTokensSink(ctx context.Context, sink *legTokens) context.Context {
	return context.WithValue(ctx, legTokensKey{}, sink)
}

// noteLegTokens stores t in ctx's sink, if any.
func noteLegTokens(ctx context.Context, t legTokens) {
	if sink, ok := ctx.Value(legTokensKey{}).(*legTokens); ok && sink != nil {
		*sink = t
	}
}

// requesterBindingReport is what a leg's round trip gives its requester-side
// binding report: the leg's tokens, as the envelopes carried them.
type requesterBindingReport struct {
	toks legTokens
}

// withRequesterBindingReport returns ctx carrying a report the round trip
// fills (noteLegTokens), for reportRequesterBindingAfter.
func withRequesterBindingReport(ctx context.Context) (context.Context, *requesterBindingReport) {
	rep := &requesterBindingReport{}
	return withLegTokensSink(ctx, &rep.toks), rep
}

// requesterBindingDue reports whether an answer is reported to the Hub: it
// was delivered to this gateway's participant, as a 2xx whose token names a
// patient, so it disclosed that patient. An answer that named no patient
// disclosed none, and one that was not delivered disclosed nothing to the
// participant: neither has a record.
func requesterBindingDue(delivered bool, stated statedAnswer, toks legTokens) bool {
	// A framed answer states its 2xx; an unframed one states none, and is a
	// 2xx (a non-2xx never reaches here as an answer).
	answered2xx := stated.status == 0 || stated.status/100 == 2
	return delivered && answered2xx && toks.responseToken.Subject != "" && toks.response != ""
}

// reportRequesterBindingAfter is the requester side's whole report on a leg
// whose request names no patient (its row's subject: from-answer), for a
// workstream that carries one: when the answer is due a record
// (requesterBindingDue), it binds, after the answer is written
// (afterAnswer), the member the answer names (memberOf, the leg's own
// reading of its answer) through this participant's records, by the shared
// rule (resolveSubjectBinding), and reports that binding to the Hub
// (reportRequesterBinding), noting what became of it on the call's record.
// memberOf reports readable false for an answer that is not the resource
// the leg answers with, and member "" for one naming no one member.
func (g *Gateway) reportRequesterBindingAfter(ctx context.Context, leg string, rep *requesterBindingReport, delivered bool, stated statedAnswer, answer []byte, memberOf func(answer []byte) (member string, readable bool)) {
	if rep == nil || !requesterBindingDue(delivered, stated, rep.toks) {
		return
	}
	toks := rep.toks
	g.afterAnswer(ctx, func(ctx context.Context) {
		bound := g.bindRequesterMember(ctx, answer, memberOf)
		exchangeOf(ctx).requesterBindingOutcome(g.reportRequesterBinding(ctx, leg, toks, bound))
	})
}

// bindRequesterMember binds the member an answer names (memberOf) through
// this participant's own records by the shared rule (resolveSubjectBinding),
// or says why it could not (RequesterUnboundReasons).
func (g *Gateway) bindRequesterMember(ctx context.Context, answer []byte, memberOf func([]byte) (string, bool)) requesterBound {
	member, readable := memberOf(answer)
	switch {
	case !readable:
		return requesterBound{unbound: RequesterUnboundUnreadable}
	case member == "":
		return requesterBound{unbound: RequesterUnboundNoMember}
	}
	pci, held, found, err := g.resolveSubjectBinding(ctx, member, answer)
	switch {
	case errors.Is(err, errPatientUnidentifiable):
		return requesterBound{unbound: RequesterUnboundUnidentifiable}
	case err != nil:
		return requesterBound{unbound: RequesterUnboundSystemOfRecord}
	case !found || pci == "":
		return requesterBound{unbound: RequesterUnboundUnknownMember}
	}
	return requesterBound{pci: pci, held: held}
}

// requesterBound is a requester's own binding of the patient an answer
// disclosed: the patient and whether its records hold the member, or, when
// it could not bind one, why (unbound).
type requesterBound struct {
	pci     string
	held    bool
	unbound string
}

// afterAnswer runs f once the call's answer is written, off the call: its
// caller has its answer whatever f does or how long it takes, within
// requesterBindingBudget, or until a stop cuts it. The call's exchange record
// waits for f, so what f notes is on it. f's context keeps the call's values
// and none of its cancellation. A panic in f is recorded as other.
func (g *Gateway) afterAnswer(ctx context.Context, f func(ctx context.Context)) {
	done := exchangeOf(ctx).awaitAfterAnswer()
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.bindingBudget())
	unhook := context.AfterFunc(g.afterStop().ctx, cancel)
	g.afterAnswers.Add(1)
	go func() {
		defer g.afterAnswers.Done()
		defer done()
		defer cancel()
		defer unhook()
		defer func() {
			if recover() != nil {
				exchangeOf(actx).requesterBindingAborted()
			}
		}()
		f(actx)
	}()
}

// bindingBudget is requesterBindingBudget, or a test's shorter one.
func (g *Gateway) bindingBudget() time.Duration {
	if g.bindingBudgetForTest > 0 {
		return g.bindingBudgetForTest
	}
	return requesterBindingBudget
}

// reportRequesterBinding reports bound, this gateway's binding of the patient
// the answer of leg disclosed, to the Hub, and returns what became of it. The
// answer's token must name a patient: an answer that discloses none has no
// record. It is sent once, and again only as resendRequesterBinding allows.
func (g *Gateway) reportRequesterBinding(ctx context.Context, leg string, toks legTokens, bound requesterBound) RequesterBindingRecord {
	x := exchangeOf(ctx)
	if ctx.Err() != nil && bound.pci == "" {
		// Cut (a stop, or the budget run out) while binding: the patient was
		// not bound because the binding did not finish, whatever held it up
		// (a system of record that hangs past the budget among them), so no
		// reason of the answer's or of this participant's records is named.
		return RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Outcome: ExchangeOther}
	}
	rec := RequesterBindingRecord{Binding: shnsdk.RequesterBindingUnavailable, Unbound: bound.unbound}
	body := shnsdk.RequesterBinding{RequestToken: toks.request, ResponseToken: toks.response, Binding: shnsdk.RequesterBindingUnavailable}
	if bound.pci != "" {
		involvement := shnsdk.InvolvementRequesterDerived
		if bound.held {
			involvement = shnsdk.InvolvementRequesterHeld
		}
		rec = RequesterBindingRecord{Binding: shnsdk.RequesterBindingBound, Involvement: involvement}
		x.requesterBindingPending(rec)
		token, status, err := g.requesterBindingToken(ctx, leg, toks, bound.pci, involvement)
		switch {
		case status != 0:
			// The Framework refused the binding's token: nothing is sent.
			rec.Outcome, rec.RefusedBy, rec.Status = RequesterBindingRefused, RefusedByAuthorizationFramework, status
			return rec
		case err != nil && ctx.Err() != nil:
			// Cut while the token was asked for, by a stop or the budget: a
			// Framework that hangs past the budget lands here too, as other,
			// since it neither answered nor failed.
			rec.Outcome = ExchangeOther
			return rec
		case err != nil:
			rec.Outcome, rec.NotSent = RequesterBindingNotSent, RequesterNotSentAuthorization
			return rec
		}
		body.Binding = shnsdk.RequesterBindingBound
		body.Involved = &shnsdk.InvolvedToken{Token: token, Involvement: involvement}
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > shnsdk.MaxRequesterBindingBytes {
		rec.Outcome, rec.NotSent = RequesterBindingNotSent, RequesterNotSentUnbuildable
		return rec
	}
	// One holder assertion for the record: a resend is the same request,
	// byte for byte.
	assertion, err := g.cfg.Identity.AssertionForBody("hub", g.cfg.Clock(), time.Minute, raw)
	if err != nil {
		rec.Outcome, rec.NotSent = RequesterBindingNotSent, RequesterNotSentUnbuildable
		return rec
	}
	if ctx.Err() != nil {
		// Cut before anything was dialed.
		rec.Outcome = ExchangeOther
		return rec
	}
	// Once a byte is written (postRequesterBinding), the record may be the
	// Hub's.
	x.requesterBindingPending(rec)
	start := time.Now()
	status, header, sent, err := g.postRequesterBinding(ctx, raw, assertion)
	if wait, ok := resendRequesterBinding(ctx, status, header, err, time.Since(start)); ok {
		// The Hub stated it never took the record: until it is sent again,
		// none of it is the Hub's, so a cut now is other.
		x.requesterBindingNotTaken()
		if !g.waitToResend(ctx, wait) {
			rec.Outcome = ExchangeOther
			return rec
		}
		rec.Resent = true
		x.requesterBindingPending(rec)
		status, _, sent, err = g.postRequesterBinding(ctx, raw, assertion)
	}
	switch {
	case errors.Is(err, errRecordUnbuildable):
		rec.Outcome, rec.NotSent = RequesterBindingNotSent, RequesterNotSentUnbuildable
	case err == nil && status == http.StatusNoContent:
		rec.Outcome = RequesterBindingAccepted
	case err == nil:
		rec.Outcome, rec.RefusedBy, rec.Status = RequesterBindingRefused, RefusedByHub, status
	case sent:
		rec.Outcome = RequesterBindingUnknown
	case ctx.Err() != nil:
		// Cut before any byte of the record was written, by a stop or the
		// budget: a Hub that hangs past the budget before the record is
		// written lands here too, as other, since it neither answered nor
		// failed.
		rec.Outcome = ExchangeOther
	default:
		rec.Outcome, rec.NotSent = RequesterBindingNotSent, RequesterNotSentHubUnreachable
	}
	return rec
}

// The wait before a record's one resend, from the Hub's Retry-After: bounded
// to 1-3 s, and 1 s when it is absent or unreadable.
const (
	requesterBindingResendMinWait = time.Second
	requesterBindingResendMaxWait = 3 * time.Second
)

// resendRequesterBinding reports whether a record is sent again, and after
// what wait: only when the Hub's answer to its first send was a 503 carrying
// HubDeliveredHeader exactly "no", the Hub's statement that it never took the
// record (its readiness refusal, before the registry is loaded: nothing of
// the record was read or claimed), and only when the wait and another send,
// timed as the first took, end before the report's budget does. Decided by the
// status and the header, never by the answer's text. Any other 503 is final:
// one without the header (a load balancer's own, or the Hub's own refusal
// when its replay or audit store is unavailable, made once the record is
// read, which carries none) may have been taken. A transport failure is
// never resent: the record may have been written. It is asked of the first
// send only, so a second refusal is final.
func resendRequesterBinding(ctx context.Context, status int, h http.Header, err error, took time.Duration) (time.Duration, bool) {
	if err != nil || status != http.StatusServiceUnavailable {
		return 0, false
	}
	if vs := h.Values(HubDeliveredHeader); len(vs) != 1 || vs[0] != "no" {
		return 0, false
	}
	wait := requesterBindingResendWait(h)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait+took {
		return 0, false
	}
	return wait, true
}

// requesterBindingResendWait is h's Retry-After in whole seconds, bounded to
// requesterBindingResendMinWait..requesterBindingResendMaxWait; absent,
// repeated or not a number of seconds, it is the least wait.
func requesterBindingResendWait(h http.Header) time.Duration {
	vs := h.Values("Retry-After")
	if len(vs) != 1 {
		return requesterBindingResendMinWait
	}
	n, err := strconv.ParseUint(vs[0], 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return requesterBindingResendMaxWait
	case err != nil:
		return requesterBindingResendMinWait
	}
	switch {
	case n < uint64(requesterBindingResendMinWait/time.Second):
		return requesterBindingResendMinWait
	case n > uint64(requesterBindingResendMaxWait/time.Second):
		return requesterBindingResendMaxWait
	}
	return time.Duration(n) * time.Second
}

// waitToResend waits d before a record's resend, or a test's stand-in for the
// wait; false when ctx ends first (a stop, or the budget).
func (g *Gateway) waitToResend(ctx context.Context, d time.Duration) bool {
	if g.resendWaitForTest != nil {
		return g.resendWaitForTest(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// errRecordUnbuildable is a record whose request could not be built here
// (its holder assertion, or the request itself): nothing was dialed.
var errRecordUnbuildable = errors.New("requester binding: the record's request could not be built")

// requesterBindingToken asks the Authorization Framework for the token of
// this gateway's binding (pci) of the patient the answer disclosed: for the
// leg's response operation, in its response frame, bound to the answer's
// payload and correlation, with involvement, presenting the answer's own
// token. It returns the token as the record carries it, or the status of a
// refusal the Framework stated (its 403 denial, its 502 naming the rule it
// could not record the decision on, or an earlier deploy's 400 for a request
// it does not read), or an error when no token was minted and no refusal was
// stated: the Framework unreachable, limiting this gateway, answering a bare
// status (a 5xx, a proxy's 502), or minting a token that does not verify or
// does not carry the involvement asked for.
func (g *Gateway) requesterBindingToken(ctx context.Context, leg string, toks legTokens, pci, involvement string) (string, int, error) {
	spec, ok := legSpecFor(leg)
	if !ok {
		return "", 0, errors.New("requester binding: unknown leg")
	}
	resp := toks.responseToken
	r := (&http.Request{}).WithContext(ctx)
	tok, err := g.authorizeRequest(r, authorizeReq{
		Frame: spec.RespFrame, Operation: spec.RespOp, SubjectPCI: pci, CorrelationID: resp.CorrelationID,
		PayloadHash: resp.PayloadHash, Involvement: involvement, ResponseToken: toks.response,
	})
	var refusal *authzRefusal
	var statusErr *shnsdk.StatusError
	switch {
	case errors.Is(err, errAuthorizationDenied):
		return "", http.StatusForbidden, err
	case errors.As(err, &refusal):
		// The Framework states a rule only on its 502 (authzStatedRule).
		return "", http.StatusBadGateway, err
	case errors.As(err, &statusErr) && statusErr.Code == http.StatusBadRequest:
		// An earlier deploy refuses a requester-side involvement 400.
		return "", http.StatusBadRequest, err
	case err != nil:
		return "", 0, err
	case tok.Involvement != involvement:
		// A Framework that does not carry the involvement asked for.
		return "", 0, errors.New("requester binding: the token does not carry the involvement asked for")
	}
	s, err := tokenJSON(tok)
	if err != nil {
		return "", 0, err
	}
	return s, 0, nil
}

// postRequesterBinding POSTs one record to the Hub, under assertion, a
// holder assertion for the audience hub whose bh binds body, once: it returns
// the Hub's status and headers, whether any byte of the request was written
// (so a failure after it is unknown, not unsent), and the transport's
// failure, if any; a request that could not be built is errRecordUnbuildable,
// nothing dialed. A redirect is the Hub's answer, never followed
// (serviceclient).
func (g *Gateway) postRequesterBinding(ctx context.Context, body []byte, assertion string) (int, http.Header, bool, error) {
	// The first header field written is the first byte of the record on the
	// wire: from then on the Hub may hold it.
	x := exchangeOf(ctx)
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaderField: func(string, []string) {
			if !wrote.Swap(true) {
				x.requesterBindingMaySend()
			}
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.HubURL+shnsdk.HubRequesterBindingPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, false, fmt.Errorf("%w: %w", errRecordUnbuildable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Holder-Assertion", assertion)
	client := g.hubClient()
	if client.Timeout > 0 {
		// One timer decides, the budget's or the client's, as on a leg.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
		req = req.WithContext(ctx)
		untimed := *client
		untimed.Timeout = 0
		client = &untimed
	}
	resp, err := client.Do(req)
	if err != nil {
		if serviceclient.IsRedirectRefused(err) {
			return serviceclient.RedirectStatus(err), nil, true, nil
		}
		return 0, nil, wrote.Load(), err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, true, nil
}
