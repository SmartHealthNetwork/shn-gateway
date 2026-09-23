package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fixedClock is the deterministic clock the native-PAS tests inject for the
// gateway-projected EOB `created`.
var fixedClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }

// eobProcedureSystemBytes returns item[0].productOrService.coding[0].system of an EOB JSON.
func eobProcedureSystemBytes(t *testing.T, eobJSON []byte) string {
	t.Helper()
	var eob struct {
		Item []struct {
			ProductOrService struct {
				Coding []struct {
					System string `json:"system"`
				} `json:"coding"`
			} `json:"productOrService"`
		} `json:"item"`
	}
	if err := json.Unmarshal(eobJSON, &eob); err != nil {
		t.Fatalf("unmarshal EOB: %v", err)
	}
	if len(eob.Item) == 0 || len(eob.Item[0].ProductOrService.Coding) == 0 {
		t.Fatalf("EOB has no productOrService coding: %s", eobJSON)
	}
	return eob.Item[0].ProductOrService.Coding[0].System
}

// stubPartnerSrv is a partner $submit endpoint returning a fixed status + body.
// (Named distinctly from the native_test.go stubPartner struct, same package.)
func stubPartnerSrv(t *testing.T, code int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Native submission delivers the participant bytes without projecting or storing
// clinical decisions. Local projection and Patient Access are separate actions.
func TestNativeSubmit_RelaysWithoutClinicalWrites(t *testing.T) {
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED") // CPT 72148, binds to MBR-COVERED

	t.Run("payer approval bytes without clinical writes", func(t *testing.T) {
		const partnerRef = "PARTNER-REF-CONF"
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"` + partnerRef + `","preAuthPeriod":{"end":"2030-01-01"}}`)
		body = fixturePASResponse(t, body, true)
		srv := nativeRelayServer(t, http.StatusOK, body, conformant)
		store := nativeClinicalStoreForbidden{}
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved conformant submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("Response not forwarded verbatim")
		}
		assertNativeRelayWithoutClinicalEffects(t, res, body, http.StatusOK, "application/json")
	})

	t.Run("payer pend bytes without clinical writes", func(t *testing.T) {
		body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}},{"resource":{"resourceType":"Task","status":"requested"}}]}`)
		body = fixturePASResponse(t, body, true)
		srv := nativeRelayServer(t, http.StatusOK, body, conformant)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("pended conformant submit: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("pended Bundle not forwarded verbatim")
		}
		assertNativeRelayWithoutClinicalEffects(t, res, body, http.StatusOK, "application/json")
	})

	t.Run("HCPCS-coded request remains opaque relay", func(t *testing.T) {

		hcpcs := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
			{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424","display":"Stationary Oxygen System"}]}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
		]}`)
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-HCPCS"}`)
		body = fixturePASResponse(t, body, true)
		srv := nativeRelayServer(t, http.StatusOK, body, hcpcs)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-hcpcs", "PCI-1", hcpcs)
		if err != nil {
			t.Fatalf("HCPCS conformant submit: unexpected error: %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("HCPCS submit must FORWARD (status 0), got %d msg=%s", res.Status, res.Message)
		}
		if !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("HCPCS submit response not relayed verbatim")
		}
		assertNativeRelayWithoutClinicalEffects(t, res, body, http.StatusOK, "application/json")
	})

	t.Run("unrecognized procedure system remains opaque relay", func(t *testing.T) {
		// These source bytes are an opaque relay fixture, not terminology evidence.
		other := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
			{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://snomed.info/sct","code":"12345","display":"x"}]}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
		]}`)
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-OTHER"}`)
		body = fixturePASResponse(t, body, true)
		srv := nativeRelayServer(t, http.StatusOK, body, other)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-other", "PCI-1", other)
		if err != nil {
			t.Fatalf("unrecognized-system submit: unexpected error: %v", err)
		}
		if res.Status != 0 || !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("unrecognized-system submit must forward verbatim; status=%d", res.Status)
		}
		assertNativeRelayWithoutClinicalEffects(t, res, body, http.StatusOK, "application/json")
	})
}

