// The native responder forwards participant messages to trusted backend
// endpoints. The engine owns network authority, sealing, participant-selected
// content checks and audit; the backend owns clinical business decisions.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const maxPartnerBody = 8 << 20 // 8 MiB cap on a partner response body

const relayBodyCap = 6 << 20 // 6 MiB — headroom under the 8 MiB MaxResponseBytes for seal + wrapper

type nativeResponder struct {
	diagnostic func(diagnostics.Event) bool
	client     *http.Client
	baseURL    string // FHIR base ($questionnaire-package, $submit, CoverageEligibilityRequest)
	cdsBaseURL string // CDS Hooks base (/cds-services/{id}); defaults to baseURL when co-located
	// dtrBaseURL / pasBaseURL are the per-operation FHIR bases for a partner
	// that serves DTR (/Questionnaire/$questionnaire-package, $next-question)
	// and PAS (/Claim/$submit) from different bases. Empty ⇒ that operation uses
	// baseURL (dtrBase/pasBase apply the fallback) — the byte-identical fallback
	// every deployment that sets neither relies on. Published endpoint
	// evidence selects submission/package URLs only (endpointForDispatch);
	// inquiry/next-question keep their independently configured base and path.
	dtrBaseURL string
	pasBaseURL string
	// crdServiceID and crdDispatchServiceID optionally name the partner's CDS service
	// for the order-select and order-dispatch legs; empty selects the listed service
	// whose hook is the request's hook (crdservice.go). Either way the service must be
	// listed for the request's hook.
	crdServiceID         string
	crdDispatchServiceID string
	// cds caches the partner's CDS service listing.
	cds   cdsServiceListing
	store Store // gateway-owned shadow ledger + EOB Store for the PAS legs (nil ⇒ read-only only)
	clock func() time.Time
	// declaredContractVersions is the operator-declared token set for the
	// foreign partner (PAYER_DAVINCI_CONTRACT_VERSIONS) — the peer-config
	// source of the routing filter. Empty = silent peer:
	// admit absent or builder-known request declarations only. A future request
	// requires explicit matching endpoint configuration.
	declaredContractVersions []string
	responseDeclarations     NativeResponseDeclarations
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
	// hook); endpointForDispatch reads it per-leg. Both run concurrently with the
	// request-serving path (Handle may be mid-flight on another goroutine
	// while a checks cycle completes), so this is a real mutex, not a
	// construction-time-only field like the options above.
	evMu sync.RWMutex
	// endpointEvidence is "<contract>@<line>" -> the partner's published
	// per-version operation URL, ALREADY same-origin-validated at set time
	// (SetEndpointEvidence drops anything else). nil/absent-token ⇒
	// endpointForDispatch falls back to the configured base+path — the fence
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

	// payorEdgeOwn / payorEdgeBackend implement the payer-edge identity mapping seam
	// (payoredge.go, PAYER_DAVINCI_PAYOR_OWN / PAYER_DAVINCI_PAYOR_BACKEND): when both
	// set, the CRD/DTR/PAS legs re-stamp the inbound Coverage's payor identity from
	// an identity this gateway OWNS to payorEdgeBackend — anything else refuses loudly
	// (fail-closed). nil (the default) ⇒ seam off, every leg forwards its Coverage payor
	// verbatim — byte-identical to every deployment that does not set the two env vars.
	payorEdgeOwn     *shnsdk.PayerIdentifier
	payorEdgeBackend *shnsdk.PayerIdentifier
	// payorEdgePublished reports the payer identities this holder itself publishes on
	// the network feed — the other half of "own" (payoredge.go, ownPayerIdentities).
	// nil, or a holder with no published identity, ⇒ payorEdgeOwn alone decides.
	payorEdgePublished func() []shnsdk.PayerIdentifier

	// backendHeaders are fixed request headers a partner system routes on (a
	// tenant or plan key its API gateway reads before any payload). They go on
	// every request this responder sends the partner — listing read, CRD, DTR,
	// PAS — and never on the token endpoint (the token client
	// builds its own request). Addressing for the participant's own system,
	// never a change to the message: the body bytes are untouched. nil ⇒ none.
	backendHeaders http.Header

	// conformance is the policy of the gateway this responder runs in, passed
	// as an option because NewNativeResponder runs before engine.New. The zero
	// value is none, matching Config and the participant default.
	conformance ConformancePolicy
	// emitFinding is bound by engine.New (bindFindingEmitter) because no
	// gateway exists when this responder is constructed. nil is safe.
	emitFinding func(ConformanceFinding)
}

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

