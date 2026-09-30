package engine

import (
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestForwardedContracts pins the contracts a payer's native responder
// forwards: the PA catalog's, and nothing the gateway answers itself.
func TestForwardedContracts(t *testing.T) {
	var got []string
	for c := range forwardedContracts() {
		got = append(got, c)
	}
	slices.Sort(got)
	if want := []string{"pa.crd", "pa.dtr", "pa.pas"}; !slices.Equal(got, want) {
		t.Fatalf("forwarded contracts = %q, want %q", got, want)
	}
}

// TestDeriveDeclaredContractVersions: a payer whose own system
// declares its versions declares those lines for every forwarded contract,
// plus the build default's pa.pdex line, in native order.
func TestDeriveDeclaredContractVersions(t *testing.T) {
	pdex := shnsdk.ContractPAPDex21
	for _, tc := range []struct {
		name    string
		backend []string
		want    []string
	}{
		{"a 2.2 system", []string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2"},
			[]string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2", pdex}},
		{"order, repeats and spaces do not matter", []string{" pa.pas@2.2", "pa.crd@2.2", "pa.pas@2.2", "pa.dtr@2.2 "},
			[]string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2", pdex}},
		{"several lines", []string{"pa.pas@2.2", "pa.pas@2.0", "pa.crd@2.0"},
			[]string{"pa.crd@2.0", "pa.pas@2.0", "pa.pas@2.2", pdex}},
		{"a partial system declares only what it speaks", []string{"pa.pas@2.1"},
			[]string{"pa.pas@2.1", pdex}},
		{"another contract's token is not a forwarded line", []string{"pa.pas@2.0", "pa.pdex@9.9", "x.y@1"},
			[]string{"pa.pas@2.0", pdex}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveDeclaredContractVersions(tc.backend)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("derived %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeriveDeclaredContractVersions_Refuses: a forwarded line this build
// cannot exchange, and a system set with no forwarded line at all, refuse,
// naming what to set.
func TestDeriveDeclaredContractVersions_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend []string
		must    []string
	}{
		{"unbuildable line", []string{"pa.crd@2.2", "pa.pas@3.0"}, []string{"pa.pas@3.0", "SHN_CONTRACT_VERSIONS"}},
		{"no forwarded line", []string{"pa.pdex@2.1"}, []string{"no CRD, DTR or PAS line", "PAYER_DAVINCI_CONTRACT_VERSIONS", "SHN_CONTRACT_VERSIONS"}},
		{"empty", nil, []string{"no CRD, DTR or PAS line"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveDeclaredContractVersions(tc.backend)
			if err == nil {
				t.Fatalf("derived %q, want a refusal", got)
			}
			for _, m := range tc.must {
				if !strings.Contains(err.Error(), m) {
					t.Fatalf("refusal %q does not name %q", err, m)
				}
			}
		})
	}
}

// TestRequireForwardedContractLine: a declaration with no CRD, DTR or PAS
// line is refused; one with any is not.
func TestRequireForwardedContractLine(t *testing.T) {
	if err := RequireForwardedContractLine([]string{shnsdk.ContractPAPDex21}); err == nil || !strings.Contains(err.Error(), "no CRD, DTR or PAS line") {
		t.Fatalf("pdex-only declaration: err = %v", err)
	}
	for _, ok := range [][]string{shnsdk.SupportedContractVersions(), {"pa.dtr@2.2"}, {" pa.crd@2.0", "pa.pdex@2.1"}} {
		if err := RequireForwardedContractLine(ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
}
