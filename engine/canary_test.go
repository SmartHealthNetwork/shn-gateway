package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestCanaryTwins_CoverEveryConformanceScenarioMember pins that every member the
// 11 conformance-roster checks can resolve has a twin — a scenario added without a twin
// must fail this census, not 400 at 2am in the canary.
func TestCanaryTwins_CoverEveryConformanceScenarioMember(t *testing.T) {
	for _, m := range []string{"MBR-COVERED", "MBR-NOTCOVERED", "MBR-UC04", "MBR-UC05",
		"MBR-UC05-NOCONSENT", "MBR-UC06", "MBR-UC07", "MBR-UC07HCPCS", "MBR-UC08"} {
		if _, ok := CanaryTwins[m]; !ok {
			t.Errorf("no canary twin for %s", m)
		}
	}
}

// sceneArms names the three sceneMember arms, so a failure says WHICH lane
// resolves the twin-less member.
var sceneArms = [3]string{"default", "provider-data", "demo"}

// canaryTwinlessMembers are the scenario members that deliberately carry NO canary
// twin, each with the reason it is exempt. scenarioMember's fail-closed 400 is the
// correct answer for these — the point of the guard below is that everything NOT
// listed here must have a twin, so a roster the handlers resolve can never quietly
// drift away from the roster the canary can drive.
//
// A stale row is a failure too (checked below): an exemption that no longer matches
// any call site is deleted, not carried.
var canaryTwinlessMembers = map[string]string{
	"MBR-OX": "the order-dispatch routing proof; no continuous check drives it, and " +
		"its order is read from a seeded system of record keyed on the member id",
	"MBR-BRIDGE-REFUSE": "a visualization-only persona whose whole purpose is a refused leg — " +
		"a canary twin for it would be an abuse of a mechanism that exists to keep " +
		"continuous checks off the shared scenario personas",
	"MBR-BRIDGE-DEMO": "the bridging-demo SUCCESS persona (task2 brief A3a) — same exemption " +
		"as MBR-BRIDGE-REFUSE: a canary twin for a demo-only visualization persona would be " +
		"an abuse of a mechanism that exists to keep continuous checks off the shared " +
		"scenario personas; scenariodrive.BridgeChecks() drives it directly with " +
		"PinnedPersonas instead",
	"MBR-UNKNOWN-PAYER": "the deliberately unroutable member behind the no-registered-payer " +
		"fail-closed proof; a canary run of it would assert nothing",
}

// canaryTwinlessPrefix exempts the whole provider-data roster for ONE structural
// reason: that lane originates headlessly off orders seeded into the provider's own
// FHIR store from published data bundles, keyed on the member id. A twin member would
// need its own seeded order, and no published bundle carries one — so the lane has no
// canary at all, rather than a canary with missing twins.
const canaryTwinlessPrefix = "MBR-PD-"

