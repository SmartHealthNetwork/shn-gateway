// Package auditappend is the gateway's audit append client and its outbox:
// a record the gateway writes about its own work is
// checked and queued, then signed by the gateway's holder key as it is sent to
// the Audit Plane's /append, retried with backoff until the Plane accepts or
// refuses it.
//
// The outbox keeps the record unsigned: each send signs it with the key the
// gateway runs with then. A holder that rotates its key stops the gateway,
// rotates and starts it again, and a durable outbox's records go out under
// the new key, not as old-key bytes the Plane no longer accepts.
//
// Nothing a leg does waits on it: Append never fails a leg and is bounded by
// its own short store timeout, and a record that cannot be queued is counted
// and dropped while the leg goes on exactly as before. Every record that
// leaves the outbox without the Plane's acceptance is counted by reason.
package auditappend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/backoff"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The appender's timings.
const (
	// EnqueueTimeout bounds Append's store write, the one step on a leg's path.
	EnqueueTimeout = 200 * time.Millisecond
	// sendTimeout bounds one POST to the Plane.
	sendTimeout = 10 * time.Second
	// lease holds a claimed record from other senders while one is sent; it
	// outlasts sendTimeout.
	lease = 30 * time.Second
	// idlePoll is how often an idle worker looks for a deferred record coming
	// due; a queued record wakes it at once.
	idlePoll = 30 * time.Second
	// reportEvery is how often the worker reports the queue's depth, and an
	// unknown endpoint.
	reportEvery = time.Minute
	// DrainLimit bounds the in-memory outbox's drain at stop: a step of the
	// gateway's documented stop budget.
	DrainLimit = 10 * time.Second
)

// retrySchedule is the wait after failures in a row: 1 s, doubling to 5 min,
// drawn from the upper half of each step.
var retrySchedule = backoff.Exponential{Base: time.Second, Max: 5 * time.Minute}

// The outcomes of Append, and the reasons a record is retried or dropped:
// closed sets, each a metric dimension.
const (
	Queued = "queued"

	DropOverflow = "overflow" // queued beyond Bound: the oldest record
	DropInvalid  = "invalid"  // refused by the content guard: at Append, never stored; or a queued row at send
	DropStore    = "store"    // the outbox could not take it in time
	DropRefused  = "refused"  // the Plane refused it (400, 403, 413, a 401 other than unknown sender)
	DropConflict = "conflict" // the Plane holds another record under its recordId (409)
	DropStopped  = "stopped"  // left in the in-memory outbox at stop

	RetryUnknownSender = "unknown-sender" // 401: the Plane does not hold this holder yet
	RetryUnavailable   = "unavailable"    // 408, 429, 5xx, or an answer the client does not know
	RetryUnreachable   = "unreachable"    // no answer
	RetryStore         = "store"          // the outbox could not be read
)

// DropReasons and RetryReasons are the closed lists of Metrics.Dropped and
// Metrics.Retried reasons.
var (
	DropReasons  = []string{DropOverflow, DropInvalid, DropStore, DropRefused, DropConflict, DropStopped}
	RetryReasons = []string{RetryUnknownSender, RetryUnavailable, RetryUnreachable, RetryStore}
)

// Metrics is how the appender reports; the app maps it to its emitter.
type Metrics interface {
	Sent()
	Retried(reason string)
	Dropped(reason string, n int)
	Depth(n int)
	EndpointUnknown(depth int)
}

// Config is an appender's wiring.
type Config struct {
	// Holder and SignPriv are the gateway's own holder id and signing key,
	// from its bundle: a holder signs its own records.
	Holder   string
	SignPriv ed25519.PrivateKey
	// Endpoint is discovery's endpoints.audit, read at boot. Empty is "no
	// audit endpoint known": records are queued and nothing is sent.
	Endpoint string
	Store    Store
	Client   *http.Client
	Metrics  Metrics
	// Now defaults to the clock and Rand (the recordId's entropy) to
	// crypto/rand. Jitter spreads each retry wait over the upper half of its
	// step; nil waits the whole step. The app passes math/rand/v2's Float64,
	// and tests inject all three.
	Now    func() time.Time
	Rand   io.Reader
	Jitter func() float64
	// After is the worker's timer; nil is time.After.
	After func(time.Duration) <-chan time.Time
	// Logf writes the appender's log lines (the drain at stop); nil writes
	// none.
	Logf func(format string, args ...any)
}

