// Package checks implements the gateway's connectivity probe runner — an
// ops-surface health check independent of the request-serving path. It has
// no substrate awareness of its own: the app layer builds the Target list
// (see app/app.go's checkOptionalURL table) and, for KindToken, supplies a
// closure over its own configured credentials so this package never
// handles raw secrets.
//
// Single-flight and cooldown mirror the kit daemon's verify precedent:
// ErrBusy while a run is in flight, and a 30s cooldown returning cached
// results afterward — auth probes hit partner IdPs, and repeated failures
// can trip partner-side lockouts.
package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Result is one probe outcome (extends the kit's
// bootstrap.Probe {name,ok,detail} with target/timing).
//
// REDACTION RULE: Target and Detail render verbatim in operator UIs — they
// must NEVER contain credentials, tokens, query strings, or response bodies.
type Result struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"` // scheme://host only
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail"`
	CheckedAt time.Time `json:"checkedAt"`
	LatencyMS int64     `json:"latencyMs"`
	// Failure classifies a failing probe for machine consumption (an
	// operator console maps Code to its own copy; Hint carries the
	// redaction-safe specifics the code cannot). Present exactly when OK is
	// false. Hint obeys the REDACTION RULE above: it only ever carries
	// fragments Detail already ships.
	Failure *Failure `json:"failure,omitempty"`
}

// Failure is Result's machine-readable failure classification. Code is a
// closed enum (the Fail* constants); Hint is the redaction-safe variable
// part of Detail, factored out (empty when the code says it all).
type Failure struct {
	Code string `json:"code"`
	Hint string `json:"hint,omitempty"`
}

// Failure codes — a closed enum, one distinct operator meaning per code.
const (
	// FailUnreachable: transport-level failure (dial, DNS, TLS, per-probe
	// timeout) — the endpoint never answered.
	FailUnreachable = "unreachable"
	// FailHTTPStatus: the endpoint answered with a failing status
	// (fhir-metadata non-2xx; reachable >= 500).
	FailHTTPStatus = "http-status"
	// FailInvalidCapabilityStatement: a 2xx answer that is not a valid
	// CapabilityStatement (decode failure or wrong resourceType — Hint
	// says which).
	FailInvalidCapabilityStatement = "invalid-capability-statement"
	// FailCredentialRejected: the credential check failed. Deliberately
	// neutral — this also wears a local key-load bug and a token-leg
	// timeout (both non-StatusError closure errors), so operator copy
	// must never say the PARTNER rejected the credential.
	FailCredentialRejected = "credential-rejected"
	// FailNotChecked: the run deadline starved this probe; nothing was
	// probed.
	FailNotChecked = "not-checked"
	// FailInternal: a bug, not a network condition — URL parse /
	// request-build failure on a boot-validated URL, or an unknown probe
	// kind.
	FailInternal = "internal"
)

// Kind selects how a Target is probed.
type Kind string

const (
	KindFHIRMetadata Kind = "fhir-metadata" // GET <base>/metadata, expect CapabilityStatement
	KindToken        Kind = "token"         // run the provided token fetch (credential check)
	KindReachable    Kind = "reachable"     // GET, ok when status < 500
)

// Target is one probe the runner executes. TokenFetch is set only for
// KindToken — the app layer provides a closure over its own configured
// auth so this package never handles raw credentials.
type Target struct {
	ID         string
	Kind       Kind
	URL        string
	TokenFetch func(ctx context.Context) error
}

// ErrBusy is returned by Run when a previous run is still in flight.
var ErrBusy = errors.New("checks: run already in flight")

// StatusError lets a Target's TokenFetch closure report an HTTP status
// without leaking raw error text (redaction rule): the runner maps it to
// "credential check failed (HTTP %d)"; any other error becomes the fixed
// string "credential check failed" — never err.Error() verbatim.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("checks: status %d", e.Code)
}

