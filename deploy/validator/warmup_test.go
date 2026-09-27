package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func initialState(key string) markerState {
	return markerState{Schema: stateSchema, Line: "2.2", Key: key, State: "waiting-for-metadata", Warm: []string{}}
}
func readTestState(t *testing.T, path string) markerState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st markerState
	if err = json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}
func startWorker(t *testing.T, ctx context.Context, base, path, key string) <-chan error {
	t.Helper()
	st := initialState(key)
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runWarmup(ctx, base, path, st) }()
	return done
}

// awaitRequest waits until the request a row is waiting for reaches the lane
// (reached closes) or the worker ends, whichever comes first, and never more
// than 5 s. A worker that ends first ends the wait; ended reports that, with
// the worker's error. A bare <-reached would block the whole package until the
// test binary's timeout, which inside the image build fails it after 10 minutes.
func awaitRequest(t *testing.T, reached <-chan struct{}, done <-chan error) (ended bool, err error) {
	t.Helper()
	select {
	case <-reached:
		return false, nil
	case err := <-done:
		return true, err
	case <-time.After(5 * time.Second):
		t.Fatal("the worker neither sent the awaited request nor ended")
		return false, nil
	}
}

// mustReach is awaitRequest for a row whose worker must not end first.
func mustReach(t *testing.T, reached <-chan struct{}, done <-chan error) {
	t.Helper()
	if ended, err := awaitRequest(t, reached, done); ended {
		t.Fatalf("the worker ended before the awaited request: %v", err)
	}
}

// expiringContext is a context whose deadline passes when expire is called, so
// a row can make it pass while a post is in flight, however long the worker
// took to reach that post. A timer started before the worker cannot promise
// that: on a loaded machine it can pass during the metadata probe instead.
// Deadline reports none: only Err's DeadlineExceeded marks it a deadline, and
// neither the worker nor its HTTP client reads Deadline once the post is sent.
type expiringContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newExpiringContext() *expiringContext {
	return &expiringContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *expiringContext) expire() { c.once.Do(func() { close(c.done) }) }

func finishWorker(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
		return nil
	}
}

func readinessIdentities(line string) []string {
	rows := readinessRows(line)
	identities := make([]string, len(rows))
	for i, row := range rows {
		identities[i] = row.identity
	}
	return identities
}

// Independent OS processes execute the production observer with test-only arguments.
func TestObserverProcess(t *testing.T) {
	if os.Getenv("SHN_TEST_OBSERVER") != "1" {
		return
	}
	os.Exit(check(os.Getenv("SHN_TEST_BASE"), envLine("2.2"), os.Getenv("SHN_TEST_MARKER"), time.Second, func() string { return os.Getenv("SHN_TEST_KEY") }))
}
func observerProcess(t *testing.T, base, path, key string, want int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestObserverProcess$")
	cmd.Env = append(os.Environ(), "SHN_TEST_OBSERVER=1", "SHN_TEST_BASE="+base, "SHN_TEST_MARKER="+path, "SHN_TEST_KEY="+key, "GORACE=atexit_sleep_ms=0")
	out, err := cmd.CombinedOutput()
	got := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			got = e.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if got != want {
		t.Fatalf("probe exit=%d want=%d output=%s", got, want, out)
	}
}

