//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
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
	if errors.Is(err, errMemberNamesNoOne) {
		// Its patient names no one: the subject binding's refusal.
		exchangeOf(r.Context()).refusing(RefusalAuthority)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusBadRequest, msgMemberNamesNoOne, nil)
		return
	}
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
	// The answer extends the requester's own request Task and embeds the
	// records Bundle exactly; every copied span is declared and verified when
	// sealed.
	answer, fulfilled, err := sealCDexFulfillment(queryJSON, inner)
	var declined *cdexDeclined
	if errors.As(err, &declined) {
		g.answerCDexDecline(w, r, env, tok, consentRef, answerTok, declined)
		return
	}
	var refused *cdexRequestRefused
	if errors.As(err, &refused) {
		// The rule the refusal's answer settles under, unless that answer
		// itself fails its own check: a request that cannot be read one way
		// is refused on message integrity; a Task the gateway cannot extend
		// without changing what it asserts (signed content, or a shape its
		// edit cannot be made in), or past its own limits, on the fidelity
		// of its own edit, at the build.
		switch refused.rule {
		case cdexRefusedIntegrity:
			exchangeOf(r.Context()).refusing(RefusalIntegrity)
		case cdexRefusedAtBuild:
			exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
		}
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusUnprocessableEntity, refused.reason, nil)
		return
	}
	// A fulfillment past the gateway's own carrying limits, which extends the
	// request Task with the facility's own records, is refused 502 as the
	// fidelity of the gateway's own build, with no Retry-After (no retry
	// succeeds until the request or those records change), naming no record,
	// since the answer goes to another participant. Nothing of the
	// fulfillment is sent: no record is dropped to make it fit.
	var past *cdexBuildPastLimits
	if errors.As(err, &past) {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusBadGateway, refusalCDexFulfillmentPastLimits, nil)
		return
	}
	// Any other error is the facility's own build failing.
	if err != nil {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusInternalServerError, "build cdex result failed", nil)
		return
	}
	if status, msg := g.validateFHIR(ctx, fulfilled, "egress", ""); status != 0 {
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, status, msg, nil)
		return
	}
	g.respondLeg(w, r, "facility-disclosure", "federated-query-response", "federated-query", env.Metadata.CorrelationID, answer, builtAnswer, tok.Subject, env.Metadata.Sender, consentRef, answerTok)
}

// sealCDexFulfillment fulfills the requester's CDex request Task with the
// facility's records Bundle and seals the result as the registered
// cdex-fulfillment answer. Every part the fulfillment copies from the request
// or the records is declared as an embed, so sealing verifies it is
// byte-identical to its source. It returns the answer and its bytes (for the
// egress validation the gateway runs before sending).
//
// A request Task the fulfillment cannot extend (not one well-formed Task,
// past the gateway's own limits, naming in another case a member the
// fulfillment writes, or signed by the SDK's reading or the relay's) is the
// requester's and is returned as a *cdexRequestRefused. One in a status that
// does not allow fulfillment is the facility's own decision, returned as a
// *cdexDeclined. A fulfillment that, with the facility's records, goes past
// the gateway's own carrying limits (the SDK's ErrCDexLimit, or the relay's
// on the sealed answer) is returned as a *cdexBuildPastLimits. Any other error
// is a fault in the facility's own build.
func sealCDexFulfillment(requestTask, records []byte) (relay.Payload, []byte, error) {
	var none relay.Payload
	task := relay.NewBody(requestTask, relay.OriginPeerFrame)
	if refused := cdexRequestShape(task); refused != nil {
		return none, nil, refused
	}
	f, err := shnsdk.BuildCDexFulfillment(requestTask, records)
	switch {
	case errors.Is(err, shnsdk.ErrCDexSignedContent):
		return none, nil, &cdexRequestRefused{reason: refusalCDexSignedTask, rule: cdexRefusedAtBuild, err: err}
	case errors.Is(err, shnsdk.ErrCDexTaskStatus):
		// The facility's decline, after the network's refusals of the request
		// itself (above, and before this build: consent, the patient's binding,
		// cdexRequestShape). Signed content bears only on an edit, and a
		// decline makes none, so a signed Task that is closed is declined: the
		// SDK reads its status before any signature. The SDK alone reads the
		// rule (ErrCDexTaskStatus); the status is read here only to say which
		// it was.
		status, hasStatus := cdexTaskStatusOf(task)
		return none, nil, &cdexDeclined{status: status, hasStatus: hasStatus, err: err}
	case errors.Is(err, shnsdk.ErrCDexLimit):
		return none, nil, &cdexBuildPastLimits{err: err}
	case err != nil:
		return none, nil, err
	}
	held := relay.NewBody(records, relay.OriginUpstreamResponse)
	if carrier, err := cdexSignedByTheRelay(f, held); err != nil {
		return none, nil, pastLimitsOr(err)
	} else if carrier != "" {
		return none, nil, &cdexRequestRefused{reason: refusalCDexSignedTask, rule: cdexRefusedAtBuild, err: fmt.Errorf("%w: %s", relay.ErrSignedContent, carrier)}
	}
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
		return none, nil, pastLimitsOr(err)
	}
	return p, f.Task, nil
}

