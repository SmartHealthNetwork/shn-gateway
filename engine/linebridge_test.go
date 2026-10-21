package engine

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Module 2, the IG-line bridge, is the transform chain (arm (3)), E-08 and the
// bridged check. Building natively at a peer's line (arm (2)) is module 1
// choosing its build line, not translation, so it is not module 2's (ruled
// 2026-10-19). Module 2 is the participant's switch (TRANSLATE_IG_LINES,
// Config.TranslateIGLines), off by default. With module 2 off no arm (3)
// chain is ever selected, a fresh or resumed leg is built natively at a line
// it reaches or carried at this gateway's own line, and an answer is carried
// exactly as it arrived. Each row runs with module 2 on as its control.

// bridgeGateway is a gateway declaring pa.pas@2.1, laned at 2.0, 2.1 and 2.2,
// with arm (2)'s native view narrowed to native (nil for the build's whole
// set), and the payer declaring only pa.pas@2.2. Its chain to 2.2 is the
// registered 2.1->2.2 step.
func bridgeGateway(native []string, off bool) *Gateway {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
	fake := shnsdk.NewFakeValidator()
	return &Gateway{cfg: Config{TranslateIGLines: !off,
		Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.1"},
		ValidatorsByContractLine: contractLanes(map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake}),
		EgressNativeLines:        native,
	}}
}

func TestModuleTwoOff_ArmTwoStillBuildsNatively(t *testing.T) {
	for _, off := range []bool{false, true} {
		route, err := bridgeGateway(nil, off).selectLegRoute("payer-22", "pas-claim", "")
		if err != nil {
			t.Fatalf("module 2 off=%v: %v", off, err)
		}
		if route.Token != "pa.pas@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
			t.Fatalf("module 2 off=%v: route %+v, want built natively at 2.2 with no chain", off, route)
		}
	}
}

func TestModuleTwoOff_NoChainIsSelected(t *testing.T) {
	// The control: with arm (2) narrowed away from 2.2, module 2 reaches it by a
	// chain from 2.1.
	on, err := bridgeGateway([]string{"2.1"}, false).selectLegRoute("payer-22", "pas-claim", "")
	if err != nil || on.Token != "pa.pas@2.2" || on.BuildLine != "2.1" || len(on.Chain) == 0 {
		t.Fatalf("module 2 on: route %+v, %v; want a chain from 2.1 to 2.2", on, err)
	}
	off, err := bridgeGateway([]string{"2.1"}, true).selectLegRoute("payer-22", "pas-claim", "")
	if err != nil {
		t.Fatalf("module 2 off: %v", err)
	}
	if off.Token != "pa.pas@2.1" || off.BuildLine != "2.1" || off.Chain != nil {
		t.Fatalf("module 2 off: route %+v, want carried at this gateway's own 2.1 with no chain", off)
	}
	// The participant's own setting refuses the pair, as routing, either way.
	g := bridgeGateway([]string{"2.1"}, true)
	g.cfg.RequireSharedLine = true
	var rre *RouteRefusalError
	if _, err := g.selectLegRoute("payer-22", "pas-claim", ""); !errors.As(err, &rre) {
		t.Fatalf("module 2 off under REQUIRE_SHARED_LINE: %v, want the routing refusal", err)
	}
}

func TestModuleTwoOff_ResumeCarriesInsteadOfChaining(t *testing.T) {
	on, err := bridgeGateway([]string{"2.1"}, false).selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	if err != nil || on.Token != "pa.pas@2.2" || on.BuildLine != "2.1" || len(on.Chain) == 0 || on.Pinned != "" {
		t.Fatalf("module 2 on: route %+v, %v; want a chain from 2.1 to the pinned 2.2", on, err)
	}
	off, err := bridgeGateway([]string{"2.1"}, true).selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	if err != nil {
		t.Fatalf("module 2 off: %v", err)
	}
	if off.Token != "pa.pas@2.1" || off.Chain != nil || off.Pinned != "pa.pas@2.2" {
		t.Fatalf("module 2 off: route %+v, want carried at 2.1, naming the pin, with no chain", off)
	}
	// Arm (2) at the pin is the core's: natively laned, the pin is built
	// natively with module 2 off too.
	native, err := bridgeGateway(nil, true).selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	if err != nil || native.Token != "pa.pas@2.2" || native.BuildLine != "2.2" || native.Chain != nil {
		t.Fatalf("module 2 off, the pin native: route %+v, %v; want built natively at 2.2", native, err)
	}
}

