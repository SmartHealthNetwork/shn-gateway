package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestNativeSubmit_ConformantRecordsEOB: the conformant PAS submit leg's native-forward
// (handlePASClaimNative) relays the partner's decision verbatim AND projects the Store side-effects —
// on approval the PDex EOB single-sourced from the decision (AuthNumber = the partner's preAuthRef,
// CPT from the conformant Claim's ServiceRequest = F2 72148, patientRef = the BOUND member per R-7),
// on a pend the RecordPendedClaim ledger write. This re-points the earlier pure-relay assertion now
// that the conformant submit carries the EOB/ledger (the minimized leg's side-effects, relocated).
// Mirrors TestNativePAS_Submit for the conformant
// shape (read by parseConformantPASSubjects, not the strict ParseClaimBundle).
func TestNativeSubmit_ConformantRecordsEOB(t *testing.T) {
	conformant := originatorBuiltConformantBundle(t, "MBR-COVERED") // CPT 72148, binds to MBR-COVERED

	t.Run("approved: verbatim + EOB carries partner preAuthRef and claim CPT", func(t *testing.T) {
		const partnerRef = "PARTNER-REF-CONF"
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"` + partnerRef + `","preAuthPeriod":{"end":"2030-01-01"}}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		store := NewStubHolderData()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved conformant submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("ResponseFHIR not forwarded verbatim")
		}
		if len(res.SideEffectFHIR) != 1 {
			t.Fatalf("want 1 EOB side-effect, got %d", len(res.SideEffectFHIR))
		}
		if !bytes.Contains(res.SideEffectFHIR[0], []byte(partnerRef)) {
			t.Fatalf("EOB AuthNumber is not the partner preAuthRef (provenance):\n%s", res.SideEffectFHIR[0])
		}
		if !bytes.Contains(res.SideEffectFHIR[0], []byte("72148")) {
			t.Fatalf("EOB CPT not sourced from the conformant Claim's ServiceRequest (provenance, F2)")
		}
		if res.Commit == nil {
			t.Fatalf("approved must carry a RecordEOB Commit")
		}
	})

	t.Run("pended: verbatim Bundle + RecordPendedClaim, no EOB", func(t *testing.T) {
		body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}},{"resource":{"resourceType":"Task","status":"requested"}}]}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("pended conformant submit: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("pended Bundle not forwarded verbatim")
		}
		if res.Commit == nil || len(res.SideEffectFHIR) != 0 {
			t.Fatalf("pended must RecordPendedClaim and emit NO EOB")
		}
	})

	t.Run("HCPCS-coded ServiceRequest (http): forwards AND builds an EOB with the HCPCS system (DEF-14)", func(t *testing.T) {
		// DEF-14: a real partner codes the order in HCPCS Level II (E0424 home-oxygen, L8000).
		// The pinned systemHCPCS is http:// (the br-provider wire value) — note this fixture is
		// normalized from the prior https:// (a behavior-update, not s/https/http/). The EOB is now
		// built and carries coding.system == HCPCS (no longer the "soft" no-EOB).
		hcpcs := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
			{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424","display":"Stationary Oxygen System"}]}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
		]}`)
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-HCPCS"}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-hcpcs", "PCI-1", hcpcs)
		if err != nil {
			t.Fatalf("HCPCS conformant submit: unexpected error: %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("HCPCS submit must FORWARD (status 0), got %d msg=%s", res.Status, res.Message)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("HCPCS submit response not relayed verbatim")
		}
		if len(res.SideEffectFHIR) != 1 || res.Commit == nil {
			t.Fatalf("HCPCS submit must now build ONE EOB side-effect + Commit; got side-effects=%d commit=%v", len(res.SideEffectFHIR), res.Commit != nil)
		}
		if got := eobProcedureSystemBytes(t, res.SideEffectFHIR[0]); got != "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets" {
			t.Fatalf("EOB procedure system = %q, want HCPCS (the half-fix CPT-lock is forbidden)", got)
		}
	})

	t.Run("unrecognized procedure system: forwards, NO EOB (honest soft fallback)", func(t *testing.T) {
		// An order whose only coding is a non-{CPT,HCPCS} system yields no product coding → honest
		// no-EOB (the relay still completes). This preserves the soft fallback the allowlist guarantees.
		other := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
			{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://snomed.info/sct","code":"12345","display":"x"}]}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
		]}`)
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-OTHER"}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-other", "PCI-1", other)
		if err != nil {
			t.Fatalf("unrecognized-system submit: unexpected error: %v", err)
		}
		if res.Status != 0 || !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("unrecognized-system submit must forward verbatim; status=%d", res.Status)
		}
		if len(res.SideEffectFHIR) != 0 || res.Commit != nil {
			t.Fatalf("unrecognized-system submit must emit NO EOB (soft); got side-effects=%d", len(res.SideEffectFHIR))
		}
	})
}