// WithConformancePolicy gives the responder the enforcement policy of the
// gateway it runs in. Unset means the zero value, none.
func WithConformancePolicy(p ConformancePolicy) NativeOption {
	return func(n *nativeResponder) { n.conformance = p }
}

// ConformanceLevelForTest exposes this responder's own configured enforcement
// level — test-only introspection (the EndpointEvidenceForTest pattern)
// proving a WithConformancePolicy option (or its absence, which leaves the
// zero value, none) actually reached this responder, not just whatever
// engine.Config a caller assembled.
func (n *nativeResponder) ConformanceLevelForTest() ConformanceEnforcement {
	return n.conformance.Level()
}

// bindFindingEmitter receives the engine's finding emitter after engine.New
// builds the gateway. It cannot be a NativeOption: NewNativeResponder runs
// before engine.New, so no gateway exists when the options are applied.
func (n *nativeResponder) bindFindingEmitter(emit func(ConformanceFinding)) { n.emitFinding = emit }

// WithDTRBaseURL overrides the base used for the DTR forwards
// (/Questionnaire/$questionnaire-package and $next-question), for partners whose DTR
// endpoint is NOT co-located with their PAS/FHIR base. Unset ⇒ DTR posts use
// the FHIR baseURL (co-located default, the prior behavior).
func WithDTRBaseURL(dtrBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if dtrBaseURL != "" {
			n.dtrBaseURL = dtrBaseURL
		}
	}
}

// WithPASBaseURL overrides the base used for the PAS forwards (/Claim/$submit for
// submit and update; a later PAS operation belongs here too), for partners whose PAS
// endpoint is NOT co-located with their DTR/FHIR base. Unset ⇒ PAS posts use
// the FHIR baseURL (co-located default, the prior behavior).
func WithPASBaseURL(pasBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if pasBaseURL != "" {
			n.pasBaseURL = pasBaseURL
		}
	}
}

// WithBackendHeaders sets fixed request headers for a partner system that
// routes on one (PAYER_DAVINCI_BACKEND_HEADERS). Every request to the partner's
// bases carries them; the token endpoint never does. nil or empty ⇒ nothing added.
func WithBackendHeaders(h http.Header) NativeOption {
	return func(n *nativeResponder) {
		if len(h) == 0 {
			return
		}
		n.backendHeaders = h.Clone()
	}
}

// applyBackendHeaders adds the partner's fixed request headers to req.
func (n *nativeResponder) applyBackendHeaders(req *http.Request) {
	for k, v := range n.backendHeaders {
		req.Header[k] = append([]string(nil), v...)
	}
}

// WithCRDDispatchService names the partner's CDS service for the crd-order-dispatch
// leg. Empty (the default) selects the listed service for the order-dispatch hook.
func WithCRDDispatchService(serviceID string) NativeOption {
	return func(n *nativeResponder) { n.crdDispatchServiceID = serviceID }
}

// WithDeclaredContractVersions supplies the operator-declared contract tokens
// for the partner endpoint. Unsupported operations or independently declared
// request representations refuse before backend dispatch.
func WithDeclaredContractVersions(tokens []string) NativeOption {
	return func(n *nativeResponder) { n.declaredContractVersions = append([]string(nil), tokens...) }
}

