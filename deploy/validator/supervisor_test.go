package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type childReport struct {
	Args, Env []string
	Marker    markerState
	Directory string
}

// These are actual child processes, with all test behavior confined to _test.go.
func TestJavaProcess(t *testing.T) {
	mode := os.Getenv("SHN_TEST_CHILD")
	if mode == "" {
		return
	}
	if mode == "exit" {
		os.Exit(23)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	report := childReport{Args: os.Args, Env: os.Environ()}
	report.Directory, _ = os.Getwd()
	if path := os.Getenv("SHN_TEST_STARTUP_MARKER"); path != "" {
		raw, _ := os.ReadFile(path)
		_ = json.Unmarshal(raw, &report.Marker)
	}
	raw, _ := json.Marshal(report)
	fmt.Println(string(raw))
	if mode == "wait" {
		sig := <-signals
		fmt.Printf("signal=%s\n", sig)
		os.Exit(0)
	}
	for {
		<-signals
	}
}
func childCommand(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJavaProcess$", "--", "literal space", "$JAVA_OPTS", "", "semi;colon")
	// Race-instrumented helper exits must not add the race runtime's one-second
	// exit sleep to the intentionally short injected shutdown budget.
	cmd.Env = append(os.Environ(), "SHN_TEST_CHILD="+mode, "JAVA_OPTS=-Xmx3072m", "JAVA_TOOL_OPTIONS=literal value", "GORACE=atexit_sleep_ms=0")
	// The test owns these descriptors. Wait must not close the read
	// end before the test consumes the child's final signal acknowledgement.
	pipe, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = output
	t.Cleanup(func() {
		_ = output.Close()
		_ = pipe.Close()
	})
	cmd.Stderr = os.Stderr
	return cmd, bufio.NewReader(pipe)
}
func supervisorTestConfig(t *testing.T, base string) supervisorConfig {
	return supervisorConfig{base: base, marker: markerPath(t), line: "2.2", pasVersion: "2.2.1", key: sameJVM(), startupBudget: time.Second, stopBudget: 100 * time.Millisecond}
}
func runChild(t *testing.T, ctx context.Context, cmd *exec.Cmd, cfg supervisorConfig, signals <-chan os.Signal) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		code := superviseChild(ctx, cmd, cfg, signals)
		// Supervision has reaped the child. Close the test-owned parent writer
		// so readers can drain the remaining bytes through EOF.
		_ = cmd.Stdout.(*os.File).Close()
		done <- code
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})
	return done
}
func childResult(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not terminate/reap child")
		return -1
	}
}
func awaitChild(t *testing.T, r *bufio.Reader, cmd *exec.Cmd) childReport {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var got childReport
	if json.Unmarshal(line, &got) != nil {
		t.Fatal("invalid child report")
	}
	if !reflect.DeepEqual(got.Args, cmd.Args) || !reflect.DeepEqual(got.Env, cmd.Env) {
		t.Fatal("child argv/environment changed")
	}

	cwd, err := os.Getwd()
	if err != nil || got.Directory != cwd {
		t.Fatal("child working directory changed")
	}
	return got
}

