package lanequalify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeLane stands in for a HAPI validator at the wire: /metadata answers 200 and
// every $validate answers an OperationOutcome (HTTP 200 whatever the issues).
// respond is the per-request hook; returning false means the hook already
// answered (or deliberately never will).
type fakeLane struct {
	srv     *httptest.Server
	respond func(w http.ResponseWriter, n int, body []byte) bool
	mu      sync.Mutex
	posts   []recordedPost
}

type recordedPost struct {
	path, profile string
	body          []byte
}

func testOutcome(body []byte, profile string) string {
	if outcome := explicitProfileTestOutcome(profile); outcome != "" {
		return outcome
	}
	if outcome := supportTestOutcome(body, ""); outcome != "" {
		return outcome
	}
	if strings.Contains(string(body), `"valueBoolean":true`) {
		return targetedNegativeOutcome
	}
	return cleanOutcome
}

func newFakeLane(t *testing.T) *fakeLane {
	t.Helper()
	l := &fakeLane{}
	mux := http.NewServeMux()
	mux.HandleFunc("/fhir/metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement"}`))
	})
	mux.HandleFunc("/fhir/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.posts = append(l.posts, recordedPost{path: r.URL.Path, profile: r.URL.Query().Get("profile"), body: body})
		n := len(l.posts)
		l.mu.Unlock()
		if l.respond != nil && !l.respond(w, n, body) {
			return
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		if outcome := supportTestOutcome(body, r.URL.Query().Get("profile")); outcome != "" {
			_, _ = w.Write([]byte(outcome))
		} else {
			_, _ = w.Write([]byte(testOutcome(body, r.URL.Query().Get("profile"))))
		}
	})
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

func (l *fakeLane) base() string { return l.srv.URL + "/fhir" }

func (l *fakeLane) recorded() []recordedPost {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedPost(nil), l.posts...)
}

// progressLog collects progress callbacks under a lock.
type progressLog struct {
	mu    sync.Mutex
	lines []string
}

func (p *progressLog) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, s)
}

func (p *progressLog) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

func TestWarm_PostsTheFullCorpusInOrderAndReportsProgress(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			lane := newFakeLane(t)
			var progress progressLog
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := Warm(ctx, lane.base(), line, progress.record); err != nil {
				t.Fatalf("Warm: %v", err)
			}
			rows := readinessRows(line)
			if len(rows) != 42 || RowCount(line) != 42 {
				t.Fatalf("readiness corpus for %s has %d rows (RowCount %d), want the 42-row contract", line, len(rows), RowCount(line))
			}
			posts := lane.recorded()
			if len(posts) != len(rows) {
				t.Fatalf("posted %d rows, want %d", len(posts), len(rows))
			}
			for i, row := range rows {
				wantPath := "/fhir/" + row.resourceType + "/$validate"
				if posts[i].path != wantPath || posts[i].profile != row.profile {
					t.Errorf("post %d = %s?profile=%s, want %s?profile=%s (row %s)", i, posts[i].path, url.QueryEscape(posts[i].profile), wantPath, url.QueryEscape(row.profile), row.identity)
				}
			}
			lines := progress.snapshot()
			if len(lines) != len(rows) {
				t.Fatalf("progress reported %d times, want once per row (%d): %v", len(lines), len(rows), lines)
			}
			if !strings.Contains(lines[0], "1/42") || !strings.Contains(lines[0], rows[0].identity) {
				t.Errorf("first progress line %q should name row 1/42 %s", lines[0], rows[0].identity)
			}
			if !strings.Contains(lines[41], "42/42") || !strings.Contains(lines[41], rows[41].identity) {
				t.Errorf("last progress line %q should name row 42/42 %s", lines[41], rows[41].identity)
			}
		})
	}
}

// Rejection row: /metadata 200 with $validate hanging never
// becomes ready — Warm returns at the budget, not after, and does not re-post.
func TestWarm_HangingValidateFailsAtTheBudget(t *testing.T) {
	lane := newFakeLane(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	lane.respond = func(w http.ResponseWriter, n int, _ []byte) bool {
		<-release // hang every $validate until the test ends
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil {
		t.Fatal("Warm returned nil while $validate never answered")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Warm took %s to give up on a hanging row; it must return at the readiness budget", elapsed)
	}
	if !strings.Contains(err.Error(), readinessRows("2.0")[0].identity) || !strings.Contains(err.Error(), "1/42") {
		t.Errorf("error %q should name the hanging row 1/42 %s", err, readinessRows("2.0")[0].identity)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %q should carry the budget expiry (context.DeadlineExceeded)", err)
	}
	if got := len(lane.recorded()); got != 1 {
		t.Errorf("hanging row was posted %d times, want exactly once (never re-posted)", got)
	}
}

// Rejection row: /metadata 200 with $validate answering something
// other than an OperationOutcome is not ready.
func TestWarm_NonOperationOutcomeIsNotReady(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, _ []byte) bool {
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"not-an-outcome"}`))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "not an OperationOutcome") {
		t.Fatalf("Warm = %v, want a 'not an OperationOutcome' failure", err)
	}
	if got := len(lane.recorded()); got != 1 {
		t.Errorf("posted %d rows after the first bad answer, want 1 (failure is terminal)", got)
	}
}