// serviceRequestSubmitBundle builds a single-shot conformant ServiceRequest $submit bundle via the
// SDK (the same builder the originator uses), with InfoChanged toggled — so a row about what a
// requester may send drives the REAL built bytes rather than a hand-written approximation.
func serviceRequestSubmitBundle(t *testing.T, infoChanged bool) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{ItemFacts: syntheticPASItemFacts(), Coverage: testMemberCoverage("MBR-COVERED"),
		Provider:       testRequestingProvider(),
		MemberIDSystem: shnsdk.MemberSystem,
		SR:             sr, PatientRef: "Patient/MBR-COVERED", CoverageRef: "Coverage/MBR-COVERED", MemberID: "MBR-COVERED",
		Corr: "corr-sr-submit", Created: fixedClock(), InfoChanged: infoChanged,
		Payer: shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("serviceRequestSubmitBundle (infoChanged=%v): %v", infoChanged, err)
	}
	return b
}

// PCV-07: source procedure coding is delivered exactly, without implicit EOB
// projection. Explicit local projection correctness is a separate obligation.
func TestNativePAS_RelaysOrderCodingWithoutClinicalWrites(t *testing.T) {
	for _, tc := range []struct{ name, system, code, display string }{
		{"cpt", "http://www.ama-assn.org/go/cpt", "72148", "MRI lumbar spine w/o contrast"},
		{"hcpcs", "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", "L8000", "Breast prosthesis, mastectomy bra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
				{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
				{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"` + tc.system + `","code":"` + tc.code + `","display":"` + tc.display + `"}]}}},
				{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
				{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
			]}`)
			body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`)
			body = fixturePASResponse(t, body, true)
			srv := nativeRelayServer(t, http.StatusOK, body, bundle)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim", "corr-"+tc.name, "PCI-1", bundle)
			if err != nil {
				t.Fatal(err)
			}
			assertNativeRelayWithoutClinicalEffects(t, res, body, http.StatusOK, "application/json")
		})
	}
}

