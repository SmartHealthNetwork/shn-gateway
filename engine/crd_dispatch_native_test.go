package engine

import (
	"net/http"
	"testing"
)

func TestConformantCRDDispatchBind_FencesAllDispatchedOrders(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	// Two orders: dr1 subject MBR-COVERED (ok), dr2 subject MBR-NOTCOVERED (wrong patient).
	req := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1","DeviceRequest/dr2"],"performer":"Organization/dme1"},"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/MBR-COVERED"}}},{"fullUrl":"DeviceRequest/dr2","resource":{"resourceType":"DeviceRequest","id":"dr2","subject":{"reference":"Patient/MBR-NOTCOVERED"}}}]},"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}]}}}`)
	if _, _, status, _ := g.conformantCRDDispatchBind(req, pci); status != http.StatusForbidden {
		t.Fatalf("a second wrong-patient dispatched order must be 403; got %d", status)
	}
}

func TestConformantCRDDispatchBind_RejectsWrongSubject(t *testing.T) {
	g := &Gateway{cfg: Config{SoR: NewStubHolderData()}}
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED") // the token subject
	// dispatchedOrders → DeviceRequest whose subject is MBR-NOTCOVERED, but patientId/token = MBR-COVERED.
	req := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/MBR-NOTCOVERED"}}}]},"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}]}}}`)
	if _, _, status, _ := g.conformantCRDDispatchBind(req, pci); status != http.StatusForbidden {
		t.Fatalf("wrong-subject dispatched order must be 403; got %d", status)
	}
	ok := []byte(`{"hook":"order-dispatch","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},"prefetch":{"deviceHistory":{"resourceType":"Bundle","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","subject":{"reference":"Patient/MBR-COVERED"}}}]},"coverage":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/MBR-COVERED"}}}]}}}`)
	if _, _, status, msg := g.conformantCRDDispatchBind(ok, pci); status != 0 {
		t.Fatalf("consistent request must pass; got %d %s", status, msg)
	}
}