// cdexBuildPastLimits is a fulfillment the gateway cannot build within its own
// carrying limits from the facility's records: what it read from its own
// participant's system, refused 502 on the fidelity of its own build.
type cdexBuildPastLimits struct{ err error }

func (e *cdexBuildPastLimits) Error() string { return e.err.Error() }
func (e *cdexBuildPastLimits) Unwrap() error { return e.err }

// pastLimitsOr returns err as a *cdexBuildPastLimits when it is the relay's
// carrying limits (relay.ErrTooLarge), else err.
func pastLimitsOr(err error) error {
	if errors.Is(err, relay.ErrTooLarge) {
		return &cdexBuildPastLimits{err: err}
	}
	return err
}

// cdexRequestRefused is a request Task the facility cannot fulfil because of
// the Task itself; reason names the refusal without echoing the Task, and rule
// the network rule it is recorded under.
type cdexRequestRefused struct {
	reason string
	rule   cdexRefusalRule
	err    error
}

// cdexRefusalRule is the network rule a request Task's refusal is recorded
// under; the handler records each through the exchange's own writers, so a
// fidelity refusal names its stage. The zero value names none.
type cdexRefusalRule uint8

const (
	// cdexRefusedIntegrity is message integrity: the request can be read
	// more than one way, or not at all.
	cdexRefusedIntegrity cdexRefusalRule = iota + 1
	// cdexRefusedAtBuild is the fidelity of the gateway's own edit, at the
	// build.
	cdexRefusedAtBuild
)

func (e *cdexRequestRefused) Error() string { return e.reason }

func (e *cdexRequestRefused) Unwrap() error { return e.err }

// signed reports whether the Task was refused because a signature covers it,
// by the SDK's reading or the relay's.
func (e *cdexRequestRefused) signed() bool {
	return errors.Is(e.err, shnsdk.ErrCDexSignedContent) || errors.Is(e.err, relay.ErrSignedContent)
}

// cdexDeclined is the facility's own decision not to fulfil a request Task
// whose status does not allow it: a facility following FHIR R4's Task state
// machine declines to complete a Task the requester has already cancelled,
// completed or otherwise closed. Its gateway builds the fulfillment as the
// facility, so it answers as the facility would answer directly, never as a
// refusal of the network's. status is the Task's status when it is an R4 Task
// status code (else ""); hasStatus is whether the Task holds a status string.
type cdexDeclined struct {
	status    string
	hasStatus bool
	err       error
}

func (e *cdexDeclined) Error() string { return e.diagnostics() }

func (e *cdexDeclined) Unwrap() error { return e.err }

// diagnostics is the facility's own sentence for the decline. It echoes the
// requester's own status, and only a code from R4's closed list of Task
// statuses, so it carries nothing of the facility's and is bounded.
func (e *cdexDeclined) diagnostics() string {
	switch {
	case e.status != "":
		return fmt.Sprintf("the Task's status (%s) does not allow it to be fulfilled", e.status)
	case e.hasStatus:
		return "the Task's status is not a Task status code, so it cannot be fulfilled"
	}
	return "the Task has no status, so it cannot be fulfilled"
}

