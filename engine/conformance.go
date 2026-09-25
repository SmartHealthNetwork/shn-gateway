package engine

import "fmt"

// ConformanceEnforcement is a participant's choice about what its own gateway
// does with the messages it carries. none runs no conformance check at all and
// relays; observe runs every check, records each defect as a finding and
// relays; structural runs every check, refuses a message whose structure is broken
// and records every other defect as observe does; strict refuses a defect.
// SHN's own bridged edit is checked and refused at every level.
//
// The ZERO VALUE IS STRICT, deliberately: the default flip to none lives in
// exactly one place, the gateway/app env loader, so no in-process construction
// of engine.Config anywhere in this repo can silently become permissive.
type ConformanceEnforcement int

const (
	EnforcementStrict ConformanceEnforcement = iota
	EnforcementNone
	EnforcementObserve
	EnforcementStructural
)

func (e ConformanceEnforcement) String() string {
	switch e {
	case EnforcementNone:
		return "none"
	case EnforcementObserve:
		return "observe"
	case EnforcementStructural:
		return "structural"
	}
	return "strict"
}

// ParseConformanceEnforcement maps a setting's value to a level. It accepts
// exactly the published values and errors on anything else, including empty:
// the gateway/app env loader never calls it for an absent
// CONFORMANCE_ENFORCEMENT, and sets EnforcementObserve itself instead (the
// published default). A level that is not published is a boot error, never a
// reserved word.
func ParseConformanceEnforcement(s string) (ConformanceEnforcement, error) {
	switch s {
	case "strict":
		return EnforcementStrict, nil
	case "none":
		return EnforcementNone, nil
	case "observe":
		return EnforcementObserve, nil
	case "structural":
		return EnforcementStructural, nil
	}
	return EnforcementStrict, fmt.Errorf("CONFORMANCE_ENFORCEMENT must be none, observe, structural or strict, got %q", s)
}

// CheckKind and its four constants are already declared in finding.go — do
// not redeclare them here.

// Verdict is what the check saw. Decide is total over all four so the table
// can be tested whole. VerdictInvalid is a defect that breaks the message's
// structure, or one the check could not classify (fail closed).
// VerdictUnavailable is a check that could not run: the validator failed, or
// no validator lane serves the line. VerdictDeeper is a FHIR $validate whose
// every defect the choke point classified as a deeper rule's (classifyFHIR):
// the message is readable, and only strict refuses it.
type Verdict int

