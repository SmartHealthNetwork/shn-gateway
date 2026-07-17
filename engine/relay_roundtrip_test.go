// gateway/engine/relay_roundtrip_test.go
//
// Shared in-process exchange harness for the opaque-payload message-frame spec
// (2026-07-17): a provider (originator) Gateway wired to a fake Hub+Authz
// RoundTripper (relaySubstrate) that seals back a TEST-CONFIGURABLE LegResult
// on the response leg. Mirrors twoPayerTestSystem/twoPayerSubstrate
// (payerrouting_test.go) — same seal/authz crypto helpers from
// originate_test.go (genKeyPair/genED25519/signTestToken/sealForProvider/
// errResp), same shape — but the fake Hub's response leg is settable per-test
// via payerReturns instead of always sealing a canned success, so the frame
// decode (roundTripInner), the CRD ingress handler, and the Observer seam can
// all drive the SAME harness against a real originator Gateway without faking
// the seal/authz crypto.
package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// relaySubstrate is a fake Hub+Authz RoundTripper standing in for the
// AuthzURL/HubURL substrate in the relay-recipient-response harness. It
// intercepts "/authorize" (signs a token, mirroring twoPayerSubstrate) and
// "/route" (seals back whichever LegResult the test most recently configured
// via setResult), instead of forwarding to a real payer. The
// envelope/token-signing machinery is REAL — only the decrypted response
// PAYLOAD is test-controlled — so the sealed leg still exercises
// roundTripInner's response-leg authz/correlation/sender checks exactly as a
// real payer's answer would.
type relaySubstrate struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	clock          func() time.Time

	mu     sync.Mutex
	result LegResult // seal target for /route; meaningful only when set==true
	set    bool      // false until the test calls setResult (falls back to default success cards)

	// mutateResp, if set, transforms the sealed response-leg wire bytes AFTER
	// sealForProvider succeeds and BEFORE they're wrapped into the stub's HTTP
	// response — letting an adversarial test corrupt an otherwise-valid sealed
	// leg (e.g. its authz token) without hand-rolling the seal/encode machinery
	// itself. nil by default: additive, existing tests are unaffected.
	mutateResp func([]byte) []byte
}

// setResult configures what the NEXT (and subsequent, until changed) /route
// call(s) seal back.
func (s *relaySubstrate) setResult(lr LegResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = lr
	s.set = true
}

func (s *relaySubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body, _ := io.ReadAll(req.Body)
	switch {
	case strings.HasSuffix(path, "/authorize"):
		return s.handleAuthorize(body)
	case strings.HasSuffix(path, "/route"):
		return s.handleRoute(body)
	default:
		return errResp("unexpected stub call to " + path), nil
	}
}

