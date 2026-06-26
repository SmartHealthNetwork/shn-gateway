// crd_hcpcs_test.go — §3.1: the CRD intake accepts a HCPCS order (no longer CPT-locked).
package engine

import (
	"context"
	"net/http"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const hcpcsOrderSelectJSON = `{"hook":"order-select","context":{"patientId":"MBR-UC07HCPCS",` +
	`"draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest",` +
	`"status":"draft","intent":"order","subject":{"reference":"Patient/MBR-UC07HCPCS"},` +
	`"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets",` +
	`"code":"L8000","display":"Breast prosthesis, mastectomy bra"}]}}}]}},"prefetch":{}}`

const noProcCodingOrderSelectJSON = `{"hook":"order-select","context":{"patientId":"p",` +
	`"draftOrders":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ServiceRequest",` +
	`"status":"draft","intent":"order","subject":{"reference":"Patient/p"},` +
	`"code":{"coding":[{"system":"http://example.org/other","code":"X"}]}}}]}},"prefetch":{}}`

func TestCRD_AcceptsHCPCSOrder(t *testing.T) {
	r := newSandboxResponderForTest(t)
	res, err := r.Handle(context.Background(), "crd-order-select", "corr", "pci:x", []byte(hcpcsOrderSelectJSON))
	if err != nil {
		t.Fatalf("Handle returned engine error: %v", err)
	}
	if res.Status == http.StatusBadRequest {
		t.Fatalf("HCPCS order 400'd at CRD intake (still CPT-locked): %q", res.Message)
	}
	if res.Status != 0 {
		t.Fatalf("unexpected status %d: %q", res.Status, res.Message)
	}
	if len(res.ResponseFHIR) == 0 {
		t.Fatal("HCPCS order produced no CRD response body")
	}
	if _, err := shnsdk.ParseCards(res.ResponseFHIR); err != nil {
		t.Fatalf("CRD response is not a valid cards response: %v", err)
	}
}

func TestCRD_RejectsNoProcedureCoding(t *testing.T) {
	r := newSandboxResponderForTest(t)
	res, _ := r.Handle(context.Background(), "crd-order-select", "corr", "pci:x", []byte(noProcCodingOrderSelectJSON))
	if res.Status != http.StatusBadRequest {
		t.Fatalf("a {CPT,HCPCS}-less order must 400; got status=%d msg=%q", res.Status, res.Message)
	}
}
