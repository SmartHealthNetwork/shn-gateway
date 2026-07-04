// originate_homeoxygen_test.go — hermetic unit test for handleHomeOxygen, the
// provider-data order-dispatch origination handler.
//
// It mirrors the originate_test.go stubSubstrate harness (same crypto helpers:
// genKeyPair / genED25519 / signTestToken / sealForProvider) but drives THREE legs
// in sequence — crd-order-dispatch → dtr-questionnaire-fetch → pas-claim — because
// HomeOxygen runs the full chain (unlike the single-CRD-leg branch tests). The
// substrate is canned; the REAL operated $populate is exercised by the live
// two-RI gate, so here the Populator is a fake returning a canned QR.
//
// The honesty assertions are the point: the order code came from OpenOrder (not a
// literal), the supplier came from ResolveByReference of the order's performer (not a
// literal), and the leg originated is crd-order-DISPATCH (not order-select).
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

// ---- HomeOxygen fake SoR ----

// homeOxygenSoR wraps StubHolderData (for ResolvePatient/PatientFHIRRef) and adds the
// member's open order + supplier resolution the in-memory stub lacks. It also SPIES on
// OpenOrder / ResolveByReference so the test can assert the handler sourced the order
// code + supplier from the SoR (not literals).
//
// member is a field (not a literal) so this ONE fixture shape serves both MBR-OX
// (TestHandleHomeOxygen) and a second, differently-coded persona (D-S7K-5,
// originate_dispatch_test.go's TestHandleDispatch_ArbitraryMember, MBR-PD-UC03/E1390) —
// proving /scenario/dispatch is genuinely member-parameterized, not hardcoded to MBR-OX.
type homeOxygenSoR struct {
	*StubHolderData
	member       string
	demo         Demo
	pci          string
	orderJSON    []byte
	performerRef string
	supplierJSON []byte

	openOrderCalls   []string
	resolveByRefCall []string
}

// ResolvePatient / PatientFHIRRef are overridden to stand in for the PROVIDER-side real-FHIR-SoR
// facts the provider-data lane actually reads from (mirroring OpenOrder/ResolveByReference
// below, which the base stub can never serve — permanent provider-data-lane no-ops). This is no
// longer the ONLY way to resolve MBR-OX/MBR-PD-UC03 at all: commit 8408638 also added both to the
// engine's stubPersonas census (with the same demo values), so StubHolderData's own
// ResolvePatient/PatientFHIRRef now answer for them too — that addition was for the PAYER-side
// crd-order-dispatch inbound bind (conformantCRDDispatchBind), a separate concern from this
// fixture's provider-side origination path. The embedded StubHolderData supplies everything else.
func (s *homeOxygenSoR) ResolvePatient(memberID string) (string, Demo, bool) {
	if memberID != s.member {
		return s.StubHolderData.ResolvePatient(memberID)
	}
	return s.pci, s.demo, true
}

func (s *homeOxygenSoR) PatientFHIRRef(memberID string) (string, bool) {
	if memberID != s.member {
		return s.StubHolderData.PatientFHIRRef(memberID)
	}
	return "Patient/" + s.member, true
}

func (s *homeOxygenSoR) OpenOrder(memberID string) ([]byte, bool) {
	s.openOrderCalls = append(s.openOrderCalls, memberID)
	if memberID != s.member {
		return nil, false
	}
	return s.orderJSON, true
}

func (s *homeOxygenSoR) ResolveByReference(ref string) ([]byte, bool) {
	s.resolveByRefCall = append(s.resolveByRefCall, ref)
	if ref != s.performerRef {
		return nil, false
	}
	return s.supplierJSON, true
}

// OpenCoverage returns a parseable contained-payor Coverage for the fixture's member, the
// routing/identity SOURCE the origination handler now reads (FR-G40). Since 8408638 added
// MBR-OX/MBR-PD-UC03 to stubPersonas too, the embedded StubHolderData.OpenCoverage would in fact
// resolve the member and produce the same contained 00001 Coverage this override does — this is
// kept anyway for shape-symmetry with the OpenOrder/ResolveByReference provider-facts above (a
// single fixture pinning the exact Patient/Coverage reference shape origination reads) rather
// than relying on stub-census parity to keep holding.
func (s *homeOxygenSoR) OpenCoverage(memberID string) ([]byte, bool) {
	if memberID != s.member {
		return nil, false
	}
	cov, err := shnsdk.BuildCoverageWithPayer("Patient/"+s.member, "Coverage/"+s.member, shnsdk.CMSPayerIdentity)
	if err != nil {
		return nil, false
	}
	return cov, true
}

