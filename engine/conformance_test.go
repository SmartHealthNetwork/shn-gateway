package engine

import (
	"strings"
	"testing"
)

// The whole table. At strict every invalid verdict refuses. At none no check
// runs except SHN's own bridged edit, and at observe every check runs and only
// that bridged edit refuses: an unreadable CDS Hooks answer is relayed below
// strict.
func TestConformancePolicyTable(t *testing.T) {
	for _, tc := range []struct {
		kind  CheckKind
		rule  string
		level ConformanceEnforcement
		runs  bool
		want  Decision
	}{
		{KindFHIRIngress, "", EnforcementStrict, true, Refuse},
		{KindFHIREgress, "", EnforcementStrict, true, Refuse},
		{KindFHIRBridged, "", EnforcementStrict, true, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementStrict, true, Refuse},
		{KindCDSEnvelope, "response.json", EnforcementStrict, true, Refuse},

		{KindFHIRIngress, "", EnforcementObserve, true, Record},
		{KindFHIREgress, "", EnforcementObserve, true, Record},
		{KindFHIRBridged, "", EnforcementObserve, true, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementObserve, true, Record},
		{KindCDSEnvelope, "card.summary", EnforcementObserve, true, Record},
		{KindCDSEnvelope, "response.json", EnforcementObserve, true, Record},
		{KindCDSEnvelope, "response.object", EnforcementObserve, true, Record},
		{KindCDSEnvelope, "line", EnforcementObserve, true, Record},

		{KindFHIRIngress, "", EnforcementNone, false, Record},
		{KindFHIREgress, "", EnforcementNone, false, Record},
		{KindFHIRBridged, "", EnforcementNone, true, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementNone, false, Record},
		{KindCDSEnvelope, "card.summary", EnforcementNone, false, Record},
		{KindCDSEnvelope, "response.json", EnforcementNone, false, Record},
		{KindCDSEnvelope, "response.object", EnforcementNone, false, Record},
		{KindCDSEnvelope, "line", EnforcementNone, false, Record},
	} {
		p := NewConformancePolicy(tc.level)
		if got := p.Runs(tc.kind, tc.rule); got != tc.runs {
			t.Errorf("Runs(%s, %q) at %s = %v, want %v", tc.kind, tc.rule, tc.level, got, tc.runs)
		}
		if got := p.Decide(tc.kind, tc.rule, VerdictInvalid); got != tc.want {
			t.Errorf("Decide(%s, %q) at %s = %v, want %v", tc.kind, tc.rule, tc.level, got, tc.want)
		}
	}
}

// The one pending row: whether an unreadable CDS Hooks answer refuses below
// strict. The published table relays it (false); the other value refuses it
// at none and observe, and at none it is then the only CDS check that runs.
func TestConformancePolicyUnreadableCDSRow(t *testing.T) {
	if cdsUnreadableRefusesBelowStrict {
		t.Fatal("an unreadable CDS Hooks answer is relayed at none and observe")
	}
	for _, refuses := range []bool{false, true} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			p := newConformancePolicy(level, refuses)
			for rule := range unreadableCDSRules {
				want := Record
				if refuses {
					want = Refuse
				}
				if got := p.Decide(KindCDSEnvelope, rule, VerdictInvalid); got != want {
					t.Errorf("refuses=%v %s: Decide(%q) = %v, want %v", refuses, level, rule, got, want)
				}
				if got, want := p.Runs(KindCDSEnvelope, rule), refuses || level != EnforcementNone; got != want {
					t.Errorf("refuses=%v %s: Runs(%q) = %v, want %v", refuses, level, rule, got, want)
				}
			}
			if level == EnforcementNone && p.Runs(KindCDSEnvelope, "card.summary") {
				t.Errorf("refuses=%v: a readable-answer rule must not run at none", refuses)
			}
			if got, want := p.RunsKind(KindCDSEnvelope), refuses || level != EnforcementNone; got != want {
				t.Errorf("refuses=%v %s: RunsKind(cds) = %v, want %v", refuses, level, got, want)
			}
		}
	}
}

// A check that could not run (validator outage, no lane for the line) refuses
// only at strict and for SHN's own bridged edit; observe records it and relays.
func TestConformancePolicyUnavailableVerdict(t *testing.T) {
	for _, tc := range []struct {
		kind  CheckKind
		level ConformanceEnforcement
		want  Decision
	}{
		{KindFHIRIngress, EnforcementStrict, Refuse},
		{KindFHIREgress, EnforcementStrict, Refuse},
		{KindFHIRBridged, EnforcementStrict, Refuse},
		{KindFHIRIngress, EnforcementObserve, Record},
		{KindFHIREgress, EnforcementObserve, Record},
		{KindFHIRBridged, EnforcementObserve, Refuse},
		{KindFHIRBridged, EnforcementNone, Refuse},
	} {
		if got := NewConformancePolicy(tc.level).Decide(tc.kind, "", VerdictUnavailable); got != tc.want {
			t.Errorf("Decide(%s, unavailable) at %s = %v, want %v", tc.kind, tc.level, got, tc.want)
		}
	}
}

// At none nothing but SHN's own bridged edit runs; every level above none runs
// every kind.
func TestConformancePolicyRunsKind(t *testing.T) {
	for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope} {
		for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementObserve} {
			if !NewConformancePolicy(level).RunsKind(kind) {
				t.Errorf("%s must run at %s", kind, level)
			}
		}
		if got, want := NewConformancePolicy(EnforcementNone).RunsKind(kind), kind == KindFHIRBridged; got != want {
			t.Errorf("RunsKind(%s) at none = %v, want %v", kind, got, want)
		}
	}
	var zero ConformancePolicy
	if !zero.RunsKind(KindFHIRIngress) || !zero.Runs(KindCDSEnvelope, "card.summary") {
		t.Fatal("a zero-value policy is strict and runs every check")
	}
}

// A valid verdict decides nothing at any level.
func TestConformancePolicyValidVerdictRecords(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementObserve, EnforcementNone} {
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
	for in, want := range map[string]ConformanceEnforcement{"none": EnforcementNone, "observe": EnforcementObserve, "strict": EnforcementStrict} {
		got, err := ParseConformanceEnforcement(in)
		if err != nil || got != want {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"middle", "lenient", "NONE", "true", " none", "Observe", "basic"} {
		if _, err := ParseConformanceEnforcement(bad); err == nil {
			t.Errorf("ParseConformanceEnforcement(%q) must be a boot error", bad)
		} else if !strings.Contains(err.Error(), "none") || !strings.Contains(err.Error(), "observe") || !strings.Contains(err.Error(), "strict") {
			t.Errorf("the boot error must name every accepted value, got %v", err)
		}
	}
}

// A level's name round-trips through the setting.
func TestConformanceEnforcementStringRoundTrips(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementObserve, EnforcementNone} {
		got, err := ParseConformanceEnforcement(level.String())
		if err != nil || got != level {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v; want %v", level.String(), got, err, level)
		}
	}
}
