package engine

import (
	"slices"
	"strings"
	"testing"
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
// declares its versions declares those lines for every forwarded contract, in
// native order, and nothing else: the build default adds no pa.pdex line, even
// when the payer's own system names one.
func TestDeriveDeclaredContractVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend []string
		want    []string
	}{
		{"a 2.2 system", []string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2"},
			[]string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2"}},
		{"order, repeats and spaces do not matter", []string{" pa.pas@2.2", "pa.crd@2.2", "pa.pas@2.2", "pa.dtr@2.2 "},
			[]string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2"}},
		{"several lines", []string{"pa.pas@2.2", "pa.pas@2.0", "pa.crd@2.0"},
			[]string{"pa.crd@2.0", "pa.pas@2.0", "pa.pas@2.2"}},
		{"a partial system declares only what it speaks", []string{"pa.pas@2.1"},
			[]string{"pa.pas@2.1"}},
		{"another contract's token is not a forwarded line", []string{"pa.pas@2.0", "pa.pdex@9.9", "x.y@1"},
			[]string{"pa.pas@2.0"}},
		{"a system's own PDex line is not declared", []string{"pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0", "pa.pdex@2.1"},
			[]string{"pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0"}},
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
	if err := RequireForwardedContractLine([]string{"pa.pdex@2.1"}); err == nil || !strings.Contains(err.Error(), "no CRD, DTR or PAS line") {
		t.Fatalf("pdex-only declaration: err = %v", err)
	}
	for _, ok := range [][]string{DefaultContractVersions(), {"pa.dtr@2.2"}, {" pa.crd@2.0", "pa.pdex@2.1"}} {
		if err := RequireForwardedContractLine(ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
}
