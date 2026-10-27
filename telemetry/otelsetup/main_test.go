package otelsetup

import (
	"os"
	"strings"
	"testing"
)

// TestMain empties every OTEL_* setting of the process environment: the
// OpenTelemetry SDK reads it beside the gateway's own reader, so a setting in
// the shell that runs the tests would reach tests that set none.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "OTEL_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
