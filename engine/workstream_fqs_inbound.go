package engine

import (
	"errors"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleFederatedQueryInbound is the facility source-side handler (UC-05, consent
// backstop). The Hub-verified request token is already bound (handleInbound), but
// the facility re-enforces, independently: (1) the leg carries a consent ref; (2)
// consentsvc confirms a TREAT permit whose custodian is THIS facility and whose
// recipient is the requester (defense in depth, AI-10/AI-13); (3) the token
// subject binds to the queried patient; (4) the query is NARROW — named allowed
// types only, never bulk (FR-26, AI-1). It then returns ONLY the named records
// with a source Provenance whose .policy cites the consent (FR-32).
//
// DEF-10: single-step federated query (query and response in one Hub round-trip)
// is the connectathon topology; an async/paged query is an additive fast-follow
// (AI-9 holds: the consent check + narrowness enforcement here are unchanged when
// adding pagination, so no one-way door is taken).
func (g *Gateway) handleFederatedQueryInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, queryJSON []byte, answerTok string) {
	ctx := r.Context()

	// (1) The leg MUST carry a consent reference.
	if env.Metadata.ConsentRef == "" {
		exchangeOf(r.Context()).refusing(RefusalConsent)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "federated query missing consent reference", nil)
		return
	}

	// (4) Narrowness: parse + validate the query shape BEFORE any disclosure. A
	// missing/disallowed type or absent patient fails here (no bulk, FR-26).
	// Task.for and the query's patient are read in the relative form, so an
	// absolute or contained reference names its patient as a relative one does
	// (workstream_fqs_task.go).
	parsed, member, err := cdexTaskQuery(queryJSON)
	if err != nil {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "query rejected: "+err.Error(), nil)
		return
	}

	// (3) Bind the token subject to the queried patient. The Authorization
	// Framework checked consent for the token's patient, so a queried patient
	// this facility cannot identify as that one is refused on consent at every
	// level. One it does not hold reads the same as one that is someone else,
	// so the refusal does not tell the requester whether the facility holds
	// the queried patient.
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if g.refuseUnreadableMember(w, r, legFederatedQuery, env, tok, answerTok, refusalPatientNotConsented, readErr) ||
		g.refuseSoRFailure(w, r, legFederatedQuery, env, tok, answerTok, readErr) {
		return
	}
	if !found || pci != tok.Subject {
		exchangeOf(r.Context()).refusing(RefusalConsent)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, refusalPatientNotConsented, nil)
		return
	}

	// (2) Consent backstop: independently confirm the four-way permit AND learn the
	// authenticated consent reference the consent service reports. The facility
	// refuses to release even if a bad token reached it. The returned ref —
	// not the provider-supplied wire field — is what anchors the Provenance below
	// (attribution integrity, FR-32/C11).
	consentRef, status, msg := g.consentBackstop(ctx, pci, env.Metadata.Sender)
	if status != 0 {
		exchangeOf(r.Context()).refusing(RefusalConsent)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, status, msg, nil)
		return
	}
	// Defense in depth: the carried wire ref must match the authenticated one, so a
	// forged Metadata.ConsentRef cannot diverge from the permit that authorized this.
	if env.Metadata.ConsentRef != consentRef {
		exchangeOf(r.Context()).refusing(RefusalConsent)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "consent reference mismatch", nil)
		return
	}

	// The direction flips here: everything validated from this point on — the
	// facility's own held records, its authored Patient identity binding, its
	// authored Provenance, and the sealed fulfillment answer below — is THIS
	// participant's own build, not the peer's request handleInbound tagged the
	// context with. facilityRecordsBundle takes ctx directly, so this one retag
	// covers all of its internal checks too.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	ctx = withFindingContext(ctx, fc)
	// Disclose ONLY the named records for THIS member (minimum-necessary): every
	// record of each named type within the stated dates, exactly as the system
	// of record holds them, with the member's Patient and a source Provenance
	// per record citing the AUTHENTICATED consent ref (FR-24/FR-32/C11).
	inner, _, status, msg := g.facilityRecordsBundle(ctx, member, parsed.Queries, consentRef)
	if status != 0 {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, status, msg, nil)
		return
	}
	// The answer extends the payer's own request Task and embeds the records
	// Bundle exactly; every copied span is declared and verified when sealed.
	answer, fulfilled, err := sealCDexFulfillment(queryJSON, inner)
	var refused *cdexRequestRefused
	if errors.As(err, &refused) {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusUnprocessableEntity, refused.reason, nil)
		return
	}
	if err != nil {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusInternalServerError, "build cdex result failed", nil)
		return
	}
	if status, msg := g.validateFHIR(ctx, fulfilled, "egress", ""); status != 0 {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, status, msg, nil)
		return
	}
	g.respondLeg(w, r, "facility-disclosure", "federated-query-response", "federated-query", env.Metadata.CorrelationID, answer, http.StatusOK, tok.Subject, env.Metadata.Sender, consentRef, answerTok)
}