// WithOwnContractVersions is retained for source compatibility.
// Deprecated: native forwarding uses the actual request representation and the
// backend's independent declaration; gateway builder defaults do not gate it.
func WithOwnContractVersions(_ []string) NativeOption {
	return func(*nativeResponder) {}
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

// WithPayorEdgeIdentity turns on the payer-edge identity mapping seam (payoredge.go):
// own is the configured half of this deployment's payer identity (the assertion side —
// WithPayorEdgePublishedIdentities supplies the other half, the identities this holder
// publishes on the network feed), backend
// is the identifier this responder's backend leg knows itself by (the re-stamp target).
// Unset (the zero-value NativeOption slice) ⇒ seam off, every CRD/DTR/PAS leg forwards
// its Coverage payor verbatim (the prior behavior). Config loading enforces the
// all-or-nothing rule (PAYER_DAVINCI_PAYOR_OWN / _BACKEND); this option itself has no
// partial form.
func WithPayorEdgeIdentity(own, backend shnsdk.PayerIdentifier) NativeOption {
	return func(n *nativeResponder) {
		o, b := own, backend
		n.payorEdgeOwn, n.payorEdgeBackend = &o, &b
	}
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
// crdServiceID optionally names the partner's CDS service for the order-select leg
// ("" selects the listed service for the request's hook; FR-G26). store is the
// gateway-owned Store the PAS legs
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
// scheme+host+port equal those of the base ITS CONTRACT forwards to
// (contractBase: the DTR base for pa.dtr, the PAS base for pa.pas, the
// shared base otherwise — each of which IS the shared base unless a
// per-operation base is configured). Judged per contract base, never
// the shared base alone: a split-base partner must not be fenced out of its
// own published DTR endpoint (TestEndpointEvidenceFenceJudgedPerContractBase).
// A probe-published cross-origin
// endpoint is NEVER a target a PHI-bearing submission follows — a
// misconfigured or compromised davinci-configuration document must not be
// able to redirect traffic off the operator-configured partner.
// Non-same-origin (or unparseable) entries are DROPPED HERE, AT SET TIME —
// never stored, never resolved later — each noted via
// endpointEvidenceObserver (TestEndpointEvidenceSameOriginEnforced is the
// rejection test).
func (n *nativeResponder) SetEndpointEvidence(evidence map[string]string) {
	kept := make(map[string]string, len(evidence))
	for tok, u := range evidence {
		contract, _, _ := strings.Cut(tok, "@")
		baseOrigin, baseOK := originOf(n.contractBase(contract))
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

// dtrBase / pasBase are the per-operation bases with their fallback applied:
// the configured DTR/PAS base when set, the shared base otherwise. Every DTR
// and PAS forward reads through these (never the raw fields), so a responder
// built without the constructor defaults still forwards to the shared base.
func (n *nativeResponder) dtrBase() string {
	if n.dtrBaseURL != "" {
		return n.dtrBaseURL
	}
	return n.baseURL
}

func (n *nativeResponder) pasBase() string {
	if n.pasBaseURL != "" {
		return n.pasBaseURL
	}
	return n.baseURL
}

// contractBase is the configured base a contract's operations forward to: the
// DTR base for pa.dtr, the PAS base for pa.pas, the shared base for everything
// else. Each per-operation base defaults to the shared base, so a
// deployment that sets neither resolves every contract to baseURL exactly as
// before. It is the base the same-origin fence judges evidence against AND the
// fallback endpointForDispatch appends the operation path to.
func (n *nativeResponder) contractBase(contract string) string {
	switch contract {
	case "pa.dtr":
		return n.dtrBase()
	case "pa.pas":
		return n.pasBase()
	}
	return n.baseURL
}

func (n *nativeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	// A configured backend declaration establishes operation/representation
	// capability independently of this gateway's own builder lines. Preserve
	// real incompatibility without inferring a payload line from route choice.
	contract, cerr := legContract(leg)
	if cerr != nil {
		// ok-guard: the unchecked map index this replaces returned the
		// zero legSpec for an unknown leg, whose empty Contract silently SKIPPED the
		// foreign-peer version filter. A legType this responder does not know must
		// fail closed, not forward unfiltered bytes to the partner.
		return LegResult{}, cerr
	}
	if !n.admitsNativeRequest(ctx, contract) {
		return LegResult{Status: http.StatusUnprocessableEntity, Message: "backend does not declare this operation and representation"}, nil
	}
	// NOTE: there is deliberately NO "coverage-eligibility" arm here. Eligibility is a
	// first-class engine handler (R11): handleEligibilityInbound answers it directly off
	// the member's own Coverage in the payer's SoR and never routes it through a
	// LegResponder, so a native arm for it was unreachable and retired with the in-process
	// payer stub (§3.1/§3.2). A payer that wants a partner to decide eligibility deploys the
	// standalone SDK Responder, whose Eligibility method is untouched.
	// The network's request, as it arrived. Each leg sends it exactly, or with only
	// the registered payer-identity edit.
	in := relay.NewBody(requestFHIR, relay.OriginPeerFrame)
	switch leg {
	case "crd-order-select", "crd-order-dispatch":
		return n.forwardCRD(ctx, contract, leg, in)

	case "dtr-questionnaire-fetch":
		// A request frame that names the operation carries that operation's own input,
		// which is sent to the payer's system exactly (or with only the payer identity
		// mapped). A request that names none is refused.
		switch op := RequestFrameOperation(ctx); op {
		case shnsdk.FrameOperationQuestionnairePackage:
			return n.forwardDTROperation(ctx, in, "/Questionnaire/$questionnaire-package", payorEdgeDTRParameters, leg, "DTR")
		case shnsdk.FrameOperationNextQuestion:
			return n.forwardDTROperation(ctx, in, "/Questionnaire/$next-question", 0, leg, "DTR next-question")
		case "":
			// The older request envelope, which carried a canonical and a coverage
			// in place of the operation's own input, is no longer read.
			return LegResult{Status: http.StatusBadRequest, Message: refusalDTRUnframed}, nil
		default:
			return LegResult{Status: http.StatusBadRequest, Message: "unsupported DTR operation"}, nil
		}

	case "pas-claim":
		res, err := n.handlePASClaimNative(ctx, corrID, subjectPCI, in, requestFHIR)
		return res, err

	case "pas-claim-update":
		res, err := n.handlePASClaimUpdateNative(ctx, corrID, subjectPCI, in, requestFHIR)
		return res, err

	case "pas-claim-inquire":
		// A read of the payer's own record about an authorization it pended
		// (inquire.go). It acquires no claim and writes nothing here: the ledger
		// effect is derived by the payer gateway from the answer.
		return n.handlePASInquireNative(ctx, in)

	default:
		// The br-payer-targeting lane routes the read-only + PAS legs here; this is defensive
		// for an unrouted leg.
		return LegResult{}, fmt.Errorf("engine: nativeResponder: unhandled leg %q", leg)
	}
}

// upstreamReply is a 2xx or non-2xx answer from the participant's own
// system: its bytes as they arrived, sealed (body) and as read by this
// gateway's own parsers (raw), and the media type it was answered with
// (application/fhir+json when the system named none).
type upstreamReply struct {
	status      int
	body        relay.Body
	raw         []byte
	contentType string
	// declared is the Content-Type the upstream sent ("" when it sent none).
	declared      string
	version       string
	versionSource string
}

// forwardDTROperation sends a framed DTR operation's own input to the payer's
// system at path and relays the answer exactly. When carrier is set, the
// payer identity of every coverage in the input is mapped (when the mapping
// is configured) and nothing else changes.
func (n *nativeResponder) forwardDTROperation(ctx context.Context, in relay.Body, path string, carrier payorEdgeCarrier, leg, label string) (LegResult, error) {
	fhirJSON := nativeRequestMedia(ctx, "application/fhir+json")
	request := relay.Exact(in, fhirJSON)
	if carrier != 0 {
		mapped, lr, err := n.payorEdgeRequest(in, carrier, fhirJSON)
		if err != nil || lr.Status != 0 {
			return lr, err
		}
		request = mapped
	}
	up, bad, err := n.post(ctx, n.dtrBase(), path, request, leg, label)
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	return LegResult{ApplicationStatus: up.status, ResponseContractVersion: up.version, ResponseVersionSource: up.versionSource, Response: relay.Exact(up.body, up.contentType)}, nil
}

// post snapshots the native endpoint, then forwards request p after checking it against
// the leg's row for a recipient's request to its own system (a refused
// payload is a no-response fault: nothing is sent). An upstream that RETURNS
// an HTTP response — any status — is the recipient's answer: 2xx →
// (reply, LegResult{}, nil); non-2xx → (reply, LegResult{Status:<code>,
// Response:<upstream body, relayed>}, nil) for verbatim relay. A NO-RESPONSE
// fault (build/dial/read) is (upstreamReply{}, LegResult{}, error) → the
// engine maps it to 500 → "hub routing failed".
func (n *nativeResponder) post(ctx context.Context, base, path string, p relay.Payload, leg, label string) (upstreamReply, LegResult, error) {
	k := relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	body, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		// The responder has no gateway: the refusal is logged here and
		// observed by the inbound handler (responderFailed).
		(*Gateway)(nil).ownershipRefused(k, err)
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s request not sent: %w", label, err)
	}
	endpoint := n.endpointForDispatch(ctx, leg, base, path)
	if !endpoint.admitted {
		return upstreamReply{}, LegResult{Status: http.StatusUnprocessableEntity, Message: "backend does not declare this operation and representation"}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.url, bytes.NewReader(body))
	if err != nil {
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	req.Header.Set("Content-Type", p.ContentType())
	req.Header.Set("Accept", "application/json")
	n.applyBackendHeaders(req)
	n.emitDiagnostic(ctx, "native.request", body, 0, "", req, req.Header)
	// A redirect is the participant's answer too. Following it would change
	// the answer and may dispatch a mutating operation twice.
	client := *n.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody+1))
	if err == nil && len(rb) > maxPartnerBody {
		err = fmt.Errorf("upstream response exceeds body limit")
	}
	n.emitDiagnostic(ctx, "native.response", rb, resp.StatusCode, diagnosticReadDetail(err, len(rb)), req, resp.Header)
	if err != nil {
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s read failed: %w", label, err)
	}
	up, bad, err := upstreamAnswer(resp, rb, label)
	if err == nil {
		up.version, up.versionSource = endpoint.version, endpoint.versionSource
		bad.ResponseContractVersion, bad.ResponseVersionSource = up.version, up.versionSource
	}
	return up, bad, err
}

// upstreamAnswer classifies a complete upstream operation response.
func upstreamAnswer(resp *http.Response, rb []byte, label string) (upstreamReply, LegResult, error) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/fhir+json"
	}
	reply := upstreamReply{status: resp.StatusCode, body: relay.NewBody(rb, relay.OriginUpstreamResponse), raw: rb, contentType: ct, declared: resp.Header.Get("Content-Type")}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb))
		}
		return reply, LegResult{ApplicationStatus: resp.StatusCode, Status: resp.StatusCode, Response: relay.Exact(reply.body, resp.Header.Get("Content-Type"))}, nil
	}
	return reply, LegResult{}, nil
}

