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

func TestSandboxResponder_ConformantCRD(t *testing.T) {
	data := NewStubHolderData()
	clock := func() time.Time { return adjTestClock }
	adj := NewSandboxAdjudicator(data, clock)
	s := NewSandboxResponder(adj, data, data, clock)
	res, err := s.Handle(context.Background(), "crd-order-select-native", "corr-1", "pci-1", conformantCRD("MBR-COVERED", "72148"))
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
