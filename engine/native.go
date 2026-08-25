// native.go — the native-forward payer LegResponder (Case 1). It
// forwards each read-only leg to a partner's real Da Vinci endpoint over a
// SMART-authenticated *http.Client and returns the partner's FHIR. The engine still
// owns authority (the (A)/(B) inbound fences + the (C) outbound subject fence, now
// defending a real party), sealing, edge $validate, and audit (AI-11). The PAS legs
// (nativepas.go) reuse the originator's PUBLISHED shnsdk parsers; the CRD leg normalizes
// the partner's coverage-information to the canonical shnsdk.CardCoverage (FR-G25,
// normalizeCRDCoverage in davincimap.go) and re-renders SHN cards with shnsdk.BuildCards,
// so this file references shnsdk too. It implements the internal, unstable
// engine.LegResponder (STABILITY: connectors/* is the supported surface); it
// graduates to connectors/davinci when LegResponder promotes to shnsdk.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// crdHook is the CDS Hooks hook name SHN originates for the CRD leg. Discovery
// selects the partner CDS service whose "hook" field matches this value (FR-G26).
const crdHook = "order-select"

const maxPartnerBody = 8 << 20 // 8 MiB cap on a partner response body

const relayBodyCap = 6 << 20 // 6 MiB — headroom under the 8 MiB MaxResponseBytes for seal + wrapper

type nativeResponder struct {
	client               *http.Client
	baseURL              string // FHIR base ($questionnaire-package, $submit, CoverageEligibilityRequest)
	cdsBaseURL           string // CDS Hooks base (/cds-services/{id}); defaults to baseURL when co-located
	crdServiceID         string // discovered or overridden at construction (FR-G26)
	crdHookOverride      string // when set, the request hook is rewritten to this before CRD forward (FR-G26; br-payer's order-sign)
	crdDispatchServiceID string // partner's order-dispatch CDS service id (order-dispatch leg)
	crdDispatchHook      string // when set, the request hook is rewritten to this before order-dispatch forward
	// crdCoverageBundle wraps the CRD request's bare prefetch.coverage into a searchset Bundle
	// before forwarding — for a partner whose order-sign `coverage` prefetch is a SEARCH
	// template (Coverage?beneficiary=…) that demands a searchset Bundle (bare Coverage → 412
	// "Missing Coverage"). The SHN spine carries a BARE Coverage (provider routing +
	// the payer-side bind both read bare, crd_native.go), so the wrap is a partner-scoped EGRESS
	// step run AFTER the bind. Gated by PAYER_DAVINCI_CRD_COVERAGE_BUNDLE; off ⇒ bare verbatim
	// (br-payer conformance untouched).
	crdCoverageBundle bool
	store             Store // gateway-owned shadow ledger + EOB Store for the PAS legs (nil ⇒ read-only only)
	clock             func() time.Time
	// PAS pend re-query: a real Da Vinci payer (br-payer) re-pends a conformant
	// amendment (persistUpdatePath keeps a conditional item A4 + reschedules) and auto-resolves
	// A4→A1 only on its own timer (PasPendedResolutionService, pas.pended-resolution-delay-seconds).
	// On a re-pended ClaimUpdate the responder polls GET ClaimResponse/{id} until A1 (or timeout →
	// 422, no silent pass). Timeout MUST exceed the payer's resolution delay (E4 lowers it to 3s for
	// the harness) and stay under the originator's 30s Hub-client timeout.
	pendReQueryTimeout  time.Duration
	pendReQueryInterval time.Duration
	// declaredContractVersions is the operator-declared token set for the
	// foreign partner (PAYER_DAVINCI_CONTRACT_VERSIONS) — the peer-config
	// source of the routing filter. Empty = silent peer:
	// forward at own line, never refuse (the same pre-contract tolerance the
	// substrate filter applies).
	declaredContractVersions []string
	// ownContractVersions is THIS deployment's declared token set — the same
	// SHN_CONTRACT_VERSIONS accessor the substrate filter, the published
	// CapabilityStatements and the registry declaration read (D1a). It is the
	// "own" half of the foreign-peer filter below. Empty ⇒ the build default,
	// which is what an unset SHN_CONTRACT_VERSIONS means everywhere else.
	//
	// Kept as CONFIG (an option) rather than read off the request context like the
	// per-leg answer line: this is a fail-closed refuse-before-forward GATE, and a
	// gate must not depend on a request-scoped value that could be absent.
	ownContractVersions []string
	// strictExtensions carries PAYER_DAVINCI_STRICT_EXTENSIONS (FR-G52) as
	// DORMANT plumbing today: NO Handle-filter
	// reads it, so it produces zero behavior delta on the native-forward
	// path (byte-identical fence: TestNativeStrictExtensionsFieldIsDormant).
	// The route-layer strict consult that IS live reads
	// the SAME operator flag through g.strictPeer / Config, not through this
	// field — native.go's arm-1-only pin
	// (TestNativeForwardStaysArm1) means no chain ever reaches this
	// responder's own forward today, so a Handle-side consult would guard a
	// branch that can't run. This field goes live together with
	// transform-at-the-forward-edge (recorded deferral) — the day this
	// responder itself builds a chain-bridged payload rather than relaying
	// the provider's bytes verbatim.
	strictExtensions bool

	// evMu guards endpointEvidence — the HRex per-version endpoint
	// evidence. SetEndpointEvidence
	// WHOLESALE-replaces the map once per checks cycle (app.go's runner
	// hook); resolvedURL reads it per-leg. Both run concurrently with the
	// request-serving path (Handle may be mid-flight on another goroutine
	// while a checks cycle completes), so this is a real mutex, not a
	// construction-time-only field like the options above.
	evMu sync.RWMutex
	// endpointEvidence is "<contract>@<line>" -> the partner's published
	// per-version operation URL, ALREADY same-origin-validated at set time
	// (SetEndpointEvidence drops anything else). nil/absent-token ⇒
	// resolvedURL falls back to the configured base+path — the fence
	// (TestNativeForwardSelectsLineEndpoint's byte-identical case).
	endpointEvidence map[string]string
	// endpointEvidenceObserver, when non-nil, receives one redaction-safe
	// note per SetEndpointEvidence call that drops an entry (same-origin
	// trust-rule rejection or a malformed URL) — the endpoint-evidence
	// "observer note". nil-safe (default: silent); app.go wires it to an
	// operator-visible log line via WithEndpointEvidenceObserver, the same
	// posture as this file's existing "gateway: WARNING ..." stdout
	// precedent (gateway/app/app.go).
	endpointEvidenceObserver func(note string)
}