const (
	// cooldown caps how often a Run actually re-probes: within cooldown of
	// the last completed run, Run returns the cached results instead
	// (auth probes hit partner IdPs; repeated failures can trip
	// partner-side lockouts).
	cooldown = 30 * time.Second

	// runDeadline caps a WHOLE run: probes execute serially, so N slow
	// targets at the per-probe cap would otherwise exceed the callers'
	// budgets — control-plane callers POST with a 15s client timeout and
	// kitd's verify precedent is 15s. 12s keeps the full run inside both.
	runDeadline = 12 * time.Second

	// probeTimeout is the per-probe cap, further bounded by whatever time
	// remains on the overall run deadline.
	probeTimeout = 10 * time.Second

	// maxBodyBytes bounds how much of a fhir-metadata response body gets
	// decoded. It guards against an unbounded body, not a merely large one:
	// a real HAPI /metadata enumerating a full resource catalog runs ~2 MiB,
	// and the original 1 MiB cap truncated those mid-document, failing
	// healthy endpoints with "decode: unexpected EOF". 8 MiB gives 4×
	// headroom over observed CapabilityStatements while keeping the probe's
	// memory bounded.
	maxBodyBytes = 8 << 20
)

// Runner executes a fixed set of Targets serially, single-flight and
// cooldown-capped, and remembers the newest results.
type Runner struct {
	targets []Target
	client  *http.Client
	now     func() time.Time

	cooldown time.Duration
	deadline time.Duration // runDeadline; unexported so tests can shorten it

	mu     sync.Mutex
	busy   bool
	last   []Result
	lastAt time.Time
}

// NewRunner builds a Runner over targets. client defaults to
// http.DefaultClient when nil; now defaults to time.Now when nil.
func NewRunner(targets []Target, client *http.Client, now func() time.Time) *Runner {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	return &Runner{
		targets:  targets,
		client:   client,
		now:      now,
		cooldown: cooldown,
		deadline: runDeadline,
	}
}

// Run executes all probes serially under one runDeadline-bounded context;
// single-flight (ErrBusy while one runs) and cooldown-capped (within
// 30s of the last completed run it returns the cached results —
// auth probes hit partner IdPs and repeated failures can trip partner-side
// lockouts). Probes that don't get to run before the deadline are reported
// !OK with detail "not checked — run deadline exceeded".
func (r *Runner) Run(ctx context.Context) ([]Result, error) {
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return nil, ErrBusy
	}
	if !r.lastAt.IsZero() && r.now().Sub(r.lastAt) < r.cooldown {
		out := append([]Result(nil), r.last...)
		r.mu.Unlock()
		return out, nil
	}
	r.busy = true
	r.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, r.deadline)
	defer cancel()

	results := make([]Result, 0, len(r.targets))
	for _, t := range r.targets {
		if runCtx.Err() != nil {
			results = append(results, Result{
				ID:        t.ID,
				Target:    targetOf(t.URL),
				OK:        false,
				Detail:    "not checked — run deadline exceeded",
				Failure:   &Failure{Code: FailNotChecked},
				CheckedAt: r.now(),
			})
			continue
		}
		results = append(results, r.probe(runCtx, t))
	}

	r.mu.Lock()
	r.last, r.lastAt, r.busy = results, r.now(), false
	r.mu.Unlock()
	return results, nil
}

// Last returns the newest results; ok=false before the first run completes.
func (r *Runner) Last() ([]Result, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastAt.IsZero() {
		return nil, time.Time{}, false
	}
	return append([]Result(nil), r.last...), r.lastAt, true
}

