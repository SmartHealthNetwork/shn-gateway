// gateway/engine/frame_originate_test.go
//
// Opaque-payload message frame: the engine originator
// (roundTripInner) decodes a v1 sealed message frame from a frame-capable
// recipient. Drives the SAME in-process exchange harness relay_roundtrip_test.go
// builds (a real originator Gateway wired to a fake Hub+Authz relaySubstrate),
// swapping the sealed inner payload for an EncodeHTTPFrame result and flipping
// the recipient's registry entry to advertise MessageFrames:["v1"].
//
// Negotiation is keyed on the RECIPIENT's advertised frames: a v1-advertising
// recipient that answers a FRAMED payload is decoded (2xx → body byte-identical,
// non-2xx → *RelayError with the framed Content-Type); a v1-advertising recipient
// that answers a BARE payload is the stale-feed downgrade (legacy processing +
// loud log); a CORRUPT frame from a capable recipient is rejected (mutation row).
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// advertiseRecipientFrameV1 re-registers the harness's recipient holder ("payer")
// with MessageFrames:["v1"], preserving its crypto material, so the originator's
// SupportsMessageFrameV1(recipientHolder.MessageFrames) negotiation trips.
func advertiseRecipientFrameV1(t *testing.T, e *inProcessExchange) {
	t.Helper()
	entry, ok := e.originator.cfg.Reg.Lookup(e.payerID)
	if !ok {
		t.Fatalf("recipient %q not in registry", e.payerID)
	}
	entry.MessageFrames = []string{"v1"}
	e.originator.cfg.Reg.Set(e.payerID, entry)
}

// sealBare arms the fake Hub's response leg to seal `payload` back VERBATIM as the
// bare (Status 0) response payload — the raw wire the originator opens. A v1-framed
// payload rides here: the frame's INNER status carries the app status, so
// the outer LegResult stays 0/2xx (sealed bare, not legacy-wrapped).
func sealBare(e *inProcessExchange, payload []byte) {
	e.payerReturns(LegResult{Status: 0, ResponseFHIR: payload})
}

func TestOriginateDecodesFramedError(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	frame, err := shnsdk.EncodeHTTPFrame(422, "application/fhir+json", oo)
	if err != nil {
		t.Fatalf("EncodeHTTPFrame: %v", err)
	}
	sealBare(env, frame)

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if body != nil {
		t.Fatalf("framed non-2xx must return nil body, got %q", body)
	}
	var re *RelayError
	if !errors.As(err, &re) {
		t.Fatalf("want *RelayError, got %v", err)
	}
	if re.Status != 422 {
		t.Fatalf("RelayError.Status = %d, want 422", re.Status)
	}
	if string(re.Body) != string(oo) {
		t.Fatalf("RelayError.Body = %q, want %q", re.Body, oo)
	}
	if re.ContentType != "application/fhir+json" {
		t.Fatalf("RelayError.ContentType = %q, want application/fhir+json", re.ContentType)
	}
}

func TestOriginateDecodesFramedSuccess(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	want := []byte(`{"resourceType":"Parameters","parameter":[{"name":"ok"}]}`)
	frame, err := shnsdk.EncodeHTTPFrame(200, "application/fhir+json", want)
	if err != nil {
		t.Fatalf("EncodeHTTPFrame: %v", err)
	}
	sealBare(env, frame)

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if err != nil {
		t.Fatalf("framed 2xx must succeed, got err %v", err)
	}
	if string(body) != string(want) {
		t.Fatalf("framed 2xx body = %q, want byte-identical %q", body, want)
	}
}

