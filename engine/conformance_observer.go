package engine

import (
	"context"
	"errors"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// An authored check has an explicit profile and validator chosen by its native
// caller. The descriptor is scalar except for the selected validator interface;
// enqueueObservation owns an immutable copy of the target bytes.
type authoredValidationTarget struct {
	validator                shnsdk.Validator
	finding                  findingContext
	direction, line, profile string
}

func (g *Gateway) observeAuthoredTarget(target authoredValidationTarget, body []byte) bool {
	return g.enqueueObservation(certificationJob{authored: &target, payload: body})
}

// observeContent never decodes, classifies or checks on the delivery goroutine.
// The immutable job carries the participant policy captured for this operation.
func (g *Gateway) observeContent(in CheckInput) bool {
	if in.Exchange.policy.Action(CheckStructural) != CheckObserve && in.Exchange.policy.Action(CheckDeep) != CheckObserve {
		return false
	}
	return g.enqueueObservation(certificationJob{input: &in, payload: in.Body})
}
func (g *Gateway) collectObservation(w *certificationWorker, job certificationJob) {
	if job.authored != nil {
		g.collectAuthoredObservation(w, job)
		return
	}
	in := *job.input
	deadline := job.queued.Add(certificationQueueMaxAge)
	if limit := time.Now().Add(certificationCollectionTimeout); limit.Before(deadline) {
		deadline = limit
	}
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	ctx = withFindingContext(ctx, in.finding)
	in.evidence = &contentEvidence{}
	in.observation = &g.observationMemory
	for _, rule := range append(StructuralRules(), g.DeepRules()...) {
		if in.Exchange.policy.Action(rule.Class) != CheckObserve {
			continue
		}
		result, applies := observeRule(ctx, rule, in)
		if !applies {
			continue
		}
		g.recordObservation(w, ruleFinding(in, rule, result, "not_enforced"))
	}
}

func (g *Gateway) collectAuthoredObservation(w *certificationWorker, job certificationJob) {
	target := *job.authored
	deadline := job.queued.Add(certificationQueueMaxAge)
	if limit := time.Now().Add(certificationCollectionTimeout); limit.Before(deadline) {
		deadline = limit
	}
	ctx, cancel := context.WithDeadline(w.ctx, deadline)
	defer cancel()
	ctx = withFindingContext(ctx, target.finding)
	candidate, stop := context.WithTimeout(ctx, certificationCandidateTimeout)
	defer stop()
	evidence, err := func() (value shnsdk.ValidationEvidence, err error) {
		if candidate.Err() != nil {
			return unavailableValidatorEvidence(), candidate.Err()
		}
		defer func() {
			if recover() != nil {
				value = unavailableValidatorEvidence()
				err = errors.New("validator panic")
			}
		}()
		return delegateValidatorEvidence(candidate, target.validator, job.payload, target.profile)
	}()
	if candidate.Err() != nil {
		err = candidate.Err()
	}
	for _, rule := range []struct {
		id    string
		value shnsdk.ValidationCheckEvidence
	}{{"fhir.profile", evidence.Profile}, {"fhir.terminology", evidence.Terminology}} {
		result := validationResult(rule.value, err)
		g.recordObservation(w, ConformanceFinding{
			Kind: ConformanceObservedEvent, Direction: target.direction,
			LegType: target.finding.LegType, CorrelationID: target.finding.CorrelationID,
			Seam: target.finding.Seam, Whose: target.finding.Whose,
			Line: target.line, Profile: target.profile,
			Level: g.policy().Level().String(), CheckClass: CheckDeep,
			Action: "not_enforced", Rule: rule.id, State: result.State,
			ResultSeverity: result.Severity, ClosedReason: result.Code,
			CheckIssues: result.Issues, PayloadSHA256: sha256hex(job.payload),
		})
	}
}
func observeRule(ctx context.Context, rule ConformanceRule, in CheckInput) (out CheckResult, applies bool) {
	applies = true
	defer func() {
		if recover() != nil {
			out = deepUnavailable("checker_panic")
			applies = true
		}
	}()
	if ctx.Err() != nil {
		return deepUnavailable("observation_expired"), true
	}
	candidate, cancel := context.WithTimeout(ctx, certificationCandidateTimeout)
	defer cancel()
	if rule.Applies == nil || rule.Check == nil {
		return deepUnavailable("checker_unavailable"), true
	}
	if !rule.Applies(in) {
		return CheckResult{}, false
	}
	out = rule.Check(candidate, in)
	if candidate.Err() != nil {
		return deepUnavailable("observation_expired"), true
	}
	switch out.State {
	case CheckValid, CheckInvalid, CheckUnavailable, CheckNotApplicable:
	default:
		out = deepUnavailable("checker_unavailable")
	}
	return out, true
}
func (g *Gateway) recordObservation(w *certificationWorker, f ConformanceFinding) {
	f.RuleSet = ConformanceRuleSet
	f.Gateway = g.cfg.HolderID
	f = safeFinding(f)
	w.mu.Lock()
	if len(w.findings) == certificationRingCapacity {
		copy(w.findings, w.findings[1:])
		w.findings[len(w.findings)-1] = f
	} else {
		w.findings = append(w.findings, f)
	}
	w.mu.Unlock()
	g.publishFinding(f)
}
