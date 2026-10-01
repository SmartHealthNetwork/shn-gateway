package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Batched delivery: with PublisherConfig.BatchURL set and the ingest
// saying on a heartbeat's answer that it takes batches, the publisher posts
// up to maxBatchEvents due events in one signed request and the ingest
// answers each one. One event per request capped a publisher at one ingest
// round trip per event, below a busy gateway's emit rate, so its queue filled
// and dropped. A sink that never says so (a participant's own sink, an older
// ingest) gets one event per request, as before. A batch answered 404 or 405,
// or with a success without a result for each event, falls back to one event
// per request until a heartbeat's answer says batches again.
const (
	// maxBatchEvents and maxBatchCost bound one request: by events, and by
	// their retained cost (eventCost). An event costlier than maxBatchCost
	// alone is sent alone.
	maxBatchEvents = 256
	maxBatchCost   = 1 << 20
	// batchTimeout bounds one batch request, within its events' ownership.
	batchTimeout = 5 * time.Second
	// maxBatchAnswer bounds the ingest's answer: a result per event.
	maxBatchAnswer = 256 << 10
)

// The ingest's per-event statuses in a batch answer.
const (
	batchAccepted        = "accepted"
	batchBindingPending  = "binding_pending"
	batchScopeIgnored    = "scope_ignored"
	batchKindNotAdmitted = "kind_not_admitted"
	batchInvalid         = "invalid"
	batchConflict        = "conflict"
	batchUnavailable     = "unavailable"
)

type batchRequest struct {
	Events []json.RawMessage `json:"events"`
}

type batchAnswer struct {
	Results []batchResult `json:"results"`
}

