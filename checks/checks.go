// Package checks implements the gateway's connectivity probe runner —
// an ops-surface health check independent of the request-serving
// path. It has no substrate awareness of its own: the app layer builds the
// Target list (see gateway/app/app.go's checkOptionalURL table) and, for
// KindToken, supplies a closure over its own configured credentials so this
// package never handles raw secrets.
//
// Single-flight and cooldown mirror kitd's /api/verify precedent
// (kit/kitd/kitd.go's handleVerifyPost): ErrBusy while a run is in flight,
// and a 30s cooldown returning cached results afterward — auth probes hit
// partner IdPs, and repeated failures can trip partner-side lockouts.
package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
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
	// Failure classifies a failing probe for machine consumption (the
	// console maps Code to operator copy; Hint carries the
	// redaction-safe specifics the code cannot). Present exactly when OK is
	// false. Hint obeys the REDACTION RULE above: it only ever carries
	// fragments Detail already ships.
	Failure *Failure `json:"failure,omitempty"`
	// Capability is probe-retained capability evidence (the prober used
	// to parse fhirVersion and discard it — now it
	// keeps it). Present only when a probe parsed something. ADDITIVE on the
	// wire (the Failure-key precedent): absent ⇒ byte-identical to the
	// pre-capability-evidence shape. Values are peer-published canonical URLs / version
	// strings parsed out of the CapabilityStatement — never a whole response
	// body (REDACTION RULE) — with lists count-capped at capabilityListCap
	// and total input bounded by maxBodyBytes.
	Capability *Capability `json:"capability,omitempty"`
}

// Failure is Result's machine-readable failure classification. Code is a
// closed enum (the Fail* constants); Hint is the redaction-safe variable
// part of Detail, factored out (empty when the code says it all).
type Failure struct {
	Code string `json:"code"`
	Hint string `json:"hint,omitempty"`
}

// Capability is the retained evidence from a fhir-metadata or davinci-config
// probe, plus the contract tokens derived from it and the declared tokens it
// was checked against. List sizes are capped (capabilityListCap) —
// this JSON lands in the control plane's per-tenant CheckState row.
type Capability struct {
	FHIRVersion              string   `json:"fhirVersion,omitempty"`
	ImplementationGuides     []string `json:"implementationGuides,omitempty"`
	SupportedProfiles        []string `json:"supportedProfiles,omitempty"`
	ContractVersions         []string `json:"contractVersions,omitempty"`         // derived from evidence
	DeclaredContractVersions []string `json:"declaredContractVersions,omitempty"` // echoed from Target
	// EndpointURLs is the davinci-configuration probe's per-token endpoint
	// evidence: "<contract>@<line>" → the
	// peer-published URL for that version-coded HRex endpoint (e.g.
	// "pa.pas@2.2" → the partner's $submit URL at 2.2). Same key set as
	// ContractVersions (1:1, same cap/dedup) — populated only by the
	// davinci-config probe (fhir-metadata carries no per-endpoint URLs).
	// app.go's checks-runner wiring feeds this into
	// nativeResponder.SetEndpointEvidence, which same-origin-validates
	// before honoring any entry — this field itself carries whatever the
	// peer published, unvalidated (the trust decision lives at the sink).
	EndpointURLs map[string]string `json:"endpointURLs,omitempty"`
}

// capabilityListCap bounds each retained list (bloat fence for the control
// plane's stored results; a real CapabilityStatement can list hundreds of
// supportedProfiles — only Da Vinci-relevant heads matter here).
const capabilityListCap = 32

// davinciContractByIG maps a Da Vinci IG canonical-path fragment to the SHN
// contract name. FHIR-domain knowledge, not substrate
// awareness — this package already parses CapabilityStatements. Mirrored
// inverse of gateway/engine's hrexCodeForContract; extend BOTH sides together
// for any new contract. Deliberately asymmetric on pa.pdex: this reverse/
// probe side includes it because probing reads any peer's own publication,
// while the engine's forward map deliberately omits pa.pdex — see that map's
// comment for why.
var davinciContractByIG = map[string]string{
	"/us/davinci-crd/":  "pa.crd",
	"/us/davinci-dtr/":  "pa.dtr",
	"/us/davinci-pas/":  "pa.pas",
	"/us/davinci-pdex/": "pa.pdex",
}

