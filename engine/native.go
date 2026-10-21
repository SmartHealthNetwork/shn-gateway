// native.go — the native-forward payer LegResponder (Case 1). It
// forwards each read-only leg to a partner's real Da Vinci endpoint over a
// SMART-authenticated *http.Client and returns the partner's FHIR. The engine still
// owns authority (the (A)/(B) inbound fences + the (C) outbound subject fence, now
// defending a real party), sealing, edge $validate, and audit (AI-11). The PAS legs
// (workstream_pa_pas_forward.go) reuse the originator's PUBLISHED shnsdk parsers; the CRD legs send the
// request to the partner service chosen by its hook (workstream_pa_cds_services.go) and relay the
// partner's CDS Hooks answer exactly once it passes the CDS Hooks response rules
// (shnsdk.CheckCDSHooksResponse). It implements the internal, unstable
// engine.LegResponder (STABILITY: connectors/* is the supported surface); it
// graduates to connectors/davinci when LegResponder promotes to shnsdk.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const maxPartnerBody = 8 << 20 // 8 MiB cap on a partner response body

const relayBodyCap = 6 << 20 // 6 MiB — headroom under the 8 MiB MaxResponseBytes for seal + wrapper

// The two CDS service listing types below are the PA adapter's forward state,
// held by the core responder until the responder is constructed per workstream
// (tracked in the SHN platform repository as "The native responder is
// constructed per workstream: the CDS service listing leaves core
// native.go"). Only PA code uses them: the PA adapter's CRD forward and the
// payer responder's CDS listing wiring in package app.

// CDSService is one entry of a CDS service listing.
type CDSService struct {
	ID   string `json:"id"`
	Hook string `json:"hook"`
}

// cdsServiceListing caches the payer's listing. A listing younger than
// cdsServiceListingTTL is used as is; an older one is read again. One read
// runs at a time and nobody holds the lock while it runs: requests that still
// have a listing younger than cdsServiceListingMaxAge use it meanwhile, and
// requests without one wait for the read. A failed read keeps the last listing
// (up to cdsServiceListingMaxAge) and is remembered for
// cdsServiceListingRetryAfter.
type cdsServiceListing struct {
	mu       sync.Mutex
	services []CDSService
	read     time.Time
	failed   time.Time
	err      error
	// reading is closed when the read in progress ends; nil when none runs.
	reading chan struct{}
	// joined, when set, is called each time a request starts waiting for the
	// read in progress.
	joined func()
}

type nativeResponder struct {
	diagnostic DiagnosticSink
	client     *http.Client
	baseURL    string // FHIR base ($questionnaire-package, $submit, CoverageEligibilityRequest)
	cdsBaseURL string // CDS Hooks base (/cds-services/{id}); defaults to baseURL when co-located
	// dtrBaseURL / pasBaseURL are the per-operation FHIR bases for a partner
	// that serves DTR (/Questionnaire/$questionnaire-package, $next-question)
	// and PAS (/Claim/$submit) from different bases. Empty ⇒ that operation uses
	// baseURL (dtrBase/pasBase apply the fallback) — the byte-identical fallback
	// every deployment that sets neither relies on. Published endpoint
	// evidence (resolvedURL) still takes precedence over either.
	dtrBaseURL string
	pasBaseURL string
	// eligibilityURL is the payer's own coverage-eligibility endpoint, when it
	// declares one (WithEligibilityURL). Empty: the payer's gateway answers
	// eligibility from the payer's records itself.
	eligibilityURL string
	// crdServiceID and crdDispatchServiceID optionally name the partner's CDS service
	// for the order-select and order-dispatch legs; empty selects the listed service
	// whose hook is the request's hook (workstream_pa_cds_services.go). Either way the service must be
	// listed for the request's hook.
	crdServiceID         string
	crdDispatchServiceID string
	// cds caches the partner's CDS service listing.
	cds   cdsServiceListing
	store Store // gateway-owned shadow ledger Store for the PAS legs (nil ⇒ read-only only)
	clock func() time.Time
	// declaredContractVersions is the operator-declared token set for the
	// foreign partner (PAYER_DAVINCI_CONTRACT_VERSIONS) — the peer-config
	// source of the routing filter. Empty = silent peer:
	// forward at own line, never refuse (the same pre-contract tolerance the
	// substrate filter applies).
	declaredContractVersions []string
	// requireSharedLine is the participant's opt-in (REQUIRE_SHARED_LINE,
	// WithRequireSharedLine) to refusing a leg whose contract shares no line
	// with declaredContractVersions. Unset, such a leg is forwarded as sent and
	// recorded.
	requireSharedLine bool
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
	// noBackendCorrelation: the leg's id is not sent as X-Correlation-Id
	// (WithoutBackendCorrelation).
	noBackendCorrelation bool
	// backendDeadline bounds each call to the partner's system, measured
	// from the leg's arrival at this gateway (WithBackendDeadline); 0 sets
	// none of its own.
	backendDeadline time.Duration

	// conformance is the policy of the gateway this responder runs in, passed
	// as an option because NewNativeResponder runs before engine.New. The zero
	// value is strict, so a responder built without the option is today's
	// behavior.
	conformance ConformancePolicy
	// emitFinding is bound by engine.New (bindFindingEmitter) because no
	// gateway exists when this responder is constructed. nil is safe.
	emitFinding func(ConformanceFinding)
}

