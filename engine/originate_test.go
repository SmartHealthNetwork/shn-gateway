// originate_test.go — hermetic unit tests for runCRDThenDTROrder's per-value behavior
// switch (FR-G25).
//
// Injection approach: a stubSubstrate intercepts the Gateway's HTTP client at
// the transport level. For /authorize it returns a pre-signed Token (using a
// test authzPriv generated per test); for the Hub /route it seals the
// configured canned CardCoverage response back to the PROVIDER's test enc key
// and returns it with a valid response-leg Token. This keeps the tests
// hermetic — no live RI, no network, no full substrate boot — while exercising
// the REAL runCRDThenDTROrder branch logic through an actual UC handler call.
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
//     in runCRDThenDTROrder can reach the branch-under-test.
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
	// legTypes records, in order, the env.Metadata.TransactionType seen on each
	// /route call (leg 0 = crd-order-select, then dtr-questionnaire-fetch / pas-claim
	// for the legs the prefix attempts). The verdict-driven branch tests assert on
	// WHICH legs were attempted (proceeded vs skipped), not on reaching a 200 — leg 1+
	// still returns an error, which is sufficient to prove the gate let the flow past.
	legTypes []string
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
	s.legTypes = append(s.legTypes, env.Metadata.TransactionType)

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
		TransactionType: env.Metadata.TransactionType, // echo the request leg (crd-order-select, etc.)
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
// by a stubSubstrate returning the given CRD card coverage. The stub is returned so
// the verdict-driven branch tests can assert which legs the prefix attempted (stub.legTypes).
func crdTestSystem(t *testing.T, cov shnsdk.CardCoverage) (*Gateway, *stubSubstrate, string) {
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
	return gw, stub, pci
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
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{
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

// TestRunCRDThenDTROrder_NotCovered_ProceedFlag proves the proceedOnNotCovered param
// (Unit 2 / spec §6 D-S2-2). The generic not-covered STOP (FR-G25 / AI-1) is the DEFAULT
// (false) for every caller; ONLY a caller that opts in (handleUC08 composite, to carry the
// not-covered J3490 order to PAS for br-payer's formal A2 "Not Certified" ClaimResponse)
// proceeds past it with the order built. The opt-in never yields an auth on a denial:
// handleUC08 still asserts the PAS result is DENIED (the existing approved→502 guard).
func TestRunCRDThenDTROrder_NotCovered_ProceedFlag(t *testing.T) {
	const sys, code, disp, dx = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", "J3490", "Unclassified drugs", "D57.1"
	notCovered := shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth}

	// DEFAULT (false): a coverage denial STOPS — 200 not-covered, ok=false (FR-G25 preserved
	// for every non-opt-in caller; the adversarial Row 1 drives this on uc03/sandbox).
	t.Run("default-stops", func(t *testing.T) {
		gw, _, _ := crdTestSystem(t, notCovered)
		req := httptest.NewRequest(http.MethodPost, "/scenario/uc08", nil)
		rec := httptest.NewRecorder()
		_, ok := gw.runCRDThenDTROrder(rec, req, "MBR-COVERED", sys, code, disp, dx, false)
		if ok {
			t.Fatal("default: ok=true on a not-covered card — the FR-G25 STOP was bypassed without opt-in")
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not-covered") {
			t.Fatalf("default: want a 200 not-covered terminal stop, got %d %s", rec.Code, rec.Body.String())
		}
	})

	// OPT-IN (true): PROCEED — ok=true, the order built (for the PAS A2 submit), nothing terminal written.
	t.Run("optin-proceeds", func(t *testing.T) {
		gw, _, _ := crdTestSystem(t, notCovered)
		req := httptest.NewRequest(http.MethodPost, "/scenario/uc08", nil)
		rec := httptest.NewRecorder()
		res, ok := gw.runCRDThenDTROrder(rec, req, "MBR-COVERED", sys, code, disp, dx, true)
		if !ok {
			t.Fatalf("opt-in: ok=false on a not-covered card — the proceed flag did not let UC-08 reach PAS; body=%s", rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("opt-in: a terminal response was written (%s) — proceed must write nothing and let the caller submit PAS", rec.Body.String())
		}
		if len(res.srJSON) == 0 {
			t.Fatal("opt-in: returned no ServiceRequest — the order must be built for the PAS A2 submit")
		}
	})
}

// TestRunCRDThenDTR_Satisfied verifies the fail-closed response when the payer
// signals PA already satisfied (PANeeded==satisfied). The short-circuit path is
// deferred this slice; expect HTTP 502 with a message containing "satisfied".
func TestRunCRDThenDTR_Satisfied(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{
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

// legAttempted reports whether the prefix attempted a /route leg of the given
// transaction type (the stub records every leg in legTypes).
func legAttempted(legTypes []string, txType string) bool {
	for _, lt := range legTypes {
		if lt == txType {
			return true
		}
	}
	return false
}

// TestRunCRDThenDTR_ConditionalPANeeded verifies that PANeeded==conditional STOPS at
// the PA gate (spec 2B): the PA decision keys on the pa-needed axis, and
// `pa-needed:conditional` is NOT in {auth-needed, performpa} ⇒ PARequired() is false ⇒
// the new `!cov.PARequired()` arm fires. A conditional/unresolved PA requirement is not a
// PA requirement, so this prefix (UC-03+, which always submits PAS) has nothing to do →
// 502 "expected PA-required card" (NOT the old "conditional unsupported" — that message
// is gone). Distinct from Covered==conditional, which DOES proceed (see below).
func TestRunCRDThenDTR_ConditionalPANeeded(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredCovered,
		PANeeded: shnsdk.PANeededConditional,
	})
	rec := callUC03(t, gw)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("conditional PA: want 502 (not PA-required), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "expected PA-required card") {
		t.Errorf("conditional PA: body must say \"expected PA-required card\", got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "conditional coverage unsupported") {
		t.Errorf("conditional PA: the old \"conditional coverage unsupported\" gate must be gone, got: %s", rec.Body.String())
	}
}

// TestRunCRDThenDTR_ConditionalCovered verifies that Covered==conditional (with PA
// required) PROCEEDS past the CRD gate (spec 2B: a config-only gateway handles any
// conformant CRD verdict — conditional coverage is not a stop). br-payer's G0151 returns
// conditional + auth-needed + clinical. With a questionnaire present, the prefix proceeds
// to fetch DTR — proven by the dtr-questionnaire-fetch leg being attempted (the old gate
// 502'd "conditional coverage unsupported" before any second leg).
func TestRunCRDThenDTR_ConditionalCovered(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:        shnsdk.CoveredConditional,
		PANeeded:       shnsdk.PANeededAuthNeeded,
		Questionnaires: []string{"http://example.org/q"},
	})
	rec := callUC03(t, gw)

	if strings.Contains(rec.Body.String(), "conditional coverage unsupported") {
		t.Fatalf("conditional covered: must NOT 502 \"conditional unsupported\" (generic verdict handling), got: %s", rec.Body.String())
	}
	// It proceeded past the CRD gate: a leg beyond crd-order-select was attempted.
	if !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
		t.Errorf("conditional covered: expected the prefix to PROCEED to DTR (dtr-questionnaire-fetch leg), legTypes=%v body=%s", stub.legTypes, rec.Body.String())
	}
}

