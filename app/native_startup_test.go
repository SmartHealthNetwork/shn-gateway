package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestBuildNoValidator(t *testing.T) {
	for _, level := range []string{"none", "observe", "basic", "strict"} {
		t.Run(level, func(t *testing.T) {
			b, _, err := buildProviderForPopulate(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate", "SHN_FAKE_VALIDATOR": "", "CONFORMANCE_ENFORCEMENT": level})
			if err != nil {
				t.Fatal(err)
			}
			b.lanes.Close()
			if level == "none" && (len(b.lanes.defaults) != 0 || b.gateway.ValidatorReadinessForTest()["2.0"]) {
				t.Fatal("none constructed optional validation lanes")
			}
		})
	}
}

func TestNoValidatorReadinessDependency(t *testing.T) {
	for _, level := range []engine.ConformanceEnforcement{engine.EnforcementNone, engine.EnforcementObserve, engine.EnforcementBasic, engine.EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			var calls atomic.Int32
			lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pas@2.2"}, nil, config{ConformanceEnforcement: level}, engine.DefaultLaneURL, func(context.Context, string, string) error { calls.Add(1); return errors.New("validator down") })
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			if level == engine.EnforcementNone && (len(lanes) != 0 || len(m.defaults) != 0 || calls.Load() != 0) {
				t.Fatal("none created validator work")
			}
		})
	}
}

func TestNoValidatorNativeKeepsExplicitAdaptationIndependent(t *testing.T) {
	var creations atomic.Int32
	factory := adaptationValidatorFactory(config{FHIRValidateURL22: "http://validator.test/fhir"}, "", false, func(base string) shnsdk.Validator { creations.Add(1); return engine.NewLineFakeValidator("2.2") })
	if creations.Load() != 0 {
		t.Fatal("startup created an adaptation client")
	}
	if got := factory("pa.pas", "2.0"); got != nil {
		t.Fatal("absent source checker invented")
	}
	v := factory("pa.pas", "2.2")
	if v == nil || creations.Load() != 1 {
		t.Fatal("explicit adaptation did not obtain its checker")
	}
	if factory("pa.pas", "2.2") != v || creations.Load() != 1 {
		t.Fatal("duplicate adaptation client")
	}
}
