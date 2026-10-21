package engine

import (
	"net/http"
	"sort"
)

// Lanes a /scenario/* route resolves its member on (sceneMember's three arms).
const (
	// LaneConformance is sceneMember's default arm: the conformance roster.
	LaneConformance = "conformance"
	// LaneProviderData is ORIGINATION_PROFILE=provider-data.
	LaneProviderData = "provider-data"
	// LaneDemo is ORIGINATION_PROFILE=demo, the lane a provider gateway that
	// declares no origination profile runs.
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

// scenarioMembers is THE table scenarioMember resolves from: per scenario, the
// member on each lane and the product it originates for. ScenarioRoster is
// generated from it, so the roster drivers read and the members routes resolve
// cannot differ.
var scenarioMembers = map[string]struct {
	conformance, providerData, demo string
	order                           func() orderTuple
}{
	"uc01":            {"MBR-COVERED", "MBR-PD-UC01", "MBR-D-UC01", nil},
	"uc01-notcovered": {"MBR-NOTCOVERED", "MBR-PD-UC01-NC", "MBR-D-UC01-NC", nil},
	"uc02":            {"MBR-COVERED", "MBR-PD-UC02", "MBR-D-UC02", func() orderTuple { return originationCodes().uc02 }},
	"uc02-payerb":     {"MBR-PD-UC02-PB", "MBR-PD-UC02-PB", "MBR-PD-UC02-PB", func() orderTuple { return originationCodes().uc02 }},
	// The unknown-payer persona's system holds no order (cmd/fhirseed seeds its
	// Patient and Coverage only): its route fails closed on routing, before any
	// order is read, so its statement names none.
	"uc02-unknownpayer": {"MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER", "MBR-UNKNOWN-PAYER", nil},
	"uc03":              {"MBR-COVERED", "MBR-PD-UC03", "MBR-D-UC03", func() orderTuple { return originationCodes().uc03 }},
	"uc03-bridge-demo":  {"MBR-BRIDGE-DEMO", "MBR-BRIDGE-DEMO", "MBR-BRIDGE-DEMO", func() orderTuple { return uc03BridgeCode }},
	"uc03-line-carry":   {"MBR-BRIDGE-REFUSE", "MBR-BRIDGE-REFUSE", "MBR-BRIDGE-REFUSE", func() orderTuple { return uc03BridgeCode }},
	"uc04":              {"MBR-UC04", "MBR-PD-UC04", "MBR-D-UC04", func() orderTuple { return originationCodes().uc04 }},
	"uc05":              {"MBR-UC05", "MBR-PD-UC05", "MBR-D-UC05", func() orderTuple { return originationCodes().uc05 }},
	"uc05-noconsent":    {"MBR-UC05-NOCONSENT", "MBR-PD-UC05-NC", "MBR-D-UC05-NC", func() orderTuple { return originationCodes().uc05 }},
	"uc06":              {"MBR-UC06", "MBR-PD-UC06", "MBR-D-UC06", func() orderTuple { return originationCodes().uc06 }},
	"uc07":              {"MBR-UC07", "MBR-PD-UC07", "MBR-D-UC07", func() orderTuple { return originationCodes().uc07 }},
	"uc07hcpcs":         {"MBR-UC07HCPCS", "MBR-UC07HCPCS", "MBR-UC07HCPCS", func() orderTuple { return originationCodes().uc07hcpcs }},
	"uc08":              {"MBR-UC08", "MBR-PD-UC08", "MBR-D-UC08", func() orderTuple { return originationCodes().uc08 }},
	"homeoxygen":        {homeOxygenMember, homeOxygenMember, homeOxygenMember, func() orderTuple { return homeOxygenOrder }},
}

// ScenarioRoster is every (scenario, lane, persona set) a /scenario/* route
// resolves, with its member and product, sorted. A canary row exists wherever
// the lane's member has a canary twin (CanaryTwins).
func ScenarioRoster() []ScenarioPersona {
	var out []ScenarioPersona
	for scenario, m := range scenarioMembers {
		var order DemoOrderTuple
		if m.order != nil {
			o := m.order()
			order = DemoOrderTuple{System: o.system, Code: o.code, Display: o.display, Dx: o.dx}
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

// scenarioMember resolves the effective member for a /scenario/* request from
// scenarioMembers: the lane's member, remapped to its dedicated canary twin when
// the request carries ?personaSet=canary (observability Phase 3, settled
// decision #1 — the monitor canary must never drive the shared demo personas).
// Fails closed 400 on an unknown personaSet value or a member with no twin: a
// typo silently running the demo personas is exactly the collision the twins
// exist to prevent.
//
// The request must name that member ("member" in its body): the claim facts a
// request states are its persona's statement, so a request naming another
// member — or none — is refused before anything is read or sent
// (requestNamesMember). ok=false means the response is already written.
func (g *Gateway) scenarioMember(w http.ResponseWriter, r *http.Request, scenario string) (string, bool) {
	m, known := scenarioMembers[scenario]
	if !known {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unknown scenario " + scenario})
		return "", false
	}
	member := g.sceneMember(m.conformance, m.providerData, m.demo)
	switch ps := r.URL.Query().Get("personaSet"); ps {
	case "":
	case "canary":
		twin, ok := CanaryTwins[member]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no canary twin for member " + member})
			return "", false
		}
		member = twin
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown personaSet " + ps})
		return "", false
	}
	if !requestNamesMember(w, r, member) {
		return "", false
	}
	return member, true
}

// homeOxygenOrder is the order the home-oxygen route's member holds in its
// system of record: MBR-OX's seeded E0431 DeviceRequest
// (sdk/fixtures/providerdata/homeoxygen.json). The route checks the stated
// order against the member's open order, so a seed that changes product makes
// the route refuse rather than originate under facts stated for another order.
var homeOxygenOrder = orderTuple{systemHCPCSBuild, "E0431", "Portable gaseous oxygen system", "J44.9"}
