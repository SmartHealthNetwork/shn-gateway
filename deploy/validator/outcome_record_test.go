package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func hcpcsRow(t *testing.T) warmup {
	t.Helper()
	for _, row := range fullResponseRows("2.1") {
		if row.identity == "full-response-negative-hcpcs" {
			return row
		}
	}
	t.Fatal("no hcpcs row")
	return warmup{}
}

// expectedIssues is row's committed expected outcome as a list of JSON objects.
func expectedIssues(t *testing.T, row warmup) []map[string]any {
	t.Helper()
	raw, err := fixtures.ReadFile(row.expectedOutcome)
	if err != nil {
		t.Fatal(err)
	}
	var outcome struct {
		Issue []map[string]any `json:"issue"`
	}
	if err := json.Unmarshal(raw, &outcome); err != nil || len(outcome.Issue) != 2 {
		t.Fatalf("expected outcome: %v, %d issues", err, len(outcome.Issue))
	}
	return outcome.Issue
}

func outcomeJSON(t *testing.T, issues ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"resourceType": "OperationOutcome", "issue": issues})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const privateDiagnostics = "private diagnostics wording"

func TestOutcomeRecordNamesTheIssueThatChanged(t *testing.T) {
	row := hcpcsRow(t)
	want := expectedIssues(t, row)
	reworded := map[string]any{}
	for k, v := range want[1] {
		reworded[k] = v
	}
	reworded["diagnostics"] = privateDiagnostics
	extra := map[string]any{"severity": "error", "code": "processing", "diagnostics": privateDiagnostics,
		"details":    map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "Terminology_TX_Error"}}},
		"expression": []any{"Bundle.entry[4].resource.item[0]"}}
	warning := map[string]any{"severity": "warning", "code": "processing", "diagnostics": privateDiagnostics}

	for _, tc := range []struct {
		name   string
		answer string
		want   []string
	}{
		{"the expected answer", outcomeJSON(t, want[0], want[1]), []string{
			"issues=2",
			`issue=1/2 severity="error" code="processing" message-id="Terminology_PassThrough_TX_Message" expression="Bundle.entry[4].resource/*Claim/1762*/.item[0].productOrService"`,
			"expected=1",
			"expected=2",
		}},
		{"an expected issue reworded", outcomeJSON(t, want[0], reworded), []string{
			"issue=2/2", "expected=none differs-from=2 in=diagnostics", "expected-not-answered=2 of 2",
		}},
		{"an expected issue missing and another error added", outcomeJSON(t, want[0], extra, warning), []string{
			"issues=3", `message-id="Terminology_TX_Error"`, "expected=none", `issue=3/3 severity="warning"`,
			"expected-not-answered=2 of 2",
		}},
		{"not an OperationOutcome", `{"resourceType":"Bundle","diagnostics":"` + privateDiagnostics + `"}`, []string{
			"answer=not-an-OperationOutcome bytes=", "sha256=",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(outcomeRecord(row, tc.answer), "\n")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("record lacks %q:\n%s", w, got)
				}
			}
			if strings.Contains(got, privateDiagnostics) {
				t.Errorf("record carries diagnostics text:\n%s", got)
			}
		})
	}
	// The same issue with another wording has another digest.
	a := outcomeRecord(row, outcomeJSON(t, want[0], want[1]))[2]
	b := outcomeRecord(row, outcomeJSON(t, want[0], reworded))[2]
	if digestOf(a) == digestOf(b) || digestOf(a) == "-" {
		t.Fatalf("diagnostics digests do not tell the wordings apart:\n%s\n%s", a, b)
	}
}

func digestOf(line string) string {
	_, after, _ := strings.Cut(line, "diagnostics-sha256=")
	d, _, _ := strings.Cut(after, " ")
	return d
}

func TestOutcomeRecordIsBounded(t *testing.T) {
	row := encounterRows("2.1")[0]
	long := strings.Repeat("x", 4*maxRecordedField)
	issues := make([]map[string]any, maxRecordedIssues+5)
	for i := range issues {
		issues[i] = map[string]any{"severity": "information", "code": "informational", "expression": []any{long}}
	}
	lines := outcomeRecord(row, outcomeJSON(t, issues...))
	if len(lines) != 1+maxRecordedIssues+1 || lines[len(lines)-1] != "issues-not-listed=5" {
		t.Fatalf("lines=%d last=%q", len(lines), lines[len(lines)-1])
	}
	for _, line := range lines {
		if len(line) > 2*maxRecordedField+300 {
			t.Fatalf("line of %d bytes", len(line))
		}
	}
}

