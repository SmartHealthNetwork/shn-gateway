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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const (
	certificationQueueCapacity    = 32
	certificationRingCapacity     = 256
	certificationCandidateTimeout = 2 * time.Second
	// certificationClientTimeout bounds the certification client's own dial,
	// handshake, response header and whole request. It is longer than a
	// candidate's time, so a lane that does not answer is always cut off by
	// the candidate's context first, and the certification is recorded
	// expired, never by the client's own timer racing it.
	certificationClientTimeout     = certificationCandidateTimeout + 500*time.Millisecond
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

// CloseCertificationClients releases every certification client: a gated
// client's background qualification loop is stopped and joined, and each
// client's connection pool is released. It is what the worker's shutdown runs,
// and what an embedder that built clients but no worker must run.
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
	// At none no conformance check runs, so no certification evidence is
	// gathered either.
	if g.cfg.certificationDisabled || !g.policy().RunsKind(KindFHIRIngress) {
		// No worker will own the clients: stop any gated loop now.
		CloseCertificationClients(g.cfg.CertificationValidatorsByLine)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &certificationWorker{queue: make([]certificationJob, 0, certificationQueueCapacity), notices: make([]certificationNotice, 0, certificationRingCapacity), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}), validators: make(map[string]shnsdk.Validator)}
	for line, v := range g.cfg.CertificationValidatorsByLine {
		w.validators[line] = v
		// A gated client qualifies its lane only now that evidence is
		// collected; constructed, it dials nothing.
		if gated, ok := v.(interface{ Start() }); ok {
			gated.Start()
		}
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
		CloseCertificationClients(w.validators)
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
			// The candidate carries the collection's own context, so a client can
			// tell the candidate's time running out (the lane did not answer)
			// from the collection's (the caller gave up).
			candidate, cancel := context.WithTimeout(context.WithValue(ctx, certificationCollectionKey{}, ctx), certificationCandidateTimeout)
			result, err := w.validators[line].Validate(candidate, job.payload, profile)
			contextErr := candidate.Err()
			cancel()
			v.Issues = certificationIssueMetadata(result.Issues)
			var noLane *CertificationLaneUnavailable
			switch {
			case contextErr != nil:
				v.State = "expired"
				v.Error = contextErr.Error()
			case errors.As(err, &noLane):
				// Authored text, not a server's bytes: recorded as written so the
				// evidence says which lane is missing.
				v.State = "unavailable"
				v.Error = err.Error()
			case err != nil:
				v.State = "unavailable"
				v.Error = certificationErrorMetadata(err)
			case result.Valid:
				v.State = "valid"
				v.Valid = true
				e.Certified = append(e.Certified, line)
			default:
				if systems, unchecked := certificationUnchecked(result, job.payload, line, profile); unchecked {
					// The lane could not check the payload's terminology: no verdict
					// on the payload, so unavailable, never invalid and never valid.
					v.State = "unavailable"
					v.Error = terminologyUnavailableReason(systems)
				} else {
					v.State = "invalid"
				}
			}
		}
		e.Verdicts = append(e.Verdicts, v)
	}
	e.SourceLine = certificationSource(e.Certified, e.TargetLine)
	return e
}

// uncheckedCodeSystemShapes are the passed-through terminology verdicts that
// say the validator could not check a code system at all: it does not hold
// the code system, so it can neither expand a value set drawn from it nor
// check a code in it. Each shape is the real lanes' own, on every PAS request
// bundle (the licensed X12 code systems no lane loads), and on a 2.0 lane on a
// CRD code system it does not load. Unknown code '<system>#<code>' is not one
// of them: the validator holds that code system, and the code is not in it.
var uncheckedCodeSystemShapes = []*regexp.Regexp{
	regexp.MustCompile(`^CodeSystem is unknown and can't be validated: (\S+) for '`),
	regexp.MustCompile(`^Unable to expand ValueSet because CodeSystem could not be found: (\S+?)(?: \(validating against |$)`),
}