// Rejection row: a partial warm — one row still cold — is not
// ready, and the rows that already answered are not re-posted while waiting.
func TestWarm_PartialWarmIsNotReadyAndWarmRowsAreNotReposted(t *testing.T) {
	lane := newFakeLane(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	const coldRow = 5
	lane.respond = func(w http.ResponseWriter, n int, _ []byte) bool {
		if n == coldRow {
			<-release
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var progress progressLog
	err := Warm(ctx, lane.base(), "2.1", progress.record)
	if err == nil {
		t.Fatal("Warm returned nil with one row still cold")
	}
	rows := readinessRows("2.1")
	if !strings.Contains(err.Error(), rows[coldRow-1].identity) {
		t.Errorf("error %q should name the cold row %s", err, rows[coldRow-1].identity)
	}
	posts := lane.recorded()
	if len(posts) != coldRow {
		t.Fatalf("posted %d rows, want exactly %d (four warm rows once each, the cold row once)", len(posts), coldRow)
	}
	seen := map[string]int{}
	for _, p := range posts {
		seen[p.path+"?"+p.profile+"#"+string(p.body)]++
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("request %s posted %d times, want 1", key, n)
		}
	}
	if lines := progress.snapshot(); len(lines) != coldRow || !strings.Contains(lines[coldRow-1], rows[coldRow-1].identity) {
		t.Errorf("progress = %v, want %d entries ending at the cold row", lines, coldRow)
	}
}

// The twin must apply the image's verdict assertions, not merely accept an
// OperationOutcome: a lane that keeps returning the reproduced line-2.2 slicing
// failure past the initialization pass is not ready.
func TestWarm_FalseSlicingVerdictAfterInitializationIsNotReady(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(primeSlicingOutcome22)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.2", nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected verdict") {
		t.Fatalf("Warm = %v, want an 'unexpected verdict' failure once the qualification pass sees the slicing error", err)
	}
	if !strings.Contains(err.Error(), "qualify-1-versioned-approved") {
		t.Errorf("error %q should name the first qualification row that returned the false verdict", err)
	}
	// Four initialization rows + nine prime rows tolerated the slicing error;
	// the first qualification row is terminal.
	if got := len(lane.recorded()); got != 14 {
		t.Errorf("posted %d rows, want 14 (4 init + 9 prime + the failing qualify-1 row)", got)
	}
}

func TestWarm_UnknownLinePostsNothing(t *testing.T) {
	lane := newFakeLane(t)
	err := Warm(context.Background(), lane.base(), "9.9", nil)
	if err == nil || !strings.Contains(err.Error(), "9.9") {
		t.Fatalf("Warm = %v, want an unknown-line error naming the line", err)
	}
	if got := len(lane.recorded()); got != 0 {
		t.Errorf("posted %d rows for an unknown line, want 0", got)
	}
	if RowCount("9.9") != 0 {
		t.Errorf("RowCount(9.9) = %d, want 0", RowCount("9.9"))
	}
}

func TestPASVersion_MatchesTheCorpusTable(t *testing.T) {
	for line, want := range map[string]string{"2.0": "2.0.1", "2.1": "2.1.0", "2.2": "2.2.1"} {
		got, ok := PASVersion(line)
		if !ok || got != want {
			t.Errorf("PASVersion(%s) = %q,%v want %q,true", line, got, ok, want)
		}
	}
	if _, ok := PASVersion("9.9"); ok {
		t.Error("PASVersion(9.9) reported a version for an unknown line")
	}
}

// Verify is the strict post-ready oracle: one clean qualification pass plus the
// negative controls, no priming and no retry — the check a live gate runs the
// instant a child reports ready.
func TestVerify_RunsOneStrictPassAndTheNegativeControls(t *testing.T) {
	lane := newFakeLane(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Verify(ctx, lane.base(), "2.2"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	posts := lane.recorded()
	if len(posts) != 20 {
		t.Fatalf("Verify posted %d rows, want 20 (nine decision forms, three decision controls, four full-response rows, two encounter rows and two explicit-profile controls)", len(posts))
	}
	for i, p := range posts {
		wantPath := "/fhir/ClaimResponse/$validate"
		if i >= 16 && i < 18 {
			wantPath = "/fhir/Claim/$validate"
		} else if i >= 12 && i < 16 {
			wantPath = "/fhir/Bundle/$validate"
		}
		if p.path != wantPath {
			t.Errorf("post %d = %s, want a ClaimResponse $validate", i, p.path)
		}
	}
	negatives := 0
	for _, p := range posts {
		if p.path == "/fhir/ClaimResponse/$validate" && strings.Contains(string(p.body), `"valueBoolean":true`) {
			negatives++
		}
	}
	if negatives != 3 {
		t.Errorf("Verify posted %d negative controls, want 3", negatives)
	}
}

func TestVerify_FalseVerdictOnTheFirstRowIsTerminal(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, _ []byte) bool {
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(primeSlicingOutcome22))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Verify(ctx, lane.base(), "2.2")
	if err == nil || !strings.Contains(err.Error(), "unexpected verdict") || !strings.Contains(err.Error(), "verify-versioned-approved") {
		t.Fatalf("Verify = %v, want an 'unexpected verdict' failure naming verify-versioned-approved", err)
	}
	if got := len(lane.recorded()); got != 1 {
		t.Errorf("posted %d rows after the false verdict, want 1 (no retry)", got)
	}
}

func TestVerify_NegativeControlAcceptedCleanIsTerminal(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, body []byte) bool {
		if strings.Contains(string(body), `"valueBoolean":true`) {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(cleanOutcome)) // the mutation must be rejected, not accepted
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Verify(ctx, lane.base(), "2.0")
	if err == nil || !strings.Contains(err.Error(), "negative-versioned") {
		t.Fatalf("Verify = %v, want a failure on negative-versioned (a lane that accepts the type mutation is not validating the profile)", err)
	}
}

// A wrong verdict fails readiness loudly: the error names the row, the
// failure class AND the offending outcome's first error issue, so the child's
// failure detail says what the validator actually answered.
func TestWarm_FailureCarriesTheOffendingOutcome(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(primeSlicingOutcome22)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.2", nil)
	if err == nil {
		t.Fatal("Warm returned nil on a false verdict")
	}
	for _, want := range []string{"qualify-1-versioned-approved", "unexpected verdict", "error/processing", "Slicing cannot be evaluated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

// A non-outcome answer is excerpted, bounded, on one line.
func TestWarm_FailureExcerptIsBoundedAndSingleLine(t *testing.T) {
	lane := newFakeLane(t)
	big := "{\n\"resourceType\": \"Patient\",\n\"text\": \"" + strings.Repeat("x", 5000) + "\"\n}"
	lane.respond = func(w http.ResponseWriter, _ int, _ []byte) bool {
		w.Header().Set("Content-Type", "application/fhir+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "not an OperationOutcome") {
		t.Fatalf("Warm = %v, want a not-an-OperationOutcome failure", err)
	}
	if len(err.Error()) > 700 || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error is %d bytes / multi-line=%v; want a bounded single line", len(err.Error()), strings.Contains(err.Error(), "\n"))
	}
	if !strings.Contains(err.Error(), `"resourceType": "Patient"`) {
		t.Errorf("error %q should excerpt the answer", err)
	}
}

// The readiness budget expiring mid-row is reported as the budget, not as a
// transport fault.
func TestWarm_BudgetExpiryNamesTheBudget(t *testing.T) {
	lane := newFakeLane(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	lane.respond = func(http.ResponseWriter, int, []byte) bool { <-release; return false }
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "no answer inside the readiness budget") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Warm = %v, want 'no answer inside the readiness budget' wrapping the deadline", err)
	}
}

// A realistic HAPI outcome is pretty-printed and carries several warnings
// before the error; the summary must still name the error issue.
func TestWarm_FailureSummaryFindsTheErrorInALargeOutcome(t *testing.T) {
	var issues []string
	for i := 0; i < 8; i++ {
		issues = append(issues, fmt.Sprintf(`    {
      "severity": "warning",
      "code": "processing",
      "diagnostics": "Could not confirm that the codes provided are in the value set %d %s"
    }`, i, strings.Repeat("http://example.org/ValueSet/synthetic-terminology-", 6)))
	}
	issues = append(issues, `    {
      "severity": "error",
      "code": "processing",
      "details": {"coding": [{"system": "http://hl7.org/fhir/java-core-messageId", "code": "SLICING_CANNOT_BE_EVALUATED"}]},
      "diagnostics": "Slicing cannot be evaluated: Could not match discriminator (url) for slice Extension.extension:number in profile http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction|2.2.1 - the discriminator [url] does not have fixed value, binding or existence assertions",
      "expression": ["ClaimResponse.item[0].adjudication[0].extension[0].extension[0]"]
    }`)
	big := "{\n  \"resourceType\": \"OperationOutcome\",\n  \"issue\": [\n" + strings.Join(issues, ",\n") + "\n  ]\n}"
	if len(big) < 4096 {
		t.Fatalf("test outcome is %d bytes; it must exceed any small excerpt cap", len(big))
	}
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(big)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.2", nil)
	if err == nil {
		t.Fatal("Warm returned nil on a false verdict")
	}
	// The error issue is the tolerated prime slicing diagnostic, so the nine
	// prime rows pass and the first strict qualification row is the one that
	// fails — the summary must still surface the buried error.
	if !strings.Contains(err.Error(), "row 14/42 qualify-1-versioned-approved") || !strings.Contains(err.Error(), "error/processing: Slicing cannot be evaluated") {
		t.Fatalf("error %q should fail on row 14/42 and name the error issue buried after the warnings", err)
	}
	if strings.Contains(err.Error(), "Could not confirm") || len(err.Error()) > 700 || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q should be the bounded, single-line error issue, not the warnings", err)
	}
}

// A positive row can fail on a warning-severity profile-resolution issue;
// the summary then names that issue, not an unrelated first issue.
func TestWarm_FailureSummaryNamesTheSuspiciousWarning(t *testing.T) {
	outcome := `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"All OK"},{"severity":"warning","code":"processing","diagnostics":"Failed to retrieve profile http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"}]}`
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(outcome)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "warning/processing: Failed to retrieve profile") {
		t.Fatalf("Warm = %v, want the suspicious warning named", err)
	}
}

func TestBound_NeverSplitsARune(t *testing.T) {
	s := strings.Repeat("€", maxSummaryBytes) // 3 bytes each: the byte bound (400) lands mid-rune
	got := bound(s)
	if !utf8.ValidString(got) {
		t.Fatalf("bound produced invalid UTF-8: %q", got)
	}
	if len(got) > maxSummaryBytes+len("…") {
		t.Fatalf("bound returned %d bytes, want at most %d plus the ellipsis", len(got), maxSummaryBytes)
	}
}

// respondClaimResponse answers every non-mutated ClaimResponse row with
// outcome and lets everything else (initialization rows, negative controls)
// answer normally.
func respondClaimResponse(outcome string) func(w http.ResponseWriter, n int, body []byte) bool {
	return func(w http.ResponseWriter, _ int, body []byte) bool {
		if strings.Contains(string(body), `"resourceType":"ClaimResponse"`) && !strings.Contains(string(body), `"valueBoolean":true`) {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(outcome))
			return false
		}
		return true
	}
}

