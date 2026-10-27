package otelsetup

import (
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// processScope is the instrumentation scope of the process.* instruments.
const processScope = "github.com/SmartHealthNetwork/shn-gateway/telemetry/otelsetup/process"

// RuntimeScopes are the instrumentation scopes of the Go runtime and process
// instruments: OpenTelemetry only, outside the catalogue, with values that
// change from run to run.
var RuntimeScopes = []string{runtime.ScopeName, processScope}

// RuntimeMetric is one runtime or process instrument as it is exported: its
// unit, and each attribute's closed list of values.
type RuntimeMetric struct {
	Unit       string
	Attributes map[string][]string
}

// RuntimeMetrics are every instrument the runtime and process scopes
// (RuntimeScopes) export, by name, and nothing else
// (TestRuntimeMetricsAreTheList): the closed set the catalogue's checks
// accept outside the catalogue.
var RuntimeMetrics = map[string]RuntimeMetric{
	"go.memory.used":        {Unit: "By", Attributes: map[string][]string{"go.memory.type": {"stack", "other"}}},
	"go.memory.limit":       {Unit: "By"},
	"go.memory.allocated":   {Unit: "By"},
	"go.memory.allocations": {Unit: "{allocation}"},
	"go.memory.gc.goal":     {Unit: "By"},
	"go.goroutine.count":    {Unit: "{goroutine}"},
	"go.processor.limit":    {Unit: "{thread}"},
	"go.config.gogc":        {Unit: "%"},
	"process.cpu.time":      {Unit: "s", Attributes: map[string][]string{"cpu.mode": {"user", "system"}}},
	"process.memory.usage":  {Unit: "By"},
	"process.uptime":        {Unit: "s"},
}

// ConditionalRuntimeMetrics are the runtime and process instruments present
// only on some platforms or settings: process.memory.usage where
// /proc/self/statm can be read, go.memory.limit where a memory limit is set.
var ConditionalRuntimeMetrics = []string{"process.memory.usage", "go.memory.limit"}

// deprecatedRuntimeEnv makes the runtime instrumentation add its deprecated
// runtime.go.* instruments as well. The library reads it itself, so the
// gateway cannot stop it being read; the gateway's view drops what it
// adds from being exported.
const deprecatedRuntimeEnv = "OTEL_GO_X_DEPRECATED_RUNTIME_METRICS"

// deprecatedRuntime reports an instrument deprecatedRuntimeEnv adds
// (process.runtime.go.* and runtime.uptime, in the runtime instrumentation's
// scope). The gateway's view drops each, so the exported names stay the
// catalogue's and the semantic conventions' whatever it says.
func deprecatedRuntime(i sdkmetric.Instrument) bool {
	return i.Scope.Name == runtime.ScopeName && (strings.HasPrefix(i.Name, "process.runtime.go.") || i.Name == "runtime.uptime")
}

// runtimeSeriesBound is the most series one runtime or process instrument
// records, and runtimeKinds the SDK kinds they are created as: a kind the
// catalogue also uses has its limit raised to fit them.
const runtimeSeriesBound = 2

var runtimeKinds = []sdkmetric.InstrumentKind{
	sdkmetric.InstrumentKindObservableCounter, sdkmetric.InstrumentKindObservableUpDownCounter, sdkmetric.InstrumentKindObservableGauge,
}

// startRuntimeMetrics starts the Go runtime (go.*) and process (process.*)
// instruments on p (MON-R28). They are OpenTelemetry only, observed at each
// collection, and exist only where an exporter's provider does. A failure
// to start them is a warning, never a reason to stop exporting the
// gateway's own metrics.
func startRuntimeMetrics(p *sdkmetric.MeterProvider, version string, now func() time.Time, logf func(string, ...any)) {
	if runtime.Start(runtime.WithMeterProvider(p)) != nil {
		logf("gateway: WARNING: telemetry: the Go runtime metrics could not start; the gateway's own metrics still export")
	}
	if startProcessMetrics(p.Meter(processScope, metric.WithInstrumentationVersion(version)), ownProcess(now)) != nil {
		logf("gateway: WARNING: telemetry: the process metrics could not start; the gateway's own metrics still export")
	}
}
