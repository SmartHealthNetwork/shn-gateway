// inbound.go — recipient-side envelope dispatch and the facility / PHG responders
// plus shared inbound helpers. Part of package gateway (the Smart Gateway runs
// every holder role; this file is the inbound-dispatch, facility-disclosure, and
// patient-authorship surface). Behavior-preserving split of gateway.go (finding C);
// no logic change. See gateway.go for the package doc.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ---- Payer role ----

func (g *Gateway) handleInbound(w http.ResponseWriter, r *http.Request) {
	// Every answer written below is this gateway's answer as the leg's
	// recipient; refusals before the leg is known are checked as such.
	w, scope := g.withScope(w, relay.RoleRecipient)
	// Per-hop transport auth: verify the Hub's X-Hub-Assertion FIRST, header only,
	// before the body is read or the envelope decoded — an unauthenticated caller
	// never reaches the decoder. Sig + issuer pin ("hub") + audience (this holder)
	// + bounds + jti one-time-use. The jti guard runs on Config.Replay: in-memory
	// (per-replica) by default, shared across every replica of the holder under
	// SHN_STORE_DATABASE_URL.
	ok, unavailable := g.verifyHubAssertion(r)
	if !ok {
		if unavailable {
			// The record could not be consulted, so this is not a failed assertion:
			// answer 503 (unavailable) rather than the 403 that would accuse the Hub of a
			// bad assertion. Being honest about WHICH refusal it is does not make the
			// delivery survive: the Hub has no retry, so it turns any non-2xx here into
			// its own 502 "forward to recipient failed" and audits the leg failed. The
			// delivery is lost either way — 503 says the cause was this holder's store,
			// not the sender's credential. Hub-side retry is a separate, tracked change.
			writeStoreUnavailable(w, "one-time-use record unavailable")
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or invalid hub assertion"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode envelope failed"})
		return
	}

	// Require the binding-critical metadata fields even though the Hub already
	// guards them — the payer must not assume an implicitly-trusted Hub channel
	// (network-separation). An empty CorrelationID would otherwise skip the token's
	// correlation binding below (VerifyBound treats "" as skip) AND be echoed into
	// the response leg (respondLeg reuses it), so a downstream audit could carry an
	// empty correlation. AuthorityFrame is required for the same defense-in-depth
	// reason; the frame is additionally pinned to the literal "provider-tpo" below.
	if env.Metadata.AuthorityFrame == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing authority frame"})
		return
	}
	if env.Metadata.CorrelationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing correlation id"})
		return
	}
	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the recipient check below is cheap defense-in-depth (a misrouted
	// or directly-injected envelope addressed to someone else is rejected). The bound
	// authz token below is the AUTHORITY check (AI-11) — both are required, neither
	// substitutes for the other.
	if env.Metadata.Recipient != g.cfg.HolderID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "envelope not addressed to this holder"})
		return
	}

	// FulfillLeg dispatch: the inbound leg's expected request op + authority frame come
	// from the SAME PA catalog the origination side reads (workstream_pa.go), so the two
	// edges cannot drift. An unknown legType has no catalog entry and is rejected 400
	// BEFORE any token verification — it is not part of the protocol surface.
	spec, known := paCatalog[env.Metadata.TransactionType]
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown transaction type"})
		return
	}
	scope.leg = env.Metadata.TransactionType

	// Every governed check this leg makes is reported as this leg: an inbound
	// request's bytes are the peer's, at the seam certify.go already names.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType:       env.Metadata.TransactionType,
		CorrelationID: env.Metadata.CorrelationID,
		Seam:          inboundSeamFor(env.Metadata.TransactionType),
		Whose:         "peer",
	}))

	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the bound authz token below is the AUTHORITY check (AI-11) —
	// both are required, neither substitutes for the other.
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid authz token"})
		return
	}
	// C2/H1: the inbound token must be bound to THIS envelope — its operation
	// (pinned per TransactionType), frame, correlationID, AND Holder (the envelope
	// Sender) must match the envelope it arrived in. This stops a valid token being
	// lifted into a different envelope/operation and replayed, and stops one holder
	// routing using another holder's token.
	if err := shnsdk.VerifyBound(tok, g.cfg.AuthzPub, g.cfg.Clock(),
		spec.ReqFrame, spec.Op, env.Metadata.CorrelationID, env.Metadata.Sender, "", sha256hex(env.Ciphertext)); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authz verification failed"})
		return
	}

	// Only verified envelope metadata may provide cross-boundary attribution.
	if leg, ok := r.Context().Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		leg.hash, leg.sender, leg.recipient, leg.correlation = sha256hex(env.Ciphertext), env.Metadata.Sender, env.Metadata.Recipient, env.Metadata.CorrelationID
	}
	r = r.WithContext(diagnostics.WithRequestIdentity(r.Context(), sha256hex(env.Ciphertext), env.Metadata.Sender, env.Metadata.Recipient, env.Metadata.CorrelationID))
	g.diagnosticStage(r.Context(), "leg.verified", env.Metadata.TransactionType, nil, 0, "")
	// Request framing: decrypt ONCE here, then resolve this leg's
	// ANSWER LINE before any handler runs — a framed request states the line the
	// originator built at (honored iff native∩laned, else a legible 422), a bare
	// one is symmetrically recomputed. Centralizing it means the honor/refuse rule
	// has exactly one implementation for all eight legs instead of eight copies,
	// and the handlers receive plaintext they no longer each decrypt.
	payload, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	g.diagnosticStage(r.Context(), "recipient.opened", env.Metadata.TransactionType, payload, 0, "")
	body, answerTok, status, msg := g.unframeRequestFrom(env.Metadata.Sender, env.Metadata.TransactionType, payload)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The answer line rides the request context so the payer's content seam
	// (LegResponder — a PUBLIC partner-implementable interface) can build at it
	// without a breaking signature change; handlers additionally receive it
	// explicitly for the frame stamp. The DECLARED SET rides alongside it so a
	// builder that must fall back falls back to what this deployment declares, not
	// to the library build constant (D1a).
	r = r.WithContext(withDeclaredContractVersions(withAnswerLine(r.Context(), answerTok), g.declaredContractVersions()))
	// A request frame may name the DTR operation its body is the input of. It
	// rides the context too, for the same reason as the answer line.
	operation, status, msg := inboundFrameOperation(env.Metadata.TransactionType, payload)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	r = r.WithContext(withRequestFrameOperation(r.Context(), operation))

	g.diagnosticStage(r.Context(), "recipient.request", env.Metadata.TransactionType, body, 0, "")
	switch env.Metadata.TransactionType {
	case "coverage-eligibility":
		g.handleEligibilityInbound(w, r, env, tok, body, answerTok)
	case "crd-order-select":
		g.handleCRDNativeInbound(w, r, env, tok, body, answerTok)
	case "crd-order-dispatch":
		g.handleCRDDispatchInbound(w, r, env, tok, body, answerTok)
	case "dtr-questionnaire-fetch":
		g.handleDTRInbound(w, r, env, tok, body, answerTok)
	case "pas-claim":
		// R8 re-home (FR-16/FR-27): fence BEFORE dispatch — an unattested
		// clinician/patient QR item is nonconformant regardless of which handler
		// would otherwise run. The attestation is the QR's own content
		// (RuleAttestation): not checked at none, recorded at observe and
		// forwarded, refused at strict; the same on the two legs below.
		if reason, ok := fenceAttestedItems(body); !ok && g.guard(r.Context(), KindContent, RuleAttestation, body) {
			g.refuseInbound(w, r, legPASClaim, env, tok, answerTok, http.StatusForbidden, reason, nil)
			return
		}
		g.handlePASNativeInbound(w, r, env, tok, body, answerTok)
	case "pas-claim-update":
		// R8 re-home (FR-16/FR-27): same fence as pas-claim above — the property
		// belongs to any QR item, not only to amends.
		if reason, ok := fenceAttestedItems(body); !ok && g.guard(r.Context(), KindContent, RuleAttestation, body) {
			g.refuseInbound(w, r, legPASClaimUpdate, env, tok, answerTok, http.StatusForbidden, reason, nil)
			return
		}
		g.handlePASUpdateNativeInbound(w, r, env, tok, body, answerTok)
	case "pas-claim-inquire":
		// R8 re-home (FR-16/FR-27): the same fence as the two legs above. An
		// inquiry's profile gives it no QuestionnaireResponse, so the fence is
		// expected to pass — but the property belongs to any QR item wherever it
		// arrives, and a fence that runs on two of three PAS legs is a gap waiting
		// for the third to carry one.
		if reason, ok := fenceAttestedItems(body); !ok && g.guard(r.Context(), KindContent, RuleAttestation, body) {
			g.refuseInbound(w, r, legPASClaimInquire, env, tok, answerTok, http.StatusForbidden, reason, nil)
			return
		}
		g.handlePASInquireInbound(w, r, env, tok, body, answerTok)
	case "federated-query":
		g.handleFederatedQueryInbound(w, r, env, tok, body, answerTok)
	case "patient-dtr":
		g.handlePatientDTRInbound(w, r, env, tok, body, answerTok)
	}
}

