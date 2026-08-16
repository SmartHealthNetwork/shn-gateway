// gateway/engine/frame_originate_test.go
//
// Opaque-payload message frame (spec 2026-07-17): the engine originator
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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	body, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if err != nil {
		t.Fatalf("stale-feed bare payload must be processed as legacy success, got err %v", err)
	}
	if string(body) != string(bare) {
		t.Fatalf("stale-feed body = %q, want bare payload %q", body, bare)
	}
	// The forward stale-feed downgrade emits a structured observer event (the seam
	// the Kit's flow map consumes), not just a log line.
	var dg *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.downgrade" {
			dg = &events[i]
		}
	}
	if dg == nil {
		t.Fatalf("stale-feed downgrade did not emit a leg.downgrade observer event (events: %+v)", events)
	}
	if dg.CorrelationID != "corr-1" || dg.Counterpart != env.payerID || dg.LegType != "crd-order-select" {
		t.Fatalf("leg.downgrade event fields = %+v, want corr-1/%s/crd-order-select", dg, env.payerID)
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

// declareRecipientVersions re-registers the harness recipient with the given
// contract tokens (the advertiseRecipientFrameV1 pattern).
func declareRecipientVersions(t *testing.T, e *inProcessExchange, tokens []string) {
	t.Helper()
	entry, ok := e.originator.cfg.Reg.Lookup(e.payerID)
	if !ok {
		t.Fatalf("recipient %q not in registry", e.payerID)
	}
	entry.ContractVersions = tokens
	e.originator.cfg.Reg.Set(e.payerID, entry)
}

// TestOriginateRefusesUnsharedLine: rejection row for the version filter —
// valid exchange − shared line → typed refusal BEFORE any leg is routed
// (no seal, no authorize, no Hub round-trip).
func TestOriginateRefusesUnsharedLine(t *testing.T) {
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.crd@2.2"})

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %v", err)
	}
	if got := env.routeHitCount(); got != 0 {
		t.Fatalf("fake Hub /route was called %d times — refusal must happen BEFORE any leg routes", got)
	}

	// The refusal must also emit a structured leg.refused observer event (the seam
	// the Kit's flow map consumes) — the same discipline TestOriginateStaleFeedFallback
	// pins for leg.downgrade.
	var refused *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.refused" {
			refused = &events[i]
		}
	}
	if refused == nil {
		t.Fatalf("refusal did not emit a leg.refused observer event (events: %+v)", events)
	}
	if refused.CorrelationID != "corr-1" || refused.Counterpart != env.payerID || refused.LegType != "crd-order-select" {
		t.Fatalf("leg.refused event fields = %+v, want corr-1/%s/crd-order-select", refused, env.payerID)
	}
	if refused.Detail == "" {
		t.Fatalf("leg.refused event has empty Detail, want the refusal message")
	}
}

// TestOriginatePinnedProfileIDSkipsSelection: a non-empty Content.ProfileID is
// the PENDED-LINE PIN (spec §4) — honored verbatim, no re-selection, even when
// the registry has since changed to an incompatible declaration.
func TestOriginatePinnedProfileIDSkipsSelection(t *testing.T) {
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.crd@2.2"}) // would refuse if re-selected
	env.payerReturns(LegResult{Status: 0, ResponseFHIR: []byte(`{"resourceType":"Bundle","type":"collection"}`)})
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, ProfileID: "pa.crd@2.0", Bytes: env.crdReq}); err != nil {
		t.Fatalf("pinned leg must not re-select: %v", err)
	}
}

// TestScenarioRefusalWrites422: the refusal reaches the origination caller as
// a legible 422 body via relayOriginationError — the same chokepoint every
// scenario + ingress OriginateLeg error path already calls.
func TestScenarioRefusalWrites422(t *testing.T) {
	rre := &RouteRefusalError{
		Contract: "pa.pas", LegType: "pas-claim", Recipient: "payer-x",
		Own: []string{"pa.pas@2.0"}, Peer: []string{"pa.pas@2.2"},
	}
	rec := httptest.NewRecorder()
	g := &Gateway{} // relayOriginationError touches no gateway state
	if !g.relayOriginationError(rec, rre) {
		t.Fatal("RouteRefusalError not relayed as a 422")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, rec.Body.String())
	}
	for _, must := range []string{"pa.pas@2.0", "pa.pas@2.2"} {
		if !strings.Contains(body.Error, must) {
			t.Fatalf("422 body %q missing %q", body.Error, must)
		}
	}
}