// uncheckedCodeSystem is the code system an error issue says the validator
// could not check, if it says so, and whether it says the validator could not
// expand the bound value set for want of it (the second shape): only then is
// the required-binding miss on that element undecidable too. "CodeSystem is
// unknown" alone says only that the coding's own system is unknown; the value
// set may be drawn from systems the validator holds, and a code from an
// unknown system is then a miss it did decide.
func uncheckedCodeSystem(iss shnsdk.Issue) (system string, unexpandable bool, ok bool) {
	if iss.Severity != "error" || iss.MessageID != "Terminology_PassThrough_TX_Message" {
		return "", false, false
	}
	for i, shape := range uncheckedCodeSystemShapes {
		if m := shape.FindStringSubmatch(iss.Diagnostics); m != nil {
			return m[1], i == 1, true
		}
	}
	return "", false, false
}

// missedCodingsTail is how a required-binding miss's diagnostics end after its
// code list: the list's own ")" and, when the validator checked the resource
// against a profile it names, that attribution.
var missedCodingsTail = regexp.MustCompile(`^(?s)(.*)\)(?: \(validating against \S+ \[[^\]]*\]\))?$`)

// missedCodingSystems is the code system of every code a required-binding
// miss on a CodeableConcept names: "... (codes = <system>#<code>[,
// <system>#<code>]) [(validating against ...)]". The list runs to the ")"
// that closes it at the end of the diagnostics, so a ")" inside a code stays
// in the code. ok is false when the list cannot be read unambiguously: no list
// or more than one, a tail in another shape, or a code without a system.
func missedCodingSystems(diagnostics string) (systems []string, ok bool) {
	const open = "(codes = "
	if strings.Count(diagnostics, open) != 1 {
		return nil, false
	}
	m := missedCodingsTail.FindStringSubmatch(diagnostics[strings.Index(diagnostics, open)+len(open):])
	if m == nil || m[1] == "" {
		return nil, false
	}
	for _, coding := range strings.Split(m[1], ", ") {
		// A code is its system, "#" and the code: the last "#" ends the system,
		// so a system canonical carrying a fragment keeps it.
		hash := strings.LastIndex(coding, "#")
		if hash <= 0 {
			return nil, false
		}
		systems = append(systems, coding[:hash])
	}
	return systems, true
}

// certificationUnchecked reads an invalid certification result: true when
// the lane could not check the payload's terminology and found nothing else.
// Every error and fatal issue must be one of:
//   - a passed-through verdict that the validator could not check a code
//     system (uncheckedCodeSystem);
//   - the required-binding miss that follows from it on the same element
//     (Terminology_TX_NoValid_1_CC), when for every code it names the
//     validator said on that element that it could not expand the bound value
//     set for want of that code's system (missedCodingSystems);
//   - a no-match summary or PAS slice-match consequence that follows from
//     those alone, read exactly as the structural level reads them
//     (allExcused), on an entry that carries such terminology itself.
//
// Anything else — a structural error, an invariant, a code the validator
// checked and did not find, a binding miss on a code system it holds, a fatal
// issue, an unidentified issue — keeps the answer a verdict: invalid. systems
// is every code system the lane could not check, sorted.
func certificationUnchecked(res shnsdk.Result, body []byte, line, profile string) (systems []string, ok bool) {
	errs := fhirErrors(res)
	if len(errs) == 0 {
		return nil, false
	}
	unexpandableAt := map[string]map[string]bool{} // element → code systems a value set there could not be expanded for
	named := map[string]bool{}
	for i, e := range errs {
		system, unexpandable, ok := uncheckedCodeSystem(e.Issue)
		if !ok {
			continue
		}
		if at := strings.Join(e.Expression, "\x00"); unexpandable {
			if unexpandableAt[at] == nil {
				unexpandableAt[at] = map[string]bool{}
			}
			unexpandableAt[at][system] = true
		}
		named[system] = true
		errs[i].excused = true
	}
	for i, e := range errs {
		if e.excused || e.Severity != "error" || e.MessageID != "Terminology_TX_NoValid_1_CC" || len(e.Expression) == 0 {
			continue
		}
		systems, ok := missedCodingSystems(e.Diagnostics)
		if !ok {
			continue
		}
		atElement := unexpandableAt[strings.Join(e.Expression, "\x00")]
		all := true
		for _, system := range systems {
			all = all && atElement[system]
		}
		errs[i].excused = all
	}
	// A summary or slice-match consequence is excused only by the unchecked
	// terminology on its own entry: an entry with none has no such cause.
	unchecked := map[int]bool{}
	for _, e := range errs {
		if e.excused {
			unchecked[e.entry] = true
		}
	}
	if !allExcused(errs, body, line, profile) {
		return nil, false
	}
	for _, e := range errs {
		if !unchecked[e.entry] {
			return nil, false
		}
	}
	for system := range named {
		systems = append(systems, system)
	}
	sort.Strings(systems)
	return systems, true
}