// Appender queues and sends the gateway's audit records.
type Appender struct {
	cfg      Config
	schedule backoff.Exponential
	wake     chan struct{}
}

// ClientForTest is the client the appender sends with.
func (a *Appender) ClientForTest() *http.Client { return a.cfg.Client }

// New returns an appender over cfg. It needs the holder, its signing key and
// a store.
func New(cfg Config) (*Appender, error) {
	switch {
	case cfg.Holder == "":
		return nil, errors.New("auditappend: no holder")
	case len(cfg.SignPriv) != ed25519.PrivateKeySize:
		return nil, errors.New("auditappend: no signing key")
	case cfg.Store == nil:
		return nil, errors.New("auditappend: no store")
	case cfg.Metrics == nil:
		return nil, errors.New("auditappend: no metrics")
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	s := retrySchedule
	s.Jitter = cfg.Jitter
	return &Appender{cfg: cfg, schedule: s, wake: make(chan struct{}, 1)}, nil
}

// EndpointKnown reports whether discovery named an audit endpoint at boot.
func (a *Appender) EndpointKnown() bool { return a.cfg.Endpoint != "" }

// Append queues r: it mints the recordId when r has none, checks the record
// (Check) and stores it unsigned, once its signed request would fit MaxBody.
// It never fails or holds up the caller beyond EnqueueTimeout: a record it
// cannot queue is counted under its reason and dropped. It returns the
// outcome, Queued or a drop reason.
func (a *Appender) Append(r shnsdk.AuditRecord) string {
	if r.RecordID == "" {
		id, err := NewRecordID(a.cfg.Now(), a.cfg.Rand)
		if err != nil {
			a.cfg.Metrics.Dropped(DropInvalid, 1)
			return DropInvalid
		}
		r.RecordID = id
	}
	if err := Check(r, a.cfg.Holder); err != nil {
		a.cfg.Metrics.Dropped(DropInvalid, 1)
		return DropInvalid
	}
	body, err := json.Marshal(r)
	if err != nil {
		a.cfg.Metrics.Dropped(DropInvalid, 1)
		return DropInvalid
	}
	if req, err := a.request(r); err != nil || len(req) > MaxBody {
		a.cfg.Metrics.Dropped(DropInvalid, 1)
		return DropInvalid
	}
	ctx, cancel := context.WithTimeout(context.Background(), EnqueueTimeout)
	defer cancel()
	dropped, err := a.cfg.Store.Enqueue(ctx, Entry{RecordID: r.RecordID, Body: body}, a.cfg.Now(), Bound)
	if err != nil {
		a.cfg.Metrics.Dropped(DropStore, 1)
		return DropStore
	}
	if dropped > 0 {
		a.cfg.Metrics.Dropped(DropOverflow, dropped)
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return Queued
}

// Run sends queued records until ctx ends. With no endpoint known it sends
// nothing and reports the unknown endpoint with the queue's depth.
//
// One record is sent at a time, the earliest due first (oldest among equals),
// so a record that keeps failing does not hold back the records behind it. After a failure the
// worker waits the retry schedule's wait for the failures in a row, and the
// record it could not send is due again at the end of that same wait; a
// newly queued record does not cut the wait short, so an outage is retried at
// the schedule's pace, not at the pace records are made. A success resets the
// schedule, and an idle worker wakes at once for a queued record.
func (a *Appender) Run(ctx context.Context) {
	report := a.cfg.After(0)
	failures := 0
	// backoff is the running retry wait, nil when none runs. It outlives the
	// loop's turns: a report tick during it only reports, and only its end
	// sends again, so the schedule climbs through an outage.
	var backoff <-chan time.Time
	for {
		if backoff == nil && a.EndpointKnown() {
			switch out, retryIn := a.sendOne(ctx, failures+1); out {
			case outcomeRetried:
				failures++
				backoff = a.cfg.After(retryIn)
			case outcomeLeft:
				failures = 0
				select {
				case <-ctx.Done():
					return
				case <-report:
					a.report(ctx)
					report = a.cfg.After(reportEvery)
				default:
				}
				continue
			case outcomeIdle:
				// Nothing due once no retry wait runs: no failure is pending
				// (another replica may have sent the record).
				failures = 0
			case outcomeStopped:
				return
			}
		}
		var wake <-chan struct{}
		var idle <-chan time.Time
		if backoff == nil {
			wake, idle = a.wake, a.cfg.After(idlePoll)
		}
		select {
		case <-ctx.Done():
			return
		case <-report:
			a.report(ctx)
			report = a.cfg.After(reportEvery)
		case <-wake:
		case <-idle:
		case <-backoff:
			backoff = nil
		}
	}
}

// report emits the queue's depth, and the unknown endpoint when it is.
func (a *Appender) report(ctx context.Context) {
	n, err := a.cfg.Store.Depth(ctx)
	if err != nil {
		return
	}
	a.cfg.Metrics.Depth(n)
	if !a.EndpointKnown() {
		a.cfg.Metrics.EndpointUnknown(n)
	}
}

// What sendOne did with the outbox.
const (
	outcomeIdle    = iota // nothing was due
	outcomeLeft           // a record left the outbox: accepted, or dropped
	outcomeRetried        // a record, or the outbox itself, failed and waits
	outcomeStopped        // ctx ended mid-send; the record stays claimed for its lease
)

// sendOne sends the oldest due record. On a failure the record is due again
// after the retry schedule's wait for failures in a row, returned as retryIn,
// one draw for both the record and the worker.
func (a *Appender) sendOne(ctx context.Context, failures int) (outcome int, retryIn time.Duration) {
	now := a.cfg.Now()
	e, ok, err := a.cfg.Store.Claim(ctx, now, lease)
	switch {
	case ctx.Err() != nil:
		return outcomeStopped, 0
	case err != nil:
		a.cfg.Metrics.Retried(RetryStore)
		return outcomeRetried, a.schedule.Wait(failures)
	case !ok:
		return outcomeIdle, 0
	}
	var answer verdict
	var reason string
	if req, err := a.signed(e); err != nil {
		// Not a record this holder may send: left by another build, or not
		// what Append stored. It leaves the outbox, counted, unsent.
		answer, reason = verdictDropped, DropInvalid
	} else {
		answer, reason = a.send(ctx, req)
	}
	switch answer {
	case verdictAccepted, verdictDropped:
		// The Plane answered, or the record is not one to send: it leaves the
		// outbox even when the stop began meanwhile, on a context of its own.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), EnqueueTimeout)
		defer cancel()
		if a.cfg.Store.Delete(dctx, e.Seq) == nil {
			if answer == verdictAccepted {
				a.cfg.Metrics.Sent()
			} else {
				a.cfg.Metrics.Dropped(reason, 1)
			}
		}
		return outcomeLeft, 0
	}
	if ctx.Err() != nil {
		// Cut off at stop: no answer is not the Plane's. The claim lapses
		// after its lease, and a durable outbox sends it at the next start.
		return outcomeStopped, 0
	}
	retryIn = a.schedule.Wait(failures)
	a.cfg.Metrics.Retried(reason)
	_ = a.cfg.Store.Defer(ctx, e.Seq, now.Add(retryIn), reason)
	return outcomeRetried, retryIn
}

type verdict int

const (
	verdictAccepted verdict = iota
	verdictDropped
	verdictRetry
)

// signed is a queued record's append request, signed now with the key the
// gateway runs with. The record is read back exactly as Append stored it
// (one record, no member it does not know, under the row's own recordId) and
// checked again, so the outbox never sends what the guard would refuse.
func (a *Appender) signed(e Entry) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(e.Body))
	dec.DisallowUnknownFields()
	var r shnsdk.AuditRecord
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errors.New("auditappend: a queued row holds more than one record")
	}
	if r.RecordID != e.RecordID {
		return nil, errors.New("auditappend: a queued record's recordId is not its row's")
	}
	if err := Check(r, a.cfg.Holder); err != nil {
		return nil, err
	}
	return a.request(r)
}