type batchResult struct {
	Sequence uint64 `json:"sequence"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
}

// batcher is a publisher's batch delivery state across requests.
type batcher struct {
	url string
	// maxEvents halves when the ingest answers 413 for a batch of more than
	// one event, and doubles back, up to maxBatchEvents, with each batch
	// settled in full.
	maxEvents int
	// backoff is the wait before events the ingest could not take are due
	// again; it grows with each such answer and resets on a full success.
	backoff time.Duration
}

// sent is one event in a batch request, with its ownership deadline.
type sent struct {
	event    Event
	deadline time.Time
}

// publishBatch sends the events takeBatch took (the due ones, in queue
// order) in one request and settles each by the ingest's answer; stop settles
// those left unanswered when ctx ends. It reports
// whether the sink does not speak the batch protocol, in which case every
// event it took is due again for single-event delivery.
func (b *batcher) publishBatch(ctx context.Context, q *Queue, cfg PublisherConfig, events []Event, stop func(uint64)) (single bool) {
	now := cfg.Clock()
	q.mu.Lock()
	maxRetained := q.limits.MaxBytes + 2*int64(maxPublisherIdentityBytes)
	q.mu.Unlock()
	var batch []sent
	var raws []json.RawMessage
	for _, e := range events {
		e.Source, e.Incarnation = cfg.Source, cfg.Incarnation
		deadline := q.ownershipDeadline(e.Sequence, now)
		if !deadline.After(now) {
			q.dropFor(e.Sequence, expired)
			continue
		}
		if eventCost(e) > maxRetained {
			q.dropFor(e.Sequence, oversized)
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			q.dropFor(e.Sequence, unencodable)
			continue
		}
		batch = append(batch, sent{event: e, deadline: deadline})
		raws = append(raws, raw)
	}
	if len(batch) == 0 {
		return false
	}
	body, err := json.Marshal(batchRequest{Events: raws})
	if err != nil { // the events each encoded: unreachable, but never silent
		for _, s := range batch {
			q.dropFor(s.event.Sequence, unencodable)
		}
		return false
	}
	latest := batch[0].deadline
	for _, s := range batch[1:] {
		if s.deadline.After(latest) {
			latest = s.deadline
		}
	}
	result, err := post(ctx, cfg, b.url, body, min(batchTimeout, latest.Sub(cfg.Clock())), maxBatchAnswer)
	var answer batchAnswer
	answered := err == nil && result.status >= 200 && result.status < 300 && result.complete &&
		json.Unmarshal(result.body, &answer) == nil && answers(answer, batch)
	if ctx.Err() != nil && !answered {
		// Stopped with the batch unsettled; one the ingest answered is
		// settled below, as single delivery acknowledges before stopping.
		for _, s := range batch {
			stop(s.event.Sequence)
		}
		return false
	}
	switch {
	case err != nil:
		b.retry(q, cfg, batch)
	case result.status == http.StatusNotFound || result.status == http.StatusMethodNotAllowed:
		b.again(q, batch)
		return true
	case result.status == http.StatusRequestEntityTooLarge && len(batch) == 1:
		// One event the ingest will never take: no smaller batch holds it.
		q.dropFor(batch[0].event.Sequence, oversized)
	case result.status == http.StatusRequestEntityTooLarge:
		b.maxEvents = max(1, len(batch)/2)
		b.again(q, batch)
	case result.status >= 200 && result.status < 300:
		if !answered {
			// A success without a result for each event is a sink that
			// does not speak the batch protocol.
			b.again(q, batch)
			return true
		}
		b.settle(q, cfg, batch, answer.Results)
	default:
		b.retry(q, cfg, batch)
	}
	return false
}

// answers reports whether the answer holds one result for each event, in
// order.
func answers(a batchAnswer, batch []sent) bool {
	if len(a.Results) != len(batch) {
		return false
	}
	for i, r := range a.Results {
		if r.Sequence != batch[i].event.Sequence {
			return false
		}
	}
	return true
}

// settle settles each event by its result.
func (b *batcher) settle(q *Queue, cfg PublisherConfig, batch []sent, results []batchResult) {
	var later []sent
	for i, r := range results {
		e := batch[i].event
		switch r.Status {
		case batchAccepted:
			q.Acknowledge(e.Sequence)
		case batchBindingPending:
			q.deferredBinding(e.Sequence, cfg.Clock(), cfg.Jitter)
		case batchScopeIgnored:
			q.discard(e.Sequence)
		case batchKindNotAdmitted:
			// The ingest never admits this kind from this publisher: withhold
			// it until it is probed again. Only this status says so; a
			// decline of one event says nothing about its kind.
			q.discard(e.Sequence)
			q.suppressKind(e.Kind)
		case batchInvalid, batchConflict:
			q.dropFor(e.Sequence, invalid)
		default: // unavailable, or a status this publisher does not know
			later = append(later, batch[i])
		}
	}
	if len(later) == 0 {
		b.backoff = 0
		b.maxEvents = min(maxBatchEvents, 2*b.maxEvents)
		return
	}
	b.retry(q, cfg, later)
}

// retry makes events the ingest did not take due again after the backoff,
// each until its own ownership deadline. A test event the ingest did not
// take is not retried (one held for its binding is deferred, as any event).
func (b *batcher) retry(q *Queue, cfg PublisherConfig, batch []sent) {
	b.backoff = nextRetryWait(b.backoff)
	now := cfg.Clock()
	due := now.Add(cfg.Jitter(b.backoff))
	for _, s := range batch {
		switch {
		case s.event.Kind == "test":
			q.dropFor(s.event.Sequence, testNotAccepted)
		case !s.deadline.After(now):
			q.dropFor(s.event.Sequence, expired)
		default:
			at := due
			if s.deadline.Before(at) {
				at = s.deadline
			}
			q.retryAt(s.event.Sequence, at)
		}
	}
}

// again makes events due at once, without backoff: the request did not reach
// an ingest that could judge them.
func (b *batcher) again(q *Queue, batch []sent) {
	for _, s := range batch {
		q.retryAt(s.event.Sequence, time.Time{})
	}
}

// post sends a signed body and reads at most limit bytes of the answer, within
// timeout.
func post(ctx context.Context, cfg PublisherConfig, endpoint string, raw []byte, timeout time.Duration, limit int) (publishResult, error) {
	if timeout <= 0 {
		return publishResult{}, errEvidenceExpired
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return publishResult{}, err
	}
	ts := cfg.Clock().UTC().Format(time.RFC3339Nano)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvidenceSource, cfg.Source)
	req.Header.Set(HeaderEvidenceIncarnation, cfg.Incarnation)
	req.Header.Set(HeaderEvidenceTime, ts)
	req.Header.Set(HeaderEvidenceSignature, Sign(cfg.Key, cfg.Source, cfg.Incarnation, ts, raw))
	reqCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()
	resp, err := cfg.Client.Do(req.WithContext(reqCtx))
	if err != nil {
		return publishResult{}, err
	}
	defer resp.Body.Close()
	return readAnswer(resp, limit)
}