// contractTokenFromCanonical derives "<contract>@<major.minor>" from a
// VERSIONED Da Vinci canonical ("…|2.0.1"). Unversioned entries and
// non-contract IGs yield ok=false — evidence must never be guessed.
func contractTokenFromCanonical(entry string) (string, bool) {
	base, ver, ok := strings.Cut(entry, "|")
	if !ok || ver == "" {
		return "", false
	}
	var contract string
	for frag, c := range davinciContractByIG {
		if strings.Contains(base, frag) {
			contract = c
			break
		}
	}
	if contract == "" {
		return "", false
	}
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return "", false
	}
	return contract + "@" + parts[0] + "." + parts[1], true
}

// driftAgainstDeclared applies the drift rule: group both token sets by
// contract; every contract present in BOTH must share at least one line.
// Returns a redaction-safe hint naming the disagreeing sets and true on
// drift. Order-insensitive, deterministic output.
func driftAgainstDeclared(declared, evidence []string) (string, bool) {
	lines := func(tokens []string) map[string]map[string]bool {
		m := map[string]map[string]bool{}
		for _, tok := range tokens {
			c, l, ok := strings.Cut(tok, "@")
			if !ok {
				continue
			}
			if m[c] == nil {
				m[c] = map[string]bool{}
			}
			m[c][l] = true
		}
		return m
	}
	dec, ev := lines(declared), lines(evidence)
	var drifts []string
	for c, dl := range dec {
		el, both := ev[c]
		if !both {
			continue
		}
		shared := false
		for l := range dl {
			if el[l] {
				shared = true
				break
			}
		}
		if !shared {
			drifts = append(drifts, fmt.Sprintf("%s declared %s but endpoint publishes %s", c, joinLines(c, dl), joinLines(c, el)))
		}
	}
	if len(drifts) == 0 {
		return "", false
	}
	sort.Strings(drifts)
	return strings.Join(drifts, "; "), true
}

// joinLines renders a contract's line set as sorted tokens ("pa.pas@2.0").
func joinLines(contract string, set map[string]bool) string {
	var out []string
	for l := range set {
		out = append(out, contract+"@"+l)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
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
	// FailVersionDrift: the endpoint's PUBLISHED contract versions disagree
	// with the operator-declared per-peer versions for a contract both sides
	// know. Evidence-absent contracts never
	// drift — silence is not disagreement.
	FailVersionDrift = "version-drift"
	// FailInvalidDavinciConfiguration: the peer PUBLISHED a
	// .well-known/davinci-configuration that could not be parsed (2xx but
	// undecodable). Distinct from absence, which is OK — HRex publication is
	// optional.
	FailInvalidDavinciConfiguration = "invalid-davinci-configuration"
)

// Kind selects how a Target is probed.
type Kind string

const (
	KindFHIRMetadata Kind = "fhir-metadata" // GET <base>/metadata, expect CapabilityStatement
	KindToken        Kind = "token"         // run the provided token fetch (credential check)
	KindReachable    Kind = "reachable"     // GET, ok when status < 500
	// KindDavinciConfig: GET <base>/.well-known/davinci-configuration;
	// absence (404) tolerated
	KindDavinciConfig Kind = "davinci-config"
)

// Target is one probe the runner executes. TokenFetch is set only for
// KindToken — the app layer provides a closure over its own configured
// auth so this package never handles raw credentials.
type Target struct {
	ID         string
	Kind       Kind
	URL        string
	TokenFetch func(ctx context.Context) error
	// DeclaredVersions is the operator-declared contract-token set for this
	// peer (opaque to this package); probes with version evidence compare
	// against it (FailVersionDrift).
	DeclaredVersions []string
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
	// budgets — the hosted control plane's ChecksClient POSTs with a 15s client timeout and
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

	// OnResults, when non-nil, is invoked with a freshly-completed run's
	// results at the exact point they land in Last() — the
	// "results collected each cycle" hook a substrate-aware
	// caller wires a sink into (app.go feeds the native-forward responder's
	// per-line endpoint evidence through it, TestProbeEvidenceReachesResponder).
	// Set directly on the Runner (the engine.Config.Observer field
	// precedent) rather than threaded through NewRunner, so every existing
	// 3-arg call site is untouched. Intended to be set once, before Run is
	// first called (no lock: matches Observer's own unsynchronized-field
	// convention). NOT invoked on a cooldown-cached short-circuit — no new
	// evidence that cycle. This package stays substrate-unaware: the
	// callback receives the same generic []Result every Run caller already
	// sees, nothing engine-specific.
	OnResults func([]Result)
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
// single-flight (ErrBusy while one runs) and cooldown-capped:
// within 30s of the last completed run it returns the cached results —
// auth probes hit partner IdPs and repeated failures can trip partner-side
// lockouts. Probes that don't get to run before the deadline are reported
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
	if r.OnResults != nil {
		r.OnResults(append([]Result(nil), results...))
	}
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
		res.OK, res.Detail, res.Failure, res.Capability = r.probeFHIRMetadata(pctx, t)
	case KindToken:
		res.OK, res.Detail, res.Failure = r.probeToken(pctx, t.TokenFetch)
	case KindReachable:
		res.OK, res.Detail, res.Failure = r.probeReachable(pctx, t.URL)
	case KindDavinciConfig:
		res.OK, res.Detail, res.Failure, res.Capability = r.probeDavinciConfig(pctx, t)
	default:
		res.OK, res.Detail = false, fmt.Sprintf("unknown probe kind %q", t.Kind)
		res.Failure = &Failure{Code: FailInternal, Hint: fmt.Sprintf("unknown probe kind %q", t.Kind)}
	}

	res.LatencyMS = r.now().Sub(start).Milliseconds()
	return res
}