// r4TaskStatuses is FHIR R4's TaskStatus value set.
var r4TaskStatuses = map[string]bool{
	"draft": true, "requested": true, "received": true, "accepted": true, "rejected": true, "ready": true,
	"cancelled": true, "in-progress": true, "on-hold": true, "failed": true, "completed": true, "entered-in-error": true,
}

// cdexTaskStatusOf reads the request Task's status member, named exactly as
// the SDK reads it: the value when it is an R4 Task status code, and whether
// the Task holds a status string at all.
func cdexTaskStatusOf(task relay.Body) (string, bool) {
	d, err := relay.Doc(task)
	if err != nil {
		return "", false
	}
	n, ok := d.Member(d.Root(), "status")
	if !ok || d.Kind(n) != relay.KindString {
		return "", false
	}
	v, err := d.StringValue(n)
	if err != nil {
		return "", false
	}
	if !r4TaskStatuses[v] {
		return "", true
	}
	return v, true
}

// answerCDexDecline answers a request Task the facility declines with the
// facility's own error answer, as a FHIR server answers a change its business
// rules do not allow: 422, an OperationOutcome whose one issue is a business
// rule, built as cdex-decline, the one builder admitted on the federated
// query's error answer. It is recorded as that answer (upstream-error, the
// answer's builder cdex-decline), under no network rule, and is the same at
// every conformance level: it is the facility's decision, not a check.
func (g *Gateway) answerCDexDecline(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, consentRef, answerTok string, declined *cdexDeclined) {
	msg := declined.diagnostics()
	body, err := json.Marshal(map[string]any{"resourceType": "OperationOutcome", "issue": []map[string]string{{"severity": "error", "code": "business-rule", "diagnostics": msg}}})
	var p relay.Payload
	if err == nil {
		p, err = relay.Authored(relay.BuilderCDexDecline, body, "application/fhir+json")
	}
	if err != nil {
		// A fault in the facility's own build, as the fulfillment's is.
		g.refuseInbound(w, r, legFederatedQuery, env, tok, answerTok, http.StatusInternalServerError, "build cdex result failed", nil)
		return
	}
	// The answer is checked, and so recorded, as the leg's error answer
	// (upstream-error), as a participant's own error answer is.
	g.respondLegError(w, r, legFederatedQuery.frame, legFederatedQuery.op, legFederatedQuery.tx, env.Metadata.CorrelationID,
		LegResult{Status: http.StatusUnprocessableEntity, Message: msg, Response: p}, tok.Subject, env.Metadata.Sender, consentRef, answerTok)
}

// refusalCDexSignedTask answers a request Task the fulfillment would extend
// though a signature covers it.
const refusalCDexSignedTask = "federated query refused: the request Task carries signed content, which a fulfillment cannot extend"

// The refusals of a request that is not a Task the fulfillment can extend.
const (
	refusalCDexNotOneTask   = "federated query refused: the request is not one well-formed Task"
	refusalCDexFoldedMember = "federated query refused: the request Task names a member the fulfillment writes (contained, output) in another case"
	refusalCDexPastLimits   = "federated query refused: the request Task is past the gateway's own limits for building a fulfillment"
)

// refusalCDexFulfillmentPastLimits answers a fulfillment past the gateway's own
// carrying limits. The fulfillment extends the request Task with the
// facility's records, so either can take it past them, and the reason blames
// neither.
const refusalCDexFulfillmentPastLimits = "federated query refused: the fulfillment is past the gateway's own carrying limits; it cannot be answered until the request or the facility's records change"

// refusalCDexRecordsPastLimits answers a records Bundle past the gateway's own
// carrying limits: it holds the facility's records, with the Patient and
// Provenance written for them, and nothing of the request.
const refusalCDexRecordsPastLimits = "federated query refused: the facility's records are past the gateway's own carrying limits; it cannot be answered until the facility's records change"

