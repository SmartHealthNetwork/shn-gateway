// pendstate_pin_test.go — behavioral proof that the pended-line pin
// actually threads through REAL scenarios, not just a
// hand-built pendState. TestPendStatePinsContractLine (versionroute_test.go)
// proves the pendState CARRIES the pin across store/load; this file proves the
// four production pin sites SELECT it once and HONOR it verbatim on the resume
// leg, each against a live (fake-Hub) pend→resume round trip with a registry
// drift in between that a dropped pin (or a re-selection) would not survive:
//
//	completeClinician  (UC-06, two-phase resume)   — drift to an INCOMPATIBLE line
//	completePatient    (UC-07, two-phase resume)   — drift to a DIFFERENT line
//	handleUC04         (in-request pend→update)    — MID-REQUEST drift
//	handleUC05         (in-request pend→update)    — MID-REQUEST drift + federation
//
// These round trips replaced the lexical source-guards that used to stand in for
// the last three: they grepped originate.go/originate_resume.go for the literal
// `ProfileID: st.pasToken` / `ProfileID: pasToken` references, which caught a
// deleted wiring line but could never catch a pin that was passed and then
// ignored. What survives from those guards is the SELECT-BEFORE-BUILD
// ORDERING assertion (assertBuildsAfterLineSelection), which is a genuinely
// structural property no round trip can observe: both bundle builds must follow
// the selection in SOURCE ORDER, or bytes get built at one line while the leg
// routes another.
//
// Modeled directly on homeOxygenSubstrate/newInProcessExchange's per-leg-type
// canned fake Hub (originate_homeoxygen_test.go / relay_roundtrip_test.go) —
// same shape, generalized to every leg a pend→resume chain needs across the four
// scenarios (crd-order-select, dtr-questionnaire-fetch, pas-claim PENDED,
// patient-dtr, federated-query, pas-claim-update approved).
package engine

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// encPair is one holder's X25519 keypair as the fake substrate holds it. The
// stub needs the PRIVATE half so it can OPEN the sealed request: two answers are
// request-derived (federated-query echoes the CDex Task it was asked), and the
// request-frame assertions read the contract-version the REQUEST FRAME claimed, which lives
// inside the ciphertext.
type encPair struct {
	pub  *[32]byte
	priv *[32]byte
}

// pendResumeSubstrate is the fake Hub for a pend→resume chain. It answers each
// leg type with a CANNED response (the substrate never runs real adjudication;
// the verdict is canned here, same discipline as homeOxygenSubstrate's canned
// approve) and records, per leg, both the TransactionType and the contract
// version the request frame CLAIMED — the two things the pin tests assert on.
type pendResumeSubstrate struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	clock          func() time.Time
	pci            string
	// patientRef is the member this run drives ("Patient/MBR-UC0X"); the canned
	// pended response and the canned patient-attested item are both about them.
	patientRef string
	// pendedItem is the supplemental item the canned PENDED response asks for.
	pendedItem string
	// encKeys maps a recipient holder id to the keypair the stub stands in for, so
	// it can open that holder's sealed requests.
	encKeys map[string]encPair
	// onLeg, when set, runs BEFORE the leg is answered. It is the MID-REQUEST DRIFT
	// seam: handleUC04/handleUC05 never return to the test between their pas-claim
	// and pas-claim-update legs, so the only place to drift the registry underneath
	// them is here, from inside the pas-claim answer.
	onLeg func(legType string)

	legTypes []string
	// claimed records, per legType, the contract version each request frame
	// declared ("" when the request crossed bare).
	claimed map[string][]string
}