// A full PAS response answers with dozens of informational issues before its
// errors. The bound must never hide the issues that decide the verdict: they
// are listed first, whatever their place in the answer.
func TestOutcomeRecordListsTheDecidingIssuesFirst(t *testing.T) {
	row := hcpcsRow(t)
	want := expectedIssues(t, row)
	issues := make([]map[string]any, 0, 44)
	for i := 0; i < 40; i++ {
		issues = append(issues, map[string]any{"severity": "information", "code": "informational"})
	}
	extra := map[string]any{"severity": "error", "code": "processing", "diagnostics": privateDiagnostics,
		"details": map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "Terminology_TX_Error"}}}}
	suspicious := map[string]any{"severity": "warning", "code": "processing",
		"details": map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "SLICING_CANNOT_BE_EVALUATED"}}}}
	issues = append(issues, want[0], want[1], extra, suspicious)
	lines := outcomeRecord(row, outcomeJSON(t, issues...))
	if lines[0] != "issues=44 fatal=0 error=3 warning=1 information=40" {
		t.Fatalf("header %q", lines[0])
	}
	for i, w := range []string{"issue=41/44 ", "issue=42/44 ", `issue=43/44 severity="error" code="processing" message-id="Terminology_TX_Error"`, `issue=44/44 severity="warning"`} {
		if !strings.HasPrefix(lines[1+i], w) {
			t.Errorf("line %d = %q, want it to start %q", 1+i, lines[1+i], w)
		}
	}
	if !strings.Contains(lines[3], "expected=none") || !strings.Contains(lines[4], "suspicious=true") || lines[len(lines)-1] != "issues-not-listed=12" {
		t.Fatalf("record:\n%s", strings.Join(lines, "\n"))
	}
}

// End to end: verify-verdicts on line 2.1, where the lane rewords one of the
// hcpcs row's expected issues. The command fails, as before, and its failure
// lines name the row and the issue that differed, without the wording.
func TestVerifyVerdictsRecordsTheIssuesOfAnUnexpectedVerdict(t *testing.T) {
	lane := verdictLane(t, "2.1", "verify-verdicts")
	want := expectedIssues(t, hcpcsRow(t))
	want[1]["diagnostics"] = privateDiagnostics
	lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/fhir/Bundle/$validate" {
			return true
		}
		lane.postsMu.Lock()
		body := lane.posts[len(lane.posts)-1].body
		lane.postsMu.Unlock()
		if !bytes.Contains(body, []byte(`"L9999"`)) {
			return true
		}
		w.Header().Set("Content-Type", "application/fhir+json")
		fmt.Fprint(w, outcomeJSON(t, want...))
		return false
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = writer
	code := verdictCommand([]string{"verify-verdicts", "--base", lane.base(), "--line", "2.1", "--pas-version", "2.1.0", "--budget", "5s"}, emptyEnv)
	os.Stderr = old
	writer.Close()
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if code != 1 || !strings.Contains(out, "row=full-response-negative-hcpcs elapsed=") || !strings.Contains(out, "reason=unexpected verdict") {
		t.Fatalf("exit=%d, want the hcpcs row's unexpected verdict:\n%s", code, out)
	}
	for _, w := range []string{
		"verdict: line=2.1 row=full-response-negative-hcpcs issues=2",
		"verdict: line=2.1 row=full-response-negative-hcpcs issue=1/2 ",
		"expected=1",
		"expected=none differs-from=2 in=diagnostics",
		"verdict: line=2.1 row=full-response-negative-hcpcs expected-not-answered=2 of 2",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("failure output lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, privateDiagnostics) {
		t.Fatalf("failure output carries diagnostics text:\n%s", out)
	}
}

// regexTimeoutIssue is the issue the validator reports when it gives up on a
// primitive's type regex, as a CPU-starved lane answered it.
func regexTimeoutIssue() map[string]any {
	return map[string]any{"severity": "error", "code": "processing",
		"details":     map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": regexTimeoutMessageID}}},
		"diagnostics": `Exception evaluating regex '\S*' on type uri: Regex evaluation timed out after `,
		"expression":  []any{"ClaimResponse.item[0].adjudication[0].extension[0].extension[0].url"}}
}

