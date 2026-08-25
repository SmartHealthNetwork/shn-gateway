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
	"context"
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
	// denyAuthorize makes /authorize return 403 — the errAuthorizationDenied
	// path (LegMetric outcome "denied"). failAuthorize returns 500 — the
	// opaque "authorization failed" path (outcome "failed").
	denyAuthorize bool
	failAuthorize bool
	// pasDenialRationale, when set, makes leg 1 (the PAS-submit response, only
	// reached by a caller that proceeds past a not-covered CRD verdict — D-S2-2)
	// return a DENIED ClaimResponse carrying this rationale, built at pasLine.
	// Used by TestHandleUC08_DemoLane_ProceedsPastNotCoveredToDeny — the only test
	// that reaches leg 1 today (every other crdTestSystem-based test returns
	// before DTR/PAS).
	pasDenialRationale string
	pasLine            string
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
	if s.denyAuthorize {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}
	if s.failAuthorize {
		return errResp("stub: authorize forced failure"), nil
	}
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
		// R3: the response Operation is contract-keyed per leg TYPE (workstream_pa.go's
		// pa.crd manifest rows) — "crd-cards" for order-select, "crd-dispatch-cards" for
		// order-dispatch (handleUC03Oxygen's re-key onto the oxygen family). A mismatched
		// Operation fails the response-leg contract check ("response leg authorization
		// failed"), so this must echo the REQUEST leg's own expected response op, not a
		// literal frozen to order-select's shape.
		if env.Metadata.TransactionType == "crd-order-dispatch" {
			respOp = "crd-dispatch-cards"
		} else {
			respOp = "crd-cards"
		}
		respFrame = "payer-coverage"
	case 1: // PAS-submit leg — only reached via the not-covered proceed opt-in (D-S2-2).
		if s.pasDenialRationale == "" {
			return errResp("stub: unexpected leg 1 (no pasDenialRationale configured)"), nil
		}
		respPayload, err = shnsdk.BuildDeniedResponseAtLine(s.pasLine, "Patient/"+s.pci, corrID, s.pasDenialRationale, s.clock())
		if err != nil {
			return errResp("stub: BuildDeniedResponseAtLine: " + err.Error()), nil
		}
		respOp = "pas-response"
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

	sor := newCensusSoR()
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

	gw := mustNew(t, Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
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

// callUC03Branch is callUC03 with an explicit branch in the JSON body
// (handleUC03's branch switch).
func callUC03Branch(t *testing.T, gw *Gateway, branch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", strings.NewReader(`{"branch":"`+branch+`"}`))
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandleUC03_BridgeRefuseSelectsMember pins the member half of the
// sanctioned handleUC03 branch switch, observed the way this file's
// existing tests observe row behavior (legAttempted/stub.legTypes) — a fresh
// crdTestSystem fixture per branch, matching the file's one-test-one-fixture
// idiom (stub.legTypes accumulates for the fixture's lifetime, so branches
// must not share one). crdTestSystem's PayerRouter (payerRouterFor) is
// registered ONLY for shnsdk.CMSPayerIdentity — the identity MBR-COVERED's
// Coverage carries. MBR-BRIDGE-REFUSE's Coverage carries the distinct demo
// identity engine.BridgeRefusePayerID (urn:shn:demo-payer|SHN-BRIDGE-REFUSE,
// holderdata.go), which this fixture never registers. recipientForWith
// (gateway.go) fails closed 422 on an unregistered payer identifier BEFORE
// any leg is attempted (FR-G40/AI-G11/OWD-G10) — so reaching THAT specific
// 422, naming THAT specific identifier, with NO leg attempted, proves the
// branch switch resolved MBR-BRIDGE-REFUSE and not MBR-COVERED (which DOES
// clear this same gate — see the "" case below and every unbranched
// callUC03 test in this file). This is deliberately NOT a full run: the
// demo's PAS-leg refusal only fires against the real
// narrowed/pas-skewed peer, which this hermetic unit fixture does not stand
// up — driving further would just hit the SAME unregistered-payer wall for a
// different reason.
func TestHandleUC03_BridgeRefuseSelectsMember(t *testing.T) {
	t.Run("bridge-refuse: fails closed at routing, no leg attempted", func(t *testing.T) {
		gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded})
		rec := callUC03Branch(t, gw, "bridge-refuse")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("want 422 (unregistered demo payer — proves member selection ran), got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "SHN-BRIDGE-REFUSE") {
			t.Errorf("body = %s, want it to name the bridge-refuse payer identifier", rec.Body.String())
		}
		if len(stub.legTypes) != 0 {
			t.Errorf("legTypes = %v, want none — the routing gate must fail BEFORE any leg", stub.legTypes)
		}
	})

	t.Run("unknown branch: 400, uc01's idiom", func(t *testing.T) {
		gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded})
		rec := callUC03Branch(t, gw, "bogus")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
		}
		if len(stub.legTypes) != 0 {
			t.Errorf("legTypes = %v, want none — an unknown branch must reject before any member/SoR work", stub.legTypes)
		}
	})

	t.Run(`"" is the literal-default branch: clears the routing gate exactly like callUC03's nil body`, func(t *testing.T) {
		gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded})
		_ = callUC03Branch(t, gw, "")
		// R3: the "" branch re-keys onto the oxygen family's order-DISPATCH hook (register
		// §11 ruling (b)) — crd-order-select was the L8000/order-select shape this branch
		// carried before.
		if !legAttempted(stub.legTypes, "crd-order-dispatch") {
			t.Errorf("legTypes = %v, want crd-order-dispatch attempted — MBR-COVERED must still clear routing", stub.legTypes)
		}
	})
}

