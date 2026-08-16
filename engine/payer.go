// payer.go — the payer-side DTR questionnaire responder and the shared (C)
// outbound subject fence. Part of package gateway (the Smart Gateway runs every
// holder role; this file is the payer-adjudication surface). The conformant PAS
// inbound handlers live in pas_native.go; the minimized CRD/PAS handlers are no
// longer part of the contract. See gateway.go for the package doc.
package engine

import (
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleDTRInbound answers a DTR questionnaire fetch: parse the canonical, look
// up the Questionnaire fixture (unknown → 400), egress-validate it, and respond.
func (g *Gateway) handleDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	ctx := r.Context()

	result, err := g.cfg.Responder.Handle(ctx, "dtr-questionnaire-fetch", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		// build/marshal fault (gateway's own) → 500
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "responder failed"})
		return
	}
	if result.Status != 0 {
		// e.g. 400 "unknown questionnaire canonical"
		g.respondLegError(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	if status, msg := g.fenceResponseSubject("dtr-questionnaire-fetch", "", result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Egress $validate is a NEAR-RELAY for a verbatim foreign package (FR-G28, R-8
	// precedent): a real br-payer's $questionnaire-package response carries FOREIGN Da Vinci
	// DTR profiles (dtr-std-questionnaire / dtr-questionnaireresponse) that SHN — hosting US
	// Core only — cannot resolve, so a foreign-$validate would 422 a conformant payer
	// response. The trust-critical subject fence above still runs (no Questionnaire may carry
	// a subject); we skip ONLY the profile-resolution $validate the conformant crd/pas native
	// legs also skip. An SHN-PRODUCED package (the sandbox responder) is egress-$validated.
	//
	// The predicate is result.ResponseRelayed — the per-RESULT truth the native DTR case
	// sets — rather than the deployment-wide PayerDavinciNative flag it replaces. The two
	// coincide today (the composite always routes dtr-questionnaire-fetch to native, and
	// app.go sets PayerDavinciNative exactly when that composite is wired), so this is a
	// precision change, not a behavior change: it just stops a hypothetical sandbox-answered
	// DTR leg on a native deployment from skipping validation it should get.
	//
	// F7: an SHN-produced package is built at the ANSWER LINE (adjudicator.go's
	// buildQuestionnairePackageAtLine), so it must be validated on THAT line's lane — DTR 2.2
	// requires a QuestionnaireResponse entry that the 2.0 IG does not, and checking 2.2 bytes
	// against a 2.0 lane is exactly the wrong-lane failure the seam exists to prevent.
	if !result.ResponseRelayed {
		if status, msg := g.validateFHIR(ctx, result.ResponseFHIR, "egress", shnsdk.LineOf(answerTok)); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}
	// Stamp honesty: a verbatim foreign package is relayed UNSTAMPED (the same rule as the PAS
	// relay); an SHN-produced package is stamped at the line it was built at.
	g.respondLeg(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", env.Metadata.CorrelationID, result.ResponseFHIR, tok.Subject, env.Metadata.Sender, "", answerTok, result.ResponseRelayed)
}

// fenceResponseSubject is the (C) outbound fence: a connector must not swap the
// patient between the request it was handed and the response it returned. It
// compares the response resource's patient ref to boundPatientRef (the inbound
// member-namespace ref, e.g. "Patient/<member>", already proven by (A) to resolve
// to pci == tok.Subject). NOT compared to tok.Subject, which is a derived PCI.
// Returns (0,"") on pass or (status, msg) to write. Per-leg arms are added as each
// leg moves behind the seam.
func (g *Gateway) fenceResponseSubject(leg, boundPatientRef string, res LegResult) (int, string) {
	switch leg {
	case "coverage-eligibility":
		ref, err := ParseCoverageEligibilityResponsePatient(res.ResponseFHIR)
		if err != nil {
			return http.StatusInternalServerError, "parse response subject failed"
		}
		if ref != boundPatientRef {
			return http.StatusForbidden, "response patient does not match request patient"
		}
	case "dtr-questionnaire-fetch":
		// The response is a $questionnaire-package Bundle; walk its Questionnaire
		// entries (the wrapper has no top-level subject) and reject if any carries one.
		if packageQuestionnaireHasSubject(res.ResponseFHIR) {
			return http.StatusForbidden, "questionnaire response unexpectedly carries a subject"
		}
	case "pas-claim", "pas-claim-update":
		// Converged conformant PAS leg, served by either the sandbox responder (in-process,
		// SHN member namespace → fence strict) or the native-forward responder (verbatim relay
		// of a real RI answering in its OWN namespace → stand down, R-7). The responder declares
		// which via res.ResponseSubjectForeign; the SHN-produced EOB side-effect is ALWAYS fenced.
		if !res.ResponseSubjectForeign {
			refs, err := ParsePASResponsePatients(res.ResponseFHIR)
			if err != nil {
				return http.StatusInternalServerError, "parse response subject failed"
			}
			for _, ref := range refs {
				if ref != boundPatientRef {
					return http.StatusForbidden, "response patient does not match request patient"
				}
			}
		}
		// EOB Store side-effect: SHN-produced from the bound member, both paths — fence unconditionally.
		for _, se := range res.SideEffectFHIR {
			ref, err := parseEOBPatient(se)
			if err != nil {
				return http.StatusInternalServerError, "parse side-effect subject failed"
			}
			if ref != boundPatientRef {
				return http.StatusForbidden, "side-effect patient does not match request patient"
			}
		}
	}
	return 0, ""
}
