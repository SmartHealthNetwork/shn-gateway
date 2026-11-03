//go:build !shn_coreonly

package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/pa/personas"
)

// scenarioMember resolves the effective member for a /scenario/* request from
// the personas' scenario table (personas.ScenarioMembers): the lane's member,
// remapped to its dedicated canary twin when the request carries
// ?personaSet=canary (observability Phase 3, settled decision #1 — the monitor
// canary must never drive the shared demo personas). Fails closed 400 on an
// unknown personaSet value or a member with no twin: a typo silently running
// the demo personas is exactly the collision the twins exist to prevent.
//
// The member the request names ("member" in its body) selects nothing here:
// the claim facts a request states are its persona's statement, so the member
// it names is checked against this one at the build of the leg that consumes
// those facts (statedMemberFault, at the PAS claim), and not at all on a route
// that consumes none. ok=false means the response is already written.
func (g *Gateway) scenarioMember(w http.ResponseWriter, r *http.Request, scenario string) (string, bool) {
	m, known := personas.ScenarioMembers(scenario)
	if !known {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unknown scenario " + scenario})
		return "", false
	}
	member := g.sceneMember(m.Conformance, m.ProviderData, m.Demo)
	switch ps := r.URL.Query().Get("personaSet"); ps {
	case "":
	case "canary":
		twin, ok := personas.CanaryTwins[member]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no canary twin for member " + member})
			return "", false
		}
		member = twin
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown personaSet " + ps})
		return "", false
	}
	return member, true
}