// scenarioMemberLiterals returns, for every scenario the /scenario/* routes
// resolve (scenarioMembers, the one table scenarioMember reads), the member on
// each of the three arms.
func scenarioMemberLiterals(t *testing.T) map[string][]string {
	t.Helper()
	byMember := map[string][]string{}
	for _, scenario := range sortedKeys(scenarioMembers) {
		m := scenarioMembers[scenario]
		for i, member := range [3]string{m.conformance, m.providerData, m.demo} {
			byMember[member] = appendUnique(byMember[member], sceneArms[i])
		}
	}
	if len(byMember) == 0 {
		t.Fatal("scenarioMembers resolves no member")
	}
	return byMember
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// TestCanaryTwins_CoverEveryLaneScenarioMember is the hermetic guard that keeps
// CanaryTwins from falling behind the members the handlers actually resolve.
//
// The census above pins ONE roster by hand, which is how the demo roster came to be
// missing: the handlers were re-based onto MBR-D-UC0N members, the twin table was
// not, and every canary request against a gateway on that lane failed closed with
// "no canary twin for member MBR-D-UC0N". Nothing hermetic caught it — the test that
// would have needs Postgres and skips by default, and the deployment gate never
// drives a persona set at all.
//
// So this guard derives the required set from the scenarioMember call sites
// themselves, in BOTH directions: every member a call site can resolve needs a twin
// (unless it is exempt above, with a reason), and every member the table carries must
// be one some call site actually resolves. It reads source text rather than driving
// the handlers because the members are literals at the call sites, and a literal is
// exactly what a lane re-base changes.
func TestCanaryTwins_CoverEveryLaneScenarioMember(t *testing.T) {
	byMember := scenarioMemberLiterals(t)

	// Direction 1 — a member the handlers resolve, with no twin, is a lane whose
	// canary is dead.
	for _, member := range sortedKeys(byMember) {
		if _, ok := CanaryTwins[member]; ok {
			continue
		}
		if _, exempt := canaryTwinlessMembers[member]; exempt {
			continue
		}
		if strings.HasPrefix(member, canaryTwinlessPrefix) {
			continue
		}
		t.Errorf("scenarioMember resolves %s on the %s arm(s) but CanaryTwins has no twin for it — "+
			"every canary request for that scenario fails closed 400 %q. Add the twin (and seed it "+
			"substrate-side), or add %s to canaryTwinlessMembers with the reason it needs none.",
			member, strings.Join(byMember[member], "+"), "no canary twin for member "+member, member)
	}

	// Direction 2 — a twin for a member nothing resolves is dead weight that reads
	// like coverage.
	for _, member := range sortedKeys(CanaryTwins) {
		if _, ok := byMember[member]; !ok {
			t.Errorf("CanaryTwins carries a twin for %s, but no scenarioMember call site resolves that member — "+
				"delete the row or fix the member id", member)
		}
	}

	// Direction 3 — a stale exemption. An exemption that covers nothing is a claim
	// about the code that has stopped being true.
	for _, member := range sortedKeys(canaryTwinlessMembers) {
		if _, ok := byMember[member]; !ok {
			t.Errorf("canaryTwinlessMembers exempts %s, but no scenarioMember call site resolves it — delete the row", member)
		}
		if _, ok := CanaryTwins[member]; ok {
			t.Errorf("%s is exempted from needing a canary twin AND has one — the two tables disagree", member)
		}
	}
	prefixHits := 0
	for member := range byMember {
		if strings.HasPrefix(member, canaryTwinlessPrefix) {
			prefixHits++
		}
	}
	if prefixHits == 0 {
		t.Errorf("canaryTwinlessPrefix %q exempts nothing any more — delete it", canaryTwinlessPrefix)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestScenarioMember_DemoArmResolvesTwins is the runtime half of the guard above: the
// static scan proves the TABLE covers the demo arm's members, this proves the SEAM
// hands back a twin for them rather than the shared persona. One row per demo-roster
// member, driven through the real sceneMember/scenarioMember pair on a demo-profile
// gateway with a canary persona set.
func TestScenarioMember_DemoArmResolvesTwins(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "demo"}}

	for _, scenario := range sortedKeys(scenarioMembers) {
		member := scenarioMembers[scenario].demo
		want, ok := CanaryTwins[member]
		if !ok {
			continue // direction 1 above already reported it
		}
		r := httptest.NewRequest(http.MethodPost, "/scenario/x?personaSet=canary",
			strings.NewReader(testClaimBody(`"member":"`+want+`"`)))
		w := httptest.NewRecorder()
		got, ok := g.scenarioMember(w, r, scenario)
		if !ok {
			t.Errorf("%s under personaSet=canary: failed closed %d %s", member, w.Code, strings.TrimSpace(w.Body.String()))
			continue
		}
		if got != want {
			t.Errorf("%s under personaSet=canary resolved %q, want the twin %q", member, got, want)
		}
		if got == member {
			t.Errorf("%s under personaSet=canary resolved the SHARED persona — the twins exist to prevent exactly this", member)
		}
	}
}

// TestCensusPersonas_CanaryClones: every twin resolves in censusPersonas with the
// original's coverage/clinical facts and a distinct family name (distinct PCI).
func TestCensusPersonas_CanaryClones(t *testing.T) {
	for orig, twin := range CanaryTwins {
		o, ok := censusPersonas[orig]
		if !ok {
			t.Fatalf("original %s missing", orig)
		}
		c, ok := censusPersonas[twin]
		if !ok {
			t.Fatalf("twin %s missing", twin)
		}
		if c.inforce != o.inforce || c.hasClinical != o.hasClinical {
			t.Errorf("%s: coverage/clinical facts diverge from %s", twin, orig)
		}
		if c.demo.FamilyName != o.demo.FamilyName+"-Canary" {
			t.Errorf("%s family = %q", twin, c.demo.FamilyName)
		}
	}
}

// Rejection rows (valid request − one mutation → reject; every guard ships its
// rejection test): unknown personaSet and twin-less member must 400, never
// silently fall through to the shared demo personas.
func TestScenario_PersonaSetRejections(t *testing.T) {
	g := newTestProviderGateway(t) // reuse the package's existing constructor helper
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("/scenario/uc03?personaSet=bogus", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown personaSet: got %d, want 400", resp.StatusCode)
	}
	// homeoxygen's member literal has no canary twin → canary must fail closed.
	if resp := post("/scenario/homeoxygen?personaSet=canary", `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("twin-less member under canary: got %d, want 400", resp.StatusCode)
	}
	// control: personaSet absent keeps working (any status but 400-personaSet).
	resp := post("/scenario/uc03", testRouteBody(g, "/scenario/uc03", ""))
	if resp.StatusCode == http.StatusBadRequest {
		t.Fatalf("control uc03 without personaSet unexpectedly 400")
	}
}

// TestScenario_UC03BridgeBranches pins the sanctioned handleUC03 branch switch for BOTH
// bridge arms ("bridge-demo" and "line-carry"): "" (or an absent body) keeps working
// exactly like before; "bridge-demo"/"line-carry" are each a KNOWN branch (never 400 on
// the branch
// itself); an unrecognized branch 400s (uc01's idiom); and ?personaSet=canary combined
// with either bridge branch fails closed 400 naming the branch's own member —
// deliberately NOT via a new CanaryTwins entry (the reviewer's ruling: a canary twin for
// a demo-only persona would be semantic abuse of the mechanism) but because
// scenarioMember's existing no-twin guard already fails closed for any member without
// one, and bridge personas never get one.
func TestScenario_UC03BridgeBranches(t *testing.T) {
	g := newTestProviderGateway(t)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := post("/scenario/uc03", `{"branch":"bogus"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown branch: got %d, want 400", resp.StatusCode)
	}

	for _, tc := range []struct{ branch, member string }{
		{"bridge-demo", "MBR-BRIDGE-DEMO"},
		{"line-carry", "MBR-BRIDGE-REFUSE"},
	} {
		// The branch is KNOWN: it must not 400 on the branch check itself (this
		// fixture has no PayerRouter at all, so it 422s downstream for every member —
		// the point here is ruling OUT a 400 from the switch).
		if resp := post("/scenario/uc03", testRouteBody(g, "/scenario/uc03", `"branch":"`+tc.branch+`"`)); resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("known branch %s unexpectedly 400: %s", tc.branch, body)
		}
		// canary + the bridge branch: scenarioMember's existing no-twin guard fires
		// (CanaryTwins deliberately carries no entry for either bridge persona).
		resp := post("/scenario/uc03?personaSet=canary", `{"branch":"`+tc.branch+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("canary + %s: got %d, want 400 (no twin)", tc.branch, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), tc.member) {
			t.Errorf("canary + %s body = %s, want it to name %s", tc.branch, body, tc.member)
		}
	}
}

// newTestProviderGateway builds the minimal provider-role Gateway the package's other
// handler tests construct (copied from TestHandler_HomeOxygenRouteRegistered in
// originate_homeoxygen_test.go — no newTestProviderGateway-style helper existed yet).
func newTestProviderGateway(t *testing.T) *Gateway {
	t.Helper()
	_, signPriv := genED25519(t)
	encPub, encPriv := genKeyPair(t)
	stub := newCensusSoR()
	return mustNew(t, Config{
		Role:     "provider",
		HolderID: "provider",
		Identity: shnsdk.Identity{
			HolderID: "provider",
			SignPriv: signPriv,
			EncPub:   encPub,
			EncPriv:  encPriv,
		},
		SoR:                stub,
		Store:              stub,
		Validator:          shnsdk.NewFakeValidator(),
		OriginationProfile: "demo",
	})
}