// A prime row tolerates the known 2.2 slicing error; when it fails on a
// suspicious warning beside that tolerated error, the summary names the
// warning, not the tolerated error.
func TestWarm_FailureSummarySkipsTheToleratedPrimeSlicingError(t *testing.T) {
	outcome := strings.TrimSuffix(primeSlicingOutcome22, `]}`) + `,{"severity":"warning","code":"processing","diagnostics":"Failed to retrieve profile http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.2.1"}]}`
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(outcome)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.2", nil)
	if err == nil || !strings.Contains(err.Error(), "prime-versioned-approved") {
		t.Fatalf("Warm = %v, want the first prime row to fail", err)
	}
	if !strings.Contains(err.Error(), "warning/processing: Failed to retrieve profile") || strings.Contains(err.Error(), "Slicing cannot be evaluated") {
		t.Fatalf("error %q should name the suspicious warning, not the tolerated slicing error", err)
	}
}

// A negative row expects exactly the targeted rejection; when it fails on a
// second, non-targeted error, the summary names that error.
func TestWarm_FailureSummarySkipsTheExpectedTargetedRejection(t *testing.T) {
	extra := `,{"severity":"error","code":"invalid","diagnostics":"ClaimResponse.request: minimum required = 1, but only found 0"}`
	outcome := strings.Replace(targetedNegativeOutcome, `,{"severity":"warning"`, extra+`,{"severity":"warning"`, 1)
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, body []byte) bool {
		if strings.Contains(string(body), `"valueBoolean":true`) {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(outcome))
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "negative-versioned") {
		t.Fatalf("Warm = %v, want the first negative row to fail", err)
	}
	if !strings.Contains(err.Error(), "error/invalid: ClaimResponse.request") || strings.Contains(err.Error(), "Extension_EXT_Type") || strings.Contains(err.Error(), "found type boolean") {
		t.Fatalf("error %q should name the extra error, not the expected targeted rejection", err)
	}
}

