package otelsetup

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient/redirecttest"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// The OTLP HTTP exporter follows no redirect (serviceclient): a collector
// that answers with one fails the export as a failing status (http-status,
// its hint naming the status and redirect-refused), never as unreachable;
// the other origin and path receive nothing, the configured headers never
// leave for them, and the Location's path and query appear in no state or
// log line.
func TestExport_FollowsNoRedirect(t *testing.T) {
	for _, c := range redirecttest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			f := redirecttest.Serve(t, c)
			extra := map[string]string{"OTEL_METRIC_EXPORT_TIMEOUT": "700", "OTEL_EXPORTER_OTLP_TIMEOUT": "700"}
			if c.TLS() {
				extra["OTEL_EXPORTER_OTLP_CERTIFICATE"] = trustFile(t, f)
			}
			tel, _, l := build(t, ProtocolHTTP, f.URL+"/v1/metrics", extra)
			tel.exp.reg.Store(telemetry.New(outcomeSink{t: t, want: "failed"}))
			exportOnce(tel)
			f.NotFollowed(t)
			st := tel.ExportCheck()
			hint := fmt.Sprintf("HTTP %d, redirect-refused", c.Code)
			if st.OK || st.Code != CodeHTTPStatus || st.Hint != hint {
				t.Fatalf("state %+v, want failed %s %q", st, CodeHTTPStatus, hint)
			}
			redirecttest.NoSecret(t, "the check", fmt.Sprintf("%+v", st))
			for _, line := range l.all() {
				redirecttest.NoSecret(t, "a log line", line)
				if strings.Contains(line, secretToken) {
					t.Fatalf("a log line names the export header: %q", line)
				}
			}
		})
	}
}

// trustFile writes the fixture's TLS certificate as a PEM bundle the
// exporter's trust setting reads.
func trustFile(t *testing.T, f *redirecttest.Fixture) string {
	t.Helper()
	cert := f.Certificate()
	if cert == nil {
		t.Fatal("the fixture serves no certificate")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