func (s *pendResumeSubstrate) RoundTrip(req *http.Request) (*http.Response, error) {
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

func (s *pendResumeSubstrate) handleAuthorize(body []byte) (*http.Response, error) {
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

// openRequest opens the sealed request for the recipient the stub stands in for
// and, when the payload is a request frame, strips the frame and records the
// contract version it claimed. Returns the bare application payload.
func (s *pendResumeSubstrate) openRequest(env shnsdk.Envelope, txType string) ([]byte, error) {
	keys, ok := s.encKeys[env.Metadata.Recipient]
	if !ok {
		return nil, nil // no key for this recipient: the answer is request-independent
	}
	payload, err := shnsdk.Open(env, keys.pub, keys.priv)
	if err != nil {
		return nil, err
	}
	claimed := ""
	if shnsdk.IsFramed(payload) {
		hdr, body, ferr := shnsdk.DecodeHTTPFrame(payload)
		if ferr != nil {
			return nil, ferr
		}
		claimed, payload = hdr.Headers[shnsdk.FrameHeaderContractVersion], body
	}
	s.claimed[txType] = append(s.claimed[txType], claimed)
	return payload, nil
}

func (s *pendResumeSubstrate) handleRoute(body []byte) (*http.Response, error) {
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("stub: decode envelope: " + err.Error()), nil
	}
	corrID := env.Metadata.CorrelationID
	txType := env.Metadata.TransactionType
	s.legTypes = append(s.legTypes, txType)

	reqPayload, err := s.openRequest(env, txType)
	if err != nil {
		return errResp("stub: open " + txType + " request: " + err.Error()), nil
	}
	if s.onLeg != nil {
		s.onLeg(txType)
	}

	var respPayload []byte
	// sender is the holder the stub answers AS; it must match the leg's recipient
	// or the provider's VerifyBound (C2/AI-11) rejects the answer.
	sender := env.Metadata.Recipient
	respOp, respFrame := "pas-response", "payer-coverage"
	switch txType {
	case "crd-order-select":
		cov := shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededAuthNeeded,
			Questionnaires: []string{shnsdk.QuestionnaireCanonicalLumbarMRI}}
		respPayload, err = shnsdk.BuildCards(cov)
		respOp, respFrame = "crd-cards", "payer-coverage"
	case "dtr-questionnaire-fetch":
		respPayload, err = buildQuestionnairePackage(shnsdk.SandboxLumbarQuestionnaire())
		respOp, respFrame = "dtr-questionnaire", "payer-coverage"
	case "pas-claim":
		respPayload, err = shnsdk.BuildPendedResponse(s.patientRef, "corr-pend", []string{s.pendedItem}, s.clock())
	case "patient-dtr":
		// The Trust-operated PHG's answer (UC-07): the patient-authored, signature-
		// attested item. RespOp/RespFrame are the patient-authorship pair from
		// paCatalog's "patient-dtr" row — DISTINCT from the payer-coverage pair, and
		// load-bearing: VerifyBound pins spec.RespOp per TransactionType (C2/AI-11).
		respPayload, err = s.patientDTRAnswer()
		respOp, respFrame = "patient-dtr-response", "patient-authorship"
	case "federated-query":
		// metro-spine's consent-gated disclosure (UC-05): the named records plus the
		// source Provenance, wrapped as a CDex query result. Request-derived — the
		// result echoes the CDex Task it answers — so it needs the opened request.
		respPayload, err = s.federatedQueryAnswer(reqPayload)
		respOp, respFrame = "federated-query-response", "facility-disclosure"
	case "pas-claim-update":
		// RespOp is DISTINCT from pas-claim's (workstream_pa.go's paCatalog: "pas-update-response",
		// not "pas-response") — VerifyBound pins spec.RespOp per TransactionType (C2/AI-11), so
		// reusing pas-claim's default here would authz-fail this leg, not silently mis-route it.
		respOp = "pas-update-response"
		respPayload = homeOxygenApprovedClaimResponse() // "outcome":"complete" + preAuthRef → ParseClaimResponse reads "approved"
	default:
		return errResp("stub: unexpected leg " + txType), nil
	}
	if err != nil {
		return errResp("stub: build " + txType + " response: " + err.Error()), nil
	}

	meta := shnsdk.Metadata{
		Sender:          sender,
		Recipient:       "provider",
		TransactionType: txType,
		AuthorityFrame:  respFrame,
		Timestamp:       s.clock().UTC().Format(time.RFC3339),
		CorrelationID:   corrID,
		ConsentRef:      env.Metadata.ConsentRef,
	}
	out, err := sealForProvider(meta, respPayload, s.providerEncPub, s.authzPriv,
		corrID, respOp, respFrame, sender, s.pci, s.clock())
	if err != nil {
		return errResp("stub: sealForProvider: " + err.Error()), nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(out)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// patientDTRAnswer is the canned PHG response: the patient-authored functional-
// status item, attested with the patient as signer (FR-18/27). Deterministic —
// the score is the harness/demo default completePatient sends.
func (s *pendResumeSubstrate) patientDTRAnswer() ([]byte, error) {
	attested, err := shnsdk.BuildPatientAttestedItem(oswestryLinkID, "42", s.patientRef,
		s.clock().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	return json.Marshal(patientDTRResponse{AttestedItem: attested})
}

// federatedQueryAnswer is the canned metro-spine disclosure: the named operative
// DiagnosticReport plus a source Provenance citing the consent, wrapped as the
// CDex query result the provider's ExtractCDexEvidence consumes. The
// DocumentReference type-leg gets the same shape (the provider only extracts
// evidence from the DiagnosticReport leg — see handleUC05's loop comment).
func (s *pendResumeSubstrate) federatedQueryAnswer(requestTaskJSON []byte) ([]byte, error) {
	drJSON, err := shnsdk.BuildDiagnosticReport("dr-uc05-operative", s.patientRef, "72148",
		"MRI lumbar spine w/o contrast")
	if err != nil {
		return nil, err
	}
	provJSON, err := shnsdk.BuildProvenanceWithPolicy("DiagnosticReport/dr-uc05-operative",
		"Organization/metro-spine", "Consent/consent-linda-treat", shnsdk.PurposeTreatment, s.clock())
	if err != nil {
		return nil, err
	}
	inner, err := shnsdk.BuildRecordsBundle([][]byte{drJSON, provJSON})
	if err != nil {
		return nil, err
	}
	return shnsdk.BuildCDexQueryResult(requestTaskJSON, inner)
}

// claimedFor returns the contract versions the request frames claimed for legType.
func (s *pendResumeSubstrate) claimedFor(legType string) []string { return s.claimed[legType] }

// pendFixtureOpts configures newPendResumeFixture per scenario.
type pendFixtureOpts struct {
	// member is the persona the scenario drives; its demographics must match
	// holderdata.go's census exactly (that is what ResolvePCI keys on).
	member     string
	birthDate  string
	familyName string
	pendedItem string
	// extraRoles registers additional holders the scenario's legs target by ROLE:
	// "phg" for UC-07's patient-dtr leg, "facility" for UC-05's federated-query leg.
	extraRoles map[string]string // role -> holder id
	// declared is this gateway's OWN declared contract-version set and the payer
	// holder's declaration. nil ⇒ both stay at the build default / silent (the
	// pa.pas@2.0 own-line case).
	declared []string
}

// newPendResumeFixture wires a real provider Gateway (sandbox lane —
// OriginationProfile unset, the member resolved off NewStubHolderData's built-in
// census, same as every other hermetic sandbox test) against pendResumeSubstrate.
// Every registry entry advertises requestFrames v1, exactly as internal/provision
// stamps it in production, so REQUEST legs carry the contract-version claim the
// pin tests read.
func newPendResumeFixture(t *testing.T, opts pendFixtureOpts) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, payerEncPriv := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	base := NewStubHolderData()
	pci := shnsdk.ResolvePCI(opts.member, opts.birthDate, opts.familyName) // must match holderdata.go's census

	stub := &pendResumeSubstrate{
		authzPriv:      authzPriv,
		providerEncPub: provEncPub,
		clock:          clock,
		pci:            pci,
		patientRef:     "Patient/" + opts.member,
		pendedItem:     opts.pendedItem,
		encKeys:        map[string]encPair{"payer": {pub: payerEncPub, priv: payerEncPriv}},
		claimed:        map[string][]string{},
	}

	reg := shnsdk.NewRegistry()
	requestFrames := shnsdk.SupportedRequestFrames()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub,
		RequestFrames: requestFrames, ContractVersions: opts.declared})
	// The payer declares opts.declared; nil ⇒ a SILENT peer, which selects this
	// build's own canonical line (pa.pas@2.0).
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub,
		RequestFrames: requestFrames, ContractVersions: opts.declared})
	for role, id := range opts.extraRoles {
		encPub, encPriv := genKeyPair(t)
		signPub, _ := genED25519(t)
		reg.Set(id, shnsdk.RegistryEntry{ID: id, Role: role, EncPub: encPub, SignPub: signPub,
			RequestFrames: requestFrames})
		stub.encKeys[id] = encPair{pub: encPub, priv: encPriv}
	}

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
		AuthzURL:                 "http://stub.test",
		AuthzPub:                 authzPub,
		HubTransportPub:          authzPub,
		HubURL:                   "http://stub.test",
		Reg:                      reg,
		DeclaredContractVersions: opts.declared,
		Validator:                shnsdk.NewFakeValidator(),
		SoR:                      base,
		Store:                    base,
		Clock:                    clock,
		NPI:                      "1234567890",
		Populator:                fakePopulator{canonical: shnsdk.QuestionnaireCanonicalLumbarMRI},
		Client:                   &http.Client{Transport: stub},
	})
	return gw, stub
}

