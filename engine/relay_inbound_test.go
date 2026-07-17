// gateway/engine/relay_inbound_test.go
package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// inboundTestRequester bundles the requester holder id (respondLegError's `requester`
// arg — a plain string, matching the real inbound-handler call sites, e.g.
// crd_native.go's env.Metadata.Sender) with the X25519 key pair the test needs to
// Open the sealed response leg. A real peer Registry carries only PUBLIC keys
// (EncPub); the requester's PRIVATE key is kept here, out-of-band, purely so the
// test can decrypt what buildResponseLeg sealed TO it.
type inboundTestRequester struct {
	ID      string
	EncPub  *[32]byte
	EncPriv *[32]byte
}

// inboundAuthzStub is a minimal fake Authorization Framework for the respondLegError
// harness. respondLegError (via buildResponseLeg -> g.authorize) calls ONLY
// .../authorize to mint the response-leg token; it never calls the Hub /route (that
// round trip belongs to the ORIGINATOR's roundTripInner, not the payer's inbound
// response path) — so, unlike twoPayerSubstrate (payerrouting_test.go), this stub
// needs no /route branch. Lifted from twoPayerSubstrate.handleAuthorize.
type inboundAuthzStub struct {
	authzPriv ed25519.PrivateKey
	clock     func() time.Time
}

func (s *inboundAuthzStub) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/authorize") {
		return errResp("inboundAuthzStub: unexpected call to " + req.URL.Path), nil
	}
	body, _ := io.ReadAll(req.Body)
	var authReq struct {
		Frame         string `json:"frame"`
		Operation     string `json:"operation"`
		SubjectPCI    string `json:"subjectPCI"`
		CorrelationID string `json:"correlationId"`
		PayloadHash   string `json:"payloadHash"`
	}
	_ = json.Unmarshal(body, &authReq)
	tok := shnsdk.Token{
		Operation:     authReq.Operation,
		Scope:         "crd-context",
		Subject:       authReq.SubjectPCI,
		Frame:         authReq.Frame,
		Holder:        "payer",
		CorrelationID: authReq.CorrelationID,
		Expiry:        s.clock().Add(time.Hour),
		PayloadHash:   authReq.PayloadHash,
	}
	tok = signTestToken(tok, s.authzPriv)
	b, _ := json.Marshal(map[string]any{"token": tok})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// newInboundTestGateway builds a hermetic PAYER Gateway wired to a fake Authorization
// Framework (inboundAuthzStub) and a Registry carrying one requester holder
// ("requester", role provider) — everything respondLegError's buildResponseLeg call
// needs to mint a token and seal a response leg to that requester. When frameCapable
// is true the requester's registry entry advertises MessageFrames:["v1"] (the
// negotiation switch): the responder frames its answers only to a
// peer that declared it can decode; a non-capable requester gets the pre-v0.27.0 bare
// contract. Returns the gateway plus the requester's id + key pair (the test needs the
// private half to Open the seal — a real Registry never carries it).
func newInboundTestGateway(t *testing.T, frameCapable bool) (*Gateway, inboundTestRequester) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	reqEncPub, reqEncPriv := genKeyPair(t)
	reqSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	reg := shnsdk.NewRegistry()
	reqEntry := shnsdk.RegistryEntry{ID: "requester", Role: "provider", EncPub: reqEncPub, SignPub: reqSignPub}
	if frameCapable {
		reqEntry.MessageFrames = shnsdk.SupportedMessageFrames()
	}
	reg.Set("requester", reqEntry)

	sor := NewStubHolderData()
	gw := New(Config{
		Role:            "payer",
		HolderID:        "payer",
		Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: authzPub, // payer role requires a non-empty HubTransportPub (hop-auth); unused by respondLegError
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Adjudicator:     NewSandboxAdjudicator(sor, clock), // payer role requires a Responder; this derives the default one
		Clock:           clock,
		Client:          &http.Client{Transport: &inboundAuthzStub{authzPriv: authzPriv, clock: clock}},
	})
	return gw, inboundTestRequester{ID: "requester", EncPub: reqEncPub, EncPriv: reqEncPriv}
}

