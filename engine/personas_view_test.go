package engine

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/personas"
)

// scenarioMemberRow is one scenario's member on each lane.
type scenarioMemberRow struct{ conformance, providerData, demo string }

// scenarioMembers is the personas' scenario table (personas.ScenarioMembers),
// the one table scenarioMember resolves from, as the engine's rows read it.
var scenarioMembers = func() map[string]scenarioMemberRow {
	out := map[string]scenarioMemberRow{}
	for _, s := range personas.Scenarios() {
		m, _ := personas.ScenarioMembers(s)
		out[s] = scenarioMemberRow{m.Conformance, m.ProviderData, m.Demo}
	}
	return out
}()

// Every persona order is coded in the system module 1 builds its orders in,
// so a persona's stated order is the order module 1 originates.
func TestPersonaOrdersAreCodedInTheBuildSystem(t *testing.T) {
	c := personas.DemoOrderCodes()
	for name, o := range map[string]personas.DemoOrderTuple{
		"UC02": c.UC02, "UC03": c.UC03, "UC04": c.UC04, "UC05": c.UC05, "UC06": c.UC06,
		"UC07": c.UC07, "UC07HCPCS": c.UC07HCPCS, "UC08": c.UC08, "UC03Bridge": c.UC03Bridge,
	} {
		if o.System != systemHCPCSBuild {
			t.Errorf("%s is coded in %q, want %q", name, o.System, systemHCPCSBuild)
		}
	}
	for _, p := range personas.ScenarioRoster() {
		if p.Order.Code != "" && p.Order.System != systemHCPCSBuild {
			t.Errorf("scenario %s's order is coded in %q, want %q", p.Scenario, p.Order.System, systemHCPCSBuild)
		}
	}
}
