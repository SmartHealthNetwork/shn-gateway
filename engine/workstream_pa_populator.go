//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// FilledItem is the gateway-engine-LOCAL attribution surface for a DTR auto-filled
// QR item (the scenario route's answer, QRItems field). The SDK's FillQuestionnaire drops the
// FilledItem summary (UI-only); the gateway reconstructs it via fillSummary
// from the ClinicalContext. JSON tags are byte-for-byte compatible with the original
// dtr.FilledItem shape so the scenario response format is unchanged.
type FilledItem struct {
	LinkID    string `json:"linkId"`
	Answer    string `json:"answer"`
	Origin    string `json:"origin"`
	SourceRef string `json:"sourceRef,omitempty"`
}

// Populator produces a populated QuestionnaireResponse for a DTR leg. It is a
// PROVIDER-LOCAL connector seam (runs inside the holder's trust domain against the
// provider's data — NOT across a sealed Hub leg), selected by config. Backends:
//   - managedPopulator — in-house FillQuestionnaire (demo green-keeper; default)
//   - nativePopulator  — forward to an SDC Questionnaire/$populate endpoint (pass-through)
//   - (future) operated CQL engine — same interface, config-only drop-in
//
// Populate takes the full $questionnaire-package (so a backend that needs the bundled
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
	// Order is the order the questionnaire is for, as the payer returned it with its
	// coverage information in the coverage requirements answer (the order sent when the
	// payer returned none). OrderRef names it.
	Order    []byte
	Authored time.Time
	// Line is the DTR contract line ("2.0"/"2.1"/"2.2") the fetched
	// $questionnaire-package was pulled at — selected once at the DTR-fetch leg
	// (module1_originate.go's select-before-build, F7) and threaded here so the QR the
	// Populator builds matches the wire shape the fetch already committed to
	// (closes an earlier KNOWN GAP where every authored QR was frozen
	// at 2.0 regardless of the selected line). Empty delegates to "2.0" — the
	// pre-line-threading default — so this stays an ADDITIVE field: a kit/custom
	// Populator implementation that never sets it keeps building at 2.0 exactly
	// as before, never a required-but-forgotten wiring hole.
	Line string
}

// errNoClinicalContext is a sentinel so the consumer maps "no clinical data for member"
// to 502 (a data fault), distinct from a managed fill marshal error (500).
var errNoClinicalContext = errors.New("no clinical context for member")

// errPopulateUpstream is a sentinel: a native $populate transport error, non-2xx, or
// unparsable response. The consumer maps it to 502. Defined here (the seam file),
// beside errNoClinicalContext, for statusForPopulateErr (module1_originate.go) and the
// native populator (workstream_pa_nativepopulate.go).
var errPopulateUpstream = errors.New("$populate upstream failed")

// errPopulateForeignSubject is the native foreign-subject fence: the operated engine returned a QR
// about a patient other than the one we asked to populate. It reads as the gateway's own subject
// fence does (msgFillForeignSubject): SHN's own fill failing its check, refused 422 (refuseOwnFill).
var errPopulateForeignSubject = errors.New(msgFillForeignSubject)

// The answers to a fill that fails this gateway's own check of it (refuseOwnFill).
const (
	msgFillForeignSubject   = "populated QR subject does not match patient"
	msgFillForeignCanonical = "populated QR questionnaire does not match canonical"
)

// managedPopulator is the in-house backend: today's ClinicalContext + FillQuestionnaire,
// byte-preserved behind the seam. The demo GREEN-KEEPER — FillQuestionnaire fails
// loud on any questionnaire it does not recognize, so this is NOT a general legacy fallback (the
// genuine legacy answer is an operated $populate, backend #3).
type managedPopulator struct {
	sor SystemOfRecord
}

func newManagedPopulator(sor SystemOfRecord) *managedPopulator {
	return &managedPopulator{sor: sor}
}

// WithPopulator sets the gateway's DTR populator (provider-local) in place of
// the managed populator over the participant's system of record. The native
// pass-through backend is set this way from config (PROVIDER_DTR_NATIVE, or an
// operated-populate origination profile); a test may set its own. A nil p
// keeps the managed populator.
func WithPopulator(p Populator) Option {
	return func(g *Gateway) {
		if st := g.paState(); st != nil && p != nil {
			st.populator = p
		}
	}
}

// populator is the gateway's DTR populator, or nil on a gateway assembled
// without New.
func (g *Gateway) populator() Populator {
	if st := g.paState(); st != nil {
		return st.populator
	}
	return nil
}

func (m *managedPopulator) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	q, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		return nil, nil, err // no-Questionnaire → consumer maps to 502
	}
	cc, ok, readErr := ReadSystemOfRecord(m.sor).ClinicalContextContext(ctx, pc.Member)
	if readErr != nil {
		return nil, nil, safeSoRError(readErr)
	}
	if !ok {
		return nil, nil, errNoClinicalContext
	}
	// Thread the selected DTR line (closes an earlier KNOWN GAP: every authored
	// QR used to be frozen at the 2.0 shape regardless of the selected line — the 2.2
	// two-RI run showed wrong-line QR bytes could pass SILENTLY, the UC-03 auto-fill site
	// this closes). "" (the pre-line-threading caller shape) delegates to "2.0" — PopulateContext.Line's
	// documented additive default.
	line := pc.Line
	if line == "" {
		line = "2.0"
	}
	qr, err := shnsdk.FillQuestionnaireAtLine(line, q, cc, shnsdk.QRContext{
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

// fillSummary reconstructs the []FilledItem summary from the ClinicalContext —
// the same leaves shnsdk.FillQuestionnaire answers, in the same order (the fill
// carries no per-item summary, so the scenario answer's summary is rebuilt here). Items with
// a negative/absent flag (prior-surgery=false, etc.) are omitted, matching the
// fill. functional-status-oswestry is intentionally absent (no local source).
func fillSummary(cc shnsdk.ClinicalContext) []FilledItem {
	var out []FilledItem
	out = append(out, FilledItem{
		LinkID:    "conservative-therapy-weeks",
		Answer:    itoa(cc.ConservativeTherapyWeeks),
		Origin:    "auto",
		SourceRef: cc.ConservativeTherapyRef,
	})
	out = append(out, FilledItem{
		LinkID:    "neuro-deficit",
		Answer:    boolStr(cc.NeuroDeficit),
		Origin:    "auto",
		SourceRef: cc.NeuroDeficitRef,
	})
	out = append(out, FilledItem{
		LinkID:    "prior-imaging",
		Answer:    boolStr(cc.PriorImaging),
		Origin:    "auto",
		SourceRef: cc.PriorImagingRef,
	})
	if cc.PriorSurgery {
		out = append(out, FilledItem{
			LinkID:    "prior-surgery",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.PriorSurgeryRef,
		})
	}
	if cc.HighDisability {
		out = append(out, FilledItem{
			LinkID:    "high-disability",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.HighDisabilityRef,
		})
	}
	if cc.PatientReported {
		out = append(out, FilledItem{
			LinkID: "patient-reported-required",
			Answer: "true",
			Origin: "auto",
		})
	}
	return out
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
