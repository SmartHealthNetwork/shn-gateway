package validatorissue

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestCouldNotRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue shnsdk.Issue
		want  bool
	}{
		{"the primitive regex exception", shnsdk.Issue{Severity: "error", MessageID: RegexException}, true},
		{"a profile regex that timed out", shnsdk.Issue{Severity: "error", MessageID: RegexTimedOut}, true},
		{"a fatal issue with the id", shnsdk.Issue{Severity: "fatal", MessageID: RegexException}, false},
		{"a warning with the id", shnsdk.Issue{Severity: "warning", MessageID: RegexTimedOut}, false},
		{"another error", shnsdk.Issue{Severity: "error", MessageID: "Validation_VAL_Profile_Minimum"}, false},
		{"the text without the id", shnsdk.Issue{Severity: "error", Diagnostics: "Regex evaluation timed out after "}, false},
		{"no id", shnsdk.Issue{Severity: "error"}, false},
	} {
		if got := CouldNotRun(tc.issue); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOnlyCouldNotRun(t *testing.T) {
	timeout := shnsdk.Issue{Severity: "error", MessageID: RegexException}
	profile := shnsdk.Issue{Severity: "error", MessageID: RegexTimedOut}
	structural := shnsdk.Issue{Severity: "error", MessageID: "Validation_VAL_Profile_Minimum"}
	warning := shnsdk.Issue{Severity: "warning", MessageID: "Terminology_TX_NoValid_2_CC"}
	fatal := shnsdk.Issue{Severity: "fatal", MessageID: RegexException}
	for _, tc := range []struct {
		name string
		res  shnsdk.Result
		want bool
	}{
		{"one", shnsdk.Result{Details: []shnsdk.Issue{timeout}}, true},
		{"both ids", shnsdk.Result{Details: []shnsdk.Issue{timeout, profile}}, true},
		{"beside a warning", shnsdk.Result{Details: []shnsdk.Issue{warning, timeout}}, true},
		{"beside a real error", shnsdk.Result{Details: []shnsdk.Issue{timeout, structural}}, false},
		{"beside a fatal", shnsdk.Result{Details: []shnsdk.Issue{timeout, fatal}}, false},
		{"a warning alone", shnsdk.Result{Details: []shnsdk.Issue{warning}}, false},
		{"no details", shnsdk.Result{Issues: []string{"invalid"}}, false},
		{"a valid result", shnsdk.Result{Valid: true, Details: []shnsdk.Issue{timeout}}, false},
	} {
		if got := OnlyCouldNotRun(tc.res); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
