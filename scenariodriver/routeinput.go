package scenariodriver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ErrNoScenarioPersona refuses a route request the network's descriptor
// states no persona for: the driver sends what the persona states, never a
// value of its own.
var ErrNoScenarioPersona = errors.New("scenariodriver: the discovery descriptor states no persona for this route")

// ScenarioRouteInput is what a driver sends an origination route for one of
// the descriptor's scenario personas: the persona's member, its order (system
// and code) and its claim facts, exactly as the descriptor states them, and —
// when amend is true — the certificationType the persona states for its
// amendment (DiscoveryScenarioPersona.Amendment). A persona with no
// order (an eligibility scenario) states the member alone. extra holds the
// route's own keys (branch, answer, ...), which must not name these.
//
// A persona whose order states no claim facts is refused here: a route
// request without them is one the route would refuse anyway, and a driver
// never fills them in.
func ScenarioRouteInput(p shnsdk.DiscoveryScenarioPersona, amend bool, extra map[string]any) (map[string]any, error) {
	if p.MemberID == "" {
		return nil, fmt.Errorf("%w: persona for %s/%s names no member", ErrNoScenarioPersona, p.Scenario, p.Lane)
	}
	body := map[string]any{}
	for k, v := range extra {
		body[k] = v
	}
	for _, k := range []string{"member", "order", "claimFacts", "certificationType"} {
		if _, clash := extra[k]; clash {
			return nil, fmt.Errorf("scenariodriver: route input key %q is the persona's statement, not the caller's", k)
		}
	}
	body["member"] = p.MemberID
	if p.Order == nil {
		if amend {
			return nil, fmt.Errorf("%w: %s/%s persona %s states no order to amend", ErrNoScenarioPersona, p.Scenario, p.Lane, p.MemberID)
		}
		return body, nil
	}
	facts, err := p.Order.ClaimFacts()
	if err != nil {
		return nil, err
	}
	if facts.ClaimType == nil || facts.Priority == nil || facts.ServiceCategory == nil {
		return nil, fmt.Errorf("%w: %s/%s persona %s states no claim type, priority or service category", ErrNoScenarioPersona, p.Scenario, p.Lane, p.MemberID)
	}
	body["order"] = map[string]string{"system": p.Order.System, "code": p.Order.Code}
	body["claimFacts"] = facts
	if amend {
		if p.Amendment == nil || p.Amendment.CertificationType == nil || p.Amendment.CertificationType.Code == "" {
			return nil, fmt.Errorf("%w: %s/%s persona %s states no certificationType for its amendment", ErrNoScenarioPersona, p.Scenario, p.Lane, p.MemberID)
		}
		body["certificationType"] = p.Amendment.CertificationType
	}
	return body, nil
}

// ScenarioRouteBody is ScenarioRouteInput for the persona the descriptor
// states for scenario on lane (persona set personaSet), marshalled as the
// route's JSON body. ok-less: a scenario the descriptor states no persona for
// is refused (ErrNoScenarioPersona).
func ScenarioRouteBody(d shnsdk.Discovery, scenario, lane, personaSet string, amend bool, extra map[string]any) (string, error) {
	p, ok := d.ScenarioPersona(scenario, lane, personaSet)
	if !ok {
		return "", fmt.Errorf("%w: scenario %q lane %q persona set %q", ErrNoScenarioPersona, scenario, lane, personaSet)
	}
	in, err := ScenarioRouteInput(p, amend, extra)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// amendingScenarios are the routes whose flow sends an amendment
// (pas-claim-update): the driver states the amendment's certificationType.
var amendingScenarios = map[string]bool{"uc04": true, "uc05": true, "uc06": true, "uc07": true}

// defaultBranches are the branch values that select a route's default persona.
var defaultBranches = map[string]bool{"": true, "covered": true, "consent": true}

// ScenarioKey names the descriptor's scenario for a route path and the branch
// its body selects: the route's path segment for the default branch
// ("/scenario/uc04" → "uc04"; a resume route "/scenario/uc06/complete" →
// "uc06"), and "<segment>-<branch>" for a branch that selects another persona
// ("uc01-notcovered", "uc05-noconsent", "uc03-line-carry"; the keys of engine.ScenarioRoster). amend reports
// whether the route sends an amendment.
func ScenarioKey(path, branch string) (scenario string, amend bool) {
	seg := strings.TrimPrefix(path, "/scenario/")
	if i := strings.IndexAny(seg, "/?"); i >= 0 {
		seg = seg[:i]
	}
	scenario = seg
	if !defaultBranches[branch] {
		scenario = seg + "-" + branch
	}
	// A start route submits and pends; only the full flow and its complete
	// route send the amendment.
	return scenario, amendingScenarios[seg] && !strings.HasSuffix(strings.SplitN(path, "?", 2)[0], "/start")
}

// PersonaRouteBody is the JSON body a driver sends a route path on lane
// (persona set personaSet): the route's own keys (extra, whose "branch" picks
// the persona) and the persona's statement from the descriptor d.
func PersonaRouteBody(d shnsdk.Discovery, lane, personaSet, path string, extra map[string]any) (string, error) {
	branch, _ := extra["branch"].(string)
	scenario, amend := ScenarioKey(path, branch)
	return ScenarioRouteBody(d, scenario, lane, personaSet, amend, extra)
}
