// observer.go — the gateway observer seam: opt-in structured edge events for
// local inspection tooling (see STABILITY.md).
//
// The observer is ADDITIVE instrumentation at the gateway edge: when
// Config.Observer is non-nil, the engine emits one structured ObserverEvent at
// each edge seam — origination legs (roundTrip), the Da Vinci ingress routes,
// and every $validate call. Events include PAYLOAD SNAPSHOTS: the cleartext
// FHIR exactly as this gateway saw it at its own edge (where payloads
// legitimately live — the substrate itself stays payload-blind).
//
// nil Observer (the default) = no emission and no overhead beyond one nil
// check: the published gateway binary never observes unless its operator asks.
// Emission MUST NOT change exchange behavior; TestObserver_ConformanceNeutral
// pins responses byte-identical with the observer on vs off.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ObserverEvent is one structured observation at the gateway edge. Kinds:
//
//	leg.originated    an origination leg is about to be sent (Payload = request cleartext;
//	                  Route = the routed line + build story on the select-before-build path,
//	                  nil on the legacy OriginateLeg fallback — nothing was selected to report)
//	leg.response      the origination leg's verified, decrypted response (Payload = response
//	                  cleartext; Status set when the recipient's app-level answer was a
//	                  relayed non-2xx, omitted for an ordinary 2xx response)
//	leg.failed        the origination leg errored for a reason OTHER than a relayed
//	                  recipient response (Detail = error text). Route is present for
//	                  TWO refusal variants sharing this seam: egressAdapt's
//	                  transform-chain refusal and guardPendCarry's
//	                  carry-integrity refusal on the pinned resume legs —
//	                  each carries the routed line/chain via routeInfoFor, same
//	                  rendering as leg.originated's Route; nil for roundTrip's
//	                  other leg.failed causes (auth denial, Hub unreachable, …),
//	                  which carry no route story to report
//	leg.refused       version-matched routing found no shared contract line for the leg
//	                  and refused before anything was sent (Detail = refusal message;
//	                  Route.Own/Peer/BridgeIssue = the structured refusal, nil when the
//	                  refusal wasn't a *RouteRefusalError, e.g. a catalog error)
//	leg.downgrade     the recipient advertises frame v1 but answered bare; processed as
//	                  legacy (stale-feed downgrade) (Detail = downgrade message)
//	leg.transformed   a cross-version transform chain bridged the leg to the peer's
//	                  contract line before it was sent (Detail = comma-joined module
//	                  chain summary, e.g. "pa.pas 2.0->2.1, pa.pas 2.1->2.2"). Note: the
//	                  Provenance + LossReport this describes ride INSIDE the transformed
//	                  payload itself (or observer-only where the target profile can't
//	                  tolerate the extra resource) — never the envelope, never Hub-visible.
//	ingress.received  a Da Vinci ingress call arrived (LegType = route tag, Payload = request body)
//	ingress.responded the ingress call was answered (Detail = HTTP status, Payload = response body)
//	validate.result   a $validate ran (Detail = "valid" | "invalid" | "validator unavailable")
//	sor.read          the gateway read its data source (Op = SystemOfRecord method,
//	                  Detail = "found"/"not found"/coverage status, Payload = the
//	                  resource bytes for byte-returning reads)
type ObserverEvent struct {
	Time           time.Time       `json:"time"`
	Kind           string          `json:"kind"`
	LegType        string          `json:"legType,omitempty"`
	Direction      string          `json:"direction,omitempty"` // "originate" | "ingress" | "validate" | "sor"
	CorrelationID  string          `json:"correlationId,omitempty"`
	Counterpart    string          `json:"counterpart,omitempty"`
	AuthorityFrame string          `json:"authorityFrame,omitempty"`
	Op             string          `json:"op,omitempty"`
	Status         int             `json:"status,omitempty"` // relayed recipient app-status (non-2xx relay)
	Payload        json.RawMessage `json:"payload,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	Route          *RouteInfo      `json:"route,omitempty"`
}

// RouteInfo is the structured routing story attached to leg-scoped observer
// events (additive; nil wherever routing played no part). On leg.originated
// it carries the selected Token/BuildLine and, on an arm-(3) transform-chain
// route, the Chain the payload was bridged through; on leg.refused it
// carries the *RouteRefusalError's own/peer declarations and bridge issue
// instead (Token/BuildLine/Chain empty — nothing was selected).
type RouteInfo struct {
	Token       string      `json:"token,omitempty"`
	BuildLine   string      `json:"buildLine,omitempty"`
	Chain       []ChainStep `json:"chain,omitempty"`
	Own         []string    `json:"own,omitempty"`         // refusals only
	Peer        []string    `json:"peer,omitempty"`        // refusals only
	BridgeIssue string      `json:"bridgeIssue,omitempty"` // refusals only
}

// ChainStep is one hop of a RouteInfo.Chain, mirroring one CompatStep of the
// selected transform chain. Module matches LossReport.Module's own format
// ("pa.pas 2.1->2.2") so the same string reads identically whether it comes
// from the routed story (observer-only) or the LossReport riding inside a
// transformed payload.
type ChainStep struct {
	Module string `json:"module"` // "pa.dtr 2.1->2.2"
	From   string `json:"from"`
	To     string `json:"to"`
	Class  string `json:"class"` // "full" | "carry" | "gated"
}

// routeInfoFor renders a resolved legRoute (select-before-build's Token +
// BuildLine + Chain) as the observer-facing RouteInfo. Chain is nil-through:
// an arm (1)/(2) route (route.Chain == nil) produces a RouteInfo with a nil
// Chain, never a speculative empty one.
//
// Each hop's From/To (and Module, built from them) is rendered in WALK
// order, not the manifest row's stored ascending order — mirroring
// applyChain's own curLine-vs-step.From/To direction switch (transform.go)
// exactly, so a down-walking chain (own build line HIGHER than the routed
// target — an ordinary SHN_CONTRACT_VERSIONS shape, not just a theoretical
// one) renders "2.1->2.0" here as faithfully as the LossReport riding inside
// the transformed payload itself. CompatStep.Class is direction-invariant
// (declared per row, not per walk), so it passes through unchanged.
func routeInfoFor(route legRoute) *RouteInfo {
	ri := &RouteInfo{Token: route.Token, BuildLine: route.BuildLine}
	curLine := route.BuildLine
	for _, s := range route.Chain {
		from, to := s.From, s.To
		if curLine == s.To {
			from, to = s.To, s.From
		}
		// curLine == s.From (or an unreached/disconnected row, which egressAdapt's
		// applyChain would already have refused before any caller reaches this
		// point with bytes to build a Content from) keeps the manifest's own
		// ascending From/To.
		ri.Chain = append(ri.Chain, ChainStep{
			Module: s.Contract + " " + from + "->" + to,
			From:   from, To: to, Class: string(s.Class),
		})
		curLine = to
	}
	return ri
}

// refusalRouteInfo renders a *RouteRefusalError as the observer-facing
// RouteInfo's refusal shape (Own/Peer/BridgeIssue; Token/BuildLine/Chain
// stay empty — nothing was selected to report). nil when the error carries
// NO refusal detail at all (Own/Peer/BridgeIssue all empty — a catalog/
// library mismatch that never even attempted arms (2)/(3), spec
// versionroute.go's `refusal("")` sites): RouteInfo's own doc says "nil
// wherever routing played no part", and an empty {} would misreport a
// refusal story that isn't there.
func refusalRouteInfo(e *RouteRefusalError) *RouteInfo {
	if len(e.Own) == 0 && len(e.Peer) == 0 && e.BridgeIssue == "" {
		return nil
	}
	return &RouteInfo{Own: e.Own, Peer: e.Peer, BridgeIssue: e.BridgeIssue}
}

// observe emits e to the configured Observer, stamping Time from the gateway
// clock. nil-safe: without an Observer this is one nil check. Payload slices
// are passed by reference — observers must treat events as read-only and
// serialize promptly (the SSE hub marshals on receipt).
func (g *Gateway) observe(e ObserverEvent) {
	if g.cfg.Observer == nil {
		return
	}
	e.Time = g.cfg.Clock()
	g.cfg.Observer(e)
}

// observingValidator decorates the configured shnsdk.Validator so EVERY
// $validate the engine runs — whatever the call site — emits one
// validate.result event. Result and error pass through untouched.
type observingValidator struct {
	inner shnsdk.Validator
	g     *Gateway
}

func (v observingValidator) Validate(ctx context.Context, resourceJSON []byte, profile string) (shnsdk.Result, error) {
	res, err := v.inner.Validate(ctx, resourceJSON, profile)
	detail := "valid"
	switch {
	case err != nil:
		detail = "validator unavailable"
	case !res.Valid:
		detail = "invalid"
	}
	v.g.observe(ObserverEvent{Kind: "validate.result", Direction: "validate", Payload: json.RawMessage(resourceJSON), Detail: detail})
	return res, err
}

// observeIngress wraps a Da Vinci ingress handler with observer emissions:
// ingress.received (the caller's request body) and ingress.responded (HTTP
// status + response body). The body is re-buffered so handler behavior is
// unchanged; with a nil Observer the handler runs bare (zero overhead).
// NOTE: ingress.received fires before the handler's own auth check — the
// observer is a loopback-local diagnostic surface and seeing rejected calls
// is part of its job (a 401 is inspector content too).
func (g *Gateway) observeIngress(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.Observer == nil {
			h(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
		if err != nil {
			// Auth-ordering corollary: this 400 fires BEFORE the wrapped handler's own
			// auth check, so on the observed path an unreadable body 400s where the
			// unobserved path would 401 first. A read failure here is a torn connection,
			// not a conformance surface; the neutrality gates compare complete exchanges.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.observe(ObserverEvent{Kind: "ingress.received", Direction: "ingress", LegType: route, Payload: json.RawMessage(body)})
		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		g.observe(ObserverEvent{Kind: "ingress.responded", Direction: "ingress", LegType: route,
			Detail: strconv.Itoa(rec.status), Payload: json.RawMessage(rec.buf.Bytes())})
	}
}

// recordingWriter tees status + body while writing through to the client.
type recordingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (rw *recordingWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
func (rw *recordingWriter) Write(p []byte) (int, error) {
	rw.buf.Write(p)
	return rw.ResponseWriter.Write(p)
}

// Unwrap exposes the underlying writer so http.ResponseController verbs
// (Flush, Hijack, deadlines) pass through the tee — observing a route must
// not disable streaming behavior its handler could otherwise use.
func (rw *recordingWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// observingSoR decorates the configured SystemOfRecord so EVERY data-source
// read — whatever the call site — emits one sor.read event. Results and
// errors pass through untouched (neutrality is pinned by
// TestObserver_ConformanceNeutral, which drives UC-03 with the decoration
// active).
//
// Unlike observingValidator this cannot hold *Gateway: the decoration must
// land BEFORE New()'s Responder/Populator derivations capture cfg.SoR
// (see the install site in New()), and g does not
// exist yet at that point. It closes over the Observer func and Clock
// directly instead.
type observingSoR struct {
	inner    SystemOfRecord
	observer func(ObserverEvent)
	clock    func() time.Time
}

func (o observingSoR) emit(op, detail string, payload []byte) {
	e := ObserverEvent{Time: o.clock(), Kind: "sor.read", Direction: "sor", Op: op, Detail: detail}
	if len(payload) > 0 {
		e.Payload = json.RawMessage(payload)
	}
	o.observer(e)
}

func sorFoundDetail(found bool) string {
	if found {
		return "found"
	}
	return "not found"
}

func (o observingSoR) ResolvePatient(memberID string) (string, Demo, bool) {
	pci, demo, found := o.inner.ResolvePatient(memberID)
	o.emit("ResolvePatient", sorFoundDetail(found), nil)
	return pci, demo, found
}

func (o observingSoR) PatientFHIRRef(memberID string) (string, bool) {
	ref, found := o.inner.PatientFHIRRef(memberID)
	o.emit("PatientFHIRRef", sorFoundDetail(found), nil)
	return ref, found
}

func (o observingSoR) CoverageInforce(memberID string) (bool, string) {
	inforce, reason := o.inner.CoverageInforce(memberID)
	detail := "inforce"
	if !inforce {
		detail = reason
		if detail == "" {
			detail = "not inforce"
		}
	}
	o.emit("CoverageInforce", detail, nil)
	return inforce, reason
}

func (o observingSoR) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	cc, found := o.inner.ClinicalContext(memberID)
	o.emit("ClinicalContext", sorFoundDetail(found), nil)
	return cc, found
}

func (o observingSoR) SupplementalReport(memberID string) ([]byte, bool) {
	report, found := o.inner.SupplementalReport(memberID)
	o.emit("SupplementalReport", sorFoundDetail(found), report)
	return report, found
}

func (o observingSoR) FacilityRecords(memberID string) (map[string][]byte, bool) {
	records, found := o.inner.FacilityRecords(memberID)
	// No payload: a multi-resource map is not one FHIR resource snapshot
	// (accepted gap — additive later if the inspector needs it).
	o.emit("FacilityRecords", sorFoundDetail(found), nil)
	return records, found
}

func (o observingSoR) OpenOrder(memberID string) ([]byte, bool) {
	orderJSON, found := o.inner.OpenOrder(memberID)
	o.emit("OpenOrder", sorFoundDetail(found), orderJSON)
	return orderJSON, found
}

func (o observingSoR) OpenCoverage(memberID string) ([]byte, bool) {
	coverageJSON, found := o.inner.OpenCoverage(memberID)
	o.emit("OpenCoverage", sorFoundDetail(found), coverageJSON)
	return coverageJSON, found
}

func (o observingSoR) ResolveByReference(ref string) ([]byte, bool) {
	resourceJSON, found := o.inner.ResolveByReference(ref)
	o.emit("ResolveByReference", sorFoundDetail(found), resourceJSON)
	return resourceJSON, found
}