// Virtual elapsed ticks inject both callers' schedules. At common offsets both
// real probe processes start before either is waited, so they can race on state.
func probeSchedule(t *testing.T, base, path, key string, ticks ...int) {
	t.Helper()
	tick := make(chan int, len(ticks))
	for _, second := range ticks {
		tick <- second
	}
	close(tick)
	for second := range tick {
		var commands []*exec.Cmd
		var outputs []*bytes.Buffer
		for _, interval := range []int{5, 15} {
			if second%interval != 0 {
				continue
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestObserverProcess$")
			cmd.Env = append(os.Environ(), "SHN_TEST_OBSERVER=1", "SHN_TEST_BASE="+base, "SHN_TEST_MARKER="+path, "SHN_TEST_KEY="+key, "GORACE=atexit_sleep_ms=0")
			output := new(bytes.Buffer)
			cmd.Stdout = output
			cmd.Stderr = output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			commands = append(commands, cmd)
			outputs = append(outputs, output)
		}
		for i, cmd := range commands {
			err := cmd.Wait()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Errorf("scheduled probe at %ds: err=%v output=%s", second, err, outputs[i])
			}
		}
	}
}
func TestWorkerSequentialAcrossIndependentObservers(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	release := make(chan struct{})
	entered := make(chan string, 42)
	var active, maxActive atomic.Int32
	lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
		}
		entered <- r.URL.Path
		if r.URL.Path == "/fhir/Bundle/$validate" {
			<-release
		}
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := markerPath(t)
	done := startWorker(t, ctx, lane.base(), path, sameJVM())
	if got := <-entered; got != "/fhir/Bundle/$validate" {
		t.Fatalf("first=%s", got)
	}
	st := readTestState(t, path)
	if st.State != "warming" || st.Row != "init-pas-request-bundle" || st.FiredAt.IsZero() || len(st.Warm) != 0 {
		t.Fatalf("in-flight state=%+v", st)
	}
	probeSchedule(t, lane.base(), path, sameJVM(), 5, 10, 15, 20, 30)
	if len(lane.recorded()) != 1 || maxActive.Load() != 1 {
		t.Fatal("overlap or repeated submission")
	}
	close(release)
	if err := finishWorker(t, done); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range lane.recorded() {
		got = append(got, p.path)
	}
	want := make([]string, 0, 42)
	for _, row := range readinessRows("2.2") {
		want = append(want, "/fhir/"+row.resourceType+"/$validate")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order=%v", got)
	}
	st = readTestState(t, path)
	if st.State != "ready" || !reflect.DeepEqual(st.Warm, readinessIdentities("2.2")) {
		t.Fatalf("ready state=%+v", st)
	}
	for i := 0; i < 3; i++ {
		observerProcess(t, lane.base(), path, sameJVM(), 0)
	}
	if len(lane.recorded()) != 42 || maxActive.Load() != 1 {
		t.Fatal("ready observers repeated validation")
	}
	for i, row := range readinessRows("2.2") {
		body, _ := fixtureBody(row)
		p := lane.recorded()[i]
		if p.profile != row.profile || string(p.body) != string(body) {
			t.Fatalf("wire fixture/profile mismatch for %s", row.identity)
		}
	}
}

// A lane that keeps giving the answer it gave while still warming (its real
// first answer, the SLICING_CANNOT_BE_EVALUATED errors) past the prime pass is
// not ready.
func TestVerdictReadinessRejectsPermanentPASSlicingFailure(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the hook answers the ClaimResponse rows and the corpus stops at the first qualification row
	slicing := primeSlicingAnswer22(t)
	lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/fhir/ClaimResponse/$validate" {
			return true
		}
		w.Header().Set("Content-Type", "application/fhir+json;charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(slicing)
		return false
	}
	marker := markerPath(t)
	done := startWorker(t, context.Background(), lane.base(), marker, sameJVM())
	if err := finishWorker(t, done); err == nil {
		t.Fatal("false PAS slicing verdict published readiness")
	}
	if got := check(lane.base(), envLine("2.2"), marker, time.Second, sameJVM); got != 1 {
		t.Fatalf("false verdict health exit = %d, want 1", got)
	}
	posts := lane.recorded()
	if len(posts) != 14 || posts[len(posts)-1].path != "/fhir/ClaimResponse/$validate" {
		t.Fatalf("worker did not stop at failing qualification row: %+v", posts)
	}
}