// newSignedInboundRequest returns a bare inbound request carrying a background
// context. g.authorize (respondLegError's only outbound call, via buildResponseLeg)
// reads ONLY r.Context() off the request — it mints a FRESH holder assertion itself
// and never reads any inbound token/header off r — so no signing is needed here; the
// name matches the shape of the real inbound-handler call sites (an *http.Request
// already past the Hub's per-hop verification).
func newSignedInboundRequest(t *testing.T, g *Gateway, requester string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.Background())
}

// openResponseLeg decodes the sealed response-leg envelope a handler wrote to the
// ResponseRecorder body and Opens it with the requester's key pair (Open needs only
// the RECIPIENT's own X25519 pair — box.OpenAnonymous — never the sender's key, since
// Seal uses an anonymous sealed box). It returns the RAW sealed payload — a v1 message
// frame for a capable requester, a bare application body for a legacy one.
func openResponseLeg(t *testing.T, requester inboundTestRequester, body []byte) []byte {
	t.Helper()
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	payload, err := shnsdk.Open(env, requester.EncPub, requester.EncPriv)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	return payload
}

// TestRespondLegErrorFramesForCapableRequester (new↔new): a responder's application
// NON-2xx answer to a frame-capable requester is sealed as a v1 message frame carrying
// the app status, relayed 200-to-Hub. The sealed payload decodes (shnsdk.DecodeHTTPFrame)
// to the verbatim app status + body.
func TestRespondLegErrorFramesForCapableRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 422, ResponseFHIR: oo}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200 (framed app answer is 200-to-Hub)", rec.Code)
	}
	payload := openResponseLeg(t, requester, rec.Body.Bytes())
	if !shnsdk.IsFramed(payload) {
		t.Fatalf("capable requester: response payload is not a v1 frame: %q", payload)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if hdr.Status != 422 || string(body) != string(oo) {
		t.Fatalf("framed answer = %d/%s, want 422 + OperationOutcome verbatim", hdr.Status, body)
	}
	if hdr.Headers["Content-Type"] != "application/fhir+json" {
		t.Fatalf("framed Content-Type = %q, want application/fhir+json", hdr.Headers["Content-Type"])
	}
}

// TestRespondLegErrorConnectorMisuse2xxRoutesToSuccess: a LegResult with a 2xx Status
// handed to respondLegError is connector misuse (a success belongs on the success seal).
// The guard reroutes it through respondLeg, so a capable requester gets a framed 200
// SUCCESS carrying the body verbatim — never a framed error (a 2xx would be nonsensical
// as an *AppAnswerError/*RelayError), and never bare.
func TestRespondLegErrorConnectorMisuse2xxRoutesToSuccess(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	fhir := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 200, ResponseFHIR: fhir}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200", rec.Code)
	}
	payload := openResponseLeg(t, requester, rec.Body.Bytes())
	if !shnsdk.IsFramed(payload) {
		t.Fatalf("capable requester: 2xx-misuse payload must be a framed success, got bare: %q", payload)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if hdr.Status != 200 || string(body) != string(fhir) {
		t.Fatalf("2xx-misuse framed = %d/%s, want a 200 success carrying the body verbatim", hdr.Status, body)
	}
	if hdr.Headers["Content-Type"] != "application/fhir+json" {
		t.Fatalf("2xx-misuse Content-Type = %q, want application/fhir+json (success seal)", hdr.Headers["Content-Type"])
	}
}

// TestRespondLegErrorFramesSynthesizedErrorForCapableRequester: a NON-2xx answer with an
// EMPTY ResponseFHIR (the shape most internal-rejection call sites carry — a DTR 400
// "unknown questionnaire canonical" or a PAS 409 that sets only Message) frames a
// synthesized {"error": Message} body as application/json, not an empty one.
func TestRespondLegErrorFramesSynthesizedErrorForCapableRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	msg := "ClaimUpdate references no pending claim available for this patient"
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 409, Message: msg}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200 (framed app answer is 200-to-Hub)", rec.Code)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if hdr.Status != 409 {
		t.Fatalf("framed status = %d, want 409", hdr.Status)
	}
	if hdr.Headers["Content-Type"] != "application/json" {
		t.Fatalf("synthesized-body Content-Type = %q, want application/json", hdr.Headers["Content-Type"])
	}
	var synth map[string]string
	if err := json.Unmarshal(body, &synth); err != nil {
		t.Fatalf("synthesized body not valid JSON: %v (%s)", err, body)
	}
	if synth["error"] != msg {
		t.Fatalf("synthesized body error = %q, want %q", synth["error"], msg)
	}
}

