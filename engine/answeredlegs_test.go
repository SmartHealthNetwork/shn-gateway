package engine

import (
	"slices"
	"testing"
)

// A gateway takes the legs whose response side its role may hold, and a role
// that answers none declares an empty list: absent would say something else.
func TestAnsweredLegs(t *testing.T) {
	if payer := AnsweredLegs("payer"); !slices.IsSorted(payer) {
		t.Fatalf("payer legs are not sorted: %q", payer)
	}
	for _, role := range []string{"provider", "no-such-role"} {
		if got := AnsweredLegs(role); got == nil || len(got) != 0 {
			t.Fatalf("AnsweredLegs(%q) = %#v, want an empty list", role, got)
		}
	}
}
