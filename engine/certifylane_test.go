package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// gatedTestUpstream is a real validator lane of line, replayed strictly
// (testdata/recordings/lane-<line>-certify-literal.json) and counting the
// requests it is asked, with the errors it answers the literal Claim against
// the profile "profile": the lane finds it invalid. A row certifies that request
// at most once (its hits say so); the lane's answer with no profile is not asked.
func gatedTestUpstream(t *testing.T, line string, hits *int32) (*httptest.Server, []string) {
	t.Helper()
	rec := certifyRecording(t, "lane-"+line+"-certify-literal")
	rec.Subset() // the no-profile request is not asked; hits bound the other
	_, _, verdict := recordedAnswer(t, rec, "profile", literalClaim)
	return countingLane(t, rec, hits), verdict
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
	v := newGatedCertification(lane, NewCertificationOperationValidator(lane.Base()), qualify, reason)
	v.pending, v.failing, v.requalifying = "default lane qualification pending", "default lane qualification failed, retrying", "default lane re-qualifying"
	v.delay, v.interval, v.eager, v.idle = 5*time.Millisecond, 5*time.Millisecond, time.Hour, time.Hour
	v.Start()
	return v
}

// The client qualifies in the background, never on the exchange: while an
// attempt is blocked the exchange answers "pending" at once and starts nothing;
// the first exchange after the attempt succeeds certifies through the client,
// once; routing's lane stays untouched and the loop stops.
func TestGatedCertificationQualifiesInBackgroundNeverOnTheExchange(t *testing.T) {
	var hits int32
	upstream, verdict := gatedTestUpstream(t, "2.2", &hits)
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
	_, err := v.Validate(context.Background(), literalClaim, "profile")
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
		res, err := v.Validate(context.Background(), literalClaim, "profile")
		if err == nil {
			if res.Valid || !reflect.DeepEqual(res.Issues, verdict) || atomic.LoadInt32(&hits) != 1 {
				t.Fatalf("after qualification: res=%+v hits=%d, want the lane's invalid verdict from one request", res, hits)
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
	upstream, verdict := gatedTestUpstream(t, "2.1", &hits)
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
		res, err := v.Validate(context.Background(), literalClaim, "profile")
		if err == nil {
			if res.Valid || !reflect.DeepEqual(res.Issues, verdict) || atomic.LoadInt32(&hits) != 1 {
				t.Fatalf("after the validator came up: res=%+v hits=%d, want the lane's invalid verdict from one request", res, hits)
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
	upstream, _ := gatedTestUpstream(t, "2.2", &hits)
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
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("a closed client dialed the lane")
	}
}

// A lane routing has already qualified needs no attempt of this client's own:
// the loop sees it ready and stops, and the exchange certifies.
func TestGatedCertificationHonoursRoutingReadiness(t *testing.T) {
	var hits int32
	upstream, verdict := gatedTestUpstream(t, "2.2", &hits)
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
	res, err := v.Validate(context.Background(), literalClaim, "profile")
	if err != nil || res.Valid || !reflect.DeepEqual(res.Issues, verdict) || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("with routing's lane ready: res=%+v err=%v hits=%d", res, err, hits)
	}
	loopStopped(t, v)
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Fatalf("the client attempted %d qualifications of a lane routing had already qualified", got)
	}
}

// Once qualified, the gated client certifies a real lane's clean answer: the
// 2.1 lane's answer to the versioned approved ClaimResponse (warnings, no
// error), asked with the request the lane recorded.
func TestGatedCertificationCertifiesARealCleanAnswer(t *testing.T) {
	rec, payload, profile, answers := recordedClaimResponseRow(t, "2.1")
	if len(answers) == 0 || bytes.Contains(answers[0].Response.Body, []byte(`"severity":"error"`)) {
		t.Fatal("the 2.1 lane's first recorded answer is no longer clean")
	}
	var hits int32
	upstream := countingLane(t, rec, &hits)
	lane := NewDiscoveredLane("2.1", upstream.URL+"/fhir", shnsdk.NewOperationValidator(upstream.URL+"/fhir"))
	if err := lane.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	v := newFastGated(lane, nil, "FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured")
	defer v.Close()
	res, err := v.Validate(context.Background(), payload, profile)
	if err != nil || !res.Valid || len(res.Issues) != 0 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("gated client: res=%+v err=%v hits=%d, want the lane's clean verdict from one request", res, err, hits)
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

// waitQualified waits for the gated client's own qualification to pass.
func waitQualified(t *testing.T, v *GatedCertificationValidator) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v.mu.Lock()
		ready := v.ready
		v.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lane never qualified")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// laneVerdict is the verdict e records for line.
func laneVerdict(t *testing.T, e CertificationEvidence, line string) LaneVerdict {
	t.Helper()
	for _, v := range e.Verdicts {
		if v.Line == line {
			return v
		}
	}
	t.Fatalf("no %s verdict in %+v", line, e)
	return LaneVerdict{}
}

// slicingErrors are the lane's SLICING_CANNOT_BE_EVALUATED errors in a
// recorded answer.
func slicingErrors(t *testing.T, answer []byte) outcomeIssues {
	t.Helper()
	var outcome struct {
		Issue outcomeIssues `json:"issue"`
	}
	if err := json.Unmarshal(answer, &outcome); err != nil {
		t.Fatal(err)
	}
	var out outcomeIssues
	for _, issue := range outcome.Issue {
		if issue["severity"] == "error" && issueMessageID(issue) == "SLICING_CANNOT_BE_EVALUATED" {
			out = append(out, issue)
		}
	}
	return out
}

// A lane configured by address certifies only once it has passed the
// qualification a default lane passes. The lane is the real 2.2 lane,
// replayed cold (../internal/lanequalify/testdata/recordings/lane-2.2-warm.json):
// the first time it is asked about the versioned approved ClaimResponse it is
// still warming and answers three SLICING_CANNOT_BE_EVALUATED errors, and
// afterwards only warnings. Before qualification the collector records the
// line unavailable, "lane not qualified", and dials nothing, so the warming
// answer can never become an invalid verdict. The qualification is the real
// readiness corpus, which meets and absorbs the warming answer; afterwards the
// collector meets the lane's settled answer and certifies the line.
func TestQualifiedCertificationNeverRecordsAWarmingAnswer(t *testing.T) {
	rec, payload, profile, answers := recordedClaimResponseRow(t, "2.2")
	if len(answers) < 2 || len(slicingErrors(t, answers[0].Response.Body)) != 3 {
		t.Fatal("the 2.2 lane's first recorded answer is no longer the warming one with three slicing errors")
	}
	var hits int32
	lane := countingLane(t, rec, &hits)
	release := make(chan struct{})
	qualifier := func(ctx context.Context, base, line string) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return lanequalify.Warm(ctx, base, line, nil)
	}
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), qualifier)
	defer v.Close()
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.2": v}, nil)

	certificationSubmitPayload(g, "warming", payload)
	certificationFlush(t, g)
	got := laneVerdict(t, g.CertificationEvidenceForTest()[0], "2.2")
	if got.State != "unavailable" || got.Valid || got.Error != "certification validator unavailable: lane not qualified; qualification pending" || got.Profile != profile {
		t.Fatalf("before qualification: %+v, want unavailable, lane not qualified", got)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("an unqualified lane was dialed")
	}

	close(release)
	waitQualified(t, v)
	if got := atomic.LoadInt32(&hits); int(got) != lanequalify.RowCount("2.2") {
		t.Fatalf("qualification asked %d requests, want the %d-row corpus", got, lanequalify.RowCount("2.2"))
	}
	certificationSubmitPayload(g, "settled", payload)
	certificationFlush(t, g)
	e := g.CertificationEvidenceForTest()[1]
	if got := laneVerdict(t, e, "2.2"); got.State != "valid" || !got.Valid || !slices.Equal(e.Certified, []string{"2.2"}) {
		t.Fatalf("after qualification: %+v certified %v, want the lane's settled clean answer certified", got, e.Certified)
	}
	if v.lane.Ready() {
		t.Fatal("the certification client's own lane became ready outside its own qualification")
	}
}