func TestVerdictReadinessRejectsDirtyQualificationPasses(t *testing.T) {
	for name, failingPost := range map[string]int{"second pass": 14, "third pass": 23} {
		t.Run(name, func(t *testing.T) {
			lane := newFakeLane(t, "2.2")
			lane.partial() // the hook answers the failing row and the corpus stops there
			slicing := primeSlicingAnswer22(t)
			var posts atomic.Int32
			lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
				if int(posts.Add(1)) != failingPost {
					return true
				}
				_, _ = w.Write(slicing)
				return false
			}
			marker := markerPath(t)
			if err := finishWorker(t, startWorker(t, context.Background(), lane.base(), marker, sameJVM())); err == nil {
				t.Fatal("dirty qualification pass published readiness")
			}
			state := readTestState(t, marker)
			wantRow := readinessRows("2.2")[failingPost-1].identity
			if len(lane.recorded()) != failingPost || len(state.Warm) != failingPost-1 || state.Row != wantRow || state.State != "failed" {
				t.Fatalf("failure was not terminal at %s: posts=%d state=%+v", wantRow, len(lane.recorded()), state)
			}
		})
	}
}

// A deadline that passes before the first post (here while the metadata probe
// is still refused) fails the worker as "metadata unavailable", terminally,
// without posting anything. The row's wait ends when the worker does: it would
// block for good on a wait that only a post can end.
func TestWorkerDeadlineBeforeFirstPostFailsWithoutPosting(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the worker ends before its first post, so no recorded answer is used
	atomic.StoreInt32(&lane.metadata, http.StatusServiceUnavailable)
	entered := make(chan struct{})
	var once sync.Once
	lane.validate = func(http.ResponseWriter, *http.Request) bool {
		once.Do(func() { close(entered) })
		return true
	}
	path := markerPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := startWorker(t, ctx, lane.base(), path, sameJVM())
	ended, err := awaitRequest(t, entered, done)
	if !ended || err == nil || err.Error() != "metadata unavailable" {
		t.Fatalf("ended=%v err=%v, want the worker to end with metadata unavailable before posting", ended, err)
	}
	if n := len(lane.recorded()); n != 0 {
		t.Fatalf("posted %d times, want none", n)
	}
	if st := readTestState(t, path); st.State != "failed" || st.Failure != "metadata unavailable" || len(st.Warm) != 0 {
		t.Fatalf("state %+v, want failed with metadata unavailable and nothing warm", st)
	}
}

