// Package lanequalify qualifies a default validator endpoint before the gateway
// admits that lane. The corpus and bounded orchestrator match the packaged
// validator and Kit qualifier; metadata alone is never readiness.
package lanequalify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// RowCount reports how many rows Warm posts for line (0 for an unknown line).
func RowCount(line string) int {
	return len(readinessRows(line))
}

// PASVersion reports the PAS package version the corpus asserts for line, so
// a caller can fence it against the IG set the validator actually loads.
func PASVersion(line string) (string, bool) {
	return pasVersion(line)
}

// Warm posts line's full readiness corpus to base (the validator's
// http://host:port/fhir root) one row at a time and returns nil only when
// every row answered with its expected verdict. ctx bounds the whole run: a
// row that has not answered when ctx expires fails the run with that row
// named, and no row is ever posted twice — an uncertain server-side result
// is terminal for this process, exactly as it is for the image's worker; a
// fresh child process is the recovery boundary. progress (nil allowed) is
// told which row is in flight, as "warming n/total: <row>".
func Warm(ctx context.Context, base, line string, progress func(string)) error {
	rows := readinessRows(line)
	if len(rows) == 0 {
		return fmt.Errorf("validatorwarm: no readiness corpus for line %q", line)
	}
	return post(ctx, base, rows, progress)
}

// Verify is the strict post-ready oracle for a validator that already reports
// ready: one clean qualification pass over the PAS ClaimResponse forms plus
// the targeted negative controls (the image's `verify-verdicts` command). It
// never primes and never retries — a false verdict from a ready child is the
// finding — so it belongs in gates, not in readiness.
func Verify(ctx context.Context, base, line string) error {
	rows := append(qualificationRows(line, "verify"), negativeRows(line)...)
	rows = append(rows, fullResponseRows(line)...)
	rows = append(rows, encounterRows(line)...)
	rows = append(rows, explicitProfileRows(line)...)
	if len(rows) == 0 {
		return fmt.Errorf("validatorwarm: no verification corpus for line %q", line)
	}
	return post(ctx, base, rows, nil)
}

func post(ctx context.Context, base string, rows []warmup, progress func(string)) error {
	client := httpClient()
	for i, row := range rows {
		if progress != nil {
			progress(fmt.Sprintf("warming %d/%d: %s", i+1, len(rows), row.identity))
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("validatorwarm: row %d/%d %s not posted: %w", i+1, len(rows), row.identity, err)
		}
		if err := validate(ctx, client, base, row); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("validatorwarm: row %d/%d %s: no answer inside the readiness budget (%w)", i+1, len(rows), row.identity, ctxErr)
			}
			var oe *outcomeError
			if errors.As(err, &oe) {
				return fmt.Errorf("validatorwarm: row %d/%d %s: %w; answer: %s", i+1, len(rows), row.identity, err, summarize(row, oe.excerpt))
			}
			return fmt.Errorf("validatorwarm: row %d/%d %s: %w", i+1, len(rows), row.identity, err)
		}
	}
	return nil
}

// maxSummaryBytes bounds what a failure carries into the child's status
// detail: enough to read the validator's first error issue, never a whole
// OperationOutcome.
const maxSummaryBytes = 400

// summarize renders a rejected answer for a status line. For an
// OperationOutcome it names the issue that flipped the row's verdict,
// mirroring what assertVerdict rejects per mode: skipping what the mode
// tolerates (initialization rows tolerate every error severity, the prime
// pass tolerates the known slicing error, the negative control expects its
// targeted rejection), the first error/fatal issue, else the first
// profile-resolution/slicing issue the assertions treat as suspicious, else
// the first issue — as "severity/code: diagnostics". A negative control
// with no error, or with its expected rejection reported more than once,
// says so. Anything that is not an OperationOutcome is a bounded prefix of
// the answer.
func summarize(row warmup, excerpt string) string {
	outcome, err := decodeOperationOutcome([]byte(excerpt))
	if err != nil || outcome.ResourceType != "OperationOutcome" || len(outcome.Issue) == 0 {
		return bound(excerpt)
	}
	isError := func(issue outcomeIssue) bool { return issue.Severity == "error" || issue.Severity == "fatal" }
	tolerated := func(issue outcomeIssue) bool {
		switch row.mode {
		case verdictInitialize:
			return isError(issue) && !suspiciousProfileIssue(issue)
		case verdictPrime:
			return allowedPrimeSlicing(row.line, issue)
		case verdictNegative:
			return targetedNegative(issue)
		}
		return false
	}
	var errorsSeen, targetedSeen int
	var chosen *outcomeIssue
	for i := range outcome.Issue {
		issue := outcome.Issue[i]
		if isError(issue) {
			errorsSeen++
		}
		if row.mode == verdictNegative && targetedNegative(issue) {
			targetedSeen++
		}
		if tolerated(issue) {
			continue
		}
		if chosen == nil && isError(issue) {
			chosen = &outcome.Issue[i]
		}
	}
	if chosen == nil {
		for i := range outcome.Issue {
			if !tolerated(outcome.Issue[i]) && suspiciousProfileIssue(outcome.Issue[i]) {
				chosen = &outcome.Issue[i]
				break
			}
		}
	}
	if chosen == nil && row.mode == verdictNegative {
		if errorsSeen == 0 {
			return "no error issue (expected the targeted reviewActionCode type rejection)"
		}
		if targetedSeen > 1 {
			return fmt.Sprintf("targeted reviewActionCode type rejection reported %d times (expected exactly once)", targetedSeen)
		}
	}
	if chosen == nil {
		chosen = &outcome.Issue[0]
	}
	return bound(fmt.Sprintf("%s/%s: %s", chosen.Severity, chosen.Code, chosen.Diagnostics))
}

// bound folds s onto one line and truncates it to maxSummaryBytes on a rune
// boundary, marking the cut.
func bound(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxSummaryBytes {
		return s
	}
	cut := maxSummaryBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
