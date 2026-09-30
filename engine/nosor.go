package engine

import (
	"context"
	"errors"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// NoSystemOfRecord is the system of record of a payer-native gateway that
// keeps none: a payer whose own system answers every exchange behind
// PAYER_DAVINCI_BASE_URL has no member store for the gateway to read, and
// pointing the gateway at someone else's store would be the network supplying
// the participant's facts.
//
// It holds no one and reads nothing. Every member a request names is one it
// does not hold, so subject binding takes the carry path (resolveSubjectBinding:
// the identifier derived from the member id and the Patient the request
// carries), exactly as for any member a store does not hold, and the payer
// files its records under that binding, never under the token's subject,
// which the requester chooses. The answer fence reads a patient reference it
// does not hold.
// Eligibility answered from the payer's own records is not offered
// (handleEligibilityInbound). The other reads, which only a provider's,
// facility's or PHG's paths make, fail with ErrNoSystemOfRecord, so such a
// path refuses as a system-of-record failure instead of treating "no store"
// as "no data". (A request only a provider or facility serves, such as a
// federated query, never reaches a payer.)
func NoSystemOfRecord() SystemOfRecord { return noSystemOfRecord{} }

// ErrNoSystemOfRecord is the failure of a read a store-less gateway cannot
// answer.
var ErrNoSystemOfRecord = errors.New("this gateway keeps no system of record")

type noSystemOfRecord struct{}

// hasSystemOfRecord reports whether the gateway keeps a store it can read,
// looking through the observer's wrapper (observingSoR) around it.
func hasSystemOfRecord(sor SystemOfRecord) bool {
	if o, ok := sor.(observingSoR); ok {
		sor = o.inner
	}
	_, none := sor.(noSystemOfRecord)
	return !none
}

// The member lookups a payer leg makes answer "not held", without error.

func (noSystemOfRecord) ResolvePatient(string) (string, Demo, bool) { return "", Demo{}, false }
func (noSystemOfRecord) PatientFHIRRef(string) (string, bool)       { return "", false }
func (noSystemOfRecord) ResolvePatientContext(context.Context, string) (string, Demo, bool, error) {
	return "", Demo{}, false, nil
}
func (noSystemOfRecord) PatientFHIRRefContext(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// Every other read has no answer without a store.

func (noSystemOfRecord) CoverageInforce(string) (bool, string) { return false, "" }
func (noSystemOfRecord) ClinicalContext(string) (shnsdk.ClinicalContext, bool) {
	return shnsdk.ClinicalContext{}, false
}
func (noSystemOfRecord) SupplementalReport(string) ([]byte, bool)         { return nil, false }
func (noSystemOfRecord) FacilityRecords(string) (map[string][]byte, bool) { return nil, false }
func (noSystemOfRecord) OpenOrder(string) ([]byte, bool)                  { return nil, false }
func (noSystemOfRecord) OpenCoverage(string) ([]byte, bool)               { return nil, false }
func (noSystemOfRecord) ResolveByReference(string) ([]byte, bool)         { return nil, false }
func (noSystemOfRecord) CoverageInforceContext(context.Context, string) (bool, string, error) {
	return false, "", ErrNoSystemOfRecord
}
func (noSystemOfRecord) ClinicalContextContext(context.Context, string) (shnsdk.ClinicalContext, bool, error) {
	return shnsdk.ClinicalContext{}, false, ErrNoSystemOfRecord
}
func (noSystemOfRecord) SupplementalReportContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, ErrNoSystemOfRecord
}
func (noSystemOfRecord) FacilityRecordsContext(context.Context, string) (map[string][]byte, bool, error) {
	return nil, false, ErrNoSystemOfRecord
}
func (noSystemOfRecord) OpenOrderContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, ErrNoSystemOfRecord
}
func (noSystemOfRecord) OpenCoverageContext(context.Context, string) ([][]byte, error) {
	return nil, ErrNoSystemOfRecord
}
func (noSystemOfRecord) ResolveByReferenceContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, ErrNoSystemOfRecord
}

// Compile-time: it satisfies both the legacy and the contextual read.
var (
	_ SystemOfRecord        = noSystemOfRecord{}
	_ ContextSystemOfRecord = noSystemOfRecord{}
)
