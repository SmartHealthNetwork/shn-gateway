package engine

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pasTailClock is the deterministic clock the PAS-tail byte-parity tests inject (it must
// equal what the SDK builder stamps as Bundle.timestamp / Claim.created for byte-parity).
var pasTailClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }

// pasTailDeviceRequest is the HomeOxygen-style DME order (a DeviceRequest → InfoChanged
// must stay FALSE → HomeOxygen's wire bytes are unchanged by the extraction).
func pasTailDeviceRequest() []byte {
	return []byte(`{"resourceType":"DeviceRequest","id":"dr-ox","status":"active","intent":"order","subject":{"reference":"Patient/MBR-OX"},"performer":{"reference":"Organization/org-dme-ox"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431","display":"Portable gaseous oxygen system"}]}}`)
}

// pasTailServiceRequest is a single-shot procedure order (a ServiceRequest).
func pasTailServiceRequest() []byte {
	// performer is the party requesting the service — a LIST on a
	// ServiceRequest, unlike a DeviceRequest's single reference — and it names
	// the participant's own requesting-provider record, which every lane's system
	// of record holds.
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-PD-UC04"},"performer":[{"reference":"` + OrderingProviderRef + `"}],"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
}

// TestBuildPASSubmitBundle_ByteParity is the load-bearing extraction guard: the shared lean
// PAS tail's bundle builder (buildPASSubmitBundle) produces, for EITHER order type, exactly the
// bundle the plain SDK builder produces with no InfoChanged — a fresh submit states no
// information change, so neither order type carries that stamp.
//
// This proves the extraction did not move HomeOxygen's wire bytes (the live gate is the final proof).
func TestBuildPASSubmitBundle_ByteParity(t *testing.T) {
	const (
		member      = "MBR-OX"
		patientRef  = "Patient/" + member
		coverageRef = "Coverage/" + member
		corr        = "corr-pas-tail"
	)
	qr := []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-x","status":"completed","subject":{"reference":"` + patientRef + `"}}`)

	t.Run("DeviceRequest (HomeOxygen) -> byte-identical, no infoChanged", func(t *testing.T) {
		order := pasTailDeviceRequest()
		got, err := buildPASSubmitBundle("2.0", true, order, qr, testRequestingProvider(), testMemberCoverage(member), testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, patientRef, coverageRef, member, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		// The existing HomeOxygen path builds this EXACT call (relaysReferencePayerBytes(provider-data)=true,
		// no InfoChanged set → default false).
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Insurer: testPayerOrganization(shnsdk.CMSPayerIdentity), Coverage: testMemberCoverage(member),
			Provider:       testRequestingProvider(),
			MemberIDSystem: shnsdk.MemberSystem,
			QR:             qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef, MemberID: member,
			Corr: corr, Created: pasTailClock(),
			ContainedInsurer: true, AbsoluteRefs: true, PayerOrgEntry: true,
			Payer: shnsdk.CMSPayerIdentity,
		})
		if err != nil {
			t.Fatalf("want bundle: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("DeviceRequest bundle NOT byte-identical to HomeOxygen's existing build:\n got=%s\nwant=%s", got, want)
		}
		if bytes.Contains(got, []byte(pasInfoChangedExtURL)) {
			t.Fatalf("a fresh DeviceRequest single-shot must carry no infoChanged stamp")
		}
	})

	t.Run("ServiceRequest single-shot -> byte-identical, no infoChanged", func(t *testing.T) {
		order := pasTailServiceRequest()
		got, err := buildPASSubmitBundle("2.0", true, order, qr, testRequestingProvider(), testMemberCoverage(member), testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, patientRef, coverageRef, member, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Insurer: testPayerOrganization(shnsdk.CMSPayerIdentity), Coverage: testMemberCoverage(member),
			Provider:       testRequestingProvider(),
			MemberIDSystem: shnsdk.MemberSystem,
			QR:             qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef, MemberID: member,
			Corr: corr, Created: pasTailClock(),
			ContainedInsurer: true, AbsoluteRefs: true, PayerOrgEntry: true,
			Payer: shnsdk.CMSPayerIdentity,
		})
		if err != nil {
			t.Fatalf("want bundle: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ServiceRequest bundle NOT equal to the plain SDK build:\n got=%s\nwant=%s", got, want)
		}
		if bytes.Contains(got, []byte(pasInfoChangedExtURL)) {
			t.Fatalf("a fresh ServiceRequest single-shot must carry no infoChanged stamp")
		}
	})

	t.Run("non-br-payer profile -> never sets InfoChanged (byte-identical)", func(t *testing.T) {
		// This row exercises buildPASSubmitBundle's brPayer=false ARM directly with a literal
		// (no real caller passes false any more: relaysReferencePayerBytes(g.cfg.OriginationProfile)
		// is unconditionally true for any real role=provider deployment, since gateway/app.go's
		// loadConfig normalizes an unset ORIGINATION_PROFILE to "demo"). The arm itself stays —
		// it is what keeps a lane that does NOT relay reference-payer bytes byte-identical — this
		// row pins that the SHN-native shape it would produce keeps its pre-existing bytes.
		order := pasTailServiceRequest()
		got, err := buildPASSubmitBundle("2.0", false, order, qr, testRequestingProvider(), testMemberCoverage(member), testPayerOrganization(shnsdk.CMSPayerIdentity), shnsdk.MemberSystem, patientRef, coverageRef, member, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{Coverage: testMemberCoverage(member),
			Provider:       testRequestingProvider(),
			MemberIDSystem: shnsdk.MemberSystem,
			QR:             qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef, MemberID: member,
			Corr: corr, Created: pasTailClock(),
			Payer: shnsdk.CMSPayerIdentity,
		})
		if err != nil {
			t.Fatalf("want bundle: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("managed lane bundle NOT byte-identical to the plain SDK build:\n got=%s\nwant=%s", got, want)
		}
	})
}