// stripInfoChangedExtension removes the Da Vinci PAS infoChanged item extension from every
// Claim.item[*] of a built update bundle — used to reconstruct a genuinely non-conformant
// carry-forward amendment now that BuildConformantClaimUpdateBundle emits infoChanged
// unconditionally and can no longer produce one itself. Reuses
// mutateBundleEntries (pas_native_test.go) for the JSON surgery.
func stripInfoChangedExtension(t *testing.T, bundleJSON []byte) []byte {
	t.Helper()
	return mutateBundleEntries(t, bundleJSON, func(res map[string]any) {
		if res["resourceType"] != "Claim" {
			return
		}
		items, _ := res["item"].([]any)
		for _, it := range items {
			item, _ := it.(map[string]any)
			if item == nil {
				continue
			}
			ext, _ := item["extension"].([]any)
			kept := make([]any, 0, len(ext))
			for _, e := range ext {
				em, _ := e.(map[string]any)
				if em != nil && em["url"] == pasInfoChangedExtURL {
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(item, "extension")
			} else {
				item["extension"] = kept
			}
		}
	})
}

// loadDeniedClaimResponseBytes builds a bare ClaimResponse with a terminal A3
// denial (reviewActionCode A3) — the still-insufficient/denied update path.
func loadDeniedClaimResponseBytes(t *testing.T) []byte {
	t.Helper()
	b, err := shnsdk.BuildDeniedResponse("Patient/MBR-COVERED", "partner", "denied for test", fixedClock())
	if err != nil {
		t.Fatalf("loadDeniedClaimResponseBytes BuildDeniedResponse: %v", err)
	}
	return b
}

// PCV-07/08/09: native updates carry the first peer reply without consulting a
// local pend record. These outcomes retain the original conformant request.
func TestNativeUpdate_ConformantRequestRelaysWithoutLocalPend(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundle(t)
	approval := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	absentPendReply := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true)
	denial := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
	for _, row := range []struct {
		name   string
		body   []byte
		status int
		seed   bool
	}{
		{"approval bytes", approval, http.StatusOK, true},
		{"peer 500 boom", []byte("boom"), http.StatusInternalServerError, true},
		{"no prior local pend", absentPendReply, http.StatusOK, false},
		{"terminal denial bytes", denial, http.StatusOK, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := nativeRelayServer(t, row.status, row.body, bundle)
			var store Store = nativeClinicalStoreForbidden{}
			if row.seed {
				local := newCensusSoR()
				if err := local.RecordPendedClaim("PCI-CONF-UPD", updatePriorCorr); err != nil {
					t.Fatal(err)
				}
				before, _, err := local.PendRecordOf("PCI-CONF-UPD", updatePriorCorr)
				if err != nil {
					t.Fatal(err)
				}
				store = local
				t.Cleanup(func() {
					after, _, err := local.PendRecordOf("PCI-CONF-UPD", updatePriorCorr)
					if err != nil || after != before {
						t.Errorf("native update changed local pend: before=%+v after=%+v err=%v", before, after, err)
					}
				})
			}
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-CONF-UPD", bundle)
			if err != nil {
				t.Fatal(err)
			}
			if res.ApplicationStatus != row.status || res.Response.ContentType() != "application/json" || !bytes.Equal(responseBytes(res), row.body) || !res.ResponseRelayed() {
				t.Fatalf("native reply changed: status=%d application=%d media=%q body=%q", res.Status, res.ApplicationStatus, res.Response.ContentType(), responseBytes(res))
			}
			if res.Status != 0 && res.Status != row.status {
				t.Fatalf("local status=%d peer=%d", res.Status, row.status)
			}
			if res.Commit != nil || res.Rollback != nil || len(res.SideEffectFHIR) != 0 {
				t.Fatal("native response acquired local clinical work")
			}
		})
	}
	t.Run("transport unreachable", func(t *testing.T) {
		n := NewNativeResponder(&http.Client{}, "http://127.0.0.1:1", "shn-order-select", nativeClinicalStoreForbidden{}, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-CONF-UPD", bundle)
		if err == nil || res.ApplicationStatus != 0 || len(responseBytes(res)) != 0 || res.Commit != nil || res.Rollback != nil || len(res.SideEffectFHIR) != 0 {
			t.Fatalf("no-response transport fault fabricated reply/effect: err=%v res=%+v", err, res)
		}
	})
}

// The historical body remains synthetic graph-wrapped; it is evidence of the
// bytes this adapter carries, not historical graph or approval conformance.
func TestNativeUpdate_AmendAfterResolution(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "br-payer", "pas-update-response-amend-after-resolution.json"))
	if err != nil {
		t.Fatal(err)
	}
	afterTimer := fixturePASResponse(t, raw, true)
	for _, row := range []struct {
		name    string
		request func() []byte
	}{
		{"re-evaluation", func() []byte { return originatorBuiltConformantUpdateBundleProfile(t, true) }},
		{"carry-forward", func() []byte {
			return stripInfoChangedExtension(t, originatorBuiltConformantUpdateBundleProfile(t, true))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			request := row.request()
			srv := nativeRelayServer(t, http.StatusOK, afterTimer, request, "application/fhir+json")
			store := newCensusSoR()
			if err := store.RecordPendedClaim(updatePCI, updatePriorCorr); err != nil {
				t.Fatal(err)
			}
			before, _, err := store.PendRecordOf(updatePCI, updatePriorCorr)
			if err != nil {
				t.Fatal(err)
			}
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", updatePCI, request)
			if err != nil {
				t.Fatal(err)
			}
			assertNativeRelayWithoutClinicalEffects(t, res, afterTimer, http.StatusOK, "application/fhir+json")
			if !res.ResponseRelayed() {
				t.Fatal("historical peer reply ownership lost")
			}
			after, _, err := store.PendRecordOf(updatePCI, updatePriorCorr)
			if err != nil || after != before {
				t.Fatalf("native update changed local pend: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