const (
	defaultPendReQueryTimeout  = 12 * time.Second
	defaultPendReQueryInterval = 1 * time.Second
)

// NativeOption configures optional nativeResponder behavior.
type NativeOption func(*nativeResponder)

// WithCDSBaseURL overrides the base used for CDS Hooks (CRD) posts, for partners whose
// CDS Hooks endpoint is NOT co-located with their FHIR base — e.g. br-payer serves CDS
// Hooks at root /cds-services but FHIR ops under /fhir. Unset ⇒ CDS posts use the FHIR
// baseURL (co-located default, the prior behavior). FR-G28 / OWD-G8.
func WithCDSBaseURL(cdsBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if cdsBaseURL != "" {
			n.cdsBaseURL = cdsBaseURL
		}
	}
}

// WithCRDHook sets the CDS Hooks hook value the native-forward stamps on the CRD request
// before forwarding, to match the partner's configured CRD service (e.g. br-payer's
// order-sign-crd demands hook:order-sign while SHN originates the canonical order-select).
// Unset ⇒ forward the originator's hook verbatim (the prior behavior).
func WithCRDHook(hook string) NativeOption {
	return func(n *nativeResponder) {
		if hook != "" {
			n.crdHookOverride = hook
		}
	}
}

// WithCRDDispatchService configures the partner's order-dispatch CDS service id + hook for the
// crd-order-dispatch leg (distinct from order-select's crdServiceID/crdHookOverride). ONE payer-gw
// serves both: crd-order-select→order-sign-crd, crd-order-dispatch→order-dispatch-crd.
func WithCRDDispatchService(serviceID, hook string) NativeOption {
	return func(n *nativeResponder) { n.crdDispatchServiceID, n.crdDispatchHook = serviceID, hook }
}

// WithCRDCoverageBundle enables wrapping the CRD request's bare prefetch.coverage into a
// searchset Bundle before forwarding — such a partner's order-sign `coverage` prefetch is a SEARCH
// template (Coverage?beneficiary=…) demanding a searchset Bundle, while the SHN spine carries a
// bare Coverage (provider routing + payer-side bind both read bare, crd_native.go). Idempotent:
// an already-Bundle coverage is left as-is. Unset ⇒ forward the coverage verbatim (br-payer's
// shape). Set via PAYER_DAVINCI_CRD_COVERAGE_BUNDLE (per-partner, opt-in).
func WithCRDCoverageBundle(on bool) NativeOption {
	return func(n *nativeResponder) { n.crdCoverageBundle = on }
}

