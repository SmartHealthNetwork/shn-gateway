package engine

import (
	"strings"
	"testing"
)

// The compatibility table keeps the existing certifier behavior explicit
// while callers migrate to classed rules. Adaptation and unreadable-answer
// failures remain mandatory; legacy optional checks enforce only at strict.
func TestConformancePolicyTable(t *testing.T) {
	for _, tc := range []struct {
		kind  CheckKind
		rule  string
		level ConformanceEnforcement
		want  Decision
	}{
		{KindFHIRIngress, "", EnforcementStrict, Refuse},
		{KindFHIREgress, "", EnforcementStrict, Refuse},
		{KindFHIRBridged, "", EnforcementStrict, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementStrict, Refuse},
		{KindCDSEnvelope, "response.json", EnforcementStrict, Refuse},

		{KindFHIRIngress, "", EnforcementNone, Record},
		{KindFHIREgress, "", EnforcementNone, Record},
		{KindFHIRBridged, "", EnforcementNone, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementNone, Record},
		{KindCDSEnvelope, "card.summary", EnforcementNone, Record},
		{KindCDSEnvelope, "response.json", EnforcementNone, Refuse},
		{KindCDSEnvelope, "response.object", EnforcementNone, Refuse},
		{KindCDSEnvelope, "line", EnforcementNone, Refuse},
	} {
		got := NewConformancePolicy(tc.level).Decide(tc.kind, tc.rule, VerdictInvalid)
		if got != tc.want {
			t.Errorf("Decide(%s, %q) at %s = %v, want %v", tc.kind, tc.rule, tc.level, got, tc.want)
		}
	}
}

// A valid verdict decides nothing at any level.
func TestConformancePolicyValidVerdictRecords(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope} {
			if got := NewConformancePolicy(level).Decide(kind, "", VerdictValid); got != Record {
				t.Errorf("a valid verdict must never refuse (%s at %s): %v", kind, level, got)
			}
		}
	}
}

// The zero value is none so direct engine construction and app configuration
// have the same participant default.
func TestConformanceEnforcementZeroValueIsNone(t *testing.T) {
	var level ConformanceEnforcement
	if level != EnforcementNone || level.String() != "none" {
		t.Fatalf("the zero value must be none, got %v (%q)", level, level.String())
	}
	p := NewConformancePolicy(EnforcementNone)
	if p.Decide(KindFHIRIngress, "", VerdictInvalid) != Record {
		t.Fatal("a none policy must not refuse a legacy optional conformance check")
	}
}

// TestConformanceEnforcementZeroValueIsNoneDirect asserts the enum's real zero
// rather than only a constructor result, so reordering the constants cannot
// silently make direct Config construction disagree with the app default.
func TestConformanceEnforcementZeroValueIsNoneDirect(t *testing.T) {
	var e ConformanceEnforcement
	if e != EnforcementNone {
		t.Fatalf("the zero value of ConformanceEnforcement must be EnforcementNone, got %v", e)
	}
}

// TestDecisionZeroValueIsRefuse asserts the zero value directly on the type,
// rather than through Decide. Decide's every return statement names a symbol
// — "return Refuse" or "return Record" — never a bare zero-valued Decision,
// so comparing Decide's result to the symbol Refuse is a symbol-to-symbol
// comparison: it carries no information about which of the two is bound to
// the numeric zero. Swap Refuse and Record in the iota block and every
// Decide-based assertion in this file keeps passing, because both sides of
// each comparison move together — while a genuinely zero-valued Decision
// (what a zero-valued ConformancePolicy embeds, and what any struct field of
// type Decision defaults to when unset) has silently become Record. Only a
// bare `var d Decision` compared to Refuse catches that.
func TestDecisionZeroValueIsRefuse(t *testing.T) {
	var d Decision
	if d != Refuse {
		t.Fatalf("the zero value of Decision must be Refuse, got %v", d)
	}
}

func TestParseConformanceEnforcement(t *testing.T) {
	for in, want := range map[string]ConformanceEnforcement{
		"none": EnforcementNone, "observe": EnforcementObserve,
		"basic": EnforcementBasic, "strict": EnforcementStrict,
	} {
		got, err := ParseConformanceEnforcement(in)
		if err != nil || got != want {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", " ", "middle", "lenient", "NONE", "Observe", "BASIC", "true", " none", "strict "} {
		if _, err := ParseConformanceEnforcement(bad); err == nil {
			t.Errorf("ParseConformanceEnforcement(%q) must be a boot error", bad)
		} else if !strings.Contains(err.Error(), "none") || !strings.Contains(err.Error(), "observe") ||
			!strings.Contains(err.Error(), "basic") || !strings.Contains(err.Error(), "strict") {
			t.Errorf("the boot error must name all accepted values, got %v", err)
		}
	}
}
