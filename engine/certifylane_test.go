package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func gatedTestUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"All OK"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loopStopped fails the row within seconds if the client's loop is still
// running, instead of hanging the package on a join.
func loopStopped(t *testing.T, v *GatedCertificationValidator) {
	t.Helper()
	done := make(chan struct{})
	go func() { v.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the qualification loop is still running")
	}
}

// newFastGated builds a gated client whose own schedule is milliseconds, so
// the rows below observe the loop rather than wait on it.
func newFastGated(lane *DiscoveredLane, qualify LaneQualifier, reason string) *GatedCertificationValidator {
	v := &GatedCertificationValidator{
		lane: lane, client: NewCertificationOperationValidator(lane.Base()), qualify: qualify, reason: reason,
		now: time.Now, delay: 5 * time.Millisecond, interval: 5 * time.Millisecond,
		eager: time.Hour, idle: time.Hour,
	}
	v.ctx, v.cancel = context.WithCancel(context.Background())
	if qualify != nil {
		v.wg.Add(1)
		go v.loop()
	}
	return v
}

// The client qualifies in the background, never on the exchange: while an
// attempt is blocked the exchange answers "pending" at once and starts nothing;
// the first exchange after the attempt succeeds certifies through the client,
// once; routing's lane stays untouched and the loop stops.
func TestGatedCertificationQualifiesInBackgroundNeverOnTheExchange(t *testing.T) {
	var hits int32
	upstream := gatedTestUpstream(t, &hits)
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	var attempts int32
	qualifier := func(ctx context.Context, base, line string) error {
		atomic.AddInt32(&attempts, 1)
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lane := NewDiscoveredLane("2.2", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	v := newFastGated(lane, qualifier, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured")
	defer v.Close()

	<-entered // the loop's first attempt is inside the (blocked) qualifier
	started := time.Now()
	_, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
	if elapsed := time.Since(started); elapsed > certificationCandidateTimeout {
		t.Fatalf("the exchange waited %s on qualification; it must answer within the candidate bound", elapsed)
	}
	var unavailable *CertificationLaneUnavailable
	if !errors.As(err, &unavailable) || err.Error() != "certification validator unavailable: FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured; default lane qualification pending" {
		t.Fatalf("exchange during qualification: err = %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts while one is blocked = %d, want 1 (exchanges start none)", got)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("the upstream was dialed before qualification")
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
		if err == nil {
			if !res.Valid || atomic.LoadInt32(&hits) != 1 {
				t.Fatalf("after qualification: res=%+v hits=%d", res, hits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never certified after the attempt succeeded: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lane.Ready() {
		t.Fatal("the certification client's own qualification changed routing's lane readiness")
	}
	loopStopped(t, v) // the loop stopped on success
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts after success = %d, want 1 (the loop stops)", got)
	}
}

// A validator that comes up late is qualified within one interval of coming up
// without any exchange: the loop keeps retrying, the reason reads "failed,
// retrying" meanwhile, and the exchange after the first success certifies.
func TestGatedCertificationRetriesUntilTheValidatorComesUp(t *testing.T) {
	var hits int32
	upstream := gatedTestUpstream(t, &hits)
	var up atomic.Bool
	var attempts int32
	qualifier := func(context.Context, string, string) error {
		atomic.AddInt32(&attempts, 1)
		if up.Load() {
			return nil
		}
		return errors.New("validator not up")
	}
	lane := NewDiscoveredLane("2.1", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	v := newFastGated(lane, qualifier, "FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured")
	defer v.Close()

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&attempts) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the loop did not keep retrying while the validator was down")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := v.Validate(context.Background(), []byte(`{}`), "profile"); err == nil || !strings.HasSuffix(err.Error(), "default lane qualification failed, retrying") {
		t.Fatalf("while down: err = %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("the upstream was dialed while unqualified")
	}

	up.Store(true)
	for {
		res, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
		if err == nil {
			if !res.Valid || atomic.LoadInt32(&hits) != 1 {
				t.Fatalf("after the validator came up: res=%+v hits=%d", res, hits)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not certified after the validator came up: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lane.Ready() {
		t.Fatal("routing's lane readiness changed")
	}
}

// Closing the clients stops a gated loop that would otherwise run for the
// process: what the worker's shutdown, a failed engine construction and
// disabled certification all rely on.
func TestCloseCertificationClientsStopsGatedLoops(t *testing.T) {
	var hits int32
	upstream := gatedTestUpstream(t, &hits)
	block := make(chan struct{})
	defer close(block)
	qualifier := func(ctx context.Context, _, _ string) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lane := NewDiscoveredLane("2.2", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	v := newFastGated(lane, qualifier, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured")
	CloseCertificationClients(map[string]shnsdk.Validator{"2.2": v, "2.0": shnsdk.NewOperationValidator(upstream.URL + "/fhir")})
	loopStopped(t, v)
	if v.ctx.Err() == nil {
		t.Fatal("the gated client's context was not cancelled")
	}
}

// A lane routing has already qualified needs no attempt of this client's own:
// the loop sees it ready and stops, and the exchange certifies.
func TestGatedCertificationHonoursRoutingReadiness(t *testing.T) {
	var hits int32
	upstream := gatedTestUpstream(t, &hits)
	var attempts int32
	never := func(context.Context, string, string) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("not mine to answer")
	}
	lane := NewDiscoveredLane("2.2", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	v := newFastGated(lane, never, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured")
	defer v.Close()
	res, err := v.Validate(context.Background(), []byte(`{"resourceType":"Claim"}`), "profile")
	if err != nil || !res.Valid || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("with routing's lane ready: res=%+v err=%v hits=%d", res, err, hits)
	}
	loopStopped(t, v)
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Fatalf("the client attempted %d qualifications of a lane routing had already qualified", got)
	}
}

// A failed certification-lane qualification names the host it dialed and why,
// never a bare "failed" and never the qualifier's error text.
func TestCertificationQualificationLogNamesHostAndReason(t *testing.T) {
	var out strings.Builder
	var mu sync.Mutex
	old := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(p) }))
	defer log.SetOutput(old)
	missing := &net.DNSError{Err: "private resolver detail", Name: "shn-validator-2-1", IsNotFound: true}
	lane := NewDiscoveredLane("2.1", "http://shn-validator-2-1:8080/fhir", shnsdk.NewOperationValidator("http://shn-validator-2-1:8080/fhir"))
	v := newFastGated(lane, func(context.Context, string, string) error { return fmt.Errorf("metadata unavailable: %w", missing) }, "FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		logged := out.String()
		mu.Unlock()
		if strings.Contains(logged, "certification_lane_qualification") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no qualification event")
		}
		time.Sleep(5 * time.Millisecond)
	}
	v.Close()
	mu.Lock()
	logged := out.String()
	mu.Unlock()
	_, raw, _ := strings.Cut(logged, "gateway: certification_lane_qualification ")
	var event struct {
		Host, State, Reason string
	}
	if err := json.Unmarshal([]byte(strings.SplitN(raw, "\n", 2)[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Host != "shn-validator-2-1" || event.State != "failed" || event.Reason != "name does not resolve" {
		t.Fatalf("event = %+v", event)
	}
	if strings.Contains(logged, "private resolver detail") {
		t.Fatal("the qualifier's error text reached the log")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