// On a lane that has passed qualification, SLICING_CANNOT_BE_EVALUATED is what
// the lane found: it may be a real slicing problem in the payload, so it is a
// verdict, invalid. The base is the 2.2 lane's settled answer to the versioned
// approved ClaimResponse (warnings only: valid); the mutation adds the three
// slicing errors the same lane answered while warming.
func TestQualifiedCertificationRecordsASlicingAnswerInvalid(t *testing.T) {
	_, payload, _, answers := recordedClaimResponseRow(t, "2.2")
	var settled struct {
		Issue outcomeIssues `json:"issue"`
	}
	if err := json.Unmarshal(answers[len(answers)-1].Response.Body, &settled); err != nil {
		t.Fatal(err)
	}
	slicing := slicingErrors(t, answers[0].Response.Body)
	if len(slicing) != 3 {
		t.Fatal("the warming answer no longer carries three slicing errors")
	}
	for name, row := range map[string]struct {
		issues outcomeIssues
		state  string
	}{
		"settled":           {settled.Issue, "valid"},
		"settled+slicing":   {append(settled.Issue.clone(t), slicing.clone(t)...), "invalid"},
		"one slicing error": {append(settled.Issue.clone(t), slicing.clone(t)[0]), "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			served := serveOutcome(t, row.issues)
			v := NewQualifiedCertificationValidator("2.2", served, func(context.Context, string, string) error { return nil })
			defer v.Close()
			v.Start()
			waitQualified(t, v)
			g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.2": v}, nil)
			certificationSubmitPayload(g, name, payload)
			certificationFlush(t, g)
			e := g.CertificationEvidenceForTest()[0]
			got := laneVerdict(t, e, "2.2")
			if got.State != row.state || got.Valid != (row.state == "valid") || got.Error != "" || slices.Contains(e.Certified, "2.2") != (row.state == "valid") {
				t.Fatalf("%+v certified %v, want %s", got, e.Certified, row.state)
			}
		})
	}
}

