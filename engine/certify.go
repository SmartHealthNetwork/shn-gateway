package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const (
	certificationQueueCapacity     = 32
	certificationRingCapacity      = 256
	certificationCandidateTimeout  = 2 * time.Second
	certificationCollectionTimeout = 6 * time.Second
	certificationQueueMaxAge       = 30 * time.Second
)

// LaneVerdict is an observational result, never a routing readiness assertion.
type LaneVerdict struct {
	Line    string   `json:"line"`
	Profile string   `json:"profile"`
	State   string   `json:"state"`
	Valid   bool     `json:"valid"`
	Issues  []string `json:"issues"`
	Error   string   `json:"error"`
}

// CertificationEvidence records metadata about immutable bytes at a foreign
// content seam. Only valid verdicts contribute to Certified; no decision reads it.
type CertificationEvidence struct {
	Mode          string        `json:"mode"`
	LegType       string        `json:"legType"`
	Direction     string        `json:"direction"`
	Seam          string        `json:"seam"`
	CorrelationID string        `json:"correlationId"`
	Species       string        `json:"species"`
	PayloadSHA256 string        `json:"payloadSHA256"`
	Candidates    []string      `json:"candidates"`
	Verdicts      []LaneVerdict `json:"verdicts"`
	Certified     []string      `json:"certified"`
	SourceLine    string        `json:"sourceLine"`
	TargetLine    string        `json:"targetLine"`
}

type certificationJob struct {
	evidence CertificationEvidence
	payload  []byte
	queued   time.Time
	sequence uint64
}
type certificationNotice struct {
	evidence CertificationEvidence
	sequence uint64
}

type certificationWorker struct {
	notices              []certificationNotice
	wake                 chan struct{}
	droppedNotifications uint64
	mu                   sync.Mutex
	queue                []certificationJob
	ctx                  context.Context
	cancel               context.CancelFunc
	done                 chan struct{}
	changed              chan struct{}
	accepted, completed  uint64
	closed               bool
	ring                 []CertificationEvidence
	validators           map[string]shnsdk.Validator
}

// DisableCertificationForTest leaves exchanges intact while suppressing evidence
// collection. It is not an operator configuration or a routing-lane switch.
func DisableCertificationForTest(c *Config) { c.certificationDisabled = true }

func (g *Gateway) startCertification() {
	if g.cfg.certificationDisabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &certificationWorker{queue: make([]certificationJob, 0, certificationQueueCapacity), notices: make([]certificationNotice, 0, certificationRingCapacity), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}), validators: make(map[string]shnsdk.Validator)}
	for line, v := range g.cfg.CertificationValidatorsByLine {
		w.validators[line] = v
	}
	g.certification = w
	go g.runCertification(w)
}

// Close cancels observation HTTP work and joins the single owned worker.
// Observer callbacks are cooperative: they must return promptly. Owners stop
// serving requests before closing and release callback barriers before joining.
func (g *Gateway) Close() error {
	w := g.certification
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	<-w.done
	return nil
}

func (g *Gateway) enqueueCertification(job certificationJob) {
	w := g.certification
	if w == nil {
		return
	}
	e := job.evidence
	e.Mode = "evidence"
	e.Species = detectSpecies(job.payload)
	e.Candidates = candidateOrder(e.Species, job.payload)
	if len(e.Candidates) == 0 {
		return
	}
	hash := sha256.Sum256(job.payload)
	e.PayloadSHA256 = hex.EncodeToString(hash[:])
	e.Certified = []string{}
	e.Verdicts = []LaneVerdict{}
	if _, ok := shnsdk.PASLineDef(e.TargetLine); !ok {
		e.TargetLine = ""
	}
	job.evidence = e
	w.mu.Lock()
	defer w.mu.Unlock()
	reason := ""
	switch {
	case w.closed:
		reason = "certification closed"
	case len(job.payload) > shnsdk.MaxRequestBytes:
		reason = "payload exceeds observation limit"
	case len(w.queue) == cap(w.queue):
		reason = "certification queue full"
	}
	if reason != "" {
		e = certificationUnavailable(e, "unavailable", reason)
		if !w.closed {
			if len(w.notices) < cap(w.notices) {
				w.accepted++
				w.notices = append(w.notices, certificationNotice{evidence: e, sequence: w.accepted})
				w.signalLocked()
			} else {
				w.droppedNotifications++
				for i := range e.Verdicts {
					e.Verdicts[i].Error += fmt.Sprintf("; observer notifications dropped=%d", w.droppedNotifications)
				}
			}
		}
		w.storeLocked(e)
		return
	}
	job.payload = bytes.Clone(job.payload)
	if job.queued.IsZero() {
		job.queued = time.Now()
	}
	w.accepted++
	job.sequence = w.accepted
	w.queue = append(w.queue, job)
	w.signalLocked()
}

