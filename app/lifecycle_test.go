package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type lifecycleTransport func(*http.Request) (*http.Response, error)

func (f lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The strict transport exercises the real exported constructors, discovery and
// metadata qualifier without DNS/network or a substitute qualifier. The delayed
// cancellation return distinguishes cancellation from a completed worker join.
func lifecycleEnv(t *testing.T, failBuild bool) (map[string]string, <-chan struct{}, <-chan struct{}, func()) {
	t.Helper()
	env := buildEnvWithBundle(t, "provider", "provider")
	env["SHN_DISCOVERY_URL"] = "http://fixture.test/discovery"
	env["FHIR_VALIDATE_URL"] = "http://fixture.test/fhir"
	env["FHIR_VALIDATE_URL_2_2"] = "http://fixture.test/v22"
	env["FHIR_DATA_URL"] = "http://fixture.test/fhir"
	env["HOST"] = "127.0.0.1"
	env["PORT"] = freePort(t) // not the default 8080, which another gateway or test run may hold
	if failBuild {
		env["REGISTRAR_URL"] = "http://fixture.test/registrar"
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, cancelOnce, releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	old := http.DefaultTransport
	http.DefaultTransport = lifecycleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "shn-validator-2-1:8080" && r.Method == "GET" && r.URL.Path == "/fhir/metadata" {
			startOnce.Do(func() { close(started) })
			<-r.Context().Done()
			cancelOnce.Do(func() { close(canceled) })
			<-release
			return nil, r.Context().Err()
		}
		if r.URL.Host != "fixture.test" && r.URL.Host != "populate.test" {
			return nil, fmt.Errorf("unexpected fixture host %s", r.URL.Host)
		}
		body, status := `{}`, http.StatusOK
		switch r.URL.Path {
		case "/discovery":
			body = `{"endpoints":{},"authzPublicKeyURL":"http://fixture.test/key","hubTransportKeyURL":"http://fixture.test/key"}`
		case "/key":
			body = fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(make([]byte, 32)))
		case "/registrar/holders":
			<-started
			status = http.StatusServiceUnavailable
		case "/fhir/metadata", "/v22/metadata":
			body = `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	// The certification clients dial through this fixture too, never on their
	// own: a real lookup of fixture.test is live network, and a synctest
	// bubble can join a lookup an earlier test left running outside it.
	oldClient := certificationClient
	certificationClient = func(endpoint string) *shnsdk.OperationValidator {
		return &shnsdk.OperationValidator{BaseURL: endpoint, Client: &http.Client{}} // http.DefaultTransport at request time
	}
	t.Cleanup(func() { finish(); http.DefaultTransport = old; certificationClient = oldClient })
	return env, started, canceled, finish
}

// The loopback ports freePort chooses from: below the ephemeral range of Linux
// (32768-60999) and of macOS (49152-65535).
const (
	freePortLow  = 20000
	freePortHigh = 32767
)

// freePort is a loopback port, probed free, for Run to bind. Run binds PORT
// itself, after building the gateway, so a test can only hand it a number and
// release the port until then. A port the kernel hands out for 127.0.0.1:0 sits
// in the ephemeral range, where any other socket on the machine (an outgoing
// connection's own port, another listener on port 0) can take it in that window.
// A port below every ephemeral range is taken only by a socket bound to it by
// number. The probe order is seeded by the process and the test, so two
// processes, or two tests, start from different ports; two calls in one test
// return the same port while it is free.
func freePort(t *testing.T) string {
	t.Helper()
	h := fnv.New64a()
	_, _ = h.Write([]byte(t.Name()))
	r := rand.New(rand.NewPCG(uint64(os.Getpid()), h.Sum64()))
	var last error
	for range 200 {
		port := freePortLow + r.IntN(freePortHigh-freePortLow+1)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			last = err
			continue
		}
		_ = l.Close()
		return strconv.Itoa(port)
	}
	t.Fatalf("freePort: no free port in %d-%d after 200 probes; last: %v", freePortLow, freePortHigh, last)
	return ""
}

func TestFreePortIsBelowEveryEphemeralRange(t *testing.T) {
	ephemeral := [][2]int{{32768, 60999}, {49152, 65535}} // Linux, macOS
	// The range this machine actually uses, where it says: a host that widens
	// its ephemeral range over freePort's makes freePort unsafe there.
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		var lo, hi int
		if _, err := fmt.Sscan(string(b), &lo, &hi); err != nil {
			t.Fatalf("read ip_local_port_range %q: %v", b, err)
		}
		if lo <= freePortHigh && hi >= freePortLow {
			t.Fatalf("this host's ephemeral range %d-%d overlaps freePort's %d-%d", lo, hi, freePortLow, freePortHigh)
		}
	}
	seen := map[int]bool{}
	for i := range 50 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			port, err := strconv.Atoi(freePort(t))
			if err != nil {
				t.Fatal(err)
			}
			if port < freePortLow || port > freePortHigh {
				t.Fatalf("freePort = %d, want %d-%d", port, freePortLow, freePortHigh)
			}
			for _, r := range ephemeral {
				if port >= r[0] && port <= r[1] {
					t.Fatalf("freePort = %d, inside the ephemeral range %d-%d", port, r[0], r[1])
				}
			}
			seen[port] = true
		})
	}
	if len(seen) < 2 {
		t.Errorf("freePort gave %d distinct ports over 50 tests, want the probe order to vary by test", len(seen))
	}
}

// awaitRunStarted waits for ch, or reports Run's own error if it returned
// first (a bind failure, say) instead of timing out on ch.
func awaitRunStarted(t *testing.T, ch, done <-chan struct{}, err *error, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-done:
		t.Fatalf("Run returned before %s: %v", label, *err)
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func awaitLifecycle(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestHandlerLifecycleOwnership(t *testing.T) {
	for _, name := range []string{"Handler", "HandlerWithClock", "HandlerForTest", "HandlerForTestWithClock"} {
		t.Run(name, func(t *testing.T) {
			env, started, canceled, release := lifecycleEnv(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var h http.Handler
			var cleanup func()
			var err error
			getenv := func(k string) string { return env[k] }
			switch name {
			case "Handler":
				h, err = Handler(ctx, getenv, io.Discard)
			case "HandlerWithClock":
				h, err = HandlerWithClock(ctx, getenv, io.Discard, func() time.Time { return time.Unix(1234, 0) })
			case "HandlerForTest":
				h, cleanup, err = HandlerForTest(ctx, getenv, io.Discard)
			case "HandlerForTestWithClock":
				h, cleanup, err = HandlerForTestWithClock(ctx, getenv, io.Discard, func() time.Time { return time.Unix(1234, 0) })
			}
			if err != nil {
				t.Fatal(err)
			}
			awaitLifecycle(t, started, "default metadata worker")
			closer, ok := h.(io.Closer)
			if !ok {
				cancel()
				release()
				awaitLifecycle(t, canceled, "failed-test cancellation")
				t.Fatalf("%s returned %T without lifecycle ownership", name, h)
			}
			if cleanup == nil {
				cleanup = func() { _ = closer.Close() }
			}
			defer func() {
				cancel()
				release()
				cleanup()
			}()
			done := make(chan struct{})
			go func() { cleanup(); close(done) }()
			awaitLifecycle(t, canceled, "cleanup cancellation")
			select {
			case <-done:
				t.Error("cleanup returned before worker finished")
			default:
			}
			release()
			awaitLifecycle(t, done, "cleanup join")
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuildErrorJoinsDefaultWorkers(t *testing.T) {
	env, _, canceled, release := lifecycleEnv(t, true)
	done := make(chan struct{})
	var err error
	go func() {
		_, err = Handler(context.Background(), func(k string) string { return env[k] }, io.Discard)
		close(done)
	}()
	awaitLifecycle(t, canceled, "post-discovery build-error cancellation")
	select {
	case <-done:
		t.Error("build error returned before worker finished")
	default:
	}
	release()
	awaitLifecycle(t, done, "build-error join")
	if err == nil || !strings.Contains(err.Error(), "converge peer registry") {
		t.Fatalf("build error=%v", err)
	}
}

func TestRunShutdownJoinsDefaultWorkers(t *testing.T) {
	env, started, canceled, release := lifecycleEnv(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() { err = Run(ctx, func(k string) string { return env[k] }, io.Discard); close(done) }()
	awaitRunStarted(t, started, done, &err, "Run default worker")
	cancel()
	awaitLifecycle(t, canceled, "Run cancellation")
	select {
	case <-done:
		t.Error("Run returned before worker finished")
	default:
	}
	release()
	awaitLifecycle(t, done, "Run join")
	if err != nil {
		t.Fatal(err)
	}
}

// Run owns its boot checks as well as default-lane qualification. A
// canceled transport may still be unwinding when Run releases dependencies.
func TestRunShutdownJoinsBootChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env, _, _, releaseLane := lifecycleEnv(t, false)
		releaseLane()
		base := http.DefaultTransport
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		http.DefaultTransport = lifecycleTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "fixture.test" && r.URL.Path == "/fhir" {
				close(started)
				<-r.Context().Done()
				close(canceled)
				<-release
				return nil, r.Context().Err()
			}
			return base.RoundTrip(r)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		var err error
		go func() { err = Run(ctx, func(k string) string { return env[k] }, io.Discard); close(done) }()
		awaitRunStarted(t, started, done, &err, "boot connectivity request")
		cancel()
		awaitLifecycle(t, canceled, "boot connectivity cancellation")
		synctest.Wait()
		select {
		case <-done:
			t.Error("Run returned while boot connectivity request was still unwinding")
		default:
		}
		close(release)
		awaitLifecycle(t, done, "boot connectivity join")
		if err != nil {
			t.Fatal(err)
		}
	})
}

// Cleanup must own cancellation even when a listener error leaves the parent
// context live, and must wait for each worker to stop using its dependencies.
func TestRunWorkersCancelAndJoinWithLiveParent(t *testing.T) {
	for _, worker := range []string{"checks", "registrar", "key refresh", "pool stats"} {
		t.Run(worker, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				block := func(ctx context.Context) {
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release
				}
				client := &http.Client{Transport: lifecycleTransport(func(r *http.Request) (*http.Response, error) {
					block(r.Context())
					return nil, r.Context().Err()
				})}
				b := built{checksRunner: checks.NewRunner(nil, client, time.Now)}
				switch worker {
				case "checks":
					b.checksRunner = checks.NewRunner([]checks.Target{{ID: "test", URL: "http://fixture.test/check", Kind: checks.KindReachable}}, client, time.Now)
				case "registrar":
					b.registrarURL, b.client, b.reg = "http://fixture.test/registrar", client, shnsdk.NewRegistry()
				case "key refresh":
					b.keyRefresh = block
				case "pool stats":
					b.poolStats = block
				}
				parent := context.Background()
				stop := b.startWorkers(parent)
				<-started
				done := make(chan struct{})
				go func() { stop(); close(done) }()
				<-canceled
				synctest.Wait()
				select {
				case <-done:
					t.Error("worker cleanup returned before the worker released its dependencies")
				default:
				}
				close(release)
				<-done
				if parent.Err() != nil {
					t.Fatal("worker cleanup canceled its caller's context")
				}
				stop()
			})
		})
	}
}

func TestRunListenerErrorWithLiveParent(t *testing.T) {
	env, _, _, release := lifecycleEnv(t, false)
	release()
	env["HOST"] = "invalid:host:" // The constructed listener address has too many colons.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var err error
	go func() { err = Run(ctx, func(k string) string { return env[k] }, io.Discard); close(done) }()
	awaitLifecycle(t, done, "listener-error cleanup")
	if err == nil || !strings.Contains(err.Error(), "too many colons") {
		t.Fatalf("expected listener address error, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run canceled its caller's context after listener failure")
	}
}
