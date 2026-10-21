package engine

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleEligibilityIngress terminates a coverage eligibility request from the
// participant's own system at POST /CoverageEligibilityRequest/$submit, the
// base R4 operation (IHE FAIS ITI-124), and carries it to the payer on the
// coverage-eligibility leg exactly as it was submitted: the request itself, or
// the Parameters holding it, which is the operation's invocation. The request
// is bound to one patient, routed by the payer it names (no default), and the
// payer's answer is relayed exactly. Nothing is added to the request or the
// answer.
func (g *Gateway) handleEligibilityIngress(w http.ResponseWriter, r *http.Request) {
	const legType = "coverage-eligibility"
	w, scope := g.withScope(w, relay.RoleRequester)
	scope.leg = legType
	w = &fhirOperationWriter{w}
	if g.ingressAuthRefused(w, r) {
		return
	}
	exchangeOf(r.Context()).leg(legType)
	builder, ok := g.ingressBuilder(w, r, legType)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: legType, Seam: "provider-ingress", Whose: "own"}))
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	// Read as the payer's gateway reads it (handleEligibilityInbound): a
	// request that repeats a member name is refused as message integrity, and
	// one naming its type or patient only in another case names no patient
	// (the patient-match rule, eligibilityNamedInAnotherCase), each with the
	// payer's refusal text.
	if repeatsAName(body) {
		dup, ok := repeatedMember(body)
		if !ok {
			dup = "a member name"
		}
		exchangeOf(r.Context()).refusing(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "eligibility request repeats " + dup + ": it can be read two ways, so it is not read"})
		return
	}
	subs, err := eligibilitySubmissions(body)
	if msg, ok := eligibilityNamedInAnotherCase(err); ok {
		exchangeOf(r.Context()).refusing(RefusalAuthority)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse eligibility request failed: " + err.Error()})
		return
	}
	if len(subs) > maxEligibilityBatch {
		exchangeOf(r.Context()).refusing(RefusalLimit)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": eligibilityBatchTooLarge(len(subs))})
		return
	}
	// The member each request's patient names, in any reference form, bound to
	// a patient as the other native legs bind one: this participant's own
	// system first, else the member id and the Patient the request carries
	// (with the resources carried with it). The first request's patient, in
	// document order, is the leg's patient; every other request's is named on
	// the leg's involved list when the network reads it (involved.go), so the
	// exchange is recorded under each.
	var pcis, members []string
	for i, sub := range subs {
		member, err := eligibilitySubmissionMember(sub)
		if msg, ok := eligibilityNamedInAnotherCase(err); ok {
			exchangeOf(r.Context()).refusing(RefusalAuthority)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg + eligibilityBatchAt(subs, i)})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse member failed" + eligibilityBatchAt(subs, i)})
			return
		}
		pci, found, readErr := g.resolveSubjectPCI(r.Context(), member, sub.scope)
		if readErr != nil {
			status, msg := subjectBindingFailure(r.Context(), readErr)
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		if !found {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member" + eligibilityBatchAt(subs, i)})
			return
		}
		members = append(members, member)
		if !slices.Contains(pcis, pci) {
			pcis = append(pcis, pci)
		}
	}
	pci := pcis[0]
	// Each request is routed by the payer it names, a reference it does not
	// carry the resource for read from this participant's system of record
	// for its own patient, only to route (eligibilitySoRRead).
	recipient, status, msg := g.eligibilityRecipient(r.Context(), subs, members)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	child := g.ingressCorrelation(w, r)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	// The participant's request is carried exactly as it arrived.
	// A module client's request is the gateway's own, sent as its module built it.
	request, ok := sealModuleRequest(w, builder, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), mediaFHIRJSON), body)
	if !ok {
		return
	}
	ex := g.exchanges.Begin(workstreamPA)
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true, OnBehalf: builder != ""}
	var answerType string
	var answerStatus int
	answer, err := g.OriginateLeg(withAnswerStatusSink(withAnswerMediaTypeSink(r.Context(), &answerType), &answerStatus), r, recipient, legType, pci, child, "", content)
	spec, _ := legSpecFor(legType)
	leg := Leg{Type: legType, Physics: spec.Physics, Content: content, Subjects: pcis}
	if err != nil {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		// The payer answered non-2xx: its framed answer is relayed verbatim.
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// The answer is the payer's content, checked as a payer's gateway checks
	// its own system's eligibility answer, and validated as this gateway
	// validates the answer to an eligibility request it builds itself:
	//   - a repeated member name refuses at every level (it can be read two
	//     ways);
	//   - an answer that is not a CoverageEligibilityResponse naming its
	//     patient, or a Bundle of them (RuleAnswerShape), refuses at structural
	//     and strict, and is relayed and recorded below;
	//   - the answer is validated at this gateway's conformance level
	//     (recorded at observe; refused at structural for a structural issue,
	//     and at strict). A validator this gateway cannot reach, or has no lane
	//     for, is its own failure (500), never the payer's.
	// Which patient the answer names is checked where it can be judged, by
	// the payer's gateway, which knows the payer's own Patient ids. What is
	// relayed is the payer's answer exactly.
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	refuse := func(status int, msg string) {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, status, map[string]string{"error": msg})
	}
	if repeatsAMember(answerCtx, answer) {
		dup, _ := repeatedMember(answer)
		refuse(http.StatusBadGateway, "payer eligibility answer repeats "+dup+": it can be read two ways, so it is not carried")
		return
	}
	if !eligibilityAnswerReadable(answer) && g.guard(answerCtx, KindContent, RuleAnswerShape, answer) {
		refuse(http.StatusBadGateway, "payer eligibility answer is not a CoverageEligibilityResponse naming its patient, or a Bundle of them")
		return
	}
	if gr := g.validateGoverned(answerCtx, findingContextFrom(answerCtx), g.validatorForLine(""), answer, "ingress", "", "", false); gr.Status != 0 {
		switch {
		case gr.NoLane:
			refuse(http.StatusInternalServerError, "no FHIR validator lane configured for this leg")
		case gr.Status == http.StatusInternalServerError:
			refuse(http.StatusInternalServerError, "validator unavailable")
		default:
			refuse(http.StatusBadGateway, gr.Msg)
		}
		return
	}
	g.recordLeg(ex.ID, leg.Project(child, "answered"))
	ct := carriedFHIRMediaType(answerType)
	if ct == "" {
		ct = mediaFHIRJSON
	}
	g.writePayload(w, relayedAnswerStatus(answerStatus), ct, relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct),
		relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// eligibilityBatchAt names request i of a batch in a refusal's text: nothing
// for a submission of one request.
func eligibilityBatchAt(subs []eligibilitySubmission, i int) string {
	if len(subs) < 2 {
		return ""
	}
	return " (request " + strconv.Itoa(i) + " of the batch)"
}

// eligibilityAnswerReadable reports whether an answer is a
// CoverageEligibilityResponse naming its patient, or a Bundle of them
// (eligibilityAnswerResponses), as a payer's gateway reads its own system's
// answer. A response naming its patient by identifier only names it: which
// patient that is is the payer's gateway's check.
func eligibilityAnswerReadable(answer []byte) bool {
	responses, ok := eligibilityAnswerResponses(answer)
	if !ok {
		return false
	}
	for _, resp := range responses {
		if _, err := ParseCoverageEligibilityResponsePatient(resp.response); err != nil && !errors.Is(err, errNoEligibilityPatientRef) {
			return false
		}
	}
	return true
}
