package engine

import (
	"strings"
	"testing"
)

// The whole table, both levels. At strict every invalid verdict refuses. At
// none only SHN's own bridged edit and the three structural CDS Hooks rules
// do — those three because the reader that follows the certifier needs the
// shape (§2).
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

// A valid verdict decides nothing at either level.
func TestConformancePolicyValidVerdictRecords(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementNone} {
		for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope} {
			if got := NewConformancePolicy(level).Decide(kind, "", VerdictValid); got != Record {
				t.Errorf("a valid verdict must never refuse (%s at %s): %v", kind, level, got)
			}
		}
	}
}

// The zero value is strict: no in-process engine.Config construction can
// become permissive by omission (§3).
func TestConformanceEnforcementZeroValueIsStrict(t *testing.T) {
	var level ConformanceEnforcement
	if level != EnforcementStrict || level.String() != "strict" {
		t.Fatalf("the zero value must be strict, got %v (%q)", level, level.String())
	}
	var p ConformancePolicy
	if p.Decide(KindFHIRIngress, "", VerdictInvalid) != Refuse {
		t.Fatal("a zero-value policy must refuse: an unset policy is strict, never permissive")
	}
}

// TestConformanceEnforcementZeroValueIsStrictDirect asserts the zero value on
// the type itself: a bare, never-assigned ConformanceEnforcement must equal
// EnforcementStrict. This is the property that stops any in-process
// engine.Config construction anywhere in the tree from going permissive by
// omitting the field, and it must be checked directly against a genuine zero
// value — not only through a function whose return path always carries a
// named symbol (see TestDecisionZeroValueIsRefuse for why that distinction
// matters).
func TestConformanceEnforcementZeroValueIsStrictDirect(t *testing.T) {
	var e ConformanceEnforcement
	if e != EnforcementStrict {
		t.Fatalf("the zero value of ConformanceEnforcement must be EnforcementStrict, got %v", e)
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
	for in, want := range map[string]ConformanceEnforcement{"none": EnforcementNone, "strict": EnforcementStrict} {
		got, err := ParseConformanceEnforcement(in)
		if err != nil || got != want {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"middle", "lenient", "NONE", "true", " none"} {
		if _, err := ParseConformanceEnforcement(bad); err == nil {
			t.Errorf("ParseConformanceEnforcement(%q) must be a boot error", bad)
		} else if !strings.Contains(err.Error(), "none") || !strings.Contains(err.Error(), "strict") {
			t.Errorf("the boot error must name both accepted values, got %v", err)
		}
	}
}
