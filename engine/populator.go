package engine

import (
	"context"
	"errors"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Populator produces a populated QuestionnaireResponse for a DTR leg. It is a
// PROVIDER-LOCAL connector seam (runs inside the holder's trust domain against the
// provider's data — NOT across a sealed Hub leg), selected by config. Backends:
//   - managedPopulator — in-house FillQuestionnaire (sandbox/demo green-keeper; default)
//   - nativePopulator  — forward to an SDC Questionnaire/$populate endpoint (pass-through)
//   - (future) operated CQL engine — same interface, config-only drop-in
//
// Populate takes the full §6.2 package (so a backend that needs the bundled
// Libraries/ValueSets can use them) and returns the populated QR JSON plus an OPTIONAL
// per-item fill summary (nil when the backend did not itself fill — e.g. a remote
// $populate engine did, so the gateway has no per-item attribution to surface).
type Populator interface {
	Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) (qrJSON []byte, fill []FilledItem, err error)
}

// PopulateContext carries the references + clock the population needs.
type PopulateContext struct {
	Member     string
	PatientRef string // logical SHN ref ("Patient/<member>") — the QR subject downstream uses
	// SubjectFHIRRef is the FHIR-store-resolvable Patient ref for the operated $populate subject
	// (SoR.PatientFHIRRef — possibly a scoped id). Defaults to PatientRef. The native backend sends
	// this as the subject; the consumer fence verifies the returned QR.subject against it, then
	// normalizes QR.subject → PatientRef. The managed backend ignores it (it fills by PatientRef).
	SubjectFHIRRef string
	CoverageRef    string
	OrderRef       string
	Authored       time.Time
}

// errNoClinicalContext is a sentinel so the consumer maps "no clinical data for member"
// to 502 (a data fault), distinct from a managed fill marshal error (500).
var errNoClinicalContext = errors.New("engine: no clinical context for member")

// errPopulateUpstream is a sentinel: a native $populate transport error, non-2xx, or
// unparsable response. The consumer maps it to 502. Defined here (the seam file) so
// statusForPopulateErr (originate.go) can reference it before nativepopulate.go exists.
var errPopulateUpstream = errors.New("engine: $populate upstream failed")

// errPopulateForeignSubject is the native foreign-subject fence: the operated engine returned a QR
// about a patient other than the one we asked to populate. Maps to 502. The message is kept stable
// ("...subject does not match...") since it is the participant-visible rejection reason.
var errPopulateForeignSubject = errors.New("engine: populated QR subject does not match patient")

// managedPopulator is the in-house backend: today's ClinicalContext + FillQuestionnaire,
// byte-preserved behind the seam. The sandbox/demo GREEN-KEEPER — FillQuestionnaire fails
// loud on any non-sandbox questionnaire, so this is NOT a general legacy fallback (the
// genuine legacy answer is an operated $populate, backend #3).
type managedPopulator struct {
	sor SystemOfRecord
}

func newManagedPopulator(sor SystemOfRecord) *managedPopulator {
	return &managedPopulator{sor: sor}
}

func (m *managedPopulator) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	q, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		return nil, nil, err // no-Questionnaire → consumer maps to 502
	}
	cc, ok := m.sor.ClinicalContext(pc.Member)
	if !ok {
		return nil, nil, errNoClinicalContext
	}
	qr, err := shnsdk.FillQuestionnaire(q, cc, shnsdk.QRContext{
		PatientRef:  pc.PatientRef,
		CoverageRef: pc.CoverageRef,
		OrderRef:    pc.OrderRef,
		Authored:    pc.Authored,
	})
	if err != nil {
		return nil, nil, err // managed fill error → consumer maps to 500
	}
	return qr, fillSummary(cc), nil
}
