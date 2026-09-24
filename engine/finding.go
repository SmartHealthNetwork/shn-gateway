package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
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
)

// ConformanceFinding is what a governed check saw, before it reaches either
// carrier. Issues here is RAW validator diagnostic text, exactly as the
// caller has it — emitFinding, and only emitFinding, redacts it through
// certificationIssueMetadata before either carrier is written. This struct
// must never be marshalled or logged by anyone but emitFinding: a caller (or
// a future call site) that serializes it directly, or logs f.Issues on its
// own, leaks a diagnostic that can contain an entire foreign resource (§5).
type ConformanceFinding struct {
	RuleSet        string       `json:"ruleSet,omitempty"`
	Gateway        string       `json:"gateway,omitempty"`
	Kind           string       `json:"kind"`
	Direction      string       `json:"direction,omitempty"`
	LegType        string       `json:"legType"`
	CorrelationID  string       `json:"correlationId,omitempty"`
	Seam           string       `json:"seam,omitempty"`
	Whose          string       `json:"whose,omitempty"`
	Line           string       `json:"line,omitempty"`
	Profile        string       `json:"profile,omitempty"`
	Profiles       []string     `json:"profiles,omitempty"`
	CheckClass     CheckClass   `json:"checkClass,omitempty"`
	Operation      string       `json:"operation,omitempty"`
	ResultSeverity string       `json:"resultSeverity,omitempty"`
	ClosedReason   string       `json:"closedReason,omitempty"`
	Level          string       `json:"level"`
	Decision       string       `json:"decision,omitempty"`
	Action         string       `json:"action,omitempty"`
	State          CheckState   `json:"state,omitempty"`
	CheckIssues    []CheckIssue `json:"checkIssues,omitempty"`
	Rule           string       `json:"rule,omitempty"`
	Path           string       `json:"path,omitempty"`
	PayloadSHA256  string       `json:"payloadSha256,omitempty"`
	Issues         []string     `json:"issues,omitempty"` // RAW diagnostics; see the struct comment — emitFinding redacts before either carrier sees this.
}

// emitFinding is the one place a governed check's finding is written to both
// carriers, and the one place the redaction rule in ConformanceFinding's
// comment is enforced: f.Issues arrives raw and is replaced with
// certificationIssueMetadata's authored count summary before
// anything is marshalled, so the redacted form — never the raw diagnostics —
// is what the log line and the observer event actually carry. It is called
// before any decision is acted on, at both levels, so the record of what was
// seen never depends on what was done about it.
func (g *Gateway) emitFinding(f ConformanceFinding) {
	f = safeFinding(f)
	f.Gateway = g.cfg.HolderID
	g.publishFinding(f)
}

func (g *Gateway) publishFinding(f ConformanceFinding) {
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

// No diagnostic values, reference-bearing paths, or historical delivery claims
// may cross either finding carrier. Rule identifiers are authored registry IDs.
func safeFinding(f ConformanceFinding) ConformanceFinding {
	f.Issues = certificationIssueMetadata(f.Issues)
	f.Path = ""
	if f.Action == "" {
		if f.Decision == "refused" {
			f.Action = "refused"
		} else {
			f.Action = "not_enforced"
		}
	}
	f.Decision = ""
	f.RuleSet = ConformanceRuleSet
	if f.CheckClass != CheckStructural && f.CheckClass != CheckDeep {
		f.CheckClass = ""
	}
	switch f.Operation {
	case shnsdk.FrameOperationQuestionnairePackage, shnsdk.FrameOperationNextQuestion, "":
	default:
		f.Operation = ""
	}
	switch f.ResultSeverity {
	case "fatal", "error", "warning", "information", "":
	default:
		f.ResultSeverity = ""
	}
	f.ClosedReason = safeFindingReason(f.ClosedReason, f.Rule)
	if !knownFindingProfile(f.Profile) {
		f.Profile = ""
	}
	profiles := make([]string, 0, min(len(f.Profiles), 8))
	listed := f.Profiles
	if len(listed) > 64 {
		listed = listed[:64]
	}
	for _, p := range listed {
		if !knownFindingProfile(p) {
			continue
		}
		seen := false
		for _, existing := range profiles {
			if existing == p {
				seen = true
				break
			}
		}
		if !seen && len(profiles) < 8 {
			profiles = append(profiles, p)
		}
	}
	f.Profiles = profiles
	if len(f.CheckIssues) > 64 {
		f.CheckIssues = f.CheckIssues[:64]
	}
	safe := make([]CheckIssue, 0, len(f.CheckIssues))
	for _, i := range f.CheckIssues {
		severity := "information"
		switch i.Severity {
		case "fatal", "error", "warning", "information":
			severity = i.Severity
		}
		safe = append(safe, CheckIssue{Severity: severity, Code: safeValidationCode(i.Code)})
	}
	f.CheckIssues = safe
	return f
}

// Only registry-owned codes and contract-selected targets may reach logs or
// observer events. An adapter's diagnostic or a message's meta.profile cannot
// become classification metadata.
func safeFindingReason(code, rule string) string {
	switch code {
	case "checker_canceled", "content_unreadable", "observation_body_budget", "resource_budget", "validator_issue_budget", "validator_unavailable", "version_unavailable", "hook_unavailable", "checker_panic", "checker_unavailable", "observation_expired":
		return code
	}
	if code != "" && safeValidationCode(code) == code {
		return code
	}
	if code == rule {
		for _, r := range StructuralRules() {
			if r.ID == code {
				return code
			}
		}
		for _, r := range (&Gateway{}).DeepRules() {
			if r.ID == code {
				return code
			}
		}
	}
	return ""
}

func knownFindingProfile(profile string) bool {
	if profile == "" {
		return false
	}
	// Authored DTR QuestionnaireResponses are also checked against this fixed
	// FHIR R4 base target before they leave the participant's gateway.
	if profile == baseQRProfile {
		return true
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, leg := range []string{"pas-claim", "pas-claim-update", "pas-claim-inquire"} {
			for _, species := range []string{"Claim", "ClaimResponse", "PASRequestBundle", "PASResponseBundle", "QuestionnaireResponse"} {
				if known, ok := profileFor(species, line, leg); ok && known == profile {
					return true
				}
			}
		}
		if def, ok := shnsdk.DTRLineDef(line); ok {
			for _, known := range []string{shnsdk.QuestionnairePackageInputProfile + "|" + def.PackageVersion, certificationDTR + "DTR-QPackageBundle|" + def.PackageVersion, certificationDTR + "dtr-qpackage-output-parameters|" + def.PackageVersion} {
				if profile == known {
					return true
				}
			}
		}
	}
	return false
}

// ruleFinding is shared by the asynchronous observer and synchronous refusal.
// This function does not parse or ask the validator for another verdict.
func ruleFinding(in CheckInput, rule ConformanceRule, result CheckResult, action string) ConformanceFinding {
	f := ConformanceFinding{Kind: ConformanceObservedEvent, Direction: in.Direction, LegType: in.Exchange.legType,
		CorrelationID: in.Exchange.correlationID, Seam: in.finding.Seam, Whose: in.finding.Whose,
		Line: in.DeclaredVersion, Level: in.Exchange.policy.Level().String(), State: result.State,
		Action: action, Rule: rule.ID, CheckClass: rule.Class, Operation: in.Exchange.operation,
		ResultSeverity: result.Severity, ClosedReason: result.Code, CheckIssues: result.Issues,
		PayloadSHA256: sha256hex(in.Body)}
	return f
}
