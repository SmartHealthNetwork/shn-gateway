package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// readConformantGolden reads a conformant request golden from the monorepo
// (../../testdata/golden/conformant). The byte-match against these goldens is a
// monorepo gate; the published standalone gateway module has no ../../testdata, so the
// reads skip there (mirrors the SDK's readConformantGolden helper). ErrNotExist → t.Skip.
func readConformantGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/golden/conformant/" + name)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("conformant golden %q lives in the monorepo (../../testdata); skipped in the standalone gateway module", name)
	}
	if err != nil {
		t.Fatalf("read conformant golden %q: %v", name, err)
	}
	return b
}

// loadPASGolden loads the committed br-payer conformant $submit bundle and rebinds it onto member.
func loadPASGolden(t *testing.T, member string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/br-payer/pas-submit-request.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return rebindPASPatient(t, raw, member)
}

// rebindPASPatient sets the Patient.id STRUCTURALLY (the golden is pretty-printed `"id": "…"` with
// a space, so a raw string-replace would no-op), then string-replaces every Patient/<oldID>
// reference on the freshly-marshaled (spacing-normalized) JSON. IDENTICAL to the tworilive copy
// — different package, same logic, so the same golden yields the same bundle everywhere.
func rebindPASPatient(t *testing.T, bundleJSON []byte, newID string) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	entries, _ := b["entry"].([]any)
	oldID := ""
	for _, e := range entries {
		r, _ := e.(map[string]any)["resource"].(map[string]any)
		if r != nil && r["resourceType"] == "Patient" {
			oldID, _ = r["id"].(string)
			r["id"] = newID // structural set — spacing-proof
		}
	}
	if oldID == "" {
		t.Fatal("golden has no Patient resource to rebind")
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal rebind: %v", err)
	}
	return bytes.ReplaceAll(out, []byte("Patient/"+oldID), []byte("Patient/"+newID))
}

// TestTask0_ConformantGoldensBind is the FIRST oracle of the PA-contract convergence:
// the hand-derived demo-persona conformant CRD order-select + PAS $submit goldens that the
// Originator must learn to reproduce MUST subject-bind through the conformant parsers
// the payer-side already runs (conformantCRDBind / parseConformantPASSubjects). These goldens are
// the byte-pinned target the SDK builders byte-match against; if they don't bind here,
// nothing downstream can. (The SECOND oracle — make validate on the SHN-produced resources — is run
// out-of-band.)
func TestTask0_ConformantGoldensBind(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, found := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !found {
		t.Fatal("MBR-COVERED not resolvable in StubHolderData")
	}

	// --- CRD order-select golden binds (the shape the Originator reproduces at the CRD legs). ---
	crdGolden := readConformantGolden(t, "crd-order-select-request.json")
	srJSON, covJSON, status, msg := g.conformantCRDBind(crdGolden, pci)
	if status != 0 {
		t.Fatalf("conformant CRD golden rejected: status=%d (%s), want 0", status, msg)
	}
	if len(srJSON) == 0 || len(covJSON) == 0 {
		t.Fatalf("CRD bind must return SR + Coverage for validation; srJSON=%d covJSON=%d", len(srJSON), len(covJSON))
	}

	// --- PAS $submit golden binds (the shape the Originator reproduces at the PAS submit sites). ---
	pasGolden := readConformantGolden(t, "pas-submit-request.json")
	s, status, msg := parseConformantPASSubjects(pasGolden)
	if status != 0 {
		t.Fatalf("conformant PAS golden rejected: status=%d (%s), want 0", status, msg)
	}
	if s.member != "MBR-COVERED" {
		t.Fatalf("PAS golden member = %q, want MBR-COVERED", s.member)
	}
	// The demo-persona golden carries an answered QR (R-5: tolerated, bound for consistency).
	if s.qrJSON == nil {
		t.Fatal("demo-persona PAS golden should carry an answered QuestionnaireResponse")
	}
}

// originatorBuiltConformantBundle builds the LEAN conformant $submit bundle the way the
// Originator does — via the additive shnsdk.BuildConformantClaimBundle, demo
// persona only (Linda Johansson / MBR-COVERED, CPT 72148, M51.16), no br-payer foreign
// seed. The QR's qr-context refs point at the bundle-local Coverage/SR (internally
// consistent).
func originatorBuiltConformantBundle(t *testing.T, member string) []byte {
	t.Helper()
	ref := "Patient/" + member
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	qrJSON, err := shnsdk.FillQuestionnaire(shnsdk.SandboxLumbarQuestionnaire(), shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		OrderRef:    "ServiceRequest/convergence-sr",
		Authored:    created,
	})
	if err != nil {
		t.Fatalf("FillQuestionnaire: %v", err)
	}
	got, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR:          qrJSON,
		SR:          srJSON,
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		Corr:        "convergence-pas-submit-0001",
		Created:     created,
		Payer:       shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimBundle: %v", err)
	}
	return got
}

