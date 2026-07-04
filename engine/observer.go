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
//	leg.originated    an origination leg is about to be sent (Payload = request cleartext)
//	leg.response      the origination leg's verified, decrypted response (Payload = response cleartext)
//	leg.failed        the origination leg errored (Detail = error text)
//	ingress.received  a Da Vinci ingress call arrived (LegType = route tag, Payload = request body)
//	ingress.responded the ingress call was answered (Detail = HTTP status, Payload = response body)
//	validate.result   a $validate ran (Detail = "valid" | "invalid" | "validator unavailable")
type ObserverEvent struct {
	Time           time.Time       `json:"time"`
	Kind           string          `json:"kind"`
	LegType        string          `json:"legType,omitempty"`
	Direction      string          `json:"direction,omitempty"` // "originate" | "ingress" | "validate"
	CorrelationID  string          `json:"correlationId,omitempty"`
	Counterpart    string          `json:"counterpart,omitempty"`
	AuthorityFrame string          `json:"authorityFrame,omitempty"`
	Op             string          `json:"op,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Detail         string          `json:"detail,omitempty"`
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
