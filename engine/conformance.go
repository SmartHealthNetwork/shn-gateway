package engine

import "fmt"

// ConformanceEnforcement is a participant's choice about what its own gateway
// does with a conformance defect it finds. The network always validates and
// always records; only strict refuses.
//
// The ZERO VALUE IS STRICT, deliberately: the default flip to none lives in
// exactly one place, the gateway/app env loader, so no in-process construction
// of engine.Config anywhere in this repo can silently become permissive.
type ConformanceEnforcement int

const (
	EnforcementStrict ConformanceEnforcement = iota
	EnforcementNone
)

func (e ConformanceEnforcement) String() string {
	if e == EnforcementNone {
		return "none"
	}
	return "strict"
}

// ParseConformanceEnforcement maps a setting's value to a level. It accepts
// exactly the two published values and errors on anything else, including
// empty: the gateway/app env loader never calls it for an absent
// CONFORMANCE_ENFORCEMENT, and sets EnforcementNone itself instead (the
// published default). No third value is accepted — a middle level is a future
// set of table rows, not a reserved word.
func ParseConformanceEnforcement(s string) (ConformanceEnforcement, error) {
	switch s {
	case "strict":
		return EnforcementStrict, nil
	case "none":
		return EnforcementNone, nil
	}
	return EnforcementStrict, fmt.Errorf("CONFORMANCE_ENFORCEMENT must be none or strict, got %q", s)
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

// Decision is what the gateway does about an invalid verdict. Refuse is the
// zero value for the same reason EnforcementStrict is.
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
	return ConformancePolicy{level: level}
}

func (p ConformancePolicy) Level() ConformanceEnforcement { return p.level }

// Decide is the whole table.
func (p ConformancePolicy) Decide(kind CheckKind, rule string, v Verdict) Decision {
	if v != VerdictInvalid {
		return Record
	}
	switch {
	case kind == KindFHIRBridged:
		return Refuse
	case kind == KindCDSEnvelope && alwaysRefusedCDSRules[rule]:
		return Refuse
	case p.level == EnforcementStrict:
		return Refuse
	}
	return Record
}