// TestPasMemberFromRef covers the tolerant member extractor used by the payer-side bind:
// a relative ref and an absolute fullUrl both yield the bare id; a ref with no Patient/
// segment is returned unchanged (so ResolvePatient fails closed → unknown member).
func TestPasMemberFromRef(t *testing.T) {
	cases := map[string]string{
		"Patient/MBR-COVERED":                          "MBR-COVERED",
		"https://shn.example/fhir/Patient/MBR-COVERED": "MBR-COVERED",
		"urn:uuid:no-patient-segment":                  "urn:uuid:no-patient-segment",
	}
	for in, want := range cases {
		if got := pasMemberFromRef(in); got != want {
			t.Errorf("pasMemberFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseConformantPASSubjects_AbsoluteRefs proves the br-payer-targeting lane (provider-data)
// bundle — ABSOLUTIZED refs (ContainedInsurer+AbsoluteRefs, so a real Da Vinci payer resolves
// them) — still binds at the SHN payer-gw: the member resolves from the absolute fullUrl refs and
// the patient-consistency fence holds. Without the tolerant extractor this 400s "unknown member".
func TestParseConformantPASSubjects_AbsoluteRefs(t *testing.T) {
	const member = "MBR-COVERED"
	ref := "Patient/" + member
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	qrJSON, err := shnsdk.FillQuestionnaire(shnsdk.SandboxLumbarQuestionnaire(), shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		OrderRef:    "ServiceRequest/convergence-sr",
		Authored:    created,
	})
	if err != nil {
		t.Fatalf("FillQuestionnaire: %v", err)
	}
	got, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR:               qrJSON,
		SR:               srJSON,
		PatientRef:       ref,
		CoverageRef:      "Coverage/convergence-coverage",
		Corr:             "convergence-pas-submit-0001",
		Created:          created,
		ContainedInsurer: true,
		AbsoluteRefs:     true,
		Payer:            shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimBundle: %v", err)
	}
	s, status, msg := parseConformantPASSubjects(got)
	if status != 0 || s.member != member {
		t.Fatalf("absolute-ref br-payer bundle rejected by payer-gw bind: status=%d (%s) member=%q, want %q", status, msg, s.member, member)
	}
}

// TestParseConformantPASSubjects_AcceptsOriginatorBuilt: the payer-side bind accepts the
// LEAN conformant $submit bundle the Originator builds (shnsdk.BuildConformantClaimBundle)
// — it three-way subject-binds to the member, exactly like the golden. This is the
// builder↔parser contract: if the Originator-built bytes don't bind here, nothing
// downstream (the sandbox / a real br-payer) sees them.
func TestParseConformantPASSubjects_AcceptsOriginatorBuilt(t *testing.T) {
	got := originatorBuiltConformantBundle(t, "MBR-COVERED")
	s, status, msg := parseConformantPASSubjects(got)
	if status != 0 || s.member != "MBR-COVERED" {
		t.Fatalf("parseConformantPASSubjects rejected Originator-built bundle: %d %s member=%q", status, msg, s.member)
	}
	if s.qrJSON == nil {
		t.Fatal("Originator-built conformant bundle should carry an answered QuestionnaireResponse")
	}
}

// TestSandbox_PASClaimNative_ApprovesOriginatorBuilt: the LEAN Originator-built bundle
// also ADJUDICATES through the sandbox pas-claim responder (the QR drives an
// approval), proving the lean shape is end-to-end usable, not just bind-acceptable.
func TestSandbox_PASClaimNative_ApprovesOriginatorBuilt(t *testing.T) {
	s := newSandboxResponderForTest(t)
	res, err := s.Handle(context.Background(), "pas-claim", "corr-orig", "pci-covered",
		originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if err != nil {
		t.Fatalf("sandbox pas-claim: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("status=%d msg=%s", res.Status, res.Message)
	}
	parsed, err := shnsdk.ParseClaimResponse(res.ResponseFHIR)
	if err != nil || parsed.Outcome != "approved" || parsed.PreAuthRef == "" {
		t.Fatalf("want approved + ref, got %+v err=%v", parsed, err)
	}
}

// nonLumbarConformantBundle builds a LEAN conformant $submit bundle IDENTICAL to
// originatorBuiltConformantBundle EXCEPT the ServiceRequest carries a NON-lumbar
// procedure (CPT 29881, knee arthroscopy). The QR is still the lumbar approval QR,
// so the sandbox (which adjudicates on the QR, never the SR's CPT) still APPROVES —
// which is the whole point: the resulting EOB's productOrService display MUST flow
// from THIS ServiceRequest, not the hardcoded lumbar persona (DEF-14, FR-28).
func nonLumbarConformantBundle(t *testing.T, member string) []byte {
	t.Helper()
	ref := "Patient/" + member
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srJSON, err := shnsdk.BuildServiceRequest("29881", "Arthroscopy, knee, surgical, with meniscectomy", "M23.2", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	qrJSON, err := shnsdk.FillQuestionnaire(shnsdk.SandboxLumbarQuestionnaire(), shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		OrderRef:    "ServiceRequest/convergence-sr",
		Authored:    created,
	})
	if err != nil {
		t.Fatalf("FillQuestionnaire: %v", err)
	}
	got, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR:          qrJSON,
		SR:          srJSON,
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		Corr:        "followups-knee-0001",
		Created:     created,
		Payer:       shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimBundle: %v", err)
	}
	return got
}

// TestSandbox_PASClaim_EOBDisplayFromServiceRequest (DEF-14, FR-28): the approved-path
// EOB's item[0].productOrService.coding[0].display must be SOURCED from the request's
// ServiceRequest, not the hardcoded "MRI lumbar spine w/o contrast" persona string.
// The sandbox adjudicates on the QR (lumbar approval), so a knee SR (29881) + the
// lumbar QR still APPROVES — the EOB then carries the knee CPT AND the knee display.
// This is the load-bearing proof of the de-personalization: all live personas are 72148
// lumbar, so only a non-lumbar SR can distinguish "display flows from the SR" from
// "display is the hardcoded lumbar literal".
func TestSandbox_PASClaim_EOBDisplayFromServiceRequest(t *testing.T) {
	s := newSandboxResponderForTest(t)
	res, err := s.Handle(context.Background(), "pas-claim", "corr-knee", "pci-covered",
		nonLumbarConformantBundle(t, "MBR-COVERED"))
	if err != nil {
		t.Fatalf("sandbox pas-claim: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("status=%d msg=%s", res.Status, res.Message)
	}
	if len(res.SideEffectFHIR) == 0 {
		t.Fatal("approved pas-claim must emit the EOB side-effect")
	}
	var eob struct {
		Item []struct {
			ProductOrService struct {
				Coding []struct {
					Code    string `json:"code"`
					Display string `json:"display"`
				} `json:"coding"`
			} `json:"productOrService"`
		} `json:"item"`
	}
	if err := json.Unmarshal(res.SideEffectFHIR[0], &eob); err != nil {
		t.Fatalf("parse EOB: %v", err)
	}
	got := eob.Item[0].ProductOrService.Coding[0]
	if got.Code != "29881" {
		t.Fatalf("EOB CPT = %q, want 29881", got.Code)
	}
	if got.Display == "MRI lumbar spine w/o contrast" {
		t.Fatal("DEF-14 regression: EOB display is the hardcoded lumbar persona, not the actual service")
	}
	if got.Display != "Arthroscopy, knee, surgical, with meniscectomy" {
		t.Fatalf("EOB display = %q, want the knee service display", got.Display)
	}
}

// --- the conformant amended re-POST golden -------

// TestTask0B_ConformantUpdateGoldenBinds is the update-golden oracle, pointed at the
// PRODUCTION parseConformantPASUpdateFacts (the throwaway task0bUpdateFacts spike
// helper was promoted into pas_native.go and deleted). The hand-derived demo-persona conformant amended
// re-POST golden (UC-04 operative-DR variant) MUST (a) subject-bind through the SAME tolerant
// parser the conformant submit leg runs (parseConformantPASSubjects → member==MBR-COVERED) AND
// (b) satisfy the FR-32 Provenance/DR facts the inbound gate enforces (mirror of
// payer.go:393-424): Claim.related[prior] present, Provenance has ≥1 agent, Provenance targets the
// EXACT supplemental DiagnosticReport by id. (The make-validate oracle is run out-of-band.)
func TestTask0B_ConformantUpdateGoldenBinds(t *testing.T) {
	golden := readConformantGolden(t, "pas-update-request.json")

	// (a) subject-binds through the conformant leg's tolerant parser (tolerates the full
	// entry set: Patient, Coverage, Org, AND the new Provenance + DiagnosticReport).
	s, status, msg := parseConformantPASSubjects(golden)
	if status != 0 {
		t.Fatalf("conformant update golden rejected by parseConformantPASSubjects: status=%d (%s), want 0", status, msg)
	}
	if s.member != "MBR-COVERED" {
		t.Fatalf("update golden member = %q, want MBR-COVERED", s.member)
	}
	if !s.hasDR {
		t.Fatal("update golden (DR variant) must carry a DiagnosticReport (parser must see hasDR)")
	}

	// (b) the FR-21 (Claim.related[prior]) + FR-32 (Provenance/DR) facts the inbound gate enforces,
	// now extracted by the PRODUCTION parseConformantPASUpdateFacts (the inbound gate).
	f, status, msg := parseConformantPASUpdateFacts(golden)
	if status != 0 {
		t.Fatalf("parseConformantPASUpdateFacts rejected the golden: status=%d (%s), want 0", status, msg)
	}
	if f.relatedClaim == "" {
		t.Fatal("FR-21: Claim.related[prior] must be non-empty (the amendment's distinguishing field)")
	}
	if f.relatedClaim != "convergence-pas-submit-0001" {
		t.Fatalf("Claim.related[prior] = %q, want the original submit corr convergence-pas-submit-0001", f.relatedClaim)
	}
	if len(f.provenanceAgents) == 0 {
		t.Fatal("FR-32: Provenance must name ≥1 agent")
	}
	if f.provenanceAgents[0] == "" {
		t.Fatal("FR-32: Provenance.agent[0].who.reference must be non-empty")
	}
	if !f.hasDR || f.diagnosticReportID == "" {
		t.Fatal("FR-32 (DR variant): the supplemental DiagnosticReport must carry an id")
	}
	// The DR-variant FR-32 arm (payer.go:402-407): Provenance.target references DiagnosticReport/<id>.
	wantTarget := "DiagnosticReport/" + f.diagnosticReportID
	targeted := false
	for _, ref := range f.provenanceTargets {
		if ref == wantTarget {
			targeted = true
			break
		}
	}
	if !targeted {
		t.Fatalf("FR-32: Provenance.target must reference the supplemental %s; targets=%v", wantTarget, f.provenanceTargets)
	}
}

// --- the conformant update inbound bind (conformantPASUpdateBind) + FR-32 rejection set ---

// updateGatewayForTest builds a Gateway whose SoR is the stub holder data (resolves the demo
// personas), enough to drive conformantPASUpdateBind's subject-bind + FR-32 arms.
func updateGatewayForTest(t *testing.T) (*Gateway, string) {
	t.Helper()
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, found := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !found {
		t.Fatal("MBR-COVERED not resolvable in StubHolderData")
	}
	return g, pci
}

// mutateBundleEntries is a small JSON-surgery helper: it unmarshals the conformant Bundle,
// hands each entry resource (as a map) to fn, and re-marshals. fn mutates in place.
func mutateBundleEntries(t *testing.T, bundleJSON []byte, fn func(res map[string]any)) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatalf("mutateBundleEntries: unmarshal: %v", err)
	}
	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		res, _ := entry["resource"].(map[string]any)
		if res != nil {
			fn(res)
		}
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("mutateBundleEntries: marshal: %v", err)
	}
	return out
}

// dropUpdateEntry removes every entry whose resourceType == rt from the conformant update bundle.
func dropUpdateEntry(t *testing.T, bundleJSON []byte, rt string) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		t.Fatalf("dropUpdateEntry: unmarshal: %v", err)
	}
	entries, _ := b["entry"].([]any)
	kept := make([]any, 0, len(entries))
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res != nil && res["resourceType"] == rt {
			continue
		}
		kept = append(kept, e)
	}
	b["entry"] = kept
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("dropUpdateEntry: marshal: %v", err)
	}
	return out
}