// uc06Fixture is the original UC-06 wiring (silent peer ⇒ own-line pa.pas@2.0).
func uc06Fixture(t *testing.T) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	return newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes",
		pendedItem: "functional-status",
	})
}

// driftRegistryTo rewrites holder's declared contract-version set in gw's live
// registry — the mid-pend/mid-request drift every fixture below runs.
func driftRegistryTo(t *testing.T, gw *Gateway, holder string, tokens []string) {
	t.Helper()
	entry, ok := gw.cfg.Reg.Lookup(holder)
	if !ok {
		t.Fatalf("holder %q disappeared from the registry", holder)
	}
	entry.ContractVersions = tokens
	gw.cfg.Reg.Set(holder, entry)
}

// assertFreshSelectionRefuses proves a drift is REAL: an unpinned (fresh)
// selection for legType now fails. Without it, a pin test proves nothing —
// re-selection would have succeeded anyway.
func assertFreshSelectionRefuses(t *testing.T, gw *Gateway, holder, legType string) {
	t.Helper()
	if tok, err := gw.selectLegToken(holder, legType); err == nil {
		t.Fatalf("fresh selectLegToken(%s, %s) = %q with no error post-drift — the drift setup is broken, "+
			"so honoring the pin proves nothing", holder, legType, tok)
	}
}

// assertClaimedVersions asserts the REQUEST FRAMES for legType claimed exactly
// want — the wire-level evidence that the pinned token (not a re-selection) is
// what the resume leg actually routed under.
func assertClaimedVersions(t *testing.T, stub *pendResumeSubstrate, legType string, want ...string) {
	t.Helper()
	got := stub.claimedFor(legType)
	if len(got) != len(want) {
		t.Fatalf("%s: %d request frame(s) claimed %v, want %d claiming %v", legType, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s request %d claimed contract version %q, want %q", legType, i, got[i], want[i])
		}
	}
}

