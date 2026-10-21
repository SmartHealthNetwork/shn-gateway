package engine

import (
	"bytes"
	"io"
	"log"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ReplayScopeScenarioJTI is the one-time-use record of the /scenario callers'
// assertions, per caller label, kept until each assertion expires.
const ReplayScopeScenarioJTI = "scenario-jti"

// errUnauthorizedCaller is the one answer every refused /scenario caller
// gets: it says no more about why.
const errUnauthorizedCaller = "unauthorized caller"

// requireScenarioCaller admits a request to an operator route only from a
// caller this gateway admits (Config.ScenarioCallers): its assertion is
// verified (scenarioauth.Verify) for this gateway's own holder id
// (Config.HolderID) before the route runs, and its jti spent once. A refused
// request reaches nothing, reads nothing and sends nothing. A one-time-use
// record that cannot be consulted refuses with 503, never admits.
//
// Until every stack configures its callers, a gateway with no
// Config.ScenarioCallers at all (nil) leaves its operator routes
// unauthenticated, as they were: a temporary default that becomes refuse.
// An empty set admits nothing.
func (g *Gateway) requireScenarioCaller(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callers := g.cfg.ScenarioCallers
		if callers == nil {
			h(w, r)
			return
		}
		header := r.Header.Get(scenarioauth.HeaderName)
		if header == "" {
			// Refused before the body is read: nothing to verify it against.
			refuseScenarioCaller(w, r, "no assertion")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes+1))
		if err != nil || len(body) > shnsdk.MaxRequestBytes {
			refuseScenarioCaller(w, r, "the body could not be read whole")
			return
		}
		now := g.cfg.Clock()
		c, err := scenarioauth.Verify(header, r.Method, r.URL.RequestURI(), body, g.cfg.HolderID, callers, now)
		if err != nil {
			refuseScenarioCaller(w, r, err.Error())
			return
		}
		if len(c.JTI) > MaxReplayKeyBytes {
			refuseScenarioCaller(w, r, "jti too long")
			return
		}
		// Spent last, once the request is otherwise admitted, so a refused
		// request does not burn its caller's assertion.
		replayed, err := g.replay.CheckAndRecord(ReplayScopeScenarioJTI, c.Label, c.JTI, now, c.Expiry)
		if err != nil {
			log.Printf("gateway: /scenario caller one-time-use record unavailable: %v (refusing)", err)
			g.noteStoreError(storeErrReplay)
			writeUnavailable(w, "the one-time-use record of /scenario callers is unavailable")
			return
		}
		if replayed {
			refuseScenarioCaller(w, r, "assertion already used")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		h(w, r)
	}
}

// refuseScenarioCaller answers 401, the same body for every reason, and logs
// the route and the reason (never the assertion).
func refuseScenarioCaller(w http.ResponseWriter, r *http.Request, why string) {
	log.Printf("gateway: refused a /scenario caller: %s %s: %s", r.Method, r.URL.Path, why)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": errUnauthorizedCaller})
}
