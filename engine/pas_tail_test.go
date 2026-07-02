package engine

import (
	"bytes"
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

// pasTailServiceRequest is a single-shot procedure order (a ServiceRequest → InfoChanged
// must be TRUE → it flips the payer gate into the timer-poll lane).
func pasTailServiceRequest() []byte {
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-x","status":"active","intent":"order","subject":{"reference":"Patient/MBR-PD-UC04"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)
}

// TestBuildPASSubmitBundle_ByteParity is the load-bearing extraction guard: the shared lean
// PAS tail's bundle builder (buildPASSubmitBundle) sets InfoChanged = !orderIsDeviceRequest, so
//   - a DeviceRequest (HomeOxygen) → InfoChanged:false → byte-IDENTICAL to the existing
//     HomeOxygen path's BuildConformantClaimBundle(... no InfoChanged ...); and
//   - a ServiceRequest single-shot → InfoChanged:true → carries the infoChanged poll discriminator.
//
// This proves the extraction did not move HomeOxygen's wire bytes (the live gate is the final proof).
func TestBuildPASSubmitBundle_ByteParity(t *testing.T) {
	const (
		patientRef  = "Patient/MBR-OX"
		coverageRef = "Coverage/MBR-OX"
		corr        = "corr-pas-tail"
	)
	qr := []byte(`{"resourceType":"QuestionnaireResponse","id":"qr-x","status":"completed","subject":{"reference":"` + patientRef + `"}}`)

	t.Run("DeviceRequest (HomeOxygen) -> InfoChanged:false, byte-identical", func(t *testing.T) {
		order := pasTailDeviceRequest()
		got, err := buildPASSubmitBundle(true, order, qr, patientRef, coverageRef, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		// The existing HomeOxygen path builds this EXACT call (targetsBrPayer(provider-data)=true,
		// no InfoChanged set → default false).
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
			QR: qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef,
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
		if requestClaimHasInfoChanged(got) {
			t.Fatalf("DeviceRequest single-shot must NOT carry infoChanged (HomeOxygen byte-parity)")
		}
	})

	t.Run("ServiceRequest single-shot -> InfoChanged:true (poll discriminator present)", func(t *testing.T) {
		order := pasTailServiceRequest()
		got, err := buildPASSubmitBundle(true, order, qr, patientRef, coverageRef, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
			QR: qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef,
			Corr: corr, Created: pasTailClock(),
			ContainedInsurer: true, AbsoluteRefs: true, PayerOrgEntry: true, InfoChanged: true,
			Payer: shnsdk.CMSPayerIdentity,
		})
		if err != nil {
			t.Fatalf("want bundle: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ServiceRequest bundle NOT equal to the InfoChanged:true SDK build:\n got=%s\nwant=%s", got, want)
		}
		if !requestClaimHasInfoChanged(got) {
			t.Fatalf("ServiceRequest single-shot must carry the infoChanged poll discriminator")
		}
	})

	t.Run("non-br-payer profile -> never sets InfoChanged (sandbox byte-identical)", func(t *testing.T) {
		// The sandbox/managed lane (targetsBrPayer=false) keeps the byte-identical sandbox path AND
		// never sets the poll discriminator regardless of order type.
		order := pasTailServiceRequest()
		got, err := buildPASSubmitBundle(false, order, qr, patientRef, coverageRef, corr, pasTailClock(), shnsdk.CMSPayerIdentity)
		if err != nil {
			t.Fatalf("buildPASSubmitBundle: %v", err)
		}
		want, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
			QR: qr, SR: order, PatientRef: patientRef, CoverageRef: coverageRef,
			Corr: corr, Created: pasTailClock(),
			Payer: shnsdk.CMSPayerIdentity,
		})
		if err != nil {
			t.Fatalf("want bundle: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("sandbox lane bundle NOT byte-identical to the plain SDK build:\n got=%s\nwant=%s", got, want)
		}
	})
}

// TestClassifyResolution_RealA1 proves the existing g.classifyResolution parses a REAL approved A1
// ClaimResponse (the SDK BuildClaimResponse shape — A1 detected from the nested reviewAction
// extension code, NOT category.text). This is the resolution-site check submitClaimAndResolve reuses.
func TestClassifyResolution_RealA1(t *testing.T) {
	a1, err := shnsdk.BuildClaimResponse("AUTH-A1-1", "2030-01-01", "Patient/MBR-OX", "corr-a1", pasTailClock())
	if err != nil {
		t.Fatalf("BuildClaimResponse: %v", err)
	}
	var g Gateway
	parsed, approved := g.classifyResolution(a1)
	if !approved {
		t.Fatalf("classifyResolution must read a real A1 ClaimResponse as approved:\n%s", a1)
	}
	if parsed.PreAuthRef != "AUTH-A1-1" {
		t.Fatalf("PreAuthRef = %q, want AUTH-A1-1", parsed.PreAuthRef)
	}

	// Control: a pended (queued) Bundle is NOT approved (a non-resolution is never a silent pass).
	pended, err := shnsdk.BuildPendedResponse("Patient/MBR-OX", "corr-p", []string{"operative-report"}, pasTailClock())
	if err != nil {
		t.Fatalf("BuildPendedResponse: %v", err)
	}
	if _, ok := g.classifyResolution(pended); ok {
		t.Fatalf("classifyResolution must NOT read a pended response as approved")
	}
}
