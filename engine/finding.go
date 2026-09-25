package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
)

// findingContextKey carries the leg metadata a conformance finding needs from
// the handler that owns the leg to the validation choke point. Only metadata
// rides the context: the enforcement policy is configuration, built once at
// boot and read off the gateway (the house rule at native.go:77-79 — a gate
// must not depend on a request-scoped value that could be absent).
type findingContextKey struct{}

// findingContext is what a conformance finding says about the leg the checked
// bytes belong to. Seam uses the vocabulary certify.go already records
// ("provider-ingress", "payer-native", …). Whose is "own" (this participant's
// own system's bytes), "peer" (bytes received from a peer) or "network" (SHN's
// own bridged edit).
type findingContext struct {
	LegType       string
	CorrelationID string
	Seam          string
	Whose         string
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

// ConformanceFinding is what a governed check saw, before it reaches either
// carrier. Issues here is RAW validator diagnostic text, exactly as the
// caller has it — emitFinding, and only emitFinding, redacts it through
// certificationIssueMetadata before either carrier is written. This struct
// must never be marshalled or logged by anyone but emitFinding: a caller (or
// a future call site) that serializes it directly, or logs f.Issues on its
// own, leaks a diagnostic that can contain an entire foreign resource (§5).
type ConformanceFinding struct {
	Kind          string   `json:"kind"`
	Direction     string   `json:"direction,omitempty"`
	LegType       string   `json:"legType"`
	CorrelationID string   `json:"correlationId,omitempty"`
	Seam          string   `json:"seam,omitempty"`
	Whose         string   `json:"whose,omitempty"`
	Line          string   `json:"line,omitempty"`
	Profile       string   `json:"profile,omitempty"`
	Level         string   `json:"level"`
	Verdict       string   `json:"verdict,omitempty"` // "unavailable" when the check could not run; absent for an invalid verdict
	Decision      string   `json:"decision"`
	Rule          string   `json:"rule,omitempty"`
	Path          string   `json:"path,omitempty"`
	PayloadSHA256 string   `json:"payloadSha256,omitempty"`
	Issues        []string `json:"issues,omitempty"` // RAW diagnostics; see the struct comment — emitFinding redacts before either carrier sees this.
}

// emitFinding is the one place a governed check's finding is written to both
// carriers, and the one place the redaction rule in ConformanceFinding's
// comment is enforced: f.Issues arrives raw and is replaced with
// certificationIssueMetadata's authored summary (count + size + hash) before
// anything is marshalled, so the redacted form — never the raw diagnostics —
// is what the log line and the observer event actually carry. It is called
// before any decision is acted on, at every level that runs the check, so the
// record of what was seen never depends on what was done about it.
func (g *Gateway) emitFinding(f ConformanceFinding) {
	f.Issues = certificationIssueMetadata(f.Issues)
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	log.Printf("gateway: conformance: %s", b)
	g.observe(ObserverEvent{
		Kind:          ConformanceObservedEvent,
		Direction:     "validate",
		LegType:       f.LegType,
		CorrelationID: f.CorrelationID,
		Detail:        string(b),
	})
}

// findingIssuesShown bounds the issue list a strict refusal body echoes, the
// same bound the CDS Hooks certifier already applies (native.go).
const findingIssuesShown = 5

// boundIssues is the ONLY path by which validator diagnostic text leaves this
// gateway: the strict refusal body returned to the sender (§5). A finding
// never carries these strings.
func boundIssues(issues []string) []string {
	if len(issues) <= findingIssuesShown {
		return issues
	}
	out := append([]string{}, issues[:findingIssuesShown]...)
	return append(out, fmt.Sprintf("and %d more", len(issues)-findingIssuesShown))
}
