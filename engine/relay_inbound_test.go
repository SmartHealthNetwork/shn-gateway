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
// crd_native.go's env.Metadata.Sender) with the X25519 key pair
// decryptAndUnwrapResponseLeg needs to Open the sealed response leg. A real peer
// Registry carries only PUBLIC keys (EncPub); the requester's PRIVATE key is kept
// here, out-of-band, purely so the test can decrypt what buildResponseLeg sealed TO it.
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
// needs to mint a token and seal a response leg to that requester. Returns the
// gateway plus the requester's id + key pair (decryptAndUnwrapResponseLeg needs the
// private half to Open the seal — a real Registry never carries it).
func newInboundTestGateway(t *testing.T) (*Gateway, inboundTestRequester) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	reqEncPub, reqEncPriv := genKeyPair(t)
	reqSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	reg := shnsdk.NewRegistry()
	reg.Set("requester", shnsdk.RegistryEntry{ID: "requester", Role: "provider", EncPub: reqEncPub, SignPub: reqSignPub})

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

// decryptAndUnwrapResponseLeg decodes the sealed response-leg envelope respondLegError
// wrote to the ResponseRecorder body, Opens it with the requester's key pair (Open
// needs only the RECIPIENT's own X25519 pair — box.OpenAnonymous — never the
// sender's key, since Seal uses an anonymous sealed box), and unwraps the relay
// wrapper (unwrapRelayResponse).
func decryptAndUnwrapResponseLeg(t *testing.T, requester inboundTestRequester, body []byte) (int, []byte) {
	t.Helper()
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	payload, err := shnsdk.Open(env, requester.EncPub, requester.EncPriv)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	status, respBody, _ := unwrapRelayResponse(payload)
	return status, respBody
}

// respondLegError with the flag ON must return HTTP 200 to the Hub carrying a
// sealed envelope whose decrypted payload unwraps to {status, body}.
func TestRespondLegError_FlagOn_SealsWrappedRelay(t *testing.T) {
	g, requester := newInboundTestGateway(t) // helper: builds a Gateway + a requester holder in its registry
	g.cfg.RelayRecipientErrors = true
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID) // helper: a request carrying a valid inbound correlation
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 502, ResponseFHIR: oo}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200 (sealed relay)", rec.Code)
	}
	status, body := decryptAndUnwrapResponseLeg(t, requester, rec.Body.Bytes()) // helper: Open + unwrapRelayResponse
	if status != 502 || string(body) != string(oo) {
		t.Fatalf("unwrapped relay = %d/%s, want 502 + OperationOutcome", status, body)
	}
}

// respondLegError with the flag ON, given a non-2xx LegResult with an EMPTY
// ResponseFHIR — the shape most internal-rejection call sites carry (e.g. a DTR 400
// "unknown questionnaire canonical" or a PAS 409 that sets only Message) — must
// synthesize {"error": Message} as the wrapped body instead of sealing an empty one.
func TestRespondLegError_FlagOn_SynthesizesErrorBodyWhenNoResponseFHIR(t *testing.T) {
	g, requester := newInboundTestGateway(t)
	g.cfg.RelayRecipientErrors = true
	msg := "ClaimUpdate references no pending claim available for this patient"
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 409, Message: msg}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200 (sealed relay)", rec.Code)
	}
	status, body := decryptAndUnwrapResponseLeg(t, requester, rec.Body.Bytes()) // helper: Open + unwrapRelayResponse
	if status != 409 {
		t.Fatalf("unwrapped relay status = %d, want 409", status)
	}
	var synth map[string]string
	if err := json.Unmarshal(body, &synth); err != nil {
		t.Fatalf("synthesized body not valid JSON: %v (%s)", err, body)
	}
	if synth["error"] != msg {
		t.Fatalf("synthesized body error = %q, want %q", synth["error"], msg)
	}
}

// respondLegError with the flag ON must route a 2xx LegResult through the BARE
// success seal (respondLeg), never the relay wrapper — this locks the feature's core
// "wrap only non-2xx" invariant. Unlike decryptAndUnwrapResponseLeg
// (which only reads unwrapRelayResponse's status+body), this test also inspects the
// wrapped bool directly: if respondLegError ever started wrapping a 2xx, wrapped
// would flip to true here and the test would fail.
func TestRespondLegError_FlagOn_2xxStatusStaysBareUnwrapped(t *testing.T) {
	g, requester := newInboundTestGateway(t)
	g.cfg.RelayRecipientErrors = true
	fhir := []byte(`{"resourceType":"ClaimResponse","status":"active"}`)
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 200, ResponseFHIR: fhir}, "pci-1", requester.ID, "")
	if rec.Code != 200 {
		t.Fatalf("to-Hub status = %d, want 200 (sealed relay)", rec.Code)
	}
	status, body := decryptAndUnwrapResponseLeg(t, requester, rec.Body.Bytes()) // helper: Open + unwrapRelayResponse
	if status != 200 || string(body) != string(fhir) {
		t.Fatalf("unwrapped relay = %d/%s, want 200 + bare FHIR verbatim", status, body)
	}
	// Open the envelope again and inspect unwrapRelayResponse's wrapped bool
	// directly — decryptAndUnwrapResponseLeg discards it, but it's exactly what
	// proves the 2xx path took respondLeg's bare seal rather than the wrapper: a
	// bare payload happens to unwrap to (200, payload, false) too, so status+body
	// alone can't distinguish "genuinely bare" from "wrapped with status 200."
	env, err := shnsdk.DecodeEnvelope(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	payload, err := shnsdk.Open(env, requester.EncPub, requester.EncPriv)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	if _, _, wrapped := unwrapRelayResponse(payload); wrapped {
		t.Fatalf("2xx LegResult produced a wrapped relay payload; want bare (respondLeg), wrapped=%v", wrapped)
	}
}

func TestRespondLegError_FlagOff_LegacyHubRoutingFailed(t *testing.T) {
	g, requester := newInboundTestGateway(t)
	g.cfg.RelayRecipientErrors = false
	rec := httptest.NewRecorder()
	r := newSignedInboundRequest(t, g, requester.ID)
	g.respondLegError(rec, r, "payer-coverage", "crd-cards", "crd-order-select",
		"corr-1", LegResult{Status: 502, Message: "upstream payer PAS submit returned 502 Bad Gateway"}, "pci-1", requester.ID, "")
	if rec.Code != 502 {
		t.Fatalf("flag off: to-Hub status = %d, want 502 (legacy non-2xx)", rec.Code)
	}
}
