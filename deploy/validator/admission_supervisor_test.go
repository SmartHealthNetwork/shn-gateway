package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorRequiresWorkerResultAndCurrentCompleteMarker(t *testing.T) {
	for _, mode := range []string{"ready", "worker-error", "failed", "stale", "incomplete", "child-exit", "shutdown", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			lane := newFakeLane(t)
			cfg := supervisorTestConfig(t, lane.base())
			cfg.startupBudget = 3 * time.Second
			if mode == "deadline" {
				cfg.startupBudget = 2 * time.Second
			}
			ctx, cancel := context.WithCancel(context.Background())
			a := testAdmission(t, strings.TrimPrefix(lane.srv.URL, "http://"), 8, nil)
			published, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			cfg.warm = func(worker context.Context, base, path string, st markerState) error {
				if err := runWarmup(worker, base, path, st); err != nil {
					return err
				}
				close(published)
				if mode == "deadline" {
					<-worker.Done()
				} else {
					select {
					case <-release:
					case <-worker.Done():
					}
				}
				if mode == "worker-error" {
					return errors.New("controlled worker failure")
				}
				current, err := readState(path, cfg.line, cfg.key)
				if err != nil {
					return err
				}
				switch mode {
				case "failed":
					current.State = "failed"
					current.Failure = "wrong response status"
				case "stale":
					current.Key = otherJVM()
				case "incomplete":
					current.Warm = current.Warm[:1]
				}
				if err := writeState(path, current); err != nil {
					return err
				}
				return nil
			}
			cmd, reader := childCommand(t, "wait")
			done := make(chan int, 1)
			go func() {
				code := superviseBoundary(ctx, cmd, cfg, make(chan os.Signal), a)
				cmd.Stdout.(*os.File).Close()
				done <- code
			}()
			joined := false
			defer func() {
				cancel()
				unblock()
				if !joined {
					childResult(t, done)
				}
			}()
			awaitChild(t, reader, cmd)
			select {
			case <-published:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not publish ready")
			}
			// Marker publication is deliberately ahead of worker result delivery.
			if st := readTestState(t, cfg.marker); st.State != "ready" {
				t.Fatal(st)
			}
			if check("http://"+a.addr()+"/fhir", envLine("2.2"), cfg.marker, time.Second, sameJVM) != 1 {
				t.Fatal("marker alone admitted public service")
			}
			switch mode {
			case "shutdown":
				cancel()
			case "child-exit":
				cmd.Process.Kill()
			}
			unblock()
			if mode == "ready" {
				deadline := time.After(3 * time.Second)
				for {
					a.mu.Lock()
					ready := a.ready
					a.mu.Unlock()
					if ready {
						break
					}
					select {
					case <-deadline:
						t.Fatal("successful worker did not admit")
					case <-time.After(time.Millisecond):
					}
				}
				if check("http://"+a.addr()+"/fhir", envLine("2.2"), cfg.marker, time.Second, sameJVM) != 0 {
					t.Fatal("qualified public service unhealthy")
				}
			} else {
				select {
				case <-a.ctx.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("failed/raced worker did not close public admission")
				}
				if mode != "child-exit" && mode != "shutdown" {
					if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
						t.Fatal("qualification failure killed child")
					}
				}
			}
			cancel()
			childResult(t, done)
			joined = true
			if len(lane.recorded()) != len(readinessRows("2.2")) {
				t.Fatal("worker corpus omitted/retried")
			}
		})
	}
}

func TestSupervisorPublicBindFailurePrecedesChildLaunch(t *testing.T) {
	lane := newFakeLane(t)
	cfg := supervisorTestConfig(t, lane.base())
	cfg.public = strings.TrimPrefix(lane.srv.URL, "http://")
	cmd, _ := childCommand(t, "exit")
	if superviseChild(context.Background(), cmd, cfg, make(chan os.Signal)) != 1 || cmd.Process != nil {
		t.Fatal("launched child despite public collision")
	}
}