// forwardCRD sends a CDS Hooks request to the partner service chosen by its hook:
// exactly as the network carried it, or with only the payer identity of its
// prefetch coverage mapped (when that mapping is configured). The hook and every
// other byte are never changed. The partner's answer is relayed exactly, with its
// own media type, once it passes the CDS Hooks response rules at the served CRD
// line; an answer that breaks one is refused (502) rather than repaired, and a
// non-2xx answer is relayed as the partner's error.
func (n *nativeResponder) forwardCRD(ctx context.Context, contract, leg string, in relay.Body) (LegResult, error) {
	serviceID, refused, err := n.selectCRDService(ctx, leg, in)
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	request, refused, err := n.payorEdgeRequest(in, payorEdgeCRDRequest, nativeRequestMedia(ctx, "application/json"))
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	up, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+serviceID, request, leg, "CRD")
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	// A CDS Hooks answer is JSON: one sent without a media type is carried as
	// application/json.
	ct := up.declared
	if ct == "" {
		ct = "application/json"
	}
	return LegResult{ApplicationStatus: up.status, ResponseContractVersion: up.version, ResponseVersionSource: up.versionSource, Response: relay.Exact(up.body, ct)}, nil
}

// certifyCDSHooksAnswer applies the CDS Hooks response rules (and, at a CRD
// line, the CRD card rules) to a participant's answer. It never changes the
// answer. Every violation is recorded as a finding at both levels; whether a
// refusing violation actually refuses is the receiving participant's choice,
// which the policy holds. A SHOULD-level finding is logged and the answer
// passes, as before.
//
// whose classifies WHOSE SYSTEM the certified bytes came from, for the
// finding: "own" when the bytes are this gateway's own backend answering
// (forwardCRD, certifying before it ever relays to a peer) or "peer" when
// they are a peer's answer received over the network (crdAnswerOutcome).
// This is independent of the refusal MESSAGE, which always names the
// participant the requester asked ("payer") regardless of which side is
// doing the certifying — a requester reading a refusal never sees "own".
//
// emit may be nil: a responder the engine never wired still certifies and
// still refuses at strict, it simply records nothing.
func certifyCDSHooksAnswer(ctx context.Context, policy ConformancePolicy, emit func(ConformanceFinding), body []byte, line, whose string) LegResult {
	violations := shnsdk.CheckCDSHooksResponse(body, line)
	if len(violations) == 0 {
		return LegResult{}
	}
	fc := findingContextFrom(ctx)
	var refusing, advisory []string
	var refuse bool
	for _, v := range violations {
		desc := v.Rule
		if v.Path != "" {
			desc += " at " + v.Path
		}
		if v.Severity != shnsdk.SeverityError {
			// One finding per violation (§5), advisory included: a SHOULD-level
			// finding never refuses, but it is still something the participant
			// should be able to read back.
			advisory = append(advisory, desc)
			if emit != nil {
				emit(ConformanceFinding{
					Kind: string(KindCDSEnvelope), Direction: "validate",
					LegType: fc.LegType, CorrelationID: fc.CorrelationID, Seam: fc.Seam,
					Whose: whose, Line: line,
					Level: policy.Level().String(), Decision: Record.String(), State: CheckInvalid,
					Rule: v.Rule, Path: v.Path,
					PayloadSHA256: sha256hex(body),
				})
			}
			continue
		}
		decision := policy.Decide(KindCDSEnvelope, v.Rule, VerdictInvalid)
		if decision == Refuse {
			refuse = true
			refusing = append(refusing, desc)
		}
		if emit != nil {
			emit(ConformanceFinding{
				Kind: string(KindCDSEnvelope), Direction: "validate",
				LegType: fc.LegType, CorrelationID: fc.CorrelationID, Seam: fc.Seam,
				Whose: whose, Line: line,
				Level: policy.Level().String(), Decision: decision.String(), State: CheckInvalid,
				Rule: v.Rule, Path: v.Path,
				PayloadSHA256: sha256hex(body),
			})
		}
	}
	if len(advisory) > 0 {
		log.Printf("gateway: payer CRD response: CDS Hooks recommendations not met: %s", strings.Join(advisory, "; "))
	}
	if !refuse {
		return LegResult{}
	}
	const shown = 5
	if len(refusing) > shown {
		refusing = append(refusing[:shown], fmt.Sprintf("and %d more", len(refusing)-shown))
	}
	return LegResult{
		Status:  http.StatusBadGateway,
		Message: "payer CRD response is not a valid CDS Hooks response: " + strings.Join(refusing, "; "),
	}
}

// applyPayorEdgeToCRDRequest is the CRD legs' payer-edge identity mapping
// (payoredge.go): maps the payer identity of every Coverage in the CDS Hooks request's
// prefetch.coverage (a bare Coverage or a Bundle) to n.payorEdgeBackend, ONLY when they
// name an identity this gateway owns (ownPayerIdentities: the identities this holder
// publishes on the network feed, union the configured one). Unconfigured, the request is
// sent exactly. Configured, a request with no prefetch.coverage at all refuses too — an
// absent one is itself the "no resolvable payor identifier" case, not a benign skip.
func (n *nativeResponder) applyPayorEdgeToCRDRequest(in relay.Body) (relay.Payload, LegResult, error) {
	return n.payorEdgeRequest(in, payorEdgeCRDRequest, "application/json")
}
