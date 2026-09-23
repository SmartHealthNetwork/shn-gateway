package engine

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestContractLineSet_DuplicatesAndFilter: the registrar ADMITS duplicate
// declared tokens by design (messageFrames precedent — validateContractVersions
// has no dedup), so every consumer builds a SET. A recorded spec requirement.
func TestContractLineSet_DuplicatesAndFilter(t *testing.T) {
	got := contractLineSet([]string{"pa.pas@2.0", "pa.pas@2.0", "pa.crd@2.1", "pa.pas@2.2", "garbage"}, "pa.pas")
	if len(got) != 2 || !got["2.0"] || !got["2.2"] {
		t.Fatalf("lines = %v, want {2.0, 2.2} (duplicates collapsed, other contracts + malformed filtered)", got)
	}
}

// TestCompareLines_Numeric: lines compare numerically per dot segment —
// 2.10 > 2.9 > 2.0; a missing segment is 0 (2 == 2.0 < 2.1).
func TestCompareLines_Numeric(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.0", "2.0", 0}, {"2.1", "2.0", 1}, {"2.9", "2.10", -1},
		{"2", "2.0", 0}, {"2.0", "2.0.1", -1}, {"10.0", "9.9", 1},
	}
	for _, tc := range cases {
		if got := compareLines(tc.a, tc.b); got != tc.want {
			t.Errorf("compareLines(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSelectContractToken pins the routing rule: highest
// common line wins deterministically; a silent peer (declared NOTHING) routes
// at own line (pre-contract build — rollout safety); a peer with a non-empty
// declaration missing the contract, or sharing no line, is REFUSED (both-sides-
// know: a published capability list is exhaustive for routing, unlike the
// checks drift rule which compares two descriptions of the same endpoint).
func TestSelectContractToken(t *testing.T) {
	own := []string{"pa.pas@2.0", "pa.crd@2.0"}
	cases := []struct {
		name        string
		peer        []string
		contract    string
		wantToken   string
		wantRefused bool
	}{
		{"neutral contract never filters", []string{"pa.pas@9.9"}, "", "", false},
		{"silent peer routes at own line", nil, "pa.pas", "pa.pas@2.0", false},
		{"shared line selected", []string{"pa.pas@2.0"}, "pa.pas", "pa.pas@2.0", false},
		{"duplicates tolerated", []string{"pa.pas@2.0", "pa.pas@2.0"}, "pa.pas", "pa.pas@2.0", false},
		{"highest common wins", []string{"pa.pas@2.0", "pa.pas@2.2"}, "pa.pas", "pa.pas@2.0", false}, // own has only 2.0
		{"no shared line refused", []string{"pa.pas@2.2"}, "pa.pas", "", true},
		{"declared set missing the contract refused", []string{"pa.crd@2.0"}, "pa.pas", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, refused := selectContractToken(own, tc.peer, len(tc.peer) > 0, tc.contract)
			if tok != tc.wantToken || refused != tc.wantRefused {
				t.Fatalf("selectContractToken = (%q,%v), want (%q,%v)", tok, refused, tc.wantToken, tc.wantRefused)
			}
		})
	}
}

// TestSilentPeerRoutesAtHighestDeclared: a silent peer routes at
// the originator's highest DECLARED line, not its highest native line. The
// build is tri-line native; the declaration decides.
func TestSilentPeerRoutesAtHighestDeclared(t *testing.T) {
	cases := []struct {
		name string
		own  []string
		want string
	}{
		{"declares 2.0 only, native reaches 2.2", []string{"pa.pas@2.0", "pa.crd@2.0"}, "pa.pas@2.0"},
		{"declares 2.0 and 2.2", []string{"pa.pas@2.0", "pa.pas@2.2"}, "pa.pas@2.2"},
		{"declares 2.1 only", []string{"pa.pas@2.1"}, "pa.pas@2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, refused := selectContractToken(tc.own, nil, false, "pa.pas")
			if refused || tok != tc.want {
				t.Fatalf("silent peer: got (%q, refused=%v), want %q", tok, refused, tc.want)
			}
		})
	}
}

// TestPACatalog_Contracts pins the legType→contract column (the verified
// mapping). Version-neutral legs are EXPLICITLY "" — an unmapped new
// legType must make a deliberate choice, so the pin lists every key.
func TestPACatalog_Contracts(t *testing.T) {
	want := map[string]string{
		"coverage-eligibility":    "",
		"crd-order-select":        "pa.crd",
		"crd-order-dispatch":      "pa.crd",
		"dtr-questionnaire-fetch": "pa.dtr",
		"federated-query":         "",
		"patient-dtr":             "",
		"pas-claim":               "pa.pas",
		"pas-claim-update":        "pa.pas",
		"pas-claim-inquire":       "pa.pas",
	}
	if len(paCatalog) != len(want) {
		t.Fatalf("catalog has %d legs, pin has %d — update BOTH", len(paCatalog), len(want))
	}
	for leg, contract := range want {
		if paCatalog[leg].Contract != contract {
			t.Errorf("paCatalog[%q].Contract = %q, want %q", leg, paCatalog[leg].Contract, contract)
		}
	}
}

// TestSelectLegToken_RefusalIsLegible: the refusal names the failing contract,
// the leg, and BOTH parties' declared tokens (AI-G11's legible-422 grammar,
// "no match, no bridge → legible refusal").
func TestSelectLegToken_RefusalIsLegible(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-x", shnsdk.RegistryEntry{ID: "payer-x", Role: "payer",
		ContractVersions: []string{"pa.pas@2.2", "pa.pas@2.2"}}) // duplicate on purpose
	g := &Gateway{cfg: Config{Reg: reg}}

	tok, err := g.selectLegToken("payer-x", "pas-claim")
	if tok != "" || err == nil {
		t.Fatalf("want refusal, got (%q, %v)", tok, err)
	}
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %T", err)
	}
	msg := rre.Error()
	for _, must := range []string{"pa.pas", "pas-claim", "payer-x", "pa.pas@2.0", "pa.pas@2.2"} {
		if !strings.Contains(msg, must) {
			t.Fatalf("refusal %q missing %q", msg, must)
		}
	}
	if strings.Count(msg, "pa.pas@2.2") != 1 {
		t.Fatalf("duplicate declared token must collapse in the refusal: %q", msg)
	}

	// Silent peer: no refusal, own line selected.
	reg.Set("payer-old", shnsdk.RegistryEntry{ID: "payer-old", Role: "payer"})
	tok, err = g.selectLegToken("payer-old", "pas-claim")
	if err != nil || tok != "pa.pas@2.0" {
		t.Fatalf("silent peer: got (%q, %v), want (pa.pas@2.0, nil)", tok, err)
	}

	// Version-neutral leg: no token, no refusal, even against the 2.2-only peer.
	tok, err = g.selectLegToken("payer-x", "federated-query")
	if err != nil || tok != "" {
		t.Fatalf("neutral leg: got (%q, %v)", tok, err)
	}
}