// inboundSeamFor names the seam a finding belongs to, matching the seam
// strings certificationPair already records ("payer-native",
// "provider-ingress").
func inboundSeamFor(legType string) string {
	switch legType {
	case "federated-query":
		return "facility-inbound"
	case "patient-dtr":
		return "phg-inbound"
	default:
		return "payer-native"
	}
}

// handleEligibilityInbound is the UC-01 coverage-eligibility inbound logic,
// unchanged from the original handleInbound body.
func (g *Gateway) handleEligibilityInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, cerJSON []byte, answerTok string) {
	ctx := r.Context()

	// Data minimization at the holder boundary: bind the token subject to the
	// payload's patient via a CHEAP json.Unmarshal-level parse BEFORE any external
	// $validate. A wrong-patient payload must be rejected here so it never leaves
	// the holder boundary for the (shared) validator. The Parse* helpers operate on
	// un-$validate'd JSON and return errors (not panics) on malformed input, so a
	// malformed payload fails closed with 400 before $validate — nothing leaks.
	member, err := shnsdk.ParseEligibilityRequestMember(cerJSON)
	if err != nil {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, "parse member failed", nil)
		return
	}

	// H2a: bind the token's subject to the payload's patient. The token authorizes
	// a specific PCI; resolving the CER's member must yield that same PCI. This
	// stops a token authorizing patient A being paired with a payload for patient B.
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusBadRequest, refusalUnknownMember, nil)
		return
	}
	if pci != tok.Subject {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusForbidden, "token subject does not match request patient", nil)
		return
	}

	// Only AFTER the subject binds do we ingress-validate the clinical payload via
	// the external $validate — fail-closed as before.
	//
	// F7: the lane is selected per LINE like every other validate, but this
	// site deliberately does NOT route through g.validateFHIR — its failure contract
	// (422 on !Valid, with the choke point's bounded govResult.Issues echoed)
	// differs from validateFHIR's, and unifying them would change the wire.
	// The line comes from the same
	// shnsdk.LineOf(answerTok) call every other site uses — it is not special-cased
	// here — and it evaluates to "" (the canonical lane) because coverage-eligibility
	// is version-neutral (paCatalog Contract ""), so answerTok itself is always "".
	// A nil lane keeps THIS site's 500 rather than borrowing another's.
	// Unconditional on purpose, not an oversight — this is the PAYER'S side of the SAME
	// eligibility exchange originate.go's two UC-01 sites
	// cover; cerJSON here is the REQUEST the requesting gateway's own engine built
	// (shnsdk.BuildEligibilityRequest — never a foreign relay, since only SHN gateways ever
	// originate a substrate leg), so it is SHN-produced on every lane and always validates.
	ingressValidator := g.validatorForContractLine(strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok))
	// Routed through the choke point so an invalid inbound request emits its
	// conformance finding. handleInbound already tagged this leg's context
	// (Whose "peer" — these are the requester's own bytes), so it is read as-is.
	// This site's own status/message contract is preserved explicitly below.
	if gr := g.validateGoverned(ctx, findingContextFrom(ctx), ingressValidator, cerJSON, "ingress", shnsdk.LineOf(answerTok), "", false); gr.Status != 0 {
		if gr.NoLane {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no FHIR validator lane configured for this leg (FR-36/FR-G29)"})
			return
		}
		if gr.Status == http.StatusInternalServerError {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
			return
		}
		// The issues echo is now BOUNDED (findingIssuesShown + "and N more"),
		// where it was unbounded before the migration — see the PR body.
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, http.StatusUnprocessableEntity, refusalIngressValidation,
			map[string]any{"error": refusalIngressValidation, "issues": gr.Issues})
		return
	}

	boundPatientRef := "Patient/" + member

	// Coverage-derived direct read (R11): eligibility is a data-plane
	// read of the payer's OWN SoR, not adjudication, so it is answered directly —
	// NEVER through the injected Adjudicator/Responder occupant (which is consulted
	// only for the PA legs: OrderSelect/Questionnaire/PriorAuth). One Coverage read
	// feeds both facts, the payer twin of FR-G40's "one payer fact, read once":
	// CoverageInforce is the verdict, OpenCoverage's payor (parsed the SAME way the
	// origination-side machinery does, gateway.go's recipientForWith /
	// shnsdk.ParsePayerIdentifier) is the insurer identity — replacing the
	// hardcoded Organization/payer literal BuildEligibilityResponse used to stamp.
	inforce, reason, readErr := ReadSystemOfRecord(g.cfg.SoR).CoverageInforceContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return
	}
	coverageJSON, hasCoverage, status, msg := g.memberCoverage(r.Context(), member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	var insurer shnsdk.PayerIdentifier
	switch {
	case hasCoverage:
		// The member HAS a Coverage record: its own payor is the insurer. An
		// unresolvable payor on an EXISTING Coverage fails closed (R4:
		// CoverageEligibilityResponse.insurer is 1..1) — a hollow or
		// stale-literal response is never built.
		var ok bool
		resolve, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
		insurer, ok = shnsdk.ParsePayerIdentifier(coverageJSON, resolve)
		if writeSoRFailure(w, *readErr) {
			return
		}
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no payer identifier on member coverage"})
			return
		}
	default:
		// No Coverage row at all (a genuinely unenrolled member, or the
		// MBR-UC04/MBR-UC08-class fixture gap — the member itself already resolved,
		// via ResolvePatient above, so this is a Coverage absence, not an unknown
		// member) is the PRE-EXISTING business answer: a valid not-covered response
		// (CoverageInforce already returns (false,"") for this case — no disposition
		// text, matching the pre-promotion behavior), never a 422. There is no
		// member Coverage to name an insurer from, so this reads the payer's OWN
		// well-known Organization instead (Organization/payer — the same literal
		// every other payor reference in this codebase already resolves against,
		// seeded with an identifier by internal/fhirseed) — one self-read, not a
		// fabricated identity.
		orgJSON, orgFound, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(r.Context(), "Organization/payer")
		if writeSoRFailure(w, readErr) {
			return
		}
		if !orgFound {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "payer's own well-known Organization is not on file"})
			return
		}
		var ok bool
		insurer, ok = shnsdk.ParseOrganizationIdentifier(orgJSON)
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "payer's own well-known Organization has no identifier"})
			return
		}
	}
	crrJSON, err := shnsdk.BuildEligibilityResponse(env.Metadata.CorrelationID, boundPatientRef, inforce, reason, insurer, g.cfg.Clock())
	if err != nil {
		// Build/marshal fault (gateway's own) → 500, parity with today's
		// StatusInternalServerError build-failure paths. NOT 502.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build eligibility response failed"})
		return
	}
	answer, err := relay.Authored(relay.BuilderSDKEligibility, crrJSON, "application/fhir+json")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build eligibility response failed"})
		return
	}
	result := LegResult{Response: answer}
	responseFHIR, err := g.admit(result.Response, answerKey("coverage-eligibility", relay.OutcomeAnswered))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return
	}
	if status, msg := g.fenceResponseSubject("coverage-eligibility", boundPatientRef, env.Metadata.CorrelationID, result); status != 0 {
		g.refuseInbound(w, r, legEligibility, env, tok, answerTok, status, msg, nil)
		return
	}
	// Egress $validate, behavior-identical to the pre-seam inline path: validator
	// error → 500 "validator unavailable"; !Valid → 500 "egress validation failed".
	// (NOT g.validateFHIR, which returns 422 on !Valid — that would change the
	// failure-path contract.)
	// F7: per-line lane selection, same "" (version-neutral) line as the ingress site
	// above — and, as the note says, deliberately NOT g.validateFHIR: this site's
	// !Valid answer is a 500, not a 422, and that distinction is the contract.
	// Egress is unconditional by definition (R-8 is an ingress-only carve-out) — crrJSON
	// here is always SHN-produced
	// (shnsdk.BuildEligibilityResponse), so this was never in scope for the skip either way.
	egressValidator := g.validatorForContractLine(strings.SplitN(answerTok, "@", 2)[0], shnsdk.LineOf(answerTok))
	// Routed through the choke point so an invalid egress response still emits
	// its conformance finding. responseFHIR is THIS gateway's own built answer,
	// not the inbound request's bytes, so Whose is overridden to "own" (the
	// context the handler entry tagged names the peer's inbound leg). Both
	// outcomes here answer 500 (unchanged from before the migration) — the
	// choke point's own message already matches this site's literals
	// byte-for-byte in both cases, so it is relayed directly.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	if gr := g.validateGoverned(ctx, fc, egressValidator, responseFHIR, "egress", shnsdk.LineOf(answerTok), "", false); gr.Status != 0 {
		msg := gr.Msg
		if gr.NoLane {
			msg = "no FHIR validator lane configured for this leg (FR-36/FR-G29)"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
		return
	}

	g.respondLeg(w, r, "payer-coverage", "eligibility-response", "coverage-eligibility", env.Metadata.CorrelationID, result.Response, tok.Subject, env.Metadata.Sender, "", answerTok)
}

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
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "federated query missing consent reference", nil)
		return
	}

	// (4) Narrowness: parse + validate the query shape BEFORE any disclosure. A
	// missing/disallowed type or absent patient fails here (no bulk, FR-26).
	parsed, err := shnsdk.ParseCDexTaskDataRequest(queryJSON)
	if err != nil {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "query rejected: "+err.Error(), nil)
		return
	}

	// (3) Bind the token subject to the queried patient.
	member := strings.TrimPrefix(parsed.PatientRef, "Patient/")
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusBadRequest, refusalUnknownMember, nil)
		return
	}
	if pci != tok.Subject {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusForbidden, "token subject does not match queried patient", nil)
		return
	}

	// (2) Consent backstop: independently confirm the four-way permit AND learn the
	// authenticated consent reference the consent service reports. The facility
	// refuses to release even if a bad token reached it. The returned ref —
	// not the provider-supplied wire field — is what anchors the Provenance below
	// (attribution integrity, FR-32/C11).
	consentRef, status, msg := g.consentBackstop(ctx, pci, env.Metadata.Sender)
	if status != 0 {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, status, msg, nil)
		return
	}
	// Defense in depth: the carried wire ref must match the authenticated one, so a
	// forged Metadata.ConsentRef cannot diverge from the permit that authorized this.
	if env.Metadata.ConsentRef != consentRef {
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
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// The answer extends the payer's own request Task and embeds the records
	// Bundle exactly; every copied span is declared and verified when sealed.
	answer, fulfilled, err := sealCDexFulfillment(queryJSON, inner)
	var refused *cdexRequestRefused
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": refused.reason})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build cdex result failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, fulfilled, "egress", ""); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	g.respondLeg(w, r, "facility-disclosure", "federated-query-response", "federated-query", env.Metadata.CorrelationID, answer, tok.Subject, env.Metadata.Sender, consentRef, answerTok)
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

// recordClinicalDate pulls the date a facility record is selected on for FR-24
// (and ordered by as evidence): DiagnosticReport.effectiveDateTime, else the
// end of DiagnosticReport.effectivePeriod, else its start; or
// DocumentReference.date. "" if absent.
func recordClinicalDate(resJSON []byte) string {
	var r struct {
		EffectiveDateTime string `json:"effectiveDateTime"`
		EffectivePeriod   struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"effectivePeriod"`
		Date string `json:"date"`
	}
	_ = json.Unmarshal(resJSON, &r)
	for _, d := range []string{r.EffectiveDateTime, r.EffectivePeriod.End, r.EffectivePeriod.Start} {
		if d != "" {
			return d
		}
	}
	return r.Date
}

// consentBackstop independently confirms a TREAT permit (custodian = this
// facility, recipient = requester) via the Trust-operated consent service and
// returns the AUTHENTICATED consent reference the service reports — the ref that
// anchors the disclosure's source Provenance (never the provider-supplied wire
// field). When ConsentURL is unset it fails closed (no consent service ⇒ no
// disclosure). Returns (consentRef, 0, "") on permit, or ("", status, msg) on refusal.
func (g *Gateway) consentBackstop(ctx context.Context, pci, requester string) (string, int, string) {
	if g.cfg.ConsentURL == "" {
		return "", http.StatusForbidden, "consent service unavailable"
	}
	var out shnsdk.ConsentCheckResponse
	req := shnsdk.ConsentCheckRequest{PCI: pci, Purpose: shnsdk.PurposeTreatment, Custodian: g.cfg.HolderID, Recipient: requester}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", http.StatusInternalServerError, "consent auth"
	}
	assertionHdr, err := g.cfg.Identity.AssertionForBody("consent", g.cfg.Clock(), shnsdk.MaxAssertionTTL, raw)
	if err != nil {
		return "", http.StatusInternalServerError, "consent auth"
	}
	hdr := map[string]string{"X-Holder-Assertion": assertionHdr}
	if err := shnsdk.PostRaw(ctx, g.cfg.Client, g.cfg.ConsentURL+"/check", raw, &out, hdr); err != nil {
		return "", http.StatusBadGateway, "consent check failed"
	}
	if !out.Permit {
		return "", http.StatusForbidden, "no active consent for this disclosure"
	}
	return out.ConsentRef, 0, ""
}

// patientDTRRequest is the provider→PHG ask: please have the patient author + attest
// this questionnaire item. It carries the patient's raw answer (entered at the patient
// surface) + the item link id + the patient ref. The PHG applies the patient
// attestation (the questionnaireresponse-signature) on the patient's authority.
type patientDTRRequest struct {
	LinkID     string `json:"linkId"`
	Answer     string `json:"answer"`
	PatientRef string `json:"patientRef"`
}

// patientDTRResponse carries the patient-authored, attested QR item back to the provider.
type patientDTRResponse struct {
	AttestedItem json.RawMessage `json:"attestedItem"`
}

// handlePatientDTRInbound is the Trust-operated PHG responder (UC-07). It binds the
// token subject to the patient, builds the patient-authored attested item REQUEST-
// SCOPED (questionnaireresponse-signature, who=Patient), and responds on the
// patient-authorship frame. The PHG persists NOTHING (OWD-8/AI-3): the patient's
// answer arrives in the request and the attested item leaves in the response.
func (g *Gateway) handlePatientDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	var req patientDTRRequest
	if err := decodeMessage(reqJSON, &req); err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, "parse patient-dtr request failed", nil)
		return
	}
	// H2: bind the token subject to the patient the request names (the patient whose
	// authorship the Trust surface is exercising).
	member := strings.TrimPrefix(req.PatientRef, "Patient/")
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, refusalUnknownMember, nil)
		return
	}
	if pci != tok.Subject {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusForbidden, "token subject does not match patient", nil)
		return
	}

	// #5: the Trust signer validates the answer against the item's constraint
	// before attesting — it must not sign a non-conformant patient answer. This
	// is the authoritative, un-bypassable guard (AI-10 fiduciary surface).
	if err := shnsdk.ValidatePatientAnswer(req.LinkID, req.Answer); err != nil {
		g.refuseInbound(w, r, legPatientDTR, env, tok, answerTok, http.StatusBadRequest, "invalid patient answer: "+err.Error(), nil)
		return
	}

	attested, err := shnsdk.BuildPatientAttestedItem(req.LinkID, req.Answer, req.PatientRef, g.cfg.Clock().Format("2006-01-02"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build patient-attested item failed"})
		return
	}
	respJSON, err := json.Marshal(patientDTRResponse{AttestedItem: attested})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "marshal response failed"})
		return
	}
	answer, err := relay.Authored(relay.BuilderSDKPatientDTR, respJSON, "application/json")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "marshal response failed"})
		return
	}
	g.respondLeg(w, r, "patient-authorship", "patient-dtr-response", "patient-dtr", env.Metadata.CorrelationID, answer, tok.Subject, env.Metadata.Sender, "", answerTok)
}

// verifyHubAssertion checks the X-Hub-Assertion header: a shnsdk.Assertion
// signed by the Hub's transport key with HolderID "hub" (issuer pin — a second
// Hub would join the verified contract here) and Audience == this holder, within
// bounds, jti unused. Fails closed on every malformed input.
//
// The second return says WHY a false refused: unavailable=true means the one-time-use
// record could not be consulted (the caller answers 503), false means the assertion
// itself was rejected (403). The refusal is the same either way — an unrecorded jti is
// never admitted — and the delivery is lost either way, since the Hub does not retry a
// failed forward. What the distinction buys is a truthful cause: a database outage is
// not reported to the Hub as a bad assertion.
func (g *Gateway) verifyHubAssertion(r *http.Request) (ok bool, unavailable bool) {
	hdr := r.Header.Get("X-Hub-Assertion")
	if hdr == "" {
		return false, false
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		return false, false
	}
	var a shnsdk.Assertion
	if err := json.Unmarshal(raw, &a); err != nil {
		return false, false
	}
	if a.HolderID != "hub" {
		return false, false
	}
	if shnsdk.VerifyAssertion(a, g.cfg.HolderID, g.cfg.HubTransportPub, g.cfg.Clock()) != nil {
		return false, false
	}
	if len(a.JTI) > MaxReplayKeyBytes {
		// The Hub's own jti, refused with the ordinary denial before the record is
		// consulted: an oversized key would otherwise be reported as a store outage on a
		// healthy database (see MaxReplayKeyBytes).
		return false, false
	}
	now := g.cfg.Clock()
	// A record that could not be consulted refuses exactly as a replay does (fail
	// closed — an unrecorded jti is never admitted), but it is reported as an outage so
	// the cause is legible: the Hub audits the forward failed either way.
	replayed, err := g.replay.CheckAndRecord(ReplayScopeHubJTI, "", a.JTI, now, now.Add(shnsdk.MaxAssertionTTL))
	if err != nil {
		log.Printf("gateway: hub assertion one-time-use record unavailable: %v (refusing)", err)
		g.noteStoreError(storeErrReplay)
		return false, true
	}
	return !replayed, false
}
