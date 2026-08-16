// authoredqr_line_test.go — proves the selected pa.dtr line threads through
// the THREE authored-QR build sites (closes an earlier KNOWN GAP
// recorded at handleUC04's attestation refill (originate.go), scenarioToPend's
// UC-06 attestation refill (originate_resume.go), and populator.go's
// managedPopulator.Populate — the UC-03 silent wrong-line pass).
// TestManagedPopulatorBuildsAtLine (populator_test.go) proves the
// managedPopulator.Populate site directly at the unit level; THIS file proves
// the other two — handleUC04's and scenarioToPend's attestation refill — end
// to end against a live (fake-Hub) round trip, reading the ACTUAL wire bytes
// of the submitted PAS bundle (the same discipline
// test/conformance/per_line_uc_test.go's assertWireBuiltAtLines uses).
//
// WIRE-MARKER NOTE (load-bearing for why this fixture pins the 2.2 line):
// DTR 2.0 and 2.1 are BYTE-IDENTICAL on every marker this build can check —
// DTRLineDef("2.0") and DTRLineDef("2.1") agree on every field
// FillQuestionnaireFromAnswersAtLine/FillQuestionnaireAtLine read
// (SingleCoverageConstraint, AutoOriginSourceCode, IntendedUseCodeSystem — see
// sdk/linedef.go and per_line_uc_test.go:389-407's recorded vacuity). Only DTR
// 2.2 has an observable wire delta (the qr-coverage extension + the
// intendedUse code-system rename to coverage-information-codes), so a 2.0-only
// or 2.1-only fixture could pass even if the sites silently stayed on the 2.0
// builder. This fixture therefore declares ONLY the 2.2 line on both sides —
// it is the one line that can prove the selected line genuinely reached the
// build call, not merely that some valid QR came out.
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
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// authoredQRSoR is a minimal provider-data SoR: it embeds StubHolderData (for the
// members already in its census, e.g. MBR-UC06) and layers in per-fixture open
// orders + "extra" personas (demographics for members the base census doesn't
// carry, e.g. the scene-member-mapped MBR-PD-UC04) — the same
// embed-and-override shape as originate_homeoxygen_test.go's homeOxygenSoR.
type authoredQRSoR struct {
	*StubHolderData
	orders        map[string][]byte
	extraPersonas map[string]Demo
}

func (s *authoredQRSoR) ResolvePatient(memberID string) (string, Demo, bool) {
	if d, ok := s.extraPersonas[memberID]; ok {
		return shnsdk.ResolvePCI(memberID, d.BirthDate, d.FamilyName), d, true
	}
	return s.StubHolderData.ResolvePatient(memberID)
}

func (s *authoredQRSoR) PatientFHIRRef(memberID string) (string, bool) {
	if _, ok := s.extraPersonas[memberID]; ok {
		return "Patient/" + memberID, true
	}
	return s.StubHolderData.PatientFHIRRef(memberID)
}

// ClinicalContext: the provider-data attestation sites this fixture drives
// (handleUC04 / scenarioToPend's uc06 branch) re-fill from the seeded ORDER, not
// from ClinicalContext — but runCRDThenDTROrder's DTR block ALWAYS calls the
// managed Populator first (site 3, proven directly by
// TestManagedPopulatorBuildsAtLine), which needs a non-empty ClinicalContext to
// proceed at all. Reuse the MBR-COVERED canned clinical facts for any extra
// persona — its content is discarded by the provider-data attestation refill
// that follows, so only "found=true" matters here.
func (s *authoredQRSoR) ClinicalContext(memberID string) (shnsdk.ClinicalContext, bool) {
	if _, ok := s.extraPersonas[memberID]; ok {
		return s.StubHolderData.ClinicalContext("MBR-COVERED")
	}
	return s.StubHolderData.ClinicalContext(memberID)
}

func (s *authoredQRSoR) OpenCoverage(memberID string) ([]byte, bool) {
	if _, ok := s.extraPersonas[memberID]; ok {
		cov, err := shnsdk.BuildCoverageWithPayer("Patient/"+memberID, memberID, shnsdk.CMSPayerIdentity)
		if err != nil {
			return nil, false
		}
		return cov, true
	}
	return s.StubHolderData.OpenCoverage(memberID)
}

