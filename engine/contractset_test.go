package engine

import (
	"errors"
	"slices"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestContractSets_DeclareNoPDex: the gateway's native set, its default and
// every declared set it parses leave out pa.pdex@2.1, including a value that
// names no token (empty, or only commas and spaces), which the SDK answers
// with its own default; a set naming the line is refused.
func TestContractSets_DeclareNoPDex(t *testing.T) {
	for name, set := range map[string][]string{"native": NativeContractVersions(), "default": DefaultContractVersions()} {
		if slices.Contains(set, shnsdk.ContractPAPDex21) || len(set) == 0 {
			t.Errorf("%s set = %v, want a non-empty set without %s", name, set, shnsdk.ContractPAPDex21)
		}
	}
	for _, csv := range []string{"", " ", ",", " , ", ",,", "pa.crd@2.0,,pa.pas@2.0"} {
		got, err := ParseDeclaredContractVersions(csv)
		if err != nil {
			t.Fatalf("ParseDeclaredContractVersions(%q): %v", csv, err)
		}
		if slices.Contains(got, shnsdk.ContractPAPDex21) || len(got) == 0 {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, want a set without %s", csv, got, shnsdk.ContractPAPDex21)
		}
	}
	if got, _ := ParseDeclaredContractVersions(" , "); !slices.Equal(got, DefaultContractVersions()) {
		t.Errorf("a value naming no token parses to %v, want the default %v", got, DefaultContractVersions())
	}
	for _, csv := range []string{"pa.pdex@2.1", " pa.pdex@2.1 ", "pa.crd@2.0, pa.pdex@2.1", ",pa.pdex@2.1,"} {
		if got, err := ParseDeclaredContractVersions(csv); !errors.Is(err, ErrPDexUndeclared) {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, %v; want ErrPDexUndeclared", csv, got, err)
		}
	}
}
