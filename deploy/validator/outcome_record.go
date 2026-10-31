// outcome_record.go renders the answer behind a failed verdict row as a bounded
// issue list, so a verdict that changes between attempts on the same input
// names the issue that changed. Only the verifier commands print it: they post
// the fixed synthetic corpus, never a participant's message. The diagnostics
// text is never printed; its digest is, so an issue whose wording alone changed
// still shows, and can be compared with the committed expected outcome.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	maxRecordedIssues = 32  // issues listed per failed row; the total is always printed
	maxRecordedField  = 160 // bytes kept of any one field
)

// outcomeRecord returns the lines that describe excerpt, the answer a row
// rejected. An answer that is not an OperationOutcome is described by its size
// and digest only. The issues that can decide a verdict (error, fatal, or a
// profile-resolution issue) are listed first, so the bound never hides one;
// the rest fill the list up to maxRecordedIssues, in answer order. For a row
// that expects a committed outcome, each error issue is marked with the
// expected issue it equals, or with the one it equals in every field but its
// diagnostics, and the expected issues no answer issue equalled are listed.
func outcomeRecord(row warmup, excerpt string) []string {
	outcome, err := decodeOperationOutcome([]byte(excerpt))
	if err != nil || outcome.ResourceType != "OperationOutcome" {
		return []string{fmt.Sprintf("answer=not-an-OperationOutcome bytes=%d sha256=%s", len(excerpt), digest(excerpt))}
	}
	var expected []outcomeIssue
	if row.mode == verdictSupportNegative {
		if raw, err := fixtures.ReadFile(row.expectedOutcome); err == nil {
			if want, err := decodeOperationOutcome(raw); err == nil {
				expected = want.Issue
			}
		}
	}
	used := make([]bool, len(expected))
	marks := make([]string, len(outcome.Issue))
	counts := map[string]int{}
	var deciding, other []int
	for i, issue := range outcome.Issue {
		counts[issue.Severity]++
		isError := issue.Severity == "error" || issue.Severity == "fatal"
		malformed := (!isError && issue.Severity != "warning" && issue.Severity != "information") || !validIssueType(issue.Code)
		if isError || malformed || suspiciousProfileIssue(issue) {
			deciding = append(deciding, i)
		} else {
			other = append(other, i)
		}
		if expected != nil && isError {
			marks[i] = " expected=none"
			if k := matchExpected(issue, expected, used); k >= 0 {
				used[k] = true
				marks[i] = fmt.Sprintf(" expected=%d", k+1)
			} else if k := repeatsExpected(issue, expected); k >= 0 {
				marks[i] = fmt.Sprintf(" expected=none repeats=%d", k+1)
			} else if k := nearExpected(issue, expected); k >= 0 {
				marks[i] = fmt.Sprintf(" expected=none differs-from=%d in=diagnostics", k+1)
			}
		}
	}
	lines := []string{fmt.Sprintf("issues=%d fatal=%d error=%d warning=%d information=%d",
		len(outcome.Issue), counts["fatal"], counts["error"], counts["warning"], counts["information"])}
	listed := 0
	for _, i := range append(deciding, other...) {
		if listed == maxRecordedIssues {
			break
		}
		listed++
		issue := outcome.Issue[i]
		lines = append(lines, fmt.Sprintf("issue=%d/%d severity=%s code=%s message-id=%s expression=%s diagnostics-sha256=%s suspicious=%t%s",
			i+1, len(outcome.Issue), field(issue.Severity), field(issue.Code), field(messageID(issue)),
			field(strings.Join(issue.Expression, ",")), digest(issue.Diagnostics), suspiciousProfileIssue(issue), marks[i]))
	}
	if listed < len(outcome.Issue) {
		lines = append(lines, fmt.Sprintf("issues-not-listed=%d", len(outcome.Issue)-listed))
	}
	if expected != nil {
		var missing []string
		for k, ok := range used {
			if !ok {
				missing = append(missing, fmt.Sprint(k+1))
			}
		}
		if len(missing) > 0 {
			lines = append(lines, fmt.Sprintf("expected-not-answered=%s of %d", strings.Join(missing, ","), len(expected)))
		}
	}
	return lines
}

// matchExpected is the first expected issue not yet used that issue equals,
// as assertSupportNegative compares them, or -1.
func matchExpected(issue outcomeIssue, expected []outcomeIssue, used []bool) int {
	for k, want := range expected {
		if !used[k] && reflect.DeepEqual(issue, want) {
			return k
		}
	}
	return -1
}

// repeatsExpected is the first expected issue that issue equals, already
// matched by an earlier issue of the answer, or -1.
func repeatsExpected(issue outcomeIssue, expected []outcomeIssue) int {
	for k, want := range expected {
		if reflect.DeepEqual(issue, want) {
			return k
		}
	}
	return -1
}

// nearExpected is the first expected issue that equals issue in every field
// but its diagnostics, or -1.
func nearExpected(issue outcomeIssue, expected []outcomeIssue) int {
	issue.Diagnostics = ""
	for k, want := range expected {
		want.Diagnostics = ""
		if reflect.DeepEqual(issue, want) {
			return k
		}
	}
	return -1
}

func messageID(issue outcomeIssue) string {
	for _, coding := range issue.Details.Coding {
		if coding.System == messageIDSystem {
			return coding.Code
		}
	}
	return ""
}

// field quotes s for one log line, cut to maxRecordedField bytes on a rune
// boundary; an empty field reads as "-".
func field(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > maxRecordedField {
		cut := maxRecordedField
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// digest is the first 12 hex digits of s's SHA-256, or "-" for an empty s.
func digest(s string) string {
	if s == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// regexTimeoutMessageID is the validator's message id for a primitive whose
// type regex it could not evaluate. Under CPU starvation the validator gives up
// on the regex after a fixed wall-clock limit and reports this id as an error
// on a value that matches: the check could not run, which is not a verdict on
// the value.
const regexTimeoutMessageID = "TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION"

// onlyRegexTimeout reports whether a row rejected excerpt, its answer, for an
// unexpected verdict (reason, the failure class; never a wrong status or an
// answer that could not be read), the answer carries at least one
// regexTimeoutMessageID issue, and it would have passed row's own assertions
// without those issues: the regex check that could not run was the only
// deviation in the verdict. The row still fails; this only names why. An
// answer left with no issue is read as one informational issue, as a clean
// validation reports.
func onlyRegexTimeout(row warmup, reason, excerpt string) bool {
	if reason != "unexpected verdict" {
		return false
	}
	if _, err := decodeOperationOutcome([]byte(excerpt)); err != nil {
		return false
	}
	var outcome map[string]any
	dec := json.NewDecoder(strings.NewReader(excerpt))
	dec.UseNumber()
	if err := dec.Decode(&outcome); err != nil {
		return false
	}
	issues, _ := outcome["issue"].([]any)
	var kept []any
	for _, value := range issues {
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		var issue outcomeIssue
		if err := json.Unmarshal(raw, &issue); err != nil {
			return false
		}
		if messageID(issue) != regexTimeoutMessageID {
			kept = append(kept, value)
		}
	}
	if len(kept) == len(issues) {
		return false
	}
	if len(kept) == 0 {
		kept = []any{map[string]any{"severity": "information", "code": "informational"}}
	}
	outcome["issue"] = kept
	rest, err := json.Marshal(outcome)
	if err != nil {
		return false
	}
	return assertVerdict(row, 200, rest) == nil
}