// cdexRequestShape refuses a request that is not the shape a fulfillment
// extends, or returns nil:
//
//   - a body that is not one JSON document, or repeats a member name (exactly,
//     or equal under case folding), can be read two ways, so it is refused on
//     message integrity;
//   - one past the relay's limits on a body the gateway transmits (nesting
//     depth, tokens or size) is past the gateway's own bound: it cannot read,
//     so cannot extend and send, the Task within them, which is the fidelity
//     of its own edit;
//   - a root that is not a Task (its resourceType named exactly), or whose
//     contained or output is not an array, is not what the fulfillment
//     extends, and one naming contained or output only in another case would
//     repeat that member once the fulfillment writes it: either is refused on
//     the fidelity of the gateway's own edit.
func cdexRequestShape(task relay.Body) *cdexRequestRefused {
	d, err := relay.Doc(task)
	if err != nil {
		var limit *relay.LimitError
		if errors.As(err, &limit) {
			return &cdexRequestRefused{reason: refusalCDexPastLimits, rule: cdexRefusedAtBuild, err: err}
		}
		return &cdexRequestRefused{reason: refusalCDexNotOneTask, rule: cdexRefusedIntegrity, err: err}
	}
	notATask := &cdexRequestRefused{reason: refusalCDexNotOneTask, rule: cdexRefusedAtBuild, err: errors.New("not a Task whose contained and output, when present, are arrays")}
	if d.Kind(d.Root()) != relay.KindObject {
		return notATask
	}
	rt, ok := d.Member(d.Root(), "resourceType")
	if !ok || d.Kind(rt) != relay.KindString {
		return notATask
	}
	if v, err := d.StringValue(rt); err != nil || v != "Task" {
		return notATask
	}
	for _, key := range []string{"contained", "output"} {
		if v, ok := d.Member(d.Root(), key); ok && d.Kind(v) != relay.KindArray {
			return notATask
		}
	}
	// A member equal under case folding to one the fulfillment writes, beside
	// that member named exactly, is a repeated name, refused above.
	for _, m := range d.Members(d.Root()) {
		for _, key := range []string{"contained", "output"} {
			if m.Name != key && strings.EqualFold(m.Name, key) {
				return &cdexRequestRefused{reason: refusalCDexFoldedMember, rule: cdexRefusedAtBuild, err: fmt.Errorf("the Task names %q as %q", key, m.Name)}
			}
		}
	}
	return nil
}

// cdexSignedByTheRelay names a signature that covers what the fulfillment f
// changes, read by the relay's one rule, which the SDK's reading need not
// match, or returns "". The gateway judges signed content by that rule alone,
// whatever the SDK's reading finds (the shn-sdk release a gateway pins may
// read less than the relay), so a fulfillment is refused when:
//
//   - a signature covers the request Task, read in the fulfilled Task, which
//     holds the request Task and the records it embeds: a signature in the
//     Task (such as one at a place FHIR R4 types Signature, though it lacks a
//     member R4 requires), or a signed Provenance in the Task or among the
//     records that targets it (its members read in any case);
//   - the fulfillment writes the records Bundle's id (it has none, or one the
//     Task already uses) and a signature covers that Bundle.
//
// The fulfilled Task differs from the request only by its status, the added
// output and the embedded records, none of which can take a signature off
// the Task. Embedding could add one only through the relay's wide reading of
// "#" (a signed Provenance with that target in a contained array inside the
// records would read as covering the Task); the records this gateway builds
// hold no contained array outside their records, so it refuses no more here.
func cdexSignedByTheRelay(f shnsdk.CDexFulfillment, held relay.Body) (string, error) {
	out, err := relay.Doc(relay.NewBody(f.Task, relay.OriginPeerFrame))
	if err != nil {
		return "", err
	}
	if carrier := out.SignedCarrier(out.Root()); carrier != "" {
		return carrier, nil
	}
	rd, err := relay.Doc(held)
	if err != nil {
		return "", err
	}
	if id, ok := rd.Member(rd.Root(), "id"); ok {
		if own, err := rd.StringValue(id); err == nil && own == f.ResultsID {
			return "", nil // the records are embedded as they are
		}
	}
	return rd.SignedCarrier(rd.Root()), nil
}
