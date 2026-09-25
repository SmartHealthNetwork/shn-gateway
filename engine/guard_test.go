package engine

import (
	"context"
	"strings"
	"testing"
)

// The network rules bind a leg's authority and consent to one patient and read
// a body one way only: they refuse at every level and record no conformance finding. Every content rule
// follows the level: nothing at none, recorded at observe, refused at strict

func TestGuardTable(t *testing.T) {
	for rule := range networkRules {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
			p := NewConformancePolicy(level)
			if !p.Runs(KindNetwork, rule) || p.Decide(KindNetwork, rule, VerdictInvalid) != Refuse {
				t.Errorf("network rule %q must run and refuse at %s", rule, level)
			}
		}
	}
	for rule := range contentRules {
		for _, tc := range []struct {
			level ConformanceEnforcement
			runs  bool
			want  Decision
		}{
			{EnforcementNone, false, Record},
			{EnforcementObserve, true, Record},
			{EnforcementStructural, true, map[bool]Decision{true: Refuse, false: Record}[refusesAt(EnforcementStructural, rule)]},
			{EnforcementStrict, true, Refuse},
		} {
			p := NewConformancePolicy(tc.level)
			if p.Runs(KindContent, rule) != tc.runs || p.Decide(KindContent, rule, VerdictInvalid) != tc.want {
				t.Errorf("content rule %q at %s: runs=%v decide=%v, want %v %v", rule, tc.level,
					p.Runs(KindContent, rule), p.Decide(KindContent, rule, VerdictInvalid), tc.runs, tc.want)
			}
		}
	}
	for rule := range networkRules {
		if contentRules[rule] {
			t.Errorf("rule %q is in both tables", rule)
		}
	}
}

// The split, pinned by name: the subject binding and any repeated member name
// are network level; patient consistency inside a payload is payload level.
func TestGuardSubjectSplit(t *testing.T) {
	for _, rule := range []string{RuleSubjectPCI, RuleSubjectToken, RuleDuplicateKey} {
		if !networkRules[rule] {
			t.Errorf("%q must be a network-level rule", rule)
		}
	}
	for _, rule := range []string{RulePatientMixed, RulePatientAnswer} {
		if !contentRules[rule] {
			t.Errorf("%q must be a content (payload-level) rule", rule)
		}
	}
}

func TestGuardRecordsAndRefusesPerLevel(t *testing.T) {
	for _, tc := range []struct {
		kind     CheckKind
		rule     string
		level    ConformanceEnforcement
		refuse   bool
		findings int
		decision string
	}{
		{KindContent, RulePatientMixed, EnforcementNone, false, 0, ""},
		{KindContent, RulePatientMixed, EnforcementObserve, false, 1, "relayed"},
		{KindContent, RulePatientMixed, EnforcementStrict, true, 1, "refused"},
		{KindNetwork, RuleSubjectToken, EnforcementNone, true, 0, ""},
		{KindNetwork, RuleSubjectToken, EnforcementObserve, true, 0, ""},
		{KindNetwork, RuleSubjectToken, EnforcementStrict, true, 0, ""},
	} {
		var findings []ConformanceFinding
		ctx := withFindingContext(context.Background(), findingContext{LegType: "pas-claim", Whose: "peer", CorrelationID: "c1"})
		got := guardDefect(ctx, NewConformancePolicy(tc.level), func(f ConformanceFinding) { findings = append(findings, f) }, tc.kind, tc.rule, VerdictInvalid, []byte(`{"secret":"PHI"}`))
		if got != tc.refuse || len(findings) != tc.findings {
			t.Errorf("%s %s at %s: refuse=%v findings=%d, want %v %d", tc.kind, tc.rule, tc.level, got, len(findings), tc.refuse, tc.findings)
			continue
		}
		if tc.findings == 1 {
			f := findings[0]
			if f.Kind != string(KindContent) || f.Rule != tc.rule || f.Decision != tc.decision || f.Level != tc.level.String() || f.LegType != "pas-claim" || f.CorrelationID != "c1" || f.PayloadSHA256 == "" {
				t.Errorf("finding = %+v", f)
			}
			if strings.Contains(f.PayloadSHA256+f.Path+strings.Join(f.Issues, ""), "PHI") {
				t.Errorf("a finding carries no payload text: %+v", f)
			}
		}
	}
	// A nil emitter still decides.
	if !guardDefect(context.Background(), NewConformancePolicy(EnforcementStrict), nil, KindContent, RulePatientMixed, VerdictInvalid, nil) {
		t.Fatal("a nil emitter must not change the decision")
	}
}

// A content check that could not finish is recorded as unavailable at observe
// and refuses at strict.
func TestGuardUnavailable(t *testing.T) {
	for _, tc := range []struct {
		level    ConformanceEnforcement
		refuse   bool
		findings int
	}{
		{EnforcementNone, false, 0},
		{EnforcementObserve, false, 1},
		{EnforcementStrict, true, 1},
	} {
		var findings []ConformanceFinding
		got := guardDefect(context.Background(), NewConformancePolicy(tc.level), func(f ConformanceFinding) { findings = append(findings, f) }, KindContent, RulePatientMixed, VerdictUnavailable, nil)
		if got != tc.refuse || len(findings) != tc.findings {
			t.Errorf("%s: refuse=%v findings=%d, want %v %d", tc.level, got, len(findings), tc.refuse, tc.findings)
		}
		if len(findings) == 1 && findings[0].Verdict != "unavailable" {
			t.Errorf("%s: finding verdict = %q, want unavailable", tc.level, findings[0].Verdict)
		}
	}
}

// Moving a content rule into networkRules is the whole of changing its level
// behavior: call sites that name it as content then refuse at every level and
// record nothing, with no other edit.
func TestGuardRuleMovesByTableAlone(t *testing.T) {
	delete(contentRules, RulePatientMixed)
	networkRules[RulePatientMixed] = true
	defer func() {
		delete(networkRules, RulePatientMixed)
		contentRules[RulePatientMixed] = true
	}()
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		var findings []ConformanceFinding
		p := NewConformancePolicy(level)
		if !p.Runs(KindContent, RulePatientMixed) {
			t.Errorf("%s: a network rule must run at every level", level)
		}
		if !guardDefect(context.Background(), p, func(f ConformanceFinding) { findings = append(findings, f) }, KindContent, RulePatientMixed, VerdictInvalid, nil) {
			t.Errorf("%s: a network rule must refuse at every level", level)
		}
		if len(findings) != 0 {
			t.Errorf("%s: a network rule records no finding, got %+v", level, findings)
		}
	}
}
