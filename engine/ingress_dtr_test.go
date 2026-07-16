package engine

import (
	"bytes"
	"testing"
)

// TestDTRFromPackageParams_OrderOrCanonical proves the $questionnaire-package ingress accepts
// EITHER a `questionnaire` canonical (br-payer / SDC path) OR an `order` (the external-payer lane, whose
// operation keys off the CRD-updated order and has no `questionnaire` param), extracts the order
// verbatim, and derives the patient reference for authz from the coverage beneficiary or, absent
// coverage, from the order subject.
func TestDTRFromPackageParams_OrderOrCanonical(t *testing.T) {
	t.Run("order-only (no canonical) is valid", func(t *testing.T) {
		body := []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr-1","subject":{"reference":"Patient/abby"}}},` +
			`{"name":"coverage","resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/abby"}}}]}`)
		canonical, patientRef, coverage, order, ok := dtrFromPackageParams(body)
		if !ok {
			t.Fatal("order-based request must be accepted")
		}
		if canonical != "" {
			t.Errorf("no canonical expected, got %q", canonical)
		}
		if patientRef != "Patient/abby" {
			t.Errorf("patientRef = %q, want Patient/abby", patientRef)
		}
		if len(coverage) == 0 || !bytes.Contains(coverage, []byte(`"Coverage"`)) {
			t.Errorf("coverage not carried: %s", coverage)
		}
		if len(order) == 0 || !bytes.Contains(order, []byte(`"sr-1"`)) {
			t.Errorf("order not carried verbatim: %s", order)
		}
	})

	t.Run("order-only patient from subject when no coverage", func(t *testing.T) {
		body := []byte(`{"resourceType":"Parameters","parameter":[` +
			`{"name":"order","resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/abby"}}}]}`)
		_, patientRef, _, order, ok := dtrFromPackageParams(body)
		if !ok || patientRef != "Patient/abby" || len(order) == 0 {
			t.Fatalf("ok=%v patientRef=%q order=%s", ok, patientRef, order)
		}
	})

	t.Run("canonical path unchanged", func(t *testing.T) {
		body := []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire","valueCanonical":"http://x/q"}]}`)
		canonical, _, _, order, ok := dtrFromPackageParams(body)
		if !ok || canonical != "http://x/q" || len(order) != 0 {
			t.Fatalf("ok=%v canonical=%q order=%s", ok, canonical, order)
		}
	})

	t.Run("neither canonical nor order is rejected", func(t *testing.T) {
		body := []byte(`{"resourceType":"Parameters","parameter":[{"name":"coverage","resource":{"resourceType":"Coverage"}}]}`)
		if _, _, _, _, ok := dtrFromPackageParams(body); ok {
			t.Fatal("a request with neither canonical nor order must be rejected")
		}
	})
}
