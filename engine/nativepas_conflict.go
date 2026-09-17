// nativepas_conflict.go — the pas-claim-update leg's one re-issue after a payer-side
// version conflict.
//
// A Da Vinci PAS payer that keeps its authorizations in a FHIR store resolves a pended
// claim on its own timer by writing the same ClaimResponse the amendment's update path
// reads and rewrites. When the amendment lands inside that window (observed live: the
// timer's write and the amendment's $submit four milliseconds apart), the payer's own
// store refuses the amendment's write with an optimistic-lock conflict — HAPI's
// ResourceVersionConflictException, HTTP 409, "HAPI-0989: Trying to update
// ClaimResponse/{id}/_history/{n} but this is not the current version" — and the
// amendment was NOT persisted. Nothing about the amendment was wrong; re-issuing the
// identical ClaimUpdate once the timer's write has landed reaches the amend-after-resolution
// path the responder already handles (the re-pend that keeps the stale "complete" outcome
// beside the A4 item, then the timer-resolved A1 on re-query).
//
// The responder therefore re-issues the identical ClaimUpdate exactly once after such a
// conflict, and relays whatever the re-issue answers. Any other 409 — a payer's genuine
// conflict answer, a non-OperationOutcome body — is relayed untouched on the first answer,
// and a second version conflict is relayed too: one re-issue, never a loop.
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// payerVersionConflictRetryDelay is how long the responder waits before re-issuing the
// ClaimUpdate after the payer's version conflict: long enough for the timer's write that
// caused it to have landed (it is a single row update), short enough to stay far inside the
// originator's client budget. The wait is time-based on purpose: the update leg holds no
// ClaimResponse id at this point (the conflict answered instead of one), so there is nothing
// to re-read first, and the timer's write is already committed by the time the conflict is
// answered — the wait only leaves room for the payer's own transaction to close. A second
// conflict after the wait is relayed as the payer's answer, never waited on again.
var payerVersionConflictRetryDelay = 250 * time.Millisecond

// isPayerVersionConflict reports whether a payer's non-2xx body is a FHIR store's
// optimistic-lock refusal: an OperationOutcome carrying an issue with the FHIR
// "conflict" code, or HAPI's ResourceVersionConflictException diagnostics (HAPI-0989 /
// "not the current version"). The HTTP status is the caller's check; this reads only the
// body, so a conflict-shaped body on any other status is not one. Accepting the bare
// "conflict" issue code is deliberate: a payer that used it for a semantic conflict rather
// than a version one would see one identical re-POST of an amendment it already refused,
// which is bounded and changes nothing on the gateway's side.
func isPayerVersionConflict(body []byte) bool {
	var oo struct {
		ResourceType string `json:"resourceType"`
		Issue        []struct {
			Code        string `json:"code"`
			Diagnostics string `json:"diagnostics"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &oo); err != nil || oo.ResourceType != "OperationOutcome" {
		return false
	}
	for _, is := range oo.Issue {
		if is.Code == "conflict" {
			return true
		}
		if strings.Contains(is.Diagnostics, "HAPI-0989") || strings.Contains(is.Diagnostics, "not the current version") {
			return true
		}
	}
	return false
}

// sleepCtx waits d or until ctx is done, whichever comes first, returning ctx's error in
// the latter case so a cancelled leg never re-issues.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