func TestWorkerAmbiguousFailureNeverResubmits(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "disconnect", "redirect", "incomplete", "oversize", "non-outcome", "missing-code", "ambiguous-outcome", "publication"} {
		t.Run(mode, func(t *testing.T) {
			lane := newFakeLane(t, "2.2")
			lane.partial() // the first row fails; the recovery below runs against a fresh lane
			path := markerPath(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			// A failing row must still release the held handler, or the lane's
			// Close cleanup (registered earlier, so run later) waits on it.
			var releaseOnce sync.Once
			releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseHandler)
			var active atomic.Int32
			var enteredOnce sync.Once
			lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
				active.Add(1)
				defer active.Add(-1)
				enteredOnce.Do(func() { close(entered) })
				switch mode {
				case "cancel", "deadline":
					<-release
					return false
				case "disconnect":
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
				case "redirect":
					w.Header().Set("Location", lane.base()+"/Task/$validate")
					w.WriteHeader(307)
				case "incomplete":
					w.Header().Set("Content-Length", "500")
					fmt.Fprint(w, `{"resourceType":"OperationOutcome"}`)
				case "oversize":
					fmt.Fprint(w, `{"resourceType":"OperationOutcome"}`+strings.Repeat(" ", 1<<20))
				case "non-outcome":
					fmt.Fprint(w, `{"resourceType":"Bundle","secret":"do not log"}`)
				case "missing-code":
					fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"information"}]}`)
				case "ambiguous-outcome":
					fmt.Fprint(w, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","Severity":"information","code":"processing","diagnostics":"unrelated validation failure"}]}`)
				case "publication":
					os.Remove(path)
					os.Mkdir(path, 0700)
					return true
				}
				return false
			}
			var ctx context.Context
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadline := newExpiringContext()
			if mode == "deadline" {
				// The deadline passes while the post is in flight: see expiringContext.
				ctx = deadline
			}
			done := startWorker(t, ctx, lane.base(), path, sameJVM())
			mustReach(t, entered, done)
			switch mode {
			case "cancel":
				cancel()
			case "deadline":
				deadline.expire()
			}
			if err := finishWorker(t, done); err == nil {
				t.Fatal("ambiguous response credited")
			}
			if mode == "cancel" || mode == "deadline" {
				if active.Load() != 1 {
					t.Fatal("test lost abandoned active server request")
				}
			}
			probeSchedule(t, lane.base(), path, sameJVM(), 5, 15, 30)
			if len(lane.recorded()) != 1 {
				t.Fatalf("terminal failure generated %d posts", len(lane.recorded()))
			}
			if mode != "publication" {
				st := readTestState(t, path)
				if st.State != "failed" || st.Row != "init-pas-request-bundle" || len(st.Warm) != 0 {
					t.Fatalf("terminal state=%+v", st)
				}
			}
			releaseHandler()
			if mode == "publication" {
				os.Remove(path)
			}
			// A fresh JVM is the recovery boundary: a new incarnation's worker
			// warms a new lane, whose recording serves the whole corpus once.
			fresh := newFakeLane(t, "2.2")
			done = startWorker(t, context.Background(), fresh.base(), path, otherJVM())
			if err := finishWorker(t, done); err != nil {
				t.Fatal(err)
			}
			observerProcess(t, fresh.base(), path, otherJVM(), 0)
			observerProcess(t, fresh.base(), path, sameJVM(), 1)
			if len(lane.recorded()) != 1 {
				t.Fatalf("recovery re-posted to the failed lane: %d posts", len(lane.recorded()))
			}
		})
	}
}

func TestObserverRejectsInvalidState(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // observers ask only the recorded metadata
	ready := initialState(sameJVM())
	ready.State = "ready"
	ready.Warm = readinessIdentities("2.2")
	cases := map[string]func(*markerState){"schema": func(s *markerState) { s.Schema++ }, "line": func(s *markerState) { s.Line = "2.1" }, "incarnation": func(s *markerState) { s.Key = otherJVM() }, "empty-key": func(s *markerState) { s.Key = "" }, "partial": func(s *markerState) { s.Warm = s.Warm[:33] }, "duplicate": func(s *markerState) { s.Warm[33] = s.Warm[0] }, "reordered": func(s *markerState) { s.Warm[10], s.Warm[11] = s.Warm[11], s.Warm[10] }, "extra": func(s *markerState) { s.Warm = append(s.Warm, "extra") }, "inflight": func(s *markerState) { s.State = "warming"; s.Row = "negative-meta" }, "failed": func(s *markerState) { s.State = "failed"; s.Failure = "arbitrary secret\n" }, "unknown-state": func(s *markerState) { s.State = "secret" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st := ready
			st.Warm = append([]string{}, ready.Warm...)
			mutate(&st)
			path := markerPath(t)
			if err := writeState(path, st); err != nil {
				t.Fatal(err)
			}
			observerProcess(t, lane.base(), path, sameJVM(), 1)
		})
	}
	for _, raw := range []string{"", "warm\n", `{"schema":1`, strings.Repeat("x", 65537)} {
		path := markerPath(t)
		os.WriteFile(path, []byte(raw), 0600)
		observerProcess(t, lane.base(), path, sameJVM(), 1)
	}
	path := markerPath(t)
	writeState(path, ready)
	observerProcess(t, lane.base(), path, "", 1)
	observerProcess(t, lane.base(), path, sameJVM(), 0)
	if len(lane.recorded()) != 0 {
		t.Fatal("observer repaired invalid state")
	}
}