// ---- behavior-branch tests (FR-G25, Finding 1 + Finding 2) ----

// runCRDThenDTROrderTestTuple is an arbitrary, stable order-select tuple these tests drive
// runCRDThenDTROrder with directly. R3 re-keyed handleUC03's own "" branch onto the
// oxygen family's DIFFERENT gate (order-dispatch, NeedsDTR-based — runCRDDispatch), so the
// tests below — which are about runCRDThenDTROrder's OWN generic verdict switch
// (FR-G25/Finding 1+2), not about UC-03's business content — call it DIRECTLY (mirroring
// TestRunCRDThenDTROrder_NotCovered_ProceedFlag's existing pattern) instead of routing
// through an HTTP handler that no longer carries this shape.
const (
	runCRDThenDTROrderTestSystem  = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets"
	runCRDThenDTROrderTestCode    = "L8000"
	runCRDThenDTROrderTestDisplay = "Breast prosthesis, mastectomy bra"
	runCRDThenDTROrderTestDx      = "Z90.10"
)

// callRunCRDThenDTROrder drives runCRDThenDTROrder directly for MBR-COVERED with the
// tuple above and returns the recorded response — the order-select analog of callUC03,
// now that handleUC03's own HTTP surface no longer rides this function for its "" branch.
func callRunCRDThenDTROrder(t *testing.T, gw *Gateway) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	rec := httptest.NewRecorder()
	gw.runCRDThenDTROrder(rec, req, "MBR-COVERED", runCRDThenDTROrderTestSystem, runCRDThenDTROrderTestCode, runCRDThenDTROrderTestDisplay, runCRDThenDTROrderTestDx, false)
	return rec
}

