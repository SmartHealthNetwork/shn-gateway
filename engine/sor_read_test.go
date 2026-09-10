package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSoRLegacyResults(t *testing.T) {
	legacy := &censusSoR{}
	reader := ReadSystemOfRecord(legacy)
	for _, member := range []string{"MBR-UC04", "MBR-UC05", "absent"} {
		t.Run("ResolvePatient/"+member, func(t *testing.T) {
			a, b, c, err := reader.ResolvePatientContext(context.Background(), member)
			wanta, wantb, wantc := legacy.ResolvePatient(member)
			if err != nil || !reflect.DeepEqual([]any{a, b, c}, []any{wanta, wantb, wantc}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("PatientFHIRRef/"+member, func(t *testing.T) {
			a, b, err := reader.PatientFHIRRefContext(context.Background(), member)
			wanta, wantb := legacy.PatientFHIRRef(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("CoverageInforce/"+member, func(t *testing.T) {
			a, b, err := reader.CoverageInforceContext(context.Background(), member)
			wanta, wantb := legacy.CoverageInforce(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("ClinicalContext/"+member, func(t *testing.T) {
			a, b, err := reader.ClinicalContextContext(context.Background(), member)
			wanta, wantb := legacy.ClinicalContext(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("SupplementalReport/"+member, func(t *testing.T) {
			a, b, err := reader.SupplementalReportContext(context.Background(), member)
			wanta, wantb := legacy.SupplementalReport(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("FacilityRecords/"+member, func(t *testing.T) {
			a, b, err := reader.FacilityRecordsContext(context.Background(), member)
			wanta, wantb := legacy.FacilityRecords(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("OpenOrder/"+member, func(t *testing.T) {
			a, b, err := reader.OpenOrderContext(context.Background(), member)
			wanta, wantb := legacy.OpenOrder(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("OpenCoverage/"+member, func(t *testing.T) {
			a, b, err := reader.OpenCoverageContext(context.Background(), member)
			wanta, wantb := legacy.OpenCoverage(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
		t.Run("ResolveByReference/"+member, func(t *testing.T) {
			a, b, err := reader.ResolveByReferenceContext(context.Background(), member)
			wanta, wantb := legacy.ResolveByReference(member)
			if err != nil || !reflect.DeepEqual([]any{a, b}, []any{wanta, wantb}) {
				t.Fatalf("legacy result changed: %v", err)
			}
		})
	}
}

// A nil embedded implementation panics if a canceled adapter invokes any legacy read.
func TestSoRCanceledLegacyNeverCalled(t *testing.T) {
	var legacy struct{ SystemOfRecord }
	reader := ReadSystemOfRecord(legacy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("ResolvePatient", func(t *testing.T) {
		_, _, _, err := reader.ResolvePatientContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("PatientFHIRRef", func(t *testing.T) {
		_, _, err := reader.PatientFHIRRefContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("CoverageInforce", func(t *testing.T) {
		_, _, err := reader.CoverageInforceContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("ClinicalContext", func(t *testing.T) {
		_, _, err := reader.ClinicalContextContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("SupplementalReport", func(t *testing.T) {
		_, _, err := reader.SupplementalReportContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("FacilityRecords", func(t *testing.T) {
		_, _, err := reader.FacilityRecordsContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("OpenOrder", func(t *testing.T) {
		_, _, err := reader.OpenOrderContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("OpenCoverage", func(t *testing.T) {
		_, _, err := reader.OpenCoverageContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("ResolveByReference", func(t *testing.T) {
		_, _, err := reader.ResolveByReferenceContext(ctx, "member")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

type preferredSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
}

func TestSoRPrefersContextCapability(t *testing.T) {
	sor := &preferredSoR{}
	if ReadSystemOfRecord(sor) != sor {
		t.Fatal("context capability replaced by legacy adapter")
	}
}

func TestSoRFailureResponse(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"authentication", &SoRReadError{Kind: SoRAuthenticationFailed}, 502},
		{"invalid", &SoRReadError{Kind: SoRInvalidResponse}, 502},
		{"unavailable", &SoRReadError{Kind: SoRUnavailable}, 503},
		{"unknown", errors.New("private-upstream-sentinel"), 502},
		{"deadline", context.DeadlineExceeded, 503},
		{"canceled", context.Canceled, 503},
		{"wrapped", fmt.Errorf("private-upstream-sentinel: %w", &SoRReadError{Kind: SoRUnavailable}), 503},
		{"unknown kind", &SoRReadError{Kind: SoRFailureKind("private-upstream-sentinel")}, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, message := SoRFailureResponse(tc.err)
			if status != tc.status || message == "" || strings.Contains(message, "sentinel") {
				t.Fatalf("status/message = %d %q", status, message)
			}
		})
	}
	for _, kind := range []SoRFailureKind{SoRAuthenticationFailed, SoRInvalidResponse, SoRUnavailable, "private-upstream-sentinel"} {
		if message := (&SoRReadError{Kind: kind}).Error(); message == "" || strings.Contains(message, "sentinel") {
			t.Fatalf("unsafe typed error: %q", message)
		}
	}
}
