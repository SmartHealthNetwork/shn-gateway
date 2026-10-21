package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// checkingLevels are the levels that check a payload: each needs a validator
// for every line it declares, and refuses to boot without one.
var checkingLevels = []engine.ConformanceEnforcement{engine.EnforcementObserve, engine.EnforcementStructural, engine.EnforcementStrict}

// A gateway at none checks no payload, so a declared line with no lane boots
// unlaned, whether or not a canonical validator is configured; no nil lane
// enters the map, and every configured lane is built exactly as at any other
// level. FR-G29.
func TestNoneBootsDeclaredLinesUnlaned(t *testing.T) {
	none := config{ConformanceEnforcement: engine.EnforcementNone}
	for _, canonical := range []shnsdk.Validator{nil, shnsdk.NewFakeValidator()} {
		lanes, err := validatorLanesForDeclared(env(nil), engine.NativeContractVersions(), canonical, none)
		if err != nil {
			t.Fatalf("canonical=%v: %v", canonical, err)
		}
		for line, v := range lanes {
			if v == nil {
				t.Fatalf("canonical=%v: a nil lane entered the map at %s", canonical, line)
			}
		}
		if lanes["2.1"] != nil || lanes["2.2"] != nil {
			t.Fatalf("canonical=%v: lanes %v, want 2.1 and 2.2 unlaned", canonical, lanes)
		}
		if (lanes["2.0"] != nil) != (canonical != nil) {
			t.Fatalf("canonical=%v: 2.0 lane %v, want the canonical validator exactly when there is one", canonical, lanes["2.0"])
		}
	}
	laned := none
	laned.FHIRValidateURL22 = "http://validator-2-2.example/fhir"
	lanes, err := validatorLanesForDeclared(env(nil), engine.NativeContractVersions(), nil, laned)
	if err != nil {
		t.Fatal(err)
	}
	if lanes["2.2"] == nil || lanes["2.1"] != nil || lanes["2.0"] != nil {
		t.Fatalf("lanes %v, want only the configured 2.2 lane", lanes)
	}
}

// REJECTION: at every level that checks, a declared line with no lane still
// refuses boot, and the refusal names both remedies: the line's validator, or
// the level that checks nothing.
func TestCheckingLevelsRefuseDeclaredLineWithoutLane(t *testing.T) {
	for _, level := range checkingLevels {
		_, err := validatorLanesForDeclared(env(nil), []string{"pa.pas@2.0", "pa.pas@2.2"}, shnsdk.NewFakeValidator(), config{ConformanceEnforcement: level})
		if err == nil {
			t.Fatalf("%v: a declared 2.2 with no lane booted", level)
		}
		for _, want := range []string{"pa.pas@2.2", "set FHIR_VALIDATE_URL_2_2", "or set CONFORMANCE_ENFORCEMENT=none"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: refusal %q does not name %q", level, err, want)
			}
		}
	}
}

// At none a declared default lane is never waited for: it qualifies in the
// background, so one that fails qualification does not fail boot, and one that
// passes is ready once its worker finishes. At every level that checks, the
// same failure fails boot (the rejection row).
func TestNoneQualifiesDeclaredDefaultLanesInTheBackground(t *testing.T) {
	declared := []string{"pa.pas@2.0", "pa.pas@2.2"}
	failing := func(context.Context, string, string) error { return errors.New("wrong-line verdict") }
	passing := func(context.Context, string, string) error { return nil }
	for _, canonical := range []shnsdk.Validator{nil, shnsdk.NewFakeValidator()} {
		for name, qualify := range map[string]engine.LaneQualifier{"failing": failing, "passing": passing} {
			_, m, err := discoverValidatorLanes(context.Background(), env(nil), declared, canonical, config{ConformanceEnforcement: engine.EnforcementNone}, engine.DefaultLaneURL, qualify)
			if err != nil {
				t.Fatalf("canonical=%v, %s qualification: none refused boot: %v", canonical, name, err)
			}
			m.workers.Wait()
			if got := m.defaults["2.2"].Ready(); got != (name == "passing") {
				t.Errorf("canonical=%v, %s qualification: 2.2 ready=%v", canonical, name, got)
			}
			m.Close()
		}
	}
	for _, level := range checkingLevels {
		_, _, err := discoverValidatorLanes(context.Background(), env(nil), declared, shnsdk.NewFakeValidator(), config{ConformanceEnforcement: level}, engine.DefaultLaneURL, failing)
		if err == nil || !strings.Contains(err.Error(), "declared validator line 2.2") {
			t.Errorf("%v: a declared default lane failing qualification = %v, want the boot refusal", level, err)
		}
	}
}

