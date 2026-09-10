package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

const startupBudget = 600 * time.Second
const shutdownBudget = 20 * time.Second

type supervisorConfig struct {
	base, marker, line, pasVersion, key string
	startupBudget, stopBudget           time.Duration
}

// PID 1 is the sole admission boundary: docker exec cannot become another writer
// or submitter. This guard precedes identity/state/child side effects.
func supervise(argv []string) int {
	if runtime.GOOS != "linux" || os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "supervisor: requires Linux PID 1")
		return 1
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "supervisor: child command required")
		return 1
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Env nil inherits the full environment; arguments are literal, with no shell
	// or reinterpretation of JAVA_OPTS/JAVA_TOOL_OPTIONS or inherited JVM options.
	cfg := supervisorConfig{base: defaultBase, marker: defaultMarker, line: lineFromEnv(os.Getenv), pasVersion: os.Getenv(pasVersionEnv), key: incarnation(), startupBudget: startupBudget, stopBudget: shutdownBudget}
	return superviseChild(context.Background(), cmd, cfg, signals)
}

// superviseChild owns a single exec.Cmd and one worker. Worker failure is terminal
// readiness, but intentionally leaves Java live for explicit Compose recovery.
// Only process shutdown cancels both; Java is never restarted in place.
func superviseChild(ctx context.Context, cmd *exec.Cmd, cfg supervisorConfig, signals <-chan os.Signal) int {
	if ctx.Err() != nil || cfg.key == "" || !validPASConfiguration(cfg.line, cfg.pasVersion) {
		fmt.Fprintln(os.Stderr, "supervisor: startup unavailable")
		return 1
	}
	started := time.Now()
	st := markerState{Schema: stateSchema, Line: cfg.line, Key: cfg.key, State: "waiting-for-metadata", Warm: []string{}}
	if err := writeState(cfg.marker, st); err != nil {
		fmt.Fprintln(os.Stderr, "supervisor: state publication failed")
		return 1
	}
	if err := cmd.Start(); err != nil {
		st.State = "failed"
		st.Failure = "child launch failed"
		_ = writeState(cfg.marker, st)
		fmt.Fprintln(os.Stderr, "supervisor: child launch failed")
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	workerCtx, cancelWorker := context.WithDeadline(context.Background(), started.Add(cfg.startupBudget))
	defer cancelWorker()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); _ = runWarmup(workerCtx, cfg.base, cfg.marker, st) }()
	var timer *time.Timer
	var killAt <-chan time.Time
	shutdown := func(sig os.Signal) {
		cancelWorker()
		_ = cmd.Process.Signal(sig)
		if timer == nil {
			timer = time.NewTimer(cfg.stopBudget)
			killAt = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	canceled := ctx.Done()
	for {
		select {
		case err := <-exited:
			cancelWorker()
			<-workerDone
			// Serialize the final marker after the worker: a departing JVM must not leave
			// readiness even if its final HTTP response raced with process shutdown.
			if current, e := readState(cfg.marker, cfg.line, cfg.key); e == nil {
				st = current
			}
			st.State = "failed"
			st.Failure = "child exited"
			_ = writeState(cfg.marker, st)
			if err == nil {
				return 0
			}
			if exit, ok := err.(*exec.ExitError); ok {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal())
				}
				return exit.ExitCode()
			}
			return 1
		case sig, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			shutdown(sig)
		case <-canceled:
			canceled = nil
			shutdown(syscall.SIGTERM)
		case <-killAt:
			_ = cmd.Process.Kill()
			killAt = nil
		}
	}
}
