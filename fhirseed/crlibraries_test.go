package fhirseed_test

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
)

func TestCRPrepopLibraries(t *testing.T) {
	libs := fhirseed.CRPrepopLibraries()
	want := []string{
		"BasicPatientInfoPrepopulation", "DTRHelpers",
		"HomeHealthAssessmentPrepopulation", "HomeOxygenDispatchPrepopulation",
	}
	if len(libs) != len(want) {
		t.Fatalf("got %d libraries, want %d: %v", len(libs), len(want), keys(libs))
	}
	for _, id := range want {
		body, ok := libs[id]
		if !ok {
			t.Errorf("missing library %q", id)
			continue
		}
		if len(body) == 0 {
			t.Errorf("library %q is empty", id)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