// Lane answers a faultLane gives: the lane's recorded answer; a recorded
// invalid answer (at 200, a verdict); and three authored faults: the lane
// closes the connection without answering (a lane that stopped: a process
// going away mid-request), answers 503 with no body (an answer, from the lane
// or something in front of it), or holds the request until the caller gives
// up.
const (
	laneRecorded int32 = iota
	laneInvalid
	laneDown
	laneServerError
	laneHeld
)

// faultLane answers each request as mode says; hits counts every request.
func faultLane(t *testing.T, rec *testrecord.Recording, invalid []byte, hits, mode *int32) *httptest.Server {
	t.Helper()
	replay := rec.Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		switch atomic.LoadInt32(mode) {
		case laneDown:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case laneServerError:
			w.WriteHeader(http.StatusServiceUnavailable)
		case laneHeld:
			// The body is read first: only then does the server notice the
			// caller closing the connection.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
				t.Error("a held request was never abandoned by its caller")
			}
		case laneInvalid:
			w.Header().Set("Content-Type", "application/fhir+json;charset=UTF-8")
			_, _ = w.Write(invalid)
		default:
			replay.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A qualified lane that stops answering at the connection may have restarted
// and be warming again: the failure clears the qualification and the client
// qualifies again at once. That certification, and every one until the new
// attempt passes, is unavailable naming the re-qualification, and nothing is
// dialed for them; once it passes, the lane certifies again. A verdict never
// clears the qualification: neither the lane's recorded clean answer nor a
// recorded invalid one (the 2.2 lane's warming answer, served at 200). The
// lane is the real 2.1 lane
// (../internal/lanequalify/testdata/recordings/lane-2.1-warm.json); the closed
// connection is authored.
func TestQualifiedCertificationRequalifiesAfterALaneFailure(t *testing.T) {
	rec, payload, _, _ := recordedClaimResponseRow(t, "2.1")
	_, _, _, warming := recordedClaimResponseRow(t, "2.2")
	var hits, mode int32
	lane := faultLane(t, rec, warming[0].Response.Body, &hits, &mode)
	var attempts int32
	gate := make(chan struct{}, 8)
	gate <- struct{}{} // the first attempt passes at once
	v := NewQualifiedCertificationValidator("2.1", NewCertificationOperationValidator(lane.URL+"/fhir"), func(ctx context.Context, _, _ string) error {
		atomic.AddInt32(&attempts, 1)
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	defer v.Close()
	v.Start()
	waitQualified(t, v)
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.1": v}, nil)
	certify := func(id string) LaneVerdict {
		t.Helper()
		certificationSubmitPayload(g, id, payload)
		certificationFlush(t, g)
		records := g.CertificationEvidenceForTest()
		return laneVerdict(t, records[len(records)-1], "2.1")
	}

	// Verdicts, valid and invalid, keep the qualification.
	if got := certify("valid"); got.State != "valid" {
		t.Fatalf("qualified lane: %+v, want valid", got)
	}
	atomic.StoreInt32(&mode, laneInvalid)
	if got := certify("invalid"); got.State != "invalid" {
		t.Fatalf("a recorded invalid answer: %+v, want invalid", got)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 || atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("after two verdicts: attempts=%d hits=%d, want 1 and 2", got, hits)
	}

	// The lane closes the connection: that certification is unavailable, and
	// the client re-qualifies.
	atomic.StoreInt32(&mode, laneDown)
	const requalifying = "certification validator unavailable: lane not qualified; re-qualifying"
	if got := certify("down"); got.State != "unavailable" || got.Error != requalifying {
		t.Fatalf("a failed answer: %+v, want unavailable %q", got, requalifying)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("hits=%d, want the one failed request", hits)
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&attempts) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the failure started no qualification")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// While re-qualifying (the attempt is held), nothing is dialed, even with
	// the lane back up.
	atomic.StoreInt32(&mode, laneRecorded)
	if got := certify("requalifying"); got.State != "unavailable" || got.Error != requalifying {
		t.Fatalf("while re-qualifying: %+v, want unavailable %q", got, requalifying)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("hits=%d: a lane being re-qualified was dialed", hits)
	}
	gate <- struct{}{}
	waitQualified(t, v)
	if got := certify("requalified"); got.State != "valid" || atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("after re-qualification: %+v hits=%d, want valid from one request", got, hits)
	}
}

// A default lane that routing qualified is used on routing's readiness until
// it stops answering at the connection; then routing's readiness no longer counts for this
// client, which qualifies the lane again itself. Routing's lane is untouched.
func TestGatedCertificationRequalifiesARoutingLaneAfterAFailure(t *testing.T) {
	rec, payload, profile, _ := recordedClaimResponseRow(t, "2.1")
	var hits, mode int32
	lane := faultLane(t, rec, nil, &hits, &mode)
	routing := NewDiscoveredLane("2.1", lane.URL+"/fhir", shnsdk.NewOperationValidator(lane.URL+"/fhir"))
	if err := routing.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var attempts int32
	v := newFastGated(routing, func(ctx context.Context, _, _ string) error {
		atomic.AddInt32(&attempts, 1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, "FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured")
	defer v.Close()
	if res, err := v.Validate(context.Background(), payload, profile); err != nil || !res.Valid {
		t.Fatalf("routing-qualified lane: %+v %v", res, err)
	}
	loopStopped(t, v) // routing's readiness stopped the client's own loop
	atomic.StoreInt32(&mode, laneDown)
	const want = "certification validator unavailable: FHIR_CERTIFY_URL_2_1 and FHIR_VALIDATE_URL_2_1 are not configured; default lane re-qualifying"
	for i := range 2 {
		_, err := v.Validate(context.Background(), payload, profile)
		var unavailable *CertificationLaneUnavailable
		if !errors.As(err, &unavailable) || err.Error() != want {
			t.Fatalf("certification %d after the failure: %v, want %q", i, err, want)
		}
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits=%d: the lane was dialed again before it re-qualified", hits)
	}
	if !routing.Ready() {
		t.Fatal("the certification client changed routing's lane readiness")
	}
	atomic.StoreInt32(&mode, laneRecorded)
	close(release)
	waitQualified(t, v)
	if res, err := v.Validate(context.Background(), payload, profile); err != nil || !res.Valid || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("after its own qualification: %+v %v attempts=%d", res, err, attempts)
	}
}

// qualifiedFaultLane is a 2.1 address client over a faultLane, qualified by
// its first attempt; each later attempt waits on gate. minRequalify is set by
// the row.
func qualifiedFaultLane(t *testing.T, client func(url string) *shnsdk.OperationValidator, minRequalify time.Duration) (v *GatedCertificationValidator, payload, profile []byte, hits, mode, attempts *int32, gate chan struct{}) {
	t.Helper()
	rec, body, prof, _ := recordedClaimResponseRow(t, "2.1")
	hits, mode, attempts = new(int32), new(int32), new(int32)
	lane := faultLane(t, rec, nil, hits, mode)
	gate = make(chan struct{}, 8)
	gate <- struct{}{}
	v = NewQualifiedCertificationValidator("2.1", client(lane.URL+"/fhir"), func(ctx context.Context, _, _ string) error {
		atomic.AddInt32(attempts, 1)
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	v.minRequalify = minRequalify // set before Start: no loop runs yet
	t.Cleanup(v.Close)
	v.Start()
	waitQualified(t, v)
	return v, body, []byte(prof), hits, mode, attempts, gate
}

// A lane that answers, whatever it answers, has not stopped: a 5xx marks only
// that certification unavailable (the error the collector records hashed), the
// lane stays qualified, starts no qualification, and certifies the next
// payload. So does a caller whose own deadline ends while the lane is still
// working: that says nothing about the lane.
func TestQualifiedCertificationKeepsQualificationWhenTheLaneAnswers(t *testing.T) {
	v, payload, profile, hits, mode, attempts, _ := qualifiedFaultLane(t, NewCertificationOperationValidator, time.Hour)
	var unavailable *CertificationLaneUnavailable

	atomic.StoreInt32(mode, laneServerError)
	if _, err := v.Validate(context.Background(), payload, string(profile)); err == nil || errors.As(err, &unavailable) {
		t.Fatalf("a 503: err = %v, want the execution failure itself, not a lane re-qualification", err)
	}

	atomic.StoreInt32(mode, laneHeld)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := v.Validate(ctx, payload, string(profile))
	cancel()
	if err == nil || errors.As(err, &unavailable) {
		t.Fatalf("the caller's deadline: err = %v, want the deadline itself", err)
	}

	atomic.StoreInt32(mode, laneRecorded)
	if res, err := v.Validate(context.Background(), payload, string(profile)); err != nil || !res.Valid {
		t.Fatalf("the next payload: %+v %v, want the lane's clean answer", res, err)
	}
	if got := atomic.LoadInt32(attempts); got != 1 || atomic.LoadInt32(hits) != 3 {
		t.Fatalf("attempts=%d hits=%d, want the one boot qualification and three requests", got, *hits)
	}
}

// A lane that does not answer in time while the collection is still waiting
// has stopped, like one that refuses. Production timings throughout: the
// certification client's own limits (NewCertificationOperationValidator) and
// the collector's candidate and collection contexts, against a qualified lane
// that holds every request. The candidate's time runs out first, with the
// collection still live: the verdict is expired, the lane re-qualifies, and
// the next certification reads "re-qualifying" without dialing.
func TestQualifiedCertificationRequalifiesALaneThatTimesOut(t *testing.T) {
	v, payload, _, hits, mode, attempts, _ := qualifiedFaultLane(t, NewCertificationOperationValidator, time.Hour)
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.1": v}, nil)
	atomic.StoreInt32(mode, laneHeld)
	certificationSubmitPayload(g, "held", payload)
	certificationFlush(t, g)
	if got := laneVerdict(t, g.CertificationEvidenceForTest()[0], "2.1"); got.State != "expired" {
		t.Fatalf("a held request: %+v, want expired", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(attempts) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("a lane that did not answer within the candidate's time started no qualification")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := atomic.LoadInt32(hits)
	certificationSubmitPayload(g, "requalifying", payload)
	certificationFlush(t, g)
	if got := laneVerdict(t, g.CertificationEvidenceForTest()[1], "2.1"); got.State != "unavailable" || got.Error != "certification validator unavailable: lane not qualified; re-qualifying" {
		t.Fatalf("while re-qualifying: %+v", got)
	}
	if atomic.LoadInt32(hits) != before {
		t.Fatal("a lane being re-qualified was dialed")
	}
}

// When the collection's own time runs out first (a job that waited in the
// queue until its thirty seconds were nearly spent), the caller gave up: the
// verdict is expired and the lane stays qualified.
func TestQualifiedCertificationKeepsQualificationWhenTheCollectionEnds(t *testing.T) {
	v, payload, _, _, mode, attempts, _ := qualifiedFaultLane(t, NewCertificationOperationValidator, time.Hour)
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.1": v}, nil)
	atomic.StoreInt32(mode, laneHeld)
	queued := time.Now().Add(-certificationQueueMaxAge + 200*time.Millisecond)
	g.enqueueCertification(certificationJob{evidence: CertificationEvidence{LegType: "pas-claim", Seam: "provider-ingress", Direction: "request", CorrelationID: "late", TargetLine: "2.1"}, payload: payload, queued: queued})
	certificationFlush(t, g)
	if got := laneVerdict(t, g.CertificationEvidenceForTest()[0], "2.1"); got.State != "expired" {
		t.Fatalf("a collection that ran out: %+v, want expired", got)
	}
	v.mu.Lock()
	stale, ready := v.stale, v.ready
	v.mu.Unlock()
	if stale || !ready || atomic.LoadInt32(attempts) != 1 {
		t.Fatalf("stale=%v ready=%v attempts=%d: the collection's own deadline cleared the qualification", stale, ready, *attempts)
	}
	atomic.StoreInt32(mode, laneRecorded)
	certificationSubmitPayload(g, "next", payload)
	certificationFlush(t, g)
	if got := laneVerdict(t, g.CertificationEvidenceForTest()[1], "2.1"); got.State != "valid" {
		t.Fatalf("the next payload: %+v, want the lane's clean answer", got)
	}
}

// Re-qualification runs at most once per minimum interval. The lane closes the
// connection, is re-qualified, and closes it again within the interval: the
// second failure clears the qualification (nothing is dialed) but starts no
// corpus run until the interval has passed.
func TestQualifiedCertificationRequalifiesAtMostOncePerInterval(t *testing.T) {
	v, payload, profile, hits, mode, attempts, gate := qualifiedFaultLane(t, NewCertificationOperationValidator, time.Hour)
	const requalifying = "certification validator unavailable: lane not qualified; re-qualifying"
	var unavailable *CertificationLaneUnavailable

	atomic.StoreInt32(mode, laneDown)
	if _, err := v.Validate(context.Background(), payload, string(profile)); !errors.As(err, &unavailable) || err.Error() != requalifying {
		t.Fatalf("first closed connection: %v", err)
	}
	gate <- struct{}{}
	waitQualified(t, v)
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts=%d after the first re-qualification, want 2", got)
	}

	if _, err := v.Validate(context.Background(), payload, string(profile)); !errors.As(err, &unavailable) || err.Error() != requalifying {
		t.Fatalf("second closed connection: %v", err)
	}
	gate <- struct{}{} // an attempt, were one started, would pass at once
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts=%d: a second corpus run started inside the interval", got)
	}
	before := atomic.LoadInt32(hits)
	atomic.StoreInt32(mode, laneRecorded)
	if _, err := v.Validate(context.Background(), payload, string(profile)); !errors.As(err, &unavailable) || err.Error() != requalifying {
		t.Fatalf("inside the interval: %v, want re-qualifying", err)
	}
	if atomic.LoadInt32(hits) != before {
		t.Fatal("an unqualified lane was dialed inside the interval")
	}
}

// blockingLog holds every log line containing hold until release is closed,
// and signals held when one is held.
type blockingLog struct {
	hold    string
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), b.hold) {
		b.once.Do(func() { close(b.held) })
		<-b.release
	}
	return len(p), nil
}

// The lane stops answering in the window between a qualification passing and
// its loop exiting (while the qualification's "ready" line is still being
// written): a new qualification still starts, and the lane qualifies again.
// Were the old loop still marked running at that moment, no loop would ever
// run again and the line would read "re-qualifying" for good.
func TestQualifiedCertificationRequalifiesAFailureWhileQualificationEnds(t *testing.T) {
	logs := &blockingLog{hold: `"state":"ready"`, held: make(chan struct{}), release: make(chan struct{})}
	previous := log.Writer()
	log.SetOutput(logs)
	defer log.SetOutput(previous)

	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL + "/fhir"
	closed.Close() // the lane refuses every connection
	var attempts int32
	v := NewQualifiedCertificationValidator("2.1", NewCertificationOperationValidator(base), func(context.Context, string, string) error {
		atomic.AddInt32(&attempts, 1)
		return nil
	})
	defer v.Close()
	// Runs before Close joins the loops (defers run last first), so a failed
	// row never leaves a loop blocked in the log.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(logs.release) }) }
	defer release()
	v.Start()

	<-logs.held // the first qualification passed; its loop is writing "ready"
	_, err := v.Validate(context.Background(), []byte(`{"resourceType":"ClaimResponse"}`), "profile")
	var unavailable *CertificationLaneUnavailable
	if !errors.As(err, &unavailable) || err.Error() != "certification validator unavailable: lane not qualified; re-qualifying" {
		t.Fatalf("a refused connection: %v, want re-qualifying", err)
	}
	release()
	waitQualified(t, v)
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts=%d, want the boot qualification and the re-qualification", got)
	}
}

// recordingLane records every request it is sent and answers each 503; a
// qualification against it never passes.
func recordingLane(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// A gated client starts nothing when it is constructed: its qualification
// starts with the certification worker. At none no worker starts, so the lane
// is never dialed, not even its metadata; at observe the worker starts the
// client's loop, and its first attempt asks the lane's metadata at once. The
// qualifier is the real one (QualifyValidatorLane); the lane records requests.
func TestGatedCertificationStartsOnlyWhereEvidenceIsCollected(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			lane, asked := recordingLane(t)
			v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), QualifyValidatorLane)
			t.Cleanup(v.Close)
			time.Sleep(100 * time.Millisecond)
			if got := asked(); len(got) != 0 {
				t.Fatalf("a constructed client dialed the lane: %v", got)
			}
			g := &Gateway{cfg: Config{Clock: time.Now, ConformanceEnforcement: level, CertificationValidatorsByLine: map[string]shnsdk.Validator{"2.2": v}}}
			g.startCertification()
			t.Cleanup(func() { _ = g.Close() })
			if level == EnforcementNone {
				time.Sleep(200 * time.Millisecond)
				v.mu.Lock()
				looping := v.looping
				v.mu.Unlock()
				if got := asked(); len(got) != 0 || looping {
					t.Fatalf("at none: the lane was asked %v, loop running %v", got, looping)
				}
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for len(asked()) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("at observe the worker's start began no qualification")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if got := asked()[0]; got != "GET /fhir/metadata" {
				t.Fatalf("first request %q, want the qualification's metadata probe", got)
			}
		})
	}
}