// TestOriginateRejectsStampMismatch: mutation row — valid framed 2xx answer
// − contractVersion stamped with a DIFFERENT line than the originator routed
// → rejected before the body reaches any parser (spec 2026-08-10 §4; the
// in-seal tamper case the wire mutator table cannot express).
func TestOriginateRejectsStampMismatch(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	body := []byte(`{"resourceType":"Bundle","type":"collection"}`) // any valid JSON — OriginateLeg returns the raw body to direct callers; there is no crdCards fixture field (cards are built inline in the fake substrate)
	frame, err := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{
		"Content-Type":                    "application/fhir+json",
		shnsdk.FrameHeaderContractVersion: "pa.crd@2.2",
	}, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sealBare(env, frame)
	_, err = env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	if err == nil || !strings.Contains(err.Error(), "contract version mismatch") {
		t.Fatalf("want contract-version-mismatch rejection, got %v", err)
	}
}

// TestOriginateRejectsStampMismatchAcrossLines is the line-aware sibling of the row
// above: the same CONTRACT, a genuinely DIFFERENT LINE (pa.crd@2.2 answering a
// pa.crd@2.0 leg). The 2026-08-10 row used a token whose contract differed too;
// now that both lines are natively buildable, this is a real, plausible payload of
// the WRONG SHAPE — and it must still be rejected before any parser or validator
// touches it. The leg is pinned via Content.ProfileID (the pended-pin form) so the
// routed line is unambiguous.
func TestOriginateRejectsStampMismatchAcrossLines(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	frame, err := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{
		"Content-Type":                    "application/fhir+json",
		shnsdk.FrameHeaderContractVersion: "pa.crd@2.2",
	}, []byte(`{"resourceType":"Bundle","type":"collection"}`))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sealBare(env, frame)
	_, err = env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, ProfileID: "pa.crd@2.0", Bytes: env.crdReq})
	if err == nil || !strings.Contains(err.Error(), "contract version mismatch") {
		t.Fatalf("want contract-version-mismatch rejection for pa.crd@2.2 on a pa.crd@2.0 leg, got %v", err)
	}
	if !strings.Contains(err.Error(), "pa.crd@2.2") || !strings.Contains(err.Error(), "pa.crd@2.0") {
		t.Fatalf("rejection must name BOTH lines, got %v", err)
	}
}

// TestOriginateAcceptsMatchingStamp: the same exchange with the CORRECT stamp
// succeeds — the guard rejects mismatches, not stamps.
func TestOriginateAcceptsMatchingStamp(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	body := []byte(`{"resourceType":"Bundle","type":"collection"}`)
	frame, _ := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{
		"Content-Type":                    "application/fhir+json",
		shnsdk.FrameHeaderContractVersion: "pa.crd@2.0",
	}, body)
	sealBare(env, frame)
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq}); err != nil {
		t.Fatalf("matching stamp must pass: %v", err)
	}
}

// TestOriginateToleratesAbsentStamp: an unstamped frame is a pre-version
// responder — tolerated (absence means pre-version contract). This is the
// existing TestOriginateDecodesFramedSuccess behavior; assert it explicitly
// under the new guard so the tolerance is pinned, not incidental.
func TestOriginateToleratesAbsentStamp(t *testing.T) {
	env := newInProcessExchange(t)
	advertiseRecipientFrameV1(t, env)
	frame, _ := shnsdk.EncodeHTTPFrame(200, "application/fhir+json", []byte(`{"resourceType":"Bundle","type":"collection"}`))
	sealBare(env, frame)
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq}); err != nil {
		t.Fatalf("absent stamp must be tolerated: %v", err)
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

// TestLegOriginatedCarriesRoute: on the select-before-build path (arm 1: a
// real selectLegLine token+build line, empty Chain) the leg.originated
// observer event carries the SAME route story the caller threaded onto
// Content.Route via routeInfoFor(route) — the Kit's flow map reads this off
// leg.originated, not off a re-derivation. Drives selectLegLine directly for
// "crd-order-select" (selectLegLine is a general per-legType primitive; the
// harness's fake Hub answers every leg with a crd-cards/payer-coverage
// response leg, so this legType is what lets the drive actually complete —
// production's promotion of the CRD legs onto this primitive is covered separately).
func TestLegOriginatedCarriesRoute(t *testing.T) {
	env := newInProcessExchange(t)
	env.payerReturns(LegResult{Status: 0, ResponseFHIR: []byte(`{"resourceType":"Bundle","type":"collection"}`)})

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	route, err := env.originator.selectLegLine(env.payerID, "crd-order-select", "corr-1")
	if err != nil {
		t.Fatalf("selectLegLine: %v", err)
	}
	if route.Chain != nil {
		t.Fatalf("want an arm-1 route (nil Chain) — got %+v", route)
	}

	content := Content{
		WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route),
		Bytes: env.crdReq,
	}
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", content); err != nil {
		t.Fatalf("OriginateLeg: %v", err)
	}

	var originated *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.originated" {
			originated = &events[i]
		}
	}
	if originated == nil {
		t.Fatalf("no leg.originated event (events: %+v)", events)
	}
	if originated.Route == nil {
		t.Fatal("leg.originated.Route is nil, want the select-before-build route story")
	}
	if originated.Route.Token != route.Token || originated.Route.BuildLine != route.BuildLine {
		t.Fatalf("Route = %+v, want Token=%s BuildLine=%s", originated.Route, route.Token, route.BuildLine)
	}
	if originated.Route.Chain != nil {
		t.Fatalf("arm-1 Route.Chain must stay nil, got %+v", originated.Route.Chain)
	}
}

