package engine

import "fmt"

// ConformanceEnforcement is a participant's choice about which payload checks
// its own gateway runs and enforces. Its zero value is the published default,
// none, for both direct construction and environment-based configuration.
type ConformanceEnforcement int

const (
	EnforcementNone ConformanceEnforcement = iota
	EnforcementObserve
	EnforcementBasic
	EnforcementStrict
)

func (e ConformanceEnforcement) String() string {
	switch e {
	case EnforcementNone:
		return "none"
	case EnforcementObserve:
		return "observe"
	case EnforcementBasic:
		return "basic"
	case EnforcementStrict:
		return "strict"
	default:
		return fmt.Sprintf("invalid(%d)", int(e))
	}
}

// ParseConformanceEnforcement maps a setting's value to a level. It accepts
// exactly the four published values and errors on anything else, including
// empty. The gateway/app env loader does not call it for an absent setting;
// its zero-valued configuration already means none.
func ParseConformanceEnforcement(s string) (ConformanceEnforcement, error) {
	switch s {
	case "none":
		return EnforcementNone, nil
	case "observe":
		return EnforcementObserve, nil
	case "basic":
		return EnforcementBasic, nil
	case "strict":
		return EnforcementStrict, nil
	}
	return EnforcementNone, fmt.Errorf("CONFORMANCE_ENFORCEMENT must be none, observe, basic, or strict, got %q", s)
}

func (e ConformanceEnforcement) valid() bool {
	return e >= EnforcementNone && e <= EnforcementStrict
}

// CheckKind and its four constants are already declared in finding.go — do
// not redeclare them here.

// Verdict is what the check saw. Decide is total over both so the table can be
// tested whole.
type Verdict int

const (
	VerdictValid Verdict = iota
	VerdictInvalid
)

// Decision is what the gateway does about an invalid verdict. Its zero value
// remains Refuse so an uninitialized legacy decision cannot permit a message.
type Decision int

const (
	Refuse Decision = iota
	Record
)

// String renders what the gateway did to the MESSAGE, for a log line an
// operator reads — not the identifier's own name. Record's finding is
// recorded, but what the operator needs to know is that the message itself
// was relayed unchanged; "recorded" would describe the finding's fate and
// leave the message's fate implicit. The wording is deliberate, not a
// mismatch with the Refuse/Record identifiers above.
func (d Decision) String() string {
	if d == Record {
		return "relayed"
	}
	return "refused"
}

// findingEmitterBinder is implemented by a responder that records conformance
// findings. A responder that does not implement it (a test wrapper, say)
// still certifies and still refuses at strict; it simply records nothing.
type findingEmitterBinder interface {
	bindFindingEmitter(func(ConformanceFinding))
}

// alwaysRefusedCDSRules are the CDS Hooks structural rules that refuse at
// every level: the reader that follows the certifier (crdAnswerOutcome, which
// parses with ParseCRDResponse) needs the answer's shape. Breaking one of
// these does not make an answer non-conformant so much as unreadable.
var alwaysRefusedCDSRules = map[string]bool{
	"response.json":   true,
	"response.object": true,
	"line":            true,
}

// ConformancePolicy answers, for one check, whether an invalid verdict refuses
// the message or is only recorded. It is CONFIGURATION: built once at boot
// from the setting, held on the gateway config, never request-scoped, and it
// never sees payload bytes.
type ConformancePolicy struct{ level ConformanceEnforcement }

func NewConformancePolicy(level ConformanceEnforcement) ConformancePolicy {
	if !level.valid() {
		panic("unvalidated conformance configuration")
	}
	return ConformancePolicy{level: level}
}

func (p ConformancePolicy) Level() ConformanceEnforcement { return p.level }

// Decide keeps existing certifier callers compiling while they migrate to
// classed rules. Their optional runtime checks are deeper checks; mandatory
// adaptation and readability failures remain independent of participant
// conformance policy.
func (p ConformancePolicy) Decide(kind CheckKind, rule string, v Verdict) Decision {
	if v != VerdictInvalid {
		return Record
	}
	switch {
	case kind == KindFHIRBridged:
		return Refuse
	case kind == KindCDSEnvelope && alwaysRefusedCDSRules[rule]:
		return Refuse
	case p.Action(CheckDeep) == CheckEnforce:
		return Refuse
	}
	return Record
}