// TestScenarioToPendSelectsAndCompleteClinicianHonorsPin is the
// wiring proof: a REAL scenarioToPend (not a hand-built pendState) selects
// pasToken and the returned pendState carries it; completeClinician's
// pas-claim-update leg then survives a MID-PEND registry drift to an
// incompatible declared line — which a fresh selectLegToken call (proven right
// alongside it) now refuses. If the pin were dropped anywhere on this path
// (gateway.go's pendState.pasToken field, scenarioToPend's selection/storage, or
// completeClinician's Content.ProfileID), OriginateLeg would re-select on the
// resume leg, hit the drifted registry, and this test would fail closed with a
// RouteRefusalError surfaced as a 502 — not a silent pass.
func TestScenarioToPendSelectsAndCompleteClinicianHonorsPin(t *testing.T) {
	gw, stub := uc06Fixture(t)

	// --- run to PENDED ---
	req1 := httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil)
	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, req1, "uc06", "MBR-UC06")
	if !ok {
		t.Fatalf("scenarioToPend failed: %d %s", rec1.Code, rec1.Body.String())
	}
	if !legAttempted(stub.legTypes, "pas-claim") {
		t.Fatalf("pas-claim leg never ran (legs: %v)", stub.legTypes)
	}
	if st.pasToken != "pa.pas@2.0" {
		t.Fatalf("pendState.pasToken = %q, want pa.pas@2.0 (own SupportedContractVersions line, silent peer)", st.pasToken)
	}
	if st.recipient != "payer" {
		t.Fatalf("pendState.recipient = %q, want payer", st.recipient)
	}

	// --- mid-pend drift: the payer's declared line no longer overlaps this build's. ---
	driftRegistryTo(t, gw, "payer", []string{"pa.pas@9.9"})
	assertFreshSelectionRefuses(t, gw, "payer", "pas-claim")

	// --- resume: completeClinician's pas-claim-update leg must still succeed,
	// honoring st.pasToken verbatim (no re-selection). ---
	req2 := httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", nil)
	rec2 := httptest.NewRecorder()
	if ok := gw.completeClinician(rec2, req2, st, "", ""); !ok {
		t.Fatalf("completeClinician failed after mid-pend registry drift — the pin was not honored (a dropped/empty "+
			"pasToken re-selects and hits the drifted registry): %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("completeClinician status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if !legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatalf("pas-claim-update leg never ran (legs: %v)", stub.legTypes)
	}
	// The wire says so too: the resume request FRAME claimed the pinned token.
	assertClaimedVersions(t, stub, "pas-claim-update", "pa.pas@2.0")
	var resp struct {
		AuthNumber string `json:"authNumber"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil || resp.AuthNumber == "" {
		t.Fatalf("resume response missing authNumber: %v (%s)", err, rec2.Body.String())
	}
}

// TestCompletePatient_HonorsPinAcrossDifferentLineDrift is completeClinician's
// structural twin (UC-07's resume, a separate function) AND the request-frame
// DIVERGENCE row: the drift here is not to a bogus, unroutable line but to a
// genuinely DIFFERENT, perfectly routable one.
//
// Both sides start declaring the 2.0 line, so the pend pins pa.pas@2.0. Mid-pend,
// both sides GROW to {2.0, 2.2} (a grow-only declaration change — the
// realistic fleet event). A fresh selection would now pick pa.pas@2.2, and this
// test asserts that divergence explicitly. The resume leg must nevertheless carry
// pa.pas@2.0 on the wire: the amendment answers a pend that was adjudicated at
// 2.0, so re-negotiating it to 2.2 would silently change the contract the payer
// is holding open. That is the failure a bogus-line drift can never surface —
// re-selecting to 2.2 succeeds, so only reading the CLAIMED VERSION off the
// request frame catches it.
//
// The RESPONDER half of request framing (honoring a claimed token rather than recomputing its
// own, and stamping the honored line on the answer) is proven against the REAL
// payer gateway in test/conformance's per-line pin test — a fake Hub can only
// honor by construction.
func TestCompletePatient_HonorsPinAcrossDifferentLineDrift(t *testing.T) {
	const (
		line20 = "pa.pas@2.0"
		line22 = "pa.pas@2.2"
	)
	// Declare only the 2.0 line on both sides at pend time. CRD/DTR are declared at
	// 2.0 too so the run-to-pend prefix routes at the same line.
	declared20 := []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
		pendedItem: "patient-reported-functional-status",
		extraRoles: map[string]string{"phg": "phg"},
		declared:   declared20,
	})

	// --- run to PENDED ---
	req1 := httptest.NewRequest(http.MethodPost, "/scenario/uc07", nil)
	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, req1, "uc07", "MBR-UC07")
	if !ok {
		t.Fatalf("scenarioToPend(uc07) failed: %d %s", rec1.Code, rec1.Body.String())
	}
	if st.pasToken != line20 {
		t.Fatalf("pendState.pasToken = %q, want %s (both sides declared only the 2.0 line)", st.pasToken, line20)
	}
	assertClaimedVersions(t, stub, "pas-claim", line20)

	// --- mid-pend drift to a DIFFERENT, ROUTABLE line: both sides grow to {2.0, 2.2}. ---
	grown := append(append([]string(nil), declared20...), shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22)
	gw.cfg.DeclaredContractVersions = grown
	driftRegistryTo(t, gw, "payer", grown)

	// Prove the DIVERGENCE is real: an unpinned (fresh) selection now picks 2.2.
	// Unlike the bogus-line drift, this one does NOT refuse — which is exactly why
	// a re-selection here would be silent, and why the claimed-version assertion
	// below is the only thing that can catch it.
	fresh, err := gw.selectLegToken("payer", "pas-claim")
	if err != nil {
		t.Fatalf("fresh selectLegToken after the grow-only drift: %v (the drift setup is broken)", err)
	}
	if fresh != line22 {
		t.Fatalf("fresh selectLegToken = %q, want %s — the drift did not change what a re-selection "+
			"would choose, so this test cannot prove honor-vs-recompute", fresh, line22)
	}

	// --- resume: completePatient runs the patient-DTR exchange and the pinned
	// pas-claim-update leg. ---
	req2 := httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete", nil)
	rec2 := httptest.NewRecorder()
	if ok := gw.completePatient(rec2, req2, st, ""); !ok {
		t.Fatalf("completePatient failed after the mid-pend line drift: %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("completePatient status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if !legAttempted(stub.legTypes, "patient-dtr") {
		t.Fatalf("patient-dtr leg never ran (legs: %v)", stub.legTypes)
	}
	// THE ROW: the resume request claimed the PINNED line, not the line a fresh
	// selection would now choose.
	assertClaimedVersions(t, stub, "pas-claim-update", line20)
	if got := stub.claimedFor("pas-claim-update"); got[0] == fresh {
		t.Fatalf("pas-claim-update claimed %q — the same token a FRESH selection yields; the resume leg "+
			"re-negotiated instead of honoring the pin", got[0])
	}
	// patient-dtr is a version-NEUTRAL leg (paCatalog Contract ""), so it must carry
	// no contract claim at all — a stamp there would be a fabricated version on a
	// leg no contract line governs.
	assertClaimedVersions(t, stub, "patient-dtr", "")

	var resp struct {
		AuthNumber string `json:"authNumber"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil || resp.AuthNumber == "" {
		t.Fatalf("UC-07 resume response missing authNumber: %v (%s)", err, rec2.Body.String())
	}
}

// TestCompletePatient_ChainResumeRunsEgressAdapt is the regression
// test for a CRITICAL review finding: completePatient's pas-claim-update leg must
// re-derive its route via g.selectResumeRoute and thread the output through
// g.egressAdapt — exactly like completeClinician's sibling leg — never build
// directly at shnsdk.LineOf(st.pasToken) with no adaptation. Arm 1/2 (native
// reach) cannot distinguish the two code paths: BuildLine == LineOf(Token)
// either way, so bytes come out identical whether or not egressAdapt ran. Only
// an EgressNativeLines-forced ARM-3 CHAIN resume can observe the difference: own declares
// only pa.pas@2.1, the payer 2.2, and EgressNativeLines restricts
// native reach to {2.1} for the ENTIRE test (never widened, so the missing
// wiring cannot be masked by a native build) — the wired-but-broken code
// builds NATIVELY at 2.2 (LineOf(st.pasToken)) and never touches
// egressAdapt/leg.transformed at all; the fixed code builds at 2.1 and
// transforms up.
func TestCompletePatient_ChainResumeRunsEgressAdapt(t *testing.T) {
	declared21 := []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
		pendedItem: "patient-reported-functional-status",
		extraRoles: map[string]string{"phg": "phg"},
		declared:   declared21,
	})
	// The payer declares pa.pas@2.2 (crd/dtr stay shared @2.1 so the run-to-pend
	// prefix routes normally) — no shared pas line, and D1c restricts arm (2)'s
	// native view to {2.1} so only arm (3) (the transform chain) can bridge it.
	driftRegistryTo(t, gw, "payer", []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS22})
	gw.cfg.EgressNativeLines = []string{"2.1"}

	var transformed int
	gw.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == legTransformedKind {
			transformed++
		}
	}

	// --- run to PENDED: the initial pas-claim submit must ALSO chain (2.1 Up
	// 2.2, request sub-case FULL per compat.go) — confirms the fixture is sane.
	req1 := httptest.NewRequest(http.MethodPost, "/scenario/uc07", nil)
	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, req1, "uc07", "MBR-UC07")
	if !ok {
		t.Fatalf("scenarioToPend(uc07) failed: %d %s", rec1.Code, rec1.Body.String())
	}
	if st.pasToken != "pa.pas@2.2" {
		t.Fatalf("pendState.pasToken = %q, want pa.pas@2.2 (arm-3 chain target)", st.pasToken)
	}
	if transformed == 0 {
		t.Fatalf("fixture invalid: the initial pas-claim submit must have chained (leg.transformed observed=%d)", transformed)
	}
	beforeResume := transformed

	// --- resume: completePatient's pas-claim-update leg must ALSO re-derive the
	// route (arm 3 again — the mesh never widened) and run it through egressAdapt.
	req2 := httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete", nil)
	rec2 := httptest.NewRecorder()
	if ok := gw.completePatient(rec2, req2, st, ""); !ok {
		t.Fatalf("completePatient failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("completePatient status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if transformed <= beforeResume {
		t.Fatalf("completePatient's pas-claim-update leg never ran g.egressAdapt (leg.transformed count "+
			"%d -> %d, want an increase) — the resume path is building shnsdk.LineOf(st.pasToken) directly "+
			"instead of re-running g.selectResumeRoute, unlike completeClinician's sibling leg", beforeResume, transformed)
	}
	assertClaimedVersions(t, stub, "pas-claim-update", "pa.pas@2.2")
}

// TestHandleUC04_PinsBothLegsAcrossMidRequestDrift is the in-request twin: UC-04
// pends and amends inside ONE request, so there is no point at which the test
// gets control between the two legs. The drift therefore runs from INSIDE the
// substrate, on the pas-claim answer (stub.onLeg) — the registry changes under
// handleUC04 while it is mid-flight, before its pas-claim-update leg is built.
//
// Because the drift is to an INCOMPATIBLE line, a re-selection on the update leg
// fails closed (RouteRefusalError → 502), so a dropped pin is a hard red, not a
// silent line change. assertFreshSelectionRefuses proves the drift really did
// take effect.
func TestHandleUC04_PinsBothLegsAcrossMidRequestDrift(t *testing.T) {
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
		pendedItem: "operative-diagnostic-report",
	})
	driftOnPASClaim(gw, stub)

	rec := httptest.NewRecorder()
	gw.handleUC04(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc04", nil))
	assertInRequestPendResumePinned(t, gw, stub, rec, "handleUC04")
}