// TestOriginateDecodesFramedErrorFromUnadvertisedRecipient pins the inverse
// stale-feed window (hardened at final review): a recipient that (correctly) frames
// its non-2xx answer while the ORIGINATOR's registry view of it is still pre-upgrade
// (MessageFrames absent — dynamic re-registration / rolling-deploy window). Decode is
// keyed on the frame magic, not the recipient's advertised frames, so the framed
// answer MUST still surface as a verbatim *RelayError — never handed raw to the app.
func TestOriginateDecodesFramedErrorFromUnadvertisedRecipient(t *testing.T) {
	env := newInProcessExchange(t)
	// deliberately DO NOT advertiseRecipientFrameV1 — recipient's entry stays legacy.
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid"}]}`)
	frame, err := shnsdk.EncodeHTTPFrame(422, "application/fhir+json", oo)
	if err != nil {
		t.Fatalf("EncodeHTTPFrame: %v", err)
	}
	sealBare(env, frame)

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if body != nil {
		t.Fatalf("framed non-2xx must return nil body, got %q", body)
	}
	var re *RelayError
	if !errors.As(err, &re) {
		t.Fatalf("want *RelayError from an unadvertised-but-framing recipient, got %v", err)
	}
	if re.Status != 422 || string(re.Body) != string(oo) || re.ContentType != "application/fhir+json" {
		t.Fatalf("RelayError not verbatim: status=%d ct=%q body=%q", re.Status, re.ContentType, re.Body)
	}
}

func TestOriginateStaleFeedFallback(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env) // recipient advertises v1...
	bare := []byte(`{"resourceType":"Parameters","parameter":[{"name":"stale"}]}`)
	sealBare(env, bare) // ...but answers BARE JSON (stale-feed view)

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if err != nil {
		t.Fatalf("stale-feed bare payload must be processed as legacy success, got err %v", err)
	}
	if string(body) != string(bare) {
		t.Fatalf("stale-feed body = %q, want bare payload %q", body, bare)
	}
}

func TestOriginateRejectsCorruptFrame(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	// v1 magic byte + garbage: a capable recipient that answers a corrupt frame
	// must be rejected, never processed as a bare/legacy body (mutation row for
	// the decode guard).
	corrupt := []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	sealBare(env, corrupt)

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if err == nil {
		t.Fatalf("corrupt frame must be rejected, got body %q", body)
	}
	if body != nil {
		t.Fatalf("corrupt frame must return nil body, got %q", body)
	}
	if err.Error() != "response frame decode failed" {
		t.Fatalf("corrupt frame err = %q, want %q", err.Error(), "response frame decode failed")
	}
}

// ---- The 19 origination sites relay a recipient's verbatim framed answer ----

// framedErrorFixture is the shared framed non-2xx the recipient answers in the relay tests: a
// 400 OperationOutcome with Content-Type application/fhir+json (the same shape
// TestOriginateDecodesFramedError above decodes).
func framedErrorFixture() (status int, ct string, body []byte) {
	return http.StatusBadRequest, "application/fhir+json",
		[]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"payer rejected the request"}]}`)
}

// assertRelayedVerbatim asserts the handler surfaced the recipient's framed answer byte-identically
// (exact status + body + Content-Type) — NOT a collapse to the generic {"error":"recipient answered
// N (M bytes)"} shape. The exact-body equality alone rules the collapse out (its bytes differ).
func assertRelayedVerbatim(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCT string, wantBody []byte) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (relay must carry the recipient's status; body=%s)", rec.Code, wantStatus, rec.Body.String())
	}
	if rec.Body.String() != string(wantBody) {
		t.Fatalf("body = %q, want byte-identical %q (a collapse to {\"error\":\"recipient answered...\"} is the bug)", rec.Body.String(), wantBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != wantCT {
		t.Fatalf("Content-Type = %q, want %q (the framed answer's own type, relayed verbatim)", ct, wantCT)
	}
}

// advertiseFixturePayerFrameV1 flips the dispatch/homeoxygen fixture's "payer" holder to advertise
// message-frame v1 (preserving its crypto), so the originator decodes the frame the substrate seals
// instead of treating it as a stale-feed bare payload.
func advertiseFixturePayerFrameV1(t *testing.T, gw *Gateway) {
	t.Helper()
	entry, ok := gw.cfg.Reg.Lookup("payer")
	if !ok {
		t.Fatal("payer not registered in fixture")
	}
	entry.MessageFrames = []string{"v1"}
	gw.cfg.Reg.Set("payer", entry)
}

// TestDispatchRelaysFramedRecipientError drives the free-form /scenario/dispatch handler
// (originate_homeoxygen.go crd-order-dispatch site): a recipient that frames a 400
// OperationOutcome on the FIRST leg must surface at the HTTP caller byte-identically, not as
// {"error":"recipient answered 400 (N bytes)"}.
func TestDispatchRelaysFramedRecipientError(t *testing.T) {
	fix := newMBROXDispatchFixture(t)
	advertiseFixturePayerFrameV1(t, fix.gw)
	status, ct, body := framedErrorFixture()
	fix.stub.frameErrLeg = "crd-order-dispatch"
	fix.stub.frameErrStatus, fix.stub.frameErrCT, fix.stub.frameErrBody = status, ct, body

	req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"MBR-OX"}`))
	rec := httptest.NewRecorder()
	fix.gw.handleDispatch(rec, req)

	assertRelayedVerbatim(t, rec, status, ct, body)
}