func TestOnlyRegexTimeoutNamesARowThatOnlyTheTimeoutFailed(t *testing.T) {
	hcpcs := hcpcsRow(t)
	want := expectedIssues(t, hcpcs)
	positive := encounterRows("2.1")[0]
	var negative warmup
	for _, row := range negativeRows("2.1") {
		if row.identity == "negative-versioned" {
			negative = row
		}
	}
	targeted := map[string]any{"severity": "error", "code": "processing",
		"details":     map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "Extension_EXT_Type"}}},
		"diagnostics": "The Extension '" + reviewActionCode + "' definition allows for the types [CodeableConcept] but found type boolean",
		"expression":  []any{negativeExpression}}
	other := map[string]any{"severity": "error", "code": "processing",
		"details": map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "Terminology_TX_Error"}}}}
	info := map[string]any{"severity": "information", "code": "informational"}
	for _, tc := range []struct {
		name  string
		row   warmup
		issue []map[string]any
		want  bool
	}{
		{"positive: the timeout alone", positive, []map[string]any{regexTimeoutIssue()}, true},
		{"positive: the timeout beside information", positive, []map[string]any{info, regexTimeoutIssue()}, true},
		{"positive: the timeout beside another error", positive, []map[string]any{regexTimeoutIssue(), other}, false},
		{"positive: another error alone", positive, []map[string]any{other}, false},
		{"support negative: the expected errors and the timeout", hcpcs, []map[string]any{want[0], regexTimeoutIssue(), want[1]}, true},
		{"support negative: an expected error missing beside the timeout", hcpcs, []map[string]any{want[0], regexTimeoutIssue()}, false},
		{"support negative: the expected errors alone", hcpcs, []map[string]any{want[0], want[1]}, false},
		{"negative control: the targeted rejection and the timeout", negative, []map[string]any{targeted, regexTimeoutIssue()}, true},
		{"negative control: the timeout without the targeted rejection", negative, []map[string]any{regexTimeoutIssue()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyRegexTimeout(tc.row, "unexpected verdict", outcomeJSON(t, tc.issue...)); got != tc.want {
				t.Fatalf("onlyRegexTimeout = %v, want %v", got, tc.want)
			}
		})
	}
	if onlyRegexTimeout(positive, "unexpected verdict", `{"resourceType":"Bundle"}`) {
		t.Fatal("an answer that is not an OperationOutcome was named")
	}
	// Only an unexpected verdict is named: a row that failed on its status or
	// on an answer it could not read failed for that, whatever the issues say.
	for _, reason := range []string{"wrong response status", "not an OperationOutcome", "invalid outcome", "request failed"} {
		if onlyRegexTimeout(positive, reason, outcomeJSON(t, regexTimeoutIssue())) {
			t.Errorf("a row that failed with %q was named", reason)
		}
	}
	// The prime pass tolerates its known slicing error; the explicit-profile
	// control expects its missing-profile error. Beside either, the timeout is
	// the only deviation.
	prime := qualificationRows("2.1", "prime")[0]
	slicing := map[string]any{"severity": "error", "code": "processing", "expression": []any{negativeExpression},
		"details":     map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "SLICING_CANNOT_BE_EVALUATED"}}},
		"diagnostics": "Slicing cannot be evaluated: Could not match discriminator (url) for slice Extension.extension:number in profile " + reviewActionProfile + "|2.1.0 - the discriminator [url] does not have fixed value, binding or existence assertions"}
	explicit := explicitProfileRows("2.1")[0]
	missing := map[string]any{"severity": "error", "code": "processing",
		"details":     map[string]any{"coding": []any{map[string]any{"system": messageIDSystem, "code": "Validation_VAL_Profile_Unknown"}}},
		"diagnostics": "Invalid profile. Failed to retrieve explicitly requested profile with url=" + explicit.profile}
	for _, tc := range []struct {
		name  string
		row   warmup
		issue []map[string]any
		want  bool
	}{
		{"prime: the tolerated slicing error and the timeout", prime, []map[string]any{slicing, regexTimeoutIssue()}, true},
		{"prime: the timeout beside another error", prime, []map[string]any{slicing, regexTimeoutIssue(), other}, false},
		{"explicit profile: the expected refusal and the timeout", explicit, []map[string]any{missing, regexTimeoutIssue()}, true},
		{"explicit profile: the timeout without the expected refusal", explicit, []map[string]any{regexTimeoutIssue()}, false},
	} {
		if got := onlyRegexTimeout(tc.row, "unexpected verdict", outcomeJSON(t, tc.issue...)); got != tc.want {
			t.Errorf("%s: onlyRegexTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// End to end: a lane whose hcpcs answer carries the expected rejection plus
// the regex timeout fails the window, as before, and names the timeout; the
// same answer with another unexpected error beside the timeout is not named.
func TestVerifyVerdictsNamesARegexTimeoutAndStillFails(t *testing.T) {
	want := expectedIssues(t, hcpcsRow(t))
	other := map[string]any{"severity": "error", "code": "processing"}
	for _, tc := range []struct {
		name   string
		status int
		answer []map[string]any
		named  bool
	}{
		{"only the timeout", http.StatusOK, []map[string]any{want[0], want[1], regexTimeoutIssue()}, true},
		{"the timeout and another error", http.StatusOK, []map[string]any{want[0], want[1], regexTimeoutIssue(), other}, false},
		{"only the timeout, on a wrong status", http.StatusInternalServerError, []map[string]any{want[0], want[1], regexTimeoutIssue()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lane := verdictLane(t, "2.1", "verify-verdicts")
			lane.validate = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/fhir/Bundle/$validate" {
					return true
				}
				lane.postsMu.Lock()
				body := lane.posts[len(lane.posts)-1].body
				lane.postsMu.Unlock()
				if !bytes.Contains(body, []byte(`"L9999"`)) {
					return true
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, outcomeJSON(t, tc.answer...))
				return false
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = writer
			code := verdictCommand([]string{"verify-verdicts", "--base", lane.base(), "--line", "2.1", "--pas-version", "2.1.0", "--budget", "5s"}, emptyEnv)
			os.Stderr = old
			writer.Close()
			raw, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			out := string(raw)
			if code != 1 || !strings.Contains(out, "row=full-response-negative-hcpcs elapsed=") {
				t.Fatalf("exit=%d, want the hcpcs row to fail:\n%s", code, out)
			}
			named := strings.Contains(out, "verdict: line=2.1 row=full-response-negative-hcpcs named=regex-check-timeout: ")
			if named != tc.named {
				t.Fatalf("named=%v, want %v:\n%s", named, tc.named, out)
			}
			if strings.Contains(out, "Regex evaluation timed out") {
				t.Fatalf("failure output carries diagnostics text:\n%s", out)
			}
		})
	}
}

// Marks against the expected outcome: an expected issue is matched once, a
// copy of it is a repeat (not a change of wording), and a fatal issue is an
// error like any other.
func TestOutcomeRecordMarksRepeatsAndFatalIssues(t *testing.T) {
	row := hcpcsRow(t)
	want := expectedIssues(t, row)
	fatal := map[string]any{"severity": "fatal", "code": "processing"}
	got := strings.Join(outcomeRecord(row, outcomeJSON(t, want[0], want[0], fatal)), "\n")
	for _, w := range []string{
		"issues=3 fatal=1 error=2 warning=0 information=0",
		"issue=1/3 ", "expected=1",
		"expected=none repeats=1",
		`issue=3/3 severity="fatal"`,
		"expected-not-answered=2 of 2",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("record lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "in=diagnostics") {
		t.Errorf("an exact repeat was marked as a change of wording:\n%s", got)
	}
	lines := strings.Split(got, "\n")
	if !strings.HasSuffix(lines[3], `severity="fatal" code="processing" message-id=- expression=- diagnostics-sha256=- suspicious=false expected=none`) {
		t.Errorf("the fatal issue is not marked as an unexpected error: %q", lines[3])
	}
}

// An issue whose severity or code the row's assertions refuse decides the
// verdict too, so it is listed first; a field is cut on a rune boundary.
func TestOutcomeRecordListsMalformedIssuesFirstAndCutsOnRunes(t *testing.T) {
	row := encounterRows("2.1")[0]
	issues := make([]map[string]any, 0, 41)
	for i := 0; i < 40; i++ {
		issues = append(issues, map[string]any{"severity": "information", "code": "informational"})
	}
	issues = append(issues, map[string]any{"severity": "information", "code": "not-an-issue-type"})
	lines := outcomeRecord(row, outcomeJSON(t, issues...))
	if !strings.HasPrefix(lines[1], `issue=41/41 severity="information" code="not-an-issue-type"`) {
		t.Fatalf("the malformed issue is not listed first: %q", lines[1])
	}
	cut := field(strings.Repeat("a", maxRecordedField-1) + "é" + "bbbb")
	if strings.Contains(cut, `\x`) || !strings.HasSuffix(cut, `…"`) || strings.Contains(cut, "é") {
		t.Fatalf("field cut inside a rune: %s", cut)
	}
}