// TestPendStatePinsContractLine: the pended-line pin (SETTLED: the pin lives in pendState, NEVER ExchangeStore — AI-1). The pin is
// selected once at run-to-PENDED and honored by the resume leg even if the
// recipient's declaration changes to an incompatible line mid-pend — a pended
// exchange finishes on the line it started on.
func TestPendStatePinsContractLine(t *testing.T) {
	// Hermetic core (no full scenario needed): select → pin → registry flips →
	// pinned OriginateLeg still routes (the pin-honor test proves the leg
	// mechanics; THIS test proves the pendState carriage).
	reg := shnsdk.NewRegistry()
	reg.Set("payer-x", shnsdk.RegistryEntry{ID: "payer-x", Role: "payer", ContractVersions: []string{"pa.pas@2.0"}})
	g := &Gateway{cfg: Config{Reg: reg}, pending: map[string]pendState{}}
	tok, err := g.selectLegToken("payer-x", "pas-claim")
	if err != nil || tok != "pa.pas@2.0" {
		t.Fatalf("select: (%q, %v)", tok, err)
	}
	token := g.storePending(pendState{scenario: "uc06", recipient: "payer-x", pasToken: tok})
	// Mid-pend drift: the peer now declares an incompatible line.
	reg.Set("payer-x", shnsdk.RegistryEntry{ID: "payer-x", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
	st, ok := g.loadPending(token)
	if !ok || st.pasToken != "pa.pas@2.0" {
		t.Fatalf("pin lost across store/load: %+v", st)
	}
	// A FRESH selection now refuses — proving the pin is what keeps the resume
	// leg alive (and that nothing silently re-selects).
	if _, err := g.selectLegToken("payer-x", "pas-claim"); err == nil {
		t.Fatal("fresh selection against the drifted peer must refuse; the resume leg survives only via the pin")
	}
}

// TestSelectLegToken_RefusalContractNotDeclared: a peer with a non-empty
// declaration that omits the failing contract entirely renders RouteRefusalError's
// Peer as the literal "(contract not declared)" (versionroute.go), distinct from
// the "declares a non-shared line" case above.
func TestSelectLegToken_RefusalContractNotDeclared(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-y", shnsdk.RegistryEntry{ID: "payer-y", Role: "payer",
		ContractVersions: []string{"pa.crd@2.0"}}) // declares ONLY a different contract
	g := &Gateway{cfg: Config{Reg: reg}}

	_, err := g.selectLegToken("payer-y", "pas-claim")
	if err == nil {
		t.Fatal("want refusal")
	}
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %T", err)
	}
	if !strings.Contains(rre.Error(), "(contract not declared)") {
		t.Fatalf("refusal %q missing %q", rre.Error(), "(contract not declared)")
	}
}

// TestSelectRoutePrefersDeclaredThenNativeReachThenChain: three
// fixtures, one per arm, all against a peer that declares ONLY pa.pas@2.2
// while THIS build declares only pa.pas@2.0 — so arm (1) shared-declared
// never fires and every case genuinely exercises the widened outcome.
func TestSelectRoutePrefersDeclaredThenNativeReachThenChain(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
	fake := syntheticFakeValidator()

	t.Run("arm 2: native reach when 2.2 is laned", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.2": fake},
		}}
		route, err := g.selectLegRoute("payer-22", "pas-claim")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
			t.Fatalf("route = %+v, want native reach @2.2 (nil Chain)", route)
		}
	})

	t.Run("same without optional validator lanes remains native", func(t *testing.T) {
		g := &Gateway{cfg: Config{Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"}}}
		route, err := g.selectLegRoute("payer-22", "pas-claim")
		if err != nil || route.Token != "pa.pas@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
			t.Fatalf("native route depends on optional lane: %+v %v", route, err)
		}
	})

	t.Run("same with the lane but native restricted (D1c): arm 3 chain", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
			EgressNativeLines: []string{"2.0"}, // D1c: restrict arm (2)'s view away from 2.2
		}}
		route, err := g.selectLegRoute("payer-22", "pas-claim")
		if err != nil {
			t.Fatalf("want a chained route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.2" || route.BuildLine != "2.0" {
			t.Fatalf("route = %+v, want Token=pa.pas@2.2 BuildLine=2.0", route)
		}
		if len(route.Chain) != 2 {
			t.Fatalf("want a 2-hop pa.pas 2.0->2.2 chain, got %d steps: %+v", len(route.Chain), route.Chain)
		}
	})
}

// TestSelectRouteNativeReachHighestAmongMultipleDeclaredPeerLines (arm (2)):
// the "several native+laned peer-declared lines, no
// shared declared line" tie-break was correct in code (selectNativeReachRoute's
// `compareLines(t, best) > 0` loop) but had no fixture pinning it — every
// existing arm-2 fixture only ever gave the peer ONE declared line. Here the
// peer declares BOTH 2.1 and 2.2 (own declares only 2.0, so arm (1) never
// fires); with both lines laned, arm (2) must pick the HIGHEST (2.2), not
// just the first native line that matches.
func TestSelectRouteNativeReachHighestAmongMultipleDeclaredPeerLines(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-2122", shnsdk.RegistryEntry{ID: "payer-2122", Role: "payer", ContractVersions: []string{"pa.pas@2.1", "pa.pas@2.2"}})
	fake := syntheticFakeValidator()

	t.Run("both peer lines laned: highest (2.2) wins", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
		}}
		route, err := g.selectLegRoute("payer-2122", "pas-claim")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
			t.Fatalf("route = %+v, want native reach @2.2 (nil Chain) — the highest of the peer's two declared lines", route)
		}
	})

	t.Run("control: 2.2 builder excluded, next-highest native line (2.1)", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines: []string{"2.0", "2.1"}, // no executable 2.2 builder
			// A certification-only client for 2.2 does not make it a lane either.
			CertificationValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
		}}
		route, err := g.selectLegRoute("payer-2122", "pas-claim")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.1" || route.BuildLine != "2.1" || route.Chain != nil {
			t.Fatalf("route = %+v, want native reach @2.1 (2.2 builder unavailable so it's skipped, not 2.2)", route)
		}
	})
}

