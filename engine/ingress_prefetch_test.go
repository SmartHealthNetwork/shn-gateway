package engine

import (
	"encoding/json"
	"net/http"
	"testing"
)

// boundPCI resolves the test member's pci the way the handler does.
func boundPCI(t *testing.T, g *Gateway, member string) string {
	t.Helper()
	pci, _, ok := g.cfg.SoR.ResolvePatient(member)
	if !ok {
		t.Fatalf("member %q does not resolve", member)
	}
	return pci
}

func TestEnsureSelfContained_KeepsInlinedAndStripsCallback(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci := boundPCI(t, g, "MBR-COVERED")
	ref := "Patient/MBR-COVERED"
	out, status, msg := g.ingressEnsureSelfContained(crdReqJSON("MBR-COVERED", ref, ref), "MBR-COVERED", pci)
	if status != 0 {
		t.Fatalf("fully-inlined request: status = %d (%s), want 0", status, msg)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if _, ok := doc["fhirServer"]; ok {
		t.Error("fhirServer not stripped")
	}
	if _, ok := doc["fhirAuthorization"]; ok {
		t.Error("fhirAuthorization not stripped (non-aggregation floor requires BOTH callback fields gone)")
	}
	var prefetch map[string]json.RawMessage
	_ = json.Unmarshal(doc["prefetch"], &prefetch)
	if _, ok := prefetch["coverage"]; !ok {
		t.Error("inlined coverage dropped")
	}
	// the absent histories were resolved+inlined from the SoR
	if _, ok := prefetch["serviceHistory"]; !ok {
		t.Error("serviceHistory not resolved from SoR")
	}
}

func TestEnsureSelfContained_ResolvesAbsentCoverageFromSoR(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci := boundPCI(t, g, "MBR-COVERED")
	ref := "Patient/MBR-COVERED"
	body := crdReqJSON("MBR-COVERED", ref, ref)
	var req map[string]json.RawMessage
	_ = json.Unmarshal(body, &req)
	var prefetch map[string]json.RawMessage
	_ = json.Unmarshal(req["prefetch"], &prefetch)
	delete(prefetch, "coverage")
	req["prefetch"], _ = json.Marshal(prefetch)
	body, _ = json.Marshal(req)

	out, status, msg := g.ingressEnsureSelfContained(body, "MBR-COVERED", pci)
	if status != 0 {
		t.Fatalf("resolvable absent coverage: status = %d (%s), want 0", status, msg)
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(out, &doc)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(doc["prefetch"], &got)
	if _, ok := got["coverage"]; !ok {
		t.Error("coverage not resolved+inlined from SoR")
	}
}

func TestEnsureSelfContained_UnresolvableFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	// Unknown member: coverage cannot be resolved → 422 fail-closed (never a live callback).
	body := []byte(`{"hook":"order-select","context":{"patientId":"MBR-UNKNOWN"},"prefetch":{}}`)
	_, status, _ := g.ingressEnsureSelfContained(body, "MBR-UNKNOWN", "no-such-pci")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unresolvable prefetch: status = %d, want 422", status)
	}
}

// DEF-INGRESS-BUNDLE (closes the §4 fence's deferred gap): a KEPT serviceHistory Bundle whose
// entry references a DIFFERENT patient must fail closed (403) — a crafted history Bundle must
// not smuggle wrong-patient resources into the sealed request.
func TestEnsureSelfContained_KeptBundleWrongPatientFailsClosed(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci := boundPCI(t, g, "MBR-COVERED")
	ref := "Patient/MBR-COVERED"
	body := crdReqJSON("MBR-COVERED", ref, ref)
	var req map[string]json.RawMessage
	_ = json.Unmarshal(body, &req)
	var prefetch map[string]json.RawMessage
	_ = json.Unmarshal(req["prefetch"], &prefetch)
	// Inline a serviceHistory searchset Bundle whose entry is for MBR-NOTCOVERED (a different patient).
	prefetch["serviceHistory"] = json.RawMessage(`{"resourceType":"Bundle","type":"searchset","entry":[
	  {"resource":{"resourceType":"ServiceRequest","id":"x","subject":{"reference":"Patient/MBR-NOTCOVERED"}}}
	]}`)
	req["prefetch"], _ = json.Marshal(prefetch)
	body, _ = json.Marshal(req)

	_, status, _ := g.ingressEnsureSelfContained(body, "MBR-COVERED", pci)
	if status != http.StatusForbidden {
		t.Fatalf("kept wrong-patient bundle entry: status = %d, want 403", status)
	}
}