func certificationUnavailable(e CertificationEvidence, state, reason string) CertificationEvidence {
	for _, line := range e.Candidates {
		profile, _ := profileFor(e.Species, line, e.LegType)
		e.Verdicts = append(e.Verdicts, LaneVerdict{Line: line, Profile: profile, State: state, Error: reason, Issues: []string{}})
	}
	return e
}
func (w *certificationWorker) storeLocked(e CertificationEvidence) {
	if len(w.ring) == certificationRingCapacity {
		copy(w.ring, w.ring[1:])
		w.ring[len(w.ring)-1] = e
	} else {
		w.ring = append(w.ring, e)
	}
}
func (g *Gateway) runCertification(w *certificationWorker) {
	defer close(w.done)
	defer func() {
		for _, v := range w.validators {
			if o, ok := v.(*shnsdk.OperationValidator); ok && o.Client != nil {
				o.Client.CloseIdleConnections()
			}
		}
	}()
	for {
		var job certificationJob
		var notice certificationNotice
		w.mu.Lock()
		if len(w.queue) == 0 && len(w.notices) == 0 {
			closed := w.closed
			w.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-w.wake:
			case <-w.ctx.Done():
			}
			continue
		}
		isNotice := len(w.notices) > 0 && (len(w.queue) == 0 || w.notices[0].sequence < w.queue[0].sequence)
		if isNotice {
			notice = w.notices[0]
			copy(w.notices, w.notices[1:])
			w.notices[len(w.notices)-1] = certificationNotice{}
			w.notices = w.notices[:len(w.notices)-1]
		} else {
			job = w.queue[0]
			copy(w.queue, w.queue[1:])
			w.queue[len(w.queue)-1] = certificationJob{}
			w.queue = w.queue[:len(w.queue)-1]
		}
		w.mu.Unlock()
		e, sequence := notice.evidence, notice.sequence
		if !isNotice {
			e = g.collectCertification(w, job)
			sequence = job.sequence
			w.mu.Lock()
			w.storeLocked(e)
			w.mu.Unlock()
		}
		raw, _ := json.Marshal(e)
		log.Printf("certify: %s", raw)
		g.observe(ObserverEvent{Kind: "leg.certified", LegType: e.LegType, Direction: e.Direction, CorrelationID: e.CorrelationID, Detail: string(raw)})
		w.mu.Lock()
		w.completed = sequence
		close(w.changed)
		w.changed = make(chan struct{})
		w.mu.Unlock()
	}
}
func (g *Gateway) collectCertification(w *certificationWorker, job certificationJob) CertificationEvidence {
	e := job.evidence
	deadline := job.queued.Add(certificationQueueMaxAge)
	if now := time.Now().Add(certificationCollectionTimeout); now.Before(deadline) {
		deadline = now
	}
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	for _, line := range e.Candidates {
		profile, ok := profileFor(e.Species, line, e.LegType)
		v := LaneVerdict{Line: line, Profile: profile, Issues: []string{}}
		switch {
		case ctx.Err() != nil:
			v.State = "expired"
			v.Error = ctx.Err().Error()
		case !ok:
			v.State = "unavailable"
			v.Error = "unsupported certification profile"
		case w.validators[line] == nil:
			v.State = "unavailable"
			v.Error = "certification validator unavailable"
		default:
			candidate, cancel := context.WithTimeout(ctx, certificationCandidateTimeout)
			result, err := w.validators[line].Validate(candidate, job.payload, profile)
			contextErr := candidate.Err()
			cancel()
			v.Issues = certificationIssueMetadata(result.Issues)
			switch {
			case contextErr != nil:
				v.State = "expired"
				v.Error = contextErr.Error()
			case err != nil:
				v.State = "unavailable"
				v.Error = certificationErrorMetadata(err)
			case result.Valid:
				v.State = "valid"
				v.Valid = true
				e.Certified = append(e.Certified, line)
			default:
				v.State = "invalid"
			}
		}
		e.Verdicts = append(e.Verdicts, v)
	}
	e.SourceLine = certificationSource(e.Certified, e.TargetLine)
	return e
}

// External diagnostic strings can contain entire foreign resources. Retain only
// authored summaries, with one fixed-size entry regardless of issue count/size.
func certificationIssueMetadata(issues []string) []string {
	if len(issues) == 0 {
		return []string{}
	}
	hash := sha256.New()
	var size uint64
	for _, issue := range issues {
		size += uint64(len(issue))
		fmt.Fprintf(hash, "%d:", len(issue))
		io.WriteString(hash, issue)
	}
	return []string{fmt.Sprintf("validator issues count=%d bytes=%d sha256=%x", len(issues), size, hash.Sum(nil))}
}
func certificationErrorMetadata(err error) string {
	raw := err.Error()
	hash := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("validator error bytes=%d sha256=%x", len(raw), hash)
}