// NativeOption configures optional nativeResponder behavior.
type NativeOption func(*nativeResponder)

// RequireSharedLineForTest reports whether the participant's REQUIRE_SHARED_LINE
// reached this responder (WithRequireSharedLine), so the build that wires it
// can be proven from another package.
func (n *nativeResponder) RequireSharedLineForTest() bool { return n.requireSharedLine }

// WithBackendHeaders sets fixed request headers for a partner system that
// routes on one (PAYER_DAVINCI_BACKEND_HEADERS). Every request to the partner's
// bases carries them; the token endpoint never does. nil or empty ⇒ nothing added.
// X-Correlation-Id is never one of them: on a request to the partner's system
// it is only ever the leg's id, set by the gateway.
func WithBackendHeaders(h http.Header) NativeOption {
	return func(n *nativeResponder) {
		if len(h) == 0 {
			return
		}
		n.backendHeaders = h.Clone()
		for k := range n.backendHeaders {
			if strings.EqualFold(k, CorrelationHeader) {
				delete(n.backendHeaders, k)
			}
		}
	}
}

// WithoutBackendCorrelation stops the leg's id being sent to the partner's
// system as X-Correlation-Id (PAYER_DAVINCI_BACKEND_CORRELATION=off), for a
// system that validates that header its own way. The default sends it.
func WithoutBackendCorrelation() NativeOption {
	return func(n *nativeResponder) { n.noBackendCorrelation = true }
}

// WithBackendDeadline bounds each call to the partner's system at d from the
// leg's arrival at this gateway (PAYER_DAVINCI_BACKEND_TIMEOUT), so the work
// before the call (the member's lookup, a listing read, a token) counts
// against it too; a call outside an inbound leg counts from its own start. A
// system that has not answered by then is the partner's timeout: the
// requester is answered this gateway's own framed 504 while its leg budget
// still has room to carry it, instead of the requester giving up first and
// the call being cut short unanswered. d <= 0 sets no deadline of the
// responder's own.
func WithBackendDeadline(d time.Duration) NativeOption {
	return func(n *nativeResponder) { n.backendDeadline = d }
}

// applyBackendHeaders adds the partner's fixed request headers to req.
func (n *nativeResponder) applyBackendHeaders(req *http.Request) {
	for k, v := range n.backendHeaders {
		req.Header[k] = append([]string(nil), v...)
	}
}

// WithDeclaredContractVersions supplies the operator-declared contract tokens
// for the partner endpoint; legs whose contract shares no line refuse legibly
// instead of forwarding (the foreign-endpoint filter).
func WithDeclaredContractVersions(tokens []string) NativeOption {
	return func(n *nativeResponder) { n.declaredContractVersions = tokens }
}