// TestConformantPASUpdateBind_FR32RejectionSet drives the golden's bind through
// conformantPASUpdateBind and asserts each FR-32 arm + the wrong-patient arm 403s — the
// "valid − one mutation → reject" discipline. Each row mutates exactly the field its arm checks,
// so a row stays RED if you neuter that arm (non-vacuous).
func TestConformantPASUpdateBind_FR32RejectionSet(t *testing.T) {
	good := readConformantGolden(t, "pas-update-request.json")
	cases := []struct {
		name       string
		mutate     func(*testing.T, []byte) []byte
		tokSubject func(*Gateway, string) string // returns the token subject; default = the bound pci
		wantStatus int
	}{
		{
			name: "missing-provenance",
			mutate: func(t *testing.T, b []byte) []byte {
				return dropUpdateEntry(t, b, "Provenance")
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "provenance-no-agent",
			mutate: func(t *testing.T, b []byte) []byte {
				return mutateBundleEntries(t, b, func(res map[string]any) {
					if res["resourceType"] == "Provenance" {
						delete(res, "agent")
					}
				})
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "provenance-wrong-target",
			mutate: func(t *testing.T, b []byte) []byte {
				return mutateBundleEntries(t, b, func(res map[string]any) {
					if res["resourceType"] == "Provenance" {
						res["target"] = []any{map[string]any{"reference": "DiagnosticReport/bogus"}}
					}
				})
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong-patient",
			// Rebind the bundle onto MBR-NOTCOVERED (a DIFFERENT persona that DOES resolve) but keep
			// the token subject pinned to MBR-COVERED's pci → conformantPASBind sees pci != tokSubject
			// → 403. (Rebinding onto a non-persona member would 400 "unknown member", not the
			// authority 403 we want to exercise here.)
			mutate: func(t *testing.T, b []byte) []byte {
				return rebindPASPatient(t, b, "MBR-NOTCOVERED")
			},
			tokSubject: func(g *Gateway, defaultPCI string) string { return defaultPCI },
			wantStatus: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, pci := updateGatewayForTest(t)
			tokSubject := pci
			if tc.tokSubject != nil {
				tokSubject = tc.tokSubject(g, pci)
			}
			_, status, msg := g.conformantPASUpdateBind(tc.mutate(t, append([]byte(nil), good...)), tokSubject)
			if status != tc.wantStatus {
				t.Fatalf("%s: got %d (%s), want %d", tc.name, status, msg, tc.wantStatus)
			}
		})
	}

	// Control: the unmutated golden binds clean (status 0). Without this, a bind that 403'd
	// everything would pass the rejection set vacuously.
	t.Run("control-binds", func(t *testing.T) {
		g, pci := updateGatewayForTest(t)
		if _, status, msg := g.conformantPASUpdateBind(append([]byte(nil), good...), pci); status != 0 {
			t.Fatalf("unmutated conformant update golden rejected: status=%d (%s), want 0", status, msg)
		}
	})
}

func TestParseConformantPASSubjects_Golden(t *testing.T) {
	s, status, msg := parseConformantPASSubjects(loadPASGolden(t, "MBR-COVERED"))
	if status != 0 {
		t.Fatalf("conformant golden rejected: %d %s", status, msg)
	}
	if s.member != "MBR-COVERED" {
		t.Fatalf("member = %q, want MBR-COVERED", s.member)
	}
	// The golden carries NO QuestionnaireResponse (R-5) — bind must accept that.
	if s.qrJSON != nil {
		t.Logf("golden carried a QR (qrJSON set) — fine, bound for consistency")
	}
}

func TestParseConformantPASSubjects_MissingCoverage(t *testing.T) {
	// A bundle with Claim+SR but no Coverage → 400 (R-4: Coverage required on this leg).
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}}
	]}`)
	_, status, _ := parseConformantPASSubjects(bundle)
	if status != 400 {
		t.Fatalf("missing Coverage status = %d, want 400", status)
	}
}

func TestParseConformantPASSubjects_CoverageDivergence(t *testing.T) {
	// Coverage.beneficiary points at a different patient → 403 (R-4 smuggling vector closed).
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-OTHER"}}}
	]}`)
	_, status, _ := parseConformantPASSubjects(bundle)
	if status != 403 {
		t.Fatalf("Coverage divergence status = %d, want 403", status)
	}
}