// OpenOrder: the provider-data lane's order source (orderSource reads this directly,
// ignoring the sandbox order tuple). found=false for any member not seeded here.
func (s *authoredQRSoR) OpenOrder(memberID string) ([]byte, bool) {
	o, ok := s.orders[memberID]
	return o, ok
}

// capturingTransport wraps a substrate RoundTripper and, for every /route call,
// independently decrypts + (if framed) unwraps the request payload using the SAME
// recipient keys the substrate holds — the exact openRequest logic
// pendResumeSubstrate uses internally — and records it by TransactionType. This
// is how the test reads the ACTUAL bytes of the submitted PAS bundle (the
// embedded QuestionnaireResponse and all) without needing the substrate itself
// to know anything about this test's assertions.
type capturingTransport struct {
	inner   http.RoundTripper
	encKeys map[string]encPair

	mu       sync.Mutex
	captured map[string][][]byte
}

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/route") {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			if env, derr := shnsdk.DecodeEnvelope(body); derr == nil {
				if keys, ok := c.encKeys[env.Metadata.Recipient]; ok {
					if payload, perr := shnsdk.Open(env, keys.pub, keys.priv); perr == nil {
						p := payload
						if shnsdk.IsFramed(p) {
							if _, b, ferr := shnsdk.DecodeHTTPFrame(p); ferr == nil {
								p = b
							}
						}
						c.mu.Lock()
						c.captured[env.Metadata.TransactionType] = append(c.captured[env.Metadata.TransactionType], p)
						c.mu.Unlock()
					}
				}
			}
		}
	}
	return c.inner.RoundTrip(req)
}

func (c *capturingTransport) get(legType string) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured[legType]
}

// buildAuthoredQROrderWithID is BuildServiceRequestCoded plus a server-assigned id —
// both handleUC04 and scenarioToPend's provider-data branch persist/attest against the
// order's OWN id (resourceRef(res.srJSON), Bug-2 discipline), which the plain builder
// (used by the sandbox lane, where the order is never re-referenced this way) omits.
func buildAuthoredQROrderWithID(t *testing.T, id, patientRef, code, display, dxCode string) []byte {
	t.Helper()
	raw, err := BuildServiceRequestCoded(systemCPTBuild, code, display, dxCode, patientRef)
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal built order: %v", err)
	}
	m["id"] = id
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("remarshal order with id: %v", err)
	}
	return out
}

// authoredQRKeys is the one set of crypto material a fixture needs (provider signing +
// encryption keypair, authz signer, payer encryption keypair) — generated once and
// shared by both fixture constructors below so their setup does not fork.
type authoredQRKeys struct {
	authzPub  ed25519.PublicKey
	authzPriv ed25519.PrivateKey

	provEncPub, provEncPriv *[32]byte
	provSignPriv            ed25519.PrivateKey

	payerEncPub, payerEncPriv *[32]byte
	payerSignPub              ed25519.PublicKey
}

func genAuthoredQRKeys(t *testing.T) authoredQRKeys {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, payerEncPriv := genKeyPair(t)
	payerSignPub, _ := genED25519(t)
	return authoredQRKeys{
		authzPub: authzPub, authzPriv: authzPriv,
		provEncPub: provEncPub, provEncPriv: provEncPriv, provSignPriv: provSignPriv,
		payerEncPub: payerEncPub, payerEncPriv: payerEncPriv, payerSignPub: payerSignPub,
	}
}

// declaredLine22 is declared on BOTH sides of every fixture in this file — the
// single-overlap pin discipline TestCompletePatient_HonorsPinAcrossDifferentLineDrift
// (pendstate_pin_test.go) already establishes for a single line, applied here to
// force DTR-2.2 selection (see the file-header wire-marker note).
func declaredLine22() []string {
	return []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
}

func newAuthoredQRRegistry(keys authoredQRKeys) shnsdk.Registry {
	declared := declaredLine22()
	reg := shnsdk.NewRegistry()
	requestFrames := shnsdk.SupportedRequestFrames()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: keys.provEncPub, SignPub: keys.authzPub,
		RequestFrames: requestFrames, ContractVersions: declared})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: keys.payerEncPub, SignPub: keys.payerSignPub,
		RequestFrames: requestFrames, ContractVersions: declared})
	return reg
}

