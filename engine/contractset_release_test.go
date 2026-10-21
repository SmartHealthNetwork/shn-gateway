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
// SDK's native set and default without pa.pdex@2.1, in the SDK's order, and no
// test profile's contract.
func TestContractSets_ReleaseBuildIsPAs(t *testing.T) {
	strip := func(set []string) []string {
		return slices.DeleteFunc(set, func(tok string) bool { return tok == shnsdk.ContractPAPDex21 })
	}
	if got, want := NativeContractVersions(), strip(shnsdk.NativeContractVersions()); !slices.Equal(got, want) {
		t.Errorf("NativeContractVersions() = %v, want %v", got, want)
	}
	if got, want := DefaultContractVersions(), strip(shnsdk.SupportedContractVersions()); !slices.Equal(got, want) {
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