// WithPendReQuery overrides the PAS pend re-query poll timeout + interval (E2). Zero values keep
// the defaults. Used to make the hermetic nativeResponder tests fast (short interval) and to let
// the harness tune the bound relative to the payer's resolution delay (E4).
func WithPendReQuery(timeout, interval time.Duration) NativeOption {
	return func(n *nativeResponder) {
		if timeout > 0 {
			n.pendReQueryTimeout = timeout
		}
		if interval > 0 {
			n.pendReQueryInterval = interval
		}
	}
}

// WithDeclaredContractVersions supplies the operator-declared contract tokens
// for the partner endpoint; legs whose contract shares no line refuse legibly
// instead of forwarding (the foreign-endpoint filter).
func WithDeclaredContractVersions(tokens []string) NativeOption {
	return func(n *nativeResponder) { n.declaredContractVersions = tokens }
}

// WithOwnContractVersions supplies THIS deployment's declared contract tokens —
// the "own" half of the foreign-peer filter, and the
// sibling of WithDeclaredContractVersions, which supplies the PEER's half. Without
// it the filter fell back to the library build constant, so a deployment that
// declared 2.2 routed substrate legs at 2.2 yet refused to forward to a 2.2-only
// Da Vinci partner. Empty ⇒ the build default.
func WithOwnContractVersions(tokens []string) NativeOption {
	return func(n *nativeResponder) { n.ownContractVersions = tokens }
}

// WithStrictExtensions supplies PAYER_DAVINCI_STRICT_EXTENSIONS (FR-G52): DORMANT
// plumbing today — see the strictExtensions field comment for why. Kept
// as a constructor option (not a runtime setter) because, unlike
// SetEndpointEvidence, it never needs to change after boot.
func WithStrictExtensions(on bool) NativeOption {
	return func(n *nativeResponder) { n.strictExtensions = on }
}

// WithEndpointEvidenceObserver supplies the endpoint-evidence same-origin-drop note sink
// (see endpointEvidenceObserver's field comment). Unset ⇒ silent drops (still
// dropped — this only affects observability, never the trust decision).
func WithEndpointEvidenceObserver(f func(note string)) NativeOption {
	return func(n *nativeResponder) { n.endpointEvidenceObserver = f }
}

// ownDeclared is this responder's declared-set accessor — the nativeResponder mirror
// of Gateway.declaredContractVersions(), with the same empty-means-build-default
// rule, so the two halves of "what do we speak" cannot diverge.
func (n *nativeResponder) ownDeclared() []string {
	if len(n.ownContractVersions) > 0 {
		return n.ownContractVersions
	}
	return shnsdk.SupportedContractVersions()
}

var _ LegResponder = (*nativeResponder)(nil)

// EndpointEvidenceSetter is the sink app.go's checks-runner→responder
// evidence hook targets: implemented by *nativeResponder (SetEndpointEvidence,
// below). A separate, narrower interface from LegResponder (a stable public
// partner-implementable seam) on purpose — this is app-internal wiring
// plumbing between the checks runner and the ONE responder that can act on
// probe evidence, not part of the payer-content contract every LegResponder
// implements.
type EndpointEvidenceSetter interface {
	SetEndpointEvidence(evidence map[string]string)
}

var _ EndpointEvidenceSetter = (*nativeResponder)(nil)

