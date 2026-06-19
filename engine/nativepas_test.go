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

func TestNativePAS_Submit(t *testing.T) {
	claimApproved := loadTestClaimApproved(t)   // CPT 72148; shared fixture loader (adjudicator_test.go)
	claimCPTlessSR := loadTestClaimCPTlessSR(t) // SR entry kept, code stripped (adjudicator_test.go)

	t.Run("approved: verbatim + EOB carries partner preAuthRef and claim CPT", func(t *testing.T) {
		const partnerRef = "PARTNER-REF-XYZ"
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"` + partnerRef + `","preAuthPeriod":{"end":"2030-01-01"}}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		store := NewStubHolderData()
		n := NewNativeResponder(srv.Client(), srv.URL, store, fixedClock).(*nativeResponder)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-1", "PCI-1", claimApproved)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("ResponseFHIR not forwarded verbatim")
		}
		if len(res.SideEffectFHIR) != 1 {
			t.Fatalf("want 1 EOB side-effect")
		}
		if !bytes.Contains(res.SideEffectFHIR[0], []byte(partnerRef)) {
			t.Fatalf("EOB AuthNumber is not the partner preAuthRef (provenance §4):\n%s", res.SideEffectFHIR[0])
		}
		if !bytes.Contains(res.SideEffectFHIR[0], []byte("72148")) {
			t.Fatalf("EOB CPT not sourced from the Claim (provenance §4)")
		}
		if res.Commit == nil {
			t.Fatalf("approved must carry a RecordEOB Commit")
		}
	})

	t.Run("pended: verbatim Bundle + RecordPendedClaim", func(t *testing.T) {
		body := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"queued"}},{"resource":{"resourceType":"Task","status":"requested"}}]}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		store := NewStubHolderData()
		n := NewNativeResponder(srv.Client(), srv.URL, store, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-1", "PCI-1", claimApproved)
		if err != nil || res.Status != 0 {
			t.Fatalf("pended: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(res.ResponseFHIR, body) {
			t.Fatalf("pended Bundle not forwarded verbatim")
		}
		if res.Commit == nil || len(res.SideEffectFHIR) != 0 {
			t.Fatalf("pended must RecordPendedClaim and emit NO EOB")
		}
	})

	t.Run("CPT-less ServiceRequest -> 400", func(t *testing.T) {
		// SR entry present but no parseable CPT: ParseClaimBundle succeeds, then
		// ParseServiceRequestCPT errors → 400 (§1.4). A MISSING SR entry is a different
		// path (ParseClaimBundle 500) — not tested here.
		srv := stubPartnerSrv(t, http.StatusOK, []byte(`{}`))
		n := NewNativeResponder(srv.Client(), srv.URL, NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-1", "PCI-1", claimCPTlessSR)
		if err != nil || res.Status != http.StatusBadRequest {
			t.Fatalf("want 400, got status=%d err=%v", res.Status, err)
		}
	})

	t.Run("partner 500 -> 502", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusInternalServerError, []byte(`boom`))
		n := NewNativeResponder(srv.Client(), srv.URL, NewStubHolderData(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-1", "PCI-1", claimApproved)
		if err != nil || res.Status != http.StatusBadGateway {
			t.Fatalf("want 502, got status=%d err=%v", res.Status, err)
		}
	})
}

// loadTestClaimUpdate builds a PAS update Bundle carrying a RelatedClaim
// (Claim.related[0].claim.identifier.value == updateOrigCorr) via
// BuildClaimUpdateBundle, so ParseClaimBundle sets cb.RelatedClaim. The QR/DR
// content is irrelevant to the NATIVE update leg (the decision comes from the
// partner stub), so a UC-03 QR + minimal DR/Provenance suffices.
const updateOrigCorr = "test-corr-update-orig"

