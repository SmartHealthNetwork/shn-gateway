package diagnostics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"time"
)

const (
	HeaderEvidenceSource      = "X-SHN-Evidence-Source"
	HeaderEvidenceIncarnation = "X-SHN-Evidence-Incarnation"
	HeaderEvidenceTime        = "X-SHN-Evidence-Time"
	HeaderEvidenceSignature   = "X-SHN-Evidence-Signature"
	// HeaderEvidenceBatch on an ingest's answer to a heartbeat says it takes
	// batches at the publisher's BatchURL.
	HeaderEvidenceBatch = "X-SHN-Evidence-Batch"
	// JSON escaping is at most six bytes per retained input byte. The factor
	// also covers Marshal's result and growth scratch concurrently; no event is
	// marshaled unless its conservative retained cost is within the queue bound.
	publisherTransientMultiplier = 16
	publisherEnvelopeBytes       = 4096
	maxPublisherIdentityBytes    = 4096
)

type PublisherConfig struct {
	Source      string
	Incarnation string
	URL         string
	HealthURL   string
	// Drain, when positive, bounds a final delivery after ctx ends: the
	// publisher keeps sending what is queued, events in flight included,
	// until the queue is empty or Drain has passed, then counts what is left
	// as stopped and sends a last heartbeat. Zero stops at once, leaving
	// queued events pending.
	Drain time.Duration
	// BatchURL, when set, is the ingest's batch endpoint. Once the ingest
	// answers a heartbeat with HeaderEvidenceBatch, due events are posted
	// there together (batch.go); until then, and to a sink that never says
	// so, one event per request. The first heartbeat is sent at once.
	BatchURL string
	Key      []byte
	Client   *http.Client
	// Clock and Wait must agree: a pending observation falls due by Clock,
	// and the publisher waits for it through Wait. A Clock that Wait never
	// advances leaves a pending observation waiting for good. Clock is also
	// the queue's clock for withheld kinds, read from emitting goroutines
	// with the queue locked: it must be safe for concurrent use and must not
	// call into the queue.
	Clock     func() time.Time
	Wait      func(context.Context, time.Duration) error
	Heartbeat time.Duration
	// Jitter spreads a retry wait of d over [d/2, d]. Nil means a source
	// seeded from the publisher's identity: publishers retrying against one
	// busy ingest never fall into step, and a publisher's schedule is
	// reproducible.
	Jitter func(time.Duration) time.Duration
}

// Retry waits: a failed attempt, or an observation the ingest holds as
// binding_pending, waits from minRetryWait doubling to maxRetryWait. A pending
// observation backs off too: re-posting it on a fixed short interval kept the
// ingest's one admission slot busy exactly when its prerequisite's publisher,
// at the cap, came back, so the prerequisite could not land.
const (
	minRetryWait = 50 * time.Millisecond
	maxRetryWait = 500 * time.Millisecond
)

