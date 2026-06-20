// originate_test.go — hermetic unit tests for runCRDThenDTR's per-value behavior
// switch (Finding 1 + Finding 2 of the design spec §3, FR-G25).
//
// Injection approach: a stubSubstrate intercepts the Gateway's HTTP client at
// the transport level. For /authorize it returns a pre-signed Token (using a
// test authzPriv generated per test); for the Hub /route it seals the
// configured canned CardCoverage response back to the PROVIDER's test enc key
// and returns it with a valid response-leg Token. This keeps the tests
// hermetic — no live RI, no network, no full substrate boot — while exercising
// the REAL runCRDThenDTR branch logic through an actual UC handler call.
package engine

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ---- shared crypto helpers ----

// genKeyPair generates an ephemeral X25519 key pair.
func genKeyPair(t *testing.T) (*[32]byte, *[32]byte) {
	t.Helper()
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genKeyPair: %v", err)
	}
	return pub, priv
}

// genED25519 generates an ephemeral Ed25519 key pair.
func genED25519(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genED25519: %v", err)
	}
	return pub, priv
}

// sha256hexT computes the lowercase hex SHA-256. Mirrors the unexported
// engine sha256hex.
func sha256hexT(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signTestToken returns a signed copy of tok using authzPriv. Replicates the
// tokenSigningPayload logic (marshal with Signature=nil) since the public
// SDK's tokenSigningPayload is unexported. json.Marshal of a well-formed
// shnsdk.Token never errors, so the return value is always valid.
func signTestToken(tok shnsdk.Token, authzPriv ed25519.PrivateKey) shnsdk.Token {
	c := tok
	c.Signature = nil
	payload, _ := json.Marshal(c) // Token fields are all JSON-safe; never errors
	tok.Signature = ed25519.Sign(authzPriv, payload)
	return tok
}

// sealForProvider seals payload to providerEncPub (NaCl anonymous box, matching
// shnsdk.Seal) and stamps a valid response-leg Token so the provider's
// roundTrip VerifyBound check passes. Returns the encoded envelope bytes.
// All json.Marshal / box.SealAnonymous calls are infallible for well-formed
// inputs; errors are returned so the caller (an http.RoundTripper) can surface
// them as 500 responses instead of panicking.
func sealForProvider(meta shnsdk.Metadata, payload []byte, provEncPub *[32]byte, authzPriv ed25519.PrivateKey, corrID, operation, frame, holder, pci string, clock time.Time) ([]byte, error) {
	ct, err := box.SealAnonymous(nil, payload, provEncPub, rand.Reader)
	if err != nil {
		return nil, err
	}
	env := shnsdk.Envelope{Metadata: meta, Ciphertext: ct}
	respTok := shnsdk.Token{
		Operation:     operation,
		Scope:         "crd-context",
		Subject:       pci,
		Frame:         frame,
		Holder:        holder,
		CorrelationID: corrID,
		Expiry:        clock.Add(time.Hour),
		PayloadHash:   sha256hexT(ct),
	}
	respTok = signTestToken(respTok, authzPriv)
	tokBytes, err := json.Marshal(respTok)
	if err != nil {
		return nil, err
	}
	env.Metadata.AuthzToken = string(tokBytes)
	return shnsdk.EncodeEnvelope(env)
}

// ---- stubSubstrate ----

// stubSubstrate is a configurable RoundTripper that stands in for the full
// SHN substrate (AuthzURL + HubURL) in originate_test.go. It intercepts:
//   - calls whose path ends in "/authorize": returns a signed authz Token.
//   - calls whose path ends in "/route": returns a pre-sealed CRD card
//     response containing covResp, or (if legCount > 0 and past the first CRD
//     leg) a realistic DTR questionnaire-package response so the shared prefix
//     in runCRDThenDTR can reach the branch-under-test.
//
// All other paths return 500 (shouldn't happen in these tests).
type stubSubstrate struct {
	// authzPriv signs the per-leg tokens returned by /authorize.
	authzPriv ed25519.PrivateKey
	// providerEncPub is the provider's X25519 public key; the stub Hub seals
	// its synthetic response with this key so the provider can Open() it.
	providerEncPub *[32]byte
	// covResp is the CardCoverage the stub payer returns on the CRD leg.
	covResp shnsdk.CardCoverage
	// clock drives token expiry.
	clock func() time.Time
	// pci is the PCI the provider resolved for the test member.
	pci string
	// corrIDs records the correlation IDs seen on each /route call (the stub
	// echoes them back so the provider's correlation-ID verification passes).
	legCount int
}

func (s *stubSubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	body, _ := io.ReadAll(req.Body)

	switch {
	case strings.HasSuffix(path, "/authorize"):
		return s.handleAuthorize(req, body)
	case strings.HasSuffix(path, "/route"):
		return s.handleRoute(req, body)
	default:
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"unexpected stub call to ` + path + `"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}
}

// handleAuthorize returns a minimal signed Token accepted by the provider's
// authorize() → roundTrip internal verification. The provider uses the Token
// only to stamp AuthzToken on the outbound envelope; it does NOT verify the
// REQUEST-leg token against AuthzPub (only the RESPONSE-leg token is verified).
func (s *stubSubstrate) handleAuthorize(_ *http.Request, body []byte) (*http.Response, error) {
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
	resp := map[string]any{"token": tok}
	b, _ := json.Marshal(resp)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// handleRoute decodes the sealed incoming envelope to extract the corrID, then
// seals and returns the appropriate canned response:
//   - leg 0 (CRD): returns BuildCards(s.covResp) wrapped in a sealed envelope.
//   - leg 1+ (DTR and beyond, only reached on the happy-path which these tests
//     do not exercise): returns a canned questionnaire package.
func (s *stubSubstrate) handleRoute(_ *http.Request, body []byte) (*http.Response, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	corrID := env.Metadata.CorrelationID

	var respPayload []byte
	var respOp, respFrame string

	leg := s.legCount
	s.legCount++

	switch leg {
	case 0: // CRD leg
		respPayload, err = shnsdk.BuildCards(s.covResp)
		if err != nil {
			return errResp("stub: BuildCards: " + err.Error()), nil
		}
		respOp = "crd-cards"
		respFrame = "payer-coverage"
	default:
		// Should not be reached in the branch tests (all branches return before DTR).
		return errResp("stub: unexpected leg " + itoa(leg)), nil
	}

	meta := shnsdk.Metadata{
		Sender:          "payer",
		Recipient:       "provider",
		TransactionType: "crd-order-select",
		AuthorityFrame:  respFrame,
		Timestamp:       s.clock().UTC().Format(time.RFC3339),
		CorrelationID:   corrID,
	}
	out, err := sealForProvider(meta, respPayload, s.providerEncPub, s.authzPriv,
		corrID, respOp, respFrame, "payer", s.pci, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(out)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func errResp(msg string) *http.Response {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// ---- test gateway builder ----

// crdTestSystem builds a minimal provider Gateway whose substrate is replaced
// by a stubSubstrate returning the given CRD card coverage.
func crdTestSystem(t *testing.T, cov shnsdk.CardCoverage) (*Gateway, string) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	sor := NewStubHolderData()
	pci, _, _ := sor.ResolvePatient("MBR-COVERED")

	stub := &stubSubstrate{
		authzPriv:      authzPriv,
		providerEncPub: provEncPub,
		covResp:        cov,
		clock:          clock,
		pci:            pci,
	}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub})

	// Fake authz + hub URLs — the stub transport intercepts at the path suffix.
	const fakeBase = "http://stub.test"

	gw := New(Config{
		Role:          "provider",
		HolderID:      "provider",
		CounterpartID: "payer",
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		AuthzURL:        fakeBase,
		AuthzPub:        authzPub,
		HubTransportPub: authzPub, // not used by provider (only inbound gateways check it)
		HubURL:          fakeBase,
		Reg:             reg,
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Clock:           clock,
		NPI:             "1234567890",
		// No Adjudicator/Responder (provider role doesn't use them).
		// Populator defaults to managed (not reached in branch tests).
		Client: &http.Client{Transport: stub},
	})
	return gw, pci
}

// callUC03 drives the UC-03 handler on the given gateway using httptest and
// returns the recorded response.
func callUC03(t *testing.T, gw *Gateway) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// ---- behavior-branch tests (FR-G25, Finding 1 + Finding 2) ----

// TestRunCRDThenDTR_NotCovered verifies the explicit terminal stop for
// Covered==not-covered (AI-1: a coverage denial never silently proceeds).
// Expects HTTP 200 with outcome:"not-covered" (NOT a 502 "did not proceed").
func TestRunCRDThenDTR_NotCovered(t *testing.T) {
	gw, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredNotCovered,
		PANeeded: shnsdk.PANeededNoAuth,
	})
	rec := callUC03(t, gw)

	if rec.Code != http.StatusOK {
		t.Fatalf("not-covered: want 200 (terminal stop), got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not-covered: unmarshal response: %v", err)
	}
	if body["outcome"] != "not-covered" {
		t.Errorf("not-covered: want outcome=not-covered, got %v (full body: %s)", body["outcome"], rec.Body.String())
	}
	if v, ok := body["covered"].(bool); !ok || v {
		t.Errorf("not-covered: want covered=false, got covered=%v", body["covered"])
	}
}