// TestRunCRDThenDTR_NotCovered verifies the explicit terminal stop for
// Covered==not-covered (AI-1: a coverage denial never silently proceeds).
// Expects HTTP 200 with outcome:"not-covered" (NOT a 502 "did not proceed").
func TestRunCRDThenDTR_NotCovered(t *testing.T) {
	gw, _, _ := crdTestSystem(t, shnsdk.CardCoverage{
		Covered:  shnsdk.CoveredNotCovered,
		PANeeded: shnsdk.PANeededNoAuth,
	})
	rec := callRunCRDThenDTROrder(t, gw)

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
// (D-S2-2). The generic not-covered STOP (FR-G25 / AI-1) is the DEFAULT
// (false) for every caller; ONLY a caller that opts in (handleUC08 provider-data, to carry the
// not-covered J3490 order to PAS for br-payer's formal A2 "Not Certified" ClaimResponse)
// proceeds past it with the order built. The opt-in never yields an auth on a denial:
// handleUC08 still asserts the PAS result is DENIED (the existing approved→502 guard).
func TestRunCRDThenDTROrder_NotCovered_ProceedFlag(t *testing.T) {
	const sys, code, disp, dx = "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", "J3490", "Unclassified drugs", "D57.1"
	notCovered := shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth}

	// DEFAULT (false): a coverage denial STOPS — 200 not-covered, ok=false (FR-G25 preserved
	// for every non-opt-in caller; the adversarial Row 1 drives this on uc03).
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

// TestHandleUC08_DemoLane_ProceedsPastNotCoveredToDeny is the behavioral guard for the
// EXACT bug live `make smoke` caught: with
// OriginationProfile == "demo" — what an unset ORIGINATION_PROFILE now normalizes to at
// the config boundary, gateway/app.go's loadConfig — a not-covered CRD verdict for
// UC-08's J3490 family must NOT terminally stop at the CRD leg (the generic FR-G25
// STOP). It must proceed to PAS and come back DENIED with a rationale, matching what
// internal/scenariodrive.DemoChecks()'s UC08 check asserts
// (All(Has(`"denied":true`), Has(`"rationale"`))). Before this fix, an empty profile
// hit the CRD not-covered stop and UC-08 returned
// {"covered":false,"outcome":"not-covered","paRequired":false} instead — precisely the
// live smoke failure this test locks down.
func TestHandleUC08_DemoLane_ProceedsPastNotCoveredToDeny(t *testing.T) {
	notCovered := shnsdk.CardCoverage{Covered: shnsdk.CoveredNotCovered, PANeeded: shnsdk.PANeededNoAuth}
	gw, stub, _ := crdTestSystem(t, notCovered)
	gw.cfg.OriginationProfile = "demo" // the normalized value every real boot produces now

	// crdTestSystem hardcodes stub.pci to MBR-COVERED's PCI, but the demo profile's
	// sceneMember resolves UC-08 to MBR-D-UC08 (censusfixture_test.go) instead — a
	// DIFFERENT PCI. The stub's sealed response-leg token must carry the subject the
	// request actually resolved, or the response-leg subject-bind verification
	// (H1/AI-11) rejects it as a genuine cross-patient mismatch, not a test bug.
	pci := shnsdk.ResolvePCI("MBR-D-UC08", "1968-12-21", "Adeyemi")
	stub.pci = pci

	const rationale = "not medically necessary; conservative therapy under 6 weeks"
	stub.pasDenialRationale = rationale
	stub.pasLine = highestLine(contractLineSet(gw.declaredContractVersions(), "pa.pas"))

	req := httptest.NewRequest(http.MethodPost, "/scenario/uc08", nil)
	rec := httptest.NewRecorder()
	gw.handleUC08(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("demo not-covered UC08: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if v, ok := body["denied"].(bool); !ok || !v {
		t.Fatalf("demo not-covered UC08: want denied=true (proceeded past the CRD not-covered stop to a real PAS deny), got body=%s", rec.Body.String())
	}
	if r, _ := body["rationale"].(string); r == "" {
		t.Fatalf("demo not-covered UC08: want a non-empty rationale, got body=%s", rec.Body.String())
	}
	// Pin the ABSENCE of the CRD-leg terminal-stop shape — the exact regression: a
	// demo-lane UC08 that stopped at the not-covered CRD verdict (the bug) writes
	// {"covered":false,"outcome":"not-covered",...} instead of ever reaching PAS.
	if _, has := body["outcome"]; has {
		t.Fatalf(`demo not-covered UC08: response carries "outcome" (the CRD-leg terminal-stop shape) — did NOT proceed to PAS; body=%s`, rec.Body.String())
	}
	if len(stub.legTypes) < 2 || stub.legTypes[1] != "pas-claim" {
		t.Fatalf("demo not-covered UC08: want leg 1 to be pas-claim (proceeded to PAS submit), got legTypes=%v", stub.legTypes)
	}
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
	rec := callRunCRDThenDTROrder(t, gw)

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
	rec := callRunCRDThenDTROrder(t, gw)

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
	// This one needs the PAS leg actually attempted (not just runCRDThenDTROrder's own
	// return), so it drives the tail itself — the direct-call analog of what handleUC03's
	// (pre-R3) HTTP handler used to do after the prefix returned ok.
	req := httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil)
	rec := httptest.NewRecorder()
	res, ok := gw.runCRDThenDTROrder(rec, req, "MBR-COVERED", runCRDThenDTROrderTestSystem, runCRDThenDTROrderTestCode, runCRDThenDTROrderTestDisplay, runCRDThenDTROrderTestDx, false)
	if ok {
		_, _, status, msg, _ := gw.submitClaimAndResolve(req.Context(), req, res.pci, res.srJSON, res.qrJSON, res.patientRef, res.coverageRef, res.member, res.payer, res.recipient)
		if status != 0 {
			writeJSON(rec, status, map[string]string{"error": msg})
		}
	}

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
	sor := newCensusSoR()
	return mustNew(t, Config{
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

// TestClassifyResolution is the C4 rejection discipline for the PAS-resolution decision:
// ONLY a genuine A1 approval is approved. An amendment now resolves to a
// real A1 at the payer-gw responder (it polls br-payer's timer A4→A1), so a resolution site sees
// approved | denied | unresolved-pend here — and everything not approved → caller 502s (a pend can
// never mask a denial or be a silent pass — C1). Profile-independent now (the per-profile terminal
// pend is gone); both the provider-data and default-arm profiles asserted so no assertion is vacuous.
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
		{"approved/provider-data", "provider-data", approved, true},
		{"approved/default", "", approved, true},
		{"denied/provider-data", "provider-data", denied, false},   // denial → 502 (C1)
		{"denied/default", "", denied, false},                      // denial → 502 (C1)
		{"pend/provider-data", "provider-data", pend, false},       // unresolved pend → 502 (no silent pass)
		{"pend/default", "", pend, false},                          // unresolved pend → 502 (no silent pass)
		{"garbage/provider-data", "provider-data", garbage, false}, // unparseable → fail closed
		{"garbage/default", "", garbage, false},                    // unparseable → fail closed
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
	covJSON, err := shnsdk.BuildCoverageWithPayer("Patient/MBR-COVERED", "MBR-COVERED", shnsdk.CMSPayerIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	if !strings.Contains(string(covJSON), "#cms-payer") {
		t.Fatalf("expected contained #cms-payer payer reference, got: %s", covJSON)
	}
	// guard: the bare builder (what we are replacing) must NOT name a resolvable payer
	bare, _ := shnsdk.BuildCoverage("Patient/MBR-COVERED", "MBR-COVERED")
	if strings.Contains(string(bare), "#cms-payer") {
		t.Fatal("bare BuildCoverage unexpectedly names cms-payer; the distinction this task relies on is gone")
	}
}

// ---------------------------------------------------------------------------
// targetsBrPayer predicate tests (A4)
// ---------------------------------------------------------------------------

// failIfCalledValidator is a shnsdk.Validator test double that fails the test if Validate
// is ever called. Used to assert that the R-8 ingress-$validate skip is in effect.
type failIfCalledValidator struct{ t *testing.T }

func (f failIfCalledValidator) Validate(_ context.Context, _ []byte, _ string) (shnsdk.Result, error) {
	f.t.Fatal("validator must not be called (R-8 ingress skip expected)")
	return shnsdk.Result{}, nil
}

func TestTargetsBrPayer(t *testing.T) {
	if !targetsBrPayer("provider-data") {
		t.Fatal("provider-data should target br-payer")
	}
	// "" and any unrecognized lane are SHN-produced, never br-payer.
	for _, p := range []string{"", "unknown-lane", "provider"} {
		if targetsBrPayer(p) {
			t.Fatalf("%q must not target br-payer", p)
		}
	}
}

// R-8: provider-data relays br-payer's foreign DTR/PAS bytes → ingress $validate MUST be
// skipped for a PAYER-DIRECTED leg. The skip lives only in validateFHIRPayerIngress, never
// in plain validateFHIR.
func TestValidateFHIR_IngressSkip_ProviderData(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", Validator: failIfCalledValidator{t}}}
	if status, _ := g.validateFHIRPayerIngress(context.Background(), []byte(`{}`), ""); status != 0 {
		t.Fatalf("provider-data payer-ingress must skip $validate (R-8); got status=%d", status)
	}
}

// ---------------------------------------------------------------------------
// relaysReferencePayerBytes / R-8 ingress-$validate scope: the skip is a property of the
// COUNTERPARTY (reference-payer bytes, relayed verbatim), not of any one origination-profile
// string.
// ---------------------------------------------------------------------------

// recordingValidator is a shnsdk.Validator test double that RECORDS every resourceJSON it was
// asked to validate (so a test can assert real content actually flowed through the call, not
// merely that Validate returned a canned answer) and returns a configurable Valid result.
type recordingValidator struct {
	calls [][]byte
	valid bool
}

func (v *recordingValidator) Validate(_ context.Context, resourceJSON []byte, _ string) (shnsdk.Result, error) {
	v.calls = append(v.calls, resourceJSON)
	return shnsdk.Result{Valid: v.valid}, nil
}

// TestRelaysReferencePayerBytes pins the predicate's exact membership: provider-data (live
// br-payer) and demo (the in-process mirror of it) relay reference-payer bytes. "" is NOT in
// that list any more: an unset ORIGINATION_PROFILE is normalized to "demo" ONCE, at the
// gateway/app.go config
// boundary, before the engine — or this predicate — ever sees it. A raw "" reaching this
// predicate directly (as this hermetic test does, bypassing gateway/app entirely) is no
// longer a case any real deployment produces, so it correctly falls into the same
// fail-closed-to-validating bucket as every other unrecognized value — same as the dead,
// no-longer-configured profile literals TestTargetsBrPayer's own unrecognized-lane list
// pins.
//
// This predicate answers only "does this LANE relay reference-payer bytes at all" — it is
// the lane half of the R-8 skip decision, not the whole thing. The counterparty half (is
// THIS leg's response actually from the
// reference payer) lives entirely in which function a call site uses: validateFHIRPayerIngress
// (payer-directed legs only) versus plain validateFHIR (everything else, including the UC-05
// facility searchset — always validates regardless of what this predicate says about the
// lane).
func TestRelaysReferencePayerBytes(t *testing.T) {
	for _, p := range []string{"provider-data", "demo"} {
		if !relaysReferencePayerBytes(p) {
			t.Errorf("%q should relay reference-payer bytes (R-8 skip expected)", p)
		}
	}
	for _, p := range []string{"", "provider", "unknown-lane"} {
		if relaysReferencePayerBytes(p) {
			t.Errorf("%q must not relay reference-payer bytes", p)
		}
	}
}

// (a) THE LIVE BUG: a demo-lane PAYER-DIRECTED ingress leg carrying reference-payer bytes
// must NOT be $validate-refused. failIfCalledValidator proves the SKIP itself happened (it
// fails the test the instant Validate is called at all) rather than proving a validator
// merely happened to answer "valid" — the exact "test passes for the wrong reason" failure
// mode this fix closes. Pinned for the literal "demo" profile — the ONLY value this
// predicate itself treats specially: an unset ORIGINATION_PROFILE is now normalized to
// "demo" upstream, at the gateway/app.go config boundary
// (TestLoadConfig_UnsetOriginationProfileNormalizesToDemo pins that), so by the time any
// real Gateway's validateFHIR runs, OriginationProfile is never "" — this test's Gateway is
// constructed directly with the ALREADY-NORMALIZED value, exactly as a real boot hands it
// in. Calls validateFHIRPayerIngress — the skip is only ever reachable through that
// function now, never plain validateFHIR.
func TestValidateFHIR_IngressSkip_Demo(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "demo", Validator: failIfCalledValidator{t}}}
	if status, msg := g.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Parameters"}`), ""); status != 0 {
		t.Fatalf("demo-lane payer-ingress must skip $validate (R-8, post-retirement); got status=%d msg=%q", status, msg)
	}
}

// (a2) THE REGRESSION THIS SPLIT CLOSES, pinned directly: a demo-lane ingress leg whose
// counterparty is NOT the reference payer — the shape handleUC05's facility CDex
// federated-query searchset takes — must still $validate and fail closed on invalid bytes,
// even though the LANE (demo) is one that relays reference-payer bytes for its
// payer-directed legs. Before this split, plain validateFHIR shared the skip with every
// ingress call on the demo lane; a facility leg on the demo lane silently went unvalidated.
// Calling plain
// validateFHIR (never validateFHIRPayerIngress) is what every non-payer-directed ingress
// call site (originate.go's UC-05 federated-query read) now does.
func TestValidateFHIR_FacilityIngressStillFailsClosed_Demo(t *testing.T) {
	v := &recordingValidator{valid: false}
	g := &Gateway{cfg: Config{OriginationProfile: "demo", Validator: v}}
	status, msg := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Bundle"}`), "ingress", "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("demo-lane facility ingress with an invalid resource: status=%d, want %d; msg=%q — the R-8 payer skip must never leak to a non-payer-directed leg", status, http.StatusUnprocessableEntity, msg)
	}
	if len(v.calls) != 1 {
		t.Fatalf("facility ingress validator called %d times, want exactly 1 — it must genuinely run, not be vacuously skipped", len(v.calls))
	}
}

