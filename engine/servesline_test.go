package engine

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// allNativeLines declares every native line of every contract, the
// declaration a gateway at none can make with no validator.
var allNativeLines = NativeContractVersions()

// noValidatorGateway is a gateway with no validator at all: no canonical
// validator, no lane, no default lane. Only a gateway at none boots like this
// (gateway/app); level is the level under test.
func noValidatorGateway(level ConformanceEnforcement, requireShared bool) *Gateway {
	reg := shnsdk.NewRegistry()
	reg.Set("requester", shnsdk.RegistryEntry{ID: "requester", Role: "provider", ContractVersions: allNativeLines})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: allNativeLines})
	return &Gateway{cfg: Config{
		Reg:                      reg,
		DeclaredContractVersions: allNativeLines,
		ConformanceEnforcement:   level,
		RequireSharedLine:        requireShared,
	}}
}

// frameLegs is one leg of each contract, for its frame check: PA's, and each
// compiled-in test profile's.
var frameLegs = func() map[string]string {
	m := map[string]string{"pa.crd": "crd-order-dispatch", "pa.dtr": "dtr-questionnaire-fetch", "pa.pas": "pas-claim"}
	for _, leg := range profileLegs() {
		spec, _ := legSpecFor(leg)
		m[spec.Contract] = leg
	}
	return m
}()

// A gateway at none with no validator answers and continues at every line it
// declares: a request frame claiming one is answered at it, and a pend pinned
// to one resumes at it (FR-G29).
func TestNoneWithoutValidatorServesDeclaredLines(t *testing.T) {
	for _, requireShared := range []bool{false, true} {
		g := noValidatorGateway(EnforcementNone, requireShared)
		for _, tok := range allNativeLines {
			contract, line, _ := strings.Cut(tok, "@")
			if !g.servesLine(contract, line) {
				t.Errorf("requireShared=%v: %s not served", requireShared, tok)
			}
			_, answered, carried, status, msg := g.unframeRequestFrom("requester", frameLegs[contract], framedRequest(t, tok, []byte(`{}`)))
			if status != 0 || answered != tok || carried != "" {
				t.Errorf("requireShared=%v: frame %s = (%d %q, answered %q, carried %q), want answered at its line", requireShared, tok, status, msg, answered, carried)
			}
			if contract != "pa.pas" {
				continue
			}
			for _, leg := range []string{"pas-claim-update", "pas-claim-inquire"} {
				route, err := g.selectResumeRoute(tok, "payer", leg, "")
				if err != nil {
					t.Errorf("requireShared=%v: resume %s %s refused: %v", requireShared, leg, tok, err)
					continue
				}
				if route.Token != tok || route.BuildLine != line || route.Chain != nil {
					t.Errorf("requireShared=%v: resume %s %s = %+v, want the pinned line with no chain", requireShared, leg, tok, route)
				}
			}
		}
	}
}

// REJECTION: declaring a line serves it only at none. The same validator-less
// configuration at a level that runs checks (which gateway/app refuses to
// boot) answers no frame at its lines and does not resume at the pin: the lane
// alone serves a line there. The resume is carried away from the pin, or
// refused as routing under REQUIRE_SHARED_LINE.
func TestDeclaredWithoutLaneServesNothingAtCheckingLevels(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		g := noValidatorGateway(level, false)
		for _, tok := range allNativeLines {
			contract, line, _ := strings.Cut(tok, "@")
			if g.servesLine(contract, line) {
				t.Errorf("%v: %s served with no lane", level, tok)
			}
			// The claim is carried (the answer line is then the pair's shared
			// line, as for a bare request).
			_, _, carried, status, _ := g.unframeRequestFrom("requester", frameLegs[contract], framedRequest(t, tok, []byte(`{}`)))
			if status != 0 || carried != tok {
				t.Errorf("%v: frame %s = (%d, carried %q), want its claim carried, not honoured", level, tok, status, carried)
			}
		}
		if route, err := g.selectResumeRoute("pa.pas@2.0", "payer", "pas-claim-update", ""); err != nil || route.Pinned != "pa.pas@2.0" || route.Token == "pa.pas@2.0" {
			t.Errorf("%v: resume with no lane = %+v, %v, want it carried away from the pin", level, route, err)
		}
		g.cfg.RequireSharedLine = true
		_, err := g.selectResumeRoute("pa.pas@2.0", "payer", "pas-claim-update", "")
		var rre *RouteRefusalError
		if !errors.As(err, &rre) || !strings.Contains(rre.BridgeIssue, "no bridge to the pinned line 2.0") {
			t.Errorf("%v: resume with no lane under REQUIRE_SHARED_LINE = %v, want the pinned-line refusal", level, err)
		}
	}
}