// WithRequireSharedLine is the participant's opt-in to refusing, as routing,
// a leg whose contract shares no line with the partner's declared tokens
// (WithDeclaredContractVersions). Unset, such a leg is forwarded as sent and
// recorded.
func WithRequireSharedLine(on bool) NativeOption {
	return func(n *nativeResponder) { n.requireSharedLine = on }
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

// ownDeclared is this responder's declared-set accessor — the nativeResponder mirror
// of Gateway.declaredContractVersions(), with the same empty-means-build-default
// rule, so the two halves of "what do we speak" cannot diverge.
func (n *nativeResponder) ownDeclared() []string {
	if len(n.ownContractVersions) > 0 {
		return n.ownContractVersions
	}
	return DefaultContractVersions()
}

var _ LegResponder = (*nativeResponder)(nil)

// NewNativeResponder builds the native-forward Responder over a ready *http.Client
// (in production a smartauth bearer client; in tests a fixed-bearer client).
// crdServiceID optionally names the partner's CDS service for the order-select leg
// ("" selects the listed service for the request's hook; FR-G26). store is the
// gateway-owned Store the PAS legs
// use (the pend ledger); a nil store is valid for a read-only-only native
// responder. clock is the responder's own clock; nil ⇒ time.Now.
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

func (n *nativeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	ctx = withResponderCorrelation(ctx, corrID)
	// Its one forward to the payer's system is recorded, so a failure can say
	// whether the system was asked.
	exchangeOf(ctx).trackForward()
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
		if _, refused := selectContractToken(own, n.declaredContractVersions, true, contract); refused && !n.requireSharedLine {
			// No shared line: forwarded as sent by default, and recorded
			// (linecarry.go). Only the participant's own setting refuses.
			noteLineCarried(ctx, LineCarriedRequest, ObserverEvent{
				Direction: "ingress", LegType: leg, CorrelationID: corrID, Counterpart: "partner Da Vinci endpoint",
				Detail: "forwarded as sent; partner Da Vinci endpoint declares " +
					declaredOrNone(sortedTokens(contract, contractLineSet(n.declaredContractVersions, contract))) +
					"; this gateway speaks " + strings.Join(sortedTokens(contract, contractLineSet(own, contract)), ","),
			})
		} else if refused {
			exchangeOf(ctx).refusing(RefusalRouting)
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
	// The network's request, as it arrived. The workstream declaring the leg
	// sends it to the participant's own system (forwardFor): exactly, or with
	// only its registered edits.
	in := relay.NewBody(requestFHIR, relay.OriginPeerFrame)
	if forward := forwardFor(leg); forward != nil {
		return forward(n, ctx, contract, leg, corrID, subjectPCI, in, requestFHIR)
	}
	// The br-payer-targeting lane routes the read-only + PAS legs here; this is defensive
	// for an unrouted leg.
	return LegResult{}, fmt.Errorf("engine: nativeResponder: unhandled leg %q", leg)
}

// forwardHandler sends one leg the native responder has admitted (the core's
// version rule) to the participant's own system and answers with its reply:
// in is the network's request as it arrived, contract the leg's contract.
type forwardHandler func(n *nativeResponder, ctx context.Context, contract, leg, corrID, subjectPCI string, in relay.Body, requestFHIR []byte) (LegResult, error)

// upstreamReply is a 2xx or non-2xx answer from the participant's own
// system: its bytes as they arrived, sealed (body) and as read by this
// gateway's own parsers (raw), and the media type it was answered with
// (application/fhir+json when the system named none).
type upstreamReply struct {
	body        relay.Body
	raw         []byte
	contentType string
	// declared is the Content-Type the upstream sent ("" when it sent none).
	declared string
	// status is the HTTP status the system answered with.
	status int
}

// post forwards the request p on leg to base+path, after checking p against
// the leg's row for a recipient's request to its own system (a refused
// payload is a no-response fault: nothing is sent). An upstream that RETURNS
// an HTTP response — any status — is the recipient's answer: 2xx →
// (reply, LegResult{}, nil); non-2xx → (reply, LegResult{Status:<code>,
// Response:<upstream body, relayed>}, nil) for verbatim relay. A NO-RESPONSE
// fault (build/dial/read, or no answer by the responder's deadline) is
// (upstreamReply{}, LegResult{}, error) → responderFailed answers it as this
// gateway's own framed 502 or 504 (responderFailure).
func (n *nativeResponder) post(ctx context.Context, base, path string, p relay.Payload, leg, label string) (upstreamReply, LegResult, error) {
	k := relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	body, err := relay.Transmit(p, relay.Check(k))
	if err != nil {
		// The responder has no gateway: the refusal is logged here and
		// observed by the inbound handler (responderFailed).
		(*Gateway)(nil).ownershipRefused(k, err)
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s request not sent: %w", label, err)
	}
	// The call runs under the responder's own deadline, when it has one;
	// the request's end still cuts it short (callEndedClass reads which).
	callCtx := ctx
	if n.backendDeadline > 0 {
		arrived, ok := legArrival(ctx)
		if !ok {
			arrived = time.Now()
		}
		var cancel context.CancelFunc
		callCtx, cancel = context.WithDeadline(ctx, arrived.Add(n.backendDeadline))
		defer cancel()
	}
	timedOut := func() bool {
		return n.backendDeadline > 0 && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded)
	}
	// The gateway's own work before the call (validation, the member's
	// lookup) used the whole deadline: the operation is not sent, and the
	// time is not the payer's system's failure. A read of that system that
	// ran first (the member's lookup, the CDS service listing) is still
	// recorded, and the last one is the exchange's backend.
	if timedOut() {
		logDeadlineSpent(ctx, label, leg, n.backendDeadline)
		return upstreamReply{}, LegResult{}, &deadlineSpent{deadline: n.backendDeadline}
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return upstreamReply{}, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	// Each leg's own media type (or the sender's declared FHIR media type),
	// as a direct client would send it — never application/json on a FHIR
	// operation.
	ct := nativeRequestMediaType(ctx, p)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", acceptFor(ct))
	n.applyBackendHeaders(req)
	if n.noBackendCorrelation {
		req.Header.Del(CorrelationHeader)
	} else {
		setBackendCorrelation(ctx, req)
	}
	capture, _ := ctx.Value(nativeCertificationKey{}).(*nativeCertificationCapture)
	if capture != nil {
		capture.attempted = true
		capture.request = append([]byte(nil), body...)
		capture.response = nil
	}
	// The forward's event names the registered edits it is sent with (the
	// payer identity mapping); the record names them once the bytes are sent
	// (below).
	n.emitDiagnosticDetail(ctx, "native.request", body, true, 0, relayEditsDetail(p.Edits()), req, req.Header)
	// Whether the request was written decides what a failure tells the
	// requester: before, the payer's system never saw it; after, it may have
	// acted on it.
	var wrote atomic.Bool // once any attempt was written, it may have been received
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}))
	started := time.Now()
	x := exchangeOf(ctx)
	var callStart time.Time
	if x != nil {
		callStart = n.now()
	}
	resp, err := n.client.Do(req)
	if err != nil {
		class := callEndedClass(callCtx, backendClass(0, err))
		if timedOut() {
			// Past the deadline whatever failed (a token endpoint as slow as
			// the system itself): the system's timeout, not its credentials.
			class = BackendTimeout
		}
		x.backend(0, x.since(n.now, callStart), class)
		// A request whose bearer could not be obtained was never sent.
		sent := wrote.Load() && !smartauth.IsTokenAcquisitionError(err)
		if sent {
			// Written before the call failed: the edits were sent.
			x.sent(p)
		}
		if timedOut() {
			logUpstreamTimedOut(ctx, label, leg, base+path, n.backendDeadline, sent)
			return upstreamReply{}, LegResult{}, &upstreamFailure{err: fmt.Errorf("upstream payer %s did not answer within %s: %w", label, n.backendDeadline, err), sent: sent, timedOut: true}
		}
		logUpstreamAbandoned(ctx, label, leg, base+path, time.Since(started), sent)
		return upstreamReply{}, LegResult{}, &upstreamFailure{err: fmt.Errorf("upstream payer %s unreachable: %w", label, err), sent: sent}
	}
	// The payer's system answered: the edits the forward carried were sent.
	x.sent(p)
	pasLegOf(ctx).answered()
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	n.emitDiagnostic(ctx, "native.response", rb, resp.StatusCode, diagnosticReadDetail(err, len(rb)), req, resp.Header)
	if capture != nil {
		capture.response = append([]byte(nil), rb...)
	}
	if err != nil {
		class := callEndedClass(callCtx, bodyReadClass(err))
		if timedOut() {
			class = BackendTimeout
		}
		x.backend(resp.StatusCode, x.since(n.now, callStart), class)
		if timedOut() {
			logUpstreamTimedOut(ctx, label, leg, base+path, n.backendDeadline, true)
			return upstreamReply{}, LegResult{}, &upstreamFailure{err: fmt.Errorf("upstream payer %s answer not read within %s: %w", label, n.backendDeadline, err), sent: true, timedOut: true}
		}
		logUpstreamAbandoned(ctx, label, leg, base+path, time.Since(started), true)
		return upstreamReply{}, LegResult{}, &upstreamFailure{err: fmt.Errorf("upstream payer %s read failed: %w", label, err), sent: true}
	}
	x.backend(resp.StatusCode, x.since(n.now, callStart), backendClass(resp.StatusCode, nil))
	return upstreamAnswer(resp, rb, label)
}

