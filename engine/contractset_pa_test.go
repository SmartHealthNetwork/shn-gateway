//go:build !shn_coreonly

package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The contract-set rows that read PA's lines: a build without PA declares
// none of them.

// TestContractSets_DeclareNoPDex: the gateway's native set, its default and
// every declared set it parses leave out pa.pdex@2.1, including a value that
// names no token (empty, or only commas and spaces), which the SDK answers
// with its own default; a set naming the line is refused.
func TestContractSets_DeclareNoPDex(t *testing.T) {
	for name, set := range map[string][]string{"native": NativeContractVersions(), "default": DefaultContractVersions()} {
		if slices.Contains(set, retiredPDexToken) || len(set) == 0 {
			t.Errorf("%s set = %v, want a non-empty set without %s", name, set, retiredPDexToken)
		}
	}
	for _, csv := range []string{"", " ", ",", " , ", ",,", "pa.crd@2.0,,pa.pas@2.0"} {
		got, err := ParseDeclaredContractVersions(csv)
		if err != nil {
			t.Fatalf("ParseDeclaredContractVersions(%q): %v", csv, err)
		}
		if slices.Contains(got, retiredPDexToken) || len(got) == 0 {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, want a set without %s", csv, got, retiredPDexToken)
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

// TestContractSets_PDexRefusalText pins the refusal of a declared pa.pdex@2.1:
// it is ErrPDexUndeclared, with this text, wherever the token stands and
// whatever else the set names (a token this build cannot build included),
// because the line is checked before the SDK's parser reads the set.
func TestContractSets_PDexRefusalText(t *testing.T) {
	const want = `contract-version token "pa.pdex@2.1" is no longer declared by any gateway: a gateway serves no Patient Access API and builds no PDex resource, so a declaration must not claim it: remove it from the declared set (SHN_CONTRACT_VERSIONS)`
	if ErrPDexUndeclared.Error() != want {
		t.Fatalf("ErrPDexUndeclared = %q, want %q", ErrPDexUndeclared, want)
	}
	for _, csv := range []string{"pa.pdex@2.1", "pa.crd@2.0, pa.dtr@2.0, pa.pas@2.0, pa.pdex@2.1", "pa.pas@9.9, pa.pdex@2.1", "not-a-token,pa.pdex@2.1"} {
		if _, err := ParseDeclaredContractVersions(csv); err == nil || err.Error() != want {
			t.Errorf("ParseDeclaredContractVersions(%q) = %v, want %q", csv, err, want)
		}
	}
}

// TestRetiredPDexToken_IsItsOwnLiteral: the gateway owns the retired PDex
// token's bytes. An operator's declaration names it, and the refusal quotes it
// back, so neither may change when the SDK retires its own name for it.
func TestRetiredPDexToken_IsItsOwnLiteral(t *testing.T) {
	if retiredPDexToken != "pa.pdex@2.1" {
		t.Fatalf("retiredPDexToken = %q, want pa.pdex@2.1", retiredPDexToken)
	}
	// The gateway refuses with its own reason (its text pinned by
	// TestContractSets_PDexRefusalText), ahead of the SDK's parser.
	if _, err := ParseDeclaredContractVersions("pa.crd@2.0, pa.pdex@2.1"); !errors.Is(err, ErrPDexUndeclared) {
		t.Fatalf("a declaration naming the token: %v, want ErrPDexUndeclared", err)
	}
}

// TestSDKContractSets_CarryNoPDex: the gateway reads the SDK's native set and
// default as they are, so the SDK it requires must carry no PDex token in
// either. A pin to an SDK that lists one again fails here.
func TestSDKContractSets_CarryNoPDex(t *testing.T) {
	for name, set := range map[string][]string{"native": shnsdk.NativeContractVersions(), "supported": shnsdk.SupportedContractVersions()} {
		if len(set) == 0 {
			t.Fatalf("the SDK's %s set is empty: this check proves nothing", name)
		}
		if i := slices.IndexFunc(set, isPDexToken); i >= 0 {
			t.Errorf("the SDK's %s set carries %s: the gateway would declare a PDex surface it does not build", name, set[i])
		}
	}
}

// isPDexToken reports whether tok is a contract-version token of the pa.pdex
// contract, at any line.
func isPDexToken(tok string) bool {
	contract, _, ok := strings.Cut(tok, "@")
	return ok && contract == "pa.pdex"
}