func TestSupervisorRefusesNonPID1WithoutEffects(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("this rejection row requires non-PID-1 caller")
	}
	before, err := os.ReadFile(defaultMarker)
	existed := err == nil
	if got := supervise([]string{"/definitely/no/child"}); got != 1 {
		t.Fatalf("supervise=%d", got)
	}
	after, afterErr := os.ReadFile(defaultMarker)
	if existed != (afterErr == nil) || !bytes.Equal(before, after) {
		t.Fatal("non-PID-1 supervisor changed marker")
	}
}
func TestSupervisorForwardsSignalAndReaps(t *testing.T) {
	lane := newFakeLane(t)
	cfg := supervisorTestConfig(t, lane.base())
	cmd, reader := childCommand(t, "wait")
	signals := make(chan os.Signal, 1)
	done := runChild(t, context.Background(), cmd, cfg, signals)
	awaitChild(t, reader, cmd)
	signals <- syscall.SIGTERM
	// Consume the final witness after Wait, so pipe ownership cannot
	// depend on the reader winning a scheduling race against child reaping.
	if got := childResult(t, done); got != 0 {
		t.Fatalf("exit=%d", got)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "signal=terminated\n" {
		t.Fatalf("signal forwarding: %q %v", line, err)
	}
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Fatalf("child output drain: %q %v", rest, err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("child not reaped")
	}
	if readTestState(t, cfg.marker).State != "failed" {
		t.Fatal("exited child left ready marker")
	}
}
func TestSupervisorCancellationAndForcedShutdown(t *testing.T) {
	for _, mode := range []string{"wait", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			lane := newFakeLane(t)
			cfg := supervisorTestConfig(t, lane.base())
			cmd, reader := childCommand(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runChild(t, ctx, cmd, cfg, make(chan os.Signal))
			awaitChild(t, reader, cmd)
			cancel()
			got := childResult(t, done)
			if mode == "wait" && got != 0 {
				t.Fatalf("graceful exit=%d", got)
			}
			if mode == "ignore" && got != 137 {
				t.Fatalf("forced exit=%d", got)
			}
			if cmd.ProcessState == nil {
				t.Fatal("child not reaped")
			}
			if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
				t.Fatal("child still exists")
			}
		})
	}
}
func TestSupervisorChildExitAndStartupRefusals(t *testing.T) {
	lane := newFakeLane(t)
	for _, mode := range []string{"exit", "launch-failure", "marker-failure", "identity-failure", "missing-version", "wrong-version", "wrong-line", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			cfg := supervisorTestConfig(t, lane.base())
			cmd, _ := childCommand(t, "exit")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "launch-failure":
				cmd = exec.Command(filepath.Join(t.TempDir(), "absent"))
			case "marker-failure":
				cfg.marker += "/missing/state"
			case "identity-failure":
				cfg.key = ""
			case "missing-version":
				cfg.pasVersion = ""
			case "wrong-version":
				cfg.pasVersion = "2.1.0"
			case "wrong-line":
				cfg.line = "9.9"
			case "canceled":
				cancel()
			}
			got := superviseChild(ctx, cmd, cfg, make(chan os.Signal))
			want := 1
			if mode == "exit" {
				want = 23
			}
			if got != want {
				t.Fatalf("exit=%d want=%d", got, want)
			}
			if mode == "marker-failure" || mode == "identity-failure" || mode == "missing-version" || mode == "wrong-version" || mode == "wrong-line" || mode == "canceled" {
				if cmd.Process != nil {
					t.Fatal("launched child before startup admission")
				}
			}
		})
	}
}
func TestSupervisorWarmupFailureKeepsChildAlive(t *testing.T) {
	lane := newFakeLane(t)
	lane.validate = func(w http.ResponseWriter, _ *http.Request) bool { fmt.Fprint(w, "invalid"); return false }
	cfg := supervisorTestConfig(t, lane.base())
	cmd, reader := childCommand(t, "wait")
	signals := make(chan os.Signal, 1)
	done := runChild(t, context.Background(), cmd, cfg, signals)
	awaitChild(t, reader, cmd)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		st := readTestState(t, cfg.marker)
		if st.State == "failed" {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("worker did not publish failure")
		case <-tick.C:
		}
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("worker failure terminated Java")
	}
	observerProcess(t, lane.base(), cfg.marker, sameJVM(), 1)
	if len(lane.recorded()) != 1 {
		t.Fatal("worker failure retried")
	}
	signals <- syscall.SIGTERM
	io.ReadAll(reader)
	if childResult(t, done) != 0 {
		t.Fatal("child shutdown failed")
	}
}

// Exercise CLI rejection in an independent process too, with a prospective child
// writing a file if it is ever launched. No production admission bypass is added.
func TestSupervisorCLIProcess(t *testing.T) {
	if os.Getenv("SHN_TEST_CLI") != "1" {
		return
	}
	os.Exit(supervise([]string{os.Args[0], "-test.run=^TestUnexpectedChild$"}))
}
func TestUnexpectedChild(t *testing.T) {
	if path := os.Getenv("SHN_TEST_LAUNCH_FILE"); path != "" {
		os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
		os.Exit(0)
	}
}
func TestSecondSupervisorProcessCannotLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launched")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorCLIProcess$")
	cmd.Env = append(os.Environ(), "SHN_TEST_CLI=1", "SHN_TEST_LAUNCH_FILE="+path)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Linux PID 1") {
		t.Fatalf("second supervisor accepted: %v %s", err, out)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("second supervisor launched child")
	}
}

func TestSupervisorReplacesSurvivingMarkerBeforeChildStarts(t *testing.T) {
	lane := newFakeLane(t)
	atomic.StoreInt32(&lane.metadata, 503)
	cfg := supervisorTestConfig(t, lane.base())
	cfg.key = otherJVM()
	st := initialState(sameJVM())
	st.State = "ready"
	st.Warm = readinessIdentities("2.2")
	if err := writeState(cfg.marker, st); err != nil {
		t.Fatal(err)
	}
	cmd, reader := childCommand(t, "wait")
	cmd.Env = append(cmd.Env, "SHN_TEST_STARTUP_MARKER="+cfg.marker)
	signals := make(chan os.Signal, 1)
	done := runChild(t, context.Background(), cmd, cfg, signals)
	report := awaitChild(t, reader, cmd)
	if report.Marker.Key != otherJVM() || report.Marker.State != "waiting-for-metadata" || len(report.Marker.Warm) != 0 {
		t.Fatalf("child inherited stale readiness: %+v", report.Marker)
	}
	signals <- syscall.SIGTERM
	io.ReadAll(reader)
	if childResult(t, done) != 0 {
		t.Fatal("shutdown failed")
	}
	if len(lane.recorded()) != 0 {
		t.Fatal("POST before metadata")
	}
}
