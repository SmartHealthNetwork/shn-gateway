// originate_dispatch_test.go — hermetic unit test for handleDispatch, the
// caller-named-member wrapper around originateDispatch (the SHN Kit's
// free-form "run against your data" entry).
//
// It reuses the originate_homeoxygen_test.go fixture verbatim (homeOxygenSoR,
// homeOxygenSubstrate, fakePopulator, buildHomeOxygenSupplier) — homeOxygenSoR's
// `member`/`demo` fields were generalized (not hardcoded to "MBR-OX") precisely so
// this file could stand up a SECOND provider-data order-dispatch persona
// (MBR-PD-UC03/E1390, the prong's doc-named UC-03 member) without inventing a new
// fixture style. Each test builds its own fresh gw/sor/stub instance (spies and the
// canned substrate's leg log are per-instance state).
package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildDispatchDeviceRequest is buildHomeOxygenDeviceRequest generalized to an arbitrary
// HCPCS product code — used to seed the MBR-PD-UC03/E1390 persona (a DIFFERENT order code
// than MBR-OX/E0431), proving the dispatch handler reads the code from the SoR, not a literal.
func buildDispatchDeviceRequest(id, patientRef, performerRef, code, display, dxCode string) ([]byte, error) {
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
				"code":    code,
				"display": display,
			}},
		},
		"reasonCode": []map[string]any{{
			"coding": []map[string]string{{
				"system": "http://hl7.org/fhir/sid/icd-10-cm",
				"code":   dxCode,
			}},
		}},
	}
	return json.Marshal(dr)
}

// dispatchFixture bundles a fresh gw + its sor + its substrate for one member, built the
// same way TestHandleHomeOxygen builds its gw (same crypto/registry/config shape) —
// parameterized on member/demo/order/performer/supplier so it can stand up either MBR-OX
// or a second persona.
type dispatchFixture struct {
	gw        *Gateway
	sor       *homeOxygenSoR
	stub      *homeOxygenSubstrate
	canonical string
}

func newDispatchFixture(t *testing.T, member string, demo Demo, orderJSON []byte, performerRef string, supplierJSON []byte) *dispatchFixture {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, _ := genKeyPair(t)
	payerSignPub, _ := genED25519(t)

	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }

	base := NewStubHolderData()
	pci := shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName)

	sor := &homeOxygenSoR{
		StubHolderData: base,
		member:         member,
		demo:           demo,
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
		AuthzURL:           "http://stub.test",
		AuthzPub:           authzPub,
		HubTransportPub:    authzPub,
		HubURL:             "http://stub.test",
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

	return &dispatchFixture{gw: gw, sor: sor, stub: stub, canonical: canonical}
}

// newMBROXDispatchFixture builds the MBR-OX/E0431 persona fixture — the same seed
// TestHandleHomeOxygen uses — for the parity row.
func newMBROXDispatchFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	const performerRef = "Organization/org-dme-ox"
	orderJSON, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/MBR-OX", performerRef)
	if err != nil {
		t.Fatalf("build DeviceRequest: %v", err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatalf("build supplier: %v", err)
	}
	demo := Demo{BirthDate: "1958-07-14", FamilyName: "Okafor-Oxygen"}
	return newDispatchFixture(t, "MBR-OX", demo, orderJSON, performerRef, supplierJSON)
}

// decisionFields extracts the decision-bearing fields from a /scenario/{homeoxygen,dispatch}
// JSON response body for the parity comparison (row 1) — timestamps/ids are not part of this
// response shape, but comparing by field (not raw bytes) is still the honest assertion.
func decisionFields(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, body)
	}
	return map[string]any{
		"paRequired": m["paRequired"],
		"authNumber": m["authNumber"],
		"validUntil": m["validUntil"],
		"qrAnswers":  m["qrAnswers"],
	}
}

