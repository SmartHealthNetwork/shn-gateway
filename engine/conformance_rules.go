package engine

import "context"

// CheckClass identifies whether a conformance rule is safe to enforce at the
// participant's basic level or belongs to deeper clinical interpretation.
type CheckClass string

const (
	CheckStructural CheckClass = "structural"
	CheckDeep       CheckClass = "deep"
)

// CheckState is the closed outcome vocabulary returned by a conformance rule.
type CheckState string

const (
	CheckValid         CheckState = "valid"
	CheckInvalid       CheckState = "invalid"
	CheckUnavailable   CheckState = "unavailable"
	CheckNotApplicable CheckState = "not_applicable"
)

// CheckAction is the participant-selected treatment for a class of checks.
type CheckAction string

const (
	CheckOff     CheckAction = "off"
	CheckObserve CheckAction = "observe"
	CheckEnforce CheckAction = "enforce"
)

// CheckResult is a rule result with a closed, redaction-safe diagnostic code.
type CheckResult struct {
	State    CheckState
	Code     string       // closed, redaction-safe diagnostic code
	Severity string       // "error" or "warning"
	Issues   []CheckIssue // safe details retained independently of the summary verdict
}

// CheckIssue is bounded diagnostic metadata, never a raw checker message.
type CheckIssue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
}

// Action returns the configured action for a class of conformance checks.
func (p ConformancePolicy) Action(class CheckClass) CheckAction {
	switch p.level {
	case EnforcementNone:
		return CheckOff
	case EnforcementObserve:
		return CheckObserve
	case EnforcementBasic:
		if class == CheckStructural {
			return CheckEnforce
		}
		return CheckObserve
	case EnforcementStrict:
		return CheckEnforce
	default:
		panic("unvalidated conformance configuration")
	}
}

// ConformanceRuleSet identifies the blocking semantics of this rule table.
// Changing those semantics requires a new version and a migration note.
const ConformanceRuleSet = "participant-conformance/1"

// CheckInput describes one message at a verified participant boundary. Response
// applicability uses the actual status and independent producer declaration.
type CheckInput struct {
	Exchange        ExchangeContext
	Direction       string
	Status          int
	Body            []byte
	DeclaredVersion string
	finding         findingContext // scalar snapshot; never the request context
	decoded         *structuralDocument
	evidence        *contentEvidence
	observation     *observationBudget
}

// ConformanceRule checks content without owning a writer or durable store.
type ConformanceRule struct {
	ID      string
	Class   CheckClass
	Applies func(CheckInput) bool
	Check   func(context.Context, CheckInput) CheckResult
}