func loadTestClaimUpdate(t *testing.T) []byte {
	t.Helper()
	const (
		patientRef  = "Patient/MBR-COVERED"
		coverageRef = "Coverage/MBR-COVERED"
		updateCorr  = "test-corr-update"
	)
	srJSON, err := shnsdk.BuildServiceRequest(adjTestCPT, "MRI lumbar spine w/o contrast", "M51.16", patientRef)
	if err != nil {
		t.Fatalf("loadTestClaimUpdate BuildServiceRequest: %v", err)
	}
	q := shnsdk.SandboxLumbarQuestionnaire()
	qrJSON, err := shnsdk.FillQuestionnaire(q, shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef:  patientRef,
		CoverageRef: coverageRef,
		OrderRef:    "ServiceRequest/sr-mbr-covered",
		Authored:    adjTestClock,
	})
	if err != nil {
		t.Fatalf("loadTestClaimUpdate FillQuestionnaire: %v", err)
	}
	drJSON, err := shnsdk.BuildDiagnosticReport("dr-update", patientRef, adjTestCPT, "MRI lumbar spine w/o contrast")
	if err != nil {
		t.Fatalf("loadTestClaimUpdate BuildDiagnosticReport: %v", err)
	}
	provJSON, err := shnsdk.BuildProvenance("DiagnosticReport/dr-update", "Organization/provider", adjTestClock)
	if err != nil {
		t.Fatalf("loadTestClaimUpdate BuildProvenance: %v", err)
	}
	bundle, err := shnsdk.BuildClaimUpdateBundle(qrJSON, srJSON, drJSON, provJSON,
		patientRef, coverageRef, updateCorr, updateOrigCorr, adjTestClock)
	if err != nil {
		t.Fatalf("loadTestClaimUpdate BuildClaimUpdateBundle: %v", err)
	}
	return bundle
}

// relatedClaimOf extracts cb.RelatedClaim (the key Begin/Release/Finalize use).
func relatedClaimOf(t *testing.T, claim []byte) string {
	t.Helper()
	cb, err := shnsdk.ParseClaimBundle(claim)
	if err != nil {
		t.Fatalf("relatedClaimOf ParseClaimBundle: %v", err)
	}
	return cb.RelatedClaim
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

func TestNativePAS_Update(t *testing.T) {
	claim := loadTestClaimUpdate(t) // an amendment Claim carrying RelatedClaim

	seedPended := func() *StubHolderData {
		s := NewStubHolderData()
		_ = s.RecordPendedClaim("PCI-1", relatedClaimOf(t, claim))
		return s
	}

	t.Run("approved -> verbatim + Finalize, Rollback armed", func(t *testing.T) {
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-1", claim)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved update: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(res.ResponseFHIR, body) || res.Commit == nil || res.Rollback == nil {
			t.Fatalf("approved update must forward verbatim + Finalize Commit + armed Rollback (§3)")
		}
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("update leg must emit NO EOB (§3)")
		}
	})

	t.Run("partner 500 AFTER Begin -> 502 WITH Rollback (no strand)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusInternalServerError, []byte(`boom`))
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-1", claim)
		if err != nil || res.Status != http.StatusBadGateway {
			t.Fatalf("want 502, got status=%d err=%v", res.Status, err)
		}
		if res.Rollback == nil {
			t.Fatalf("CRITICAL (§3): a post-Begin partner failure MUST carry Rollback or the claim strands")
		}
	})

	t.Run("no prior pend -> 409 (derived-ledger fail-safe)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusOK, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`))
		s := NewStubHolderData() // NOT seeded
		n := NewNativeResponder(srv.Client(), srv.URL, s, fixedClock)
		res, _ := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-1", claim)
		if res.Status != http.StatusConflict {
			t.Fatalf("divergence/no-pend must be 409, got %d", res.Status)
		}
	})

	t.Run("still insufficient (denied A3) -> 422 + Rollback", func(t *testing.T) {
		denied := loadDeniedClaimResponseBytes(t)
		srv := stubPartnerSrv(t, http.StatusOK, denied)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, s, fixedClock)
		res, _ := n.Handle(context.Background(), "pas-claim-update", "corr-1", "PCI-1", claim)
		if res.Status != http.StatusUnprocessableEntity || res.Rollback == nil {
			t.Fatalf("non-approved update is 422 + Rollback (defensive parity §3), got status=%d rollback=%v", res.Status, res.Rollback != nil)
		}
	})
}