// Module 2 on plans only what it can certify: a chain that takes a gated row
// upward (pa.pas 2.0->2.1) can only refuse, so a 2.0 build has no reach to
// the payer's 2.2 and is carried at 2.0, as with module 2 off.
func TestModuleTwoOn_AGatedChainIsNotPlanned(t *testing.T) {
	g := bridgeGateway([]string{"2.0"}, false)
	g.cfg.DeclaredContractVersions = []string{"pa.pas@2.0"}
	route, err := g.selectLegRoute("payer-22", "pas-claim", "")
	if err != nil || route.Token != "pa.pas@2.0" || route.Chain != nil {
		t.Fatalf("route %+v, %v; want carried at 2.0 with no chain", route, err)
	}
	g.cfg.RequireSharedLine = true
	if _, err := g.selectLegRoute("payer-22", "pas-claim", ""); err == nil || !strings.Contains(err.Error(), "its chain needs the gated pa.pas 2.0 to 2.1 step") {
		t.Fatalf("under REQUIRE_SHARED_LINE: %v, want the routing refusal naming the gated step", err)
	}
	// At resume too: a pend pinned at 2.2 that only a chain from this 2.0 build
	// could reach is carried at 2.0, naming the pin, with no chain.
	resumed, err := g.selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	var resumeRefusal *RouteRefusalError
	if !errors.As(err, &resumeRefusal) {
		t.Fatalf("under REQUIRE_SHARED_LINE the resume = %+v, %v; want the routing refusal", resumed, err)
	}
	g.cfg.RequireSharedLine = false
	resumed, err = g.selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", "")
	if err != nil || resumed.Token != "pa.pas@2.0" || resumed.Chain != nil || resumed.Pinned != "pa.pas@2.2" {
		t.Fatalf("resume: route %+v, %v; want carried at 2.0, naming the pin, with no chain", resumed, err)
	}
	// The same gated row taken downward, from a 2.1 build to a 2.0-only payer,
	// is planned: its step drops what 2.0 has no slot for into carried content.
	down := bridgeGateway([]string{"2.1"}, false)
	down.cfg.Reg.Set("payer-20", shnsdk.RegistryEntry{ID: "payer-20", Role: "payer", ContractVersions: []string{"pa.pas@2.0"}})
	route, err = down.selectLegRoute("payer-20", "pas-claim", "")
	if err != nil || route.Token != "pa.pas@2.0" || route.BuildLine != "2.1" || len(route.Chain) != 1 || route.Chain[0].Class != StepGated {
		t.Fatalf("downward: route %+v, %v; want the gated 2.1->2.0 step planned", route, err)
	}
}

// A walk can only reach a gateway whose module 2 is off by a fault: it fails
// closed and sends nothing.
func TestModuleTwoOff_AChainedRouteFailsClosed(t *testing.T) {
	route, err := bridgeGateway([]string{"2.1"}, false).selectLegRoute("payer-22", "pas-claim", "")
	if err != nil || len(route.Chain) == 0 {
		t.Fatalf("fixture: route %+v, %v", route, err)
	}
	g := bridgeGateway([]string{"2.1"}, true)
	_, _, _, err = g.egressBuild(t.Context(), route, ExchangeIdentity{CorrelationID: "corr-off", LegType: "pas-claim"}, moduleBuild{
		Builder: relay.BuilderSDKPASSubmit, ContentType: "application/fhir+json", Body: []byte(`{"resourceType":"Bundle"}`), Contract: "pa.pas",
	})
	if !errors.Is(err, errBridgeOff) {
		t.Fatalf("a chained route with module 2 off: %v, want errBridgeOff", err)
	}
}

// With module 2 off, a payer's questionnaire package answer in another DTR
// line's names reaches the EHR exactly as it arrived, at every level: E-08 is
// module 2's. The control renames it.
func TestModuleTwoOff_TheDTRAnswerIsCarriedExactly(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false, "operationOutcome")
	renamed := e08Bridged(t, answer, "2.0", "2.2")
	if bytes.Equal(renamed, answer) {
		t.Fatal("fixture: nothing to rename")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			e08RowTuned(t, level, declares22, answer, nil, nil, nil, nil).wantAnswered(t, renamed, true)
			off := e08RowTuned(t, level, declares22, answer, nil, nil, nil, func(g *Gateway) { g.cfg.TranslateIGLines = false })
			off.wantAnswered(t, answer, false)
		})
	}
}