// TestNativePAS_EOBSystemTracksOrder is the DEF-14 no-wrong-EOB guard: the EOB's
// procedure system must equal the ORDER's system for every recognized procedure
// system — never the hardcoded CPT. A regression that re-hardcodes the system (the
// half-fix the DEF-14 entry calls worse than the honest no-EOB) fails the HCPCS row.
func TestNativePAS_EOBSystemTracksOrder(t *testing.T) {
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
			srv := stubPartnerSrv(t, http.StatusOK, body)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", NewStubHolderData(), fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim", "corr-"+tc.name, "PCI-1", bundle)
			if err != nil || res.Status != 0 || len(res.SideEffectFHIR) != 1 {
				t.Fatalf("%s: want forward + 1 EOB; err=%v status=%d sideeffects=%d", tc.name, err, res.Status, len(res.SideEffectFHIR))
			}
			if got := eobProcedureSystemBytes(t, res.SideEffectFHIR[0]); got != tc.system {
				t.Fatalf("%s: EOB system = %q, want the ORDER's system %q (no hardcode)", tc.name, got, tc.system)
			}
		})
	}
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

// TestNativeUpdate_ApprovedFinalizes is the CONFORMANT native update responder
// (handlePASClaimUpdateNative, the pas-claim-update leg) — the Phase-B analog of
// TestNativePAS_Update. It drives a CONFORMANT amended re-POST bundle (built by
// shnsdk.BuildConformantClaimUpdateBundle; related[prior] read via parseConformantPASUpdateFacts,
// NOT the strict ParseClaimBundle the minimized leg uses) through the native-forward path and
// proves the shadow FinalizeClaimUpdate survived the convergence: approved → verbatim + Finalize
// Commit + armed Rollback; partner-500-after-Begin → 502 + Rollback (no strand); no prior pend →
// 409; re-pend / non-approved → 422 + Rollback. NO EOB on the update leg.
func TestNativeUpdate_ApprovedFinalizes(t *testing.T) {
	// The conformant update bundle's Claim.related[0].claim.identifier.value is the original
	// submit's correlation id (convergence-pas-submit-0001), which is the BeginClaimUpdate key.
	bundle := originatorBuiltConformantUpdateBundle(t)
	const origCorr = "convergence-pas-submit-0001"
	const pci = "PCI-CONF-UPD"

	seedPended := func() *StubHolderData {
		s := NewStubHolderData()
		_ = s.RecordPendedClaim(pci, origCorr)
		return s
	}

	t.Run("approved -> verbatim + Finalize, Rollback armed, no EOB", func(t *testing.T) {
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved conformant update: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(res.ResponseFHIR, body) || res.Commit == nil || res.Rollback == nil {
			t.Fatalf("approved conformant update must forward verbatim + Finalize Commit + armed Rollback")
		}
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("conformant update leg must emit NO EOB; got %d", len(res.SideEffectFHIR))
		}
	})

	t.Run("partner 500 AFTER Begin -> 502 WITH Rollback (no strand)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusInternalServerError, []byte(`boom`))
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err != nil || res.Status != http.StatusBadGateway {
			t.Fatalf("want 502, got status=%d err=%v", res.Status, err)
		}
		if res.Rollback == nil {
			t.Fatalf("CRITICAL: a post-Begin partner failure MUST carry Rollback or the claim strands")
		}
	})

	t.Run("no prior pend -> 409 (derived-ledger fail-safe)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusOK, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`))
		s := NewStubHolderData() // NOT seeded
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, _ := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if res.Status != http.StatusConflict {
			t.Fatalf("divergence/no-pend must be 409, got %d", res.Status)
		}
	})

	t.Run("still insufficient (denied A3) -> 422 + Rollback", func(t *testing.T) {
		denied := loadDeniedClaimResponseBytes(t)
		srv := stubPartnerSrv(t, http.StatusOK, denied)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, _ := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if res.Status != http.StatusUnprocessableEntity || res.Rollback == nil {
			t.Fatalf("non-approved conformant update is 422 + Rollback (defensive parity), got status=%d rollback=%v", res.Status, res.Rollback != nil)
		}
	})
}