// The certification client's own limits (dial, TLS handshake, response header
// and the whole request) all exceed a candidate's time, so a lane that does not
// answer is always cut off by the candidate's context and recorded expired,
// never by the client's timer racing it.
func TestCertificationClientOutlastsTheCandidate(t *testing.T) {
	v := NewCertificationOperationValidator("http://lane.invalid/fhir")
	transport := v.Client.Transport.(certificationTransport).inner
	dialer := v.Client.Transport.(certificationTransport).dialer
	for name, limit := range map[string]time.Duration{"request": v.Client.Timeout, "response header": transport.ResponseHeaderTimeout, "TLS handshake": transport.TLSHandshakeTimeout, "dial": dialer.Timeout} {
		if limit <= certificationCandidateTimeout {
			t.Errorf("%s timeout %s does not outlast the candidate's %s", name, limit, certificationCandidateTimeout)
		}
	}
}

// Neither constructor starts anything: a default lane's client, like an
// address's, is not looping and has not been started until Start runs.
func TestGatedCertificationConstructorsStartNothing(t *testing.T) {
	lane, asked := recordingLane(t)
	routing := NewDiscoveredLane("2.2", lane.URL+"/fhir", shnsdk.NewOperationValidator(lane.URL+"/fhir"))
	for name, v := range map[string]*GatedCertificationValidator{
		"default lane": NewGatedCertificationValidator(routing, QualifyValidatorLane, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured"),
		"address":      NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), QualifyValidatorLane),
	} {
		t.Cleanup(v.Close)
		v.mu.Lock()
		looping, started := v.looping, v.started
		v.mu.Unlock()
		if looping || started {
			t.Errorf("%s: looping=%v started=%v after construction", name, looping, started)
		}
	}
	if got := asked(); len(got) != 0 {
		t.Fatalf("a constructed client dialed the lane: %v", got)
	}
}