// TestRunCRDThenDTR_NoDocSkipsDTR verifies the no-doc PA path (spec 2B, br-payer L8000:
// covered + auth-needed + NO DTR questionnaire). PA is required (PARequired true) so the
// prefix proceeds, but with no questionnaire NeedsDTR() is false ⇒ the DTR block is
// skipped entirely and the flow goes straight to PAS. Proven by: the
// dtr-questionnaire-fetch leg is NOT attempted, and the pas-claim leg IS.
func TestRunCRDThenDTR_NoDocSkipsDTR(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredCovered,
		PANeeded: shnsdk.PANeededAuthNeeded,
		// no Questionnaires → no-doc
	})
	rec := callUC03(t, gw)

	if legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
		t.Errorf("no-doc: DTR must be SKIPPED (no dtr-questionnaire-fetch leg), legTypes=%v body=%s", stub.legTypes, rec.Body.String())
	}
	if !legAttempted(stub.legTypes, "pas-claim") {
		t.Errorf("no-doc: expected the prefix to proceed straight to PAS (pas-claim leg), legTypes=%v body=%s", stub.legTypes, rec.Body.String())
	}
}

// TestRunCRDThenDTR_ClinicalRoutesDTR verifies the converse of the no-doc case: a
// clinical card (conditional + auth-needed WITH a questionnaire — br-payer G0151) routes
// the dtr-questionnaire-fetch leg (the doc-needed axis, NeedsDTR(), decides DTR
// independently of the PA decision).
func TestRunCRDThenDTR_ClinicalRoutesDTR(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:        shnsdk.CoveredConditional,
		PANeeded:       shnsdk.PANeededAuthNeeded,
		Questionnaires: []string{"http://example.org/q"},
	})
	_ = callUC03(t, gw)

	if !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") {
		t.Errorf("clinical: expected DTR to be routed (dtr-questionnaire-fetch leg), legTypes=%v", stub.legTypes)
	}
}

