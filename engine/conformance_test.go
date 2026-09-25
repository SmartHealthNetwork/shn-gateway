package engine

import (
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The whole table. At strict every invalid verdict refuses. At none no check
// runs except SHN's own bridged edit, and at observe every check runs and only
// that bridged edit refuses: an unreadable CDS Hooks answer is relayed below
// strict. At structural every check runs and a structural defect refuses: an
// unclassified FHIR defect, an unreadable CDS Hooks answer or one missing a
// required member, and a request or answer that cannot be read.
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

		{KindFHIRIngress, "", EnforcementStructural, true, Refuse},
		{KindFHIREgress, "", EnforcementStructural, true, Refuse},
		{KindFHIRBridged, "", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "action.description", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "card.summary", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "response.json", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "response.object", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "line", EnforcementStructural, true, Refuse},
		{KindCDSEnvelope, "card.summary.length", EnforcementStructural, true, Record},
		{KindCDSEnvelope, "card.source.topic", EnforcementStructural, true, Record},
		{KindContent, RuleRequestShape, EnforcementStructural, true, Refuse},
		{KindContent, RuleAnswerShape, EnforcementStructural, true, Refuse},
		{KindContent, RulePatientMixed, EnforcementStructural, true, Record},
		{KindNetwork, RuleSubjectToken, EnforcementStructural, true, Refuse},

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
		// Deliberately none and observe only: this row is the switch for the
		// levels that relay an unreadable answer. Structural refuses it by its
		// own table (TestConformancePolicyTable, TestStructuralClassifiesEveryCDSHooksRule).
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
// only at strict and for SHN's own bridged edit; observe and structural record it
// and relay.
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
		{KindFHIRIngress, EnforcementStructural, Record},
		{KindFHIREgress, EnforcementStructural, Record},
		{KindFHIRBridged, EnforcementStructural, Refuse},
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
		for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural, EnforcementObserve} {
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
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural, EnforcementObserve, EnforcementNone} {
		for _, kind := range []CheckKind{KindFHIRIngress, KindFHIREgress, KindFHIRBridged, KindCDSEnvelope, KindContent} {
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
	for in, want := range map[string]ConformanceEnforcement{"none": EnforcementNone, "observe": EnforcementObserve, "structural": EnforcementStructural, "strict": EnforcementStrict} {
		got, err := ParseConformanceEnforcement(in)
		if err != nil || got != want {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v", in, got, err)
		}
	}
	// basic is not a level: no alias.
	for _, bad := range []string{"middle", "lenient", "NONE", "true", " none", "Observe", "Structural", "structural ", "basic"} {
		if _, err := ParseConformanceEnforcement(bad); err == nil {
			t.Errorf("ParseConformanceEnforcement(%q) must be a boot error", bad)
		} else if !strings.Contains(err.Error(), "none") || !strings.Contains(err.Error(), "observe") || !strings.Contains(err.Error(), "structural") || !strings.Contains(err.Error(), "strict") {
			t.Errorf("the boot error must name every accepted value, got %v", err)
		}
	}
}

// A level's name round-trips through the setting.
func TestConformanceEnforcementStringRoundTrips(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural, EnforcementObserve, EnforcementNone} {
		got, err := ParseConformanceEnforcement(level.String())
		if err != nil || got != level {
			t.Errorf("ParseConformanceEnforcement(%q) = %v, %v; want %v", level.String(), got, err, level)
		}
	}
}

// A FHIR defect the choke point classified as deeper is recorded below strict
// and refused at strict; SHN's own bridged edit refuses whatever the class.
func TestConformancePolicyDeeperVerdict(t *testing.T) {
	for _, tc := range []struct {
		kind  CheckKind
		level ConformanceEnforcement
		want  Decision
	}{
		{KindFHIRIngress, EnforcementStrict, Refuse},
		{KindFHIREgress, EnforcementStrict, Refuse},
		{KindFHIRIngress, EnforcementStructural, Record},
		{KindFHIREgress, EnforcementStructural, Record},
		{KindFHIRIngress, EnforcementObserve, Record},
		{KindFHIRBridged, EnforcementStructural, Refuse},
		{KindFHIRBridged, EnforcementObserve, Refuse},
	} {
		if got := NewConformancePolicy(tc.level).Decide(tc.kind, "", VerdictDeeper); got != tc.want {
			t.Errorf("Decide(%s, deeper) at %s = %v, want %v", tc.kind, tc.level, got, tc.want)
		}
	}
}

