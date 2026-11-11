//go:build !shn_coreonly

package engine

import (
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleFederatedQueryIngress terminates a consented federated query (UC-05)
// from the participant's own system at POST /Task: the Da Vinci CDex
// task-based data request a data consumer sends a data source
// (https://hl7.org/fhir/us/davinci-cdex/task-based-approach.html), a
// cdex-task-data-request Task with one data-query. It is carried to the
// facility its owner names, on the federated-query leg, exactly as it was
// sent, and the facility's answer (a facility's gateway answers with the
// request Task fulfilled with the records) is relayed exactly. The network's
// consent checks are the leg's
// own: the Authorization Framework's consent conjunct for the patient, the
// facility's consent backstop, and the facility's binding of the queried
// patient to the consented one. Nothing is added to the request or the
// answer.
func (g *Gateway) handleFederatedQueryIngress(w http.ResponseWriter, r *http.Request) {
	const legType = "federated-query"
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
	task, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	// The patient the query is for, read as the facility reads it
	// (cdexTaskQuery), so consent is checked for the patient it discloses
	// for. A Task that reads as no data request for one patient's named
	// documents is refused here as the facility would refuse it: the network
	// cannot check consent for a patient it cannot identify (D121), nor carry
	// a query its scope does not admit.
	// Every member is read by its exact name: a Task a FHIR reader would read
	// another way (a member repeated, or named only in another case) is not
	// carried, since which patient it is for, or where it goes, could be read
	// two ways. The view is the one the facility's reader takes, and the owner
	// the Task is routed by.
	// A value of the wrong type is not that (valueTypeError): it reads one
	// way, and is refused as what it fails to be. In the owner it names no
	// holder, which routing refuses (below: the view keeps every value that
	// fit); in any other field it is one the facility's reader takes, which
	// refuses it as the leg's scope (cdexTaskQuery).
	var view federatedQueryView
	if err := decodeMessage(task, &view); err != nil && !valueTypeError(err) {
		exchangeOf(r.Context()).refusingFor(RefusalIntegrity, reasonFederatedQueryTwoWays)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errFederatedQueryTwoWays})
		return
	}
	_, member, err := cdexTaskQuery(task)
	if err != nil {
		// The leg's scope (its taxonomy row's named-docs-only): a data request
		// for one patient's named documents, the patient read from Task.for.
		exchangeOf(r.Context()).refusingFor(RefusalAuthority, reasonFederatedQueryNoPatient)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "federated query rejected: " + err.Error()})
		return
	}
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), member, task)
	if readErr != nil {
		status, msg := subjectBindingFailure(r.Context(), readErr)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		exchangeOf(r.Context()).refusing(RefusalConformance)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	facility, status, msg, reason := g.federatedQueryRecipient(view.Owner)
	if status != 0 {
		exchangeOf(r.Context()).routedFor(status, reason)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	child := g.ingressCorrelation(w, r)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	// The participant's Task is carried exactly as it arrived. The facility
	// it is sent to is the custodian whose consent the Authorization
	// Framework checks.
	// A module client's request is the gateway's own, sent as its module built it.
	request, ok := sealModuleRequest(w, builder, relay.Exact(relay.NewBody(task, relay.OriginIngressRequest), mediaFHIRJSON), task)
	if !ok {
		return
	}
	ex := g.exchanges.Begin(workstreamFQS)
	content := Content{WorkstreamType: workstreamFQS, Payload: request, Carried: true, OnBehalf: builder != ""}
	var answerType string
	var stated statedAnswer
	answer, err := g.OriginateLeg(withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated), r, facility, legType, pci, child, facility, content)
	spec, _ := legSpecFor(legType)
	leg := Leg{Type: legType, Physics: spec.Physics, Content: content, Subjects: subjectsOf(pci)}
	if err != nil {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		// The facility answered non-2xx (a consent refusal among them): its
		// framed answer is relayed verbatim.
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// The answer is the facility's content. Consent is the network's, and is
	// enforced where the disclosure is made: the facility's gateway checks it
	// (its backstop), binds the queried patient to the consented one, and,
	// where it builds the answer from its system of record, builds it from
	// that patient's records alone, at every level. Here it is checked as the
	// answer to an eligibility request is:
	//   - an answer repeating a member name refuses at every level: it can be
	//     read two ways;
	//   - an answer that reads as about a patient other than the queried one
	//     (RulePatientAnswer) refuses at strict, and is relayed and recorded
	//     below it. That check is best-effort: it reads the answer Task's own
	//     patient and the patient every resource it contains is about, and
	//     judges no free text or encoded data;
	//   - an answer that is not a Task fulfilled with a records Bundle (a
	//     data-query output naming a Bundle it contains; it is not compared
	//     with the request; RuleAnswerShape) refuses at structural and strict,
	//     and is relayed and recorded below;
	//   - the answer is validated at this gateway's conformance level. A
	//     validator this gateway cannot reach, or has no lane for, is its own
	//     failure (500), never the facility's.
	// What is relayed is the facility's answer exactly.
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	refuse := func(status int, msg string) {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, status, map[string]string{"error": msg})
	}
	if repeatsAMember(answerCtx, answer) {
		dup, _ := repeatedMember(answer)
		refuse(http.StatusBadGateway, "facility federated query answer repeats "+dup+": it can be read two ways, so it is not carried")
		return
	}
	if reason := federatedAnswerOtherPatient(answer, member); reason != "" && g.guard(answerCtx, KindContent, RulePatientAnswer, answer) {
		// The reason names the class of failure, never a patient.
		refuse(http.StatusBadGateway, "facility federated query answer is about a patient other than the queried one: "+reason)
		return
	}
	if _, err := cdexRecordsBundle(answer); err != nil && g.guard(answerCtx, KindContent, RuleAnswerShape, answer) {
		refuse(http.StatusBadGateway, "facility federated query answer is not a Task fulfilled with a records Bundle")
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
	// The answering system's own media type, whatever it is.
	ct := writableAnswerMediaType(answerType)
	g.writeRelayedAnswer(w, stated, ct, relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct),
		relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// The routing refusals of a federated query, each naming what is missing.
const (
	federatedQueryNoOwner = "no facility to route the federated query to: the Task's owner names no holder identifier (system " + holderIdentifierSystem + ")"
)

// holderIdentifierSystem is the identifier system a network holder is named
// by in a FHIR reference (Task.owner, Task.requester).
const holderIdentifierSystem = "http://smarthealth.network/ids/holder"

// federatedQueryView is a federated-query Task as the provider ingress reads
// it, by exact member names: the fields the facility's reader takes
// (cdexTaskView), and the owner it is routed by.
type federatedQueryView struct {
	cdexTaskView
	Owner *federatedQueryOwner `json:"owner"`
}

// federatedQueryOwner is a Task's owner Reference, by its exact member names.
type federatedQueryOwner struct {
	Identifier *struct {
		System string `json:"system"`
		Value  string `json:"value"`
	} `json:"identifier"`
}

// federatedQueryRecipient is the facility holder a federated-query Task is
// routed to: the holder its owner names by identifier. No default: a Task
// whose owner names no holder, a holder the network does not register, or a
// holder that is not a facility is refused (422), with what is missing.
func (g *Gateway) federatedQueryRecipient(owner *federatedQueryOwner) (string, int, string, refusalReason) {
	if owner == nil || owner.Identifier == nil || owner.Identifier.System != holderIdentifierSystem || owner.Identifier.Value == "" {
		return "", http.StatusUnprocessableEntity, federatedQueryNoOwner, reasonFederatedQueryNoOwner
	}
	id := owner.Identifier.Value
	holder, ok := g.cfg.Reg.Lookup(id)
	switch {
	case !ok:
		return "", http.StatusUnprocessableEntity, "no facility to route the federated query to: the Task's owner " + quotedBounded(id) + " is not a holder the network registers", reasonFederatedQueryNoFacility
	case holder.Role != "facility":
		return "", http.StatusUnprocessableEntity, "no facility to route the federated query to: the Task's owner " + quotedBounded(id) + " is a " + holder.Role + ", not a facility", reasonFederatedQueryNoFacility
	}
	return id, 0, "", ""
}

// errFederatedQueryTwoWays is the refusal of a federated-query Task that
// cannot be read one way only.
const errFederatedQueryTwoWays = "federated query rejected: the Task cannot be read one way only (it is not one JSON document, or it repeats a member or names one only in another case)"

// federatedRecordsRefused is a requester's check of the records a facility's
// federated answer discloses, apart from any it builds into a message of its
// own: a record about a patient other than member is the content check
// patient.answer (the requester's records fence), refused at strict and
// recorded and carried below it, as the answer to a participant's own
// federated query is (federatedAnswerOtherPatient). answer is the facility's
// answer, the finding's payload; records is its records Bundle. It returns the
// refusal's reason, which names the class of failure and never a patient, or
// "" when the records are carried.
func (g *Gateway) federatedRecordsRefused(ctx context.Context, member string, answer, records []byte) string {
	if err := requesterRecordsFence(member).check(records); err != nil && g.guard(ctx, KindContent, RulePatientAnswer, answer) {
		return err.Error()
	}
	return ""
}

// federatedAnswerOtherPatient is why a facility's federated-query answer reads
// as about a patient other than member, or "" when it does not: the answer
// Task's own patient (a Task.for it has must be a well-formed reference), or a
// resource it contains, in any Bundle, about another patient (the
// requester's records fence). It is a best-effort content check: free text
// and encoded data are not read. The reason names the class of failure,
// never a patient.
func federatedAnswerOtherPatient(answer []byte, member string) string {
	var task struct {
		For *struct {
			Reference string `json:"reference"`
		} `json:"for"`
		Contained []any `json:"contained"`
	}
	// An answer naming no patient at all (no Task.for) is the shape rule's.
	if decodeMessage(answer, &task) == nil && task.For != nil {
		switch who, ok := patientRefMember(task.For.Reference, containedObjects(task.Contained)); {
		case !ok:
			return "the answer Task names its patient otherwise than by a reference this gateway reads"
		case who != member:
			return "the answer Task is for another patient"
		}
	}
	if err := requesterRecordsFence(member).check(answer); err != nil {
		return err.Error()
	}
	return ""
}