// TestSelectRouteChainRankingFixedTargetPrefersShorterChain (chain ranking):
// this fixture closes a coverage gap — intra-target chain ranking was correct
// in code (chainBetter's class-rank -> step-count -> highest-source-line
// cascade) but had no fixture pinning it for a FIXED target with several
// own-declared sources — every existing chain fixture only ever offered ONE
// own-declared source line.
//
// Primary subtest is the concrete motivating case: own declares {2.0, 2.1}, the
// peer declares {2.2} only, and D1c (EgressNativeLines) restricts arm
// (2)'s native view so 2.2 is unreachable natively, forcing arm (3). Two
// candidate chains reach 2.2: 2.0->2.1->2.2 (2 steps, worst step class GATED
// — the 2.0<->2.1 row) and 2.1->2.2 (1 step, class CARRY — the 2.1<->2.2
// row). chainBetter's FIRST tie-break is worst-step-class (full < carry <
// gated), so this real pa.pas pair is actually decided by class ranking
// (carry beats gated) before step count ever gets consulted — recorded
// honestly rather than mislabeled as a pure step-count fixture. The control
// subtest below isolates D1b's SECOND tie-break (fewer steps beat more) with
// class held equal, using pa.crd's uniform-full manifest rows (compat.go).
// The class-ordering sub-rule in true isolation (same step count, differing
// class, same target) has no fixturable competing pair anywhere in the real
// manifest — every contract has exactly one row per adjacent line pair, so a
// chain's step count and its worst class always co-vary with the chosen
// source; fabricating fake manifest rows to force that isolation was
// deliberately avoided per review instruction.
func TestSelectRouteChainRankingFixedTargetPrefersShorterChain(t *testing.T) {
	fake := syntheticFakeValidator()

	t.Run("motivating case: pa.pas own={2.0,2.1} peer={2.2} — the 1-step 2.1->2.2 chain wins over the 2-step 2.0->2.1->2.2 chain", func(t *testing.T) {
		reg := shnsdk.NewRegistry()
		reg.Set("payer-22only", shnsdk.RegistryEntry{ID: "payer-22only", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0", "pa.pas@2.1"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
			EgressNativeLines: []string{"2.0", "2.1"}, // D1c: restrict arm (2)'s view away from 2.2, forcing arm (3)
		}}
		route, err := g.selectLegRoute("payer-22only", "pas-claim")
		if err != nil {
			t.Fatalf("want a chained route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.2" || route.BuildLine != "2.1" {
			t.Fatalf("route = %+v, want Token=pa.pas@2.2 BuildLine=2.1 (the 1-step chain from the 2.1 source)", route)
		}
		if len(route.Chain) != 1 {
			t.Fatalf("want the 1-step 2.1->2.2 chain, got %d steps: %+v", len(route.Chain), route.Chain)
		}
	})

	t.Run("control: pa.crd own={2.0,2.1} peer={2.2}, both candidate chains are class-FULL (compat.go's uniform CRD rows) — fewer steps alone decides", func(t *testing.T) {
		reg := shnsdk.NewRegistry()
		reg.Set("payer-crd22only", shnsdk.RegistryEntry{ID: "payer-crd22only", Role: "payer", ContractVersions: []string{"pa.crd@2.2"}})
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.crd@2.0", "pa.crd@2.1"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
			EgressNativeLines: []string{"2.0", "2.1"}, // D1c: forces arm (3) despite 2.2 being native
		}}
		route, err := g.selectLegRoute("payer-crd22only", "crd-order-select")
		if err != nil {
			t.Fatalf("want a chained route, got refusal: %v", err)
		}
		if route.Token != "pa.crd@2.2" || route.BuildLine != "2.1" {
			t.Fatalf("route = %+v, want Token=pa.crd@2.2 BuildLine=2.1 (the 1-step chain — class ties at FULL, so fewer steps wins)", route)
		}
		if len(route.Chain) != 1 {
			t.Fatalf("want the 1-step 2.1->2.2 chain, got %d steps: %+v", len(route.Chain), route.Chain)
		}
	})
}

// TestStrictPeerRefusesCarryChain (per-peer strict extensions, FR-G52): the
// SELECTION-level proof that a strict input really reaches arm (3) through
// g.strictPeer, via the real g.selectLegRoute — not the exported
// SelectChainRouteForTest wrapper TestAdversarial_StrictPeerRefusesChainAtSelection
// already covers. strictPeer is production-dormant BY DESIGN (always false —
// see its comment, originate.go), so the strict input
// here is injected through Config.StrictPeerForTest, a TEST-ONLY
// seam — never through a config path any real deployment can set. Combined
// with the EgressNativeLines seam to force arm (3) to fire even
// though the target line is native, exactly like
// TestSelectRoutePrefersDeclaredThenNativeReachThenChain's third case. NO
// assertion here routes through native.go — its arm-1-only pin
// (TestNativeForwardStaysArm1) forbids that topology; the dormant-plumbing
// byte-identical fence lives in native_test.go's
// TestNativeStrictExtensionsFieldIsDormant.
func TestStrictPeerRefusesCarryChain(t *testing.T) {
	// pa.pas 2.0<->2.1's manifest row is Class=gated (compat.go) — a real,
	// non-stub chain a strict peer must refuse AT SELECTION, naming the
	// overlay.
	fake := syntheticFakeValidator()
	gatedReg := shnsdk.NewRegistry()
	gatedReg.Set("payer-21", shnsdk.RegistryEntry{ID: "payer-21", Role: "payer", ContractVersions: []string{"pa.pas@2.1"}})

	t.Run("strict ON (test seam): a carry/gated-worst chain refuses, naming the overlay", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: gatedReg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines: []string{"2.0"}, // D1c: force arm 3 (2.1 is otherwise arm-2 native reach)
			StrictPeerForTest: true,            // test-only: strictPeer is hardwired false in production
		}}
		_, err := g.selectLegRoute("payer-21", "pas-claim")
		if err == nil {
			t.Fatal("want a strict refusal, got a route")
		}
		var rre *RouteRefusalError
		if !errors.As(err, &rre) {
			t.Fatalf("want *RouteRefusalError, got %T", err)
		}
		if !strings.Contains(rre.Error(), "refused for this peer (gated overlay") {
			t.Fatalf("refusal %q missing the gated-overlay bridge phrase", rre.Error())
		}
	})

	t.Run("control: strict OFF (the real production default), the SAME chain succeeds", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: gatedReg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines: []string{"2.0"},
			// StrictPeerForTest left at its zero value (false) — this IS
			// what every real deployment sees (strictPeer is unconditional).
		}}
		route, err := g.selectLegRoute("payer-21", "pas-claim")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.1" || route.BuildLine != "2.0" {
			t.Fatalf("route = %+v, want Token=pa.pas@2.1 BuildLine=2.0", route)
		}
	})

	t.Run("strict ON (test seam), a full-only chain still passes (gates lossy legs, not translation)", func(t *testing.T) {
		// pa.crd 2.0<->2.1 is Class=full (compat.go) — strict must not touch it.
		crdReg := shnsdk.NewRegistry()
		crdReg.Set("payer-crd21", shnsdk.RegistryEntry{ID: "payer-crd21", Role: "payer", ContractVersions: []string{"pa.crd@2.1"}})
		g := &Gateway{cfg: Config{
			Reg: crdReg, DeclaredContractVersions: []string{"pa.crd@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines: []string{"2.0"},
			StrictPeerForTest: true,
		}}
		route, err := g.selectLegRoute("payer-crd21", "crd-order-select")
		if err != nil {
			t.Fatalf("want a route (full-only chain, strict must not refuse it), got refusal: %v", err)
		}
		if route.Token != "pa.crd@2.1" || route.BuildLine != "2.0" || len(route.Chain) != 1 {
			t.Fatalf("route = %+v, want Token=pa.crd@2.1 BuildLine=2.0 chainLen=1", route)
		}
	})
}