// A negative control accepted clean says so, rather than quoting an
// informational issue.
func TestWarm_NegativeAcceptedCleanSaysWhatWasExpected(t *testing.T) {
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, body []byte) bool {
		if strings.Contains(string(body), `"valueBoolean":true`) {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(cleanOutcome))
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.1", nil)
	if err == nil || !strings.Contains(err.Error(), "negative-versioned") {
		t.Fatalf("Warm = %v, want the first negative row to fail", err)
	}
	if !strings.Contains(err.Error(), "no error issue (expected the targeted reviewActionCode type rejection)") {
		t.Fatalf("error %q should say the mutation was accepted", err)
	}
}

// Diagnostics decoded from JSON can contain newlines; the status line stays
// single-line.
func TestWarm_FailureSummaryIsSingleLineAfterDecoding(t *testing.T) {
	outcome := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","diagnostics":"first line\nsecond line\ttabbed"}]}`
	lane := newFakeLane(t)
	lane.respond = respondClaimResponse(outcome)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || strings.ContainsAny(err.Error(), "\n\r\t") || !strings.Contains(err.Error(), "first line second line tabbed") {
		t.Fatalf("error %q should be single-line with whitespace folded", err)
	}
}

// An initialization row tolerates conformance errors and fails only on a
// suspicious profile-resolution issue; the summary names that issue.
func TestWarm_FailureSummaryForInitializationSkipsToleratedErrors(t *testing.T) {
	outcome := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"Bundle.entry[2].resource: documented offline terminology error"},{"severity":"warning","code":"processing","diagnostics":"Failed to retrieve profile http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"}]}`
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, n int, _ []byte) bool {
		if n == 1 {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(outcome))
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.0", nil)
	if err == nil || !strings.Contains(err.Error(), "row 1/42 init-pas-request-bundle") {
		t.Fatalf("Warm = %v, want the first initialization row to fail", err)
	}
	if !strings.Contains(err.Error(), "warning/processing: Failed to retrieve profile") || strings.Contains(err.Error(), "Bundle.entry[2]") {
		t.Fatalf("error %q should name the suspicious warning, not the tolerated conformance error", err)
	}
}

// A negative control that reports its expected rejection more than once
// says so, rather than quoting the expected rejection as the reason.
func TestWarm_NegativeReportedTwiceSaysSo(t *testing.T) {
	targeted := targetedNegativeOutcome[strings.Index(targetedNegativeOutcome, `{"severity":"error"`):strings.Index(targetedNegativeOutcome, `,{"severity":"warning"`)]
	outcome := `{"resourceType":"OperationOutcome","issue":[` + targeted + `,` + targeted + `]}`
	lane := newFakeLane(t)
	lane.respond = func(w http.ResponseWriter, _ int, body []byte) bool {
		if strings.Contains(string(body), `"valueBoolean":true`) {
			w.Header().Set("Content-Type", "application/fhir+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(outcome))
			return false
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Warm(ctx, lane.base(), "2.2", nil)
	if err == nil || !strings.Contains(err.Error(), "negative-versioned") {
		t.Fatalf("Warm = %v, want the first negative row to fail", err)
	}
	if !strings.Contains(err.Error(), "targeted reviewActionCode type rejection reported 2 times (expected exactly once)") {
		t.Fatalf("error %q should say the expected rejection was reported twice", err)
	}
}

func TestVerifyIncludesEncounterAndExplicitControls(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			lane := newFakeLane(t)
			var active atomic.Int32
			lane.respond = func(w http.ResponseWriter, n int, body []byte) bool {
				if active.Add(1) != 1 {
					t.Error("overlapping validation requests")
				}
				defer active.Add(-1)
				time.Sleep(time.Millisecond)
				return true
			}
			if err := Verify(context.Background(), lane.base(), line); err != nil {
				t.Fatal(err)
			}
			posts := lane.recorded()
			if len(posts) != 20 {
				t.Fatalf("posts=%d want20", len(posts))
			}
			rows := append(encounterRows(line), explicitProfileRows(line)...)
			for i, row := range rows {
				raw, err := fixtureBody(row)
				if err != nil {
					t.Fatal(err)
				}
				p := posts[16+i]
				if p.path != "/fhir/"+row.resourceType+"/$validate" || p.profile != row.profile || !bytes.Equal(raw, p.body) {
					t.Fatalf("tail[%d] changed", i)
				}
			}
			for _, p := range posts[18:] {
				if !bytes.Contains(p.body, []byte(`"`+pasClaimResponseProfile+`"`)) {
					t.Fatal("in-band profile lost")
				}
			}
		})
	}
}
func TestVerifyRejectsFalseRestoredControlVerdicts(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, target := range []int{18, 19, 20} {
			for _, severity := range []string{"information", "warning"} {
				t.Run(fmt.Sprintf("%s/%d/%s", line, target, severity), func(t *testing.T) {
					lane := newFakeLane(t)
					lane.respond = func(w http.ResponseWriter, n int, _ []byte) bool {
						if n == target {
							fmt.Fprintf(w, `{"resourceType":"OperationOutcome","issue":[{"severity":%q,"code":"processing"}]}`, severity)
							return false
						}
						return true
					}
					if err := Verify(context.Background(), lane.base(), line); err == nil {
						t.Fatal("false control accepted")
					}
					if n := len(lane.recorded()); n != target {
						t.Fatalf("posts=%d want=%d (no retry)", n, target)
					}
				})
			}
		}
	}
}