// NewNativeResponder builds the native-forward Responder over a ready *http.Client
// (in production a smartauth bearer client; in tests a fixed-bearer client).
// crdServiceID is the partner's CDS Hooks order-select service id, resolved at boot
// via DiscoverCRDServiceID (FR-G26). store is the gateway-owned Store the PAS legs
// use (pended ledger + EOB); a nil store is valid for a read-only-only native
// responder. clock is used for the gateway-projected EOB `created`; nil ⇒ time.Now.
//
// Returns the concrete *nativeResponder (not the LegResponder interface): every
// existing caller assigns the result to a LegResponder-typed slot or field (still
// legal — *nativeResponder satisfies LegResponder, var _ below), but app.go's
// checks-runner wiring needs SetEndpointEvidence, which is not part of the public
// LegResponder seam — a caller that only wants LegResponder narrows implicitly.
func NewNativeResponder(client *http.Client, baseURL, crdServiceID string, store Store, clock func() time.Time, opts ...NativeOption) *nativeResponder {
	if clock == nil {
		clock = time.Now
	}
	n := &nativeResponder{
		client: client, baseURL: baseURL, cdsBaseURL: baseURL, crdServiceID: crdServiceID, store: store, clock: clock,
		pendReQueryTimeout: defaultPendReQueryTimeout, pendReQueryInterval: defaultPendReQueryInterval,
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// SetEndpointEvidence WHOLESALE-replaces the responder's per-line endpoint
// evidence (HRex per-version endpoint selection): app.go's checks-runner
// hook feeds it the davinci-config probe's
// retained "<contract>@<line>" -> URL map (checks.Capability.EndpointURLs)
// once per completed checks cycle. No TTL — this cycle's evidence replaces
// last cycle's outright; a token absent this time is simply gone (never
// sticky), and an empty/nil map clears everything back to the configured
// base.
//
// SAME-ORIGIN TRUST RULE (binding project ruling): an entry is honored IFF its
// scheme+host+port equal n.baseURL's. A probe-published cross-origin
// endpoint is NEVER a target a PHI-bearing submission follows — a
// misconfigured or compromised davinci-configuration document must not be
// able to redirect traffic off the operator-configured partner.
// Non-same-origin (or unparseable) entries are DROPPED HERE, AT SET TIME —
// never stored, never resolved later — each noted via
// endpointEvidenceObserver (TestEndpointEvidenceSameOriginEnforced is the
// rejection test).
func (n *nativeResponder) SetEndpointEvidence(evidence map[string]string) {
	baseOrigin, baseOK := originOf(n.baseURL)
	kept := make(map[string]string, len(evidence))
	for tok, u := range evidence {
		entryOrigin, ok := originOf(u)
		if !baseOK || !ok || entryOrigin != baseOrigin {
			if n.endpointEvidenceObserver != nil {
				n.endpointEvidenceObserver(fmt.Sprintf(
					"engine: endpoint evidence for %q dropped (not same-origin as the configured base %s): %s",
					tok, baseOrigin, redactURLForLog(u)))
			}
			continue
		}
		kept[tok] = u
	}
	n.evMu.Lock()
	n.endpointEvidence = kept
	n.evMu.Unlock()
}

// EndpointEvidenceForTest returns a copy of the currently-held endpoint
// evidence — a READ-ONLY test-observation seam (the repo's *ForTest
// precedent: TransformPASForTest, SelectChainRouteForTest,
// NormalizePASResponseForTest), used by gateway/app's
// TestProbeEvidenceReachesResponder to prove evidence that arrived through
// the REAL checksRunner.Run → OnResults → SetEndpointEvidence wiring landed
// here — never a second, test-only write path.
func (n *nativeResponder) EndpointEvidenceForTest() map[string]string {
	n.evMu.RLock()
	defer n.evMu.RUnlock()
	out := make(map[string]string, len(n.endpointEvidence))
	for k, v := range n.endpointEvidence {
		out[k] = v
	}
	return out
}

// defaultPortForScheme is the scheme's implicit port ("" for a scheme with
// none) — RFC 3986 §6.2.3: "http://x" and "http://x:80" name the SAME
// origin. Without this, a same-origin comparison done by exact string
// equality (the original shape) drops ALL evidence the moment the operator
// base and the partner-published URL disagree on whether to spell out the
// scheme's own default port — a common, unremarkable config shape, not an
// attack — silently disabling endpoint evidence for that deployment while looking, in the
// logs, exactly like a real cross-origin rejection.
func defaultPortForScheme(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// originOf reduces raw to its normalized "scheme://host:port" origin — an
// explicit port equal to the scheme's default is treated identically to no
// port at all (defaultPortForScheme). ok is false for an unparseable URL or
// one missing a scheme/host (never same-origin to anything).
func originOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(u.Scheme)
	}
	host := u.Hostname()
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host, true
}

// redactURLForLog keeps only scheme+host+port for a log line (the REDACTION
// RULE this codebase applies to every operator-visible string derived from
// partner input — checks.targetOf's exact precedent): a rejected entry's
// path/query is peer-published, untrusted, and never surfaced verbatim.
func redactURLForLog(raw string) string {
	if origin, ok := originOf(raw); ok {
		return origin
	}
	return "(unparseable)"
}

// resolvedURL is the per-line endpoint resolution: the post URL for a leg
// routed at contract@line, preferring same-origin-validated probe evidence
// over the configured base+path. contract == "" (version-neutral leg) or no
// answer line resolved on ctx, or no evidence for that exact token, all fall
// back to base+path UNCHANGED — the fence (TestNativeForwardSelectsLineEndpoint's
// byte-identical case). Read under RLock; SetEndpointEvidence is the sole
// writer (wholesale replace under Lock) — concurrency-clean under -race.
func (n *nativeResponder) resolvedURL(ctx context.Context, contract, base, path string) string {
	def := base + path
	if contract == "" {
		return def
	}
	line := answerLineOr(ctx, contract)
	if line == "" {
		return def
	}
	n.evMu.RLock()
	u, ok := n.endpointEvidence[contract+"@"+line]
	n.evMu.RUnlock()
	if !ok {
		return def
	}
	return u
}

// DiscoverCRDServiceID resolves the partner's CDS Hooks order-select service id from
// GET {base}/cds-services (FR-G26, OWD-G8). If override is non-empty it is returned
// immediately (escape hatch for partners whose hook name differs from SHN's
// origination hook — e.g. br-payer's order-sign service). Otherwise the listing is
// fetched, filtered to services whose hook matches SHN's crdHook ("order-select"),
// and the id of exactly one match is returned. Zero matches → error (fail-closed);
// multiple matches → error (ambiguous; set PAYER_DAVINCI_CRD_SERVICE_ID). A
// non-2xx or parse error is a fatal boot error (fail-closed per FR-G26).
func DiscoverCRDServiceID(ctx context.Context, client *http.Client, base, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	url := base + "/cds-services"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("engine: build GET %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("engine: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return "", fmt.Errorf("engine: read %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("engine: GET %s returned %s", url, resp.Status)
	}
	var listing struct {
		Services []struct {
			ID   string `json:"id"`
			Hook string `json:"hook"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return "", fmt.Errorf("engine: parse %s: %w", url, err)
	}
	var matches []string
	for _, svc := range listing.Services {
		if svc.Hook == crdHook {
			matches = append(matches, svc.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("engine: no %q service at %s (set PAYER_DAVINCI_CRD_SERVICE_ID to override)", crdHook, url)
	default:
		return "", fmt.Errorf("engine: ambiguous: %d %q services at %s; set PAYER_DAVINCI_CRD_SERVICE_ID to select one", len(matches), crdHook, url)
	}
}

// markForeignRelay marks a LegResult as a verbatim foreign-far-end relay: ResponseFHIR (when
// present) is the real RI's bytes in the RI's OWN patient namespace. The engine then skips the
// response member-fence (R-7) and the response egress-$validate (R-8) for this result, while
// fencing+validating the SHN-produced side-effects (EOB) unconditionally. Both flags are set
// together here (native = produced-by-foreign AND foreign-namespace); the conformant-mock north
// star is the only producer that would set them apart. Single declaration site per leg
// (covers every internal return of handlePASClaim*Native) — fail-closed if ever missed (zero value
// = strict fence + $validate).
func markForeignRelay(r LegResult) LegResult {
	r.ResponseRelayed = true
	r.ResponseSubjectForeign = true
	return r
}

func (n *nativeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	// Foreign-peer version filter: same rule as the
	// substrate OriginateLeg filter, sourced from the operator's per-peer
	// declaration instead of the registry. Refuse-before-forward: a refused
	// leg sends ZERO bytes to the partner.
	//
	// This stays ARM-1-ONLY (selectContractToken,
	// intersection-only) BY CONSTRUCTION — the forwarded body is PROVIDER-
	// BUILT bytes, not this gateway's own build product, so re-labeling or
	// chaining it under a native-reach/transform-chain token is out of scope
	// (transform-at-the-forward-edge is a recorded deferral; it goes live
	// together with the strictExtensions flag, which is dormant plumbing for
	// exactly this reason). Pinned by
	// TestNativeForwardStaysArm1.
	contract, cerr := legContract(leg)
	if cerr != nil {
		// ok-guard: the unchecked map index this replaces returned the
		// zero legSpec for an unknown leg, whose empty Contract silently SKIPPED the
		// foreign-peer version filter. A legType this responder does not know must
		// fail closed, not forward unfiltered bytes to the partner.
		return LegResult{}, cerr
	}
	if contract != "" && len(n.declaredContractVersions) > 0 {
		// D1a: the DECLARED set, not the library build constant — a deployment that
		// declares 2.2 must be willing to forward to a 2.2-only partner.
		own := n.ownDeclared()
		if _, refused := selectContractToken(own, n.declaredContractVersions, true, contract); refused {
			return LegResult{
				Status: http.StatusUnprocessableEntity,
				Message: (&RouteRefusalError{
					Contract: contract, LegType: leg, Recipient: "partner Da Vinci endpoint",
					Own:  sortedTokens(contract, contractLineSet(own, contract)),
					Peer: sortedTokens(contract, contractLineSet(n.declaredContractVersions, contract)),
				}).Error(),
			}, nil
		}
	}
	// NOTE: there is deliberately NO "coverage-eligibility" arm here. Eligibility is a
	// first-class engine handler (R11): handleEligibilityInbound answers it directly off
	// the member's own Coverage in the payer's SoR and never routes it through a
	// LegResponder, so a native arm for it was unreachable and retired with the in-process
	// payer stub (§3.1/§3.2). A payer that wants a partner to decide eligibility deploys the
	// standalone SDK Responder, whose Eligibility method is untouched.
	switch leg {
	case "crd-order-select":
		// The request is ALREADY a conformant CDS Hooks request (br-provider's bytes via
		// the ingress); forward it VERBATIM — no augmentCRDHook minimized shaping.
		// Rung-1 faithful pass-through: br-payer receives br-provider's actual request
		// bytes. The response side is identical to the minimized leg (FR-G25).
		// When crdHookOverride is set, rewrite the request's top-level "hook" field to
		// match the partner CRD service's declared hook (e.g. br-payer's order-sign-crd
		// demands hook:order-sign while SHN originates hook:order-select — 400 otherwise).
		fwd := requestFHIR
		if n.crdHookOverride != "" {
			rewritten, herr := rewriteCDSHook(requestFHIR, n.crdHookOverride)
			if herr != nil {
				return LegResult{}, herr // malformed request envelope → 500 (gateway fault)
			}
			fwd = rewritten
		}
		// Per-partner egress shaping (AFTER the payer-side bind + ingress-$validate read the
		// bare Coverage, crd_native.go): wrap prefetch.coverage bare→searchset-Bundle so the partner's
		// order-sign search-template `coverage` prefetch is satisfied (bare → 412 "Missing Coverage").
		if n.crdCoverageBundle {
			wrapped, werr := wrapCRDCoverageSearchset(fwd)
			if werr != nil {
				return LegResult{}, werr // malformed request envelope → 500 (gateway fault)
			}
			fwd = wrapped
		}
		body, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+n.crdServiceID, fwd, "CRD")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		res, nerr := normalizeCRDResponse(answerLineOr(ctx, contract), body)
		if nerr != nil {
			return res, nerr
		}
		// Same scope as the coverage-bundle wrap: relay the partner's CRD
		// systemActions — the coverage-annotated order carrying the coverage-assertion-id —
		// alongside the SHN cards, so the provider can drive DTR. The partner's $questionnaire-package
		// REQUIRES that CRD-updated order (it 501s "ServiceRequest without a Coverage Assertion Id
		// extension is not supported"); the normalized-cards-only response drops it. This is the
		// faithful Da Vinci CRD→DTR handoff. br-payer (flag off) is byte-unchanged.
		if n.crdCoverageBundle {
			res.ResponseFHIR = mergeCRDSystemActions(res.ResponseFHIR, body)
		}
		return res, nil

	case "dtr-questionnaire-fetch":
		// Parse the published leg request: canonical (required) + an OPTIONAL coverage
		// resource carried verbatim from the inbound $questionnaire-package (FR-G28).
		// Fail-closed posture: malformed JSON or a missing/empty canonical → 400 (parity
		// with a malformed-request 400, not a 500).
		var fetch dtrLegRequest
		if err := json.Unmarshal(requestFHIR, &fetch); err != nil || (fetch.Canonical == "" && len(fetch.Order) == 0) {
			return LegResult{Status: http.StatusBadRequest, Message: "parse questionnaire fetch failed"}, nil
		}
		if len(fetch.NextQuestion) > 0 {
			// An SDC adaptive $next-question round (dtr_adaptive.go): forward the carried
			// QuestionnaireResponse as the op's questionnaire-response input and relay the
			// partner's answer VERBATIM — the same stamp-honesty posture as the package relay
			// below (bytes this build did not produce: egress $validate stands down, the
			// frame stays unstamped). The answer's subject is fenced on the payer side
			// (payer.go) against the request's, and on the provider side against the patient.
			params, err := buildNextQuestionParameters(fetch.NextQuestion)
			if err != nil {
				return LegResult{}, err // marshal fault → 500 (gateway fault)
			}
			nqURL := n.resolvedURL(ctx, contract, n.baseURL, "/Questionnaire/$next-question")
			body, bad, err := n.post(ctx, nqURL, "", params, "DTR next-question")
			if err != nil {
				return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
			}
			if bad.Status != 0 {
				return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
			}
			return LegResult{ResponseFHIR: body, ResponseRelayed: true}, nil
		}
		// Two shapes: an ORDER-driven request (the CRD-updated order carries the
		// coverage-assertion-id the partner keys the questionnaire off; it has no `questionnaire` param)
		// or the canonical request (br-payer). The provider's coverage is carried through
		// in both so the partner's required `coverage` parameter is satisfied (FR-G28) — coverage is
		// already ALWAYS present at this call site (originate.go attaches it for every br-payer-targeting
		// leg, and now also whenever the selected DTR line requires it — DTRDef.QuestionnairePackageCoverageRequired),
		// so the AtLine coverage-1..1 gate below is a signature change here, not a behavior change.
		line := answerLineOr(ctx, contract)
		var params []byte
		var err error
		if len(fetch.Order) > 0 {
			params, err = buildQuestionnairePackageOrderRequestAtLine(line, fetch.Order, fetch.Coverage)
		} else {
			params, err = buildQuestionnairePackageRequestAtLine(line, fetch.Canonical, fetch.Coverage)
		}
		if err != nil {
			// A coverage-1..1 refusal (dtrPackageRequireCoverage) is a legible LOCAL
			// refusal REPLACING the partner's opaque 400 — surface
			// it the same way, not the generic 500 an unknown-line build fault maps to.
			return LegResult{Status: http.StatusBadRequest, Message: err.Error()}, nil
		}
		// Endpoint evidence: prefer the probe-retained, same-origin-validated
		// #<line> endpoint for THIS routed line over the configured base+path (evidence absent
		// ⇒ unchanged base+path, the fence).
		dtrURL := n.resolvedURL(ctx, contract, n.baseURL, "/Questionnaire/$questionnaire-package")
		body, bad, err := n.post(ctx, dtrURL, "", params, "DTR")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		// Forward the partner's $questionnaire-package Bundle VERBATIM (the
		// dependent Libraries/ValueSets are preserved for Step 3). The package→
		// Questionnaire extraction — and the no-Questionnaire 502 — is now a consumer
		// concern (originate.go), so this leg no longer inspects the body.
		//
		// ResponseRelayed marks it as bytes THIS BUILD DID NOT PRODUCE (the
		// stamp-honesty rule). Two consequences, both the same rule pas_native already
		// follows: the egress $validate stands down (R-8 — a foreign Da Vinci DTR
		// package declares profiles SHN's validator cannot resolve; this replaces
		// payer.go's coarser PayerDavinciNative gate with the per-RESULT truth), and
		// the response frame is left UNSTAMPED — SHN asserts nothing about the contract
		// line of a partner's bytes.
		//
		// Deliberately NOT markForeignRelay: that also sets ResponseSubjectForeign,
		// which would stand down the trust-critical "no Questionnaire may carry a
		// subject" fence (payer.go's fenceResponseSubject). Only the relay flag applies
		// here.
		return LegResult{ResponseFHIR: body, ResponseRelayed: true}, nil

	case "crd-order-dispatch":
		if n.crdDispatchServiceID == "" {
			return LegResult{Status: http.StatusBadGateway, Message: "crd-order-dispatch not configured (set PAYER_DAVINCI_DISPATCH_SERVICE_ID)"}, nil
		}
		fwd := requestFHIR
		if n.crdDispatchHook != "" {
			rewritten, herr := rewriteCDSHook(requestFHIR, n.crdDispatchHook)
			if herr != nil {
				return LegResult{}, herr // malformed request envelope → 500 (gateway fault)
			}
			fwd = rewritten
		}
		body, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+n.crdDispatchServiceID, fwd, "CRD-dispatch")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		return normalizeCRDResponse(answerLineOr(ctx, contract), body)

	case "pas-claim":
		res, err := n.handlePASClaimNative(ctx, corrID, subjectPCI, requestFHIR)
		return markForeignRelay(res), err

	case "pas-claim-update":
		res, err := n.handlePASClaimUpdateNative(ctx, corrID, subjectPCI, requestFHIR)
		return markForeignRelay(res), err

	default:
		// The br-payer-targeting lane routes the read-only + PAS legs here; this is defensive
		// for an unrouted leg.
		return LegResult{}, fmt.Errorf("engine: nativeResponder: unhandled leg %q", leg)
	}
}

// post forwards body to base+path. An upstream that RETURNS an HTTP response — any
// status — is the recipient's answer: 2xx → (body, LegResult{}, nil); non-2xx →
// (nil, LegResult{Status:<code>, ResponseFHIR:<upstream body>}, nil) for verbatim
// relay. A NO-RESPONSE fault (build/dial/read) is (nil, LegResult{}, error) → the
// engine maps it to 500 → "hub routing failed".
func (n *nativeResponder) post(ctx context.Context, base, path string, body []byte, label string) ([]byte, LegResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s read failed: %w", label, err)
	}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return nil, LegResult{}, fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb))
		}
		return nil, LegResult{Status: resp.StatusCode, ResponseFHIR: rb}, nil
	}
	return rb, LegResult{}, nil
}

// get reads base+path (the read sibling of post), reusing the same authed client. Used by the PAS
// pend re-query (GET ClaimResponse/{id}). Same relay-non-2xx / error-on-no-response contract as post.
func (n *nativeResponder) get(ctx context.Context, base, path, label string) ([]byte, LegResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s read failed: %w", label, err)
	}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return nil, LegResult{}, fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb))
		}
		return nil, LegResult{Status: resp.StatusCode, ResponseFHIR: rb}, nil
	}
	return rb, LegResult{}, nil
}

// normalizeCRDResponse is the CRD response tail (FR-G25): normalize the partner's
// coverage-information to the canonical CardCoverage (davincimap.go), then re-render
// SHN cards with shnsdk.BuildCards. Used by the conformant crd-order-select
// native CRD case. Fails closed (Status 502) when no canonical coverage is resolvable.
func normalizeCRDResponse(line string, body []byte) (LegResult, error) {
	cov, lr := normalizeCRDCoverage(body)
	if lr.Status != 0 {
		return lr, nil
	}
	cardsJSON, err := shnsdk.BuildCardsAtLine(line, cov)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: render normalized cards: %w", err)
	}
	return LegResult{ResponseFHIR: cardsJSON}, nil
}

// mergeCRDSystemActions returns cardsJSON with the partner's raw CRD `systemActions` merged in
// (the coverage-annotated order the provider needs to drive DTR). A no-op when the partner
// returned no systemActions or either side is unparseable — the SHN cards pass through unchanged.
func mergeCRDSystemActions(cardsJSON, rawResp []byte) []byte {
	var raw struct {
		SystemActions json.RawMessage `json:"systemActions"`
	}
	if err := json.Unmarshal(rawResp, &raw); err != nil || len(raw.SystemActions) == 0 {
		return cardsJSON
	}
	var cards map[string]json.RawMessage
	if err := json.Unmarshal(cardsJSON, &cards); err != nil {
		return cardsJSON
	}
	cards["systemActions"] = raw.SystemActions
	if merged, err := json.Marshal(cards); err == nil {
		return merged
	}
	return cardsJSON
}

// rewriteCDSHook returns reqJSON with its top-level "hook" set to hook, preserving every
// other field verbatim. Used to adapt SHN's canonical order-select request to a partner
// CRD service registered under a different hook (br-payer's order-sign-crd).
func rewriteCDSHook(reqJSON []byte, hook string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: rewrite CDS hook: %w", err)
	}
	hookJSON, err := json.Marshal(hook)
	if err != nil {
		return nil, fmt.Errorf("engine: rewrite CDS hook: %w", err)
	}
	m["hook"] = hookJSON
	return json.Marshal(m)
}

// wrapCRDCoverageSearchset rewrites a CDS Hooks request's prefetch.coverage from a BARE
// Coverage resource into a searchset Bundle wrapping it verbatim, preserving every other field.
// A partner whose `coverage` prefetch is a SEARCH template (Coverage?beneficiary=…, the partner's
// order-sign) requires the searchset shape; the SHN spine carries a bare Coverage. No-op
// (returns reqJSON unchanged) when prefetch or coverage is absent, or coverage is already a
// Bundle (idempotent — never a Bundle-in-a-Bundle).
func wrapCRDCoverageSearchset(reqJSON []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage: %w", err)
	}
	pfRaw, ok := m["prefetch"]
	if !ok {
		return reqJSON, nil
	}
	var pf map[string]json.RawMessage
	if err := json.Unmarshal(pfRaw, &pf); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage prefetch: %w", err)
	}
	covRaw, ok := pf["coverage"]
	if !ok {
		return reqJSON, nil
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	if err := json.Unmarshal(covRaw, &probe); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage resource: %w", err)
	}
	if probe.ResourceType != "Coverage" {
		return reqJSON, nil // absent-shaped or already a Bundle — leave verbatim (idempotent)
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"entry":        []any{map[string]any{"resource": covRaw}},
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal crd coverage searchset: %w", err)
	}
	pf["coverage"] = bundleJSON
	pfJSON, err := json.Marshal(pf)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal crd prefetch: %w", err)
	}
	m["prefetch"] = pfJSON
	return json.Marshal(m)
}