// request is r's append request, signed by the holder key over its signable
// content.
func (a *Appender) request(r shnsdk.AuditRecord) ([]byte, error) {
	sig := ed25519.Sign(a.cfg.SignPriv, shnsdk.SignableContent(r))
	return json.Marshal(shnsdk.AuditAppendRequest{AuditRecord: r, Signatures: []string{base64.StdEncoding.EncodeToString(sig)}})
}

// send posts one signed append request and classifies the Plane's answer.
func (a *Appender) send(ctx context.Context, body []byte) (verdict, string) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Endpoint+"/append", bytes.NewReader(body))
	if err != nil {
		return verdictRetry, RetryUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return verdictRetry, RetryUnreachable
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return classify(resp.StatusCode, answer)
}

// classify maps the Plane's answer to /append to what
// the outbox does with the record.
func classify(status int, answer []byte) (verdict, string) {
	switch {
	case status >= 200 && status < 300:
		return verdictAccepted, ""
	case status == http.StatusUnauthorized:
		var a struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &a) == nil && a.Error == "unknown sender" {
			return verdictRetry, RetryUnknownSender
		}
		return verdictDropped, DropRefused
	case status == http.StatusConflict:
		return verdictDropped, DropConflict
	case status == http.StatusBadRequest, status == http.StatusForbidden, status == http.StatusRequestEntityTooLarge:
		return verdictDropped, DropRefused
	}
	return verdictRetry, RetryUnavailable
}