func (s *relaySubstrate) handleAuthorize(body []byte) (*http.Response, error) {
	var req struct {
		Frame         string `json:"frame"`
		Operation     string `json:"operation"`
		SubjectPCI    string `json:"subjectPCI"`
		CorrelationID string `json:"correlationId"`
		PayloadHash   string `json:"payloadHash"`
	}
	_ = json.Unmarshal(body, &req)
	tok := shnsdk.Token{
		Operation:     req.Operation,
		Scope:         "crd-context",
		Subject:       req.SubjectPCI,
		Frame:         req.Frame,
		Holder:        "provider",
		CorrelationID: req.CorrelationID,
		Expiry:        s.clock().Add(time.Hour),
		PayloadHash:   req.PayloadHash,
	}
	tok = signTestToken(tok, s.authzPriv)
	b, _ := json.Marshal(map[string]any{"token": tok})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// handleRoute decodes the sealed outbound envelope and seals back the
// configured LegResult: a non-2xx Status is sealed as a v1 message frame via
// shnsdk.EncodeHTTPFrame — byte-identical to the wire form a frame-capable payer's
// respondLegError produces — so this drives roundTripInner's frame decode faithfully
// (the harness recipient advertises v1, see newInProcessExchange). A zero/2xx Status
// (or no setResult call at all) seals ResponseFHIR VERBATIM (letting the frame_originate
// tests inject a pre-built frame or a bare stale-feed payload), or a canned
// success-cards payload if ResponseFHIR is also unset.
func (s *relaySubstrate) handleRoute(body []byte) (*http.Response, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	var reqTok shnsdk.Token
	_ = json.Unmarshal([]byte(env.Metadata.AuthzToken), &reqTok)

	s.mu.Lock()
	lr, set := s.result, s.set
	s.mu.Unlock()

	var respPayload []byte
	switch {
	case !set || lr.Status == 0 || lr.Status/100 == 2:
		if lr.ResponseFHIR != nil {
			respPayload = lr.ResponseFHIR
		} else {
			respPayload, err = shnsdk.BuildCards(shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
			if err != nil {
				return errResp("stub: BuildCards: " + err.Error()), nil
			}
		}
	default:
		body := lr.ResponseFHIR
		if len(body) == 0 {
			body, _ = json.Marshal(map[string]string{"error": lr.Message})
		}
		respPayload, err = shnsdk.EncodeHTTPFrame(lr.Status, "application/fhir+json", body)
		if err != nil {
			return errResp("stub: EncodeHTTPFrame: " + err.Error()), nil
		}
	}

	meta := shnsdk.Metadata{
		Sender:          env.Metadata.Recipient, // echo the resolved holder back as Sender
		Recipient:       "provider",
		TransactionType: env.Metadata.TransactionType,
		AuthorityFrame:  "payer-coverage",
		Timestamp:       s.clock().UTC().Format(time.RFC3339),
		CorrelationID:   env.Metadata.CorrelationID,
	}
	out, err := sealForProvider(meta, respPayload, s.providerEncPub, s.authzPriv,
		env.Metadata.CorrelationID, "crd-cards", "payer-coverage", env.Metadata.Recipient, reqTok.Subject, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	if s.mutateResp != nil {
		out = s.mutateResp(out)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(out)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// inProcessExchange is the shared harness for relay-recipient-response tests:
// a real provider (originator) Gateway wired to a relaySubstrate standing in
// for the Hub + Authorization Framework. payerReturns configures what the
// fake Hub's response leg carries; callers then drive OriginateLeg or the
// Da Vinci ingress handlers directly against env.originator. Observer-related
// cases need no new field — they set env.originator.cfg.Observer directly
// (same package).
type inProcessExchange struct {
	originator *Gateway
	substrate  *relaySubstrate
	ctx        context.Context
	req        *http.Request
	payerID    string
	crdReq     []byte
}

// payerReturns configures the fake Hub's response leg: a non-2xx lr.Status is
// sealed as a v1 message frame (the wire form a frame-capable payer's
// respondLegError produces); a zero/2xx Status seals lr.ResponseFHIR verbatim (or
// the harness's default success cards if ResponseFHIR is also unset).
func (e *inProcessExchange) payerReturns(lr LegResult) {
	e.substrate.setResult(lr)
}

// corruptResponseToken arms the fake Hub's response leg with a MUTATED
// authz token: it decodes the sealed envelope sealForProvider just produced,
// re-signs the response-leg Token with a FRESH ed25519 key the originator's
// cfg.AuthzPub does NOT correspond to (modeling a token never minted by the
// real Authorization Framework), and re-encodes. The token still unmarshals
// fine (same JSON shape) — it fails at roundTripInner's VerifyBound
// signature check, not at json.Unmarshal — so this is the ONE mutation on an
// otherwise-valid relayed leg (adversarial-row discipline: valid exchange −
// one mutation → reject).
func (e *inProcessExchange) corruptResponseToken(t *testing.T) {
	t.Helper()
	_, wrongPriv := genED25519(t)
	e.substrate.mutateResp = func(out []byte) []byte {
		env, err := shnsdk.DecodeEnvelope(out)
		if err != nil {
			t.Fatalf("corruptResponseToken: decode envelope: %v", err)
		}
		var tok shnsdk.Token
		if err := json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok); err != nil {
			t.Fatalf("corruptResponseToken: unmarshal token: %v", err)
		}
		tok = signTestToken(tok, wrongPriv)
		tokBytes, err := json.Marshal(tok)
		if err != nil {
			t.Fatalf("corruptResponseToken: marshal token: %v", err)
		}
		env.Metadata.AuthzToken = string(tokBytes)
		mutated, err := shnsdk.EncodeEnvelope(env)
		if err != nil {
			t.Fatalf("corruptResponseToken: encode envelope: %v", err)
		}
		return mutated
	}
}

// newInProcessExchange builds the harness: a provider Gateway (Role
// "provider") registered against a single payer holder ("payer"), talking to
// a relaySubstrate over Config.Client — mirroring twoPayerTestSystem
// (payerrouting_test.go) but with a test-configurable response leg instead of
// an always-success canned card. Both holders advertise message-frame v1
// (SupportedMessageFrames), so the originator's roundTripInner decodes the frame
// the substrate seals — negotiation is registry-driven, keyed
// on the RECIPIENT's advertised frames.
//
// PayerRouter + EnableIngressForTest are added for the CRD ingress drive (which
// routes off the inbound Coverage's CMSPayerIdentity/00001 via recipientForWith
// and gates on ingressAuthOK first) but are INERT for the direct OriginateLeg
// drive: OriginateLeg takes `recipient` as an explicit parameter and never
// consults PayerRouter or ingressAuthBypass.
func newInProcessExchange(t *testing.T) *inProcessExchange {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	stub := &relaySubstrate{authzPriv: authzPriv, providerEncPub: provEncPub, clock: clock}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	// The recipient advertises message-frame v1 so roundTripInner decodes the frame
	// handleRoute seals; frame_originate_test.go re-asserts this per
	// case via advertiseRecipientFrameV1 (idempotent) and its stale-feed row seals a
	// bare payload against this same v1-advertising entry.
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub, MessageFrames: shnsdk.SupportedMessageFrames()})

	const fakeBase = "http://relay-stub.test"
	cfg := Config{
		Role:            "provider",
		HolderID:        "provider",
		Identity:        shnsdk.Identity{HolderID: "provider", SignPriv: provSignPriv, EncPub: provEncPub, EncPriv: provEncPriv},
		AuthzURL:        fakeBase,
		AuthzPub:        authzPub,
		HubTransportPub: authzPub, // not checked for role "provider"; kept for parity with twoPayerTestSystem
		HubURL:          fakeBase,
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             NewStubHolderData(),
		Store:           NewStubHolderData(),
		Clock:           clock,
		NPI:             "1234567890",
		Client:          &http.Client{Transport: stub},
		PayerRouter:     payerRouterFor(t, "payer"),
	}
	EnableIngressForTest(&cfg) // bypassed auth ⇒ IngressBaseURL/IngressClients not required
	gw := New(cfg)

	ctx := context.Background()
	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

	return &inProcessExchange{
		originator: gw,
		substrate:  stub,
		ctx:        ctx,
		req:        req,
		payerID:    "payer",
		crdReq:     []byte(`{"resourceType":"Parameters"}`),
	}
}

// conformantCRDRequest is a conformant CDS Hooks order-select request for `member`, with a
// callback (fhirServer + fhirAuthorization) to prove stripping, and only patient+coverage
// prefetch (the histories must be resolved from the SoR to be self-contained). The coverage
// carries a payor identity (CMSPayerIdentity) so the CRD ingress can route it off the inbound
// Coverage — payerRouterFor maps it to the harness's "payer" holder (FR-G40; no default).
// Lifted verbatim from test/ingressconformance/crd_test.go (Gate-1 hermetic conformance fence)
// so this in-package harness drives the SAME conformant fixture.
func conformantCRDRequest(member string) []byte {
	ref := "Patient/" + member
	return []byte(`{
      "hook":"order-select","hookInstance":"hi-1",
      "fhirServer":"https://provider.example/fhir",
      "fhirAuthorization":{"token_type":"Bearer","access_token":"tok"},
      "context":{"userId":"Practitioner/p1","patientId":"` + member + `",
        "draftOrders":{"resourceType":"Bundle","type":"collection","entry":[
          {"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"` + ref + `"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}
        ]},"selections":["ServiceRequest/sr1"]},
      "prefetch":{
        "patient":{"resourceType":"Patient","id":"` + member + `"},
        "coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"` + ref + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}
      }
    }`)
}

// crdIngressRequest returns a conformant CDS Hooks order-select POST for
// MBR-COVERED: the fixture's coverage carries CMSPayerIdentity/00001,
// which payerRouterFor maps to the harness's "payer" holder, and MBR-COVERED
// is seeded in NewStubHolderData, so handleCRDIngress's subject-PCI bind,
// self-containment, and payer routing all resolve and the call reaches
// OriginateLeg.
func (e *inProcessExchange) crdIngressRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(conformantCRDRequest("MBR-COVERED")))
}

// End-to-end in-process: a responder whose leg returns LegResult{502, OperationOutcome}
// (framed by the payer, negotiated via registry v1) must surface at the originator as a
// *RelayError{502, body}, NOT the Hub's generic mechanical fault. Reuses the in-process
// substrate harness (hub + authz + originator gateway) above.
func TestRoundTrip_RecipientNon2xx_SurfacesRelayError(t *testing.T) {
	env := newInProcessExchange(t)
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error"}]}`)
	env.payerReturns(LegResult{Status: 502, ResponseFHIR: oo})
	_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, Bytes: env.crdReq})
	var re *RelayError
	if !errors.As(err, &re) {
		t.Fatalf("want *RelayError, got %v", err)
	}
	if re.Status != 502 || string(re.Body) != string(oo) {
		t.Fatalf("RelayError = %d/%s, want 502 + OperationOutcome", re.Status, re.Body)
	}
}