// conformantPASBundleWithQR builds a minimal CONFORMANT PAS bundle (Claim + SR + Coverage + Patient
// + a real answered QR) for the given member — the hermetic-test shape (the sandbox needs the QR).
func conformantPASBundleWithQR(t *testing.T, member string) []byte {
	t.Helper()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ref := "Patient/" + member
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	q := shnsdk.SandboxLumbarQuestionnaire()
	qrJSON, err := shnsdk.FillQuestionnaire(q, shnsdk.SandboxUC03Context(), shnsdk.QRContext{
		PatientRef: ref, CoverageRef: "Coverage/" + member, OrderRef: "ServiceRequest/sr1", Authored: now,
	})
	if err != nil {
		t.Fatalf("FillQuestionnaire: %v", err)
	}
	entries := []map[string]any{
		{"resource": map[string]any{"resourceType": "Patient", "id": member}},
		{"resource": map[string]any{"resourceType": "Coverage", "id": "cov1", "beneficiary": map[string]any{"reference": ref}}},
		{"resource": json.RawMessage(srJSON)},
		{"resource": map[string]any{"resourceType": "Claim", "patient": map[string]any{"reference": ref}}},
		{"resource": json.RawMessage(qrJSON)},
	}
	b, _ := json.Marshal(map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries})
	return b
}