// TestHandleUC05_PinsBothLegsAcrossMidRequestDrift is TestHandleUC04's federated
// twin: UC-05's amendment evidence comes from the consent-gated federated-query
// legs to metro-spine (two type-legs, FR-24/cdex-9) rather than a local
// DiagnosticReport, so the fixture registers a facility holder and the substrate
// answers the disclosure. Same mid-request drift, same pin bar — and it
// additionally proves the pin survives the FOUR intervening legs (2 federated
// query exchanges) between the pend and the amendment.
func TestHandleUC05_PinsBothLegsAcrossMidRequestDrift(t *testing.T) {
	gw, stub := newPendResumeFixture(t, pendFixtureOpts{
		member: "MBR-UC05", birthDate: "1968-03-12", familyName: "Johansson",
		pendedItem: "operative-diagnostic-report",
		extraRoles: map[string]string{"facility": "metro-spine"},
	})
	driftOnPASClaim(gw, stub)

	rec := httptest.NewRecorder()
	gw.handleUC05(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc05", nil))
	assertInRequestPendResumePinned(t, gw, stub, rec, "handleUC05")

	if !legAttempted(stub.legTypes, "federated-query") {
		t.Fatalf("federated-query leg never ran (legs: %v)", stub.legTypes)
	}
	// The federated-query leg is version-NEUTRAL (paCatalog Contract ""): both
	// type-legs must cross with no contract claim.
	assertClaimedVersions(t, stub, "federated-query", "", "")
}

// driftOnPASClaim installs the MID-REQUEST drift: the instant the substrate sees
// the pas-claim leg, the payer's declared line is rewritten to one this build
// cannot route. handleUC04/handleUC05 are still inside the same request; their
// pas-claim-update leg is built and routed AFTER this fires.
func driftOnPASClaim(gw *Gateway, stub *pendResumeSubstrate) {
	stub.onLeg = func(legType string) {
		if legType != "pas-claim" {
			return
		}
		entry, ok := gw.cfg.Reg.Lookup("payer")
		if !ok {
			return
		}
		entry.ContractVersions = []string{"pa.pas@9.9"}
		gw.cfg.Reg.Set("payer", entry)
	}
}

// assertInRequestPendResumePinned is the shared bar for the two in-request
// pend→update scenarios: the scenario approved, BOTH pas legs ran, both claimed
// the SAME (pre-drift) token on the wire, and the drift genuinely broke fresh
// selection.
func assertInRequestPendResumePinned(t *testing.T, gw *Gateway, stub *pendResumeSubstrate, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want 200 — the pas-claim-update leg re-selected against the drifted "+
			"registry instead of honoring the pin: %s", label, rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthNumber string `json:"authNumber"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.AuthNumber == "" {
		t.Fatalf("%s response missing authNumber: %v (%s)", label, err, rec.Body.String())
	}
	for _, leg := range []string{"pas-claim", "pas-claim-update"} {
		if !legAttempted(stub.legTypes, leg) {
			t.Fatalf("%s: %s leg never ran (legs: %v)", label, leg, stub.legTypes)
		}
	}
	// ONE selection, honored on BOTH legs: the update leg claimed the same token
	// the submit leg did, even though the registry changed in between.
	assertClaimedVersions(t, stub, "pas-claim", "pa.pas@2.0")
	assertClaimedVersions(t, stub, "pas-claim-update", "pa.pas@2.0")

	// The drift really did take effect — a fresh selection now refuses, so the
	// update leg could not have re-selected its way to success.
	assertFreshSelectionRefuses(t, gw, "payer", "pas-claim")
}

// ---- The pended-flow carry-intact guard ----
//
// The pended-line pin's sibling (gateway.go's pendState.carriedEntries): the
// down-leg's declared Carried entries are pinned beside the routed token so a
// RESTORING resume chain has an independent record to verify the payload
// against BEFORE pasStep2122Up would silently no-op over an already-absent
// wrapper. Same file because it is the same seam and the same threat model —
// state that must survive the pend window verbatim or the resume is answering
// a different exchange than the one the payer is holding open.

// upChainPendFixture wires the arm-3 UPCAST topology
// TestCompletePatient_ChainResumeRunsEgressAdapt established, generalized to
// either attestation scenario: own declares only the 2.1 lines, the payer's
// pas line is 2.2, and D1c restricts native reach to {2.1} — so BOTH the
// pended pas-claim submit and the pinned pas-claim-update resume route through
// the pa.pas 2.1->2.2 chain, walked UP. That row is CARRY class (compat.go),
// so its Up half IS pasStep2122Up's restore — the step the guard must run in
// front of.
func upChainPendFixture(t *testing.T, opts pendFixtureOpts) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	gw, stub := newPendResumeFixture(t, opts)
	driftRegistryTo(t, gw, "payer", []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS22})
	gw.cfg.EgressNativeLines = []string{"2.1"}
	return gw, stub
}

// downChainPendFixture is upChainPendFixture's mirror: own declares the 2.2
// pas line, the payer 2.1, native reach restricted to {2.2} — so both pas legs
// route through the SAME manifest row walked DOWN (pasStep2122Down, the half
// that CREATES wrappers). CRD/DTR stay shared at 2.1 so the run-to-pend prefix
// routes on arm (1) either way.
func downChainPendFixture(t *testing.T, opts pendFixtureOpts) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS22}
	gw, stub := newPendResumeFixture(t, opts)
	driftRegistryTo(t, gw, "payer", []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21})
	gw.cfg.EgressNativeLines = []string{"2.2"}
	return gw, stub
}

func uc06PendOpts() pendFixtureOpts {
	return pendFixtureOpts{member: "MBR-UC06", birthDate: "1969-07-21", familyName: "Reyes",
		pendedItem: "functional-status"}
}

func uc07PendOpts() pendFixtureOpts {
	return pendFixtureOpts{member: "MBR-UC07", birthDate: "1990-08-25", familyName: "Haddad",
		pendedItem: "patient-reported-functional-status", extraRoles: map[string]string{"phg": "phg"}}
}

// realPASCarryEntries is a GENUINE declared-carry list: the real pa.pas
// 2.2->2.1 Down step run over a resource bearing a real 2.2-only top-level
// extension, its LossReport.Carried converted through the same
// toSDKLossEntries seam the production write path uses. Never hand-written —
// a hand-written path string would prove the guard rejects a fiction.
//
// It has to be injected rather than produced by the pended leg itself because
// NO SHN builder emits a 2.2-only top-level Claim extension today (produce-iff;
// transform_pas.go's pas22OnlyClaimExtensions doc note), so a real SHN-
// originated pend carries nothing — which is exactly what
// TestScenarioToPend_CarriesNothingWhenTheChainCarriesNothing pins. The
// injection stands in for the payload that WILL carry: a forwarded/native 2.2
// Claim bearing authorizationNumber, the case the multi-version spec's
// obligation exists to guard before arm 3 goes live.
func realPASCarryEntries(t *testing.T) []shnsdk.LossEntry {
	t.Helper()
	const reviewerExt = `{"url":"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-claimResponseReviewer","extension":[{"url":"wasHumanReviewedFlag","valueBoolean":true}]}`
	in := []byte(`{"resourceType":"ClaimResponse","id":"cr-pend-carry","extension":[` + reviewerExt + `]}`)
	_, report, err := pasStep2122Down(in, ExchangeIdentity{CorrelationID: "corr-pend-carry"})
	if err != nil {
		t.Fatalf("fixture: pasStep2122Down: %v", err)
	}
	entries := toSDKLossEntries(report.Carried)
	if len(entries) == 0 {
		t.Fatalf("fixture invalid: want a genuine downcast-with-carry, got %+v", report)
	}
	return entries
}

// collectFailedLegs installs an observer recording every leg.failed event.
func collectFailedLegs(gw *Gateway) *[]ObserverEvent {
	var failed []ObserverEvent
	gw.cfg.Observer = func(e ObserverEvent) {
		if e.Kind == "leg.failed" {
			failed = append(failed, e)
		}
	}
	return &failed
}

// assertPendCarryRefusal is the shared bar for both resume sites: the resume
// refused (502) naming verifyCarryPresent, NOTHING went out on the wire for
// the refused leg, and the refusal is observed on the SAME leg.failed seam
// egressAdapt's own transform refusal uses — Route present, so the
// refusal is legible as a routed leg, not a bare error string.
func assertPendCarryRefusal(t *testing.T, stub *pendResumeSubstrate, rec *httptest.ResponseRecorder, failed []ObserverEvent, label string) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("%s status = %d, want 502: %s", label, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verifyCarryPresent") {
		t.Fatalf("%s refusal does not name the carry guard: %s", label, rec.Body.String())
	}
	if legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatalf("%s: the refused pas-claim-update leg still went out on the wire (legs: %v) — the guard must "+
			"run BEFORE the chain and before anything seals", label, stub.legTypes)
	}
	found := false
	for _, e := range failed {
		if e.LegType != "pas-claim-update" {
			continue
		}
		found = true
		if e.Route == nil {
			t.Fatalf("%s: leg.failed carries no Route — a carry refusal must be as legible as egressAdapt's "+
				"own transform refusal", label)
		}
		if !strings.Contains(e.Detail, "verifyCarryPresent") {
			t.Fatalf("%s: leg.failed Detail = %q, want the carry-guard error", label, e.Detail)
		}
	}
	if !found {
		t.Fatalf("%s: the carry refusal was observer-SILENT (leg.failed events: %+v)", label, failed)
	}
}

// TestScenarioToPend_CarriesNothingWhenTheChainCarriesNothing pins the
// additive field's zero-cost property in BOTH chain directions: the pend
// record is EMPTY for every flow this build originates today, so the resume
// guard is a no-op and no existing green path can change behavior. (It is
// empty because no SHN builder emits a 2.2-only top-level Claim extension —
// produce-iff — not because the wiring is missing; the wiring is
// TestScenarioToPend_ThreadsChainCarriedEntriesIntoThePendRecord's job.)
func TestScenarioToPend_CarriesNothingWhenTheChainCarriesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T, pendFixtureOpts) (*Gateway, *pendResumeSubstrate)
		wantTok string
	}{
		{"up-chain", upChainPendFixture, "pa.pas@2.2"},
		{"down-chain", downChainPendFixture, "pa.pas@2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, _ := tc.fixture(t, uc06PendOpts())
			rec := httptest.NewRecorder()
			st, ok := gw.scenarioToPend(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil), "uc06", "MBR-UC06")
			if !ok {
				t.Fatalf("scenarioToPend failed: %d %s", rec.Code, rec.Body.String())
			}
			if st.pasToken != tc.wantTok {
				t.Fatalf("pendState.pasToken = %q, want %s (the fixture is not exercising arm 3)", st.pasToken, tc.wantTok)
			}
			if len(st.carriedEntries) != 0 {
				t.Fatalf("pendState.carriedEntries = %+v, want empty — no SHN builder emits a 2.2-only "+
					"top-level Claim extension, so nothing can be carried", st.carriedEntries)
			}
		})
	}
}

// TestCompleteClinician_PendedCarryStrippedMidPendRefuses is the obligation's
// enforcement row: a pend whose own loss record declares carried
// content, resumed on a RESTORING chain with a payload that no longer bears
// the shn-carried-content wrapper, must refuse — not silently no-op through
// pasRestoreCarriedExtensions (whose doc comment records that it cannot itself
// tell "never carried" from "carried, then stripped").
func TestCompleteClinician_PendedCarryStrippedMidPendRefuses(t *testing.T) {
	gw, stub := upChainPendFixture(t, uc06PendOpts())
	failed := collectFailedLegs(gw)

	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil), "uc06", "MBR-UC06")
	if !ok {
		t.Fatalf("scenarioToPend failed: %d %s", rec1.Code, rec1.Body.String())
	}

	// The MUTATION: the pend's record says content was carried; the payload the
	// resume rebuilds does not bear it (the wrapper stripped across the pend
	// window). Everything else about the flow is the control below, unchanged.
	st.carriedEntries = realPASCarryEntries(t)

	rec2 := httptest.NewRecorder()
	if gw.completeClinician(rec2, httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", nil), st, "", "") {
		t.Fatalf("completeClinician resumed GREEN over a stripped carry — the restore would have silently "+
			"no-opped and the amendment would answer the pend without content its own loss record declares: %s",
			rec2.Body.String())
	}
	assertPendCarryRefusal(t, stub, rec2, *failed, "completeClinician")
}

// TestCompletePatient_PendedCarryStrippedMidPendRefuses proves the guard is
// wired at BOTH resume sites (completeClinician and completePatient are
// separate functions — a review finding here was exactly a fix applied to
// one and not the other).
func TestCompletePatient_PendedCarryStrippedMidPendRefuses(t *testing.T) {
	gw, stub := upChainPendFixture(t, uc07PendOpts())
	failed := collectFailedLegs(gw)

	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, httptest.NewRequest(http.MethodPost, "/scenario/uc07", nil), "uc07", "MBR-UC07")
	if !ok {
		t.Fatalf("scenarioToPend(uc07) failed: %d %s", rec1.Code, rec1.Body.String())
	}
	st.carriedEntries = realPASCarryEntries(t)

	rec2 := httptest.NewRecorder()
	if gw.completePatient(rec2, httptest.NewRequest(http.MethodPost, "/scenario/uc07/complete", nil), st, "") {
		t.Fatalf("completePatient resumed GREEN over a stripped carry: %s", rec2.Body.String())
	}
	assertPendCarryRefusal(t, stub, rec2, *failed, "completePatient")
}

// TestCompleteClinician_UnstrippedPendResumesGreen is the refusal row's
// CONTROL: the identical fixture and the identical restoring resume chain,
// with the pend record left as the real flow produced it, resumes to APPROVED.
// Without it the refusal row could pass for any reason at all.
func TestCompleteClinician_UnstrippedPendResumesGreen(t *testing.T) {
	gw, stub := upChainPendFixture(t, uc06PendOpts())

	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil), "uc06", "MBR-UC06")
	if !ok {
		t.Fatalf("scenarioToPend failed: %d %s", rec1.Code, rec1.Body.String())
	}
	rec2 := httptest.NewRecorder()
	if !gw.completeClinician(rec2, httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", nil), st, "", "") {
		t.Fatalf("control: completeClinician must resume green over an unstripped pend: %d %s", rec2.Code, rec2.Body.String())
	}
	if !legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatalf("control: pas-claim-update leg never ran (legs: %v)", stub.legTypes)
	}
	assertClaimedVersions(t, stub, "pas-claim-update", "pa.pas@2.2")
}

// TestPendCarryGuardSkipsANonRestoringResumeChain is the guard's DIRECTION
// row: a resume chain walked DOWN creates wrappers, it never restores any — so
// a freshly built payload legitimately bears no shn-carried-content wrapper
// yet, and the guard must NOT fire even though the pend record declares a
// carry. Gating on "declared non-empty" alone would refuse this perfectly
// honest flow.
func TestPendCarryGuardSkipsANonRestoringResumeChain(t *testing.T) {
	gw, stub := downChainPendFixture(t, uc06PendOpts())

	rec1 := httptest.NewRecorder()
	st, ok := gw.scenarioToPend(rec1, httptest.NewRequest(http.MethodPost, "/scenario/uc06", nil), "uc06", "MBR-UC06")
	if !ok {
		t.Fatalf("scenarioToPend failed: %d %s", rec1.Code, rec1.Body.String())
	}
	st.carriedEntries = realPASCarryEntries(t)

	rec2 := httptest.NewRecorder()
	if !gw.completeClinician(rec2, httptest.NewRequest(http.MethodPost, "/scenario/uc06/complete", nil), st, "", "") {
		t.Fatalf("a DOWN-walking resume chain restores nothing, so the guard must not fire: %d %s",
			rec2.Code, rec2.Body.String())
	}
	if !legAttempted(stub.legTypes, "pas-claim-update") {
		t.Fatalf("pas-claim-update leg never ran (legs: %v)", stub.legTypes)
	}
}

// TestChainRestoresCarry pins the guard's gate directly against the REAL
// pa.pas manifest rows (chainFor), both directions and both chain lengths —
// the walk-direction switch every chain-walking helper in this package has to
// mirror (applyChain, envelopeChainReports, routeInfoFor).
func TestChainRestoresCarry(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"2.1", "2.2", true},  // the CARRY row walked Up == pasStep2122Up's restore
		{"2.2", "2.1", false}, // walked Down: creates wrappers, restores none
		{"2.0", "2.2", true},  // two hops, both Up — the carry row is the second
		{"2.2", "2.0", false}, // two hops, both Down
		{"2.0", "2.1", false}, // a FULL row: no carry mechanism at all
	} {
		chain := chainFor("pa.pas", tc.from, tc.to)
		if len(chain) == 0 {
			t.Fatalf("fixture invalid: no pa.pas chain %s->%s", tc.from, tc.to)
		}
		if got := chainRestoresCarry(chain, tc.from); got != tc.want {
			t.Fatalf("chainRestoresCarry(pa.pas %s->%s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestCarriedEntriesFrom pins the pend record's input: every report's Carried
// entries, flattened in chain order, through the one toSDKLossEntries
// conversion seam — and nil (never an empty non-nil slice) when a chain
// carried nothing, so the additive field stays absent for non-carry flows.
func TestCarriedEntriesFrom(t *testing.T) {
	if got := carriedEntriesFrom(nil); got != nil {
		t.Fatalf("carriedEntriesFrom(nil) = %+v, want nil", got)
	}
	noCarry := []LossReport{{Module: "pa.pas 2.1->2.2", Synthesized: []LossEntry{{Path: "Bundle.identifier"}}}}
	if got := carriedEntriesFrom(noCarry); got != nil {
		t.Fatalf("carriedEntriesFrom(synthesize-only) = %+v, want nil — Synthesized is not Carried", got)
	}
	twoStep := []LossReport{
		{Module: "pa.pas 2.2->2.1", Carried: []LossEntry{{Path: "Claim.extension:authorizationNumber", Detail: "carried; source line 2.2"}}},
		{Module: "pa.pas 2.1->2.0", Carried: []LossEntry{{Path: "ClaimResponse.extension:claimResponseReviewer"}}},
	}
	got := carriedEntriesFrom(twoStep)
	if len(got) != 2 || got[0].Path != "Claim.extension:authorizationNumber" || got[1].Path != "ClaimResponse.extension:claimResponseReviewer" {
		t.Fatalf("carriedEntriesFrom(two-step) = %+v, want both entries in chain order", got)
	}
	if got[0].Detail != "carried; source line 2.2" {
		t.Fatalf("Detail dropped in conversion: %+v", got[0])
	}
}

// TestScenarioToPend_ThreadsChainCarriedEntriesIntoThePendRecord is the WRITE-
// path wiring guard, and the one property here no round trip can observe
// today: with no SHN builder emitting a 2.2-only top-level Claim extension
// (produce-iff), every real pend carries nothing, so a behavioral test cannot
// distinguish "wired and empty" from "not wired". It is the same lexical-guard
// exception assertBuildsAfterLineSelection is (below), for the same reason,
// and it retires the day a real carrying payload exists to drive.
func TestScenarioToPend_ThreadsChainCarriedEntriesIntoThePendRecord(t *testing.T) {
	src, err := os.ReadFile("originate_resume.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := extractFunc(t, string(src), "scenarioToPend")
	if strings.Contains(fn, ", _, err = g.egressAdapt(") {
		t.Fatal("scenarioToPend DISCARDS egressAdapt's LossReports — the pended leg's Carried entries are the " +
			"pend record's only input, so discarding them leaves the resume guard nothing to verify against")
	}
	adaptAt := strings.Index(fn, "g.egressAdapt(route, bundleJSON,")
	if adaptAt < 0 {
		t.Fatal("scenarioToPend no longer runs its pas-claim leg through g.egressAdapt")
	}
	pinAt := strings.Index(fn, "carriedEntries: carriedEntriesFrom(")
	if pinAt < 0 {
		t.Fatal("scenarioToPend does not pin the chain's Carried entries into the returned pendState")
	}
	if pinAt < adaptAt {
		t.Fatal("scenarioToPend pins carriedEntries BEFORE the chain ran — it could only be recording an empty list")
	}
}

// ---- select-before-build source guard ----
//
// This is one of the two lexical guards in this file (the other being
// TestScenarioToPend_ThreadsChainCarriedEntriesIntoThePendRecord) — the
// exceptions to the replacement of the pin source-guards with the
// behavioral fixtures above, each for the same reason: a property no round trip
// can observe. This one asserts that the pas-claim line must be selected BEFORE
// either bundle is BUILT, in source order. A build that ran before the selection
// would produce bytes at one line while the leg routed (and stamped) another —
// and because both lines can be individually valid, every behavioral assertion
// above would still pass. Only reading the source order catches it. (The other
// guard differs in that it is retirable: it goes behavioral the day a real
// payload genuinely carries.)

func assertBuildsAfterLineSelection(t *testing.T, fn, fnName string) {
	t.Helper()
	sel := `route, ok := g.selectLegLineOrFail(w, res.recipient, "pas-claim", pasCorr)`
	selAt := strings.Index(fn, sel)
	if selAt < 0 {
		t.Fatalf("%s does not pre-select the pas-claim line via g.selectLegLineOrFail before its pas-claim leg", fnName)
	}
	for _, build := range []string{
		"shnsdk.BuildConformantClaimBundleAtLine(route.BuildLine,",
		"shnsdk.BuildConformantClaimUpdateBundleAtLine(route.BuildLine,",
	} {
		at := strings.Index(fn, build)
		if at < 0 {
			t.Fatalf("%s does not build via %s — the routed line must choose the builder", fnName, build)
		}
		if at < selAt {
			t.Fatalf("%s builds (%s) BEFORE selecting the line — select-before-build violated", fnName, build)
		}
	}
}

func TestHandleUC04_SelectsLineBeforeBuilding(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	assertBuildsAfterLineSelection(t, extractFunc(t, string(src), "handleUC04"), "handleUC04")
}

func TestHandleUC05_SelectsLineBeforeBuilding(t *testing.T) {
	src, err := os.ReadFile("originate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	assertBuildsAfterLineSelection(t, extractFunc(t, string(src), "handleUC05"), "handleUC05")
}
