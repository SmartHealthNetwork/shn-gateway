package engine

import (
	"slices"
	"testing"
)

func TestNativeReceiveOnlyVersions(t *testing.T) {
	for _, tc := range []struct {
		name, role, base string
		tokens           []string
		want             []string
		bad              bool
	}{
		{"configured PAS", "payer", "https://payer.example/fhir", []string{"pa.pas@2.0", "pa.pas@9.9"}, []string{"pa.pas@2.0", "pa.pas@9.9"}, false},
		{"no backend", "payer", "", []string{"pa.pas@9.9"}, nil, true},
		{"wrong role", "provider", "https://payer.example/fhir", []string{"pa.pas@9.9"}, nil, true},
		{"unsupported operation family", "payer", "https://payer.example/fhir", []string{"pa.pdex@9.9"}, nil, true},
		{"malformed", "payer", "https://payer.example/fhir", []string{"pa.pas@wrong"}, nil, true},
		{"URL has credentials", "payer", "https://user@payer.example/fhir", []string{"pa.pas@9.9"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NativeReceiveOnlyVersions(tc.role, tc.base, tc.tokens)
			if (err != nil) != tc.bad || (!tc.bad && !slices.Equal(got, tc.want)) {
				t.Fatalf("got %q, err %v; want %q, bad %v", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestNativePublishedContractVersionsFiltersUnavailableKnownLines(t *testing.T) {
	got, err := NativePublishedContractVersions("payer", "https://backend.example/fhir", []string{"pa.pas@9.9"}, []string{"pa.crd@2.0", "pa.pas@2.0", "pa.pdex@2.1"})
	if err != nil || !slices.Equal(got, []string{"pa.pdex@2.1", "pa.pas@9.9"}) {
		t.Fatalf("published=%v err=%v", got, err)
	}
}

func TestNativePublishedContractVersionsIncludesBackendOnlyKnownLine(t *testing.T) {
	build := []string{"pa.pas@2.0", "pa.pdex@2.1"}
	got, err := NativePublishedContractVersions("payer", "https://backend.example/fhir", []string{"pa.pas@2.2"}, build)
	if err != nil || !slices.Equal(got, []string{"pa.pdex@2.1", "pa.pas@2.2"}) {
		t.Fatalf("published=%v err=%v", got, err)
	}
	if !slices.Equal(build, []string{"pa.pas@2.0", "pa.pdex@2.1"}) {
		t.Fatalf("backend receive altered builder declaration: %v", build)
	}
}