// (a3) THE OTHER HALF OF FINDING 1, pinned directly: a demo-lane PAYER-DIRECTED ingress leg
// (the shape every CRD/DTR/PAS response-from-the-payer call site takes) still skips
// $validate — the fix scopes the carve-out to payer-directed legs, it does not remove it.
func TestValidateFHIR_PayerIngressStillSkips_Demo(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "demo", Validator: failIfCalledValidator{t}}}
	if status, msg := g.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Bundle"}`), ""); status != 0 {
		t.Fatalf("demo-lane payer-directed ingress must still skip $validate after the Finding-1 scope fix; got status=%d msg=%q", status, msg)
	}
}

// (b) MUTATION-VERIFY: egress (always SHN-produced, every lane) must still $validate and fail
// closed on the demo lane — the R-8 skip is ingress-only, never a blanket "this lane never
// validates" bypass. recordingValidator's valid:false IS the mutation (an invalid resource);
// the call-count assertion proves the validator genuinely ran (not vacuously skipped) before
// rejecting it.
func TestValidateFHIR_EgressStillFailsClosed_Demo(t *testing.T) {
	v := &recordingValidator{valid: false}
	g := &Gateway{cfg: Config{OriginationProfile: "demo", Validator: v}}
	status, msg := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Bundle"}`), "egress", "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("demo egress with an invalid resource: status=%d, want %d; msg=%q", status, http.StatusUnprocessableEntity, msg)
	}
	if len(v.calls) != 1 {
		t.Fatalf("egress validator called %d times, want exactly 1 — the ingress skip must not leak into egress", len(v.calls))
	}
}