// CertificationEvidenceForTest returns the oldest-first bounded ring with all
// nested slices copied; callers cannot mutate retained completion records.
func (g *Gateway) CertificationEvidenceForTest() []CertificationEvidence {
	w := g.certification
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]CertificationEvidence, len(w.ring))
	for i, e := range w.ring {
		out[i] = e
		out[i].Candidates = append([]string{}, e.Candidates...)
		out[i].Certified = append([]string{}, e.Certified...)
		out[i].Verdicts = append([]LaneVerdict{}, e.Verdicts...)
		for j := range e.Verdicts {
			out[i].Verdicts[j].Issues = append([]string{}, e.Verdicts[j].Issues...)
		}
	}
	return out
}

// FlushCertificationForTest waits for accepted jobs preceding its barrier,
// including cooperative observer delivery. Cancellation is reported explicitly.
func (g *Gateway) FlushCertificationForTest(ctx context.Context) error {
	return g.waitCertification(ctx)
}

func (g *Gateway) waitCertification(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w := g.certification
	if w == nil {
		return nil
	}
	w.mu.Lock()
	barrier := w.accepted
	for w.completed < barrier {
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		w.mu.Lock()
	}
	w.mu.Unlock()
	return nil
}

// NewCertificationOperationValidator uses a dedicated bounded transport. Raw
// server execution failures remain unavailable even when their OperationOutcome
// parses as an invalid result under the separate routing client's contract.
func NewCertificationOperationValidator(endpoint string) *shnsdk.OperationValidator {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: certificationCandidateTimeout, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: certificationCandidateTimeout, ResponseHeaderTimeout: certificationCandidateTimeout}
	return &shnsdk.OperationValidator{BaseURL: endpoint, Client: &http.Client{Transport: certificationTransport{transport}, Timeout: certificationCandidateTimeout}}
}

type certificationTransport struct{ inner *http.Transport }

func (t certificationTransport) CloseIdleConnections() { t.inner.CloseIdleConnections() }
func (t certificationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, shnsdk.MaxResponseBytes+1))
	response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(raw) > shnsdk.MaxResponseBytes {
		return nil, errors.New("certification response exceeds limit")
	}
	if response.StatusCode >= 500 {
		return nil, &certificationHTTPError{status: response.StatusCode, reason: "server execution unavailable", raw: bytes.Clone(raw)}
	}
	var oo struct {
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Severity    string `json:"severity"`
			Diagnostics string `json:"diagnostics"`
		} `json:"issue"`
	}
	if json.Unmarshal(raw, &oo) != nil || oo.ResourceType != "OperationOutcome" || oo.Issue == nil {
		return nil, &certificationHTTPError{status: response.StatusCode, reason: "malformed OperationOutcome", raw: bytes.Clone(raw)}
	}
	for _, issue := range oo.Issue {
		switch issue.Severity {
		case "fatal", "error", "warning", "information":
		default:
			return nil, &certificationHTTPError{status: response.StatusCode, reason: "malformed issue severity", raw: bytes.Clone(raw)}
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// certificationTargetKey carries a write-only observation of the selected token
// through the existing dispatch; the collector never supplies a routing input.
type certificationTargetKey struct{}

func (g *Gateway) certificationPair(leg, seam, corr, target string, request, response []byte) {
	for _, part := range []struct {
		direction string
		payload   []byte
	}{{"request", request}, {"response", response}} {
		if len(part.payload) > 0 {
			g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: leg, Seam: seam, CorrelationID: corr, TargetLine: target, Direction: part.direction}, payload: part.payload})
		}
	}
}

// certificationHTTPError keeps bounded raw execution evidence separate from its
// safe string representation. Wrapping the error cannot expose response bytes.
type certificationHTTPError struct {
	status int
	reason string
	raw    []byte
}

func (e *certificationHTTPError) Error() string {
	hash := sha256.Sum256(e.raw)
	return fmt.Sprintf("certification HTTP %d %s (response bytes=%d sha256=%x)", e.status, e.reason, len(e.raw), hash)
}

// nativeCertificationCapture belongs to one synchronous responder call. It
// captures the final POST attempt before any polling or terminal assembly.
type nativeCertificationCapture struct {
	request, response []byte
	attempted         bool
}
type nativeCertificationKey struct{}

// signalLocked wakes the sole worker without blocking an exchange handler.
func (w *certificationWorker) signalLocked() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
