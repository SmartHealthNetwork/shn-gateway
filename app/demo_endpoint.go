// demo_endpoint.go — two loopback demo endpoints on the observer listener:
//
//   - POST /demo/transform: a thin JSON shim over engine.RunTransformChain,
//     so the SHN Kit's engine exhibit can run the real compat chain through
//     the SAME binary/manifest a live leg routes through. Not a wire
//     exchange: engine.RunTransformChain calls applyChain directly (no
//     egressAdapt, no leg), so a run through this endpoint never appears on
//     the observer SSE stream — the exhibit's own request/response is its
//     own record.
//   - GET /demo/capture/{correlationId}: reads back this gateway's own
//     bounded pre-seal edge-capture store (SHN_DEMO_EDGE_CAPTURE) by
//     correlation id — the participant's own before/after payload pair for
//     a leg it already transformed, served to its own operator surface,
//     read-only.
//
// Both are registered ONLY when OBSERVER_ADDR is set (see app.go's build():
// composeObserverHandler wraps the observer hub's own handler) — they
// inherit that listener's loopback-only bind validation (app.go's config
// load: OBSERVER_ADDR must resolve to a loopback host).
package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// composeObserverHandler wraps the observer hub's own handler (GET /events,
// GET /health) with POST /demo/transform and GET /demo/capture/{correlationId}
// on the SAME mux — one listener, one loopback bind, all three surfaces. The
// catch-all "/" pattern delegates everything else to hub verbatim;
// ServeMux's most-specific-pattern-wins rule (Go 1.22+) means the two demo
// patterns are matched ahead of "/" for their exact path. gw is the SAME
// *engine.Gateway instance build() constructs for this process — the
// capture-fetch endpoint reads that gateway's own bounded edge-capture
// store, never a second one; edgeCaptureEnabled mirrors Config.DemoEdgeCapture
// (whether that store was ever built at all).
//
// /demo/capture/{correlationId} is registered WITHOUT a method prefix
// (unlike "POST /demo/transform"): the catch-all "/" pattern registered on
// this SAME mux always matches too (it has no method of its own), so a
// method-specific registration here would never trigger ServeMux's built-in
// 405 — "/" would simply win as the request's other matching pattern
// instead. Registering path-only makes THIS pattern the more specific match
// for every method on that exact path (never falling through to "/"), and
// handleDemoCapture applies the GET-only method guard itself.
func composeObserverHandler(hub http.Handler, gw *engine.Gateway, edgeCaptureEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", hub)
	mux.HandleFunc("POST /demo/transform", handleDemoTransform)
	mux.HandleFunc("/demo/capture/{correlationId}", handleDemoCapture(gw, edgeCaptureEnabled))
	return mux
}

// demoTransformRequest is the wire request the SHN Kit exhibit proxy
// consumes verbatim: {"contract","from","to","payload":<raw JSON>}.
type demoTransformRequest struct {
	Contract string          `json:"contract"`
	From     string          `json:"from"`
	To       string          `json:"to"`
	Payload  json.RawMessage `json:"payload"`
}

// demoTransformResponse is the 200 wire shape: the transformed payload plus
// the chain's per-step LossReports (json-tagged already on engine.LossReport
// — no re-shaping here). Chain is additive (omitempty): the hop list the run
// walked, in the same shape observer events carry, so a caller can report
// which modules ran without hardcoding a copy that drifts from the manifest.
type demoTransformResponse struct {
	Output      json.RawMessage     `json:"output"`
	LossReports []engine.LossReport `json:"lossReports"`
	Chain       []engine.ChainStep  `json:"chain,omitempty"`
}

// demoTransformRefusal is the 422 wire shape: semanticChange distinguishes a
// typed *engine.SemanticChangeError refusal (errors.As) from any other
// applyChain failure (e.g. an unknown chain — chainFor's nil-hole path, or a
// malformed-payload parse error surfacing from inside a step function).
// Chain is additive (omitempty): the hop list the run ATTEMPTED, even though
// it refused partway through — useful context for a caller reporting why.
type demoTransformRefusal struct {
	Refusal        string             `json:"refusal"`
	SemanticChange bool               `json:"semanticChange"`
	Chain          []engine.ChainStep `json:"chain,omitempty"`
}

// exchangeIdentityForDemo is the fixed ExchangeIdentity every demo run uses:
// a demo run is not a leg, so there is no real correlation id, leg type, or
// counterpart to carry — "demo" is a stable, honest placeholder (the same
// posture as the brief's worked example), never a synthesized wire value.
var exchangeIdentityForDemo = engine.ExchangeIdentity{CorrelationID: "demo"}