// TestHandleDispatch_Parity — row 1: POST /scenario/dispatch {"member":"MBR-OX"} returns the
// same status + decision-bearing response fields as POST /scenario/homeoxygen. Each call gets
// its OWN fresh fixture (fixture state — spies, substrate leg log — is per-instance), so this
// is a same-member, same-canned-substrate comparison, not a shared-state one.
func TestHandleDispatch_Parity(t *testing.T) {
	hoFix := newMBROXDispatchFixture(t)
	hoReq := httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", nil)
	hoRec := httptest.NewRecorder()
	hoFix.gw.handleHomeOxygen(hoRec, hoReq)
	if hoRec.Code != http.StatusOK {
		t.Fatalf("/scenario/homeoxygen: want 200, got %d body=%s", hoRec.Code, hoRec.Body.String())
	}

	dispFix := newMBROXDispatchFixture(t)
	dispReq := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"MBR-OX"}`))
	dispRec := httptest.NewRecorder()
	dispFix.gw.handleDispatch(dispRec, dispReq)
	if dispRec.Code != http.StatusOK {
		t.Fatalf("/scenario/dispatch: want 200, got %d body=%s", dispRec.Code, dispRec.Body.String())
	}

	if dispRec.Code != hoRec.Code {
		t.Errorf("status mismatch: homeoxygen=%d dispatch=%d", hoRec.Code, dispRec.Code)
	}
	hoFields := decisionFields(t, hoRec.Body.Bytes())
	dispFields := decisionFields(t, dispRec.Body.Bytes())
	for k, want := range hoFields {
		got := dispFields[k]
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("decision field %q mismatch: homeoxygen=%s dispatch=%s", k, wantJSON, gotJSON)
		}
	}
}

// TestHandleDispatch_ArbitraryMember — row 2: POST /scenario/dispatch {"member":"MBR-PD-UC03"}
// (the E1390 dispatch persona the prong's doc already names as a served member) succeeds with
// decision fields present. This is a DIFFERENT member + DIFFERENT order code (E1390, not E0431)
// than the parity row — proving the handler is member-parameterized, not hardcoded to MBR-OX.
func TestHandleDispatch_ArbitraryMember(t *testing.T) {
	const member = "MBR-PD-UC03"
	const performerRef = "Organization/org-dme-uc03"
	demo := Demo{BirthDate: "1962-03-02", FamilyName: "Delgado-Dispatch"}
	orderJSON, err := buildDispatchDeviceRequest("dr-uc03", "Patient/"+member, performerRef, "E1390", "Stationary oxygen concentrator", "J96.10")
	if err != nil {
		t.Fatalf("build DeviceRequest: %v", err)
	}
	supplierJSON, err := buildHomeOxygenSupplier("org-dme-uc03")
	if err != nil {
		t.Fatalf("build supplier: %v", err)
	}
	fix := newDispatchFixture(t, member, demo, orderJSON, performerRef, supplierJSON)

	req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"`+member+`"}`))
	rec := httptest.NewRecorder()
	fix.gw.handleDispatch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(fix.sor.openOrderCalls) == 0 || fix.sor.openOrderCalls[0] != member {
		t.Errorf("OpenOrder not called with %q (calls: %v)", member, fix.sor.openOrderCalls)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["paRequired"] != true {
		t.Errorf("paRequired = %v, want true", body["paRequired"])
	}
	authNum, _ := body["authNumber"].(string)
	if authNum == "" {
		t.Errorf("authNumber missing/empty in response: %s", rec.Body.String())
	}
}

// dispatchBadRequestFixture builds a minimal gw sufficient for the 400 rows: they all fail
// (or, for the lenient-extra-fields row, succeed) before any SoR/substrate leg is touched
// beyond ResolvePatient, so the MBR-OX multi-leg fixture is reused for uniformity.
func dispatchBadRequestFixture(t *testing.T) *dispatchFixture {
	t.Helper()
	return newMBROXDispatchFixture(t)
}

// TestHandleDispatch_BadRequest — row 3: empty body / {} / {"member":""} all 400 "member is
// required"; a body with unknown extra fields is still fine (lenient decode, like the sibling
// handlers) — {"member":"MBR-OX","extra":"whatever"} succeeds (200).
func TestHandleDispatch_BadRequest(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		fix := dispatchBadRequestFixture(t)
		req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", nil)
		rec := httptest.NewRecorder()
		fix.gw.handleDispatch(rec, req)
		assertMemberRequired400(t, rec)
	})
	t.Run("empty object", func(t *testing.T) {
		fix := dispatchBadRequestFixture(t)
		req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{}`))
		rec := httptest.NewRecorder()
		fix.gw.handleDispatch(rec, req)
		assertMemberRequired400(t, rec)
	})
	t.Run("empty member", func(t *testing.T) {
		fix := dispatchBadRequestFixture(t)
		req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":""}`))
		rec := httptest.NewRecorder()
		fix.gw.handleDispatch(rec, req)
		assertMemberRequired400(t, rec)
	})
	t.Run("unknown extra fields are lenient", func(t *testing.T) {
		fix := dispatchBadRequestFixture(t)
		req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"MBR-OX","extra":"whatever"}`))
		rec := httptest.NewRecorder()
		fix.gw.handleDispatch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200 (lenient decode of unknown extra fields), got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func assertMemberRequired400(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["error"] != "member is required" {
		t.Errorf(`error = %q, want "member is required"`, body["error"])
	}
}

// TestHandleDispatch_UnknownMember — row 4: {"member":"MBR-NOPE"} → 400 "unknown member"
// (originateDispatch's existing ResolvePatient failure path).
func TestHandleDispatch_UnknownMember(t *testing.T) {
	fix := newMBROXDispatchFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{"member":"MBR-NOPE"}`))
	rec := httptest.NewRecorder()
	fix.gw.handleDispatch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["error"] != "unknown member" {
		t.Errorf(`error = %q, want "unknown member"`, body["error"])
	}
}

// TestHandler_DispatchRouteRegistered asserts that the provider-role Handler() mux routes
// POST /scenario/dispatch (returns something other than 404 / method-not-allowed) — a
// registration probe mirroring TestHandler_HomeOxygenRouteRegistered.
func TestHandler_DispatchRouteRegistered(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodPost, "/scenario/dispatch", bytes.NewBufferString(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /scenario/dispatch not registered in provider Handler(): got %d", rec.Code)
	}
}
