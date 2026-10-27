//go:build !shn_coreonly

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry/stats"
)

// MON-R30: /internal/stats is on in every gateway, with no setting, behind
// exactly /internal/checks' access rule, and reports the exchanges the
// gateway answered, named for its replica.
func TestInternalStats_MON_R30(t *testing.T) {
	get := func(b built, remote, bearer string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/internal/stats", nil)
		req.RemoteAddr = remote
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	t.Run("loopback only without a token", func(t *testing.T) {
		b, _ := buildMonitored(t, monitoringGateway(t, nil))
		if code, body := get(b, "192.0.2.10:4000", ""); code != http.StatusForbidden || !strings.Contains(body, `"forbidden"`) {
			t.Fatalf("a remote caller got %d %s", code, body)
		}
		// One exchange the gateway answers: a leg without the Hub's assertion.
		w := httptest.NewRecorder()
		b.handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/substrate/inbound", strings.NewReader("{}")))
		code, body := get(b, "127.0.0.1:4000", "")
		if code != http.StatusOK {
			t.Fatalf("loopback got %d %s", code, body)
		}
		var r stats.Report
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		if r.Replica == "" || r.Replica != b.telemetry.ServiceInstanceID() {
			t.Fatalf("replica %q, want the gateway's service.instance.id %q", r.Replica, b.telemetry.ServiceInstanceID())
		}
		total := uint64(0)
		for _, w := range r.Windows {
			if w.Window == "1m" {
				for _, e := range w.Exchanges {
					total += e.Count
				}
			}
		}
		if total != 1 {
			t.Fatalf("1m window counts %d exchanges, want the one answered: %s", total, body)
		}
	})
	t.Run("the checks token when set", func(t *testing.T) {
		b, _ := buildMonitored(t, monitoringGateway(t, map[string]string{"CHECKS_TOKEN": "stats-token"}))
		if code, _ := get(b, "127.0.0.1:4000", ""); code != http.StatusUnauthorized {
			t.Fatalf("no bearer got %d, want 401 even from loopback", code)
		}
		if code, _ := get(b, "192.0.2.10:4000", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("a wrong bearer got %d", code)
		}
		if code, _ := get(b, "192.0.2.10:4000", "stats-token"); code != http.StatusOK {
			t.Fatalf("the token got %d", code)
		}
	})
}