// now is the responder's clock; a responder built without the constructor
// (tests) reads the wall clock.
func (n *nativeResponder) now() time.Time {
	if n.clock == nil {
		return time.Now()
	}
	return n.clock()
}

// setBackendCorrelation names the leg on a request to the participant's own
// system: X-Correlation-Id carries the leg's id, the id the network's records
// and both gateways' access lines know the exchange by, so the participant's
// logs join them. It is metadata about the request, as a participant's own
// routing headers are: the message bytes are untouched, and a system that
// ignores the header is sent exactly what it was sent before. An id that is
// not one bounded token (a Claim may name its own) is not sent: a header is
// never the reason the request fails.
func setBackendCorrelation(ctx context.Context, req *http.Request) {
	req.Header.Del(CorrelationHeader)
	if corr, _ := ctx.Value(responderCorrelationKey{}).(string); correlationShape.MatchString(corr) {
		req.Header.Set(CorrelationHeader, corr)
	}
}

type responderCorrelationKey struct{}

// withResponderCorrelation names the exchange a responder call serves, so a
// line about that call can name it too.
func withResponderCorrelation(ctx context.Context, corrID string) context.Context {
	return context.WithValue(ctx, responderCorrelationKey{}, corrID)
}

// logUpstreamAbandoned writes the payer gateway's own line when its call to
// its participant's system ended because the request it serves ended: most
// often the requester stopped waiting (its hub-leg budget ran out, or it went
// away), or this gateway is shutting down. Without it only the requester logs
// the timeout, and the payer side's operator cannot see that an answer from
// their system was abandoned. It names the elapsed time, the upstream host
// (never the path or query), the leg, the correlation id and whether the
// request was written; never a body, a header or a credential. A call that
// failed while its request was still live is "unreachable", not this line.
func logUpstreamAbandoned(ctx context.Context, label, leg, rawURL string, elapsed time.Duration, written bool) {
	cause := ctx.Err()
	if cause == nil {
		return
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	corr, _ := ctx.Value(responderCorrelationKey{}).(string)
	wrote := "no"
	if written {
		wrote = "yes"
	}
	log.Printf("gateway: upstream payer %s call abandoned after %.1fs: the request it serves ended (%v) (host %s, leg %s, correlation %s, request written: %s)", label, elapsed.Seconds(), cause, host, leg, corr, wrote)
}

// deadlineSpent is a forward never sent: the responder's deadline had passed
// before the call, spent on this gateway's own work. It is not the payer
// system's failure.
type deadlineSpent struct{ deadline time.Duration }

func (e *deadlineSpent) Error() string {
	return fmt.Sprintf("the deadline for the payer's system (%s) passed before the call was sent", e.deadline)
}

// logDeadlineSpent writes the payer gateway's line for a forward it never
// sent because its own work before the call used the deadline.
func logDeadlineSpent(ctx context.Context, label, leg string, deadline time.Duration) {
	corr, _ := ctx.Value(responderCorrelationKey{}).(string)
	log.Printf("gateway: upstream payer %s call not sent: this gateway's own work used its %s deadline for its system: answered 504 (leg %s, correlation %s)", label, deadline, leg, corr)
}

// logUpstreamTimedOut writes the payer gateway's own line when its call to
// its participant's system ran past the responder's deadline: the system's
// timeout, answered by this gateway as its own 504 while the requester was
// still waiting. Like logUpstreamAbandoned it names the upstream host (never
// the path or query), the leg and the correlation id, and whether the
// request was written; never a body, a header or a credential.
func logUpstreamTimedOut(ctx context.Context, label, leg, rawURL string, deadline time.Duration, written bool) {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	corr, _ := ctx.Value(responderCorrelationKey{}).(string)
	wrote := "no"
	if written {
		wrote = "yes"
	}
	log.Printf("gateway: upstream payer %s call timed out after %s, this gateway's deadline for its system: answered 504 (host %s, leg %s, correlation %s, request written: %s)", label, deadline, host, leg, corr, wrote)
}

// upstreamAnswer classifies a read upstream response for post and get.
func upstreamAnswer(resp *http.Response, rb []byte, label string) (upstreamReply, LegResult, error) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/fhir+json"
	}
	reply := upstreamReply{body: relay.NewBody(rb, relay.OriginUpstreamResponse), raw: rb, contentType: ct, declared: resp.Header.Get("Content-Type"), status: resp.StatusCode}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return upstreamReply{}, LegResult{}, &upstreamFailure{err: fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb)), sent: true}
		}
		return reply, LegResult{Status: resp.StatusCode, Response: relay.Exact(reply.body, resp.Header.Get("Content-Type"))}, nil
	}
	return reply, LegResult{}, nil
}

type legArrivalKey struct{}

// withLegArrival notes when an inbound leg reached this gateway, the start
// of the responder's deadline for its participant's system.
func withLegArrival(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, legArrivalKey{}, at)
}

// legArrival is when the leg ctx serves reached this gateway, if it is an
// inbound leg.
func legArrival(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(legArrivalKey{}).(time.Time)
	return at, ok
}
