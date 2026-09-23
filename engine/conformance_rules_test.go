package engine

import "testing"

func TestFourLevelActions(t *testing.T) {
	rows := []struct {
		level            ConformanceEnforcement
		structural, deep CheckAction
	}{
		{EnforcementNone, CheckOff, CheckOff},
		{EnforcementObserve, CheckObserve, CheckObserve},
		{EnforcementBasic, CheckEnforce, CheckObserve},
		{EnforcementStrict, CheckEnforce, CheckEnforce},
	}
	for _, r := range rows {
		p := NewConformancePolicy(r.level)
		if p.Action(CheckStructural) != r.structural || p.Action(CheckDeep) != r.deep {
			t.Fatalf("unexpected actions at %v", r.level)
		}
	}
	if (Config{}).ConformanceEnforcement != EnforcementNone {
		t.Fatal("direct construction must default to none")
	}
}

func TestNewConformancePolicyRejectsUnknownLevel(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an out-of-range enforcement level must be rejected when the policy is constructed")
		}
	}()
	NewConformancePolicy(ConformanceEnforcement(99))
}

func TestNewRejectsUnknownConformanceLevel(t *testing.T) {
	if _, err := New(Config{ConformanceEnforcement: ConformanceEnforcement(99)}); err == nil {
		t.Fatal("an out-of-range enforcement level must be rejected when the gateway is constructed")
	}
}