// TestSelectRouteRefusalNamesBridgeIngredient (arm (4)): no chain
// / missing target lane / gated-peer each produce a DISTINCT legible
// message — the RouteRefusalError grammar extension.
func TestSelectRouteRefusalNamesBridgeIngredient(t *testing.T) {
	fake := syntheticFakeValidator()

	t.Run("missing target lane", func(t *testing.T) {
		reg := shnsdk.NewRegistry()
		reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake},
			EgressNativeLines: []string{"2.0"}, // force actual adaptation; native needs no lane
		}}
		_, err := g.selectLegRoute("payer-22", "pas-claim")
		assertBridgeIssue(t, err, "no configured validator lane for line 2.2")
	})

	t.Run("no transform chain (target line unknown to the manifest)", func(t *testing.T) {
		reg := shnsdk.NewRegistry()
		// "9.9" is native to nothing and has no manifest row — artificially
		// laned here so the test isolates "no chain" from "missing lane".
		reg.Set("payer-99", shnsdk.RegistryEntry{ID: "payer-99", Role: "payer", ContractVersions: []string{"pa.pas@9.9"}})
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "9.9": fake},
		}}
		_, err := g.selectLegRoute("payer-99", "pas-claim")
		assertBridgeIssue(t, err, "no transform chain bridges to line 9.9")
	})

	t.Run("gated peer (strict input excludes a lossy chain)", func(t *testing.T) {
		// pa.pas 2.0<->2.1's manifest row is Class=gated (compat.go) — a real,
		// non-stub chain a strict peer must refuse. strict is the arm-3 INPUT
		// (dormant plumbing in production — no call site sets it
		// true yet); selectChainRoute is exercised directly.
		laned := func(l string) bool { return true }
		_, issue, ok := selectChainRoute("pa.pas",
			map[string]bool{"2.0": true}, map[string]bool{"2.1": true}, true, laned)
		if ok {
			t.Fatal("want a strict refusal, got a route")
		}
		if !strings.Contains(issue, "refused for this peer (gated overlay") {
			t.Fatalf("issue = %q, missing the gated-peer bridge phrase", issue)
		}
		// Non-strict: the SAME gated-class chain is a valid (worst-ranked) route.
		route, _, ok := selectChainRoute("pa.pas",
			map[string]bool{"2.0": true}, map[string]bool{"2.1": true}, false, laned)
		if !ok || route.Token != "pa.pas@2.1" {
			t.Fatalf("non-strict: want the gated chain selected, got %+v ok=%v", route, ok)
		}
	})
}

func assertBridgeIssue(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("want refusal")
	}
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %T", err)
	}
	if !strings.Contains(rre.Error(), want) {
		t.Fatalf("refusal %q missing bridge issue %q", rre.Error(), want)
	}
}

// TestOriginateLegFallbackStaysIntersectionOnly pins the caller×arm matrix:
// OriginateLeg's empty-Content.ProfileID fallback (gateway.go,
// selectLegToken) STAYS INTERSECTION-ONLY. A peer that declares a line this
// build could reach via native-reach or a transform chain, but shares NO
// declared line, must still refuse here — an arm-2/3 token would mis-stamp
// bytes that are ALREADY BUILT with no egress-adapt run.
func TestOriginateLegFallbackStaysIntersectionOnly(t *testing.T) {
	reg := shnsdk.NewRegistry()
	// 2.2 is native+laned (an arm-2-worthy peer for selectLegRoute) — the
	// fallback must not be tempted.
	reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
	fake := syntheticFakeValidator()
	g := &Gateway{cfg: Config{
		Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.2": fake},
	}, pending: map[string]pendState{}}

	_, err := g.OriginateLeg(context.Background(), nil, "payer-22", "pas-claim", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, ProfileID: "", Payload: testRequest([]byte(`{}`))})
	if err == nil {
		t.Fatal("want refusal — the fallback must never route via native reach or a transform chain")
	}
	var rre *RouteRefusalError
	if !errors.As(err, &rre) {
		t.Fatalf("want *RouteRefusalError, got %T: %v", err, err)
	}
	// Confirm it is genuinely arm-1's plain refusal (no BridgeIssue) — arms
	// 2/3 were never even attempted from this call site.
	if rre.BridgeIssue != "" {
		t.Fatalf("BridgeIssue = %q, want empty — this fallback must never attempt arms 2/3", rre.BridgeIssue)
	}
}

// The knob must never affect a shared-declared-line leg: arm 1 consults
// declared sets only ("demo mode doesn't break normal runs").
func TestEgressNativeLinesDoesNotAffectSharedDeclaredLine(t *testing.T) {
	reg := shnsdk.NewRegistry()
	reg.Set("payer-20", shnsdk.RegistryEntry{ID: "payer-20", Role: "payer", ContractVersions: []string{"pa.pas@2.0"}})
	g := &Gateway{cfg: Config{
		Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
		EgressNativeLines: []string{"2.1"}, // 2.0 deliberately NOT in the narrowed view
	}}
	route, err := g.selectLegRoute("payer-20", "pas-claim")
	if err != nil {
		t.Fatalf("want a route, got refusal: %v", err)
	}
	if route.Token != "pa.pas@2.0" || route.BuildLine != "2.0" || route.Chain != nil {
		t.Fatalf("route = %+v, want arm-1 shared-declared @2.0 (nil Chain), unaffected by EgressNativeLines", route)
	}
}

// Resume containment, three ways: the pinned line is taken as DECLARED first
// (the same rule fresh selection's arm (1) applies, which the knob does not
// touch), then as natively reachable under the knob, then by chain — and a pin
// none of the three can reach is refused.
func TestSelectResumeRouteUnderNarrowing(t *testing.T) {
	fake := syntheticFakeValidator()

	t.Run("knob permits the pinned line: native resume, no chain", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake},
			EgressNativeLines:        []string{"2.0"},
		}}
		route, err := g.selectResumeRoute("pa.pas@2.0", "payer-20", "pas-claim-update")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.0" || route.BuildLine != "2.0" || route.Chain != nil {
			t.Fatalf("route = %+v, want native resume @2.0 (nil Chain)", route)
		}
	})

	// The pin records the line the SUBMISSION was sent at, and fresh selection
	// sends at a shared DECLARED line without consulting EgressNativeLines at
	// all. A resume that ignored the declared set refused the very line the
	// submission had just used — an authorization that could be created and
	// then not continued.
	t.Run("the knob excludes the pinned line but it is still declared: native resume", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake, "2.2": fake},
			EgressNativeLines:        []string{"2.2"}, // 2.0 (the pin) is NOT in the native-reach view
		}}
		route, err := g.selectResumeRoute("pa.pas@2.0", "payer-20", "pas-claim-inquire")
		if err != nil {
			t.Fatalf("want the declared pinned line, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.0" || route.BuildLine != "2.0" || route.Chain != nil {
			t.Fatalf("route = %+v, want a native resume @2.0 (nil Chain) on the declared line", route)
		}
	})

	// REJECTION: the widening above is scoped to a pinned line this gateway
	// still declares AND still has a lane for. Neither holding, and with no
	// chain to it, the resume is refused — it does not fall back to some other
	// line and call it the same authorization.
	t.Run("a pinned line this gateway neither declares nor can reach is refused", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake},
			EgressNativeLines:        []string{"2.0"},
		}}
		if route, err := g.selectResumeRoute("pa.dtr@2.2", "payer-20", "pas-claim-inquire"); err == nil {
			t.Fatalf("want a refusal for a pin this gateway cannot reach, got route %+v", route)
		}
	})

	t.Run("knob excludes the pinned line: falls to the chain block", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.1"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines:        []string{"2.1"}, // 2.0 (the pin) is NOT in view
		}}
		route, err := g.selectResumeRoute("pa.pas@2.0", "payer-20", "pas-claim-update")
		if err != nil {
			t.Fatalf("want a chained route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.0" || route.BuildLine != "2.1" {
			t.Fatalf("route = %+v, want Token=pa.pas@2.0 BuildLine=2.1 (the legible 2.1->2.0 fall-through)", route)
		}
		if len(route.Chain) != 1 {
			t.Fatalf("want the 1-step 2.1->2.0 chain, got %d steps: %+v", len(route.Chain), route.Chain)
		}
	})
}

