package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// fixedClock is the deterministic clock the native-PAS tests inject for the
// gateway-projected EOB `created`.
var fixedClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }

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

	t.Run("HCPCS-coded ServiceRequest: forwards (no 400), no EOB (soft)", func(t *testing.T) {
		// A real partner may code the order in HCPCS (e.g. E0424 home-oxygen, L8000) rather than the
		// AMA CPT system ParseServiceRequestCPT matches. The conformant submit native-forward MUST
		// RELAY such a request (it is valid) — it must NOT 400 on CPT parseability (the EOB is a Store
		// projection, not a relay gate). The EOB is simply skipped when no AMA CPT is present
		// ("soft"; the HCPCS→EOB mapping is a later carry-forward). A minimal
		// conformant bundle whose ServiceRequest carries an HCPCS code → parseConformantPASSubjects
		// binds it, ParseServiceRequestCPT returns "" → no EOB, but the relay still completes.
		hcpcs := []byte(`{"resourceType":"Bundle","type":"collection","entry":[
			{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"https://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0424"}]}}},
			{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
			{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
		]}`)
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-HCPCS"}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-hcpcs", "PCI-1", hcpcs)
		if err != nil {
			t.Fatalf("HCPCS conformant submit: unexpected error (must relay, not 500): %v", err)
		}
		if res.Status != 0 {
			t.Fatalf("HCPCS conformant submit must FORWARD (status 0), got %d msg=%s (best-effort CPT must not 400)", res.Status, res.Message)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("HCPCS submit response not relayed verbatim")
		}
		if len(res.SideEffectFHIR) != 0 || res.Commit != nil {
			t.Fatalf("HCPCS submit (no AMA CPT) must emit NO EOB (soft); got side-effects=%d commit=%v", len(res.SideEffectFHIR), res.Commit != nil)
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
