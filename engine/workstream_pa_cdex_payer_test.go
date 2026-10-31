//go:build !shn_coreonly

package engine

// workstream_pa_cdex_payer_test.go — the payer gateway's side of the CDex submission: it
// binds the member the submission names by its own system, sends the
// submission to the payer's own $submit-attachment exactly, under the CDex
// base the payer publishes or else its Da Vinci base, and relays the payer's
// answer exactly.

import (
	"bytes"
	"net/http"
	"testing"
)

const cdexSubmitPath = "/$submit-attachment"

// At every level the submission reaches the payer's own $submit-attachment
// exactly as sent, and the payer's answer, informational or not, comes back
// exactly.
func TestCDexPayer_CarriesTheSubmissionToThePayersOwnEndpoint(t *testing.T) {
	body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			ans := p.send(t, legCDexSubmit, "", body)
			if ans.status != http.StatusOK || !bytes.Equal(ans.body, cdexPayersAnswer) {
				t.Fatalf("answer %d %s, want the payer's answer relayed exactly", ans.status, ans.body)
			}
			if p.partner.lastPath != cdexSubmitPath || !bytes.Equal(p.partner.lastBody, body) {
				t.Fatalf("the payer's system got %s\n%s\nwant %s with the submission as sent", p.partner.lastPath, p.partner.lastBody, cdexSubmitPath)
			}
			if x := got.only(t); x.Outcome != ExchangeAnswered || x.Exchange != legCDexSubmit || len(x.Edits) != 0 {
				t.Fatalf("record %+v, want an answered submission, carried unedited", x)
			}
		})
	}
}

// The payer's own error is relayed as the payer answered, status and bytes,
// at every level.
func TestCDexPayer_RelaysThePayersError(t *testing.T) {
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"TrackingId is required."}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.status = http.StatusBadRequest
			p.partner.errBody = oo
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-COVERED", cdexPayerCMS))
			if ans.status != http.StatusBadRequest || !bytes.Equal(ans.body, oo) {
				t.Fatalf("answer %d %s, want the payer's 400 relayed exactly", ans.status, ans.body)
			}
		})
	}
}

// The payer evaluates authority on its own leg: a submission it cannot read a
// MemberId from is refused before its system is called, at every level, on
// per-leg authority (D121). A member its system does not hold is carried
// unless it opted into checking members, which it records as conformance.
func TestCDexPayer_BindsTheMemberItNames(t *testing.T) {
	for _, level := range allLevels {
		t.Run("no MemberId/"+level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("", cdexPayerCMS))
			if ans.status != http.StatusBadRequest || p.partner.lastPath != "" {
				t.Fatalf("answer %d %s (payer called at %q), want 400 before the payer's system", ans.status, ans.body, p.partner.lastPath)
			}
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalAuthority)
		})
		t.Run("member not held/"+level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
			if ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-NOT-HELD", cdexPayerCMS)); ans.status != http.StatusOK || p.partner.lastPath != cdexSubmitPath {
				t.Fatalf("answer %d %s, want a member the payer does not hold carried", ans.status, ans.body)
			}
		})
		t.Run("member not held, checked/"+level.String(), func(t *testing.T) {
			p, got := newRecordingLevelPayer(t, level)
			p.g.cfg.RequireKnownMembers = true
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-NOT-HELD", cdexPayerCMS))
			if ans.status != http.StatusBadRequest || p.partner.lastPath != "" {
				t.Fatalf("answer %d %s, want the opted-in member check to refuse", ans.status, ans.body)
			}
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByPayerGateway, RefusalConformance)
		})
	}
}

// The submission goes under the CDex base the payer publishes in its own
// .well-known/davinci-configuration (HRex 1.2.0: the base the operation is
// invoked on) when it is same-origin with the payer's base; otherwise under
// the payer's base, with the operation's name either way. A
// published base on another origin is never followed: the submission falls to
// PAYER_DAVINCI_BASE_URL + /$submit-attachment.
func TestCDexPayer_ResolvesTheSubmitAttachmentURL(t *testing.T) {
	for name, row := range map[string]struct {
		evidence map[string]string
		wantPath string
	}{
		"none published":         {nil, cdexSubmitPath},
		"published same-origin":  {map[string]string{"pa.cdex@2.1": "{base}/cdex/attachments"}, "/cdex/attachments/$submit-attachment"},
		"published with a slash": {map[string]string{"pa.cdex@2.1": "{base}/cdex/attachments/"}, "/cdex/attachments/$submit-attachment"},
		"published elsewhere":    {map[string]string{"pa.cdex@2.1": "https://elsewhere.example/fhir"}, cdexSubmitPath},
		// Evidence for another contract never addresses the submission.
		"another contract's": {map[string]string{"pa.pas@2.2": "{base}/pas/$submit-attachment"}, cdexSubmitPath},
	} {
		t.Run(name, func(t *testing.T) {
			p := newLevelPayer(t, EnforcementObserve)
			p.partner.respByPath[row.wantPath] = cdexPayersAnswer
			evidence := map[string]string{}
			for tok, u := range row.evidence {
				evidence[tok] = string(bytes.ReplaceAll([]byte(u), []byte("{base}"), []byte(p.partner.srv.URL)))
			}
			p.g.cfg.Responder.(*nativeResponder).SetEndpointEvidence(evidence)
			ans := p.send(t, legCDexSubmit, "", cdexSubmissionBody("MBR-COVERED", cdexPayerCMS))
			if ans.status != http.StatusOK || p.partner.lastPath != row.wantPath {
				t.Fatalf("answer %d, the payer's system was called at %q, want %q", ans.status, p.partner.lastPath, row.wantPath)
			}
		})
	}
}