// Native continuation reachability is a builder/route fact. Optional runtime
// validator readiness cannot strand a pend whose submit already succeeded.
func TestSelectResumeRouteNativeWithoutValidator(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		t.Run(level.String(), func(t *testing.T) {
			g := &Gateway{cfg: Config{
				DeclaredContractVersions: []string{"pa.pas@2.0"},
				ConformanceEnforcement:   level,
			}}
			for _, leg := range []string{"pas-claim-update", "pas-claim-inquire"} {
				route, err := g.selectResumeRoute("pa.pas@2.0", "payer", leg)
				if err != nil || route.Token != "pa.pas@2.0" || route.BuildLine != "2.0" || len(route.Chain) != 0 {
					t.Fatalf("%s route=%+v err=%v", leg, route, err)
				}
			}
		})
	}
}

// TestLegRecordStaysVersionFree pins LegRecord's field inventory. The pended-
// line pin lives in pendState by SETTLED DECISION (AI-1: ExchangeStore is
// metadata-only, "gates nothing", and a stored line that later drives
// builder/validator selection would violate that contract). If this test
// fails, someone added a field to LegRecord — do NOT put version/routing
// state there; pendState (or a future durable pend store) is the home.
func TestLegRecordStaysVersionFree(t *testing.T) {
	typ := reflect.TypeOf(LegRecord{})
	want := []string{"Type", "CorrelationID", "Subjects", "Physics", "Outcome"}
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("LegRecord fields = %v, pinned %v — read this test's comment before updating", got, want)
	}
}

// --- select-before-build promotion: the CRD/DTR sites promoted off OriginateLeg's
// arm-1-only backfill onto select-before-build. Five sites were promoted by the
// site census (2026-08-14): crd-order-select ×2 + crd-order-dispatch
// (originate.go, originate_homeoxygen.go) and the DaVinciIngress driver's
// crd-order-select + dtr-questionnaire-fetch (ingress.go). The ingress PAS site
// stays on the fallback (forward-edge deferral — see its site comment).
// TestOriginateLegFallbackStaysIntersectionOnly is deliberately UNTOUCHED: the
// fallback keeps its signed arm-1-only semantics; D-7 moved callers OFF it, it
// did not widen it.

// d7SetPeerContractVersions re-registers the harness payer with `tokens` as its
// DECLARED contract-version set, preserving every other registry field (enc/sign
// keys, advertised message frames) so ONLY the routing axis changes between rows.
func d7SetPeerContractVersions(t *testing.T, env *inProcessExchange, tokens ...string) {
	t.Helper()
	entry, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatalf("harness payer %q not registered", env.payerID)
	}
	entry.ContractVersions = tokens
	env.originator.cfg.Reg.Set(env.payerID, entry)
}

// d7CaptureEvents wires the observer seam on the harness gateway and returns a
// pointer to the accumulating event slice (read after the handler returns — the
// engine emits synchronously on the calling goroutine).
func d7CaptureEvents(t *testing.T, env *inProcessExchange) func() []ObserverEvent {
	evs := &[]ObserverEvent{}
	env.originator.cfg.Observer = func(e ObserverEvent) { *evs = append(*evs, e) }
	return func() []ObserverEvent { observationFlush(t, env.originator); return *evs }
}

