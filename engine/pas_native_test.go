package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// readConformantGolden reads a conformant request golden from this package's
// own vendored copies (testdata/golden/conformant/, see that directory's
// README.md). It used to reach into the surrounding monorepo and t.Skip when
// the reach failed, which meant the byte-match ran only in the monorepo and
// silently vanished from the published standalone module; vendoring makes it a
// real gate in both. The copies are pinned byte-for-byte against their
// originals by the root module's
// test/conformance/gateway_vendored_golden_drift_test.go.
func readConformantGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", "conformant", name))
	if err != nil {
		t.Fatalf("read vendored conformant golden %q: %v", name, err)
	}
	return b
}

// loadPASGolden loads the committed br-payer conformant $submit bundle and rebinds it onto member.
func loadPASGolden(t *testing.T, member string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../scenariodriver/goldens/pas-submit-request.json")
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
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, _, found := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !found {
		t.Fatal("MBR-COVERED not resolvable in censusSoR")
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
	qrJSON, err := shnsdk.FillQuestionnaire(fhirseed.DemoLumbarQuestionnaire(), shnsdk.DemoLumbarContext(), shnsdk.QRContext{
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
		MemberID:    member,
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
	qrJSON, err := shnsdk.FillQuestionnaire(fhirseed.DemoLumbarQuestionnaire(), shnsdk.DemoLumbarContext(), shnsdk.QRContext{
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
		MemberID:         member,
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
// downstream (an in-process responder / a real br-payer) sees them.
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
	g := &Gateway{cfg: Config{SoR: newCensusSoR()}}
	pci, _, found := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !found {
		t.Fatal("MBR-COVERED not resolvable in censusSoR")
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
// + a real answered QR) for the given member — the hermetic-test shape (the responder needs the QR).
func conformantPASBundleWithQR(t *testing.T, member string) []byte {
	t.Helper()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ref := "Patient/" + member
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar", "M51.16", ref)
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	q := fhirseed.DemoLumbarQuestionnaire()
	qrJSON, err := shnsdk.FillQuestionnaire(q, shnsdk.DemoLumbarContext(), shnsdk.QRContext{
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

// conformantPASBundlePended builds a CONFORMANT $submit bundle that PENDS: a UC-04
// context QR (prior-surgery=true) with NO DiagnosticReport in the bundle → the responder
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
	qrJSON, err := shnsdk.FillQuestionnaire(fhirseed.DemoLumbarQuestionnaire(), shnsdk.DemoLumbarContextPriorSurgery(), shnsdk.QRContext{
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
		MemberID:    member,
		Corr:        "convergence-pas-pend-0001",
		Created:     created,
		Payer:       shnsdk.CMSPayerIdentity,
	})
	if err != nil {
		t.Fatalf("BuildConformantClaimBundle: %v", err)
	}
	return got
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
	return originatorBuiltConformantUpdateBundleCorrs(t, brPayer, "convergence-pas-update-0001", "convergence-pas-submit-0001")
}

// originatorBuiltConformantUpdateBundleCorrs is originatorBuiltConformantUpdateBundleProfile with
// caller-chosen correlations: corr is THIS amendment's own correlation (the operative update
// Claim's urn:shn:correlation identifier) and originalCorr is the original submit's — Claim.related
// [prior], and, on the PayerOrgEntry lane, the sole identifier of the prior-Claim bundle ENTRY the
// sdk appends AFTER the operative Claim. Distinct values let a test tell the two apart on the wire.
func originatorBuiltConformantUpdateBundleCorrs(t *testing.T, brPayer bool, corr, originalCorr string) []byte {
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
		MemberID:         member,
		Provenance:       provJSON,
		DiagnosticReport: drJSON,
		Corr:             corr,
		OriginalCorr:     originalCorr,
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
// nothing downstream (an in-process responder / a real br-payer) sees them. Mirrors
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

// --- Finding A corr threading: the amend's OWN correlation must win -------

// TestParseConformantPASUpdateFacts_AmendCorrWinsOverPriorClaimEntry is the rejection test for the
// ingress amendment 502. On the reference-payer (PayerOrgEntry) lane the sdk appends the original
// submit's Claim as a resolvable bundle ENTRY *after* the operative update Claim, and that prior
// entry's ONLY identifier is urn:shn:correlation|<original submit corr>. While
// parseConformantPASUpdateFacts took claimCorrelation from every Claim entry it met (last Claim
// wins), the SUBMIT's correlation was threaded onto the AMEND's envelope by handlePASIngress
// (child = f.claimCorrelation); the Hub's replay guard then rejected the child leg and the partner
// saw 502 {"error":"hub routing failed"}. The operative update Claim — the one carrying related[]
// — is the only Claim whose correlation may be threaded.
func TestParseConformantPASUpdateFacts_AmendCorrWinsOverPriorClaimEntry(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundleCorrs(t, true, "AMEND-CORR", "SUBMIT-CORR")
	f, status, msg := parseConformantPASUpdateFacts(bundle)
	if status != 0 {
		t.Fatalf("PayerOrgEntry update bundle rejected: status=%d (%s), want 0", status, msg)
	}
	if f.relatedClaim != "SUBMIT-CORR" {
		t.Fatalf("relatedClaim = %q, want the original submit corr SUBMIT-CORR (FR-21)", f.relatedClaim)
	}
	if f.claimCorrelation != "AMEND-CORR" {
		t.Fatalf("claimCorrelation = %q, want the AMEND's own corr AMEND-CORR — the prior-Claim ENTRY's "+
			"correlation must not win, or the ingress threads the submit's corr and the Hub replay guard 502s",
			f.claimCorrelation)
	}
}

// TestParseConformantPASUpdateFacts_SingleClaimShapeUnchanged pins the no-PayerOrgEntry shape (one
// Claim, which carries related[]): its own correlation still threads, exactly as before.
func TestParseConformantPASUpdateFacts_SingleClaimShapeUnchanged(t *testing.T) {
	bundle := originatorBuiltConformantUpdateBundleCorrs(t, false, "AMEND-CORR", "SUBMIT-CORR")
	f, status, msg := parseConformantPASUpdateFacts(bundle)
	if status != 0 {
		t.Fatalf("single-Claim update bundle rejected: status=%d (%s), want 0", status, msg)
	}
	if f.relatedClaim != "SUBMIT-CORR" {
		t.Fatalf("relatedClaim = %q, want SUBMIT-CORR", f.relatedClaim)
	}
	if f.claimCorrelation != "AMEND-CORR" {
		t.Fatalf("claimCorrelation = %q, want AMEND-CORR", f.claimCorrelation)
	}
}

// TestParseConformantPASUpdateFacts_InitialSubmitUnchanged pins the plain initial-submit shape (one
// Claim, NO related[]): no prior claim, and the Claim's own correlation still threads — the
// "else the first Claim" arm of the rule.
func TestParseConformantPASUpdateFacts_InitialSubmitUnchanged(t *testing.T) {
	bundle := conformantPASBundlePended(t, "MBR-COVERED")
	f, status, msg := parseConformantPASUpdateFacts(bundle)
	if status != 0 {
		t.Fatalf("initial submit bundle rejected: status=%d (%s), want 0", status, msg)
	}
	if f.relatedClaim != "" {
		t.Fatalf("relatedClaim = %q, want \"\" (an initial submit has no prior claim)", f.relatedClaim)
	}
	if f.claimCorrelation != "convergence-pas-pend-0001" {
		t.Fatalf("claimCorrelation = %q, want the submit's own corr convergence-pas-pend-0001", f.claimCorrelation)
	}
}