// TestClassifyResolution_RealA1 proves g.classifyResolution parses a REAL approved A1
// ClaimResponse (the SDK BuildClaimResponse shape — A1 detected from the nested reviewAction
// extension code, NOT category.text). This is the resolution-site check submitClaimAndFollow
// reuses.
func TestClassifyResolution_RealA1(t *testing.T) {
	a1, err := shnsdk.BuildClaimResponse("AUTH-A1-1", "2030-01-01", "Patient/MBR-OX", "corr-a1", pasTailClock())
	if err != nil {
		t.Fatalf("BuildClaimResponse: %v", err)
	}
	var g Gateway
	parsed, decision := g.classifyResolution(a1)
	if decision != "approved" {
		t.Fatalf("classifyResolution must read a real A1 ClaimResponse as approved, got %q:\n%s", decision, a1)
	}
	if parsed.PreAuthRef != "AUTH-A1-1" {
		t.Fatalf("PreAuthRef = %q, want AUTH-A1-1", parsed.PreAuthRef)
	}

	// Control: a pended (queued) Bundle reads as a PEND — the payer's own answer, with
	// what it is waiting for. Reading it as "not approved" is what used to reach an
	// operator as a failed request.
	pended, err := testPendedResponse("Patient/MBR-OX", "corr-p", "operative-report", pasTailClock())
	if err != nil {
		t.Fatalf("testPendedResponse: %v", err)
	}
	pendParsed, decision := g.classifyResolution(pended)
	if decision != "pended" {
		t.Fatalf("classifyResolution must read a pended response as pended, got %q", decision)
	}
	if len(pendParsed.NeededItems) == 0 {
		t.Fatal("a pend carries what the payer is waiting for")
	}
}

// TestSubmitClaimAndResolve_BridgedEgressRefusesAtNone is the wiring-level
// proof that a REAL call site — not just the helper in isolation — refuses a
// bridged payload even at ConformanceEnforcement=none. It forces a genuine
// (non-gated) arm-3 route: own declares only pa.pas@2.1, the peer only
// pa.pas@2.2, so selectChainRoute has no arm-1/2 escape and must pick the
// pa.pas 2.1->2.2 hop — StepCarry (transform_pas.go's pasStep2122Up), which
// does NOT semantically refuse on a Claim-shaped submit bundle the way the
// 2.0->2.1 StepGated hop does (TestTransformRefusalZeroBytes's row) — so
// egressAdapt actually SUCCEEDS and route.Chain is genuinely non-empty by
// the time submitClaimAndResolve's call reads it. With the target line 2.2
// rigged to reject every Bundle, the wire call still refuses at 422 despite
// ConformanceEnforcement=none: this is "the message refuses", the
// call-site-level twin of TestEgressAdaptValidatesAtTargetLane's
// helper-level proof.
func TestSubmitClaimAndResolve_BridgedEgressRefusesAtNone(t *testing.T) {
	env := newInProcessExchange(t)
	declareRecipientVersions(t, env, []string{"pa.pas@2.2"})
	env.originator.cfg.DeclaredContractVersions = []string{"pa.pas@2.1"}
	env.originator.cfg.EgressNativeLines = []string{"2.1"}
	env.originator.cfg.ConformanceEnforcement = EnforcementNone
	env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{
		"2.1": shnsdk.NewFakeValidator(),
		"2.2": &shnsdk.FakeValidator{RejectIfContains: `"resourceType":"Bundle"`},
	}

	// A member the participant's own system actually holds, with that system's
	// own Coverage, payer Organization and member namespace read off it exactly
	// as originateCRDThenDTR does. A made-up member and nil records are refused
	// 502 long before the egress check this row is about: a submission names
	// only parties the participant can read.
	const (
		member      = "MBR-COVERED"
		patientRef  = "Patient/" + member
		coverageRef = "Coverage/" + member
	)
	recs, status, msg := env.originator.originCRDRecords(env.ctx, "crd-order-select", member)
	if status != 0 {
		t.Fatalf("originCRDRecords: %d %s", status, msg)
	}
	realCov, found, status, msg := env.originator.memberCoverage(env.ctx, member)
	if status != 0 || !found {
		t.Fatalf("memberCoverage: status=%d found=%v %s", status, found, msg)
	}
	realPayerOrg, status, msg := env.originator.memberPayerOrganization(env.ctx, realCov)
	if status != 0 {
		t.Fatalf("memberPayerOrganization: %d %s", status, msg)
	}
	order := pasTailServiceRequest()
	sub, status, msg, err := env.originator.submitPASClaim(env.ctx, env.req, "pci-1", order, nil, nil, realCov, realPayerOrg, patientRef, coverageRef, member, recs.memberSystem, shnsdk.CMSPayerIdentity, env.payerID)
	respJSON := sub.respJSON
	if err != nil {
		t.Fatalf("submitPASClaim: unexpected error (want a clean 422 refusal, not an error path): %v", err)
	}
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d — a bridged payload must refuse on the wire even at ConformanceEnforcement=none", status, http.StatusUnprocessableEntity)
	}
	if msg == "" {
		t.Fatal("want a non-empty refusal message")
	}
	if respJSON != nil {
		t.Fatalf("respJSON must be nil (refused before the leg was ever routed), got %q", respJSON)
	}
	// The fake Hub's /route was never hit — the target-lane refusal happens
	// strictly before the Hub round-trip, mirroring TestTransformRefusalZeroBytes's row.
	if got := env.routeHitCount(); got != 0 {
		t.Fatalf("fake Hub /route was called %d times — a bridged-egress refusal must never reach the Hub", got)
	}
}