// structuralCDSRefusedRules is every CDS Hooks error rule structural refuses: an
// answer that cannot be read, or a required member missing or of the wrong
// type. With structuralCDSDeeperRules it must cover every error rule the
// SDK checks, so a rule the SDK adds is classified here before it ships.
var structuralCDSRefusedRules = map[string]bool{
	"response.json": true, "response.object": true, "line": true,
	"response.cards": true, "response.systemActions": true,
	"card.object": true, "card.uuid": true, "card.summary": true, "card.detail": true,
	"card.indicator": true, "card.source": true, "card.source.label": true, "card.suggestions": true,
	"suggestion.label": true, "suggestion.uuid": true, "suggestion.isRecommended": true, "suggestion.actions": true,
	"action.object": true, "action.type": true, "action.description": true,
	"card.links": true, "link.label": true, "link.url": true, "link.type": true, "link.appContext": true,
	"card.overrideReasons": true, "overrideReason.display": true,
}

func TestStructuralClassifiesEveryCDSHooksRule(t *testing.T) {
	p := NewConformancePolicy(EnforcementStructural)
	seen := map[string]bool{}
	for _, r := range shnsdk.CDSHooksRules() {
		seen[r.ID] = true
		structural, deeper := structuralCDSRefusedRules[r.ID], structuralCDSDeeperRules[r.ID]
		if r.Severity != shnsdk.SeverityError {
			if structural || deeper {
				t.Errorf("%s is a recommendation, never refused, and must not be classified", r.ID)
			}
			continue
		}
		if structural == deeper {
			t.Errorf("%s must be classified exactly once (structural %v, deeper %v)", r.ID, structural, deeper)
			continue
		}
		want := Refuse
		if deeper {
			want = Record
		}
		if got := p.Decide(KindCDSEnvelope, r.ID, VerdictInvalid); got != want {
			t.Errorf("structural: Decide(cds, %s) = %v, want %v", r.ID, got, want)
		}
	}
	for id := range structuralCDSRefusedRules {
		if !seen[id] {
			t.Errorf("%s is classified but is not an SDK rule", id)
		}
	}
	for id := range structuralCDSDeeperRules {
		if !seen[id] {
			t.Errorf("%s is classified but is not an SDK rule", id)
		}
	}
	if got := p.Decide(KindCDSEnvelope, "card.someFutureRule", VerdictInvalid); got != Refuse {
		t.Errorf("an unclassified CDS Hooks rule must refuse at structural, got %v", got)
	}
}

// At structural a request or answer that cannot be read refuses and every other
// content rule is recorded; a rule in neither table refuses (fail closed).
func TestStructuralContentRules(t *testing.T) {
	p := NewConformancePolicy(EnforcementStructural)
	for rule := range contentRules {
		want := Record
		if rule == RuleRequestShape || rule == RuleAnswerShape {
			want = Refuse
		}
		if got := p.Decide(KindContent, rule, VerdictInvalid); got != want {
			t.Errorf("structural: Decide(content, %s) = %v, want %v", rule, got, want)
		}
		if got := p.Decide(KindContent, rule, VerdictUnavailable); got != Record {
			t.Errorf("structural: an unavailable %s check must be recorded, got %v", rule, got)
		}
	}
	for rule := range structuralContentRefuses {
		if !contentRules[rule] {
			t.Errorf("%s refuses at structural but is not a content rule", rule)
		}
	}
	if got := p.Decide(KindContent, "content.someFutureRule", VerdictInvalid); got != Refuse {
		t.Errorf("an unclassified content rule must refuse at structural, got %v", got)
	}
	for rule := range networkRules {
		if got := p.Decide(KindNetwork, rule, VerdictInvalid); got != Refuse {
			t.Errorf("structural: network rule %s must refuse, got %v", rule, got)
		}
	}
}
