package engine

import (
	"context"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// findingContextKey carries the leg metadata a conformance finding needs from
// the handler that owns the leg to the validation choke point. Only metadata
// rides the context: the enforcement policy is configuration, built once at
// boot and read off the gateway (the house rule at native.go:77-79 — a gate
// must not depend on a request-scoped value that could be absent).
type findingContextKey struct{}

// findingContext is what a conformance finding says about the leg the checked
// bytes belong to. Seam uses the vocabulary module3_certify.go already records
// ("provider-ingress", "payer-native", …). Whose is "own" (this participant's
// own system's bytes), "peer" (bytes received from a peer) or "network" (SHN's
// own bridged edit).
type findingContext struct {
	LegType       string
	CorrelationID string
	Seam          string
	Whose         string
	// Edit and SourceSHA256 are set for the check of a payload a registered
	// edit changed: the edit, and the sha256 of the payload as it arrived,
	// before the edit (the finding's PayloadSHA256 is the edited payload's).
	Edit         relay.EditID
	SourceSHA256 string
	// OwnDeclared is set for the check of a questionnaire response the
	// gateway's own engine filled: every profile it declares is the
	// gateway's, so each is asked for, as an explicit profile is, and a
	// validator that does not hold one reads it as a defect, never as a
	// check that could not run (classifyDeclared). A profile a participant's
	// own engine declared keeps the reading of any participant declaration.
	OwnDeclared bool
}

// withFindingContext tags ctx with the leg metadata for every governed check
// the handler makes, the pattern of the frame, answer-line and declared-set
// keys at gateway.go:2069-2119.
func withFindingContext(ctx context.Context, fc findingContext) context.Context {
	return context.WithValue(ctx, findingContextKey{}, fc)
}

// findingContextFrom reads the ctx-carried leg metadata. An absent context is
// never a reason to suppress a finding: the check is reported with legType
// "unknown" and nothing else invented.
func findingContextFrom(ctx context.Context) findingContext {
	fc, _ := ctx.Value(findingContextKey{}).(findingContext)
	if fc.LegType == "" {
		fc.LegType = "unknown"
	}
	return fc
}

// ConformanceObservedEvent is the observer event kind a conformance finding
// rides. It is additive to validate.result (observer.go:203-228, emitted for
// every $validate with the payload bytes) and to crd.embedded.validated: this
// event says what the check MEANT — which policy decision it produced — and
// carries no payload.
const ConformanceObservedEvent = "conformance.observed"

// CheckKind is what the check was certifying. It lives here, with the finding,
// rather than with the policy: a finding names its kind at every level, and
// findings are emitted before any policy exists.
type CheckKind string

const (
	// KindFHIRIngress is a runtime $validate of a message received from a peer
	// or from this participant's own system.
	KindFHIRIngress CheckKind = "fhir-ingress"
	// KindFHIREgress is a runtime $validate of a message this gateway is about
	// to send, built or relayed.
	KindFHIREgress CheckKind = "fhir-egress"
	// KindFHIRBridged is a target-line $validate of a payload THIS GATEWAY
	// transformed between IG lines. Those bytes are SHN's own registered edit
	// (prime directive 3), not the participant's data, so they are refused at
	// every level: a transform SHN cannot verify must not seal.
	KindFHIRBridged CheckKind = "fhir-bridged"
	// KindCDSEnvelope is the CDS Hooks response rules over a payer's answer.
	KindCDSEnvelope CheckKind = "cds-envelope"
	// KindNetwork is a network-level check on a relay path (the networkRules):
	// binding a leg's authority and consent to one patient, and reading a body
	// one way only. It refuses at every level and records no conformance
	// finding: it is authority and message integrity, not a check of the
	// participant's payload.
	KindNetwork CheckKind = "network"
	// KindContent is a check of the shape or internal consistency of a
	// participant's message on a relay path (the contentRules). Like any
	// conformance check it does not run at none, is recorded at observe and
	// refuses at strict; at structural only an unreadable request or answer
	// refuses.
	KindContent CheckKind = "content"
)

// CheckKinds are the CheckKind values, in declaration order: the closed list a
// finding's kind is one of (a metric attribute folds against it, MON-R24).
var CheckKinds = []string{
	string(KindFHIRIngress), string(KindFHIREgress), string(KindFHIRBridged),
	string(KindCDSEnvelope), string(KindNetwork), string(KindContent),
}

// FindingMetric is what the finding metric counts of one finding: closed
// values only, never a payload, a line's diagnostics or an id.
type FindingMetric struct {
	// Exchange is the finding's leg (ExchangeKinds), Level its level
	// (ConformanceLevels), Kind its CheckKind (CheckKinds) and Action its
	// decision (FindingActions), each as recorded.
	Exchange, Level, Kind, Action string
	// Tolerated is the finding's own Tolerated flag.
	Tolerated bool
}

// The validator lanes a $validate call is made through: routing, the lane a
// check of a leg's payload uses; certification, the client module 3
// certifies through.
const (
	ValidatorLaneRouting       = "routing"
	ValidatorLaneCertification = "certification"
)

// ConformanceFinding is what a governed check saw, before it reaches either
// carrier. Issues here is RAW validator diagnostic text, exactly as the
// caller has it — emitFinding, and only emitFinding, redacts it through
// certificationIssueMetadata before either carrier is written. This struct
// must never be marshalled or logged by anyone but emitFinding: a caller (or
// a future call site) that serializes it directly, or logs f.Issues on its
// own, leaks a diagnostic that can contain an entire foreign resource (§5).
type ConformanceFinding struct {
	Kind          string `json:"kind"`
	Direction     string `json:"direction,omitempty"`
	LegType       string `json:"legType"`
	CorrelationID string `json:"correlationId,omitempty"`
	Seam          string `json:"seam,omitempty"`
	Whose         string `json:"whose,omitempty"`
	Line          string `json:"line,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Level         string `json:"level"`
	Verdict       string `json:"verdict,omitempty"` // "unavailable" when the check could not run; "valid" when a candidate line after the first passed; "unchecked" when the lane could not check a code; absent for an invalid verdict
	Decision      string `json:"decision"`
	Rule          string `json:"rule,omitempty"`
	Path          string `json:"path,omitempty"`
	PayloadSHA256 string `json:"payloadSha256,omitempty"`
	// Edit and SourcePayloadSHA256 are set on the check of a payload a
	// registered edit changed: the edit's id, and the sha256 of the payload as
	// it arrived, before the edit. Neither carries a payload.
	Edit                string   `json:"edit,omitempty"`
	SourcePayloadSHA256 string   `json:"sourcePayloadSha256,omitempty"`
	Issues              []string `json:"issues,omitempty"` // RAW diagnostics; see the struct comment — emitFinding redacts before either carrier sees this.
	// Tolerated is set when the level would have refused a payer's answer and
	// the participant tolerates that payer (CONFORMANCE_TOLERATE_PEERS, D122):
	// the answer was recorded and carried instead. Never set on a check that
	// could not run, which is never tolerated.
	Tolerated bool `json:"tolerated,omitempty"`
	// DeclaredLine and Lines are set only by a candidate-line certification (a
	// payer's answer checked against the lines this gateway supports): the line the
	// leg was routed at — the payer's declared line, or this gateway's own line for
	// a payer that declared none — and each line tried, in order, with its verdict.
	// Neither carries a diagnostic.
	DeclaredLine string               `json:"declaredLine,omitempty"`
	Lines        []LineVerdictSummary `json:"lines,omitempty"`
	// ValidatorMs is the time the check's $validate calls took, every line
	// tried together (to its failure, for one that could not be judged); 0
	// for a check that called no validator.
	ValidatorMs int64 `json:"validatorMs,omitempty"`
	// UnresolvedProfiles names the profiles the checked payload declares in
	// meta.profile that the validator does not hold, so it was not checked
	// against them (unresolvedProfileNames): each by its size and digest,
	// sorted, at most eight named, the rest counted. Set on an unavailable finding they made unavailable, and on
	// an invalid one beside the defect that decided it.
	UnresolvedProfiles []string `json:"unresolvedProfiles,omitempty"`
	// UncheckedCodeSystems names the code systems the line that decided could
	// not check (certificationUnchecked), sorted: set on an unchecked finding,
	// so a terminology gap is told apart from a defect. A code system's URL
	// only, never a code or a diagnostic.
	UncheckedCodeSystems []string `json:"uncheckedCodeSystems,omitempty"`

	// binding and tally are the call this finding belongs to, taken from the
	// request's context where the finding was made (bindFinding); they are
	// never marshalled.
	binding findingBinding
	tally   *legTally
}

// LineVerdictSummary is one line a candidate-line certification tried and what it
// saw there: "valid", "structural", "deeper", "unchecked" or "unavailable", and how long
// that line's $validate call took. Never a validator diagnostic.
type LineVerdictSummary struct {
	Line    string `json:"line"`
	Verdict string `json:"verdict"`
	// Ms is the time this line's $validate call took (to its failure for an
	// unavailable one); absent for a line with no validator lane, or a call
	// under a millisecond.
	Ms int64 `json:"ms,omitempty"`
}

// findingVerdict is a verdict as a finding names it.
func findingVerdict(v Verdict) string {
	switch v {
	case VerdictValid:
		return "valid"
	case VerdictDeeper:
		return "deeper"
	case VerdictUnchecked:
		return "unchecked"
	case VerdictUnavailable:
		return "unavailable"
	}
	return "structural"
}
