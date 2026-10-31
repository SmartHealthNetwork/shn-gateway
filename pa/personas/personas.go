// Package personas holds the Prior Authorization workstream's synthetic
// scenario personas: the member each /scenario/* route originates for on each
// lane, their canary twins, the demo lane's order tuples and the bridging-demo
// payer identities. A gateway's scenario routes resolve their members from it,
// and a driver, a seed or a discovery descriptor names the same members and
// orders from it, without importing the gateway engine. Every member, payer and
// order here is synthetic test data.
package personas

import (
	"sort"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Lanes a /scenario/* route resolves its member on (the gateway's three
// scenario member arms).
const (
	// LaneConformance is the default arm: the conformance roster.
	LaneConformance = "conformance"
	// LaneProviderData is ORIGINATION_PROFILE=provider-data.
	LaneProviderData = "provider-data"
	// LaneDemo is ORIGINATION_PROFILE=demo.
	LaneDemo = "demo"
)

// ScenarioPersona is one member a /scenario/* route originates for: the
// scenario, the lane and persona set it resolves on, the member, and the
// product the scenario originates for (zero when the scenario names none: UC-01
// eligibility).
//
// A driver that calls a route names this member in the request body
// ("member") and states its claim facts for this order ("order"); the facts
// themselves are the persona's own statement in the discovery descriptor, never
// the gateway's.
type ScenarioPersona struct {
	// Scenario is the route: the path segment after /scenario/ ("uc04"; the
	// uc06 and uc07 start, complete and full routes all resolve "uc06" or
	// "uc07"), plus the body branch where a route has one: "uc01" and
	// "uc01-notcovered" (/scenario/uc01, {"branch":"covered"|"notcovered"}),
	// "uc03-bridge-demo" and "uc03-line-carry" (/scenario/uc03,
	// {"branch":"bridge-demo"|"line-carry"}), "uc05-noconsent"
	// (/scenario/uc05, {"branch":"noconsent"}). /scenario/dispatch is not here:
	// it takes any member its request names.
	Scenario string
	// Lane is LaneConformance, LaneProviderData or LaneDemo.
	Lane string
	// PersonaSet is "" or "canary" (?personaSet=canary).
	PersonaSet string
	Member     string
	Order      DemoOrderTuple
}

// Members is a scenario's member on each lane.
type Members struct {
	Conformance, ProviderData, Demo string
}

// homeOxygenMember is the seeded provider-data persona (the E0431 HomeOxygen
// order).
const homeOxygenMember = "MBR-OX"

// scenarioMembers is THE table a gateway's scenario routes resolve their
// member from: per scenario, the member on each lane and the product it
// originates for. ScenarioRoster is generated from it, so the roster drivers
// read and the members routes resolve cannot differ.
var scenarioMembers = map[string]struct {
	conformance, providerData, demo string
	order                           func() DemoOrderTuple
}{
	"uc01":            {"MBR-COVERED", "MBR-PD-UC01", "MBR-D-UC01", nil},
	"uc01-notcovered": {"MBR-NOTCOVERED", "MBR-PD-UC01-NC", "MBR-D-UC01-NC", nil},
	"uc02":            {"MBR-COVERED", "MBR-PD-UC02", "MBR-D-UC02", func() DemoOrderTuple { return DemoOrderCodes().UC02 }},
	"uc02-payerb":     {"MBR-PD-UC02-PB", "MBR-PD-UC02-PB", "MBR-PD-UC02-PB", func() DemoOrderTuple { return DemoOrderCodes().UC02 }},
	// The unknown-payer persona's system holds no order (the platform seeder seeds its
	// Patient and Coverage only): its route fails closed on routing, before any
	// order is read, so its statement names none.
	"uc02-unknownpayer": {"MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER", nil},
	"uc03":              {"MBR-COVERED", "MBR-PD-UC03", "MBR-D-UC03", func() DemoOrderTuple { return DemoOrderCodes().UC03 }},
	"uc03-bridge-demo":  {"MBR-BRIDGE-DEMO", "MBR-BRIDGE-DEMO", "MBR-BRIDGE-DEMO", func() DemoOrderTuple { return DemoOrderCodes().UC03Bridge }},
	"uc03-line-carry":   {"MBR-BRIDGE-REFUSE", "MBR-BRIDGE-REFUSE", "MBR-BRIDGE-REFUSE", func() DemoOrderTuple { return DemoOrderCodes().UC03Bridge }},
	"uc04":              {"MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04", func() DemoOrderTuple { return DemoOrderCodes().UC04 }},
	"uc05":              {"MBR-UC05", "MBR-PD-UC05", "MBR-D-UC05", func() DemoOrderTuple { return DemoOrderCodes().UC05 }},
	"uc05-noconsent":    {"MBR-UC05-NOCONSENT", "MBR-PD-UC05-NC", "MBR-D-UC05-NC", func() DemoOrderTuple { return DemoOrderCodes().UC05 }},
	"uc06":              {"MBR-UC06", "MBR-PD-UC06", "MBR-D-UC06", func() DemoOrderTuple { return DemoOrderCodes().UC06 }},
	"uc07":              {"MBR-UC07", "MBR-PD-UC07", "MBR-D-UC07", func() DemoOrderTuple { return DemoOrderCodes().UC07 }},
	"uc07hcpcs":         {"MBR-UC07HCPCS", "MBR-UC07HCPCS", "MBR-UC07HCPCS", func() DemoOrderTuple { return DemoOrderCodes().UC07HCPCS }},
	"uc08":              {"MBR-UC08", "MBR-PD-UC08", "MBR-D-UC08", func() DemoOrderTuple { return DemoOrderCodes().UC08 }},
	"homeoxygen":        {homeOxygenMember, homeOxygenMember, homeOxygenMember, func() DemoOrderTuple { return homeOxygenOrder }},
}

// homeOxygenOrder is the order the home-oxygen route's member holds in its
// system of record: MBR-OX's seeded E0431 DeviceRequest
// (sdk/fixtures/providerdata/homeoxygen.json). The route checks the stated
// order against the member's open order, so a seed that changes product makes
// the route refuse rather than originate under facts stated for another order.
var homeOxygenOrder = DemoOrderTuple{hcpcsSystem, "E0431", "Portable gaseous oxygen system", "J44.9"}

// ScenarioMembers returns the scenario's member on each lane, and whether the
// table holds the scenario.
func ScenarioMembers(scenario string) (Members, bool) {
	m, ok := scenarioMembers[scenario]
	return Members{Conformance: m.conformance, ProviderData: m.providerData, Demo: m.demo}, ok
}

// Scenarios is every scenario the table holds, sorted.
func Scenarios() []string {
	out := make([]string, 0, len(scenarioMembers))
	for s := range scenarioMembers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ScenarioRoster is every (scenario, lane, persona set) a /scenario/* route
// resolves, with its member and product, sorted. A canary row exists wherever
// the lane's member has a canary twin (CanaryTwins).
func ScenarioRoster() []ScenarioPersona {
	var out []ScenarioPersona
	for scenario, m := range scenarioMembers {
		var order DemoOrderTuple
		if m.order != nil {
			order = m.order()
		}
		for _, lm := range []struct{ lane, member string }{
			{LaneConformance, m.conformance}, {LaneProviderData, m.providerData}, {LaneDemo, m.demo},
		} {
			out = append(out, ScenarioPersona{Scenario: scenario, Lane: lm.lane, Member: lm.member, Order: order})
			if twin, ok := CanaryTwins[lm.member]; ok {
				out = append(out, ScenarioPersona{Scenario: scenario, Lane: lm.lane, PersonaSet: "canary", Member: twin, Order: order})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Scenario != b.Scenario {
			return a.Scenario < b.Scenario
		}
		if a.Lane != b.Lane {
			return a.Lane < b.Lane
		}
		return a.PersonaSet < b.PersonaSet
	})
	return out
}

// CanaryTwins maps each scenario member to its dedicated canary twin
// (observability Phase 3, settled decision #1): the monitor's scenario canary
// drives ONLY these members, so continuous canary runs never mutate the shared
// demo personas' state (ledger rows, auth-number overwrites) and canary
// audit records are attributable by PCI. Twins share the original's birthDate
// and take family "<orig>-Canary" — the member id alone makes the PCI distinct.
// The platform seeder mirrors this table (census-pinned by its canary test) and
// seeds the twins into the deployed stack's FHIR tenants.
//
// The table must cover every member a scenario route can resolve ON A LANE A
// CANARY RUNS AGAINST — one roster is not enough. The gateway's scenario member
// resolution has three arms, and which one a deployment lands in is a
// configuration fact, not a build fact: a provider gateway that declares no
// origination profile is the family-keyed demo lane (gateway/app normalizes the
// unset value once, at the config boundary), so its scenario routes resolve the
// MBR-D-UC0N roster and a canary request asks for THOSE members' twins. A member
// missing here does not fall back to the shared persona — the route fails the
// request closed with a 400, which is the correct refusal but a dead canary.
// The engine's TestCanaryTwins_CoverEveryLaneScenarioMember derives the
// required set from the scenario table the routes resolve from, so this table
// cannot fall behind the members the handlers actually resolve.
var CanaryTwins = map[string]string{
	// Conformance roster — the default arm, plus MBR-UC07HCPCS, whose
	// scenario resolves the same member on every arm.
	"MBR-COVERED":        "MBR-CANARY-COVERED",
	"MBR-NOTCOVERED":     "MBR-CANARY-NOTCOVERED",
	"MBR-UC04":           "MBR-CANARY-UC04",
	"MBR-UC05":           "MBR-CANARY-UC05",
	"MBR-UC05-NOCONSENT": "MBR-CANARY-UC05-NOCONSENT",
	"MBR-UC06":           "MBR-CANARY-UC06",
	"MBR-UC07":           "MBR-CANARY-UC07",
	"MBR-UC07HCPCS":      "MBR-CANARY-UC07HCPCS",
	"MBR-UC08":           "MBR-CANARY-UC08",

	// Demo roster — the demo arm, which is the lane every deployed provider
	// gateway that declares no origination profile runs, and therefore the
	// lane the deployed canary actually drives.
	"MBR-D-UC01":    "MBR-CANARY-D-UC01",
	"MBR-D-UC01-NC": "MBR-CANARY-D-UC01-NC",
	"MBR-D-UC02":    "MBR-CANARY-D-UC02",
	"MBR-D-UC03":    "MBR-CANARY-D-UC03",
	"MBR-D-UC04":    "MBR-CANARY-D-UC04",
	"MBR-D-UC05":    "MBR-CANARY-D-UC05",
	"MBR-D-UC05-NC": "MBR-CANARY-D-UC05-NC",
	"MBR-D-UC06":    "MBR-CANARY-D-UC06",
	"MBR-D-UC07":    "MBR-CANARY-D-UC07",
	"MBR-D-UC08":    "MBR-CANARY-D-UC08",
}

// bridgeDemoPayerSystem namespaces the bridging-demo payer identities distinctly
// from the shared NAIC-style payer-id OID (urn:oid:2.16.840.1.113883.6.300)
// every other holder identity claims a numeric code under (00001/00078/00099/…).
// The bridge-demo payers are a visualization fixture, not a real-world-registered payer, so
// they get their own obviously-synthetic namespace rather than squatting on the next unclaimed
// NAIC-style number.
const bridgeDemoPayerSystem = "urn:shn:demo-payer"

// BridgeDemoPayerID / BridgeRefusePayerID are the payer identities the bridging-demo
// members' Coverage.payor names: coverage-derived routing (FeedPayerRouter) sends their
// legs to the bridge-demo-payer / bridge-demo-refuse holders rather than the CMS
// conformance payer. They are identities, not persona content, exported so
// the platform seeder and the SHN Kit surfaces (runner branches, the live bridging
// fixture) name the exact identity instead of duplicating the literal. Plain
// shnsdk.PayerIdentifier values, following the sdk/payer.go CMSPayerIdentity idiom.
var (
	BridgeDemoPayerID   = shnsdk.PayerIdentifier{System: bridgeDemoPayerSystem, Value: "SHN-BRIDGE-DEMO"}
	BridgeRefusePayerID = shnsdk.PayerIdentifier{System: bridgeDemoPayerSystem, Value: "SHN-BRIDGE-REFUSE"}
)
