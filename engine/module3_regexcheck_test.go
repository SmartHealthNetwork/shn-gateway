package engine

import (
	"fmt"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// These rows read the core's own conformance reading, so they run in the
// core-only build too. The choke point, tolerance and certification-run rows,
// which reach the PA zone's legs and helpers, sit with the other rows of their
// kind (module3_classify_test.go, conformance_tolerance_test.go,
// certifyterminology_test.go).

// regexTimeoutIssue is the error a CPU-starved validator lane gave on a value
// that matches its type's regex: it gave up on the regex in time. The text is
// what the lane reported; the reading uses only the message id.
func regexTimeoutIssue(expr string) shnsdk.Issue {
	return regexErr(regexCheckCouldNotRun, expr, `Exception evaluating regex '\S*' on type uri: Regex evaluation timed out after `)
}

const regexTimeoutURL = "Bundle.entry[0].resource/*ClaimResponse/1764*/.item[0].adjudication[0].extension[0].extension[0].url"

func regexErr(id, expr, diag string) shnsdk.Issue {
	return shnsdk.Issue{Severity: "error", Code: "processing", MessageID: id, Expression: []string{expr}, Diagnostics: diag}
}

// regexResult is an invalid $validate result carrying issues.
func regexResult(issues ...shnsdk.Issue) shnsdk.Result {
	var texts []string
	for _, i := range issues {
		if i.Severity == "error" || i.Severity == "fatal" {
			texts = append(texts, i.Diagnostics)
		}
	}
	return shnsdk.Result{Valid: false, Issues: texts, Details: issues}
}

// A regex check the validator could not run is no verdict on the payload:
// alone, or beside deeper rules' errors, the result is unavailable (the
// previous reading called it structural, so invalid); beside a structural
// error it is invalid for that error, so a payload cannot turn a structural
// refusal into a carry by adding one; a fatal issue stays structural.
func TestClassifyFHIRRegexCheckCouldNotRun(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"Patient"}}]}`)
	timeout := regexTimeoutIssue(regexTimeoutURL)
	structural := regexErr("Validation_VAL_Profile_Minimum", "Bundle.entry[1].resource", "Patient.name: minimum required = 1, but only found 0")
	deeper := regexErr("Terminology_TX_NoValid_1_CC", "Bundle.entry[1].resource.maritalStatus", "x")
	summary := regexErr(summaryProfileNoMatch, "Bundle.entry[0].resource", "did not match")
	for _, tc := range []struct {
		name string
		res  shnsdk.Result
		want string
	}{
		{"the timeout alone", regexResult(timeout), "unavailable"},
		{"the timeout twice", regexResult(timeout, regexTimeoutIssue("Bundle.entry[1].resource.id")), "unavailable"},
		{"the timeout beside a deeper error", regexResult(timeout, deeper), "unavailable"},
		{"the timeout beside a structural error", regexResult(timeout, structural), "structural"},
		{"the timeout beside a deeper and a structural error", regexResult(deeper, timeout, structural), "structural"},
		{"a summary over the timed-out entry", regexResult(summary, timeout), "unavailable"},
		{"a summary over an entry with the timeout and a structural error", regexResult(summary, timeout, regexErr("Validation_VAL_Profile_Minimum", "Bundle.entry[0].resource", "x")), "structural"},
		{"a summary on another entry than the timeout", regexResult(regexErr(summaryProfileNoMatch, "Bundle.entry[1].resource", "did not match"), timeout), "structural"},
		{"a fatal issue with the id", regexResult(shnsdk.Issue{Severity: "fatal", Code: "processing", MessageID: regexCheckCouldNotRun, Expression: []string{regexTimeoutURL}}), "structural"},
		{"the text without the id", regexResult(regexErr("", regexTimeoutURL, timeout.Diagnostics)), "structural"},
	} {
		for _, line := range []string{"2.0", "2.1", "2.2"} {
			if got := ClassifyFHIRForTest(tc.res, bundle, line, ""); got != tc.want {
				t.Errorf("%s at %s: %s, want %s", tc.name, line, got, tc.want)
			}
		}
	}
}

// The certification reading: a lane whose only errors are regex checks it
// could not run, or those and unchecked terminology, has no verdict on the
// payload (unavailable); beside any other error the answer stays a verdict,
// invalid. The reason stays under 256 bytes.
func TestCertificationRegexCheckCouldNotRun(t *testing.T) {
	bundle := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`)
	timeout := regexTimeoutIssue(regexTimeoutURL)
	structural := regexErr("Validation_VAL_Profile_Minimum", "Bundle.entry[0].resource", "x")
	for _, tc := range []struct {
		name      string
		res       shnsdk.Result
		unchecked bool
		reason    string
	}{
		{"the timeout alone", regexResult(timeout), true, "a primitive regex check could not run"},
		{"the timeout and its entry's summary", regexResult(regexErr(summaryProfileNoMatch, "Bundle.entry[0].resource", "did not match"), timeout), true, "a primitive regex check could not run"},
		{"the timeout beside a structural error", regexResult(timeout, structural), false, ""},
		{"a fatal issue with the id", regexResult(shnsdk.Issue{Severity: "fatal", MessageID: regexCheckCouldNotRun, Expression: []string{regexTimeoutURL}}), false, ""},
	} {
		systems, regex, ok := certificationUnchecked(tc.res, bundle, "2.1", "")
		if ok != tc.unchecked {
			t.Errorf("%s: unchecked=%v, want %v", tc.name, ok, tc.unchecked)
			continue
		}
		if ok {
			if got := uncheckedReason(systems, regex); got != tc.reason {
				t.Errorf("%s: reason %q, want %q", tc.name, got, tc.reason)
			}
		}
	}
	// Both causes: the reason names one code system and stays under 256
	// bytes, however many systems and however long their names.
	long := func(i int) string {
		return "https://example.org/" + strings.Repeat("c", 200) + "/" + strings.Repeat("x", i)
	}
	many := make([]string, 0, 12000)
	for i := 0; i < cap(many); i++ {
		many = append(many, long(i%7))
	}
	for _, systems := range [][]string{{"http://example.org/cs"}, {long(1), long(2), long(3)}, many} {
		got := uncheckedReason(systems, true)
		if !strings.HasPrefix(got, "terminology unavailable: code system bytes=") || !strings.HasSuffix(got, "; a primitive regex check could not run") || len(got) >= 256 {
			t.Errorf("both causes, %d systems: %d bytes: %q", len(systems), len(got), got)
		}
		if len(systems) > 1 && !strings.Contains(got, fmt.Sprintf(" and %d more;", len(systems)-1)) {
			t.Errorf("both causes, %d systems: the others are not counted: %q", len(systems), got)
		}
	}
}