func d7EventsOfKind(evs []ObserverEvent, kind string) []ObserverEvent {
	var out []ObserverEvent
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func d7Kinds(evs []ObserverEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// d7OneEventOfKind asserts exactly one event of `kind` and returns it.
func d7OneEventOfKind(t *testing.T, evs []ObserverEvent, kind string) ObserverEvent {
	t.Helper()
	got := d7EventsOfKind(evs, kind)
	if len(got) != 1 {
		t.Fatalf("want exactly one %s event, got %d (all kinds: %v)", kind, len(got), d7Kinds(evs))
	}
	return got[0]
}

// TestD7PromotedSitesPreserveIntersectingTopologies is pin (a) — the
// BLAST-RADIUS FENCE for the D-7 promotion. The five promoted sites run in every
// production UC, so the promotion is only safe if, on every topology that
// ALREADY had a shared declared line (arm 1) or a silent peer, the
// select-before-build path (selectLegRoute) yields EXACTLY the token the legacy
// empty-ProfileID backfill (selectLegToken) chose — same token, no chain,
// BuildLine == the token's own line. Arms 2/3 may only ever fire where the
// legacy path REFUSED; they may never re-decide a topology that already routed.
// If this fails, the promotion changed live routing, not just reachability.
func TestD7PromotedSitesPreserveIntersectingTopologies(t *testing.T) {
	fake := syntheticFakeValidator()
	// The legTypes D-7 promoted, spanning both promoted contracts.
	legTypes := []string{"crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch"}

	topologies := []struct {
		name string
		own  []string
		peer []string // nil ⇒ a SILENT (pre-contract) peer: declares nothing
	}{
		{"silent peer (pre-contract holder)",
			[]string{"pa.crd@2.0", "pa.dtr@2.0"}, nil},
		{"identical single line",
			[]string{"pa.crd@2.0", "pa.dtr@2.0"}, []string{"pa.crd@2.0", "pa.dtr@2.0"}},
		{"peer superset of own",
			[]string{"pa.crd@2.0", "pa.dtr@2.0"},
			[]string{"pa.crd@2.0", "pa.crd@2.1", "pa.crd@2.2", "pa.dtr@2.0", "pa.dtr@2.1"}},
		{"own superset of peer",
			[]string{"pa.crd@2.0", "pa.crd@2.1", "pa.crd@2.2", "pa.dtr@2.0", "pa.dtr@2.2"},
			[]string{"pa.crd@2.1", "pa.dtr@2.0"}},
		{"two shared lines: highest common wins",
			[]string{"pa.crd@2.0", "pa.crd@2.1", "pa.dtr@2.0", "pa.dtr@2.1"},
			[]string{"pa.crd@2.0", "pa.crd@2.1", "pa.dtr@2.0", "pa.dtr@2.1"}},
		{"one shared LOW line while both also declare disjoint highs",
			[]string{"pa.crd@2.0", "pa.crd@2.1", "pa.dtr@2.0", "pa.dtr@2.1"},
			[]string{"pa.crd@2.0", "pa.crd@2.2", "pa.dtr@2.0", "pa.dtr@2.2"}},
	}

	for _, tc := range topologies {
		t.Run(tc.name, func(t *testing.T) {
			reg := shnsdk.NewRegistry()
			reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", ContractVersions: tc.peer})
			g := &Gateway{cfg: Config{
				Reg: reg, DeclaredContractVersions: tc.own,
				// Every line laned, so arms 2/3 are fully ARMED — the fence is
				// only meaningful if the widened path COULD have chosen
				// differently and still doesn't.
				ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
			}}
			for _, legType := range legTypes {
				legacy, lerr := g.selectLegToken("payer", legType)
				if lerr != nil {
					t.Fatalf("%s: legacy backfill refused an intersecting topology: %v", legType, lerr)
				}
				route, rerr := g.selectLegRoute("payer", legType)
				if rerr != nil {
					t.Fatalf("%s: promoted path refused a topology the backfill routed: %v", legType, rerr)
				}
				if route.Token != legacy {
					t.Fatalf("%s: promoted token = %q, legacy backfill chose %q — the promotion MUST NOT re-decide an already-routing topology",
						legType, route.Token, legacy)
				}
				if route.Chain != nil {
					t.Fatalf("%s: promoted route carries a %d-step chain on an arm-1 topology (%+v) — transform-iff violated",
						legType, len(route.Chain), route.Chain)
				}
				if route.BuildLine != shnsdk.LineOf(legacy) {
					t.Fatalf("%s: BuildLine = %q, want %q (arm 1 builds natively at the routed line)",
						legType, route.BuildLine, shnsdk.LineOf(legacy))
				}
			}
		})
	}
}

// TestD7CRDArm2ReachesSkewedPeer is pin (b) at the selection layer, for BOTH
// promoted pa.crd legTypes: a peer declaring only pa.crd@2.2 (a legitimate
// new-from-birth or foreign holder) is now REACHED at 2.2 by native reach, not
// refused — the exact hole D-7 exists to close. Chain nil: the bytes are built
// natively at 2.2, nothing is transformed.
func TestD7CRDArm2ReachesSkewedPeer(t *testing.T) {
	fake := syntheticFakeValidator()
	reg := shnsdk.NewRegistry()
	reg.Set("payer-crd22", shnsdk.RegistryEntry{ID: "payer-crd22", Role: "payer", ContractVersions: []string{"pa.crd@2.2"}})
	g := &Gateway{cfg: Config{
		Reg: reg, DeclaredContractVersions: []string{"pa.crd@2.0"},
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.2": fake},
	}}
	for _, legType := range []string{"crd-order-select", "crd-order-dispatch"} {
		route, err := g.selectLegRoute("payer-crd22", legType)
		if err != nil {
			t.Fatalf("%s: want an arm-2 route to the 2.2-only peer, got refusal: %v", legType, err)
		}
		if route.Token != "pa.crd@2.2" || route.BuildLine != "2.2" || route.Chain != nil {
			t.Fatalf("%s: route = %+v, want native reach @2.2 with a nil Chain", legType, route)
		}
	}
}

// A producer declaration is independent of this gateway's own built line and
// validator lanes. The signed 2.2 body reaches an endpoint advertising 2.2.
func TestD7CRDIngressNativeProducerDeclarationReachesSkewedPeer(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.2")
	env.originator.cfg.Validator = nil
	env.originator.cfg.ValidatorsByLine = nil
	evs := d7CaptureEvents(t, env)
	body := conformantCRDRequest("MBR-COVERED")
	rec := ingressAtVersion(t, env, "shn-order-select", "pa.crd@2.2", body)
	if rec.Code != 200 || env.routeHitCount() != 1 {
		t.Fatalf("native delivery %d %s hits=%d", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	if len(d7EventsOfKind(evs(), legTransformedKind)) != 0 {
		t.Fatal("native carriage invoked a transform")
	}
	got := env.lastRequestPayload()
	hdr, carried, err := shnsdk.DecodeHTTPFrame(got)
	if err != nil || hdr.Headers[shnsdk.FrameHeaderContractVersion] != "pa.crd@2.2" {
		t.Fatalf("producer declaration lost: header=%+v err=%v", hdr, err)
	}
	if !bytes.Equal(carried, sentRequest(t, env)) || bytes.Contains(carried, []byte(`"fhirAuthorization"`)) {
		t.Fatalf("wrong carried bytes: %s", carried)
	}
}

type countingExchangeStore struct {
	ExchangeStore
	begins int
}

func (c *countingExchangeStore) Begin(workstream string) *Exchange {
	c.begins++
	return c.ExchangeStore.Begin(workstream)
}

// A signed source declaration cannot be relabeled to a recipient that does
// not advertise it. The failed attempt is recorded, but no Hub route is made.
func TestD7CRDIngressIncompatibleProducerRefusesBeforeHub(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.pas@2.0")
	spy := &countingExchangeStore{ExchangeStore: env.originator.exchanges}
	env.originator.exchanges = spy
	rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "no registered native endpoint") || env.routeHitCount() != 0 {
		t.Fatalf("incompatible native route: %d %s hits=%d", rec.Code, rec.Body.String(), env.routeHitCount())
	}
	if spy.begins != 1 {
		t.Fatalf("exchange attempts=%d, want one audited attempt", spy.begins)
	}
}

func TestD7CRDIngressSharedLineControl(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0")
	evs := d7CaptureEvents(t, env)
	spy := &countingExchangeStore{ExchangeStore: env.originator.exchanges}
	env.originator.exchanges = spy
	rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
	if rec.Code != 200 || env.routeHitCount() != 1 || spy.begins != 1 {
		t.Fatalf("shared native route: %d %s hits=%d begins=%d", rec.Code, rec.Body.String(), env.routeHitCount(), spy.begins)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(env.lastRequestPayload())
	if err != nil || hdr.Headers[shnsdk.FrameHeaderContractVersion] != "pa.crd@2.0" || !bytes.Equal(body, sentRequest(t, env)) {
		t.Fatalf("shared-line producer declaration/body changed: header=%+v err=%v", hdr, err)
	}
	if len(d7EventsOfKind(evs(), legTransformedKind)) != 0 {
		t.Fatal("shared native route transformed")
	}
}

// TestD7CRDArm3IdentityChainIsBytePreserving is pin (c). Under D1c narrowing
// (arm 2's native view restricted to 2.0), a pa.crd@2.2-only peer is reached by
// arm 3 — the REAL 2.0->2.1->2.2 chain over compat.go's identity rows. Because
// both rows carry nil Up/Down, applyChain's nil-func branch is a pure
// pass-through (transform.go) — no re-marshal — so the egress bytes are
// BYTE-IDENTICAL to the input while leg.transformed still reports both hops
// honestly with empty loss content. CRD legs are deliberately NOT in any
// envelope carve-out set: this byte-identity rests on the
// identity chain actually RUNNING, so a future real CRD transform module must
// run or refuse honestly, never be bypassed.
func TestD7CRDArm3IdentityChainIsBytePreserving(t *testing.T) {
	fake := syntheticFakeValidator()
	reg := shnsdk.NewRegistry()
	reg.Set("payer-crd22", shnsdk.RegistryEntry{ID: "payer-crd22", Role: "payer", ContractVersions: []string{"pa.crd@2.2"}})
	var evs []ObserverEvent
	g := &Gateway{cfg: Config{
		Reg: reg, DeclaredContractVersions: []string{"pa.crd@2.0"},
		ValidatorsByLine:  map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake},
		EgressNativeLines: []string{"2.0"}, // D1c: hold arm 2 away from 2.2 so arm 3 must bridge
		HolderID:          "provider",
		Clock:             func() time.Time { return time.Unix(1700000000, 0).UTC() },
		Observer:          func(e ObserverEvent) { evs = append(evs, e) },
	}}

	route, err := g.selectLegRoute("payer-crd22", "crd-order-select")
	if err != nil {
		t.Fatalf("want an arm-3 chained route, got refusal: %v", err)
	}
	if route.Token != "pa.crd@2.2" || route.BuildLine != "2.0" {
		t.Fatalf("route = %+v, want Token=pa.crd@2.2 BuildLine=2.0", route)
	}
	if len(route.Chain) != 2 {
		t.Fatalf("want the 2-hop pa.crd 2.0->2.1->2.2 chain, got %d steps: %+v", len(route.Chain), route.Chain)
	}

	in := []byte(`{"hook":"order-select","hookInstance":"hi-1","context":{"patientId":"MBR-COVERED"}}`)
	out, reports, err := g.egressAdapt(context.Background(), route, in,
		ExchangeIdentity{CorrelationID: "corr-d7", LegType: "crd-order-select", Counterpart: "payer-crd22"})
	if err != nil {
		t.Fatalf("egressAdapt over the pa.crd identity chain failed: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("identity chain altered the bytes:\n in = %s\nout = %s", in, out)
	}
	observationFlush(t, g)
	if len(reports) != 2 {
		t.Fatalf("want one LossReport per hop (2), got %d: %+v", len(reports), reports)
	}
	wantModules := []string{"pa.crd 2.0->2.1", "pa.crd 2.1->2.2"}
	for i, r := range reports {
		if r.Module != wantModules[i] {
			t.Fatalf("report[%d].Module = %q, want %q", i, r.Module, wantModules[i])
		}
		if len(r.Carried) != 0 || len(r.Synthesized) != 0 {
			t.Fatalf("report[%d] = %+v, want EMPTY loss content — pa.crd is identity across all three lines", i, r)
		}
	}
	transformed := d7OneEventOfKind(t, evs, legTransformedKind)
	if transformed.LegType != "crd-order-select" || transformed.Counterpart != "payer-crd22" || transformed.CorrelationID != "corr-d7" {
		t.Fatalf("leg.transformed identity = %+v, want the ExchangeIdentity the site passed", transformed)
	}
}

// The native ingress carries either producer-declared line without an
// implicit 2.0→2.2 identity walk or a validator dependency.
func TestD7CRDIngressNativeSkewDoesNotInvokeIdentityChain(t *testing.T) {
	var bodies [][]byte
	for _, version := range []string{"pa.crd@2.0", "pa.crd@2.2"} {
		t.Run(version, func(t *testing.T) {
			env := newTransportExchange(t)
			d7SetPeerContractVersions(t, env, version)
			env.originator.cfg.Validator = nil
			env.originator.cfg.ValidatorsByLine = nil
			env.originator.cfg.EgressNativeLines = []string{"2.0"}
			evs := d7CaptureEvents(t, env)
			rec := ingressAtVersion(t, env, "shn-order-select", version, conformantCRDRequest("MBR-COVERED"))
			if rec.Code != 200 || env.routeHitCount() != 1 {
				t.Fatalf("native %s: %d %s", version, rec.Code, rec.Body.String())
			}
			if len(d7EventsOfKind(evs(), legTransformedKind)) != 0 {
				t.Fatal("hidden identity walk")
			}
			hdr, body, err := shnsdk.DecodeHTTPFrame(env.lastRequestPayload())
			if err != nil || hdr.Headers[shnsdk.FrameHeaderContractVersion] != version {
				t.Fatalf("header=%+v err=%v", hdr, err)
			}
			bodies = append(bodies, bytes.Clone(body))
		})
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("native bodies differ: %q / %q", bodies[0], bodies[1])
	}
}

// Both original byte-mutation controls remain live. Native carriage of a
// producer-declared representation never invokes even a locally registered
// cross-line step, so neither an assertion edit nor whitespace is introduced.
func TestD7CRDIngressNativeSkipsMutatedIdentitySteps(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"one byte of the request changed", func(in []byte) []byte {
			out := bytes.Clone(in)
			i := bytes.Index(out, []byte(`"hookInstance":"`))
			if i < 0 {
				return append(out, 'x')
			}
			out[i+len(`"hookInstance":"`)] ^= 1
			return out
		}},
		{"whitespace appended", func(in []byte) []byte { return append(bytes.Clone(in), '\n') }},
	} {
		t.Run(row.name, func(t *testing.T) {
			idx := -1
			for i, step := range compatManifest {
				if step.Contract == "pa.crd" && step.From == "2.0" && step.To == "2.1" {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatal("missing registered step")
			}
			saved := compatManifest[idx]
			t.Cleanup(func() { compatManifest[idx] = saved })
			walked := 0
			compatManifest[idx].Up = func(p []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
				walked++
				return row.change(p), LossReport{Module: "pa.crd 2.0->2.1", Source: "2.0", Target: "2.1"}, nil
			}
			body := conformantCRDRequest("MBR-COVERED")
			control := newTransportExchange(t)
			d7SetPeerContractVersions(t, control, "pa.crd@2.2")
			if rec := ingressAtVersion(t, control, "shn-order-select", "pa.crd@2.2", body); rec.Code != 200 {
				t.Fatalf("control %d %s", rec.Code, rec.Body.String())
			}
			want := bytes.Clone(sentRequest(t, control))
			env := newTransportExchange(t)
			d7SetPeerContractVersions(t, env, "pa.crd@2.2")
			env.originator.cfg.EgressNativeLines = []string{"2.0"}
			evs := d7CaptureEvents(t, env)
			rec := ingressAtVersion(t, env, "shn-order-select", "pa.crd@2.2", body)
			if rec.Code != 200 || env.routeHitCount() != 1 || walked != 0 || !bytes.Equal(sentRequest(t, env), want) {
				t.Fatalf("native changed: status=%d hits=%d walks=%d got=%s want=%s", rec.Code, env.routeHitCount(), walked, sentRequest(t, env), want)
			}
			if len(d7EventsOfKind(evs(), legTransformedKind)) != 0 {
				t.Fatal("native delivery claimed transformation")
			}
		})
	}
}

// An explicitly selected CRD identity bridge must refuse bytes its registered
// step changes. The third row ensures a step cannot mutate the caller's input
// and thereby make a later equality check compare two altered slices.
func TestD7CRDIngressExplicitIdentityRejectsChangedBytes(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"hookInstance byte", func(in []byte) []byte {
			out := bytes.Clone(in)
			out[bytes.Index(out, []byte(`"hookInstance":"`))+len(`"hookInstance":"`)] ^= 1
			return out
		}},
		{"trailing whitespace", func(in []byte) []byte { return append(bytes.Clone(in), '\n') }},
		{"in-place hookInstance byte", func(in []byte) []byte {
			in[bytes.Index(in, []byte(`"hookInstance":"`))+len(`"hookInstance":"`)] ^= 1
			return in
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			idx := -1
			for i, step := range compatManifest {
				if step.Contract == "pa.crd" && step.From == "2.0" && step.To == "2.1" {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatal("missing registered CRD identity step")
			}
			saved := compatManifest[idx]
			t.Cleanup(func() { compatManifest[idx] = saved })
			compatManifest[idx].Up = func(p []byte, x ExchangeIdentity) ([]byte, LossReport, error) {
				return row.change(p), LossReport{Module: "pa.crd 2.0->2.1", Source: "2.0", Target: "2.1"}, nil
			}
			env := newTransportExchange(t)
			d7SetPeerContractVersions(t, env, "pa.crd@2.2")
			fake := syntheticFakeValidator()
			env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake}
			env.originator.cfg.EgressNativeLines = []string{"2.0"}
			env.originator.cfg.DemoEdgeCapture = true
			route, err := env.originator.selectLegRoute("payer", "crd-order-select")
			if err != nil || len(route.Chain) != 2 {
				t.Fatalf("explicit route=%+v err=%v", route, err)
			}
			original := []byte(`{"hook":"order-select","hookInstance":"h1","context":{"patientId":"MBR-COVERED"}}`)
			want := bytes.Clone(original)
			evs := d7CaptureEvents(t, env)
			out, reports, err := env.originator.egressAdapt(context.Background(), route, original,
				ExchangeIdentity{CorrelationID: "changed-identity", LegType: "crd-order-select", Counterpart: "payer"})
			var typed *ingressContextError
			if !errors.As(err, &typed) || typed.status != 502 || typed.code != "adaptation_failed" || out != nil || reports != nil {
				t.Fatalf("changed identity accepted: out=%s reports=%+v err=%v", out, reports, err)
			}
			if !bytes.Equal(original, want) {
				t.Fatalf("adapter mutated caller's source bytes: %s", original)
			}
			if len(d7EventsOfKind(evs(), "leg.failed")) != 1 || len(d7EventsOfKind(evs(), legTransformedKind)) != 0 || env.routeHitCount() != 0 {
				t.Fatalf("failure was misreported or dispatched: events=%v hits=%d", d7Kinds(evs()), env.routeHitCount())
			}
			if _, ok := env.originator.edgeCaptureLookup("changed-identity"); ok {
				t.Fatal("refused transform entered successful edge capture")
			}
		})
	}
}

func TestD7CRDIngressNativeWithoutValidatorDoesNotDemandLane(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.2")
	env.originator.cfg.Validator = nil
	env.originator.cfg.ValidatorsByLine = nil
	rec := ingressAtVersion(t, env, "shn-order-select", "pa.crd@2.2", conformantCRDRequest("MBR-COVERED"))
	if rec.Code != 200 || env.routeHitCount() != 1 {
		t.Fatalf("native unlaned status=%d %s", rec.Code, rec.Body.String())
	}
}

func TestD7CRDIngressExplicitBridgeMissingLaneRefuses(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.2")
	env.originator.cfg.EgressNativeLines = []string{"2.0"}
	env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": syntheticFakeValidator()}
	_, err := env.originator.selectLegRoute("payer", "crd-order-select")
	var refusal *RouteRefusalError
	if !errors.As(err, &refusal) || !strings.Contains(refusal.BridgeIssue, "no configured validator lane for line 2.2") {
		t.Fatalf("explicit bridge missing source/target proof: %v", err)
	}
	if env.routeHitCount() != 0 {
		t.Fatal("bridge selection dispatched")
	}
}

// The edge capture is populated only by explicit adaptation. Its before/after
// bytes are the source-ready body carried at the participant boundary.
func TestD7CRDIngressExplicitIdentityCaptureHoldsSourceReadyBytes(t *testing.T) {
	env := newTransportExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.0")
	env.originator.cfg.DemoEdgeCapture = true
	body := ehrRequest(supported)
	rec := ingressAtVersion(t, env, "shn-order-sign", "pa.crd@2.0", body)
	if rec.Code != 200 {
		t.Fatalf("native ingress %d %s", rec.Code, rec.Body.String())
	}
	sent := sentRequest(t, env)
	for _, s := range []string{"fhirAuthorization", "ehr-secret-token", "fhirServer"} {
		if bytes.Contains(sent, []byte(s)) {
			t.Fatalf("source-ready bytes contain %s", s)
		}
	}
	d7SetPeerContractVersions(t, env, "pa.crd@2.2")
	fake := syntheticFakeValidator()
	env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake}
	env.originator.cfg.EgressNativeLines = []string{"2.0"}
	route, err := env.originator.selectLegRoute("payer", "crd-order-select")
	if err != nil || len(route.Chain) != 2 {
		t.Fatalf("explicit route=%+v err=%v", route, err)
	}
	evs := d7CaptureEvents(t, env)
	out, _, err := env.originator.egressAdapt(context.Background(), route, sent, ExchangeIdentity{CorrelationID: "explicit-capture", LegType: "crd-order-select", Counterpart: "payer"})
	if err != nil || !bytes.Equal(out, sent) {
		t.Fatalf("identity output=%s err=%v", out, err)
	}
	if len(d7EventsOfKind(evs(), legTransformedKind)) != 1 {
		t.Fatal("explicit identity transform not observed")
	}
	capture, ok := env.originator.edgeCaptureLookup("explicit-capture")
	if !ok || !bytes.Equal(capture.Before, sent) || !bytes.Equal(capture.After, sent) {
		t.Fatalf("capture=%+v ok=%v, sent=%s", capture, ok, sent)
	}
}