// newAuthoredQRGateway builds the real provider-data Gateway every fixture in this
// file drives — sor/transport/clock are the only per-scenario knobs.
func newAuthoredQRGateway(t *testing.T, keys authoredQRKeys, sor SystemOfRecord, transport http.RoundTripper, clock func() time.Time) *Gateway {
	t.Helper()
	return New(Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: keys.provSignPriv,
			EncPub:   keys.provEncPub,
			EncPriv:  keys.provEncPriv,
		},
		AuthzURL:                 "http://stub.test",
		AuthzPub:                 keys.authzPub,
		HubTransportPub:          keys.authzPub,
		HubURL:                   "http://stub.test",
		Reg:                      newAuthoredQRRegistry(keys),
		DeclaredContractVersions: declaredLine22(),
		Validator:                shnsdk.NewFakeValidator(),
		SoR:                      sor,
		Store:                    NewStubHolderData(),
		Clock:                    clock,
		NPI:                      "1234567890",
		OriginationProfile:       "provider-data",
		Client:                   &http.Client{Transport: transport},
	})
}

func newAuthoredQRSoR(member string, demo Demo, orderJSON []byte) *authoredQRSoR {
	return &authoredQRSoR{
		StubHolderData: NewStubHolderData(),
		orders:         map[string][]byte{member: orderJSON},
		extraPersonas:  map[string]Demo{member: demo},
	}
}

// newAuthoredQRPendFixture wires scenarioToPend's UC-06 case: pas-claim answers
// PENDED (pendResumeSubstrate's shape, reused verbatim from pendstate_pin_test.go).
func newAuthoredQRPendFixture(t *testing.T, member string, demo Demo, orderJSON []byte, pendedItem string) (*Gateway, *pendResumeSubstrate, *capturingTransport) {
	t.Helper()
	keys := genAuthoredQRKeys(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	pci := shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName)

	stub := &pendResumeSubstrate{
		authzPriv:      keys.authzPriv,
		providerEncPub: keys.provEncPub,
		clock:          clock,
		pci:            pci,
		patientRef:     "Patient/" + member,
		pendedItem:     pendedItem,
		encKeys:        map[string]encPair{"payer": {pub: keys.payerEncPub, priv: keys.payerEncPriv}},
		claimed:        map[string][]string{},
	}
	capture := &capturingTransport{inner: stub, encKeys: stub.encKeys, captured: map[string][][]byte{}}
	gw := newAuthoredQRGateway(t, keys, newAuthoredQRSoR(member, demo, orderJSON), capture, clock)
	return gw, stub, capture
}

// newAuthoredQRSingleShotFixture wires handleUC04's provider-data lean single-shot
// tail (D-PD-1: no amendment) — pas-claim answers a DIRECT APPROVAL, never PENDED.
func newAuthoredQRSingleShotFixture(t *testing.T, member string, demo Demo, orderJSON []byte) (*Gateway, *uc04SingleShotSubstrate, *capturingTransport) {
	t.Helper()
	keys := genAuthoredQRKeys(t)
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	pci := shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName)

	stub := &uc04SingleShotSubstrate{
		authzPriv:      keys.authzPriv,
		providerEncPub: keys.provEncPub,
		clock:          clock,
		pci:            pci,
		encKeys:        map[string]encPair{"payer": {pub: keys.payerEncPub, priv: keys.payerEncPriv}},
	}
	capture := &capturingTransport{inner: stub, encKeys: stub.encKeys, captured: map[string][][]byte{}}
	gw := newAuthoredQRGateway(t, keys, newAuthoredQRSoR(member, demo, orderJSON), capture, clock)
	return gw, stub, capture
}

// uc04SingleShotSubstrate is the canned substrate for handleUC04's provider-data lean
// single-shot tail: crd-order-select → dtr-questionnaire-fetch → pas-claim, with
// pas-claim answering a DIRECT APPROVAL (homeOxygenApprovedClaimResponse, reused
// from originate_homeoxygen_test.go) — handleUC04's provider-data branch never
// pends, unlike pendResumeSubstrate's shape (used by the scenarioToPend/UC-06
// fixture above).
type uc04SingleShotSubstrate struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	clock          func() time.Time
	pci            string
	encKeys        map[string]encPair

	legTypes []string
}

