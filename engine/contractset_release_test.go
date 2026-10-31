//go:build !shn_toyworkstream

package engine

import (
	"maps"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestContractSets_ReleaseBuildIsPAs pins the release build's contract sets
// to what they were before the compiled-in workstreams supplied them: the
// SDK's native set and default, in the SDK's order, and no test profile's
// contract.
func TestContractSets_ReleaseBuildIsPAs(t *testing.T) {
	if got, want := NativeContractVersions(), shnsdk.NativeContractVersions(); !slices.Equal(got, want) {
		t.Errorf("NativeContractVersions() = %v, want %v", got, want)
	}
	if got, want := DefaultContractVersions(), shnsdk.SupportedContractVersions(); !slices.Equal(got, want) {
		t.Errorf("DefaultContractVersions() = %v, want %v", got, want)
	}
	for _, tok := range append(NativeContractVersions(), DefaultContractVersions()...) {
		if strings.HasPrefix(tok, "toy.") {
			t.Errorf("a release build carries the toy contract %s", tok)
		}
	}
	if _, err := ParseDeclaredContractVersions("toy.echo@1"); err == nil {
		t.Error("a release build accepts a declared toy contract")
	}
}

// TestLegOperations_ReleaseBuildIsPAsAndFQSs: a release build's leg
// operations are PA's and FQS's alone, which share no leg.
func TestLegOperations_ReleaseBuildIsPAsAndFQSs(t *testing.T) {
	want := FQSLegOperations()
	for legType, op := range PALegOperations() {
		if _, dup := want[legType]; dup {
			t.Fatalf("%s is a leg of both PA and FQS", legType)
		}
		want[legType] = LegOperation(op)
	}
	if got := LegOperations(); !maps.Equal(got, want) || len(got) < 9 {
		t.Fatalf("LegOperations() = %d legs, want PA's and FQS's %d", len(got), len(want))
	}
}

// TestRegistry_ReleaseBuildIsPAFQSAndModuleOne pins what a release build
// compiles in: the PA and FQS workstream adapters, in that order, and on-behalf
// building (module 1) as its one module.
func TestRegistry_ReleaseBuildIsPAFQSAndModuleOne(t *testing.T) {
	var adapters, mods []string
	for _, w := range workstreams {
		adapters = append(adapters, w.id())
	}
	for _, m := range modules {
		mods = append(mods, m.id())
	}
	if want := []string{workstreamPA, workstreamFQS}; !slices.Equal(adapters, want) {
		t.Errorf("the release build's workstreams are %v, want %v", adapters, want)
	}
	if want := []string{"module1"}; !slices.Equal(mods, want) {
		t.Errorf("the release build's modules are %v, want %v", mods, want)
	}
}
