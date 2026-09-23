package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// Nonconformance states where a peer's message departs from what the
	// operation it answers declares, in the peer's own terms — what it sent and
	// what the definition names. It is evidence, not a verdict: the message was
	// still read and still relayed, and nothing here changed a byte of it. A
	// conformant message carries none.
	Nonconformance []string `json:"nonconformance,omitempty"`
}

type certificationJob struct {
	input    *CheckInput
	authored *authoredValidationTarget
	payload  []byte
	queued   time.Time
	sequence uint64
	reserved int
}
type certificationWorker struct {
	wake                         chan struct{}
	mu                           sync.Mutex
	queue                        []certificationJob
	ctx                          context.Context
	cancel                       context.CancelFunc
	done                         chan struct{}
	changed                      chan struct{}
	accepted, completed, dropped uint64
	closed                       bool
	findings                     []ConformanceFinding
}

// DisableCertificationForTest leaves exchanges intact while suppressing evidence
// collection. It is not an operator configuration or a routing-lane switch.
func DisableCertificationForTest(c *Config) { c.certificationDisabled = true }

// CloseCertificationClients releases every certification client: a gated
// client's background qualification loop is stopped and joined, and each
// client's connection pool is released. New retires these legacy dedicated
// clients; embedders that construct them without New must close them themselves.
func CloseCertificationClients(clients map[string]shnsdk.Validator) {
	for _, v := range clients {
		switch c := v.(type) {
		case *shnsdk.OperationValidator:
			if c.Client != nil {
				c.Client.CloseIdleConnections()
			}
		case interface{ Close() }:
			c.Close() // a gated client: stops its background qualification, releases its pool
		case interface{ CloseIdleConnections() }:
			c.CloseIdleConnections()
		}
	}
}

func (g *Gateway) startCertification() {
	CloseCertificationClients(g.cfg.CertificationValidatorsByLine)
	if g.cfg.certificationDisabled || g.policy().Level() == EnforcementNone || g.policy().Level() == EnforcementStrict {
		// No worker will own the clients: stop any gated loop now.
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &certificationWorker{queue: make([]certificationJob, 0, certificationQueueCapacity), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	g.certification = w
	go g.runCertification(w)
}

// Close cancels queued work and returns within the collection bound. Callback
// completion remains independently observable through WaitObserverCompletion.
// Owners stop serving before Close. A custom checker must honor context.
func (g *Gateway) Close() error {
	g.closeObserver()
	w := g.certification
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closed = true
	w.cancel()
	for _, job := range w.queue {
		g.observationMemory.release(job.reserved)
		w.dropped++
	}
	w.queue = nil
	close(w.changed)
	w.changed = make(chan struct{})
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-time.After(certificationCollectionTimeout):
		return context.DeadlineExceeded
	}
}

func (g *Gateway) enqueueObservation(job certificationJob) bool {
	w := g.certification
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || len(job.payload) > observationMessageLimit || len(w.queue) == certificationQueueCapacity || !g.observationMemory.reserve(len(job.payload)) {
		w.dropped++
		return false
	}
	job.reserved = len(job.payload)
	job.payload = bytes.Clone(job.payload)
	if job.input != nil {
		in := *job.input
		in.Body = job.payload
		in.decoded = nil
		in.evidence = nil
		in.Exchange.boundary = append([]BoundaryCompletion(nil), in.Exchange.boundary...)
		job.input = &in
	}
	if job.queued.IsZero() {
		job.queued = time.Now()
	}
	w.accepted++
	job.sequence = w.accepted
	w.queue = append(w.queue, job)
	w.signalLocked()
	return true
}

func (g *Gateway) runCertification(w *certificationWorker) {
	defer close(w.done)
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return
		}
		if len(w.queue) == 0 {
			w.mu.Unlock()
			select {
			case <-w.wake:
			case <-w.ctx.Done():
			}
			continue
		}
		job := w.queue[0]
		copy(w.queue, w.queue[1:])
		w.queue[len(w.queue)-1] = certificationJob{}
		w.queue = w.queue[:len(w.queue)-1]
		w.mu.Unlock()
		func() {
			defer func() {
				if recover() != nil {
					g.recordObservation(w, ConformanceFinding{Kind: ConformanceObservedEvent, State: CheckUnavailable, Action: "not_enforced", Rule: "observation", Issues: []string{"checker_panic"}})
				}
			}()
			g.collectObservation(w, job)
		}()
		g.observationMemory.release(job.reserved)
		w.mu.Lock()
		w.completed = job.sequence
		close(w.changed)
		w.changed = make(chan struct{})
		w.mu.Unlock()
	}
}

// External diagnostic strings can contain entire foreign resources. Retain only
// authored summaries, with one fixed-size entry regardless of issue count/size.
func certificationIssueMetadata(issues []string) []string {
	if len(issues) == 0 {
		return []string{}
	}
	return []string{fmt.Sprintf("validator_issues count=%d", len(issues))}
}

// CertificationEvidenceForTest is retained for source compatibility. Native
// observations no longer probe candidate IG lines or infer a source line.
// Use ConformanceObservationsForTest for the actual registry findings.
func (g *Gateway) CertificationEvidenceForTest() []CertificationEvidence { return nil }

// ConformanceObservationsForTest returns copied metadata and dropped job count.
// An empty set is not a certification claim, including at none.
func (g *Gateway) ConformanceObservationsForTest() ([]ConformanceFinding, uint64) {
	w := g.certification
	if w == nil {
		return nil, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]ConformanceFinding(nil), w.findings...)
	for i := range out {
		out[i].Issues = append([]string(nil), out[i].Issues...)
		out[i].CheckIssues = append([]CheckIssue(nil), out[i].CheckIssues...)
	}
	return out, w.dropped
}

// FlushCertificationForTest waits for accepted jobs preceding its barrier,
// including cooperative observer delivery. Cancellation is reported explicitly.
func (g *Gateway) FlushCertificationForTest(ctx context.Context) error {
	if err := g.waitCertification(ctx); err != nil {
		return err
	}
	return g.waitObserver(ctx)
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
	for w.completed < barrier && !w.closed {
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		w.mu.Lock()
	}
	defer w.mu.Unlock()
	if w.dropped > 0 || w.completed < barrier {
		return errors.New("observation incomplete")
	}
	return nil
}

// NewCertificationOperationValidator uses a dedicated bounded transport. The
// SDK's single decoder owns content interpretation and private response capture.
func NewCertificationOperationValidator(endpoint string) *shnsdk.OperationValidator {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: certificationCandidateTimeout, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: certificationCandidateTimeout, ResponseHeaderTimeout: certificationCandidateTimeout}
	return &shnsdk.OperationValidator{BaseURL: endpoint, Client: &http.Client{Transport: transport, Timeout: certificationCandidateTimeout}}
}

// signalLocked wakes the sole worker without blocking an exchange handler.
func (w *certificationWorker) signalLocked() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