// TestRunCRDThenDTR_Satisfied verifies the fail-closed response when the payer
// signals PA already satisfied (PANeeded==satisfied). The short-circuit path is
// deferred this slice; expect HTTP 502 with a message containing "satisfied".
func TestRunCRDThenDTR_Satisfied(t *testing.T) {
	gw, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:       shnsdk.CoveredCovered,
		PANeeded:      shnsdk.PANeededSatisfied,
		SatisfiedPaID: "PA-PREV-001",
	})
	rec := callUC03(t, gw)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("satisfied: want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "satisfied") {
		t.Errorf("satisfied: response body must contain \"satisfied\", got: %s", rec.Body.String())
	}
}

// TestRunCRDThenDTR_ConditionalPANeeded verifies that PANeeded==conditional is
// handled fail-closed (deferred this slice). Expect HTTP 502 with "conditional".
func TestRunCRDThenDTR_ConditionalPANeeded(t *testing.T) {
	gw, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredCovered,
		PANeeded: shnsdk.PANeededConditional,
	})
	rec := callUC03(t, gw)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("conditional PA: want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "conditional") {
		t.Errorf("conditional PA: response body must contain \"conditional\", got: %s", rec.Body.String())
	}
}

// TestRunCRDThenDTR_ConditionalCovered verifies that Covered==conditional is
// handled fail-closed (deferred this slice). Expect HTTP 502 with "conditional".
func TestRunCRDThenDTR_ConditionalCovered(t *testing.T) {
	gw, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredConditional,
		PANeeded: shnsdk.PANeededAuthNeeded,
	})
	rec := callUC03(t, gw)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("conditional covered: want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "conditional") {
		t.Errorf("conditional covered: response body must contain \"conditional\", got: %s", rec.Body.String())
	}
}
