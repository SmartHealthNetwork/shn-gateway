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
		body = fixturePASResponse(t, body, true)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		store := newCensusSoR()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", store, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved conformant submit: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("Response not forwarded verbatim")
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
		body = fixturePASResponse(t, body, true)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-conf", "PCI-1", conformant)
		if err != nil || res.Status != 0 {
			t.Fatalf("pended conformant submit: err=%v status=%d", err, res.Status)
		}
		if !bytes.Equal(responseBytes(res), body) {
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
		body = fixturePASResponse(t, body, true)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
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
		body = fixturePASResponse(t, body, true)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim", "corr-other", "PCI-1", other)
		if err != nil {
			t.Fatalf("unrecognized-system submit: unexpected error: %v", err)
		}
		if res.Status != 0 || !bytes.Equal(responseBytes(res), body) {
			t.Fatalf("unrecognized-system submit must forward verbatim; status=%d", res.Status)
		}
		// No EOB, but the payer's decision is still recorded: the soft fallback
		// drops the projection this gateway could not state, never the fact that
		// the payer decided.
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("unrecognized-system submit must emit NO EOB (soft); got side-effects=%d", len(res.SideEffectFHIR))
		}
		if res.Commit == nil {
			t.Fatal("the payer's decision must still be recorded")
		}
	})
}

// serviceRequestSubmitBundle builds a single-shot conformant ServiceRequest $submit bundle via the
// SDK (the same builder the originator uses), with InfoChanged toggled — so a row about what a
// requester may send drives the REAL built bytes rather than a hand-written approximation.
func serviceRequestSubmitBundle(t *testing.T, infoChanged bool) []byte {
	t.Helper()
	sr := []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
	b, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Coverage: testMemberCoverage("MBR-COVERED"),
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
			body = fixturePASResponse(t, body, true)
			srv := stubPartnerSrv(t, http.StatusOK, body)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
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

	seedPended := func() *censusSoR {
		s := newCensusSoR()
		_ = s.RecordPendedClaim(pci, origCorr)
		return s
	}

	t.Run("approved -> verbatim + Finalize, Rollback armed, no EOB", func(t *testing.T) {
		body := []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`)
		body = fixturePASResponse(t, body, true)
		srv := stubPartnerSrv(t, http.StatusOK, body)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("approved conformant update: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(responseBytes(res), body) || res.Commit == nil || res.Rollback == nil {
			t.Fatalf("approved conformant update must forward verbatim + Finalize Commit + armed Rollback")
		}
		if len(res.SideEffectFHIR) != 0 {
			t.Fatalf("conformant update leg must emit NO EOB; got %d", len(res.SideEffectFHIR))
		}
	})

	// Superseded assertion (pre-relay-recipient-response): a post-Begin partner non-2xx no
	// longer collapses to a generic 502 — its REAL status + body relay verbatim
	// (2026-07-15). The Rollback guard (no strand) is unchanged and still the point of this case.
	t.Run("partner 500 AFTER Begin -> relayed verbatim WITH Rollback (no strand)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusInternalServerError, []byte(`boom`))
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err != nil || res.Status != http.StatusInternalServerError {
			t.Fatalf("want 500 (relayed verbatim), got status=%d err=%v", res.Status, err)
		}
		if string(responseBytes(res)) != "boom" {
			t.Fatalf("want the partner's body relayed verbatim, got %q", responseBytes(res))
		}
		if res.Rollback == nil {
			t.Fatalf("CRITICAL: a post-Begin partner failure MUST carry Rollback or the claim strands")
		}
	})

	// A TRUE no-response fault (dial failure, distinct from the relayed-partner-500 case above)
	// must still carry Rollback: n.post's fault branch returns (nil, LegResult{}, err), and
	// handlePASClaimUpdateNative's `if err != nil { return LegResult{Rollback: release}, err }`
	// (nativepas.go ~line 51) is the ONLY place that attaches Rollback on that path — post()
	// itself never does. If that branch regressed to a bare `return LegResult{}, err`, this
	// subtest's Rollback assertion goes red.
	t.Run("partner UNREACHABLE after Begin -> error return still carries Rollback (no strand)", func(t *testing.T) {
		s := seedPended()
		n := NewNativeResponder(&http.Client{}, "http://127.0.0.1:1", "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err == nil {
			t.Fatal("a true no-response fault must surface as an error return, not a relayable status")
		}
		if res.Rollback == nil {
			t.Fatalf("CRITICAL: a post-Begin true fault MUST carry Rollback or the claim strands")
		}
	})

	t.Run("no prior pend -> 409 (derived-ledger fail-safe)", func(t *testing.T) {
		srv := stubPartnerSrv(t, http.StatusOK, fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1"}`), true))
		s := newCensusSoR() // NOT seeded
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, _ := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if res.Status != http.StatusConflict {
			t.Fatalf("divergence/no-pend must be 409, got %d", res.Status)
		}
	})

	// A payer's terminal denial on the update leg is the PAYER'S answer: it is
	// relayed, and the authorization is decided so nothing re-pends it. The rows
	// that pin the relay and the ledger effect in full are in
	// nativepas_relay_test.go; this one keeps the case beside its siblings.
	t.Run("terminal denial -> relayed, decided", func(t *testing.T) {
		denied := fixturePASResponse(t, loadDeniedClaimResponseBytes(t), true)
		srv := stubPartnerSrv(t, http.StatusOK, denied)
		s := seedPended()
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
		res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, bundle)
		if err != nil || res.Status != 0 {
			t.Fatalf("a payer denial is an answer, not this gateway's 422: err=%v status=%d msg=%s", err, res.Status, res.Message)
		}
		if !bytes.Equal(responseBytes(res), denied) || !res.ResponseRelayed() {
			t.Fatal("the payer's denial must reach the requester unchanged")
		}
		if res.Rollback == nil {
			t.Fatal("Rollback stays armed until the response leg is sealed")
		}
	})
}