// probeFHIRMetadata GETs <base>/metadata and expects a CapabilityStatement.
func (r *Runner) probeFHIRMetadata(ctx context.Context, t Target) (bool, string, *Failure, *Capability) {
	base := t.URL
	redacted := targetOf(base)

	u, err := url.Parse(base)
	if err != nil {
		// A boot-validated URL failing to parse is a bug, not a network
		// condition — internal, not unreachable.
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}, nil
	}
	// JoinPath appends the "metadata" path element relative to u's existing
	// path (so a base with its own path, e.g. ".../fhir", probes
	// ".../fhir/metadata" rather than corrupting a trailing query string —
	// unlike string concatenation, it preserves RawQuery/User untouched).
	full := u.JoinPath("metadata").String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}, nil
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("GET %s/metadata: %v", redacted, redactErr(err)),
			&Failure{Code: FailUnreachable, Hint: redactErr(err).Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode),
			&Failure{Code: FailHTTPStatus, Hint: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}

	var cs struct {
		ResourceType        string   `json:"resourceType"`
		FhirVersion         string   `json:"fhirVersion"`
		ImplementationGuide []string `json:"implementationGuide"`
		Rest                []struct {
			Resource []struct {
				SupportedProfile []string `json:"supportedProfile"`
			} `json:"resource"`
		} `json:"rest"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&cs); err != nil {
		return false, fmt.Sprintf("GET %s/metadata: decode: %v", redacted, err),
			&Failure{Code: FailInvalidCapabilityStatement, Hint: fmt.Sprintf("decode: %v", err)}, nil
	}
	if cs.ResourceType != "CapabilityStatement" {
		return false, fmt.Sprintf("resourceType %q, want CapabilityStatement", cs.ResourceType),
			&Failure{Code: FailInvalidCapabilityStatement, Hint: fmt.Sprintf("resourceType %q", cs.ResourceType)}, nil
	}

	cap := &Capability{FHIRVersion: cs.FhirVersion}
	seen := map[string]bool{}
	collect := func(entries []string, into *[]string) {
		for _, e := range entries {
			if len(*into) < capabilityListCap {
				*into = append(*into, e)
			}
			if tok, ok := contractTokenFromCanonical(e); ok && !seen[tok] && len(cap.ContractVersions) < capabilityListCap {
				seen[tok] = true
				cap.ContractVersions = append(cap.ContractVersions, tok)
			}
		}
	}
	collect(cs.ImplementationGuide, &cap.ImplementationGuides)
	for _, rest := range cs.Rest {
		for _, res := range rest.Resource {
			collect(res.SupportedProfile, &cap.SupportedProfiles)
		}
	}
	sort.Strings(cap.ContractVersions) // deterministic across map-ordered walks
	cap.DeclaredContractVersions = t.DeclaredVersions

	if hint, drift := driftAgainstDeclared(t.DeclaredVersions, cap.ContractVersions); drift {
		return false, "declared contract versions drift from published capability: " + hint,
			&Failure{Code: FailVersionDrift, Hint: hint}, cap
	}
	return true, fmt.Sprintf("CapabilityStatement (FHIR %s)", cs.FhirVersion), nil, cap
}

// contractByHRexCode is the inverse of gateway/engine's hrexCodeForContract
// (extend BOTH sides together for any new contract): HRex endpoint-code base
// → SHN contract name. Deliberately asymmetric on pa.pdex: this reverse/
// probe side includes it because probing reads any peer's own publication,
// while the engine's forward map deliberately omits pa.pdex — see that map's
// comment for why.
var contractByHRexCode = map[string]string{
	"davinci_crd_hook_endpoint":       "pa.crd",
	"davinci_dtr_qpackage_endpoint":   "pa.dtr",
	"davinci_pas_submission_endpoint": "pa.pas",
	"davinci_pdex_patient_endpoint":   "pa.pdex",
}

// probeDavinciConfig GETs <base>/.well-known/davinci-configuration (HRex
// 1.2.0 endpoint discovery). 404/410 → OK "not published (optional)". A
// published doc yields tokens from its version-specific endpoint codes
// ("<code>#<major.minor>"); unknown codes are ignored; drift is evaluated
// against t.DeclaredVersions exactly as the fhir-metadata probe does.
func (r *Runner) probeDavinciConfig(ctx context.Context, t Target) (bool, string, *Failure, *Capability) {
	redacted := targetOf(t.URL)
	u, err := url.Parse(t.URL)
	if err != nil {
		return false, fmt.Sprintf("GET %s/.well-known/davinci-configuration: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}, nil
	}
	full := u.JoinPath(".well-known", "davinci-configuration").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return false, fmt.Sprintf("GET %s/.well-known/davinci-configuration: %v", redacted, redactErr(err)),
			&Failure{Code: FailInternal, Hint: redactErr(err).Error()}, nil
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("GET %s/.well-known/davinci-configuration: %v", redacted, redactErr(err)),
			&Failure{Code: FailUnreachable, Hint: redactErr(err).Error()}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return true, "davinci-configuration not published (optional)", nil, nil
	}
	// DECIDED 2026-08-11: an auth-gated well-known is functionally
	// unpublished — HRex 1.2.0 requires plain-TLS readability, so 401/403 mean
	// "not discoverable", not "broken". Tolerated like 404, with the HRex
	// nonconformance NAMED in the detail so it surfaces without paging. The
	// fhir-metadata probe deliberately keeps 401 as a failure — /metadata is
	// core conformance surface. Flip this branch to FailHTTPStatus if the
	// posture is revisited; TestDavinciConfigProbe pins whichever stands.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return true, fmt.Sprintf("davinci-configuration not readable without auth (HTTP %d; HRex 1.2.0 expects plain-TLS readability) — treated as not published", resp.StatusCode), nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode),
			&Failure{Code: FailHTTPStatus, Hint: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	var doc struct {
		Endpoints map[string]string `json:"endpoints"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&doc); err != nil {
		return false, fmt.Sprintf("GET %s/.well-known/davinci-configuration: decode: %v", redacted, err),
			&Failure{Code: FailInvalidDavinciConfiguration, Hint: fmt.Sprintf("decode: %v", err)}, nil
	}
	cap := &Capability{DeclaredContractVersions: t.DeclaredVersions}
	seen := map[string]bool{}
	for code, u := range doc.Endpoints {
		base, line, ok := strings.Cut(code, "#")
		if !ok || line == "" {
			continue
		}
		contract, known := contractByHRexCode[base]
		if !known {
			continue
		}
		tok := contract + "@" + line
		if !seen[tok] && len(cap.ContractVersions) < capabilityListCap {
			seen[tok] = true
			cap.ContractVersions = append(cap.ContractVersions, tok)
			// Endpoint evidence: retain the endpoint URL alongside the token (previously
			// discarded — the loop ranged over doc.Endpoints' KEYS only).
			// Same cap/dedup as ContractVersions; an empty published URL is
			// not retained (nothing for the sink to resolve to).
			if u != "" {
				if cap.EndpointURLs == nil {
					cap.EndpointURLs = map[string]string{}
				}
				cap.EndpointURLs[tok] = u
			}
		}
	}
	sort.Strings(cap.ContractVersions)
	if hint, drift := driftAgainstDeclared(t.DeclaredVersions, cap.ContractVersions); drift {
		return false, "declared contract versions drift from published davinci-configuration: " + hint,
			&Failure{Code: FailVersionDrift, Hint: hint}, cap
	}
	return true, fmt.Sprintf("davinci-configuration (%d version-coded endpoints)", len(cap.ContractVersions)), nil, cap
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