// fakePopulator returns a canned populated QR — the operated $populate stand-in for the
// hermetic test (the REAL CQL $populate is the live gate). It echoes back the
// requested subject + advertised canonical so the handler's QR-subject / QR-questionnaire
// fences pass.
type fakePopulator struct {
	canonical string
}

func (f fakePopulator) Populate(_ context.Context, _ []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	// Echo back the requested subject + advertised canonical (so the fences pass) AND nested
	// quantity answers for linkIds 2.2 (O₂-sat) / 2.3 (PaO₂) — the SAME shape the real operated
	// $populate returns — so the handler's QRAnswers surfacing (the crux evidence path) is
	// exercised hermetically. The values 86/54 are the computed answers verified live against br-payer a8bece4.
	qr := map[string]any{
		"resourceType":  "QuestionnaireResponse",
		"id":            "qr-homeoxygen",
		"status":        "completed",
		"subject":       map[string]string{"reference": pc.PatientRef},
		"questionnaire": f.canonical,
		// The operated HAPI CR engine emits valueDecimal for these quantity-type O₂ items (verified
		// live against the HomeOxygen questionnaire), so the fixture uses valueDecimal too.
		"item": []map[string]any{{
			"linkId": "2",
			"item": []map[string]any{
				{"linkId": "2.2", "answer": []map[string]any{{"valueDecimal": 86}}},
				{"linkId": "2.3", "answer": []map[string]any{{"valueDecimal": 54}}},
			},
		}},
	}
	b, _ := json.Marshal(qr)
	return b, nil, nil
}

// ---- multi-leg substrate ----

// homeOxygenSubstrate is the canned three-leg substrate for the HomeOxygen chain. It
// serves, in order: crd-order-dispatch (an ADVISORY card advertising the HomeOxygen
// questionnaire — conditional coverage, NOT auth-needed), dtr-questionnaire-fetch (a
// questionnaire-package), pas-claim (an A1 "approved" ClaimResponse). It records every
// leg's TransactionType so the test can assert WHICH CRD leg was originated.
type homeOxygenSubstrate struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	clock          func() time.Time
	pci            string
	canonical      string

	legTypes []string
}

