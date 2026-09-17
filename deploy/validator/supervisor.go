package main

import (
	"context"
	"fmt"
	"net/url"
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
	public                              string
	startupBudget, stopBudget           time.Duration
	warm                                func(context.Context, string, string, markerState) error
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
	childArgs, err := launchArgs(argv, os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd := exec.Command(childArgs[0], childArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Env nil inherits the full environment; arguments are literal, with no shell
	// or reinterpretation of JAVA_OPTS/JAVA_TOOL_OPTIONS or inherited JVM options.
	cfg := supervisorConfig{base: privateBase, public: ":8080", marker: defaultMarker, line: lineFromEnv(os.Getenv), pasVersion: os.Getenv(pasVersionEnv), key: incarnation(), startupBudget: startupBudget, stopBudget: shutdownBudget, warm: runImageWarmup}
	return superviseChild(context.Background(), cmd, cfg, signals)
}

// superviseChild owns a single exec.Cmd and one worker. Worker failure is terminal
// readiness, but intentionally leaves Java live for explicit Compose recovery.
// Only process shutdown cancels both; Java is never restarted in place.
func superviseChild(ctx context.Context, cmd *exec.Cmd, cfg supervisorConfig, signals <-chan os.Signal) int {
	backend, err := url.Parse(cfg.base)
	if err != nil || backend.Scheme != "http" || backend.Host == "" || ctx.Err() != nil || cfg.key == "" || !validPASConfiguration(cfg.line, cfg.pasVersion) {
		fmt.Fprintln(os.Stderr, "supervisor: startup unavailable")
		return 1
	}
	a, err := newAdmission(ctx, cfg.public, backend.Host, forwardingLimit, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervisor: public listener unavailable")
		return 1
	}
	return superviseBoundary(ctx, cmd, cfg, signals, a)
}

// superviseBoundary owns the already reserved public listener and one child.
// The internal seam permits deterministic held-result tests without changing
// the image CLI or weakening the worker's real corpus.
func superviseBoundary(ctx context.Context, cmd *exec.Cmd, cfg supervisorConfig, signals <-chan os.Signal, a *admission) int {
	defer func() { a.revoke(); a.wait() }()
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
	go func() { err := cmd.Wait(); a.revoke(); exited <- err }()
	workerCtx, cancelWorker := context.WithDeadline(context.Background(), started.Add(cfg.startupBudget))
	defer cancelWorker()
	workerDone := make(chan struct{})
	workerResult := make(chan error, 1)
	warm := cfg.warm
	if warm == nil {
		warm = runWarmup
	}
	go func() { defer close(workerDone); workerResult <- warm(workerCtx, cfg.base, cfg.marker, st) }()
	var timer *time.Timer
	var killAt <-chan time.Time
	shuttingDown := false
	shutdown := func(sig os.Signal) {
		shuttingDown = true
		a.revoke()
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
		case err := <-workerResult:
			workerResult = nil // one finite result, never retry a failed worker
			current, stateErr := readState(cfg.marker, cfg.line, cfg.key)
			if err != nil || stateErr != nil || current.State != "ready" || workerCtx.Err() != nil || ctx.Err() != nil || shuttingDown {
				a.revoke()
				fmt.Fprintln(os.Stderr, "supervisor: qualification unavailable; restart validator")
				continue
			}
			// The process waiter revokes before publishing exit; admission also
			// rejects lifetime cancellation. A completed marker is never enough.
			if a.admit() {
				fmt.Fprintln(os.Stderr, "supervisor: public admission ready")
			}
		case <-a.errors:
			fmt.Fprintln(os.Stderr, "supervisor: public listener failed")
			shutdown(syscall.SIGTERM)
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
