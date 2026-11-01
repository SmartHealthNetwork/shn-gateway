// Package validatorissue reads a FHIR validator's issues for what they say
// about the check rather than the payload. An error that says a check did not
// run is no verdict on the message: the value it could not check may be good
// or bad. Issues are read by message id, never by their text.
package validatorissue

import shnsdk "github.com/SmartHealthNetwork/shn-sdk"

const (
	// RegexException is the message id HAPI's validator gives an error when
	// evaluating a primitive's type regex failed. A validator gives it on a
	// value that matches when a regex overruns its time limit. A malformed code,
	// oid or base64 value is refused by the validator's own checks before its
	// regex runs.
	RegexException = "TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION"
	// RegexTimedOut is the message id it gives an error when a regex a profile
	// puts on an element did not finish within that limit: "could not be
	// executed".
	RegexTimedOut = "REGEX_MATCH_TIMED_OUT"
)

// CouldNotRun reports whether i says a check could not run. A fatal issue
// carrying one of these ids is not read this way: fatal stops the validator.
func CouldNotRun(i shnsdk.Issue) bool {
	return i.Severity == "error" && (i.MessageID == RegexException || i.MessageID == RegexTimedOut)
}

// OnlyCouldNotRun reports whether res is an invalid result whose every error
// and fatal issue says a check could not run, so it is no verdict at all. An
// issue of any other kind beside them is read as usual, and a result without
// issue details says nothing either way.
func OnlyCouldNotRun(res shnsdk.Result) bool {
	if res.Valid {
		return false
	}
	found := false
	for _, d := range res.Details {
		if d.Severity != "error" && d.Severity != "fatal" {
			continue
		}
		if !CouldNotRun(d) {
			return false
		}
		found = true
	}
	return found
}