const (
	VerdictValid Verdict = iota
	VerdictInvalid
	VerdictUnavailable
	VerdictDeeper
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

// unreadableCDSRules are the CDS Hooks rules whose violation leaves an answer
// unreadable rather than non-conformant: not one JSON object, or at a line
// this SDK does not know. Whether they refuse below strict is one table row,
// cdsUnreadableRefusesBelowStrict.
var unreadableCDSRules = map[string]bool{
	"response.json":   true,
	"response.object": true,
	"line":            true,
}

// structuralCDSDeeperRules are the CDS Hooks error rules that judge a readable
// answer's meaning (summary length, CRD topic, selection behavior, an action's
// resource): recorded at structural. Every other error rule is structural in CDS
// Hooks terms (an unreadable answer, or a required member missing or of the
// wrong type) and refuses at structural, as does any rule the table does not name
// (fail closed). SHOULD-level rules never reach Decide.
var structuralCDSDeeperRules = map[string]bool{
	"card.summary.length":                true,
	"card.source.topic":                  true,
	"card.selectionBehavior":             true,
	"card.selectionBehavior.at-most-one": true,
	"action.resource":                    true,
}

// cdsUnreadableRefusesBelowStrict is the table row for an unreadable CDS Hooks
// answer at none and observe. None and observe refuse nothing a participant's
// payload is judged by, so the answer is relayed (and recorded at observe).
// Read only by newConformancePolicy.
const cdsUnreadableRefusesBelowStrict = false

// ConformancePolicy answers, for one check, whether it runs and whether a
// defect it finds refuses the message or is only recorded. It is
// CONFIGURATION: built once at boot from the setting, held on the gateway
// config, never request-scoped, and it never sees payload bytes. The zero value
// is strict.
type ConformancePolicy struct {
	level                        ConformanceEnforcement
	unreadableRefusesBelowStrict bool
}

func NewConformancePolicy(level ConformanceEnforcement) ConformancePolicy {
	return newConformancePolicy(level, cdsUnreadableRefusesBelowStrict)
}

func newConformancePolicy(level ConformanceEnforcement, unreadableRefusesBelowStrict bool) ConformancePolicy {
	return ConformancePolicy{level: level, unreadableRefusesBelowStrict: unreadableRefusesBelowStrict}
}

func (p ConformancePolicy) Level() ConformanceEnforcement { return p.level }

// The network rules. Binding the leg's authority and consent to one patient is
// network level; the internal consistency of the payload is not. A body that
// can be read two ways cannot be carried, owned or edited faithfully, so a
// repeated member name is message integrity, network level too. Network rules
// refuse at every level.
//
// networkRules is what Decide reads for a check routed through the guard as
// KindNetwork. RuleSubjectPCI and RuleSubjectToken name the subject bind for
// the record: their sites refuse unconditionally rather than through the
// guard (without a pci the leg has no authority to carry, whatever the level),
// so moving them between the tables changes nothing. For a rule that is
// routed through the guard, moving it between this table and contentRules
// changes its level behavior and nothing else.
const (
	// RuleSubjectPCI: the request names a subject the requesting gateway binds
	// to a PCI; a receiving gateway that requires known members
	// (Config.RequireKnownMembers) also refuses a member it does not hold.
	// Without a PCI the leg has no authority to carry, so this row cannot relay.
	RuleSubjectPCI = "subject.pci"
	// RuleSubjectToken: the request's patient is the patient the leg's token
	// authorizes. The federated-query and patient-authored DTR legs, and
	// eligibility a payer answers from its records, refuse a difference. The Da
	// Vinci CRD, DTR and PAS legs, and eligibility carried to a payer's own
	// endpoint, do not compare them: the payer handles the member the request names as it would
	// directly, and keys everything it records about the exchange by its own
	// binding of that member (bindInboundSubject), never by the token.
	RuleSubjectToken = "subject.token"
	// RuleDuplicateKey: a repeated JSON member name, exactly or under case
	// folding, anywhere in a body the gateway reads.
	RuleDuplicateKey = "json.duplicate-key"
)

// Content rules: checks of a participant's message that judge its shape or
// internal consistency, never who may send it to whom.
const (
	// RulePatientMixed: another patient referenced inside one request.
	RulePatientMixed = "patient.mixed"
	// RulePatientAnswer: an answer about a patient other than the request's,
	// or one that names its patient otherwise than by reference.
	RulePatientAnswer = "patient.answer"
	// RuleRequestShape: a request missing a required element or not the
	// operation's shape.
	RuleRequestShape = "request.shape"
	// RulePrefetchFill: CRD prefetch this gateway could not fill from the
	// participant's own system of record.
	RulePrefetchFill = "prefetch.fill"
	// RuleAttestation: a QuestionnaireResponse item whose FR-16/FR-17
	// attestation is incomplete.
	RuleAttestation = "qr.attestation"
	// RuleUpdateProvenance: a PAS update whose FR-32 provenance is incomplete.
	RuleUpdateProvenance = "pas.update-provenance"
	// RuleAnswerShape: an answer this gateway cannot read (graph, decision,
	// inquiry answer, questionnaire package).
	RuleAnswerShape = "answer.shape"
	// RuleEOBDecision: a payer decision the EOB rules refuse to state.
	RuleEOBDecision = "eob.decision"
	// RuleInsurer: a Claim.insurer reference that does not resolve.
	RuleInsurer = "claim.insurer"
)

var networkRules = map[string]bool{RuleSubjectPCI: true, RuleSubjectToken: true, RuleDuplicateKey: true}

// structuralContentRefuses are the content rules structural refuses: a request or an
// answer this gateway cannot read is structurally broken. Every other content
// rule judges consistency or a business rule and is recorded at structural. A rule
// in neither table refuses at structural (fail closed).
var structuralContentRefuses = map[string]bool{RuleRequestShape: true, RuleAnswerShape: true}

var contentRules = map[string]bool{
	RulePatientMixed: true, RulePatientAnswer: true, RuleRequestShape: true,
	RulePrefetchFill: true, RuleAttestation: true, RuleUpdateProvenance: true, RuleAnswerShape: true,
	RuleEOBDecision: true, RuleInsurer: true,
}

// Runs reports whether a check runs at all. At none only SHN's own bridged
// edit and the network rules are checked; a check that does not run makes
// no validator call and records no finding.
func (p ConformancePolicy) Runs(kind CheckKind, rule string) bool {
	switch {
	case kind == KindFHIRBridged, p.level != EnforcementNone:
		return true
	case kind == KindNetwork, networkRules[rule]:
		return true
	}
	return kind == KindCDSEnvelope && unreadableCDSRules[rule] && p.unreadableRefusesBelowStrict
}

// RunsKind reports whether any check of kind runs, so a caller can skip the
// work of checking when none of its rules would run.
func (p ConformancePolicy) RunsKind(kind CheckKind) bool {
	switch {
	case kind == KindFHIRBridged, kind == KindNetwork, p.level != EnforcementNone:
		return true
	}
	return kind == KindCDSEnvelope && p.unreadableRefusesBelowStrict
}

// Decide is the whole table: what a check that ran does about what it saw.
func (p ConformancePolicy) Decide(kind CheckKind, rule string, v Verdict) Decision {
	if v == VerdictValid {
		return Record
	}
	switch {
	case kind == KindFHIRBridged:
		return Refuse
	case networkRules[rule]:
		// Decided by the rule, whichever kind the call site names: moving a
		// rule between networkRules and contentRules is the whole change.
		return Refuse
	case p.level == EnforcementStrict:
		return Refuse
	case p.level == EnforcementStructural:
		return decideStructural(kind, rule, v)
	case kind == KindCDSEnvelope && unreadableCDSRules[rule] && p.unreadableRefusesBelowStrict:
		return Refuse
	}
	return Record
}

// decideStructural is structural's row of the table, below the rows that refuse at every
// level. A check that could not run, and a FHIR defect classified as a deeper
// rule's, are recorded. A structural or unclassified FHIR defect refuses; a CDS
// Hooks or content rule refuses unless the table names it as recorded.
func decideStructural(kind CheckKind, rule string, v Verdict) Decision {
	if v == VerdictUnavailable || v == VerdictDeeper {
		return Record
	}
	switch kind {
	case KindCDSEnvelope:
		if structuralCDSDeeperRules[rule] {
			return Record
		}
	case KindContent:
		if contentRules[rule] && !structuralContentRefuses[rule] {
			return Record
		}
	}
	return Refuse
}