// TestOriginateLegFallbackOmitsRoute: the legacy OriginateLeg fallback
// (empty Content.ProfileID -> selectLegToken, arm-1-only) never runs
// select-before-build, so it has no route story to synthesize after the
// fact — leg.originated carries Route: nil on this path (never a
// speculative/re-derived one).
func TestOriginateLegFallbackOmitsRoute(t *testing.T) {
	env := newInProcessExchange(t)
	env.payerReturns(LegResult{Status: 0, ResponseFHIR: []byte(`{"resourceType":"Parameters"}`)})

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, Bytes: env.crdReq}); err != nil {
		t.Fatalf("OriginateLeg: %v", err)
	}

	var originated *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.originated" {
			originated = &events[i]
		}
	}
	if originated == nil {
		t.Fatalf("no leg.originated event (events: %+v)", events)
	}
	if originated.Route != nil {
		t.Fatalf("legacy OriginateLeg fallback must carry Route: nil, got %+v", originated.Route)
	}
}

// TestLegRefusedCarriesStructuredRoute: a version-routing refusal's
// leg.refused event carries the *RouteRefusalError's Own/Peer/BridgeIssue
// structured (Route.Token/BuildLine/Chain empty — nothing was selected),
// while Detail stays the exact Error() string byte-unchanged (the existing
// wire-contract pin TestOriginateRefusesUnsharedLine already covers).
func TestLegRefusedCarriesStructuredRoute(t *testing.T) {
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.crd@2.2"})

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %v", err)
	}

	var refused *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.refused" {
			refused = &events[i]
		}
	}
	if refused == nil {
		t.Fatalf("no leg.refused event (events: %+v)", events)
	}
	if refused.Detail != rre.Error() {
		t.Fatalf("Detail = %q, want the exact Error() string %q (byte-unchanged wire pin)", refused.Detail, rre.Error())
	}
	if refused.Route == nil {
		t.Fatal("leg.refused.Route is nil, want the structured refusal")
	}
	if !reflect.DeepEqual(refused.Route.Own, rre.Own) || !reflect.DeepEqual(refused.Route.Peer, rre.Peer) || refused.Route.BridgeIssue != rre.BridgeIssue {
		t.Fatalf("Route = %+v, want Own=%v Peer=%v BridgeIssue=%q", refused.Route, rre.Own, rre.Peer, rre.BridgeIssue)
	}
	if refused.Route.Token != "" || refused.Route.BuildLine != "" || refused.Route.Chain != nil {
		t.Fatalf("a refusal Route must carry no selection fields, got %+v", refused.Route)
	}
}

// TestLegOriginatedRouteChainOnArm3: on an arm-3 transform-chain route, the
// leg.originated event's Route.Chain mirrors the selected chain step-for-
// step (module/from/to/class), each ChainStep.Module matching
// LossReport.Module's own format ("<contract> <from>-><to>", transform.go).
// A hand-built legRoute (arm-3 selection itself is versionroute_test.go's
// concern) driven through "crd-order-select" — see
// TestLegOriginatedCarriesRoute for why that legType, not "pas-claim".
func TestLegOriginatedRouteChainOnArm3(t *testing.T) {
	env := newInProcessExchange(t)
	env.payerReturns(LegResult{Status: 0, ResponseFHIR: []byte(`{"resourceType":"Bundle","type":"collection"}`)})

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	route := legRoute{
		Token: "pa.crd@2.2", BuildLine: "2.0",
		Chain: []CompatStep{
			{Contract: "pa.crd", From: "2.0", To: "2.1", Class: StepFull},
			{Contract: "pa.crd", From: "2.1", To: "2.2", Class: StepCarry},
		},
	}
	content := Content{
		WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route),
		Bytes: env.crdReq,
	}
	if _, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", content); err != nil {
		t.Fatalf("OriginateLeg: %v", err)
	}

	var originated *ObserverEvent
	for i := range events {
		if events[i].Kind == "leg.originated" {
			originated = &events[i]
		}
	}
	if originated == nil || originated.Route == nil {
		t.Fatalf("no leg.originated Route (events: %+v)", events)
	}
	want := []ChainStep{
		{Module: "pa.crd 2.0->2.1", From: "2.0", To: "2.1", Class: "full"},
		{Module: "pa.crd 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"},
	}
	if !reflect.DeepEqual(originated.Route.Chain, want) {
		t.Fatalf("Route.Chain = %+v, want %+v", originated.Route.Chain, want)
	}
}