// unstartedClients are a default lane's client (its routing lane already
// qualified, so only the missing Start holds it back) and an address's
// client, both over lane, with qualifiers that count their calls.
func unstartedClients(t *testing.T, lane *httptest.Server, qualified *atomic.Int32) map[string]*GatedCertificationValidator {
	t.Helper()
	count := func(context.Context, string, string) error { qualified.Add(1); return nil }
	routing := NewDiscoveredLane("2.2", lane.URL+"/fhir", shnsdk.NewOperationValidator(lane.URL+"/fhir"))
	if err := routing.Qualify(context.Background(), func(context.Context, string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	clients := map[string]*GatedCertificationValidator{
		"default lane": NewGatedCertificationValidator(routing, count, "FHIR_CERTIFY_URL_2_2 and FHIR_VALIDATE_URL_2_2 are not configured"),
		"address":      NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), count),
	}
	for _, v := range clients {
		t.Cleanup(v.Close)
	}
	return clients
}

// notStartedLines is every certification_lane_not_started line in logged.
func notStartedLines(logged string) []string {
	var out []string
	for _, line := range strings.Split(logged, "\n") {
		if _, event, ok := strings.Cut(line, "gateway: certification_lane_not_started "); ok {
			out = append(out, event)
		}
	}
	return out
}