// REJECTION: at none, a line the gateway does not declare and has no lane
// for is still not served. Declaring is what serves it.
func TestNoneServesOnlyDeclaredLines(t *testing.T) {
	g := noValidatorGateway(EnforcementNone, true)
	g.cfg.DeclaredContractVersions = []string{"pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0"}
	if g.servesLine("pa.pas", "2.2") {
		t.Fatal("an undeclared, unlaned line is served")
	}
	_, _, _, status, msg := g.unframeRequestFrom("requester", "pas-claim", framedRequest(t, "pa.pas@2.2", []byte(`{}`)))
	if status != http.StatusUnprocessableEntity || !strings.Contains(msg, "no FHIR validator lane for line 2.2") {
		t.Fatalf("undeclared frame under REQUIRE_SHARED_LINE = (%d %q), want the routing refusal", status, msg)
	}
	if _, err := g.selectResumeRoute("pa.pas@2.2", "payer", "pas-claim-update", relay.BuilderSDKPASUpdate); err == nil {
		t.Fatal("a pin at an undeclared, unlaned line resumed")
	}
}

// The predicate is the lane alone wherever a lane decides: a gateway with the
// same lane configuration selects the same routes at none as at observe
// (route selection unchanged).
func TestServesLineIsTheLaneWhenLaned(t *testing.T) {
	fake := shnsdk.NewFakeValidator()
	lanes := map[string]shnsdk.Validator{}
	for _, tok := range allNativeLines {
		lanes[shnsdk.LineOf(tok)] = fake
	}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		g := noValidatorGateway(level, true)
		g.cfg.Validator = fake
		g.cfg.ValidatorsByContractLine = contractLanes(lanes)
		for _, tok := range allNativeLines {
			contract, line, _ := strings.Cut(tok, "@")
			if !g.servesLine(contract, line) {
				t.Errorf("%v: laned %s not served", level, tok)
			}
		}
		g.cfg.DeclaredContractVersions = []string{"pa.pas@2.0"}
		if route, err := g.selectResumeRoute("pa.pas@2.2", "payer", "pas-claim-update", relay.BuilderSDKPASUpdate); err != nil || route.BuildLine != "2.2" || route.Chain != nil {
			t.Errorf("%v: laned undeclared pin = %+v, %v, want native reach at 2.2", level, route, err)
		}
	}
}

// A gateway at none with no validator never selects a bridged transform, and
// native reach still needs a lane for every builder not certified at the
// peer's line (a certified one is TestBuildsAt's and
// TestNativeReachBuildsUnlanedOnlyForACertifiedBuilderAtNone's). A peer that
// shares no declared line is carried at this gateway's own line, unchanged, or
// refused routing under REQUIRE_SHARED_LINE.
func TestNoneWithoutValidatorNeverBridges(t *testing.T) {
	for _, requireShared := range []bool{false, true} {
		g := noValidatorGateway(EnforcementNone, requireShared)
		g.cfg.DeclaredContractVersions = []string{"pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0"}
		g.cfg.Reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.crd@2.2", "pa.dtr@2.2", "pa.pas@2.2"}})
		for _, contract := range []string{"pa.crd", "pa.dtr", "pa.pas"} {
			if route, ok := g.selectNativeReachRoute(contract, map[string]bool{"2.2": true}, carriedRequest); ok {
				t.Errorf("%s: native reach with no lane: %+v", contract, route)
			}
		}
		for _, leg := range []string{"crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim"} {
			route, err := g.selectLegRoute("payer-22", leg, "")
			if requireShared {
				var rre *RouteRefusalError
				if !errors.As(err, &rre) {
					t.Errorf("%s under REQUIRE_SHARED_LINE = %+v, %v, want the routing refusal", leg, route, err)
				}
				continue
			}
			if err != nil || route.Chain != nil || route.BuildLine != "2.0" || !strings.HasSuffix(route.Token, "@2.0") {
				t.Errorf("%s = %+v, %v, want carried at its own 2.0 with no chain", leg, route, err)
			}
		}
		route, err := g.selectResumeRoute("pa.pas@2.2", "payer-22", "pas-claim-update", relay.BuilderSDKPASUpdate)
		if requireShared {
			if err == nil {
				t.Errorf("a pin at an undeclared line resumed under REQUIRE_SHARED_LINE: %+v", route)
			}
		} else if err != nil || route.Chain != nil || route.Token != "pa.pas@2.0" || route.Pinned != "pa.pas@2.2" {
			t.Errorf("a pin at an undeclared line = %+v, %v, want it carried at pa.pas@2.0 with no chain", route, err)
		}
	}
}