func handleDemoTransform(w http.ResponseWriter, r *http.Request) {
	// Bounded exactly like every other inbound handler in this codebase
	// (gateway/engine/observer.go's observeIngress, ingress.go, inbound.go,
	// sdk/responder.go — shnsdk.MaxRequestBytes, 8 MiB): SHN Kit proxies
	// partner-facing traffic to this endpoint, so an unbounded Decode would be
	// a memory-exhaustion vector fed by attacker-controlled bytes.
	var req demoTransformRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, shnsdk.MaxRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "shn: malformed JSON request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	out, reports, err := engine.RunTransformChain(req.Contract, req.From, req.To, []byte(req.Payload), exchangeIdentityForDemo)
	chain := engine.ChainSteps(req.Contract, req.From, req.To)
	if err != nil {
		var scErr *engine.SemanticChangeError
		resp := demoTransformRefusal{
			Refusal:        err.Error(),
			SemanticChange: errors.As(err, &scErr),
			Chain:          chain,
		}
		writeDemoJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}

	if reports == nil {
		reports = []engine.LossReport{}
	}
	writeDemoJSON(w, http.StatusOK, demoTransformResponse{Output: out, LossReports: reports, Chain: chain})
}

func writeDemoJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; nothing left to do on a write failure
}

// demoCaptureResponse is the GET /demo/capture/{correlationId} 200 wire
// shape: one transformed egress leg's own captured pre-seal before/after
// payload pair (engine.EdgeCapture), reshaped for the wire since that type
// carries no JSON tags of its own. LossReports marshals the engine's own
// []LossReport DIRECTLY — no shnsdk conversion — exactly like its
// /demo/transform sibling (demoTransformResponse above): engine.LossReport
// already carries the same json tags as shnsdk.LossReport
// (TestLossReportSDKSchemaParity), so there is nothing a conversion would
// add here.
type demoCaptureResponse struct {
	CorrelationID string              `json:"correlationId"`
	LegType       string              `json:"legType"`
	Contract      string              `json:"contract"`
	From          string              `json:"from"`
	To            string              `json:"to"`
	Chain         []engine.ChainStep  `json:"chain"`
	LossReports   []engine.LossReport `json:"lossReports"`
	Before        json.RawMessage     `json:"before"`
	After         json.RawMessage     `json:"after"`
	CapturedAt    time.Time           `json:"capturedAt"`
}

// demoCaptureError is the shared 404 wire shape for the demo capture-fetch
// endpoint: {"error": "..."}. Two distinct messages: the flag itself is off
// ("edge capture is not enabled") vs. the flag is on but this id has
// nothing captured ("no capture for this leg") — a caller needs to tell
// those apart to know whether to ask again.
type demoCaptureError struct {
	Error string `json:"error"`
}

// handleDemoCapture serves GET /demo/capture/{correlationId} on the
// observer loopback mux: this gateway's own bounded pre-seal edge-capture
// store (engine/edgecapture.go), read back by correlation id. Same
// loopback-only, observer-mux-composed posture as handleDemoTransform.
//
// Not a wire exchange: the store is populated at egressAdapt's own pre-seal
// choke point (never on the wire, never in the audit record, never part of
// any conformance surface), and this endpoint only ever serves the
// participant's OWN captured payloads back to their own operator surface,
// read-only — there is no cross-participant reach here, and no write path.
func handleDemoCapture(gw *engine.Gateway, edgeCaptureEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The mux registers this path without a method restriction (see
		// composeObserverHandler's doc comment) so this endpoint, not the
		// catch-all "/", handles every method on this exact path — the
		// method guard therefore lives here rather than in the mux pattern.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "shn: method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !edgeCaptureEnabled {
			writeDemoJSON(w, http.StatusNotFound, demoCaptureError{Error: "edge capture is not enabled"})
			return
		}

		// r.PathValue("correlationId") is never "" here: the mux pattern
		// requires a path segment at this position, so an id-less request
		// (GET /demo/capture/) falls through to the catch-all "/" before ever
		// reaching this handler (TestDemoCaptureEndpoint_EmptyID404 pins that
		// routed-layer behavior) — there is no id=="" case for this code to
		// defend against.
		id := r.PathValue("correlationId")
		capture, ok := gw.EdgeCaptureFor(id)
		if !ok {
			writeDemoJSON(w, http.StatusNotFound, demoCaptureError{Error: "no capture for this leg"})
			return
		}

		// Both slices normalize nil ⇒ [] on the wire, same as
		// demoTransformResponse's lossReports: a caller ranges over both
		// without a nil check.
		lossReports := capture.LossReports
		if lossReports == nil {
			lossReports = []engine.LossReport{}
		}
		chain := capture.Chain
		if chain == nil {
			chain = []engine.ChainStep{}
		}
		writeDemoJSON(w, http.StatusOK, demoCaptureResponse{
			CorrelationID: capture.CorrelationID,
			LegType:       capture.LegType,
			Contract:      capture.Contract,
			From:          capture.From,
			To:            capture.To,
			Chain:         chain,
			LossReports:   lossReports,
			Before:        capture.Before,
			After:         capture.After,
			CapturedAt:    capture.CapturedAt,
		})
	}
}