func TestWorkerPublicationAndIdentityFailuresPreventDispatch(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // nothing is dispatched
	for _, tc := range []struct {
		name, key string
		badPath   bool
	}{{"empty identity", "", false}, {"unwritable", sameJVM(), true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := markerPath(t)
			if tc.badPath {
				path += "/missing/warm"
			}
			if err := runWarmup(context.Background(), lane.base(), path, initialState(tc.key)); err == nil {
				t.Fatal("invalid startup accepted")
			}
			if len(lane.recorded()) != 0 {
				t.Fatal("dispatched without state/identity")
			}
		})
	}
}

// The lane's real answer to the first row, with a status it never sent for
// it: the status alone fails the row.
func TestWorkerRejectsWrongOutcomeStatus(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the hook answers the first row and the corpus stops there
	answer := recordedAnswer(t, readinessRows("2.2")[0], false)
	lane.validate = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(422)
		_, _ = w.Write(answer)
		return false
	}
	path := markerPath(t)
	if err := finishWorker(t, startWorker(t, context.Background(), lane.base(), path, sameJVM())); err == nil {
		t.Fatal("wrong HTTP status accepted")
	}
	observerProcess(t, lane.base(), path, sameJVM(), 1)
}

func TestWorkerConsumesCompleteResponseBeforeNextRow(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the hook answers the first row itself
	answer := recordedAnswer(t, readinessRows("2.2")[0], false)
	entered := make(chan struct{})
	release := make(chan struct{})
	lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/fhir/Bundle/$validate" || len(lane.recorded()) != 1 {
			return true
		}
		// The lane's real answer, then a held trailing byte.
		_, _ = w.Write(answer)
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		fmt.Fprint(w, "\n")
		return false
	}
	path := markerPath(t)
	done := startWorker(t, context.Background(), lane.base(), path, sameJVM())
	mustReach(t, entered, done)
	observerProcess(t, lane.base(), path, sameJVM(), 1)
	if len(lane.recorded()) != 1 || len(readTestState(t, path).Warm) != 0 {
		t.Fatal("credited before EOF")
	}
	close(release)
	if err := finishWorker(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLaterFailurePreservesCompletedPrefix(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the second row hangs and the run is canceled
	entered := make(chan struct{})
	release := make(chan struct{})
	lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/fhir/QuestionnaireResponse/$validate" {
			close(entered)
			<-release
			return false
		}
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := markerPath(t)
	done := startWorker(t, ctx, lane.base(), path, sameJVM())
	mustReach(t, entered, done)
	st := readTestState(t, path)
	if st.Row != "init-dtr-questionnaireresponse" || !reflect.DeepEqual(st.Warm, []string{"init-pas-request-bundle"}) {
		t.Fatalf("partial state=%+v", st)
	}
	cancel()
	if err := finishWorker(t, done); err == nil {
		t.Fatal("cancellation accepted")
	}
	probeSchedule(t, lane.base(), path, sameJVM(), 5, 15, 30)
	st = readTestState(t, path)
	if len(lane.recorded()) != 2 || !reflect.DeepEqual(st.Warm, []string{"init-pas-request-bundle"}) || st.State != "failed" {
		t.Fatalf("lost/repeated completed progress: %+v posts=%d", st, len(lane.recorded()))
	}
	close(release)
}

func TestWorkerWaitsForMetadataAndBoundsStartup(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	atomic.StoreInt32(&lane.metadata, 503)
	ctx, cancel := context.WithCancel(context.Background())
	path := markerPath(t)
	done := startWorker(t, ctx, lane.base(), path, sameJVM())
	observerProcess(t, lane.base(), path, sameJVM(), 1)
	if len(lane.recorded()) != 0 {
		t.Fatal("POST before metadata")
	}
	cancel()
	if finishWorker(t, done) == nil {
		t.Fatal("unavailable metadata accepted")
	}
	if len(lane.recorded()) != 0 || readTestState(t, path).State != "failed" {
		t.Fatal("metadata cancellation dispatched or lost failure")
	}
	atomic.StoreInt32(&lane.metadata, http.StatusOK) // back to the recorded answer
	path = markerPath(t)
	if err := finishWorker(t, startWorker(t, context.Background(), lane.base(), path, otherJVM())); err != nil {
		t.Fatal(err)
	}
}