// terminologyUnavailableReason is the unavailable verdict's authored reason,
// naming the code systems the lane could not check (namedCodeSystem). At
// most two are named, so the reason stays under 256 bytes.
func terminologyUnavailableReason(systems []string) string {
	const maxNamed = 2
	names := make([]string, 0, maxNamed)
	for _, system := range systems {
		if len(names) == maxNamed {
			break
		}
		names = append(names, namedCodeSystem(system))
	}
	reason := "terminology unavailable: " + strings.Join(names, ", ")
	if more := len(systems) - len(names); more > 0 {
		reason += fmt.Sprintf(" and %d more", more)
	}
	return reason
}

// x12CodeSystem is the shape of an X12 code system canonical: X12's host and
// two numeric segments (a version and a code list). Such a name identifies
// public, licensed terminology and has room for nothing else.
var x12CodeSystem = regexp.MustCompile(`^https://codesystem\.x12\.org/[0-9]{1,8}/[0-9]{1,8}$`)

// namedCodeSystem is how an unavailable verdict names a code system. The name
// comes from a validator's diagnostics, quoting the payload's own coding, so
// it is written as it is only when it is an X12 code system canonical
// (x12CodeSystem); any other is written as its size and digest, like every
// other diagnostic the evidence keeps.
func namedCodeSystem(system string) string {
	if x12CodeSystem.MatchString(system) {
		return system
	}
	hash := sha256.Sum256([]byte(system))
	return fmt.Sprintf("code system bytes=%d sha256=%x", len(system), hash)
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
	dialer := &net.Dialer{Timeout: certificationClientTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: certificationClientTimeout, ResponseHeaderTimeout: certificationClientTimeout}
	return &shnsdk.OperationValidator{BaseURL: endpoint, Client: &http.Client{Transport: certificationTransport{inner: transport, dialer: dialer}, Timeout: certificationClientTimeout}}
}

// certificationTransport is the certification client's transport; dialer is
// the one its inner transport dials with, kept so its limits can be read.
type certificationTransport struct {
	inner  *http.Transport
	dialer *net.Dialer
}

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

// certificationCollectionKey carries a collection's own context (its deadline
// and the worker's shutdown) on each candidate's context (callerGaveUp).
type certificationCollectionKey struct{}

// certificationTargetKey carries a write-only observation of the selected token
// through the existing dispatch; the collector never supplies a routing input.
type certificationTargetKey struct{}

// certificationPair certifies both halves of one exchange. responseNonconformance,
// when non-empty, is carried on the RESPONSE half's evidence: it describes the
// answer that was received, so it belongs with that answer's record and nowhere
// else.
func (g *Gateway) certificationPair(leg, seam, corr, target string, request, response []byte, responseNonconformance ...string) {
	for _, part := range []struct {
		direction      string
		payload        []byte
		nonconformance []string
	}{{"request", request, nil}, {"response", response, responseNonconformance}} {
		if len(part.payload) > 0 {
			g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: leg, Seam: seam, CorrelationID: corr, TargetLine: target, Direction: part.direction, Nonconformance: part.nonconformance}, payload: part.payload})
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
