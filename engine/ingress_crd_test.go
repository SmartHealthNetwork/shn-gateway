package engine

import "testing"

// A conformant inbound CDS Hooks order-select request (pinned conformant shape, trimmed to the
// patient-bearing fields the cross-field fence inspects). patientId + every prefetch/order
// patient ref must resolve to ONE pci.
func crdReqJSON(patientID, orderSubjectRef, coverageBeneficiaryRef string) []byte {
	return []byte(`{
      "hook": "order-select",
      "hookInstance": "hi-1",
      "fhirServer": "https://provider.example/fhir",
      "fhirAuthorization": {"token_type":"Bearer","access_token":"tok","expires_in":300},
      "context": {
        "userId": "Practitioner/p1",
        "patientId": "` + patientID + `",
        "draftOrders": {"resourceType":"Bundle","type":"collection","entry":[
          {"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","subject":{"reference":"` + orderSubjectRef + `"}}}
        ]},
        "selections": ["ServiceRequest/sr1"]
      },
      "prefetch": {
        "patient": {"resourceType":"Patient","id":"` + patientID + `"},
        "coverage": {"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"` + coverageBeneficiaryRef + `"}}
      }
    }`)
}

func TestIngressSubjectPCI_AllReferencesAgree(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	ref := "Patient/MBR-COVERED"
	pci, status, _ := g.ingressCRDSubjectPCI(crdReqJSON("MBR-COVERED", ref, ref))
	if status != 0 {
		t.Fatalf("all-agree request: status = %d, want 0", status)
	}
	if pci == "" {
		t.Fatal("all-agree request: empty pci")
	}
}

// A KNOWN different member in the order subject must fail closed (403): both resolve, but to
// DIFFERENT pcis — this exercises the rp != pci branch (not the unknown-member branch).
func TestIngressSubjectPCI_DivergentKnownPatientFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	good := "Patient/MBR-COVERED"
	_, status, _ := g.ingressCRDSubjectPCI(crdReqJSON("MBR-COVERED", "Patient/MBR-NOTCOVERED", good))
	if status != 403 {
		t.Fatalf("divergent KNOWN patient: status = %d, want 403", status)
	}
}

// An UNKNOWN patient reference (cannot resolve) must also fail closed.
func TestIngressSubjectPCI_UnknownReferenceFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	good := "Patient/MBR-COVERED"
	_, status, _ := g.ingressCRDSubjectPCI(crdReqJSON("MBR-COVERED", "Patient/MBR-UNKNOWN", good))
	if status == 0 {
		t.Fatal("unknown patient reference: want fail-closed, got 0")
	}
}

// A draft order with NO recognizable patient subject is rejected (not skipped): an order with
// no patient must never ride into the sealed request behind the bound subject's authority.
func TestIngressSubjectPCI_DraftOrderMissingSubjectFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	body := []byte(`{
      "hook":"order-select",
      "context":{
        "patientId":"MBR-COVERED",
        "draftOrders":{"resourceType":"Bundle","type":"collection","entry":[
          {"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1"}}
        ]}
      },
      "prefetch":{"patient":{"resourceType":"Patient","id":"MBR-COVERED"}}
    }`)
	_, status, _ := g.ingressCRDSubjectPCI(body)
	if status != 403 {
		t.Fatalf("draft order missing subject: status = %d, want 403", status)
	}
}

// A missing context.patientId fails closed (400).
func TestIngressSubjectPCI_MissingPatientId(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	_, status, _ := g.ingressCRDSubjectPCI([]byte(`{"hook":"order-select","context":{}}`))
	if status != 400 {
		t.Fatalf("missing patientId: status = %d, want 400", status)
	}
}

// A bare prefetch.patient resource whose IDENTITY (its id, not a reference) is a DIFFERENT person
// than the bound patient fails closed (403) — patientRefOf can't see a Patient's id, so this is the
// dedicated §4 fence for the prefetch Patient (a different person's demographics must not ride into
// the bound patient's sealed exchange).
func TestIngressSubjectPCI_DivergentPrefetchPatientFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	body := []byte(`{
      "hook":"order-select","context":{"patientId":"MBR-COVERED",
        "draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}}]}},
      "prefetch":{
        "patient":{"resourceType":"Patient","id":"MBR-NOTCOVERED"},
        "coverage":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}
    }`)
	_, status, _ := g.ingressCRDSubjectPCI(body)
	if status != 403 {
		t.Fatalf("divergent prefetch.patient id: status = %d, want 403", status)
	}
}
