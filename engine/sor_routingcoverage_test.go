package engine

import (
	"slices"
	"testing"
)

// routingCoverage is a Coverage with the given status ("" for none).
func routingCoverage(id, status string) []byte {
	b := `{"resourceType":"Coverage","id":"` + id + `"`
	if status != "" {
		b += `,"status":"` + status + `"`
	}
	return []byte(b + `}`)
}

// The coverage a request is routed by is the active ones when any is active,
// else every one the system returned.
func TestRoutingCoverageChoice(t *testing.T) {
	for _, row := range []struct {
		name string
		in   [][]byte
		want []int
	}{
		{"none", nil, nil},
		{"one active", [][]byte{routingCoverage("a", "active")}, []int{0}},
		{"active and cancelled: the active one", [][]byte{routingCoverage("c", "cancelled"), routingCoverage("a", "active")}, []int{1}},
		{"several active, in order", [][]byte{routingCoverage("a1", "active"), routingCoverage("d", "draft"), routingCoverage("a2", "active")}, []int{0, 2}},
		{"only cancelled: every one", [][]byte{routingCoverage("c1", "cancelled"), routingCoverage("c2", "cancelled")}, []int{0, 1}},
		{"none in force: every one", [][]byte{routingCoverage("d", "draft"), routingCoverage("e", "entered-in-error"), routingCoverage("x", "")}, []int{0, 1, 2}},
		{"an unreadable status is not active", [][]byte{[]byte(`{"resourceType":"Coverage","status":"active","status":"cancelled"}`), routingCoverage("c", "cancelled")}, []int{0, 1}},
		{"a status that is not a string is not active", [][]byte{[]byte(`{"resourceType":"Coverage","status":["active"]}`), routingCoverage("c", "cancelled")}, []int{0, 1}},
		{"status is matched exactly", [][]byte{routingCoverage("A", "Active"), routingCoverage("a", "active")}, []int{1}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := routingCoverageChoice(row.in); !slices.Equal(got, row.want) {
				t.Fatalf("chose %v, want %v", got, row.want)
			}
		})
	}
}

// A payor reference names a record relatively or by an absolute URL ending in
// it, with or without a version; a reference to a longer id, or to the id
// under another type, does not.
func TestNamedBy(t *testing.T) {
	for _, row := range []struct {
		ref  string
		want bool
	}{
		{"Organization/pay-2", true},
		{"https://ehr.example/fhir/Organization/pay-2", true},
		{"Organization/pay-2/_history/1", true},
		{"https://ehr.example/fhir/Organization/pay-2/_history/1", true},
		// A base whose path contains /_history/ is part of the reference.
		{"https://ehr.example/_history/fhir/Organization/pay-2", true},
		{"Organization/pay-22", false},
		{"Organization/pay-22/_history/1", false},
		{"https://ehr.example/fhir/xOrganization/pay-2", false},
		{"Practitioner/pay-2", false},
		{"", false},
	} {
		if got := namedBy([]string{row.ref}, "Organization/pay-2"); got != row.want {
			t.Errorf("namedBy(%q) = %v, want %v", row.ref, got, row.want)
		}
	}
	if namedBy([]string{"Organization/pay-2"}, "") {
		t.Error("a record with no name is named by nothing")
	}
}
