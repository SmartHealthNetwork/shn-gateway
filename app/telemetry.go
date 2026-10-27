//go:build !shn_coreonly

package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/otelsetup"
)

// otelSettingNames are the OTEL_* names set (non-empty) in the process
// environment. They only name, in a boot warning, the OTEL_* settings the
// gateway does not read; no value is read here, every setting the
// gateway acts on is read through getenv.
func otelSettingNames() []string {
	var names []string
	for _, kv := range os.Environ() {
		if name, v, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "OTEL_") && v != "" {
			names = append(names, name)
		}
	}
	return names
}

// telemetryCheckTargets are the monitoring checks (MON-R13, MON-R44), each
// advisory (a failure never counts against the gateway's readiness) and read
// from the gateway's own state, never by contacting the vendor:
// telemetry-config when any monitoring setting is set, and telemetry-export
// when an OTLP exporter is on. With no monitoring setting there are none, so
// /internal/checks is as before (MON-R1).
func telemetryCheckTargets(tel *otelsetup.Telemetry, endpoint string) []checks.Target {
	var out []checks.Target
	if tel.ConfigCheck().Present {
		out = append(out, checks.Target{ID: "telemetry-config", Kind: checks.KindLocal, Advisory: true, Local: func() (bool, string, *checks.Failure) {
			return configCheck(tel.ConfigCheck())
		}})
	}
	if tel.ExportCheck().Present {
		out = append(out, checks.Target{ID: "telemetry-export", Kind: checks.KindLocal, URL: endpoint, Advisory: true, Local: func() (bool, string, *checks.Failure) {
			return exportCheck(tel.ExportCheck())
		}})
	}
	return out
}

// configCheck reports the monitoring settings that are not valid, and
// whether one of them turned monitoring off or each fell back while export
// continues (MON-R44); never a value.
func configCheck(st otelsetup.ConfigState) (bool, string, *checks.Failure) {
	if st.OK {
		return true, "valid", nil
	}
	names := strings.Join(st.Settings, ", ")
	return false, strings.Join(st.Summaries, "; "), &checks.Failure{Code: checks.FailInvalidTelemetryConfiguration, Hint: names}
}

// exportCheck reports the newest export's cached outcome (MON-R13): its
// failure code is one of the existing codes the export maps to, and its hint
// a status, never an error's text.
func exportCheck(st otelsetup.ExportState) (bool, string, *checks.Failure) {
	switch {
	case !st.Exported:
		return false, "no export has run yet", &checks.Failure{Code: checks.FailNotChecked}
	case st.OK:
		return true, "the newest export was accepted", nil
	}
	detail := "the newest export failed (" + st.Code
	if st.Hint != "" {
		detail += ", " + st.Hint
	}
	return false, detail + fmt.Sprintf("); %d in a row", st.Failures), &checks.Failure{Code: st.Code, Hint: st.Hint}
}
