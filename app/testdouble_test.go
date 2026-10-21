package app

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// releasedBuild makes this test stand in for an untagged, non-test build: every
// release, go install and plain go build of the gateway (testdouble.go).
func releasedBuild(t *testing.T) {
	t.Helper()
	prev := testDoubleAvailable
	testDoubleAvailable = func() bool { return false }
	t.Cleanup(func() { testDoubleAvailable = prev })
}

// productionShaped is a deployed gateway's configuration: a store, an https
// discovery anchor and system of record, and a real validator lane.
func productionShaped() map[string]string {
	return map[string]string{
		"ROLE":                   "payer",
		"SHN_SECRETS":            "/etc/shn/bundles/payer",
		"SHN_DISCOVERY_URL":      "https://accounts.example/discovery",
		"SHN_STORE_DATABASE_URL": "postgres://gw:secret@db.example:5432/shn_gateway?sslmode=require",
		"FHIR_DATA_URL":          "https://fhir.example/r4",
		"FHIR_VALIDATE_URL":      "https://validator.example/fhir",
	}
}

// A released build refuses to boot with SHN_FAKE_VALIDATOR set, whatever else is
// configured: at every conformance level (none included, where D117 leaves no
// runtime validation unless the participant opts in), beside a real validator URL,
// with no validator at all, and for any value, not only the one a test build honours.
func TestReleasedBuildRefusesTheValidatorTestDoubleAtBoot(t *testing.T) {
	releasedBuild(t)
	if _, err := loadConfig(env(productionShaped())); err != nil {
		t.Fatalf("control: the production-shaped configuration must load without the double: %v", err)
	}
	type row struct {
		name string
		edit func(map[string]string)
	}
	var rows []row
	for _, level := range []string{"", "none", "observe", "structural", "strict"} {
		rows = append(rows,
			row{"level " + level + ", beside a validator URL", func(m map[string]string) { m["CONFORMANCE_ENFORCEMENT"] = level }},
			row{"level " + level + ", no validator, no default lanes", func(m map[string]string) {
				m["CONFORMANCE_ENFORCEMENT"] = level
				delete(m, "FHIR_VALIDATE_URL")
				m["FHIR_DEFAULT_VALIDATOR_LANES"] = "none"
			}},
		)
	}
	rows = append(rows,
		row{"per-line lanes configured", func(m map[string]string) {
			m["FHIR_VALIDATE_URL_2_1"] = "https://validator21.example/fhir"
			m["FHIR_VALIDATE_URL_2_2"] = "https://validator22.example/fhir"
		}},
		row{"provider role", func(m map[string]string) { m["ROLE"] = "provider"; m["SHN_SECRETS"] = "/etc/shn/bundles/provider" }},
	)
	for _, r := range rows {
		for _, value := range []string{"1", "true", "0"} {
			t.Run(r.name+", SHN_FAKE_VALIDATOR="+value, func(t *testing.T) {
				m := productionShaped()
				r.edit(m)
				m["SHN_FAKE_VALIDATOR"] = value
				_, err := loadConfig(env(m))
				if err == nil || err.Error() != testDoubleRefusal {
					t.Fatalf("loadConfig err = %v, want the test-double refusal", err)
				}
				// The boot itself: build refuses before it reads the bundle or dials anything.
				if _, err := build(context.Background(), env(m), io.Discard, nil); err == nil || err.Error() != testDoubleRefusal {
					t.Fatalf("build err = %v, want the test-double refusal", err)
				}
			})
		}
	}
}

// The refusal names the supported paths, a real validator or the level that checks
// no payload, and never the double's own setting.
func TestTestDoubleRefusalNamesTheValidatorPath(t *testing.T) {
	for _, want := range []string{"FHIR_VALIDATE_URL", "deploy/bundle/compose.yml", "deploy/eval", "FR-36", "or set CONFORMANCE_ENFORCEMENT=none"} {
		if !strings.Contains(testDoubleRefusal, want) {
			t.Errorf("refusal %q does not name %q", testDoubleRefusal, want)
		}
	}
	if strings.Contains(testDoubleRefusal, "=1") {
		t.Errorf("refusal %q hands the operator the double's setting", testDoubleRefusal)
	}
}

// No path past loadConfig reaches the double in a released build either: every
// reader sees SHN_FAKE_VALIDATOR=1 as unset, so the validator decision, the lanes and
// the certification clients fall to their real, fail-closed selection.
func TestReleasedBuildNeverReadsTheValidatorTestDouble(t *testing.T) {
	releasedBuild(t)
	fake := env(map[string]string{"SHN_FAKE_VALIDATOR": "1"})
	if fakeValidator(fake) {
		t.Fatal("fakeValidator = true in a released build")
	}
	if _, err := selectValidator(fake, "", engine.EnforcementObserve); err == nil {
		t.Fatal("selectValidator(fake, no url) = nil error, want the FR-36 fail-closed refusal")
	}
	v, err := selectValidator(fake, "https://validator.example/fhir", engine.EnforcementObserve)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(*engine.LineFakeValidator); ok {
		t.Fatal("selectValidator chose the test double over a configured URL in a released build")
	}
	canonical := shnsdk.NewOperationValidator("https://validator.example/fhir")
	if _, err := validatorLanesForDeclared(fake, engine.NativeContractVersions(), canonical, config{}); err == nil {
		t.Fatal("validatorLanesForDeclared laned every declared line with no lane configured: the double served them")
	}
	if _, _, err := discoverValidatorLanes(context.Background(), fake, engine.NativeContractVersions(), canonical, config{DefaultValidatorLanes: defaultValidatorLanesNone}, func(string) string { return "" }, nil); err == nil {
		t.Fatal("discoverValidatorLanes booted every declared line with no lane configured: the double served them")
	}
	for line, v := range certificationValidators(fake, config{}, "https://validator.example/fhir", nil, nil) {
		if _, ok := v.(*engine.LineFakeValidator); ok {
			t.Fatalf("certification client %s is the test double in a released build", line)
		}
	}
}

// A test binary keeps the double: it is what the hermetic suites boot on.
func TestTestBinaryKeepsTheValidatorTestDouble(t *testing.T) {
	m := productionShaped()
	m["SHN_FAKE_VALIDATOR"] = "1"
	if _, err := loadConfig(env(m)); err != nil {
		t.Fatalf("a test binary refused the double: %v", err)
	}
	if !fakeValidator(env(m)) {
		t.Fatal("fakeValidator = false in a test binary with SHN_FAKE_VALIDATOR=1")
	}
}
