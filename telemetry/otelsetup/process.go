package otelsetup

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.43.0/processconv"
)

// processSource is what the process instruments read, each time a
// collection runs (MON-R28): the process's CPU time by mode, its resident
// memory, and when it started. A figure the platform cannot give reports ok
// false and is left out, never reported as 0 (MON-R15).
type processSource struct {
	cpu   func() (user, system time.Duration, ok bool)
	rss   func() (bytes int64, ok bool)
	start time.Time
	now   func() time.Time
}

// ownProcess reads this process: CPU time from getrusage, resident memory
// from /proc/self/statm (Linux; elsewhere it is unknown). Its start is when
// the exporter's provider is built, at boot, on the same clock as now, so
// uptime never mixes an injected clock with the real one.
func ownProcess(now func() time.Time) processSource {
	return processSource{cpu: cpuTimes, rss: func() (int64, bool) { return statmRSS(os.ReadFile) }, start: now(), now: now}
}

// statmRSS is the resident set size from /proc/self/statm's second field, in
// pages, times the page size.
func statmRSS(read func(string) ([]byte, error)) (int64, bool) {
	b, err := read("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || pages < 0 {
		return 0, false
	}
	return pages * int64(os.Getpagesize()), true
}

// startProcessMetrics registers the process.* instruments on meter with
// their semantic-convention names and units: process.cpu.time
// {cpu.mode = user | system}, process.memory.usage and process.uptime. They
// are observable: each collection reads src in its callback, and nothing
// runs between collections.
func startProcessMetrics(meter metric.Meter, src processSource) error {
	cpu, err := processconv.NewCPUTime(meter)
	if err != nil {
		return err
	}
	mem, err := processconv.NewMemoryUsageObservable(meter)
	if err != nil {
		return err
	}
	up, err := processconv.NewUptimeObservable(meter)
	if err != nil {
		return err
	}
	user := metric.WithAttributes(cpu.AttrCPUMode(processconv.CPUModeUser))
	system := metric.WithAttributes(cpu.AttrCPUMode(processconv.CPUModeSystem))
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if u, s, ok := src.cpu(); ok {
			o.ObserveFloat64(cpu.Inst(), u.Seconds(), user)
			o.ObserveFloat64(cpu.Inst(), s.Seconds(), system)
		}
		if b, ok := src.rss(); ok {
			o.ObserveInt64(mem.Inst(), b)
		}
		o.ObserveFloat64(up.Inst(), src.now().Sub(src.start).Seconds())
		return nil
	}, cpu.Inst(), mem.Inst(), up.Inst())
	return err
}
