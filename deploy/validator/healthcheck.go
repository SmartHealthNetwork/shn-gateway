// healthcheck observes one PID-1-owned qualification worker (FR-G3).
// Ordinary probes never submit validation. Readiness requires the complete
// ordered initialization and PAS verdict corpus for this Java incarnation.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBase   = "http://localhost:8080/fhir"
	defaultMarker = "/tmp/shn-validator-warm"
	procStat      = "/proc/1/stat"
	procBootID    = "/proc/sys/kernel/random/boot_id"
	probeBudget   = 4 * time.Second
)

// lineFromEnv reads SHN_IG_LINE (set per hapi-<line> Dockerfile stage). The
// Dockerfile always sets it, so missing or unknown values fail closed.
func lineFromEnv(getenv func(string) string) string {
	switch l := getenv("SHN_IG_LINE"); l {
	case "2.0", "2.1", "2.2":
		return l
	default:
		return ""
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "supervise":
			os.Exit(supervise(os.Args[2:]))
		case "qualify", "verify-verdicts":
			os.Exit(verdictCommand(os.Args[1:], os.Getenv))
		default:
			fmt.Fprintln(os.Stderr, "healthcheck: invalid command")
			os.Exit(1)
		}
	}
	os.Exit(check(defaultBase, os.Getenv, defaultMarker, probeBudget, incarnation))
}

// check is an observer only. No marker is repaired and no request is submitted.
func check(base string, getenv func(string) string, marker string, budget time.Duration, key func() string) int {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := metadata(ctx, httpClient(), base+"/metadata"); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: metadata unavailable")
		return 1
	}
	st, err := readState(marker, lineFromEnv(getenv), key())
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: warm-up state unavailable; restart validator if persistent")
		return 1
	}
	if st.State == "ready" {
		return 0
	}
	if st.State == "failed" {
		fmt.Fprintf(os.Stderr, "healthcheck: warm-up failed: %s %s; restart validator\n", st.Row, st.Failure)
	} else {
		fmt.Fprintf(os.Stderr, "healthcheck: qualification pending: %d/%d completed\n", len(st.Warm), len(readinessRows(st.Line)))
	}
	return 1
}

// Both boot identity and PID 1 start time must be available. A surviving /tmp
// marker must never authorize a fresh JVM or an unknown incarnation.
func incarnation() string {
	stat, err := os.ReadFile(procStat)
	if err != nil {
		return ""
	}
	boot, err := os.ReadFile(procBootID)
	if err != nil {
		return ""
	}
	return incarnationFrom(string(stat), string(boot))
}
func incarnationFrom(stat, bootID string) string {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 20 {
		return ""
	}
	boot := strings.TrimSpace(bootID)
	if boot == "" || strings.ContainsAny(boot, " \t\r\n:") {
		return ""
	}
	ticks, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil || ticks == 0 {
		return ""
	}
	return boot + ":" + f[19]
}