func TestSandbox_PASClaimNative_Approves(t *testing.T) {
	s := newSandboxResponderForTest(t) // reuse the existing test helper
	res, err := s.Handle(context.Background(), "pas-claim", "corr1", "pci-covered",
		conformantPASBundleWithQR(t, "MBR-COVERED"))
	if err != nil {
		t.Fatalf("sandbox pas-claim: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("status=%d msg=%s", res.Status, res.Message)
	}
	parsed, err := shnsdk.ParseClaimResponse(res.ResponseFHIR)
	if err != nil || parsed.Outcome != "approved" || parsed.PreAuthRef == "" {
		t.Fatalf("want approved + ref, got %+v err=%v", parsed, err)
	}
	// Submit cell: the SANDBOX conformant case now records the FR-34
	// Patient-Access EOB side-effect on approve (NO LONGER pure relay — the native
	// FORWARD path stays pure-relay). The minimized pas-claim case keeps
	// its own EOB until it is removed; both coexist (rekey-is-net).
	if len(res.SideEffectFHIR) != 1 {
		t.Fatalf("approved sandbox pas-claim must emit 1 EOB side-effect; got side-effects=%d", len(res.SideEffectFHIR))
	}
	if res.Commit == nil {
		t.Fatal("approved sandbox pas-claim must arm a RecordEOB Commit")
	}
}

// TestSandbox_PASClaimNative_RecordsEOB (approved path): the sandbox conformant
// pas-claim case records the FR-34 Patient-Access EOB as a Store side-effect
// + Commit (the submit cell). Running the Commit makes the EOB readable via the
// Patient Access surface (EOBsForPatient). The CPT is sourced from the bundle's
// ServiceRequest (CPT 72148), mirroring the minimized pas-claim case.
func TestSandbox_PASClaimNative_RecordsEOB(t *testing.T) {
	const subjectPCI = "pci-covered"
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	s := NewSandboxResponder(adj, data, data, clock)

	res, err := s.Handle(context.Background(), "pas-claim", "corr-eob", subjectPCI,
		originatorBuiltConformantBundle(t, "MBR-COVERED"))
	if err != nil {
		t.Fatalf("sandbox pas-claim: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("status=%d msg=%s", res.Status, res.Message)
	}
	parsed, err := shnsdk.ParseClaimResponse(res.ResponseFHIR)
	if err != nil || parsed.Outcome != "approved" {
		t.Fatalf("want approved, got %+v err=%v", parsed, err)
	}
	if len(res.SideEffectFHIR) != 1 {
		t.Fatalf("want 1 EOB side-effect, got %d", len(res.SideEffectFHIR))
	}
	// The EOB carries the Claim's ServiceRequest CPT (72148), not a hardcoded constant.
	if !bytes.Contains(res.SideEffectFHIR[0], []byte(adjTestCPT)) {
		t.Fatalf("EOB did not carry the SR CPT %s:\n%s", adjTestCPT, res.SideEffectFHIR[0])
	}
	if res.Commit == nil {
		t.Fatal("approved path must arm a RecordEOB Commit")
	}
	// Before Commit: no EOB readable. After Commit: exactly one.
	if eobs, ok := data.EOBsForPatient(subjectPCI); ok && len(eobs) != 0 {
		t.Fatalf("EOB recorded before Commit ran: got %d", len(eobs))
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	eobs, ok := data.EOBsForPatient(subjectPCI)
	if !ok || len(eobs) != 1 {
		t.Fatalf("after Commit want 1 EOB for %s, ok=%v got %d", subjectPCI, ok, len(eobs))
	}
}

// conformantPASBundlePended builds a CONFORMANT $submit bundle that PENDS: a UC-04
// context QR (prior-surgery=true) with NO DiagnosticReport in the bundle → the sandbox
// adjudicator returns PASPended (priorSurgery && !hasDR). Built via the same lean
// Originator builder the approve test uses (BuildConformantClaimBundle emits no DR).
func conformantPASBundlePended(t *testing.T, member string) []byte {
	t.Helper()
	ref := "Patient/" + member
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	// UC-04 context: PriorSurgery=true → the payer pends awaiting an operative DiagnosticReport.
	qrJSON, err := shnsdk.FillQuestionnaire(shnsdk.SandboxLumbarQuestionnaire(), shnsdk.SandboxUC04Context(), shnsdk.QRContext{
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		OrderRef:    "ServiceRequest/convergence-sr",
		Authored:    created,
	})
	if err != nil {
		t.Fatalf("FillQuestionnaire: %v", err)
	}
	got, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR:          qrJSON,
		SR:          srJSON,
		PatientRef:  ref,
		CoverageRef: "Coverage/convergence-coverage",
		Corr:        "convergence-pas-pend-0001",
		Created:     created,
		Payer:       shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimBundle: %v", err)
	}
	return got
}

// TestSandbox_PASClaimNative_RecordsPendedClaim (pended path — THE PEND→UPDATE PREREQUISITE):
// the sandbox conformant pas-claim case records the pended claim via Commit
// (RecordPendedClaim, keyed by subject PCI + corrID). This is the load-bearing handoff:
// the later (still-minimized) pas-claim-update leg's BeginClaimUpdate(subjectPCI, related)
// can only claim a pended claim the submit actually recorded. The test PROVES the handoff
// genuinely: after running the submit's Commit, a BeginClaimUpdate against the SAME
// (subjectPCI, corrID) succeeds — exactly the bind UC-04/05/07's update leg performs.
func TestSandbox_PASClaimNative_RecordsPendedClaim(t *testing.T) {
	const (
		subjectPCI = "pci-uc04"
		corrID     = "corr-pend"
	)
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	s := NewSandboxResponder(adj, data, data, clock)

	res, err := s.Handle(context.Background(), "pas-claim", corrID, subjectPCI,
		conformantPASBundlePended(t, "MBR-UC04"))
	if err != nil {
		t.Fatalf("sandbox pas-claim: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("status=%d msg=%s", res.Status, res.Message)
	}
	// The response is a pended Bundle (ClaimResponse outcome "queued" + Task).
	pended, _, err := shnsdk.ParsePendedResponse(res.ResponseFHIR)
	if err != nil {
		t.Fatalf("parse pended response: %v", err)
	}
	if !pended {
		t.Fatal("want a pended response from the UC-04 (prior-surgery, no DR) bundle")
	}
	if res.Commit == nil {
		t.Fatal("pended path must arm a RecordPendedClaim Commit (the pend→update handoff)")
	}
	// Before Commit: no pended claim recorded → BeginClaimUpdate finds nothing.
	if claimed, _ := data.BeginClaimUpdate(subjectPCI, corrID); claimed {
		t.Fatal("BeginClaimUpdate succeeded before the submit Commit ran — handoff broken")
	}
	if err := res.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// After Commit: the EXACT bind the pas-claim-update leg performs
	// (BeginClaimUpdate(subjectPCI, related)) finds and claims the recorded pend.
	claimed, err := data.BeginClaimUpdate(subjectPCI, corrID)
	if err != nil {
		t.Fatalf("BeginClaimUpdate after Commit: %v", err)
	}
	if !claimed {
		t.Fatal("BeginClaimUpdate did not find the pended claim the submit recorded — pend→update handoff would 409")
	}
}

func TestSandbox_PASClaimNative_NoQR_400(t *testing.T) {
	s := newSandboxResponderForTest(t)
	// A conformant bundle the BIND accepts (no QR) but the SANDBOX can't adjudicate → 400.
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}
	]}`)
	res, _ := s.Handle(context.Background(), "pas-claim", "corr2", "pci-covered", bundle)
	if res.Status != 400 {
		t.Fatalf("no-QR sandbox status=%d, want 400", res.Status)
	}
}

// originatorBuiltConformantUpdateBundle builds a LEAN conformant amended re-POST bundle via
// shnsdk.BuildConformantClaimUpdateBundle — demo persona only (Linda Johansson / MBR-COVERED,
// CPT 72148, M51.16). The QR/DR/Provenance are read from the golden (the amended QR is not
// reproducible via a standard FillQuestionnaire call; the builder stamps ids/strips meta.profile
// idempotently). Mirrors the sdk-side conformantUpdateInputsFromGolden helper.
func originatorBuiltConformantUpdateBundle(t *testing.T) []byte {
	return originatorBuiltConformantUpdateBundleProfile(t, false)
}

// originatorBuiltConformantUpdateBundleProfile builds the same bundle; when brPayer==true it
// sets the br-payer-targeting flags (ContainedInsurer/AbsoluteRefs/PayerOrgEntry) so the refs are
// absolutized exactly as the provider-data lane produces them for a real Da Vinci payer.
func originatorBuiltConformantUpdateBundleProfile(t *testing.T, brPayer bool) []byte {
	t.Helper()
	const member = "MBR-COVERED"
	created := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	goldenBytes := readConformantGolden(t, "pas-update-request.json")
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(goldenBytes, &bundle); err != nil {
		t.Fatalf("parse conformant update golden: %v", err)
	}
	var qrJSON, drJSON, provJSON []byte
	for _, e := range bundle.Entry {
		var rt struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &rt); err != nil {
			continue
		}
		switch rt.ResourceType {
		case "QuestionnaireResponse":
			qrJSON = e.Resource
		case "DiagnosticReport":
			drJSON = e.Resource
		case "Provenance":
			provJSON = e.Resource
		}
	}
	if qrJSON == nil || drJSON == nil || provJSON == nil {
		t.Fatal("conformant update golden missing QR/DR/Provenance entry")
	}

	ref := "Patient/" + member
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	got, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR:               qrJSON,
		SR:               srJSON,
		PatientRef:       ref,
		CoverageRef:      "Coverage/convergence-coverage",
		Provenance:       provJSON,
		DiagnosticReport: drJSON,
		Corr:             "convergence-pas-update-0001",
		OriginalCorr:     "convergence-pas-submit-0001",
		Created:          created,
		ContainedInsurer: brPayer,
		AbsoluteRefs:     brPayer,
		PayerOrgEntry:    brPayer,
		Payer:            shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimUpdateBundle: %v", err)
	}
	return got
}

// TestConformantPASUpdateBind_AcceptsAbsolutizedBrPayer is the regression guard for the layer-3
// amendment 403: the br-payer-targeting lane absolutizes bundle refs (AbsoluteRefs) so a real Da
// Vinci payer resolves them, which rewrites Provenance.target to its absolute fullUrl. The
// update-bind guard's supplemental-data check (conformantPASUpdateBind) must match the absolutized
// target to the DiagnosticReport, exactly as it matches the relative form — else UC-04/06 (which
// only reach the amendment once the layer-3 A3 fix lets the initial submit pend) 403 "ClaimUpdate
// Provenance does not target the supplemental data". Same absolutization-tolerance class as
// pasMemberFromRef.
func TestConformantPASUpdateBind_AcceptsAbsolutizedBrPayer(t *testing.T) {
	g, pci := updateGatewayForTest(t)
	brPayerBundle := originatorBuiltConformantUpdateBundleProfile(t, true)
	if _, status, msg := g.conformantPASUpdateBind(brPayerBundle, pci); status != 0 {
		t.Fatalf("br-payer (absolutized) update bundle rejected: status=%d (%s), want 0", status, msg)
	}
}

// TestParseConformantPASUpdate_AcceptsOriginatorBuilt: the payer-side conformant parser accepts the
// Originator-built amended re-POST bundle (shnsdk.BuildConformantClaimUpdateBundle). This is the
// builder↔parser contract for the update leg: if the Originator-built bytes don't bind here,
// nothing downstream (the sandbox / a real br-payer) sees them. Mirrors
// TestParseConformantPASSubjects_AcceptsOriginatorBuilt for the submit leg.
func TestParseConformantPASUpdate_AcceptsOriginatorBuilt(t *testing.T) {
	got := originatorBuiltConformantUpdateBundle(t)
	s, status, msg := parseConformantPASSubjects(got)
	if status != 0 {
		t.Fatalf("parseConformantPASSubjects rejected Originator-built update bundle: %d %s", status, msg)
	}
	if s.member != "MBR-COVERED" {
		t.Fatalf("update bundle member = %q, want MBR-COVERED", s.member)
	}
}

// TestSandbox_PASClaimNative_CPTlessServiceRequestIs400 is the rejection test for the conformant
// submit cell's CPT guard (adjudicator.go ~:297): a bundle the bind accepts AND that carries a QR
// (so it passes the QR check) but whose ServiceRequest has NO CPT code → the EOB's CPT source
// (ParseServiceRequestCPT) errors → 400 via Status (NOT a 500 via the error return). Mirrors the
// minimized TestSandboxPAS_CPTlessServiceRequestIs400 (every guard ships its rejection test).
func TestSandbox_PASClaimNative_CPTlessServiceRequestIs400(t *testing.T) {
	s := newSandboxResponderForTest(t)
	bundle := []byte(`{"resourceType":"Bundle","entry":[
		{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}},
		{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"}}}
	]}`)
	res, err := s.Handle(context.Background(), "pas-claim", "corr-cptless", "pci-covered", bundle)
	if err != nil {
		t.Fatalf("unexpected error return (must be Status 400, not 500): %v", err)
	}
	if res.Status != 400 {
		t.Fatalf("want 400 (CPT-less ServiceRequest), got %d (%s)", res.Status, res.Message)
	}
}