// classifyTestGateway builds a minimal valid provider Gateway with the given
// OriginationProfile so classifyResolution can be exercised in isolation (it reads
// only cfg.OriginationProfile; New still requires SoR/Store/Identity, so a real stub
// SoR is supplied). No substrate is wired — classifyResolution makes no network calls.
func classifyTestGateway(t *testing.T, profile string) *Gateway {
	t.Helper()
	_, provSignPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	sor := NewStubHolderData()
	return New(Config{
		Role:               "provider",
		HolderID:           "provider",
		OriginationProfile: profile,
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		SoR:   sor,
		Store: sor,
	})
}

// TestClassifyResolution is the C4 rejection discipline for the PAS-resolution decision
// (spec §2B-bis): ONLY a genuine A1 approval is approved. A composite amendment now resolves to a
// real A1 at the payer-gw responder (it polls br-payer's timer A4→A1), so a resolution site sees
// approved | denied | unresolved-pend here — and everything not approved → caller 502s (a pend can
// never mask a denial or be a silent pass — C1). Profile-independent now (the per-profile terminal
// pend is gone); both profiles asserted so no assertion is vacuous.
func TestClassifyResolution(t *testing.T) {
	// approved: bare ClaimResponse, outcome complete + preAuthRef present.
	approved := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","use":"preauthorization","preAuthRef":"PA-0123456789ab","preAuthPeriod":{"end":"2026-09-02"}}`)
	// denied: bare ClaimResponse carrying reviewActionCode A3.
	denied := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","use":"preauthorization","item":[{"adjudication":[{"extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction","extension":[{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode","valueCodeableConcept":{"coding":[{"system":"https://codesystem.x12.org/005010/306","code":"A3"}]}}]}]}]}]}`)
	// unresolved pend: a well-formed PAS Bundle with a Task input (ParseClaimResponse treats it as
	// ambiguous — neither approved nor denied). The responder resolves real pends to A1 before the
	// originator sees them, so a pend HERE is a non-resolution → NOT approved (caller 502s).
	pend := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"queued","use":"preauthorization"}},{"resource":{"resourceType":"Task","status":"requested","input":[{"type":{"text":"operative-diagnostic-report"},"valueString":"operative-diagnostic-report"}]}}]}`)
	// garbage: a non-Bundle, non-ClaimResponse object.
	garbage := []byte(`{}`)

	cases := []struct {
		name         string
		profile      string
		in           []byte
		wantApproved bool
	}{
		{"approved/composite", "composite", approved, true},
		{"approved/sandbox", "", approved, true},
		{"denied/composite", "composite", denied, false},   // denial → 502 (C1)
		{"denied/sandbox", "", denied, false},              // denial → 502 (C1)
		{"pend/composite", "composite", pend, false},       // unresolved pend → 502 (no silent pass)
		{"pend/sandbox", "", pend, false},                  // unresolved pend → 502 (no silent pass)
		{"garbage/composite", "composite", garbage, false}, // unparseable → fail closed
		{"garbage/sandbox", "", garbage, false},            // unparseable → fail closed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := classifyTestGateway(t, tc.profile)
			_, approvedGot := gw.classifyResolution(tc.in)
			if approvedGot != tc.wantApproved {
				t.Errorf("approved = %v, want %v", approvedGot, tc.wantApproved)
			}
		})
	}
}

// TestRunCRDThenDTROrder_NamesPayer proves the CRD origination Coverage carries a
// resolvable named payer (contained #cms-payer), not the dangling Organization/payer —
// a real Da Vinci payer (br-payer) 400s "lacks valid payer identifier" otherwise.
func TestRunCRDThenDTROrder_NamesPayer(t *testing.T) {
	covJSON, err := shnsdk.BuildCoverageWithPayer("Patient/MBR-COVERED", "Coverage/MBR-COVERED")
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	if !strings.Contains(string(covJSON), "#cms-payer") {
		t.Fatalf("expected contained #cms-payer payer reference, got: %s", covJSON)
	}
	// guard: the bare builder (what we are replacing) must NOT name a resolvable payer
	bare, _ := shnsdk.BuildCoverage("Patient/MBR-COVERED", "Coverage/MBR-COVERED")
	if strings.Contains(string(bare), "#cms-payer") {
		t.Fatal("bare BuildCoverage unexpectedly names cms-payer; the distinction this task relies on is gone")
	}
}