func TestStatePublicationIsAtomicForConcurrentReaders(t *testing.T) {
	path := markerPath(t)
	st := initialState(sameJVM())
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			next := st
			if i%2 == 0 {
				next.State = "ready"
				next.Warm = readinessIdentities("2.2")
			}
			if err := writeState(path, next); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		got, err := readState(path, "2.2", sameJVM())
		if err != nil {
			t.Fatalf("reader observed partial replacement: %v", err)
		}
		if got.State != "ready" && got.State != "waiting-for-metadata" {
			t.Fatalf("torn state=%+v", got)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIncarnationRejectsUnavailableOrInvalidIdentity(t *testing.T) {
	stat := "1 (java) S 0 1 1 0 -1 4194560 1234 0 0 0 55 7 0 0 20 0 41 0 987654"
	for _, boot := range []string{"", "\n", "boot\nsecret", "boot:secret"} {
		if got := incarnationFrom(stat, boot); got != "" {
			t.Fatalf("invalid boot accepted: %q", got)
		}
	}
	for _, start := range []string{"zero", "0", "-1"} {
		if got := incarnationFrom(strings.Replace(stat, "987654", start, 1), "boot"); got != "" {
			t.Fatalf("invalid start time accepted: %q", got)
		}
	}
}

func TestObserverDoesNotReflectStateDiagnostics(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the observer asks only the recorded metadata
	path := markerPath(t)
	st := initialState(sameJVM())
	st.State = "failed"
	st.Failure = "private-response-body\nforged-log"
	writeState(path, st)
	cmd := exec.Command(os.Args[0], "-test.run=^TestObserverProcess$")
	cmd.Env = append(os.Environ(), "SHN_TEST_OBSERVER=1", "SHN_TEST_BASE="+lane.base(), "SHN_TEST_MARKER="+path, "SHN_TEST_KEY="+sameJVM())
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "private-response-body") || strings.Contains(string(out), "forged-log") || len(out) > 512 {
		t.Fatalf("unsafe observer diagnostic: %q err=%v", out, err)
	}
}

func TestWorkerFailureLogIsBoundedAndIncludesElapsed(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	lane.partial() // the hook answers the first row and the corpus stops there
	lane.validate = func(w http.ResponseWriter, _ *http.Request) bool {
		fmt.Fprint(w, `{"resourceType":"Bundle","diagnostics":"private-response-body"}`)
		return false
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = writer
	path := markerPath(t)
	workerErr := finishWorker(t, startWorker(t, context.Background(), lane.base(), path, sameJVM()))
	os.Stderr = old
	writer.Close()
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if workerErr == nil {
		t.Fatal("non-outcome accepted")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	failure := lines[len(lines)-1]
	if !strings.Contains(failure, "elapsed=") || !strings.Contains(failure, "row=init-pas-request-bundle") || !strings.Contains(failure, "outcome=failed") || strings.Contains(string(raw), "private-response-body") || len(raw) > 512 {
		t.Fatalf("failure lacks bounded timing diagnostic: %s", raw)
	}
}

func TestWorkerMetadataPollingRecoversWithoutEarlyPost(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	first := make(chan struct{})
	var gets atomic.Int32
	// The first /metadata answers 503 (a lane not yet up); later ones, and
	// every validation, are the lane's recorded answers.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/metadata" && gets.Add(1) == 1 {
			close(first)
			w.WriteHeader(503)
			return
		}
		lane.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := startWorker(t, ctx, server.URL+"/fhir", markerPath(t), sameJVM())
	mustReach(t, first, done)
	if len(lane.recorded()) != 0 {
		t.Fatal("validation started before successful metadata")
	}
	if err := finishWorker(t, done); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 || len(lane.recorded()) != 42 {
		t.Fatalf("gets=%d posts=%d", gets.Load(), len(lane.recorded()))
	}
}

func TestWorkerMetadataEOFPrecedesValidation(t *testing.T) {
	lane := newFakeLane(t, "2.2")
	capability := recordedMetadata(t, "2.2")
	entered := make(chan struct{})
	release := make(chan struct{})
	// /metadata sends the lane's recorded answer and then holds a trailing byte.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/metadata" {
			w.Header().Set("Content-Type", capability.contentType)
			_, _ = w.Write(capability.answer)
			w.(http.Flusher).Flush()
			close(entered)
			<-release
			fmt.Fprint(w, "\n")
			return
		}
		lane.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := markerPath(t)
	done := startWorker(t, ctx, server.URL+"/fhir", path, sameJVM())
	mustReach(t, entered, done)
	if len(lane.recorded()) != 0 || readTestState(t, path).State != "waiting-for-metadata" {
		t.Fatal("validation started before metadata EOF")
	}
	close(release)
	if err := finishWorker(t, done); err != nil {
		t.Fatal(err)
	}
}

// A real loaded 2.2 lane serves a CapabilityStatement of about 2 MB.
// Metadata must not inherit the smaller OperationOutcome bound or warming
// never starts. The recording's scrub replaced the resource listing that makes
// it large, so this answer is the recorded one with a stand-in listing padded
// to the captured size.
func TestLoadedLaneMetadataAllowsWarmupAndReadiness(t *testing.T) {
	measuredBytes := measuredMetadataBytes(t, "2.2")
	if measuredBytes <= maxOutcomeBytes {
		t.Fatalf("recorded metadata is %d bytes; it must exceed the %d-byte OperationOutcome bound", measuredBytes, maxOutcomeBytes)
	}
	capability := recordedMetadata(t, "2.2")
	const open, closing = `"rest":[{"mode":"server","documentation":"`, `"}]`
	pad := measuredBytes - len(capability.answer) - len(open) - len(closing) + len(`"rest":[]`)
	body := string(replaceOnce(t, capability.answer, `"rest":[]`, open+strings.Repeat("x", pad)+closing))
	if len(body) != measuredBytes {
		t.Fatalf("padded metadata is %d bytes, want %d", len(body), measuredBytes)
	}
	lane := newFakeLane(t, "2.2")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fhir/metadata" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", capability.contentType)
			fmt.Fprint(w, body)
			return
		}
		lane.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	base := server.URL + "/fhir"
	t.Run("metadata", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := metadata(ctx, httpClient(), base+"/metadata"); err != nil {
			t.Fatalf("complete measured CapabilityStatement rejected: %v", err)
		}
	})
	t.Run("worker", func(t *testing.T) {
		// This checks the complete 42-row worker after large metadata,
		// not subsecond throughput on a contended image-build runner.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		path := markerPath(t)
		if err := finishWorker(t, startWorker(t, ctx, base, path, sameJVM())); err != nil {
			t.Fatalf("measured metadata prevented warm-up: %v; posts=%d", err, len(lane.recorded()))
		}
		if len(lane.recorded()) != 42 || readTestState(t, path).State != "ready" {
			t.Fatalf("metadata did not unlock readiness rows: posts=%d", len(lane.recorded()))
		}
	})
	t.Run("observer", func(t *testing.T) {
		path := markerPath(t)
		st := initialState(sameJVM())
		st.State = "ready"
		st.Warm = readinessIdentities("2.2")
		if err := writeState(path, st); err != nil {
			t.Fatal(err)
		}
		before := len(lane.recorded())
		observerProcess(t, base, path, sameJVM(), 0)
		if len(lane.recorded()) != before {
			t.Fatal("observer submitted validation")
		}
	})
}
