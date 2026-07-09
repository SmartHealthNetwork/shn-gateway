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
// name fragments of the two retired fhirseed steps it must never call; the guard
// trips if a future edit reintroduces one.
func TestSeedStepsExcludeRetiredAssets(t *testing.T) {
	for _, n := range seedStepNames() {
		if containsAny(n, "Sandbox", "Lumbar", "personas") {
			t.Errorf("seed step must not reference a retired non-provider-data asset: %q", n)
		}
	}
}