// TestRespondLegErrorBareForLegacyRequester (new↔legacy): a NON-2xx answer to a
// non-capable requester keeps the pre-v0.27.0 contract — a BARE non-2xx {"error": msg}
// writeJSON (which the payload-blind Hub reports as its generic mechanical 502). Byte-
// identical to the legacy shape: HTTP status == result.Status, body == {"error": msg}.
func TestRespondLegErrorBareForLegacyRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, false)
	msg := "upstream payer PAS submit returned 502 Bad Gateway"
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 502, Message: msg}, "pci-1", requester.ID, "")
	if rec.Code != 502 {
		t.Fatalf("legacy requester: to-Hub status = %d, want 502 (bare non-2xx)", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("legacy body not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if len(got) != 1 || got["error"] != msg {
		t.Fatalf("legacy body = %v, want exactly {\"error\": %q}", got, msg)
	}
}

// TestRespondLegFramesSuccessForCapableRequester (success framing): a 2xx answer to a
// frame-capable requester is sealed as a v1 frame(200, application/fhir+json, body).
func TestRespondLegFramesSuccessForCapableRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	fhir := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLeg(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", fhir, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200", rec.Code)
	}
	payload := openResponseLeg(t, requester, rec.Body.Bytes())
	if !shnsdk.IsFramed(payload) {
		t.Fatalf("capable requester: success payload is not a v1 frame: %q", payload)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if hdr.Status != 200 || string(body) != string(fhir) {
		t.Fatalf("framed success = %d/%s, want 200 + FHIR verbatim", hdr.Status, body)
	}
}

// TestRespondLegBareSuccessForLegacyRequester (success framing): a 2xx answer to a
// non-capable requester stays BARE (the pre-v0.27.0 contract) — the sealed payload is
// the FHIR body itself, NOT a frame.
func TestRespondLegBareSuccessForLegacyRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, false)
	fhir := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLeg(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", fhir, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200", rec.Code)
	}
	payload := openResponseLeg(t, requester, rec.Body.Bytes())
	if shnsdk.IsFramed(payload) {
		t.Fatalf("legacy requester: success payload must be bare, got a frame")
	}
	if string(payload) != string(fhir) {
		t.Fatalf("legacy success payload = %s, want bare FHIR verbatim", payload)
	}
}

// TestPASNativeSuccessFramedForCapableRequester drives the conformant PAS submit
// handler (handlePASNativeInbound) — whose response leg is built at the direct
// buildResponseLeg site (pas_native.go:277) — end to end for a frame-capable
// requester, and proves that direct site frames too: the sealed response leg decodes
// to frame(200, application/fhir+json, ClaimResponse).
func TestPASNativeSuccessFramedForCapableRequester(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	pci, _, ok := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !ok {
		t.Fatal("MBR-COVERED not resolvable in StubHolderData")
	}
	bundle := conformantPASBundleWithQR(t, "MBR-COVERED")

	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender:          requester.ID,
		Recipient:       "payer",
		TransactionType: "pas-claim",
		AuthorityFrame:  "payer-coverage",
		Timestamp:       g.cfg.Clock().Format(time.RFC3339),
		CorrelationID:   "corr-pas-1",
	}, bundle, g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("seal inbound PAS envelope: %v", err)
	}
	tok := shnsdk.Token{Operation: "pas-submit", Subject: pci, CorrelationID: "corr-pas-1"}

	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.handlePASNativeInbound(rec, r, env, tok)
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	payload := openResponseLeg(t, requester, rec.Body.Bytes())
	if !shnsdk.IsFramed(payload) {
		t.Fatalf("capable requester: PAS response payload is not a v1 frame: %q", payload)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if hdr.Status != 200 || hdr.Headers["Content-Type"] != "application/fhir+json" {
		t.Fatalf("framed PAS answer = %d/%q, want 200 + application/fhir+json", hdr.Status, hdr.Headers["Content-Type"])
	}
	if parsed, perr := shnsdk.ParseClaimResponse(body); perr != nil || parsed.Outcome != "approved" {
		t.Fatalf("framed PAS body not an approved ClaimResponse: %+v err=%v", parsed, perr)
	}
}
