// demo_endpoint.go — POST /demo/transform on the observer listener:
// a thin JSON shim over engine.RunTransformChain, so the SHN Kit's engine
// exhibit can run the real compat chain through the SAME binary/manifest a
// live leg routes through. Registered ONLY when OBSERVER_ADDR is set (see
// app.go's build(): composeObserverHandler wraps the observer hub's own
// handler) — it inherits that listener's loopback-only bind validation
// (app.go's config load: OBSERVER_ADDR must resolve to a loopback host).
//
// Not a wire exchange: engine.RunTransformChain calls applyChain directly
// (no egressAdapt, no leg), so a run through this endpoint never appears on
// the observer SSE stream — the exhibit's own request/response is its own
// record.
package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// composeObserverHandler wraps the observer hub's own handler (GET /events,
// GET /health) with POST /demo/transform on the SAME mux — one listener, one
// loopback bind, both surfaces. The catch-all "/" pattern delegates
// everything else to hub verbatim; ServeMux's most-specific-pattern-wins rule
// (Go 1.22+) means "POST /demo/transform" is matched ahead of "/" for that
// exact method+path.
func composeObserverHandler(hub http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", hub)
	mux.HandleFunc("POST /demo/transform", handleDemoTransform)
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
// — no re-shaping here).
type demoTransformResponse struct {
	Output      json.RawMessage     `json:"output"`
	LossReports []engine.LossReport `json:"lossReports"`
}

// demoTransformRefusal is the 422 wire shape: semanticChange distinguishes a
// typed *engine.SemanticChangeError refusal (errors.As) from any other
// applyChain failure (e.g. an unknown chain — chainFor's nil-hole path, or a
// malformed-payload parse error surfacing from inside a step function).
type demoTransformRefusal struct {
	Refusal        string `json:"refusal"`
	SemanticChange bool   `json:"semanticChange"`
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
	if err != nil {
		var scErr *engine.SemanticChangeError
		resp := demoTransformRefusal{
			Refusal:        err.Error(),
			SemanticChange: errors.As(err, &scErr),
		}
		writeDemoJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}

	if reports == nil {
		reports = []engine.LossReport{}
	}
	writeDemoJSON(w, http.StatusOK, demoTransformResponse{Output: out, LossReports: reports})
}

func writeDemoJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; nothing left to do on a write failure
}