// sealCDexFulfillment fulfills the payer's CDex request Task with the
// facility's records Bundle and seals the result as the registered
// cdex-fulfillment answer. Every part the fulfillment copies from the request
// or the records is declared as an embed, so sealing verifies it is
// byte-identical to its source. It returns the answer and its bytes (for the
// egress validation the gateway runs before sending).
//
// A request Task the fulfillment cannot extend (not one well-formed Task,
// signed, or in a status that does not allow fulfillment) is the requester's
// and is returned as a *cdexRequestRefused; any other error is a fault in the
// facility's own build.
func sealCDexFulfillment(requestTask, records []byte) (relay.Payload, []byte, error) {
	var none relay.Payload
	task := relay.NewBody(requestTask, relay.OriginPeerFrame)
	if !cdexRequestShapeOK(task) {
		return none, nil, &cdexRequestRefused{reason: "federated query refused: the request is not one well-formed Task"}
	}
	f, err := shnsdk.BuildCDexFulfillment(requestTask, records)
	switch {
	case errors.Is(err, shnsdk.ErrCDexSignedContent):
		return none, nil, &cdexRequestRefused{reason: "federated query refused: the request Task carries signed content, which a fulfillment cannot extend", err: err}
	case errors.Is(err, shnsdk.ErrCDexTaskStatus):
		return none, nil, &cdexRequestRefused{reason: "federated query refused: the request Task's status does not allow fulfillment", err: err}
	case err != nil:
		return none, nil, err
	}
	held := relay.NewBody(records, relay.OriginUpstreamResponse)
	embeds := make([]relay.Embed, 0, len(f.Copied))
	for _, c := range f.Copied {
		src := task
		if c.FromRecords {
			src = held
		}
		embeds = append(embeds, relay.Embed{Source: src, Start: c.Start, End: c.End, At: c.At})
	}
	p, err := relay.Authored(relay.BuilderCDexFulfillment, f.Task, "application/fhir+json", embeds...)
	if err != nil {
		return none, nil, err
	}
	return p, f.Task, nil
}

// cdexRequestRefused is a request Task the facility cannot fulfil because of
// the Task itself; reason names the refusal without echoing the Task.
type cdexRequestRefused struct {
	reason string
	err    error
}

func (e *cdexRequestRefused) Error() string { return e.reason }

func (e *cdexRequestRefused) Unwrap() error { return e.err }

// cdexRequestShapeOK reports whether task is one well-formed JSON document
// (no repeated member names) that is a Task whose contained and output, when
// present, are arrays: the shape a fulfillment extends.
func cdexRequestShapeOK(task relay.Body) bool {
	d, err := relay.Doc(task)
	if err != nil || d.Kind(d.Root()) != relay.KindObject {
		return false
	}
	rt, ok := d.Member(d.Root(), "resourceType")
	if !ok || d.Kind(rt) != relay.KindString {
		return false
	}
	if v, err := d.StringValue(rt); err != nil || v != "Task" {
		return false
	}
	for _, key := range []string{"contained", "output"} {
		if v, ok := d.Member(d.Root(), key); ok && d.Kind(v) != relay.KindArray {
			return false
		}
	}
	return true
}
