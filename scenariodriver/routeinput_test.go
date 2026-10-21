package scenariodriver

import (
	"encoding/json"
	"errors"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func routePersona() shnsdk.DiscoveryScenarioPersona {
	c := func(sys, code string) *shnsdk.PASCoding { return &shnsdk.PASCoding{System: sys, Code: code} }
	return shnsdk.DiscoveryScenarioPersona{Scenario: "uc04", Lane: "provider-data", MemberID: "MBR-PD-UC04", Order: &shnsdk.DiscoveryOrder{
		System: "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", Code: "G0151",
		ClaimType:       c(shnsdk.PASSystemClaimType, "professional"),
		Priority:        c(shnsdk.PASSystemProcessPriority, "normal"),
		ServiceCategory: c(shnsdk.PASSystemX12ServiceType, "42"),
	}, Amendment: &shnsdk.DiscoveryAmendment{CertificationType: c("https://codesystem.x12.org/005010/1322", "I")}}
}

func TestScenarioRouteInput_CarriesThePersonaStatement(t *testing.T) {
	in, err := ScenarioRouteInput(routePersona(), true, map[string]any{"branch": "x"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	var got struct {
		Member     string               `json:"member"`
		Order      map[string]string    `json:"order"`
		Branch     string               `json:"branch"`
		ClaimFacts shnsdk.PASClaimFacts `json:"claimFacts"`
		CertType   json.RawMessage      `json:"certificationType"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Member != "MBR-PD-UC04" || got.Order["code"] != "G0151" || got.Branch != "x" || got.ClaimFacts.ServiceCategory == nil || string(got.CertType) != `{"system":"https://codesystem.x12.org/005010/1322","code":"I"}` {
		t.Fatalf("route input = %s", raw)
	}
	if in, _ := ScenarioRouteInput(routePersona(), false, nil); in["certificationType"] != nil {
		t.Fatal("a route that sends no amendment states no certificationType")
	}
}

// An amending route whose persona states no certificationType refuses: the
// driver supplies none of its own.
func TestScenarioRouteInput_AmendmentWithoutStatedCertificationTypeRefuses(t *testing.T) {
	for name, mutate := range map[string]func(*shnsdk.DiscoveryScenarioPersona){
		"no amendment":            func(p *shnsdk.DiscoveryScenarioPersona) { p.Amendment = nil },
		"no certificationType":    func(p *shnsdk.DiscoveryScenarioPersona) { p.Amendment.CertificationType = nil },
		"empty certificationType": func(p *shnsdk.DiscoveryScenarioPersona) { p.Amendment.CertificationType.Code = "" },
	} {
		p := routePersona()
		mutate(&p)
		if in, err := ScenarioRouteInput(p, true, nil); err == nil || !errors.Is(err, ErrNoScenarioPersona) || in != nil {
			t.Errorf("%s: input=%v err=%v, want a refusal", name, in, err)
		}
		if _, err := ScenarioRouteInput(p, false, nil); err != nil {
			t.Errorf("%s on a route that sends no amendment: %v, want built", name, err)
		}
	}
}

// A persona statement missing a required fact refuses: the driver supplies none.
func TestScenarioRouteInput_MissingFactRefuses(t *testing.T) {
	for name, drop := range map[string]func(*shnsdk.DiscoveryOrder){
		"claimType":       func(o *shnsdk.DiscoveryOrder) { o.ClaimType = nil },
		"priority":        func(o *shnsdk.DiscoveryOrder) { o.Priority = nil },
		"serviceCategory": func(o *shnsdk.DiscoveryOrder) { o.ServiceCategory = nil },
	} {
		p := routePersona()
		drop(p.Order)
		if in, err := ScenarioRouteInput(p, false, nil); err == nil || !errors.Is(err, ErrNoScenarioPersona) || in != nil {
			t.Errorf("persona without %s: input=%v err=%v, want a refusal", name, in, err)
		}
	}
	p := routePersona()
	p.MemberID = ""
	if _, err := ScenarioRouteInput(p, false, nil); !errors.Is(err, ErrNoScenarioPersona) {
		t.Errorf("persona without a member: err=%v", err)
	}
	if _, err := ScenarioRouteBody(shnsdk.Discovery{}, "uc04", "provider-data", "", false, nil); !errors.Is(err, ErrNoScenarioPersona) {
		t.Errorf("descriptor without the persona: err=%v", err)
	}
	if _, err := ScenarioRouteInput(routePersona(), false, map[string]any{"member": "MBR-OTHER"}); err == nil {
		t.Error("a caller naming its own member: want a refusal")
	}
}

func TestScenarioKey(t *testing.T) {
	for _, tc := range []struct {
		path, branch, want string
		amend              bool
	}{
		{"/scenario/uc01", "covered", "uc01", false},
		{"/scenario/uc01", "notcovered", "uc01-notcovered", false},
		{"/scenario/uc05?wait=30", "noconsent", "uc05-noconsent", true},
		{"/scenario/uc03", "line-carry", "uc03-line-carry", false},
		{"/scenario/uc06/start", "", "uc06", false},
		{"/scenario/uc06/complete", "", "uc06", true},
		{"/scenario/uc04", "", "uc04", true},
	} {
		got, amend := ScenarioKey(tc.path, tc.branch)
		if got != tc.want || amend != tc.amend {
			t.Errorf("ScenarioKey(%q,%q) = %q,%v want %q,%v", tc.path, tc.branch, got, amend, tc.want, tc.amend)
		}
	}
}