func TestAdmissionLifetimeSurvivesSuccessfulStartupDeadline(t *testing.T) {
	lane := newFakeLane(t)
	cfg := supervisorTestConfig(t, lane.base())
	cfg.startupBudget = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	a := testAdmission(t, strings.TrimPrefix(lane.srv.URL, "http://"), 8, nil)
	workerExpired := make(chan struct{})
	cfg.warm = func(w context.Context, base, path string, st markerState) error {
		go func() { <-w.Done(); close(workerExpired) }()
		return runWarmup(w, base, path, st)
	}
	cmd, reader := childCommand(t, "wait")
	done := make(chan int, 1)
	go func() {
		code := superviseBoundary(ctx, cmd, cfg, make(chan os.Signal), a)
		cmd.Stdout.(*os.File).Close()
		done <- code
	}()
	defer func() { cancel(); childResult(t, done) }()
	awaitChild(t, reader, cmd)
	select {
	case <-workerExpired:
	case <-time.After(5 * time.Second):
		t.Fatal("startup deadline did not expire")
	}
	r, err := http.Get("http://" + a.addr() + "/fhir/metadata")
	if err != nil {
		t.Fatal("startup deadline revoked lifetime", err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
}

func TestSupervisorReservedListenerFailure(t *testing.T) {
	for _, mode := range []string{"held-worker-result", "admitted-active-connection"} {
		t.Run(mode, func(t *testing.T) {
			lane := newFakeLane(t)
			cfg := supervisorTestConfig(t, lane.base())
			cfg.startupBudget = 10 * time.Second
			ctx, cancel := context.WithCancel(context.Background())
			published, release, workerReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			workerContext := make(chan context.Context, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			cfg.warm = func(worker context.Context, base, path string, st markerState) error {
				defer close(workerReturned)
				workerContext <- worker
				if err := runWarmup(worker, base, path, st); err != nil {
					return err
				}
				close(published)
				if mode == "held-worker-result" {
					<-worker.Done()
				}
				<-release
				return nil // Deliberately allow a successful result after cancellation.
			}
			backend, peer := net.Pipe()
			peerDone := make(chan error, 1)
			go func() {
				defer peer.Close()
				b := make([]byte, 1)
				_, err := io.ReadFull(peer, b)
				if err == nil {
					_, err = peer.Write(b)
				}
				if err == nil {
					_, err = io.Copy(io.Discard, peer)
				}
				peerDone <- err
			}()
			a := testAdmission(t, "controlled-backend", 1, func(context.Context, string, string) (net.Conn, error) { return backend, nil })
			cmd, reader := childCommand(t, "wait")
			done := make(chan int, 1)
			go func() {
				code := superviseBoundary(ctx, cmd, cfg, make(chan os.Signal), a)
				cmd.Stdout.(*os.File).Close()
				done <- code
			}()
			joined, peerJoined := false, false
			defer func() {
				// Release every held operation before joining, including mutation
				// failures that intentionally omit the listener notification.
				cancel()
				unblock()
				backend.Close()
				peer.Close()
				if !joined {
					childResult(t, done)
				}
				if !peerJoined {
					select {
					case <-peerDone:
					case <-time.After(3 * time.Second):
						t.Error("backend observer did not join")
					}
				}
			}()
			awaitChild(t, reader, cmd)
			select {
			case <-published:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not publish complete marker")
			}
			worker := <-workerContext
			if st := readTestState(t, cfg.marker); st.State != "ready" {
				t.Fatal(st)
			}
			var client net.Conn
			if mode == "admitted-active-connection" {
				unblock()
				// Cold connections close immediately. A completed byte exchange
				// establishes actual admission and backend registration; time is
				// only a safety bound, never the ordering condition.
				deadline := time.Now().Add(3 * time.Second)
				for client == nil && time.Now().Before(deadline) {
					c, err := net.DialTimeout("tcp", a.addr(), time.Second)
					if err != nil {
						t.Fatal(err)
					}
					c.SetDeadline(deadline)
					_, err = c.Write([]byte("x"))
					b := make([]byte, 1)
					if err == nil {
						_, err = io.ReadFull(c, b)
					}
					if err == nil && string(b) == "x" {
						client = c
					} else {
						c.Close()
					}
				}
				if client == nil {
					t.Fatal("successful worker did not admit a forwarding connection")
				}
				defer client.Close()
				if a.count() != 1 {
					t.Fatal("active connection was not tracked")
				}
			}
			a.mu.Lock()
			closed, ready := a.closed, a.ready
			a.mu.Unlock()
			if closed || ready != (mode == "admitted-active-connection") {
				t.Fatalf("wrong state before listener failure: closed=%t ready=%t", closed, ready)
			}
			// Fail the actual reserved listener while its owner is still live.
			// Calling revoke here would bypass the Accept-error branch under test.
			if err := a.listener.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-a.ctx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("listener failure did not revoke admission")
			}
			select {
			case <-worker.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("listener failure did not notify supervisor/cancel worker")
			}
			if a.admit() {
				t.Fatal("failed listener reopened admission")
			}
			if mode == "held-worker-result" {
				select {
				case <-done:
					joined = true
					t.Fatal("supervisor returned before held worker joined")
				default:
				}
			}
			unblock()
			code := childResult(t, done)
			joined = true
			if code != 0 || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatalf("child shutdown/reap failed: code=%d state=%v", code, cmd.ProcessState)
			}
			if line, err := reader.ReadString('\n'); err != nil || line != "signal=terminated\n" {
				t.Fatalf("listener failure did not signal child: %q %v", line, err)
			}
			select {
			case <-workerReturned:
			default:
				t.Fatal("supervisor did not join worker")
			}
			a.mu.Lock()
			closed, ready = a.closed, a.ready
			a.mu.Unlock()
			if !closed || ready || a.admit() || a.count() != 0 {
				t.Fatal("late success reopened admission or forwarding pairs survived join")
			}
			if st := readTestState(t, cfg.marker); st.State != "failed" || st.Failure != "child exited" {
				t.Fatal("reaped child left a ready marker", st)
			}
			if client != nil {
				if _, err := client.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("active public connection survived revocation: %v", err)
				}
				select {
				case err := <-peerDone:
					peerJoined = true
					if err != nil {
						t.Fatal("active backend connection did not close cleanly", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("backend connection survived forwarding join")
				}
			}
		})
	}
}
