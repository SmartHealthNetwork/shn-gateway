package diagnostics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	HeaderEvidenceSource      = "X-SHN-Evidence-Source"
	HeaderEvidenceIncarnation = "X-SHN-Evidence-Incarnation"
	HeaderEvidenceTime        = "X-SHN-Evidence-Time"
	HeaderEvidenceSignature   = "X-SHN-Evidence-Signature"
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
	Key         []byte
	Client      *http.Client
	Clock       func() time.Time
	Wait        func(context.Context, time.Duration) error
	Heartbeat   time.Duration
}
type publishResult struct {
	status   int
	body     []byte
	complete bool
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
	nextHeartbeat := cfg.Clock().Add(cfg.Heartbeat)
	sendHeartbeat := func(ownershipDeadline time.Time) {
		health := q.Health(cfg.Clock())
		health.Source, health.Incarnation = cfg.Source, cfg.Incarnation
		raw, err := json.Marshal(health)
		if err == nil {
			_, _ = publish(ctx, cfg, cfg.HealthURL, raw, ownershipDeadline)
		}
		nextHeartbeat = cfg.Clock().Add(cfg.Heartbeat)
	}
	for {
		untilHeartbeat := nextHeartbeat.Sub(cfg.Clock())
		if untilHeartbeat <= 0 {
			sendHeartbeat(time.Time{})
			continue
		}
		nextCtx, stopNext := context.WithTimeout(ctx, untilHeartbeat)
		e, err := q.Next(nextCtx)
		stopNext()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			sendHeartbeat(time.Time{})
			continue
		}
		if err != nil {
			return err
		}
		e.Source = cfg.Source
		e.Incarnation = cfg.Incarnation
		if e.Time.IsZero() {
			e.Time = cfg.Clock()
		}
		deadline := q.ownershipDeadline(e.Sequence, cfg.Clock())
		backoff := 50 * time.Millisecond
		for {
			if err := ctx.Err(); err != nil {
				q.Drop(e.Sequence)
				return err
			}
			remaining := deadline.Sub(cfg.Clock())
			if remaining <= 0 {
				q.Drop(e.Sequence)
				break
			}
			if !cfg.Clock().Before(nextHeartbeat) {
				sendHeartbeat(deadline)
				remaining = deadline.Sub(cfg.Clock())
				if remaining <= 0 {
					q.Drop(e.Sequence)
					break
				}
			}
			q.mu.Lock()
			maxRetained := q.limits.MaxBytes + 2*int64(maxPublisherIdentityBytes)
			q.mu.Unlock()
			if eventCost(e) > maxRetained {
				q.Drop(e.Sequence)
				break
			}
			raw, err := json.Marshal(e)
			if err != nil {
				q.Drop(e.Sequence)
				break
			}
			remaining = deadline.Sub(cfg.Clock())
			if remaining <= 0 {
				q.Drop(e.Sequence)
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
				if result.status == http.StatusConflict && result.complete && json.Unmarshal(result.body, &scope) == nil && scope.Code == "binding_pending" {
					// A prerequisite can belong to this same source and be queued
					// later (ingress completes after sealing). Yield ownership,
					// not bytes or capacity, without renewing the bounded cap.
					q.deferredBinding(e.Sequence)
					wait := 50 * time.Millisecond
					if left := deadline.Sub(cfg.Clock()); left < wait {
						wait = left
					}
					if wait > 0 {
						if err := cfg.Wait(ctx, wait); err != nil {
							q.Drop(e.Sequence)
							return err
						}
					}
					break
				}
				if result.status == http.StatusUnprocessableEntity && result.complete && json.Unmarshal(result.body, &scope) == nil && scope.Code == "scope_ignored" {
					q.discard(e.Sequence)
					break
				}
			}
			if e.Kind == "test" {
				q.Drop(e.Sequence)
				break
			}
			remaining = deadline.Sub(cfg.Clock())
			if remaining <= 0 {
				q.Drop(e.Sequence)
				break
			}
			wait := backoff
			if wait > remaining {
				wait = remaining
			}
			if err := cfg.Wait(ctx, wait); err != nil {
				q.Drop(e.Sequence)
				return err
			}
			if backoff < 500*time.Millisecond {
				backoff *= 2
				if backoff > 500*time.Millisecond {
					backoff = 500 * time.Millisecond
				}
			}
		}
	}
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
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if readErr != nil {
		return publishResult{}, readErr
	}
	complete := len(body) <= 4096
	if !complete {
		body = body[:4096]
	}
	return publishResult{status: resp.StatusCode, body: body, complete: complete}, nil
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
