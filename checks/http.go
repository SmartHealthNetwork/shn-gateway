package checks

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

// Handler serves GET/POST /internal/checks (spec §7.1). Gate: when token !=
// "", require an exact "Authorization: Bearer <token>" header; when token ==
// "", accept loopback RemoteAddr only — behind any reverse proxy RemoteAddr
// is the proxy's, so an unset token means host-local access only (documented
// in gateway/docs/CONFIGURATION.md). The auth gate is checked before the
// method switch, so an unauthorized caller sees the same 401/403 regardless
// of verb — it never learns which methods the endpoint accepts.
//
// GET returns the newest completed results: 503 {"error":"checks have not
// completed yet"} before the first run ever completes, else 200
// {"results":[...],"checkedAt":"..."}.
//
// POST runs the probes: single-flight (409 {"error":"checks already
// running"} while a run is already in flight) and cooldown-capped (a run
// within 30s of the last completed one returns the cached results, still
// 200) — see Runner.Run.
func Handler(r *Runner, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if ok, code := authorized(req, token); !ok {
			msg := "unauthorized"
			if code == http.StatusForbidden {
				msg = "forbidden"
			}
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		switch req.Method {
		case http.MethodGet:
			handleGet(w, r)
		case http.MethodPost:
			handlePost(w, req, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	})
}

// bearerPrefix is the required "Authorization" header prefix for the
// token-gated mode.
const bearerPrefix = "Bearer "

// authorized applies the token-or-loopback gate described on Handler. code is
// only meaningful when ok is false: 401 for a missing/wrong bearer (token
// set), 403 for a non-loopback or unparseable RemoteAddr (token unset).
//
// The bearer comparison uses crypto/subtle.ConstantTimeCompare — never Go's
// built-in !=/== on the token value — mirroring this package's own cited
// precedent (kitd's /api/verify, kit/kitd/kitd.go's authMiddleware) and
// cmd/testdoor/token.go: a timing side-channel on a bearer-token comparison
// is exactly the kind of thing that IS worth the extra care here, even
// though this is an operator surface rather than a partner-facing one.
func authorized(r *http.Request, token string) (ok bool, code int) {
	if token != "" {
		var presented string
		if got := r.Header.Get("Authorization"); strings.HasPrefix(got, bearerPrefix) {
			presented = strings.TrimPrefix(got, bearerPrefix)
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			return false, http.StatusUnauthorized
		}
		return true, 0
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false, http.StatusForbidden
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false, http.StatusForbidden
	}
	return true, 0
}

// handleGet answers the newest completed results without triggering a run.
func handleGet(w http.ResponseWriter, r *Runner) {
	results, checkedAt, ok := r.Last()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "checks have not completed yet"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "checkedAt": checkedAt})
}

// handlePost runs the probes (or serves the cooldown-cached results), then
// answers the same shape as handleGet. ErrBusy is the only error Run
// returns; anything else is defensive.
func handlePost(w http.ResponseWriter, req *http.Request, r *Runner) {
	if _, err := r.Run(req.Context()); err != nil {
		if errors.Is(err, ErrBusy) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "checks already running"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "checks failed to run"})
		return
	}
	handleGet(w, r) // Run just completed (or served cache): Last() now has fresh results.
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