// A gated client that was never started says so on every certification,
// "certification validator not started (call Start)", rather than reading as a
// lane that has not qualified yet; it dials nothing and starts no
// qualification, even with routing's lane ready, and logs one
// certification_lane_not_started line naming the lane, however many
// certifications ask, concurrent ones included. Both constructors.
func TestGatedCertificationUnstartedSaysSo(t *testing.T) {
	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	lane, asked := recordingLane(t)
	var qualified atomic.Int32
	const want = "certification validator unavailable: certification validator not started (call Start)"
	for name, v := range unstartedClients(t, lane, &qualified) {
		t.Run(name, func(t *testing.T) {
			before := len(notStartedLines(logged.String()))
			check := func(err error) {
				var unavailable *CertificationLaneUnavailable
				if !errors.As(err, &unavailable) || err.Error() != want {
					t.Errorf("err = %v, want %q", err, want)
				}
			}
			for range 3 {
				_, err := v.Validate(context.Background(), []byte(`{"resourceType":"ClaimResponse"}`), "profile")
				check(err)
			}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					_, err := v.Validate(context.Background(), []byte(`{"resourceType":"ClaimResponse"}`), "profile")
					check(err)
				})
			}
			wg.Wait()
			lines := notStartedLines(logged.String())[before:]
			if len(lines) != 1 {
				t.Fatalf("%d certification_lane_not_started lines, want exactly one: %q", len(lines), lines)
			}
			var event struct {
				Version          int
				Line, Base, Host string
				At               string
			}
			if err := json.Unmarshal([]byte(lines[0]), &event); err != nil || event.Version != 1 || event.Line != "2.2" || event.Base != lane.URL+"/fhir" || event.Host != "127.0.0.1" || event.At == "" {
				t.Fatalf("event %s (%v), want version 1, line 2.2, base %s, host 127.0.0.1 and a time", lines[0], err, lane.URL+"/fhir")
			}
		})
	}
	if got := asked(); len(got) != 0 {
		t.Fatalf("an unstarted client dialed the lane: %v", got)
	}
	if qualified.Load() != 0 {
		t.Fatalf("an unstarted client ran %d qualification(s)", qualified.Load())
	}
}

