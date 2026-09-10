package engine

import (
	"context"
	"errors"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ContextSystemOfRecord optionally extends SystemOfRecord with request cancellation
// and read failures. An absent record has a nil error; a failed read returns zero
// results and an error. Implementations must not expose protected backend details.
type ContextSystemOfRecord interface {
	ResolvePatientContext(context.Context, string) (string, Demo, bool, error)
	PatientFHIRRefContext(context.Context, string) (string, bool, error)
	CoverageInforceContext(context.Context, string) (bool, string, error)
	ClinicalContextContext(context.Context, string) (shnsdk.ClinicalContext, bool, error)
	SupplementalReportContext(context.Context, string) ([]byte, bool, error)
	FacilityRecordsContext(context.Context, string) (map[string][]byte, bool, error)
	OpenOrderContext(context.Context, string) ([]byte, bool, error)
	OpenCoverageContext(context.Context, string) ([]byte, bool, error)
	ResolveByReferenceContext(context.Context, string) ([]byte, bool, error)
}

// ReadSystemOfRecord prefers contextual reads while retaining source compatibility
// with existing connectors. Legacy calls cannot be interrupted once invoked, and
// the adapter cannot recover errors already discarded by a legacy connector.
func ReadSystemOfRecord(sor SystemOfRecord) ContextSystemOfRecord {
	if reader, ok := sor.(ContextSystemOfRecord); ok {
		return reader
	}
	return legacySoRReader{sor: sor}
}

type legacySoRReader struct{ sor SystemOfRecord }

func (r legacySoRReader) ResolvePatientContext(ctx context.Context, key string) (string, Demo, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", Demo{}, false, err
	}
	a, b, c := r.sor.ResolvePatient(key)
	return a, b, c, nil
}

func (r legacySoRReader) PatientFHIRRefContext(ctx context.Context, key string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	a, b := r.sor.PatientFHIRRef(key)
	return a, b, nil
}

func (r legacySoRReader) CoverageInforceContext(ctx context.Context, key string) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	a, b := r.sor.CoverageInforce(key)
	return a, b, nil
}

func (r legacySoRReader) ClinicalContextContext(ctx context.Context, key string) (shnsdk.ClinicalContext, bool, error) {
	if err := ctx.Err(); err != nil {
		return shnsdk.ClinicalContext{}, false, err
	}
	a, b := r.sor.ClinicalContext(key)
	return a, b, nil
}

func (r legacySoRReader) SupplementalReportContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a, b := r.sor.SupplementalReport(key)
	return a, b, nil
}

func (r legacySoRReader) FacilityRecordsContext(ctx context.Context, key string) (map[string][]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a, b := r.sor.FacilityRecords(key)
	return a, b, nil
}

func (r legacySoRReader) OpenOrderContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a, b := r.sor.OpenOrder(key)
	return a, b, nil
}

func (r legacySoRReader) OpenCoverageContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a, b := r.sor.OpenCoverage(key)
	return a, b, nil
}

func (r legacySoRReader) ResolveByReferenceContext(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a, b := r.sor.ResolveByReference(key)
	return a, b, nil
}

// SoRFailureKind identifies a safe category of backend read failure.
type SoRFailureKind string

const (
	SoRAuthenticationFailed SoRFailureKind = "authentication_failed"
	SoRUnavailable          SoRFailureKind = "unavailable"
	SoRInvalidResponse      SoRFailureKind = "invalid_response"
)

// SoRReadError intentionally contains no raw cause, URL, response, or identifier.
type SoRReadError struct{ Kind SoRFailureKind }

func (e *SoRReadError) Error() string {
	if e != nil {
		switch e.Kind {
		case SoRAuthenticationFailed:
			return "system of record authentication failed"
		case SoRUnavailable:
			return "system of record unavailable"
		}
	}
	return "system of record returned an invalid response"
}

// SoRFailureResponse translates a non-nil read error to a safe caller response.
// Backend credentials never become caller authentication errors. A 503 does not
// promise automatic retries or business-operation idempotency.
func SoRFailureResponse(err error) (int, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusServiceUnavailable, (&SoRReadError{Kind: SoRUnavailable}).Error()
	}
	var readErr *SoRReadError
	if errors.As(err, &readErr) && readErr != nil {
		if readErr.Kind == SoRUnavailable {
			return http.StatusServiceUnavailable, readErr.Error()
		}
		return http.StatusBadGateway, readErr.Error()
	}
	return http.StatusBadGateway, (&SoRReadError{Kind: SoRInvalidResponse}).Error()
}
