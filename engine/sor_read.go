package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ContextSystemOfRecord optionally extends SystemOfRecord with request cancellation
// and read failures. An absent record has a nil error; a failed read returns zero
// results and an error. Implementations must not expose protected backend details.
//
// OpenCoverageContext returns every Coverage the system of record holds for the
// member (none is an empty result), in the order the system returned them; the
// gateway, not the connector, decides what several of them mean.
type ContextSystemOfRecord interface {
	ResolvePatientContext(context.Context, string) (string, Demo, bool, error)
	PatientFHIRRefContext(context.Context, string) (string, bool, error)
	CoverageInforceContext(context.Context, string) (bool, string, error)
	ClinicalContextContext(context.Context, string) (shnsdk.ClinicalContext, bool, error)
	SupplementalReportContext(context.Context, string) ([]byte, bool, error)
	FacilityRecordsContext(context.Context, string) (map[string][]byte, bool, error)
	OpenOrderContext(context.Context, string) ([]byte, bool, error)
	OpenCoverageContext(context.Context, string) ([][]byte, error)
	ResolveByReferenceContext(context.Context, string) ([]byte, bool, error)
}

// ErrSystemOfRecordSignature: the configured connector has a method whose
// signature an earlier gateway release used. Such a connector no longer
// satisfies ContextSystemOfRecord, and the gateway would silently fall back
// to reads that cannot report failures, so New refuses it.
var ErrSystemOfRecordSignature = errors.New("system of record connector uses an earlier method signature")

// earlierCoverageReader is the OpenCoverageContext of earlier releases, which
// returned a single record.
type earlierCoverageReader interface {
	OpenCoverageContext(context.Context, string) ([]byte, bool, error)
}

// checkSystemOfRecordSignatures refuses a connector written against an
// earlier ContextSystemOfRecord.
func checkSystemOfRecordSignatures(sor SystemOfRecord) error {
	if _, earlier := sor.(earlierCoverageReader); earlier {
		return fmt.Errorf("%w: OpenCoverageContext must now return ([][]byte, error), every Coverage record the member has (an empty result for none), and the gateway decides what several records mean", ErrSystemOfRecordSignature)
	}
	return nil
}

// ReadSystemOfRecord prefers contextual reads while retaining source compatibility
// with existing connectors. Legacy calls cannot be interrupted once invoked, and
// the adapter cannot recover errors already discarded by a legacy connector.
//
// A gateway that keeps no system of record (NoSystemOfRecord) reads nothing,
// so its reader notes no call on the exchange record.
func ReadSystemOfRecord(sor SystemOfRecord) ContextSystemOfRecord {
	reader, ok := sor.(ContextSystemOfRecord)
	if !ok {
		reader = legacySoRReader{sor: sor}
	}
	if !hasSystemOfRecord(sor) {
		return reader
	}
	return recordingSoR{reader}
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

func (r legacySoRReader) OpenCoverageContext(ctx context.Context, key string) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a, found := r.sor.OpenCoverage(key); found {
		return [][]byte{a}, nil
	}
	return nil, nil
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

// recordingSoR notes each read of the participant's own system of record on
// the call's exchange record when the call is a leg this gateway answers
// (exchangerecord.go): a backend call with its latency and, when it failed,
// its class. The reads a provider gateway makes for its own caller are not
// the answering side's backend and are not noted. A read made inside another
// noted read (a decorator reading through) is noted once.
type recordingSoR struct{ inner ContextSystemOfRecord }

type sorReadNotedKey struct{}

// begin returns the context to read under and the function that notes the
// read's result, or a nil function when the read is not noted.
func (r recordingSoR) begin(ctx context.Context) (context.Context, func(error)) {
	ctx, note := noteRead(ctx)
	if note == nil {
		return ctx, nil
	}
	return ctx, func(err error) { note(sorClass(err)) }
}

// readEndedClass is a failed read's class when the read ended with the
// request it served (cancelled) or its deadline (timeout): a connector may
// report either as its system being unavailable, without saying why.
func readEndedClass(ctx context.Context, class string) string {
	if class == "" {
		return ""
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return BackendCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return BackendTimeout
	}
	return class
}

// noteRead returns the context to read the participant's system of record
// under and the function that notes the read with its class, or a nil
// function when the read is not noted: the call is not a leg this gateway
// answers, or the read runs inside one already noted.
func noteRead(ctx context.Context) (context.Context, func(class string)) {
	x := exchangeOf(ctx)
	if x == nil || ctx.Value(sorReadNotedKey{}) != nil || !x.answering() {
		return ctx, nil
	}
	start := x.clock()
	return context.WithValue(ctx, sorReadNotedKey{}, true), func(class string) {
		x.read(0, x.clock().Sub(start), readEndedClass(ctx, class))
	}
}

func (r recordingSoR) ResolvePatientContext(ctx context.Context, key string) (string, Demo, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, c, err := r.inner.ResolvePatientContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, c, err
}

func (r recordingSoR) PatientFHIRRefContext(ctx context.Context, key string) (string, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.PatientFHIRRefContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) CoverageInforceContext(ctx context.Context, key string) (bool, string, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.CoverageInforceContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) ClinicalContextContext(ctx context.Context, key string) (shnsdk.ClinicalContext, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.ClinicalContextContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) SupplementalReportContext(ctx context.Context, key string) ([]byte, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.SupplementalReportContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) FacilityRecordsContext(ctx context.Context, key string) (map[string][]byte, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.FacilityRecordsContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) OpenOrderContext(ctx context.Context, key string) ([]byte, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.OpenOrderContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, b, err
}

func (r recordingSoR) OpenCoverageContext(ctx context.Context, key string) ([][]byte, error) {
	ctx, note := r.begin(ctx)
	a, err := r.inner.OpenCoverageContext(ctx, key)
	if note != nil {
		note(err)
	}
	return a, err
}

func (r recordingSoR) ResolveByReferenceContext(ctx context.Context, ref string) ([]byte, bool, error) {
	ctx, note := r.begin(ctx)
	a, b, err := r.inner.ResolveByReferenceContext(ctx, ref)
	if note != nil {
		note(err)
	}
	return a, b, err
}