// TestScenarioRelaysFramedRecipientError drives a UC scenario handler (handleUC03 → the
// runCRDThenDTROrder crd-order-select site) over the in-process substrate: a framed 400 on the
// first leg is relayed verbatim to the caller.
func TestScenarioRelaysFramedRecipientError(t *testing.T) {
	env := newInProcessExchange(t)
	status, ct, body := framedErrorFixture()
	env.payerReturns(LegResult{Status: status, ResponseFHIR: body})

	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	rec := httptest.NewRecorder()
	env.originator.handleUC03(rec, req)

	assertRelayedVerbatim(t, rec, status, ct, body)
}

// TestPASTailRelaysFramedRecipientError drives the full order-dispatch chain through to the shared
// lean PAS tail (pas_tail.go submitClaimAndResolve → the pas-claim site): the crd-order-dispatch +
// dtr-questionnaire-fetch legs succeed, and the recipient frames a 400 ONLY on pas-claim. The %w
// audit is load-bearing here — submitClaimAndResolve must PROPAGATE the *RelayError (not collapse it
// to err.Error()) so handleHomeOxygen can relay it verbatim.
func TestPASTailRelaysFramedRecipientError(t *testing.T) {
	fix := newMBROXDispatchFixture(t)
	advertiseFixturePayerFrameV1(t, fix.gw)
	status, ct, body := framedErrorFixture()
	fix.stub.frameErrLeg = "pas-claim"
	fix.stub.frameErrStatus, fix.stub.frameErrCT, fix.stub.frameErrBody = status, ct, body

	req := httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", nil)
	rec := httptest.NewRecorder()
	fix.gw.handleHomeOxygen(rec, req)

	assertRelayedVerbatim(t, rec, status, ct, body)
	// Prove the chain actually reached the PAS leg (else the relay would be a first-leg accident).
	if !legAttempted(fix.stub.legTypes, "pas-claim") {
		t.Fatalf("pas-claim leg was never reached (legs: %v) — the test did not exercise the PAS-tail relay", fix.stub.legTypes)
	}
}

// TestRelayErrorSurvivesHelperWrapping is the %w audit guard: a *RelayError wrapped through the
// deepest helper chain still relays (a %v wrap or error re-synthesis anywhere on the path would drop
// the sentinel and fail this).
func TestRelayErrorSurvivesHelperWrapping(t *testing.T) {
	inner := &RelayError{Status: 422, Body: []byte(`{"x":1}`), ContentType: "application/json"}
	wrapped := fmt.Errorf("resume pended claim: %w", fmt.Errorf("originate leg: %w", inner))
	rec := httptest.NewRecorder()
	g := &Gateway{} // relayOriginationError touches no gateway state
	if !g.relayOriginationError(rec, wrapped) {
		// NB: a literal %v in this message would be rejected by `go vet` (run by `go test`)
		// as a stray format directive in t.Fatal; reworded to keep the gate green.
		t.Fatal("wrapped *RelayError not relayed — a value-format wrap or error re-synthesis dropped the sentinel")
	}
	if rec.Code != 422 || rec.Body.String() != `{"x":1}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("relay lost fidelity: %d %q %q", rec.Code, rec.Body.String(), rec.Header())
	}
	if g.relayOriginationError(httptest.NewRecorder(), fmt.Errorf("plain failure")) {
		t.Fatal("non-relay error must fall through to the existing writeJSON fallback")
	}
}