// targetOf renders raw for display: scheme://host only, dropping userinfo,
// path, and query — the redaction rule. Unparseable URLs render "".
func targetOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// redactErr strips a *url.Error's embedded request URL before the error is
// used in a Detail string. net/http request errors are *url.Error, and its
// Error() method only masks the password — the username and the ENTIRE query
// string (which may itself carry a credential, e.g. ?apikey=...) survive
// verbatim in url.Error.Error(). Unwrapping to the inner error (e.g. the
// dial/net error) keeps the redaction rule intact.
func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// probe dispatches on t.Kind, bounding the probe to min(probeTimeout, time
// left on ctx) and stamping CheckedAt/LatencyMS from the runner's clock.
func (r *Runner) probe(ctx context.Context, t Target) Result {
	timeout := probeTimeout
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < timeout {
			timeout = left
		}
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := r.now()
	res := Result{ID: t.ID, Target: targetOf(t.URL), CheckedAt: start}

	switch t.Kind {
	case KindFHIRMetadata:
		res.OK, res.Detail, res.Failure = r.probeFHIRMetadata(pctx, t.URL)
	case KindToken:
		res.OK, res.Detail, res.Failure = r.probeToken(pctx, t.TokenFetch)
	case KindReachable:
		res.OK, res.Detail, res.Failure = r.probeReachable(pctx, t.URL)
	default:
		res.OK, res.Detail = false, fmt.Sprintf("unknown probe kind %q", t.Kind)
		res.Failure = &Failure{Code: FailInternal, Hint: fmt.Sprintf("unknown probe kind %q", t.Kind)}
	}

	res.LatencyMS = r.now().Sub(start).Milliseconds()
	return res
}

// probeFHIRMetadata GETs <base>/metadata and expects a CapabilityStatement.
func (r *Runner) probeFHIRMetadata(ctx context.Context, base string) (bool, string, *Failure) {
	redacted := targetOf(base)

	u, err := url.Parse(base)
	if err != nil {
		// A boot-validated URL failing to parse is a bug, not a network
		// condition — internal, not unreachable.
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}
	}
	// JoinPath appends the "metadata" path element relative to u's existing
	// path (so a base with its own path, e.g. ".../fhir", probes
	// ".../fhir/metadata" rather than corrupting a trailing query string —
	// unlike string concatenation, it preserves RawQuery/User untouched).
	full := u.JoinPath("metadata").String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailUnreachable, Hint: redactErr(err).Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode),
			&Failure{Code: FailHTTPStatus, Hint: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}

	var cs struct {
		ResourceType string `json:"resourceType"`
		FhirVersion  string `json:"fhirVersion"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&cs); err != nil {
		return false, fmt.Sprintf("GET %s/metadata: decode: %v", redacted, err),
			&Failure{Code: FailInvalidCapabilityStatement, Hint: fmt.Sprintf("decode: %v", err)}
	}
	if cs.ResourceType != "CapabilityStatement" {
		return false, fmt.Sprintf("resourceType %q, want CapabilityStatement", cs.ResourceType),
			&Failure{Code: FailInvalidCapabilityStatement, Hint: fmt.Sprintf("resourceType %q", cs.ResourceType)}
	}
	return true, fmt.Sprintf("CapabilityStatement (FHIR %s)", cs.FhirVersion), nil
}

// probeToken runs fetch as a credential check. Failures never surface
// err.Error() verbatim (redaction rule): a *StatusError classifies to its
// HTTP status class, anything else collapses to a fixed string.
func (r *Runner) probeToken(ctx context.Context, fetch func(context.Context) error) (bool, string, *Failure) {
	if fetch == nil {
		return false, "credential check failed", &Failure{Code: FailCredentialRejected}
	}
	err := fetch(ctx)
	if err == nil {
		return true, "ok", nil
	}
	var se *StatusError
	if errors.As(err, &se) {
		return false, fmt.Sprintf("credential check failed (HTTP %d)", se.Code),
			&Failure{Code: FailCredentialRejected, Hint: fmt.Sprintf("HTTP %d", se.Code)}
	}
	return false, "credential check failed", &Failure{Code: FailCredentialRejected}
}

// probeReachable GETs target; ok when the response status is below 500 (a
// 404 still proves the edge exists and is answering).
func (r *Runner) probeReachable(ctx context.Context, target string) (bool, string, *Failure) {
	redacted := targetOf(target)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, fmt.Sprintf("GET %s: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("GET %s: %v", redacted, redactErr(err)),
			&Failure{Code: FailUnreachable, Hint: redactErr(err).Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode >= http.StatusInternalServerError {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode),
			&Failure{Code: FailHTTPStatus, Hint: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return true, fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}
