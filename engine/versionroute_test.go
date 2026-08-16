package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
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
	fake := shnsdk.NewFakeValidator()

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

	t.Run("same minus the 2.2 lane: refuses naming the missing lane", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake}, // no 2.2 lane
		}}
		_, err := g.selectLegRoute("payer-22", "pas-claim")
		if err == nil {
			t.Fatal("want refusal — 2.2 is native but unlaned, no bridge available")
		}
		var rre *RouteRefusalError
		if !errors.As(err, &rre) {
			t.Fatalf("want *RouteRefusalError, got %T", err)
		}
		if !strings.Contains(rre.Error(), "no configured validator lane for line 2.2") {
			t.Fatalf("refusal %q missing the missing-lane bridge issue", rre.Error())
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
	fake := shnsdk.NewFakeValidator()

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

	t.Run("control: the 2.2 lane absent, falls back to the next-highest declared+laned line (2.1)", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.1": fake}, // no 2.2 lane
		}}
		route, err := g.selectLegRoute("payer-2122", "pas-claim")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.1" || route.BuildLine != "2.1" || route.Chain != nil {
			t.Fatalf("route = %+v, want native reach @2.1 (2.2 unlaned so it's skipped, not 2.2)", route)
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
	fake := shnsdk.NewFakeValidator()

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
	fake := shnsdk.NewFakeValidator()
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
	fake := shnsdk.NewFakeValidator()

	t.Run("missing target lane", func(t *testing.T) {
		reg := shnsdk.NewRegistry()
		reg.Set("payer-22", shnsdk.RegistryEntry{ID: "payer-22", Role: "payer", ContractVersions: []string{"pa.pas@2.2"}})
		g := &Gateway{cfg: Config{
			Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake},
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
	fake := shnsdk.NewFakeValidator()
	g := &Gateway{cfg: Config{
		Reg: reg, DeclaredContractVersions: []string{"pa.pas@2.0"},
		ValidatorsByLine: map[string]shnsdk.Validator{"2.0": fake, "2.2": fake},
	}, pending: map[string]pendState{}}

	_, err := g.OriginateLeg(context.Background(), nil, "payer-22", "pas-claim", "pci-1", "corr-1", "",
		Content{WorkstreamType: workstreamPA, ProfileID: "", Bytes: []byte(`{}`)})
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

// Resume containment, both directions: selectResumeRoute IS
// knob-affected (reads nativeLinesView, never declared).
func TestSelectResumeRouteUnderNarrowing(t *testing.T) {
	fake := shnsdk.NewFakeValidator()

	t.Run("knob permits the pinned line: native resume, no chain", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.0"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake},
			EgressNativeLines:        []string{"2.0"},
		}}
		route, err := g.selectResumeRoute("pa.pas@2.0", "payer-20")
		if err != nil {
			t.Fatalf("want a route, got refusal: %v", err)
		}
		if route.Token != "pa.pas@2.0" || route.BuildLine != "2.0" || route.Chain != nil {
			t.Fatalf("route = %+v, want native resume @2.0 (nil Chain)", route)
		}
	})

	t.Run("knob excludes the pinned line: falls to the chain block", func(t *testing.T) {
		g := &Gateway{cfg: Config{
			DeclaredContractVersions: []string{"pa.pas@2.1"},
			ValidatorsByLine:         map[string]shnsdk.Validator{"2.0": fake, "2.1": fake},
			EgressNativeLines:        []string{"2.1"}, // 2.0 (the pin) is NOT in view
		}}
		route, err := g.selectResumeRoute("pa.pas@2.0", "payer-20")
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
func d7CaptureEvents(env *inProcessExchange) *[]ObserverEvent {
	evs := &[]ObserverEvent{}
	env.originator.cfg.Observer = func(e ObserverEvent) { *evs = append(*evs, e) }
	return evs
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
	fake := shnsdk.NewFakeValidator()
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
	fake := shnsdk.NewFakeValidator()
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

// TestD7CRDIngressRoutesSkewedPeerInsteadOfRefusing is the INGRESS-LEVEL skew
// pin required by the census adjudication (site 4, ingress.go's
// handleCRDIngress). Before the promotion this drive 422'd on OriginateLeg's
// arm-1-only backfill; after it, the partner's CDS Hooks request reaches a
// pa.crd@2.2-only peer, stamped at the routed line. pa.crd bytes are LINE-INERT
// by verified derivation (compat.go's identity rows), so stamping the routed
// line is as truthful for partner-built bytes as for our own.
func TestD7CRDIngressRoutesSkewedPeerInsteadOfRefusing(t *testing.T) {
	env := newInProcessExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.2") // no shared declared line: own default is pa.crd@2.0
	evs := d7CaptureEvents(env)

	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, env.crdIngressRequest(t))

	if rec.Code != 200 {
		t.Fatalf("ingress status = %d (%s), want 200 — a 2.2-only peer must now be REACHED, not refused",
			rec.Code, rec.Body.String())
	}
	if n := len(d7EventsOfKind(*evs, "leg.refused")); n != 0 {
		t.Fatalf("got %d leg.refused events, want 0", n)
	}
	originated := d7OneEventOfKind(t, *evs, "leg.originated")
	if originated.LegType != "crd-order-select" {
		t.Fatalf("leg.originated LegType = %q, want crd-order-select", originated.LegType)
	}
	if originated.Route == nil {
		t.Fatal("leg.originated carries Route: nil — the ingress site is still on the arm-1-only backfill")
	}
	if originated.Route.Token != "pa.crd@2.2" || originated.Route.BuildLine != "2.2" {
		t.Fatalf("Route = %+v, want Token=pa.crd@2.2 BuildLine=2.2", originated.Route)
	}
	if originated.Route.Chain != nil {
		t.Fatalf("Route.Chain = %+v, want nil — arm 2 builds natively, it never transforms", originated.Route.Chain)
	}
	if n := len(d7EventsOfKind(*evs, legTransformedKind)); n != 0 {
		t.Fatalf("got %d %s events on an arm-2 route, want 0 (transform-iff)", n, legTransformedKind)
	}
}

// countingExchangeStore wraps an ExchangeStore, counting Begin calls — the
// no-Exchange-record refusal pin's spy.
type countingExchangeStore struct {
	ExchangeStore
	begins int
}

func (c *countingExchangeStore) Begin(workstream string) *Exchange {
	c.begins++
	return c.ExchangeStore.Begin(workstream)
}

// TestD7CRDIngressRoutingRefusalCreatesNoExchangeRecord pins the ingress
// site's selection-precedes-Begin ordering (ingress.go's own comment:
// "Selection precedes the exchange so a refusal costs no Exchange record" —
// console-visible behavior, previously unpinned): a peer
// declaring NO pa.crd line refuses with the legible 422 + a leg.refused
// event, and the Exchange store never Begins.
func TestD7CRDIngressRoutingRefusalCreatesNoExchangeRecord(t *testing.T) {
	env := newInProcessExchange(t)
	d7SetPeerContractVersions(t, env, "pa.pas@2.0") // declares, but no pa.crd line at all
	evs := d7CaptureEvents(env)
	spy := &countingExchangeStore{ExchangeStore: env.originator.exchanges}
	env.originator.exchanges = spy

	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, env.crdIngressRequest(t))

	if rec.Code != 422 {
		t.Fatalf("ingress status = %d (%s), want 422 (route refusal)", rec.Code, rec.Body.String())
	}
	if n := len(d7EventsOfKind(*evs, "leg.refused")); n != 1 {
		t.Fatalf("got %d leg.refused events, want 1", n)
	}
	if n := len(d7EventsOfKind(*evs, "leg.originated")); n != 0 {
		t.Fatalf("got %d leg.originated events on a refused ingress, want 0", n)
	}
	if spy.begins != 0 {
		t.Fatalf("Exchange store Begin called %d times on a refused ingress, want 0 — a refusal must cost no Exchange record", spy.begins)
	}
}

// TestD7CRDIngressSharedLineControl is the behavior-preservation CONTROL for the
// ingress promotion (adjudication requirement): the ORDINARY topology — a peer
// sharing this build's declared pa.crd@2.0 — must keep routing exactly as
// before, at 2.0, with no chain and no transform. Pin (a) proves this at the
// selection layer; this proves it at the real site.
func TestD7CRDIngressSharedLineControl(t *testing.T) {
	env := newInProcessExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0")
	evs := d7CaptureEvents(env)
	spy := &countingExchangeStore{ExchangeStore: env.originator.exchanges}
	env.originator.exchanges = spy

	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, env.crdIngressRequest(t))

	if rec.Code != 200 {
		t.Fatalf("ingress status = %d (%s), want 200 on the ordinary shared-line topology", rec.Code, rec.Body.String())
	}
	originated := d7OneEventOfKind(t, *evs, "leg.originated")
	if originated.Route == nil || originated.Route.Token != "pa.crd@2.0" || originated.Route.BuildLine != "2.0" {
		t.Fatalf("Route = %+v, want the unchanged arm-1 Token=pa.crd@2.0 BuildLine=2.0", originated.Route)
	}
	if originated.Route.Chain != nil {
		t.Fatalf("Route.Chain = %+v, want nil on a shared declared line", originated.Route.Chain)
	}
	if n := len(d7EventsOfKind(*evs, legTransformedKind)); n != 0 {
		t.Fatalf("got %d %s events on the ordinary topology, want 0", n, legTransformedKind)
	}
	if spy.begins != 1 {
		t.Fatalf("Exchange store Begin called %d times on an accepted ingress, want 1 — an accepted leg costs exactly one Exchange record", spy.begins)
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
	fake := shnsdk.NewFakeValidator()
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
	out, reports, err := g.egressAdapt(route, in,
		ExchangeIdentity{CorrelationID: "corr-d7", LegType: "crd-order-select", Counterpart: "payer-crd22"})
	if err != nil {
		t.Fatalf("egressAdapt over the pa.crd identity chain failed: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("identity chain altered the bytes:\n in = %s\nout = %s", in, out)
	}
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

// TestD7CRDIngressArm3IdentityChainKeepsEgressBytes drives pin (c) at the REAL
// ingress site: the same conformant CDS Hooks fixture, once on the ordinary
// shared line (arm 1, no chain) and once forced through the arm-3 identity chain
// to a 2.2-only peer. The bytes handed to OriginateLeg must be identical across
// the two runs — the chain is genuinely a pass-through end to end, not just at
// the primitive.
func TestD7CRDIngressArm3IdentityChainKeepsEgressBytes(t *testing.T) {
	run := func(t *testing.T, arm3 bool) ([]byte, []ObserverEvent) {
		t.Helper()
		env := newInProcessExchange(t)
		if arm3 {
			d7SetPeerContractVersions(t, env, "pa.crd@2.2")
			fake := shnsdk.NewFakeValidator()
			env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": fake, "2.1": fake, "2.2": fake}
			env.originator.cfg.EgressNativeLines = []string{"2.0"} // D1c: force arm 3
		} else {
			d7SetPeerContractVersions(t, env, "pa.crd@2.0")
		}
		evs := d7CaptureEvents(env)
		rec := httptest.NewRecorder()
		env.originator.handleCRDIngress(rec, env.crdIngressRequest(t))
		if rec.Code != 200 {
			t.Fatalf("arm3=%v: ingress status = %d (%s), want 200", arm3, rec.Code, rec.Body.String())
		}
		originated := d7OneEventOfKind(t, *evs, "leg.originated")
		return originated.Payload, *evs
	}

	arm1Bytes, arm1Evs := run(t, false)
	arm3Bytes, arm3Evs := run(t, true)

	if !bytes.Equal(arm1Bytes, arm3Bytes) {
		t.Fatalf("arm-3 identity chain changed the egress bytes at the ingress site:\narm1 = %s\narm3 = %s", arm1Bytes, arm3Bytes)
	}
	if n := len(d7EventsOfKind(arm1Evs, legTransformedKind)); n != 0 {
		t.Fatalf("arm-1 run emitted %d %s events, want 0", n, legTransformedKind)
	}
	transformed := d7OneEventOfKind(t, arm3Evs, legTransformedKind)
	if transformed.LegType != "crd-order-select" {
		t.Fatalf("leg.transformed LegType = %q, want crd-order-select", transformed.LegType)
	}
	originated := d7OneEventOfKind(t, arm3Evs, "leg.originated")
	if originated.Route == nil || originated.Route.Token != "pa.crd@2.2" || originated.Route.BuildLine != "2.0" {
		t.Fatalf("arm-3 Route = %+v, want Token=pa.crd@2.2 BuildLine=2.0", originated.Route)
	}
	if len(originated.Route.Chain) != 2 {
		t.Fatalf("arm-3 Route.Chain = %+v, want the 2-hop identity chain rendered on the observer seam", originated.Route.Chain)
	}
}

// TestD7CRDIngressRefusalNamesMissingLane is pin (d): a pa.crd@2.2-only peer
// with NO 2.2 validator lane has no honest bridge — arm 2 cannot reach an
// unlaned line and arm 3 cannot land on one — so the ingress refuses with the
// legible RouteRefusalError grammar NAMING the missing lane. The refusal is the
// promoted path's (it carries a BridgeIssue); the legacy backfill's refusal
// never named an ingredient.
func TestD7CRDIngressRefusalNamesMissingLane(t *testing.T) {
	env := newInProcessExchange(t)
	d7SetPeerContractVersions(t, env, "pa.crd@2.2")
	fake := shnsdk.NewFakeValidator()
	env.originator.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": fake} // 2.2 deliberately UNLANED
	evs := d7CaptureEvents(env)

	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, env.crdIngressRequest(t))

	if rec.Code != 422 {
		t.Fatalf("ingress status = %d (%s), want 422 — no lane for 2.2 means no honest bridge", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "no configured validator lane for line 2.2") {
		t.Fatalf("refusal body must NAME the missing lane, got %q", body)
	}
	refused := d7OneEventOfKind(t, *evs, "leg.refused")
	if refused.Route == nil || refused.Route.BridgeIssue == "" {
		t.Fatalf("leg.refused Route = %+v, want the promoted path's structured refusal with a BridgeIssue", refused.Route)
	}
	if n := len(d7EventsOfKind(*evs, "leg.originated")); n != 0 {
		t.Fatalf("got %d leg.originated events on a refusal, want 0 — nothing may reach the Hub", n)
	}
	if env.routeHitCount() != 0 {
		t.Fatalf("route hits = %d, want 0 — a refused leg never reaches the Hub", env.routeHitCount())
	}
}
