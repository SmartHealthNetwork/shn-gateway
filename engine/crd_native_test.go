package engine

import (
	"context"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// conformantCRD builds a conformant CDS Hooks order-select request whose SR subject, coverage
// beneficiary, and context.patientId all reference `member`, with a ServiceRequest carrying `cpt`.
func conformantCRD(member, cpt string) []byte {
	ref := "Patient/" + member
	return []byte(`{
      "hook":"order-select","hookInstance":"hi-1",
      "context":{
        "userId":"Practitioner/p1","patientId":"` + member + `",
        "draftOrders":{"resourceType":"Bundle","type":"collection","entry":[
          {"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"` + ref + `"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"` + cpt + `"}]}}}
        ]},
        "selections":["ServiceRequest/sr1"]
      },
      "prefetch":{
        "coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"` + ref + `"}}
      }
    }`)
}

func TestConformantCRDBind_AllAgree(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	srJSON, covJSON, status, msg := g.conformantCRDBind(conformantCRD("MBR-COVERED", "72148"), pci)
	if status != 0 {
		t.Fatalf("all-agree: status=%d (%s), want 0", status, msg)
	}
	if len(srJSON) == 0 {
		t.Fatal("all-agree: empty srJSON")
	}
	if len(covJSON) == 0 {
		t.Fatal("all-agree: empty covJSON (the bind must return the coverage for validation)")
	}
}

func TestConformantCRDBind_WrongTokenSubject(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	_, _, status, _ := g.conformantCRDBind(conformantCRD("MBR-COVERED", "72148"), "some-other-pci")
	if status != 403 {
		t.Fatalf("wrong token subject: status=%d, want 403", status)
	}
}

func TestConformantCRDBind_DivergentSubject(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	// SR subject MBR-NOTCOVERED, coverage+context MBR-COVERED → inconsistent → 400.
	body := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-NOTCOVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}]}},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}}`)
	_, _, status, _ := g.conformantCRDBind(body, pci)
	if status != 400 {
		t.Fatalf("divergent SR subject: status=%d, want 400", status)
	}
}

// TestConformantCRDBind_RejectsNoOrder is the hermetic fail-closed guard: a draftOrders Bundle that
// carries NO CDS order resource (no ServiceRequest AND no DeviceRequest — only a non-order resource)
// fails closed (400), never a silent wrong-card success. The order-select leg carries either a
// ServiceRequest (UC-04) or a DeviceRequest (UC-02 HospitalBeds), so the bind no longer rejects a
// DeviceRequest per se — it rejects the absence of any order (see TestConformantCRDBind_AcceptsDeviceRequest).
func TestConformantCRDBind_RejectsNoOrder(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	// draftOrders carries only a Patient — no ServiceRequest, no DeviceRequest.
	body := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}}]}},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}}`)
	_, _, status, msg := g.conformantCRDBind(body, pci)
	if status != 400 {
		t.Fatalf("no-order draftOrders: status=%d (%s), want 400 (no ServiceRequest or DeviceRequest → fail closed)", status, msg)
	}
}

// TestConformantCRDBind_AcceptsDeviceRequest proves the order-select leg's bind accepts a
// DeviceRequest order (UC-02 HospitalBeds E0250) whose subject, the coverage beneficiary, and
// context.patientId all reference one member resolving to the token PCI. The subject-bind is
// order-type-agnostic (it reads subject.reference, present on both ServiceRequest and DeviceRequest);
// the security property (order.subject == coverage.beneficiary == context.patientId == token PCI)
// holds for a DeviceRequest exactly as for a ServiceRequest.
func TestConformantCRDBind_AcceptsDeviceRequest(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	body := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"DeviceRequest","id":"dr1","status":"draft","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"reasonCode":[{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"M62.81"}]}],"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0250"}]}}}]},"selections":["DeviceRequest/dr1"]},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}}`)
	orderJSON, covJSON, status, msg := g.conformantCRDBind(body, pci)
	if status != 0 {
		t.Fatalf("DeviceRequest order-select: status=%d (%s), want 0 (HospitalBeds E0250 DeviceRequest is a valid order-select order)", status, msg)
	}
	if len(orderJSON) == 0 || len(covJSON) == 0 {
		t.Fatalf("DeviceRequest order-select: empty orderJSON=%d/covJSON=%d, want both non-empty for downstream validation", len(orderJSON), len(covJSON))
	}
	// The divergent-subject security property still holds for a DeviceRequest: a DR whose subject
	// disagrees with the coverage/context is rejected (the subject-bind is order-type-agnostic).
	divergent := []byte(`{"hook":"order-select","context":{"patientId":"MBR-COVERED","draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"DeviceRequest","subject":{"reference":"Patient/MBR-NOTCOVERED"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0250"}]}}}]}},"prefetch":{"coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}}`)
	if _, _, status, _ := g.conformantCRDBind(divergent, pci); status != 400 {
		t.Fatalf("divergent DeviceRequest subject: status=%d, want 400 (subject-bind holds for DeviceRequest)", status)
	}
}

// TestConformantCRDBind_AcceptsOriginatorBuilt is the SECOND oracle (after the SDK
// byte-match golden test): the payer-side conformantCRDBind accepts the request the
// Originator's SDK builder (BuildConformantOrderSelectRequest + BuildCoverageWithPayer)
// produces. This proves the producer↔consumer contract holds for the convergence shape
// without re-running through the golden file.
func TestConformantCRDBind_AcceptsOriginatorBuilt(t *testing.T) {
	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", "Patient/MBR-COVERED")
	if err != nil {
		t.Fatalf("BuildServiceRequest: %v", err)
	}
	covJSON, err := shnsdk.BuildCoverageWithPayer("Patient/MBR-COVERED", "Coverage/MBR-COVERED", shnsdk.CMSPayerIdentity)
	if err != nil {
		t.Fatalf("BuildCoverageWithPayer: %v", err)
	}
	reqJSON, err := shnsdk.BuildConformantOrderSelectRequest(srJSON, covJSON, "Patient/MBR-COVERED")
	if err != nil {
		t.Fatalf("BuildConformantOrderSelectRequest: %v", err)
	}
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if _, _, status, msg := g.conformantCRDBind(reqJSON, pci); status != 0 {
		t.Fatalf("conformantCRDBind rejected Originator-built request: %d %s", status, msg)
	}
}

func TestSandboxResponder_ConformantCRD(t *testing.T) {
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	s := NewSandboxResponder(adj, data, data, clock)
	res, err := s.Handle(context.Background(), "crd-order-select", "corr-1", "pci-1", conformantCRD("MBR-COVERED", "72148"))
	if err != nil {
		t.Fatalf("conformant sandbox CRD: %v", err)
	}
	if res.Status != 0 {
		t.Fatalf("conformant sandbox CRD: status=%d msg=%q", res.Status, res.Message)
	}
	if _, perr := shnsdk.ParseCards(res.ResponseFHIR); perr != nil {
		t.Fatalf("response is not a cards response: %v", perr)
	}
}