func (s *homeOxygenSubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
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

func (s *homeOxygenSubstrate) handleAuthorize(body []byte) (*http.Response, error) {
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

func (s *homeOxygenSubstrate) handleRoute(body []byte) (*http.Response, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	corrID := env.Metadata.CorrelationID
	txType := env.Metadata.TransactionType
	s.legTypes = append(s.legTypes, txType)

	var respPayload []byte
	var respOp, respFrame string
	switch txType {
	case "crd-order-dispatch":
		// ADVISORY card: conditional coverage + NOT auth-needed (the order-dispatch card
		// does not require PA; its job is to advertise the HomeOxygen questionnaire). The
		// handler must gate on NeedsDTR, NOT PARequired.
		respPayload, err = shnsdk.BuildCards(shnsdk.CardCoverage{
			Covered:        shnsdk.CoveredConditional,
			PANeeded:       shnsdk.PANeededNoAuth,
			Questionnaires: []string{s.canonical},
		})
		if err != nil {
			return errResp("stub: BuildCards: " + err.Error()), nil
		}
		respOp, respFrame = "crd-dispatch-cards", "payer-coverage"
	case "dtr-questionnaire-fetch":
		pkg, perr := buildQuestionnairePackage(homeOxygenQuestionnaire(s.canonical))
		if perr != nil {
			return errResp("stub: package: " + perr.Error()), nil
		}
		respPayload = pkg
		respOp, respFrame = "dtr-questionnaire", "payer-coverage"
	case "pas-claim":
		respPayload = homeOxygenApprovedClaimResponse()
		respOp, respFrame = "pas-response", "payer-coverage"
	default:
		return errResp("stub: unexpected leg " + txType), nil
	}

	meta := shnsdk.Metadata{
		Sender:          "payer",
		Recipient:       "provider",
		TransactionType: txType,
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

// homeOxygenQuestionnaire builds a minimal Questionnaire whose url is the advertised
// canonical (so the handler's F5 fetched-canonical check passes).
func homeOxygenQuestionnaire(canonical string) []byte {
	q := map[string]any{
		"resourceType": "Questionnaire",
		"id":           "home-oxygen",
		"url":          canonical,
		"status":       "active",
		"item": []map[string]any{
			{"linkId": "spo2", "type": "decimal", "text": "Resting SpO2"},
		},
	}
	b, _ := json.Marshal(q)
	return b
}

// homeOxygenApprovedClaimResponse builds an A1-equivalent approved ClaimResponse —
// outcome "complete" + a preAuthRef — which ParseClaimResponse reads as approved.
func homeOxygenApprovedClaimResponse() []byte {
	cr := map[string]any{
		"resourceType": "ClaimResponse",
		"id":           "cr-homeoxygen",
		"status":       "active",
		"outcome":      "complete",
		"preAuthRef":   "AUTH-OX-001",
		"preAuthPeriod": map[string]string{
			"end": "2027-01-01",
		},
	}
	b, _ := json.Marshal(cr)
	return b
}

// ---- the test ----

// TestHandleHomeOxygen drives the full headless order-dispatch chain off provider data:
// it asserts the handler returns 200, originated crd-order-DISPATCH (not order-select),
// sourced the order from OpenOrder (the code is never a literal), and resolved the
// supplier via ResolveByReference of the order's performer.
func TestHandleHomeOxygen(t *testing.T) {
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	base := NewStubHolderData()
	// MBR-OX is a FHIR-store seed persona (internal/fhirseed) and, as of 8408638, also lives in
	// the engine's stubPersonas census (payer-side crd-order-dispatch bind) with the same demo
	// values — either source yields the same PCI. Derive it the same way the SoR does (member +
	// birthDate + familyName).
	pci := shnsdk.ResolvePCI("MBR-OX", "1958-07-14", "Okafor-Oxygen")

	// Build the seeded order + supplier the way fhirseed does: an E0431
	// DeviceRequest whose performer references the DME supplier Organization.
	const performerRef = "Organization/org-dme-ox"
	orderJSON, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", performerRef)
	if err != nil {
		t.Fatalf("build DeviceRequest: %v", err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatalf("build supplier: %v", err)
	}
	sor := &homeOxygenSoR{
		StubHolderData: base,
		member:         "MBR-OX",
		demo:           Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"},
		pci:            pci,
		orderJSON:      orderJSON,
		performerRef:   performerRef,
		supplierJSON:   supplierJSON,
	}

	const canonical = "http://smarthealth.network/fhir/Questionnaire/home-oxygen"
	stub := &homeOxygenSubstrate{
		authzPriv:      authzPriv,
		providerEncPub: provEncPub,
		clock:          clock,
		pci:            pci,
		canonical:      canonical,
	}

	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub})

	const fakeBase = "http://stub.test"
	gw := New(Config{
		Role:        "provider",
		HolderID:    "provider",
		PayerRouter: payerRouterFor(t, "payer"),
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: provSignPriv,
			EncPub:   provEncPub,
			EncPriv:  provEncPriv,
		},
		AuthzURL:           fakeBase,
		AuthzPub:           authzPub,
		HubTransportPub:    authzPub,
		HubURL:             fakeBase,
		Reg:                reg,
		Validator:          shnsdk.NewFakeValidator(),
		SoR:                sor,
		Store:              base,
		Clock:              clock,
		NPI:                "1234567890",
		OriginationProfile: "provider-data",
		Populator:          fakePopulator{canonical: canonical},
		Client:             &http.Client{Transport: stub},
	})

	req := httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", nil)
	rec := httptest.NewRecorder()
	gw.handleHomeOxygen(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Honesty 1: the order code came from OpenOrder (the SoR), not a literal.
	if len(sor.openOrderCalls) == 0 {
		t.Error("handler never called OpenOrder — the order code must be sourced from the SoR, not a literal")
	}
	for _, m := range sor.openOrderCalls {
		if m != "MBR-OX" {
			t.Errorf("OpenOrder called with %q, want MBR-OX", m)
		}
	}

	// Honesty 2: the supplier came from ResolveByReference of the order's performer.
	if len(sor.resolveByRefCall) == 0 {
		t.Error("handler never called ResolveByReference — the supplier must be resolved from the order's performer, not a literal")
	}
	sawPerformer := false
	for _, r := range sor.resolveByRefCall {
		if r == performerRef {
			sawPerformer = true
		}
	}
	if !sawPerformer {
		t.Errorf("ResolveByReference never called with the order's performer %q (calls: %v)", performerRef, sor.resolveByRefCall)
	}

	// Honesty 3: the originated CRD leg is order-DISPATCH (not order-select).
	if !legAttempted(stub.legTypes, "crd-order-dispatch") {
		t.Errorf("crd-order-dispatch leg was not originated (legs: %v)", stub.legTypes)
	}
	if legAttempted(stub.legTypes, "crd-order-select") {
		t.Errorf("originated crd-order-select — HomeOxygen must originate crd-order-DISPATCH (legs: %v)", stub.legTypes)
	}
	// The full chain ran through to PAS.
	if !legAttempted(stub.legTypes, "dtr-questionnaire-fetch") || !legAttempted(stub.legTypes, "pas-claim") {
		t.Errorf("chain did not reach DTR + PAS (legs: %v)", stub.legTypes)
	}

	// The verdict is the genuine A1-approved (conditional-coverage A4→A1 resolved upstream).
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["authNumber"] != "AUTH-OX-001" {
		t.Errorf("authNumber = %v, want AUTH-OX-001 (the approved verdict)", body["authNumber"])
	}

	// Crux evidence (C1): the operated-$populate computed O₂ answers (2.2/2.3) are surfaced in the
	// response. The handler reads them off the returned QR (the native populator drops FilledItem
	// attribution), proving downstream that the $populate ran against the seeded obs, not a book.
	qa, ok := body["qrAnswers"].(map[string]any)
	if !ok {
		t.Fatalf("qrAnswers missing/not an object in response: %s", rec.Body.String())
	}
	if qa["2.2"] != "86" || qa["2.3"] != "54" {
		t.Errorf("qrAnswers = %v, want 2.2=86 / 2.3=54 (the populated O₂ values)", qa)
	}
}

// buildHomeOxygenDeviceRequest mirrors fhirmap.BuildDeviceRequest's shape (E0431 +
// performer) without importing internal/fhirmap into the engine test — an inline JSON
// fixture matching the seeded order is sufficient for the hermetic chain.
func buildHomeOxygenDeviceRequest(id, patientRef, performerRef string) ([]byte, error) {
	dr := map[string]any{
		"resourceType": "DeviceRequest",
		"id":           id,
		"status":       "active",
		"intent":       "order",
		"subject":      map[string]string{"reference": patientRef},
		"performer":    map[string]string{"reference": performerRef},
		"codeCodeableConcept": map[string]any{
			"coding": []map[string]string{{
				"system":  "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets",
				"code":    "E0431",
				"display": "Portable gaseous oxygen system",
			}},
		},
		"reasonCode": []map[string]any{{
			"coding": []map[string]string{{
				"system": "http://hl7.org/fhir/sid/icd-10-cm",
				"code":   "J44.9",
			}},
		}},
	}
	return json.Marshal(dr)
}

func buildHomeOxygenSupplier(id string) ([]byte, error) {
	org := map[string]any{
		"resourceType": "Organization",
		"id":           id,
		"name":         "Acme Home Medical (Non-Contracted)",
		"identifier": []map[string]string{{
			"system": "http://hl7.org/fhir/sid/us-npi",
			"value":  "1922334455",
		}},
	}
	return json.Marshal(org)
}

// TestHandler_HomeOxygenRouteRegistered asserts that the provider-role Handler() mux
// routes POST /scenario/homeoxygen (returns something other than 404 / method-not-allowed).
// It does NOT drive the full chain — that is TestHomeOxygen — this is a registration probe
// that catches a missing mux.HandleFunc("POST /scenario/homeoxygen", ...) at build time.
func TestHandler_HomeOxygenRouteRegistered(t *testing.T) {
	_, signPriv := genED25519(t)
	encPub, encPriv := genKeyPair(t)
	stub := NewStubHolderData()
	gw := New(Config{
		Role:     "provider",
		HolderID: "provider",
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: signPriv,
			EncPub:   encPub,
			EncPriv:  encPriv,
		},
		SoR:       stub,
		Store:     stub,
		Validator: shnsdk.NewFakeValidator(),
	})
	h := gw.Handler()
	req := httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /scenario/homeoxygen not registered in provider Handler(): got %d", rec.Code)
	}
}