// Close ends the appender at stop, after Run has returned. An in-memory
// outbox tries each record due, within DrainLimit, and counts what is left
// as dropped; a durable one keeps its records for the next start.
//
// The count of what is left is known only once the drain ends, so before it
// starts Close reports how many records are queued, as the queue's depth and
// in the log. A gateway killed during the drain then still leaves that count:
// records queued, and no result, which reads as unknown and never as none
// lost.
func (a *Appender) Close() {
	if a.cfg.Store.Durable() {
		return
	}
	queued := 0
	if n, err := a.cfg.Store.Depth(context.Background()); err == nil {
		queued = n
	}
	if queued > 0 {
		a.cfg.Metrics.Depth(queued)
		if a.EndpointKnown() {
			a.logf("gateway: audit: stopping with %d records in the in-memory outbox; sending them for up to %s", queued, DrainLimit)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), DrainLimit)
	defer cancel()
	if a.EndpointKnown() {
		for failures := 1; ctx.Err() == nil; {
			out, _ := a.sendOne(ctx, failures)
			if out == outcomeIdle || out == outcomeStopped {
				break
			}
			if out == outcomeRetried {
				failures++
			}
		}
	}
	left, err := a.cfg.Store.Depth(context.Background())
	if err == nil && left > 0 {
		a.cfg.Metrics.Dropped(DropStopped, left)
	}
	switch {
	case queued == 0:
	case err != nil:
		// What is left is unknown, never none.
		a.logf("gateway: audit: stopped: %d records were queued; the outbox could not be read after its drain, so what was lost is unknown", queued)
	default:
		a.logf("gateway: audit: stopped: %d of %d records sent or refused (each counted); %d left unsent, counted dropped{stopped}", max(queued-left, 0), queued, left)
	}
}

func (a *Appender) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}