func (s *uc04SingleShotSubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
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

func (s *uc04SingleShotSubstrate) handleAuthorize(body []byte) (*http.Response, error) {
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

func (s *uc04SingleShotSubstrate) handleRoute(body []byte) (*http.Response, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	corrID := env.Metadata.CorrelationID
	txType := env.Metadata.TransactionType
	s.legTypes = append(s.legTypes, txType)

	if keys, ok := s.encKeys[env.Metadata.Recipient]; ok {
		if _, perr := shnsdk.Open(env, keys.pub, keys.priv); perr != nil {
			return errResp("stub: open " + txType + " request: " + perr.Error()), nil
		}
	}

	var respPayload []byte
	respOp, respFrame := "pas-response", "payer-coverage"
	switch txType {
	case "crd-order-select":
		respPayload, err = shnsdk.BuildCards(shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded,
			Questionnaires: []string{shnsdk.QuestionnaireCanonicalLumbarMRI}})
		respOp, respFrame = "crd-cards", "payer-coverage"
	case "dtr-questionnaire-fetch":
		respPayload, err = buildQuestionnairePackage(shnsdk.SandboxLumbarQuestionnaire())
		respOp, respFrame = "dtr-questionnaire", "payer-coverage"
	case "pas-claim":
		respPayload = homeOxygenApprovedClaimResponse()
	default:
		return errResp("stub: unexpected leg " + txType), nil
	}
	if err != nil {
		return errResp("stub: build " + txType + " response: " + err.Error()), nil
	}

	meta := shnsdk.Metadata{
		Sender:          env.Metadata.Recipient,
		Recipient:       "provider",
		TransactionType: txType,
		AuthorityFrame:  respFrame,
		Timestamp:       s.clock().UTC().Format(time.RFC3339),
		CorrelationID:   corrID,
	}
	out, err := sealForProvider(meta, respPayload, s.providerEncPub, s.authzPriv,
		corrID, respOp, respFrame, env.Metadata.Recipient, s.pci, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(out)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// assert22WireMarkers asserts the two DTR-2.2 markers (see the file-header note)
// are present in payload — the embedded QuestionnaireResponse was built via the
// AtLine sibling at the SELECTED line, not the frozen 2.0 default.
func assert22WireMarkers(t *testing.T, label string, payload []byte) {
	t.Helper()
	def, ok := shnsdk.DTRLineDef("2.2")
	if !ok {
		t.Fatal("no DTRLineDef for 2.2")
	}
	body := string(payload)
	if !strings.Contains(body, def.IntendedUseCodeSystem) {
		t.Errorf("%s: submitted bundle has no intendedUse code drawn from %q (DTR line 2.2) — "+
			"the embedded QuestionnaireResponse was NOT built at the selected DTR line:\n%s",
			label, def.IntendedUseCodeSystem, body)
	}
	if !strings.Contains(body, "/StructureDefinition/qr-coverage") {
		t.Errorf("%s: submitted bundle has no qr-coverage extension (DTR line 2.2) — "+
			"the embedded QuestionnaireResponse was NOT built at the selected DTR line:\n%s", label, body)
	}
}

// TestAuthoredQRBuiltAtSelectedLine drives the two provider-data attestation
// sites — scenarioToPend's UC-06 refill (originate_resume.go) and handleUC04's
// refill (originate.go) — each against its own 2.2-pinned fixture, and asserts
// the QuestionnaireResponse embedded in the ACTUAL submitted pas-claim /
// pas-claim-update bundle carries the 2.2 wire markers. Before the fix both
// sites called the frozen (line-invariant) FillQuestionnaireFromAnswers, so
// this failed red — the submitted QR was byte-identical regardless of the
// selected line (the UC-03 silent-wrong-line-pass this task closes).
func TestAuthoredQRBuiltAtSelectedLine(t *testing.T) {
	t.Run("scenarioToPend (UC-06)", func(t *testing.T) {
		const member = "MBR-UC06"
		demo := Demo{BirthDate: "1969-07-21", FamilyName: "Reyes"} // matches stubPersonas' MBR-UC06 exactly
		orderJSON := buildAuthoredQROrderWithID(t, "sr-uc06-authoredqr", "Patient/"+member, "72148", "MRI lumbar spine w/o contrast", "M51.16")
		gw, stub, capture := newAuthoredQRPendFixture(t, member, demo, orderJSON, "functional-status")

		req := httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil)
		rec := httptest.NewRecorder()
		if _, ok := gw.scenarioToPend(rec, req, "uc06", member); !ok {
			t.Fatalf("scenarioToPend failed: %d %s", rec.Code, rec.Body.String())
		}
		if !legAttempted(stub.legTypes, "pas-claim") {
			t.Fatalf("pas-claim leg never ran (legs: %v)", stub.legTypes)
		}
		payloads := capture.get("pas-claim")
		if len(payloads) == 0 {
			t.Fatal("no pas-claim request payload captured")
		}
		assert22WireMarkers(t, "scenarioToPend/pas-claim", payloads[len(payloads)-1])
	})

	t.Run("handleUC04", func(t *testing.T) {
		// handleUC04's provider-data branch resolves the scene member via
		// scenarioMember(w, r, "MBR-UC04", "MBR-PD-UC04") — under provider-data that is
		// the SECOND (provider-data) name, which is NOT in stubPersonas' base census, so
		// this member is seeded purely via extraPersonas (newAuthoredQRSoR).
		const member = "MBR-PD-UC04"
		demo := Demo{BirthDate: "1982-11-03", FamilyName: "Chen-ProviderData"}
		orderJSON := buildAuthoredQROrderWithID(t, "sr-uc04-authoredqr", "Patient/"+member, "72148", "MRI lumbar spine w/o contrast", "M51.16")
		gw, stub, capture := newAuthoredQRSingleShotFixture(t, member, demo, orderJSON)

		req := httptest.NewRequest(http.MethodPost, "/scenario/uc04", nil)
		rec := httptest.NewRecorder()
		gw.handleUC04(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("handleUC04 status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !legAttempted(stub.legTypes, "pas-claim") {
			t.Fatalf("pas-claim leg never ran (legs: %v)", stub.legTypes)
		}
		payloads := capture.get("pas-claim")
		if len(payloads) == 0 {
			t.Fatal("no pas-claim request payload captured")
		}
		assert22WireMarkers(t, "handleUC04/pas-claim", payloads[len(payloads)-1])
	})
}

// TestRunCRDThenDTROrder_UnknownLineFailsAtAuthoredQRSite is the rejection row
// (Step 1c): an unknown selected DTR line must fail the build closed, asserted
// at the ONE site easiest to drive directly (the managedPopulator seam plus the
// exact status mapping runCRDThenDTROrder's Populate call site uses,
// statusForPopulateErr — originate.go:669) — driving the full HTTP surface
// through a bogus REGISTRY-declared line would require corrupting shnsdk's own
// line grammar, which is disproportionate here. All three authored-QR sites
// share this fail-closed behavior verbatim (Step 3: none special-cases an
// unknown line), so proving it once at the seam covers all three.
func TestRunCRDThenDTROrder_UnknownLineFailsAtAuthoredQRSite(t *testing.T) {
	sor := NewStubHolderData()
	mp := newManagedPopulator(sor)
	pkg := wrapSandboxPackage(t)
	_, _, err := mp.Populate(context.Background(), pkg, PopulateContext{
		Member: "MBR-COVERED", PatientRef: "Patient/MBR-COVERED", CoverageRef: "Coverage/MBR-COVERED",
		OrderRef: "ServiceRequest/sr-MBR-COVERED", Authored: time.Unix(1700000000, 0).UTC(), Line: "9.9",
	})
	if err == nil {
		t.Fatal("Populate at unknown line 9.9: want error (fail-closed), got nil")
	}
	// The exact mapping the Populate call site (runCRDThenDTROrder, originate.go:669)
	// applies to ANY non-sentinel Populate error: this build's own fault → 500. An
	// unknown-line error is not one of the sentinel data/partner faults
	// (errNoClinicalContext / errPopulateUpstream / errPopulateForeignSubject), so it
	// falls through to the 500 default — never a silent pass.
	if got := statusForPopulateErr(err); got != http.StatusInternalServerError {
		t.Fatalf("statusForPopulateErr(unknown-line error) = %d, want %d (500)", got, http.StatusInternalServerError)
	}
}