// nextRetryWait doubles a wait up to maxRetryWait; zero starts at minRetryWait.
func nextRetryWait(d time.Duration) time.Duration {
	if d <= 0 {
		return minRetryWait
	}
	if d *= 2; d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

// seededJitter spreads d over [d/2, d] from a source seeded by seed.
func seededJitter(seed uint64) func(time.Duration) time.Duration {
	r := mathrand.New(mathrand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return func(d time.Duration) time.Duration {
		if d <= 1 {
			return d
		}
		half := d / 2
		return half + time.Duration(r.Int64N(int64(d-half)+1))
	}
}

type publishResult struct {
	status   int
	body     []byte
	complete bool
	// batches: the answer carried HeaderEvidenceBatch.
	batches bool
}

var errEvidenceExpired = errors.New("diagnostics: evidence publication expired")

func RunPublisher(ctx context.Context, q *Queue, cfg PublisherConfig) error {
	if q == nil {
		return errors.New("diagnostics: nil queue")
	}
	if err := validatePublisherURL(cfg.URL); err != nil {
		return err
	}
	if cfg.HealthURL == "" {
		cfg.HealthURL = cfg.URL
	} else if err := validatePublisherURL(cfg.HealthURL); err != nil {
		return err
	}
	if cfg.BatchURL != "" {
		if err := validatePublisherURL(cfg.BatchURL); err != nil {
			return err
		}
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitContext
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 5 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Incarnation == "" {
		var boot [16]byte
		if _, err := rand.Read(boot[:]); err != nil {
			return err
		}
		cfg.Incarnation = hex.EncodeToString(boot[:])
	}
	if len(cfg.Source) > maxPublisherIdentityBytes || len(cfg.Incarnation) > maxPublisherIdentityBytes {
		return errors.New("diagnostics: publisher identity exceeds bound")
	}
	q.setClock(cfg.Clock)
	if cfg.Jitter == nil {
		h := fnv.New64a()
		_, _ = h.Write([]byte(cfg.Source + "\x00" + cfg.Incarnation))
		cfg.Jitter = seededJitter(h.Sum64())
	}
	nextHeartbeat := cfg.Clock().Add(cfg.Heartbeat)
	if cfg.BatchURL != "" {
		// The first heartbeat's answer says whether the ingest takes batches.
		nextHeartbeat = cfg.Clock()
	}
	// batch is set while the ingest says it takes batches; a heartbeat
	// answer that says so starts it again after a fallback.
	var batch *batcher
	sendHeartbeat := func(ctx context.Context, ownershipDeadline time.Time) {
		health := q.Health(cfg.Clock())
		health.Source, health.Incarnation = cfg.Source, cfg.Incarnation
		raw, err := json.Marshal(health)
		if err == nil {
			result, err := publish(ctx, cfg, cfg.HealthURL, raw, ownershipDeadline)
			if err == nil && result.batches && result.status >= 200 && result.status < 300 && cfg.BatchURL != "" && batch == nil {
				batch = &batcher{url: cfg.BatchURL, maxEvents: maxBatchEvents}
			}
		}
		nextHeartbeat = cfg.Clock().Add(cfg.Heartbeat)
	}
	// stop settles an event whose delivery ctx ended: with a drain to come it
	// is due again for the drain, otherwise it is dropped as stopped.
	var draining bool
	var drainEnd time.Time
	stop := func(sequence uint64) {
		if cfg.Drain > 0 && !draining {
			q.retryAt(sequence, time.Time{})
			return
		}
		q.dropFor(sequence, stopped)
	}
	deliver := func(ctx context.Context) error {
		for {
			if draining && (q.empty() || !cfg.Clock().Before(drainEnd)) {
				return nil
			}
			untilHeartbeat := nextHeartbeat.Sub(cfg.Clock())
			if untilHeartbeat <= 0 {
				sendHeartbeat(ctx, time.Time{})
				continue
			}
			if batch != nil {
				events, due := q.takeBatch(cfg.Clock(), batch.maxEvents, maxBatchCost)
				if len(events) == 0 {
					// Nothing due: wait for the earliest pending event, a new
					// event, or the heartbeat.
					wait := untilHeartbeat
					if due > 0 {
						wait = min(due, untilHeartbeat)
					}
					if err := q.waitDue(ctx, cfg.Wait, wait); err != nil {
						return err
					}
					continue
				}
				if batch.publishBatch(ctx, q, cfg, events, stop) {
					batch = nil // the sink takes one event per request
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				continue
			}
			e, ok, due := q.take(cfg.Clock())
			if !ok && due > 0 {
				// Every queued event is a pending observation not yet due: wait
				// for the earliest, or a new event, or the heartbeat.
				if err := q.waitDue(ctx, cfg.Wait, min(due, untilHeartbeat)); err != nil {
					return err
				}
				continue
			}
			if !ok {
				nextCtx, stopNext := context.WithTimeout(ctx, untilHeartbeat)
				var err error
				e, err = q.Next(nextCtx)
				stopNext()
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					sendHeartbeat(ctx, time.Time{})
					continue
				}
				if err != nil {
					return err
				}
			}
			e.Source = cfg.Source
			e.Incarnation = cfg.Incarnation
			if e.Time.IsZero() {
				e.Time = cfg.Clock()
			}
			deadline := q.ownershipDeadline(e.Sequence, cfg.Clock())
			// expire drops the event, as expired, once its ownership window has
			// run out.
			expire := func() bool {
				if deadline.Sub(cfg.Clock()) > 0 {
					return false
				}
				q.dropFor(e.Sequence, expired)
				return true
			}
			backoff := minRetryWait
			for {
				if err := ctx.Err(); err != nil {
					stop(e.Sequence)
					return err
				}
				if expire() {
					break
				}
				if !cfg.Clock().Before(nextHeartbeat) {
					sendHeartbeat(ctx, deadline)
					if expire() {
						break
					}
				}
				q.mu.Lock()
				maxRetained := q.limits.MaxBytes + 2*int64(maxPublisherIdentityBytes)
				q.mu.Unlock()
				if eventCost(e) > maxRetained {
					q.dropFor(e.Sequence, oversized)
					break
				}
				raw, err := json.Marshal(e)
				if err != nil {
					q.dropFor(e.Sequence, unencodable)
					break
				}
				if expire() {
					break
				}
				result, err := publish(ctx, cfg, cfg.URL, raw, deadline)
				if err == nil {
					if result.status >= 200 && result.status < 300 {
						q.Acknowledge(e.Sequence)
						break
					}
					var scope struct {
						Code string `json:"code"`
					}
					coded := result.complete && json.Unmarshal(result.body, &scope) == nil
					if result.status == http.StatusConflict && coded && scope.Code == "binding_pending" {
						// A prerequisite can belong to this same source and be queued
						// later (ingress completes after sealing). Yield ownership,
						// not bytes or capacity, without renewing the bounded cap:
						// the event is due again after its binding wait, and the
						// events behind it are published meanwhile.
						q.deferredBinding(e.Sequence, cfg.Clock(), cfg.Jitter)
						break
					}
					if result.status == http.StatusUnprocessableEntity && coded && scope.Code == "scope_ignored" {
						q.discard(e.Sequence)
						break
					}
					// The ingest refused this event for good: it is malformed, or
					// it conflicts with an event the ingest already holds under its
					// sequence. Retrying it cannot succeed.
					if (result.status == http.StatusBadRequest && coded && scope.Code == "invalid_event") ||
						(result.status == http.StatusConflict && coded && scope.Code == "conflict") {
						q.dropFor(e.Sequence, invalid)
						break
					}
				}
				if err := ctx.Err(); err != nil {
					stop(e.Sequence)
					return err
				}
				if e.Kind == "test" {
					q.dropFor(e.Sequence, testNotAccepted)
					break
				}
				if expire() {
					break
				}
				wait := min(cfg.Jitter(backoff), deadline.Sub(cfg.Clock()))
				if err := cfg.Wait(ctx, wait); err != nil {
					stop(e.Sequence)
					return err
				}
				backoff = nextRetryWait(backoff)
			}
		}
	}
	err := deliver(ctx)
	if ctx.Err() == nil || cfg.Drain <= 0 {
		return err
	}
	// The final drain: deliver what is queued until it is empty or the drain
	// has passed, then count what is left and say so in a last heartbeat.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Drain)
	defer cancel()
	draining, drainEnd = true, cfg.Clock().Add(cfg.Drain)
	_ = deliver(drainCtx)
	q.dropRemaining(stopped)
	last, cancelLast := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancelLast()
	sendHeartbeat(last, time.Time{})
	return ctx.Err()
}

func validatePublisherURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("diagnostics: invalid publisher URL")
	}
	return nil
}

func publish(ctx context.Context, cfg PublisherConfig, endpoint string, raw []byte, ownershipDeadline time.Time) (publishResult, error) {
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
	timeout := time.Second
	if !ownershipDeadline.IsZero() {
		remaining := ownershipDeadline.Sub(cfg.Clock())
		if remaining <= 0 {
			return publishResult{}, errEvidenceExpired
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	reqCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()
	req = req.WithContext(reqCtx)
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return publishResult{}, err
	}
	defer resp.Body.Close()
	return readAnswer(resp, 4096)
}

// readAnswer reads at most limit bytes of an ingest's answer, noting whether
// that was all of it.
func readAnswer(resp *http.Response, limit int) (publishResult, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return publishResult{}, err
	}
	complete := len(body) <= limit
	if !complete {
		body = body[:limit]
	}
	return publishResult{status: resp.StatusCode, body: body, complete: complete, batches: resp.Header.Get(HeaderEvidenceBatch) != ""}, nil
}
func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