// TestTransformRefusalZeroBytes: the end-to-end zero-bytes pin — a
// full origination whose route resolves to a gated transform chain must
// refuse at egressAdapt BEFORE ever reaching the Hub (routeHitCount stays 0,
// refuse-before-forward), observing exactly the leg.failed event this task
// adds. Forces arm (3): own narrows its DeclaredContractVersions to
// pa.pas@2.0 only, the peer declares pa.pas@2.2 only (declareRecipientVersions),
// and D1c's EgressNativeLines={"2.0"} holds arm (2)'s native-reach view away
// from 2.1/2.2 — so selectLegRoute has no arm-1/2 escape and must pick
// chainFor("pa.pas","2.0","2.2"), the same gated,carry chain
// TestEgressAdaptRefusalEmitsLegFailed pins directly. Drives the REAL
// pas_tail.go submitClaimAndResolve (build the conformant Claim Bundle at
// route.BuildLine -> egressAdapt -> [refuses here; OriginateLeg/roundTrip,
// the only path that ever calls the fake Hub's /route, is never reached])
// rather than a hand-rolled site call, so the zero-bytes property is
// asserted against the genuine origination path, not a stub.
func TestTransformRefusalZeroBytes(t *testing.T) {
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.pas@2.2"})
	env.originator.cfg.DeclaredContractVersions = []string{"pa.pas@2.0"}
	env.originator.cfg.EgressNativeLines = []string{"2.0"}
	env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{
		"2.0": shnsdk.NewFakeValidator(), "2.1": shnsdk.NewFakeValidator(), "2.2": shnsdk.NewFakeValidator(),
	}

	var events []ObserverEvent
	env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

	const (
		member      = "MBR-T4"
		patientRef  = "Patient/" + member
		coverageRef = "Coverage/" + member
	)
	order := pasTailServiceRequest()
	qr := []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-t4","status":"completed","subject":{"reference":"` + patientRef + `"}}`)

	_, respJSON, status, msg, err := env.originator.submitClaimAndResolve(env.ctx, env.req, "pci-1", order, qr, patientRef, coverageRef, member, shnsdk.CMSPayerIdentity, env.payerID)
	if err == nil {
		t.Fatal("want an error — the gated chain must refuse before any leg is routed")
	}
	var sce *SemanticChangeError
	if !errors.As(err, &sce) {
		t.Fatalf("want a *SemanticChangeError, got %T: %v", err, err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if msg == "" {
		t.Fatal("want a non-empty refusal message")
	}
	if respJSON != nil {
		t.Fatalf("respJSON must be nil (no leg was ever routed), got %q", respJSON)
	}

	// THE ROW: the fake Hub's /route was never hit — the refusal happened
	// strictly before the Hub round-trip, not after a wasted send.
	if got := env.routeHitCount(); got != 0 {
		t.Fatalf("fake Hub /route was called %d times — a transform refusal must never reach the Hub", got)
	}

	var failed int
	var ev ObserverEvent
	for _, e := range events {
		if e.Kind == "leg.failed" {
			failed++
			ev = e
		}
	}
	if failed != 1 {
		t.Fatalf("want exactly one leg.failed event, got %d (events: %+v)", failed, events)
	}
	if ev.LegType != "pas-claim" || ev.Counterpart != env.payerID {
		t.Fatalf("leg.failed fields = %+v, want LegType=pas-claim Counterpart=%s", ev, env.payerID)
	}
	if !strings.Contains(ev.Detail, "semantic-change refusal") {
		t.Fatalf("leg.failed Detail = %q, want it to contain %q", ev.Detail, "semantic-change refusal")
	}
	if ev.Route == nil || len(ev.Route.Chain) != 2 {
		t.Fatalf("leg.failed Route = %+v, want the attempted 2-step chain", ev.Route)
	}
	wantSteps := []ChainStep{
		{Module: "pa.pas 2.0->2.1", From: "2.0", To: "2.1", Class: "gated"},
		{Module: "pa.pas 2.1->2.2", From: "2.1", To: "2.2", Class: "carry"},
	}
	if !reflect.DeepEqual(ev.Route.Chain, wantSteps) {
		t.Fatalf("leg.failed Route.Chain = %+v, want %+v", ev.Route.Chain, wantSteps)
	}
}