// noValidatorBuildEnv is a provider gateway declaring every native line with no
// validator at all: no FHIR_VALIDATE_URL (discovery advertises none), no per-line
// URL, no default lane, no test double.
func noValidatorBuildEnv(t *testing.T, level string) map[string]string {
	t.Helper()
	env := nosorBuildEnv(t, "provider", map[string]string{
		"SHN_FAKE_VALIDATOR":           "",
		"FHIR_DEFAULT_VALIDATOR_LANES": defaultValidatorLanesNone,
		"SHN_CONTRACT_VERSIONS":        strings.Join(engine.NativeContractVersions(), ","),
		"FHIR_DATA_URL":                "https://sor.example/fhir",
	})
	if level != "" {
		env["CONFORMANCE_ENFORCEMENT"] = level
	}
	return env
}

// builtValidator is the canonical validator build handed engine.New, read from
// the unexported field (engine.Gateway exposes no accessor for it).
func builtValidator(t *testing.T, b built) reflect.Value {
	t.Helper()
	v := reflect.ValueOf(b.gateway).Elem().FieldByName("cfg").FieldByName("Validator")
	if !v.IsValid() {
		t.Fatal("engine.Gateway has no cfg.Validator: the test's reach into the engine is stale")
	}
	return v
}

// A provider gateway at none boots with no validator at all and says so; the
// same gateway with any validator, a single per-line lane or a default lane
// included, boots with it and does not.
func TestBuildAtNoneBootsWithNoValidator(t *testing.T) {
	b, out, err := buildForTest(t, noValidatorBuildEnv(t, "none"))
	if err != nil {
		t.Fatalf("build at none with no validator: %v", err)
	}
	b.lanes.Close()
	if !strings.Contains(out, noValidatorLog) {
		t.Fatalf("boot output does not carry the no-validator line:\n%s", out)
	}
	if v := builtValidator(t, b); !v.IsNil() {
		t.Fatalf("cfg.Validator = %v, want none", v)
	}

	withURL := noValidatorBuildEnv(t, "none")
	withURL["FHIR_VALIDATE_URL"] = "http://127.0.0.1:9/fhir"
	withURL["FHIR_VALIDATE_URL_2_1"] = "http://127.0.0.1:9/fhir21"
	withURL["FHIR_VALIDATE_URL_2_2"] = "http://127.0.0.1:9/fhir22"
	b, out, err = buildForTest(t, withURL)
	if err != nil {
		t.Fatalf("build at none with validators: %v", err)
	}
	b.lanes.Close()
	if strings.Contains(out, "gateway: no FHIR validator:") {
		t.Fatalf("a gateway with a validator logged the no-validator line:\n%s", out)
	}
	if builtValidator(t, b).IsNil() {
		t.Fatal("a configured FHIR_VALIDATE_URL was dropped at none")
	}

	unresolvedComposeLanes(t)
	for name, lane := range map[string]func(map[string]string){
		"a 2.2 lane alone":  func(m map[string]string) { m["FHIR_VALIDATE_URL_2_2"] = "http://127.0.0.1:9/fhir22" },
		"the default lanes": func(m map[string]string) { delete(m, "FHIR_DEFAULT_VALIDATOR_LANES") },
	} {
		env := noValidatorBuildEnv(t, "none")
		lane(env)
		b, out, err := buildForTest(t, env)
		if err != nil {
			t.Fatalf("%s: build at none: %v", name, err)
		}
		b.lanes.Close()
		if !builtValidator(t, b).IsNil() {
			t.Fatalf("%s: a canonical validator appeared", name)
		}
		if strings.Contains(out, "gateway: no FHIR validator:") {
			t.Fatalf("%s: a gateway with a validator lane logged the no-validator line:\n%s", name, out)
		}
	}
}

// REJECTION: at every level that checks, the unset default (observe) included,
// the same validator-less gateway refuses to boot, naming the none level as the
// other remedy.
func TestBuildAtCheckingLevelsRefusesNoValidator(t *testing.T) {
	for _, level := range []string{"", "observe", "structural", "strict"} {
		b, out, err := buildForTest(t, noValidatorBuildEnv(t, level))
		if err == nil {
			b.lanes.Close()
			t.Fatalf("CONFORMANCE_ENFORCEMENT=%q: a gateway with no validator booted", level)
		}
		if !strings.Contains(err.Error(), "no FHIR validator URL") || !strings.Contains(err.Error(), "or set CONFORMANCE_ENFORCEMENT=none") {
			t.Errorf("CONFORMANCE_ENFORCEMENT=%q: refusal %q, want the FR-36 refusal naming none", level, err)
		}
		if strings.Contains(out, "gateway: no FHIR validator:") {
			t.Errorf("CONFORMANCE_ENFORCEMENT=%q: a refused boot logged the no-validator line", level)
		}
	}
}