// Once started, the reason never appears: a default lane's client certifies on
// routing's readiness, and an address's reads its qualification's state. Through
// the collector, which starts every gated client with its worker (what
// engine.New does), no verdict ever carries it and no line is logged. A closed
// client that was never started keeps the ordinary reading: its
// qualification's state, with nothing logged.
func TestGatedCertificationStartedNeverSaysNotStarted(t *testing.T) {
	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	lane, _ := recordingLane(t)
	var qualified atomic.Int32
	clients := unstartedClients(t, lane, &qualified) // both 2.2 lanes
	address := NewQualifiedCertificationValidator("2.1", NewCertificationOperationValidator(lane.URL+"/fhir"), func(context.Context, string, string) error { return nil })
	t.Cleanup(address.Close)
	g := certificationGatewayByLine(t, map[string]shnsdk.Validator{"2.2": clients["default lane"], "2.1": address}, nil)
	for i := range 4 {
		certificationSubmitPayload(g, fmt.Sprint(i), []byte(`{"resourceType":"ClaimResponse"}`))
	}
	certificationFlush(t, g)
	records := g.CertificationEvidenceForTest()
	if len(records) != 4 {
		t.Fatalf("records=%d, want 4", len(records))
	}
	for _, e := range records {
		for _, line := range []string{"2.1", "2.2"} {
			if verdict := laneVerdict(t, e, line); verdict.State == "" || strings.Contains(verdict.Error, CertificationNotStartedReason) {
				t.Fatalf("a worker-started client at %s recorded %+v", line, verdict)
			}
		}
	}
	if lines := notStartedLines(logged.String()); len(lines) != 0 {
		t.Fatalf("a worker-started client logged %q", lines)
	}

	closed := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), nil)
	closed.Close()
	_, err := closed.Validate(context.Background(), []byte(`{"resourceType":"ClaimResponse"}`), "profile")
	if err == nil || err.Error() != "certification validator unavailable: lane not qualified; qualification pending" {
		t.Fatalf("a closed, never-started client: %v, want the ordinary pending reading", err)
	}
	if lines := notStartedLines(logged.String()); len(lines) != 0 {
		t.Fatalf("a closed client logged %q", lines)
	}
}

