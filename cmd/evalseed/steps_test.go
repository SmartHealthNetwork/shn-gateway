// gateway/cmd/evalseed/steps_test.go
package main

import "testing"

func TestSeedStepsAreProviderDataOnly(t *testing.T) {
	got := seedStepNames()
	want := []string{
		"WaitReady", "CreatePartitions(provider)", "InstallCRLibraries",
		"WarmUpPopulate", "LoadProviderDataBundles(provider)", "WriteSeedMarker(provider)",
	}
	if len(got) != len(want) {
		t.Fatalf("step count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The eval seeder installs provider-data assets only. The substrings below are
// name fragments of the retired fhirseed steps it must never call; the guard trips
// if a future edit reintroduces one.
//
// "Sandbox" is kept even though no surface answers to that name any more (the
// v0.46.0/v0.39.0 renames took the last of them). This fence is FORWARD-looking, not
// a description of today's tree: the standing rule is that the sandbox payer is gone
// for good, so a step name carrying the word is a defect whenever it appears.
// Removing the token because it currently matches nothing is exactly how a fence
// stops fencing. It is permanent.
func TestSeedStepsExcludeRetiredAssets(t *testing.T) {
	for _, n := range seedStepNames() {
		if containsAny(n, "Sandbox", "Lumbar", "personas") {
			t.Errorf("seed step must not reference a retired non-provider-data asset: %q", n)
		}
	}
}