// TestNativeUpdate_AmendAfterResolution is the update leg's row for the UC05-class
// amendment that lands AFTER the reference payer's pend-resolution timer already
// flipped the prior pend to A1. The real payer's answer (live-captured,
// testdata/br-payer/pas-update-response-amend-after-resolution.json) is a re-pend
// on the SAME ClaimResponse id with outcome "complete" (never reset by
// persistUpdatePath) + reviewAction A4 + the pended-resolution tag, and no Task.
//
// That shape used to be read as a signal to poll a rescheduled timer. It is read
// as what it is: the payer's answer to this amendment, relayed exactly, with the
// authorization returned to pended so a later amendment still binds. A decision
// that lands afterwards reaches the requester through the inquiry it performs.
func TestNativeUpdate_AmendAfterResolution(t *testing.T) {
	load := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("testdata", "br-payer", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b
	}
	const origCorr = "convergence-pas-submit-0001"
	const pci = "PCI-CONF-UPD"
	// Retain historical decision content in explicitly synthetic closed graphs.
	// These fixtures are not evidence of historical payer graph conformance.
	afterTimer := fixturePASResponse(t, load("pas-update-response-amend-after-resolution.json"), true)

	for _, row := range []struct {
		name   string
		bundle func() []byte
	}{
		{"an amendment that asked for re-evaluation", func() []byte { return originatorBuiltConformantUpdateBundleProfile(t, true) }},
		{"a carry-forward amendment", func() []byte {
			return stripInfoChangedExtension(t, originatorBuiltConformantUpdateBundleProfile(t, true))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			var gets int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				if r.Method == http.MethodGet {
					gets++
				}
				_, _ = w.Write(afterTimer)
			}))
			defer srv.Close()
			s := newCensusSoR()
			_ = s.RecordPendedClaim(pci, origCorr)
			n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", s, fixedClock)
			res, err := n.Handle(context.Background(), "pas-claim-update", "corr-1", pci, row.bundle())
			if err != nil || res.Status != 0 {
				t.Fatalf("amend-after-resolution: err=%v status=%d msg=%s", err, res.Status, res.Message)
			}
			if !bytes.Equal(responseBytes(res), afterTimer) || !res.ResponseRelayed() {
				t.Fatal("the payer's re-pend must reach the requester unchanged")
			}
			if gets != 0 {
				t.Fatalf("the leg read the payer's ClaimResponse %d time(s); nothing polls", gets)
			}
			if res.Commit == nil {
				t.Fatal("a re-pend must return the authorization to pended")
			}
			if err := res.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if rec, found, rerr := s.PendRecordOf(pci, origCorr); rerr != nil || !found || rec.State != PendStatePended {
				t.Fatalf("the authorization did not return to pended (found=%v state=%q err=%v)", found, rec.State, rerr)
			}
			if len(res.SideEffectFHIR) != 0 {
				t.Fatalf("update leg must emit NO EOB; got %d", len(res.SideEffectFHIR))
			}
		})
	}
}