// A client that was certified before it was started, then started, reads as
// any started client: the first certification gives the not-started reason and
// its one log line; after Start the next gives the qualification's state, and
// no second line is logged. The qualification never passes here.
func TestGatedCertificationLateStartReadsNormally(t *testing.T) {
	var logged lockedBuffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	lane, asked := recordingLane(t)
	v := NewQualifiedCertificationValidator("2.2", NewCertificationOperationValidator(lane.URL+"/fhir"), func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(v.Close)
	certify := func() error {
		_, err := v.Validate(context.Background(), []byte(`{"resourceType":"ClaimResponse"}`), "profile")
		return err
	}
	if err := certify(); err == nil || err.Error() != "certification validator unavailable: "+CertificationNotStartedReason {
		t.Fatalf("before Start: %v", err)
	}
	v.Start()
	if err := certify(); err == nil || err.Error() != "certification validator unavailable: lane not qualified; qualification pending" {
		t.Fatalf("after Start: %v, want the ordinary pending reading", err)
	}
	if lines := notStartedLines(logged.String()); len(lines) != 1 {
		t.Fatalf("%d certification_lane_not_started lines, want the one from before Start", len(lines))
	}
	if got := asked(); len(got) != 0 {
		t.Fatalf("an unqualified lane was dialed: %v", got)
	}
}
