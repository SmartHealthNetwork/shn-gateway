// inbound.go — recipient-side envelope dispatch: the core's checks on an
// inbound leg, the hand-off to its workstream's answer, and the shared inbound
// helpers (the consent backstop, the Hub's assertion). The legs' own handlers
// are their workstreams' (the PA adapter's workstream_pa_*.go). See gateway.go
// for the package doc.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func (g *Gateway) handleInbound(w http.ResponseWriter, r *http.Request) {
	// The responder's deadline for its participant's system counts from here
	// (WithBackendDeadline): wall time, as the requester's own budget runs.
	r = r.WithContext(withLegArrival(r.Context(), time.Now()))
	// Every answer written below is this gateway's answer as the leg's
	// recipient; refusals before the leg is known are checked as such.
	w, scope := g.withScope(w, relay.RoleRecipient)
	// Only the requester guard below may state that this gateway took no
	// action on a leg (NotActedHeader); every other answer is stripped of it.
	fence := &notActedFence{ResponseWriter: w}
	w = fence
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
		exchangeOf(r.Context()).refused(RefusalAuthentication)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or invalid hub assertion"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		exchangeOf(r.Context()).refused(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		exchangeOf(r.Context()).refused(RefusalIntegrity)
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
		exchangeOf(r.Context()).refused(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing authority frame"})
		return
	}
	if env.Metadata.CorrelationID == "" {
		exchangeOf(r.Context()).refused(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing correlation id"})
		return
	}
	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the recipient check below is cheap defense-in-depth (a misrouted
	// or directly-injected envelope addressed to someone else is rejected). The bound
	// authz token below is the AUTHORITY check (AI-11) — both are required, neither
	// substitutes for the other.
	if env.Metadata.Recipient != g.cfg.HolderID {
		exchangeOf(r.Context()).refused(RefusalRouting)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "envelope not addressed to this holder"})
		return
	}

	// FulfillLeg dispatch: the inbound leg's expected request op + authority frame come
	// from the SAME compiled-in catalogs the origination side reads (workstreams.go), so
	// the two edges cannot drift. An unknown legType has no catalog entry and is rejected
	// 400 BEFORE any token verification — it is not part of the protocol surface.
	spec, known := legSpecFor(env.Metadata.TransactionType)
	if !known {
		exchangeOf(r.Context()).refused(RefusalRouting)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown transaction type"})
		return
	}
	scope.leg = env.Metadata.TransactionType
	exchangeOf(r.Context()).leg(env.Metadata.TransactionType)

	// Every governed check this leg makes is reported as this leg: an inbound
	// request's bytes are the peer's, at the seam certify.go already names.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType:       env.Metadata.TransactionType,
		CorrelationID: env.Metadata.CorrelationID,
		Seam:          spec.Seam,
		Whose:         "peer",
	}))
	// The payer's own binding of the member, when it differs from the leg
	// token's patient, is named on the answer's involved list (involved.go).
	r = r.WithContext(g.withInvolvedCollector(r.Context(), env.Metadata.TransactionType))

	// Per-hop transport auth: the Hub's X-Hub-Assertion was verified at the top of
	// this handler; the bound authz token below is the AUTHORITY check (AI-11) —
	// both are required, neither substitutes for the other.
	var tok shnsdk.Token
	if err := json.Unmarshal([]byte(env.Metadata.AuthzToken), &tok); err != nil {
		exchangeOf(r.Context()).refused(RefusalAuthority)
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
		exchangeOf(r.Context()).refused(RefusalAuthority)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authz verification failed"})
		return
	}

	// Only verified envelope metadata may provide cross-boundary attribution.
	x := exchangeOf(r.Context())
	x.correlation(env.Metadata.CorrelationID)
	x.peers(env.Metadata.Sender, env.Metadata.Recipient)
	x.requestHash(sha256hex(env.Ciphertext))
	if leg, ok := r.Context().Value(diagnosticLegKey{}).(*diagnosticLeg); ok {
		leg.hash, leg.sender, leg.recipient, leg.correlation = sha256hex(env.Ciphertext), env.Metadata.Sender, env.Metadata.Recipient, env.Metadata.CorrelationID
	}
	r = withDiagnosticIdentity(r, env)
	g.diagnosticStage(r.Context(), "leg.verified", env.Metadata.TransactionType, nil, 0, "")
	// The answer is sealed to the requester's registered key, so a requester
	// this gateway's registry does not hold yet cannot be answered. It is
	// resolved here, before the leg is opened or any handler acts on it: a
	// requester still unknown after one bounded refresh is refused, retryably,
	// with nothing done (requester.go). Routing and addressing, the same at
	// every conformance level.
	if !g.requesterKnown(r.Context(), env.Metadata.Sender) {
		x.refused(RefusalRouting)
		refuseRequesterUnknown(w, fence)
		return
	}
	// Request framing: decrypt ONCE here, then resolve this leg's
	// ANSWER LINE before any handler runs — a framed request states the line the
	// originator built at (honored iff native∩laned, else a legible 422), a bare
	// one is symmetrically recomputed. Centralizing it means the honor/refuse rule
	// has exactly one implementation for all eight legs instead of eight copies,
	// and the handlers receive plaintext they no longer each decrypt.
	payload, err := shnsdk.Open(env, g.cfg.Identity.EncPub, g.cfg.Identity.EncPriv)
	if err != nil {
		x.refused(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decryption failed"})
		return
	}
	g.diagnosticStage(r.Context(), "recipient.opened", env.Metadata.TransactionType, payload, 0, "")
	body, answerTok, carried, status, msg := g.unframeRequestFrom(env.Metadata.Sender, env.Metadata.TransactionType, payload)
	if status != 0 {
		x.refusing(frameRefusalRule(status))
		g.refuseInboundRequest(w, r, env, tok, status, msg)
		return
	}
	// A responder that carries across lines on this leg raises the same event
	// as the gateway does (linecarry.go).
	r = r.WithContext(withLineObserver(r.Context(), g.observe))
	if carried != "" {
		// The request claims a line this gateway cannot answer at: carried to
		// the participant and answered at the line a bare request gets.
		noteLineCarried(r.Context(), LineCarriedRequest, ObserverEvent{
			Direction: "ingress", LegType: env.Metadata.TransactionType, CorrelationID: env.Metadata.CorrelationID,
			Counterpart: env.Metadata.Sender, Detail: "request claims " + boundedLine(carried) + "; answered at " + answeredAt(answerTok),
		})
	}
	// The answer line rides the request context so the payer's content seam
	// (LegResponder — a PUBLIC partner-implementable interface) can build at it
	// without a breaking signature change; handlers additionally receive it
	// explicitly for the frame stamp. The DECLARED SET rides alongside it so a
	// builder that must fall back falls back to what this deployment declares, not
	// to the library build constant (D1a).
	x.line(shnsdk.LineOf(answerTok))
	r = r.WithContext(withDeclaredContractVersions(withAnswerLine(r.Context(), answerTok), g.declaredContractVersions()))
	if contract, line, ok := strings.Cut(answerTok, "@"); ok && line != "" {
		r = r.WithContext(withAnswerCandidateLines(r.Context(), func(payload []byte) []string {
			return g.answerCandidateLines(contract, line, payload)
		}))
	}
	// The media type the sender framed for a FHIR request rides the context to
	// the native forward (mediatype.go).
	r = r.WithContext(withRequestMediaType(r.Context(), inboundFrameMediaType(payload)))
	// A request frame may name the operation its body is the input of
	// (legSpec.OperationHeader). It rides the context too, for the same reason
	// as the answer line.
	operation, status, msg := inboundFrameOperation(env.Metadata.TransactionType, payload)
	if status != 0 {
		x.refusing(frameRefusalRule(status))
		g.refuseInboundRequest(w, r, env, tok, status, msg)
		return
	}
	x.operation(operation)
	r = r.WithContext(withRequestFrameOperation(r.Context(), operation))

	g.diagnosticStage(r.Context(), "recipient.request", env.Metadata.TransactionType, body, 0, "")
	// The leg is verified and open: what follows is its answer.
	x.unwrapDone()
	if answer := answerFor(env.Metadata.TransactionType); answer != nil {
		answer(g, w, r, env, tok, body, answerTok)
		return
	}
	// A leg every check above admitted, with no handler to answer it: this
	// gateway's own failure, never the requester's, so it names no refusal
	// rule. It is answered rather than dropped: a handler that wrote nothing
	// would answer 200 and record the leg answered. Every leg the compiled-in
	// workstreams declare has a handler (TestInbound_EveryLegHasAnAnswerHandler).
	g.refuseInboundRequest(w, r, env, tok, http.StatusInternalServerError, errNoAnswerHandler+" "+env.Metadata.TransactionType)
}

// answerHandler answers one verified, open inbound leg: body is its
// decrypted request and answerTok the contract line it is answered at.
type answerHandler func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string)

// errNoAnswerHandler opens the answer to a leg the gateway admitted and has no
// handler for.
const errNoAnswerHandler = "no answer handler for leg"

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