// (c) MUTATION-VERIFY: a lane that is neither demo/"" nor provider-data (a dead profile
// literal no compose service sets any more) still $validates ingress and rejects invalid
// bytes even on a PAYER-DIRECTED leg (validateFHIRPayerIngress) — the skip is scoped exactly
// to the lanes whose counterparty is the reference payer, never a blanket ingress bypass for
// anything unrecognized.
func TestValidateFHIR_IngressStillFailsClosed_OtherLane(t *testing.T) {
	v := &recordingValidator{valid: false}
	g := &Gateway{cfg: Config{OriginationProfile: "unknown-lane", Validator: v}}
	status, msg := g.validateFHIRPayerIngress(context.Background(), []byte(`{"resourceType":"Bundle"}`), "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("non-reference-payer-lane payer-ingress with an invalid resource: status=%d, want %d; msg=%q", status, http.StatusUnprocessableEntity, msg)
	}
	if len(v.calls) != 1 {
		t.Fatalf("ingress validator called %d times, want exactly 1", len(v.calls))
	}
}

// (d) MUTATION-VERIFY: plain validateFHIR (dir=="ingress") never skips, on ANY lane —
// including demo/provider-data, the two lanes that DO relay reference-payer bytes for
// their payer-directed legs. This is the structural guarantee the validateFHIR /
// validateFHIRPayerIngress split rests on: the skip is reachable ONLY through
// validateFHIRPayerIngress, never as a side effect of the lane value alone.
func TestValidateFHIR_PlainIngressNeverSkips_AnyLane(t *testing.T) {
	for _, profile := range []string{"", "demo", "provider-data", "unknown-lane"} {
		v := &recordingValidator{valid: false}
		g := &Gateway{cfg: Config{OriginationProfile: profile, Validator: v}}
		status, msg := g.validateFHIR(context.Background(), []byte(`{"resourceType":"Bundle"}`), "ingress", "")
		if status != http.StatusUnprocessableEntity {
			t.Errorf("profile %q: plain validateFHIR ingress with an invalid resource: status=%d, want %d; msg=%q", profile, status, http.StatusUnprocessableEntity, msg)
		}
		if len(v.calls) != 1 {
			t.Errorf("profile %q: ingress validator called %d times, want exactly 1", profile, len(v.calls))
		}
	}
}
