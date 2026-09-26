// payer.go — the payer-side DTR questionnaire responder and the shared (C)
// outbound subject fence. Part of package gateway (the Smart Gateway runs every
// holder role; this file is the payer-adjudication surface). The conformant PAS
// inbound handlers live in pas_native.go; the minimized CRD/PAS handlers are no
// longer part of the contract. See gateway.go for the package doc.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// handleDTRInbound answers a DTR questionnaire request. A request frame that
// names an operation carries that operation's own input: the
// $questionnaire-package Parameters, or the SDC $next-question input. A
// request that names none is refused. The patient the request names is bound
// by this payer's own system (bindInboundSubject) before the responder sees
// it, and the responder is handed that binding; the answer is fenced and
// relayed.
func (g *Gateway) handleDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	ctx := r.Context()

	// An SDC adaptive $next-question round carries a QuestionnaireResponse ABOUT A PATIENT
	// (its answers so far), and a package request carries the patient's Coverage and
	// orders — so both get the (A) bind every patient-bearing leg gets (the conformant PAS
	// bind's rule): the request must name one patient, bound by this payer's own system,
	// independently of what the responder does with it. Fails closed BEFORE the responder
	// (and before any partner forward) sees the bytes.
	var (
		nextQuestionSubject string
		isNextQuestion      bool
		subjectPCI          string
	)
	switch RequestFrameOperation(ctx) {
	case shnsdk.FrameOperationQuestionnairePackage:
		pci, status, msg := g.bindPackageParameters(ctx, reqJSON)
		if status != 0 {
			g.refuseInbound(w, r, legDTR, env, tok, answerTok, status, msg, nil)
			return
		}
		subjectPCI = pci
	case shnsdk.FrameOperationNextQuestion:
		subject, ok := framedNextQuestionSubject(reqJSON)
		if !ok {
			g.refuseInbound(w, r, legDTR, env, tok, answerTok, http.StatusBadRequest, "parse next-question input failed", nil)
			return
		}
		nextQuestionSubject, isNextQuestion = subject, true
	case "":
		// The older request envelope (a canonical and a coverage in place of the
		// operation's own input) is no longer read: a request that names no
		// operation is refused, and the refusal names what to send.
		g.refuseInbound(w, r, legDTR, env, tok, answerTok, http.StatusBadRequest, refusalDTRUnframed, nil)
		return
	default:
		g.refuseInbound(w, r, legDTR, env, tok, answerTok, http.StatusBadRequest, "unsupported DTR operation", nil)
		return
	}
	if isNextQuestion {
		pci, status, msg := g.bindNextQuestionSubjectContext(ctx, nextQuestionSubject, reqJSON)
		if status != 0 {
			g.refuseInbound(w, r, legDTR, env, tok, answerTok, status, msg, nil)
			return
		}
		subjectPCI = pci
	}

	g.noteSubjectBinding(r.Context(), "dtr-questionnaire-fetch", env.Metadata.CorrelationID, tok.Subject, subjectPCI)
	result, err := g.cfg.Responder.Handle(ctx, "dtr-questionnaire-fetch", env.Metadata.CorrelationID, subjectPCI, reqJSON)
	if err != nil {
		// build/marshal fault (gateway's own) → 500
		g.responderFailed(w, r, legDTR, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		// e.g. 400 "unknown questionnaire canonical"
		g.respondLegError(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch",
			env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	responseFHIR, err := g.admit(result.Response, answerKey("dtr-questionnaire-fetch", relay.OutcomeAnswered))
	if err != nil {
		g.refuseInbound(w, r, legDTR, env, tok, answerTok, http.StatusInternalServerError, errOwnershipFault, nil)
		return
	}
	// The direction flips here: the $questionnaire-package answer validated below is
	// THIS participant's own build, not the peer's request handleInbound tagged the
	// context with.
	fc := findingContextFrom(ctx)
	fc.Whose = "own"
	ctx = withFindingContext(ctx, fc)
	// A relayed answer is the participant's own content: its defects below
	// (an answer that is not a questionnaire-response, one about another
	// patient, a Questionnaire carrying a subject) are not checked at none,
	// recorded at observe and relayed as the payer sent them, and refused at
	// strict. An answer this gateway built is fenced at every level.
	refuses := func(rule string) bool {
		return !result.ResponseRelayed() || g.guard(ctx, KindContent, rule, responseFHIR)
	}
	if isNextQuestion {
		// (C) for the adaptive round: the answered QuestionnaireResponse must be about the
		// SAME patient the request carried — a partner (or a relay) must not swap the subject.
		if status, msg := fenceNextQuestionSubjectWith(nextQuestionSubject, result, refuses); status != 0 {
			g.refuseInbound(w, r, legDTR, env, tok, answerTok, status, msg, nil)
			return
		}
	} else if status, msg := g.fenceResponseSubjectWith("dtr-questionnaire-fetch", "", env.Metadata.CorrelationID, result, refuses); status != 0 {
		g.refuseInbound(w, r, legDTR, env, tok, answerTok, status, msg, nil)
		return
	}
	// Egress $validate is a NEAR-RELAY for a verbatim foreign package (FR-G28, R-8
	// precedent): a real br-payer's $questionnaire-package response carries FOREIGN Da Vinci
	// DTR profiles (dtr-std-questionnaire / dtr-questionnaireresponse) that SHN — hosting US
	// Core only — cannot resolve, so a foreign-$validate would 422 a conformant payer
	// response. The trust-critical subject fence above still runs (no Questionnaire may carry
	// a subject); we skip ONLY the profile-resolution $validate the conformant crd/pas native
	// legs also skip. An SHN-PRODUCED package (a non-relaying LegResponder) is egress-$validated.
	//
	// The predicate is result.ResponseRelayed() — the per-RESULT truth the native DTR case
	// sets — rather than the deployment-wide PayerDavinciNative flag it replaces. The two
	// coincide today (a native-forward payer always routes dtr-questionnaire-fetch to native,
	// and app.go sets PayerDavinciNative exactly when that forward is wired), so this is a
	// precision change, not a behavior change: it just stops a hypothetical SHN-produced
	// DTR leg on a native deployment from skipping validation it should get.
	//
	// F7: an SHN-produced package would be built at the ANSWER LINE, so it must be validated
	// on THAT line's lane — DTR 2.2 requires a QuestionnaireResponse entry that the 2.0 IG
	// does not, and checking 2.2 bytes against a 2.0 lane is exactly the wrong-lane failure
	// the seam exists to prevent. Today a payer's DTR answer is always its partner's bytes
	// (a relayed Response), so this branch is the guard for a partner-injected occupant that
	// produces its own package rather than relaying one.
	if !result.ResponseRelayed() {
		if status, msg := g.validateFHIRForContract(ctx, responseFHIR, "egress", "pa.dtr", shnsdk.LineOf(answerTok), ""); status != 0 {
			g.refuseInbound(w, r, legDTR, env, tok, answerTok, status, msg, nil)
			return
		}
	}
	// Stamp honesty: a verbatim foreign package is relayed UNSTAMPED (the same rule as the PAS
	// relay); an SHN-produced package is stamped at the line it was built at.
	g.respondLeg(w, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", env.Metadata.CorrelationID, result.Response, tok.Subject, env.Metadata.Sender, "", answerTok)
}

// packagePatientsRefused is a questionnaire request whose patients cannot be
// read or bound: the status and message it is answered with.
type packagePatientsRefused struct {
	status int
	msg    string
}

var (
	errNotPatientRef = packagePatientsRefused{http.StatusBadRequest,
		"questionnaire-package request names a coverage beneficiary or order subject that is not a Patient reference"}
	errNotCoverage     = packagePatientsRefused{http.StatusBadRequest, "questionnaire-package coverage parameter is not a Coverage"}
	errParseParameters = packagePatientsRefused{http.StatusBadRequest, "parse questionnaire-package parameters failed"}
	errPatientNoID     = packagePatientsRefused{http.StatusBadRequest,
		"questionnaire-package request carries a Patient resource with no id, which cannot be bound to the authorized patient"}
)

// patientMember returns the member id a Patient reference names, relative
// ("Patient/<id>") or absolute (".../Patient/<id>"), with any _history
// suffix removed; ok is false when ref names no Patient.
func patientMember(ref string) (string, bool) {
	i := strings.LastIndex(ref, "Patient/")
	if i < 0 || (i > 0 && ref[i-1] != '/') {
		return "", false
	}
	id := ref[i+len("Patient/"):]
	if j := strings.IndexByte(id, '/'); j >= 0 {
		id = id[:j]
	}
	return id, id != ""
}

// packagePatients collects the patients a questionnaire request names.
//
// A content defect the walk meets (packageDefect) does not stop it: the
// resource or reference that has it names no member, and the defect is kept,
// in document order, for bindPackageParameters to judge once it knows whether
// the request has a subject at all. A defect that cannot be skipped (a body
// that cannot be read, a coverage parameter that cannot be read, or one that is
// not a Coverage on a payer that maps its identity) stops the walk.
type packagePatients struct {
	d       *relay.Document
	members map[string]bool
	// first is the first member named, in document order; coverage is the
	// first member a top-level coverage parameter's beneficiary names. The
	// request's subject is coverage, else first (the provider's DTR ingress
	// binds the same way).
	first, coverage string
	defects         []packageDefect
	// mapsPayerIdentity: this payer maps its identity by the Coverage's
	// payor, so a coverage parameter that is not a Coverage cannot be
	// addressed (Gateway.mapsPayerIdentity).
	mapsPayerIdentity bool
}

// packageDefect is a content defect of a questionnaire request: the rule it
// breaks and the refusal strict answers it with.
type packageDefect struct {
	rule    string
	refused packagePatientsRefused
}

// defect keeps a content defect; the walk goes on.
func (p *packagePatients) defect(rule string, refused packagePatientsRefused) {
	p.defects = append(p.defects, packageDefect{rule, refused})
}

// add records a member the request names.
func (p *packagePatients) add(member string) {
	if p.first == "" {
		p.first = member
	}
	p.members[member] = true
}

// patientBearing are the members through which a resource names the patient
// it is about. Coverage.subscriber is not one: the subscriber may be another
// person.
var patientBearing = []string{"beneficiary", "subject", "patient"}

// resource records every patient resource n names: a Patient resource's own
// id, and each patient-bearing reference, which must name a Patient. A
// Bundle's entries and a Parameters' resources are read the same way.
func (p *packagePatients) resource(n relay.NodeID) *packagePatientsRefused {
	d := p.d
	if d.Kind(n) != relay.KindObject {
		return &errParseParameters
	}
	switch docText(d, n, "resourceType") {
	case "Patient":
		// A Patient with no id (a name and birth date only) could be anyone,
		// so it is refused rather than left unbound (RuleRequestShape; below
		// strict it names no member).
		id := docText(d, n, "id")
		if id == "" {
			p.defect(RuleRequestShape, errPatientNoID)
		} else {
			p.add(id)
		}
	case "Bundle":
		if entries, ok := d.Member(n, "entry"); ok {
			for _, e := range d.Elems(entries) {
				if r, ok := d.Member(e, "resource"); ok {
					if refused := p.resource(r); refused != nil {
						return refused
					}
				}
			}
		}
	case "Parameters":
		if _, refused := p.parameters(n); refused != nil {
			return refused
		}
	}
	for _, key := range patientBearing {
		el, ok := d.Member(n, key)
		if !ok {
			continue
		}
		ref := docText(d, el, "reference")
		member, ok := patientMember(ref)
		if !ok {
			// A patient-bearing reference that names no Patient is the
			// request's own shape (RuleRequestShape); below strict it names
			// no member.
			p.defect(RuleRequestShape, errNotPatientRef)
			continue
		}
		p.add(member)
	}
	return nil
}

// parameters records the patients of every resource in a Parameters and
// counts its coverage parameters.
func (p *packagePatients) parameters(n relay.NodeID) (coverages int, refused *packagePatientsRefused) {
	params, ok := p.d.Member(n, "parameter")
	if !ok {
		return 0, &errParseParameters
	}
	return p.parameterList(params, true)
}

// parameterList reads one parameter (or part) array. Coverage parameters are
// counted at the top level only.
func (p *packagePatients) parameterList(arr relay.NodeID, top bool) (coverages int, refused *packagePatientsRefused) {
	d := p.d
	if d.Kind(arr) != relay.KindArray {
		return 0, &errParseParameters
	}
	for _, param := range d.Elems(arr) {
		if d.Kind(param) != relay.KindObject {
			return 0, &errParseParameters
		}
		if res, ok := d.Member(param, "resource"); ok {
			isCoverage := top && docText(d, param, "name") == "coverage"
			if isCoverage {
				// A coverage parameter that cannot be read refuses at every
				// level. One that is not a Coverage refuses at every level on
				// a payer that maps its identity, which addresses its own
				// system by the Coverage's payor; without the mapping the
				// request is already routed here, so it is the request's own
				// shape (RuleRequestShape) and names no coverage. A Coverage
				// with no beneficiary is the request's own shape too.
				switch {
				case d.Kind(res) != relay.KindObject:
					return 0, &errNotCoverage
				case docText(d, res, "resourceType") != "Coverage":
					if p.mapsPayerIdentity {
						return 0, &errNotCoverage
					}
					p.defect(RuleRequestShape, errNotCoverage)
					isCoverage = false
				default:
					if _, ok := d.Member(res, "beneficiary"); !ok {
						p.defect(RuleRequestShape, errNotPatientRef)
					}
					coverages++
				}
			}
			if refused := p.resource(res); refused != nil {
				return 0, refused
			}
			if isCoverage && p.coverage == "" {
				if b, ok := d.Member(res, "beneficiary"); ok {
					if member, ok := patientMember(docText(d, b, "reference")); ok {
						p.coverage = member
					}
				}
			}
		}
		if parts, ok := d.Member(param, "part"); ok {
			if _, refused := p.parameterList(parts, false); refused != nil {
				return 0, refused
			}
		}
	}
	return coverages, nil
}

// docText returns obj's member key when it is a string, else "".
func docText(d *relay.Document, obj relay.NodeID, key string) string {
	if d.Kind(obj) != relay.KindObject {
		return ""
	}
	v, ok := d.Member(obj, key)
	if !ok || d.Kind(v) != relay.KindString {
		return ""
	}
	s, err := d.StringValue(v)
	if err != nil {
		return ""
	}
	return s
}

// bindPackageParameters is the (A) inbound bind for a framed
// $questionnaire-package: the input must carry at least one coverage (the
// input profile's cardinality), and every patient it names — each coverage
// beneficiary, each order's subject and patient, every Patient resource and
// every other resource's patient reference — must be one patient. It returns
// this payer's binding of that patient (bindInboundSubject).
//
// The request's subject is the patient its first coverage's beneficiary
// names, else the first patient it names, bound by this payer's own system
// (RuleSubjectPCI when the participant requires known members). That, a request
// that cannot be read and a coverage parameter that is not a Coverage refuse
// at every level. The rest is the request's own content, not checked at
// none, recorded at observe and refused at strict with strict's status and
// body: no coverage, a Coverage with no beneficiary, a patient-bearing
// reference naming no Patient and a Patient with no id (RuleRequestShape),
// and more than one patient (RulePatientMixed). Below strict the request is
// bound by its subject and forwarded as sent; a request left with no subject
// at all refuses at every level, with strict's refusal.
func (g *Gateway) bindPackageParameters(ctx context.Context, body []byte) (pci string, status int, msg string) {
	d, err := relay.Doc(relay.NewBody(body, relay.OriginPeerFrame))
	if err != nil || d.Kind(d.Root()) != relay.KindObject || docText(d, d.Root(), "resourceType") != "Parameters" {
		return "", http.StatusBadRequest, "parse questionnaire-package parameters failed"
	}
	p := &packagePatients{d: d, members: map[string]bool{}, mapsPayerIdentity: g.mapsPayerIdentity()}
	coverages, refused := p.parameters(d.Root())
	if refused == nil && coverages == 0 {
		p.defect(RuleRequestShape, packagePatientsRefused{http.StatusBadRequest, "questionnaire-package request has no coverage"})
	}
	severalPatients := packagePatientsRefused{http.StatusForbidden, "questionnaire-package request covers more than one patient"}
	subject := p.coverage
	if subject == "" {
		subject = p.first
	}
	// A request that cannot be read or bound refuses at every level, as
	// strict refuses it: with the first defect in document order, which is
	// what the walk would have stopped at. It records no content finding.
	switch {
	case refused != nil && len(p.defects) > 0:
		return "", p.defects[0].refused.status, p.defects[0].refused.msg
	case refused != nil:
		return "", refused.status, refused.msg
	case subject == "" && len(p.defects) > 0:
		return "", p.defects[0].refused.status, p.defects[0].refused.msg
	case subject == "":
		return "", severalPatients.status, severalPatients.msg
	}
	if len(p.members) > 1 {
		p.defect(RulePatientMixed, severalPatients)
	}
	for _, def := range p.defects {
		if g.guard(ctx, KindContent, def.rule, body) {
			return "", def.refused.status, def.refused.msg
		}
	}
	return g.bindNextQuestionSubjectContext(ctx, "Patient/"+subject, body)
}

// bindNextQuestionSubject is the (A) inbound bind for an adaptive $next-question round:
// the carried QuestionnaireResponse's subject ("Patient/<member>", the member namespace) is
// bound by this payer's own system (bindInboundSubject), and that binding is returned. Mirrors
// conformantPASBind: no patient subject, or an unknown member where known members are
// required → 400.
func (g *Gateway) bindNextQuestionSubjectContext(ctx context.Context, subject string, body []byte) (pci string, status int, msg string) {
	member, ok := patientMember(subject)
	if !ok {
		return "", http.StatusBadRequest, "next-question request carries no patient subject"
	}
	return g.bindInboundSubject(ctx, member, body)
}

// fenceNextQuestionSubject is the (C) outbound fence for an adaptive $next-question round:
// the answered questionnaire-response must name the patient the request carried. A
// non-2xx relay (result.Status set) carries no QuestionnaireResponse and passes through to
// respondLegError untouched; an unparseable 2xx is refused (502) rather than relayed blind.
func fenceNextQuestionSubject(requestSubject string, res LegResult) (int, string) {
	return fenceNextQuestionSubjectWith(requestSubject, res, nil)
}

// fenceNextQuestionSubjectWith is fenceNextQuestionSubject with the answer's
// content defects handed to refuses (nil: every defect refuses): an answer that
// is not a questionnaire-response (RuleAnswerShape) and one about another
// patient (RulePatientAnswer). A repeated member name refuses whatever refuses
// answers. An answer whose defect does not refuse passes, to be relayed as the
// payer sent it; one that cannot be read has no subject to compare.
func fenceNextQuestionSubjectWith(requestSubject string, res LegResult, refuses func(rule string) bool) (int, string) {
	if refuses == nil {
		refuses = func(string) bool { return true }
	}
	if res.Status != 0 {
		return 0, ""
	}
	// No gateway here: a refusal is logged only. The caller, handleDTRInbound,
	// has already checked the same payload against the same row and observed
	// any refusal there.
	responseFHIR, err := (*Gateway)(nil).admit(res.Response, answerKey("dtr-questionnaire-fetch", relay.OutcomeAnswered))
	if err != nil {
		return http.StatusInternalServerError, errOwnershipFault
	}
	qr, _, err := parseNextQuestionResponse(responseFHIR)
	if err != nil {
		if errors.Is(scanMessage(responseFHIR), relay.ErrDuplicateKey) || refuses(RuleAnswerShape) {
			return http.StatusBadGateway, "next-question response is not a questionnaire-response"
		}
		return 0, ""
	}
	subj, serr := questionnaireResponseSubject(qr)
	if (serr != nil || subj != requestSubject) && refuses(RulePatientAnswer) {
		return http.StatusForbidden, "response patient does not match request patient"
	}
	return 0, ""
}

// fenceResponseSubject is the (C) outbound fence: a connector must not swap the
// patient between the request it was handed and the response it returned. It
// compares the response resource's patient ref to boundPatientRef (the inbound
// member-namespace ref, e.g. "Patient/<member>", already bound by (A) through this
// payer's own system). NOT compared to tok.Subject, which is a derived PCI.
// The response is read after the same ownership check its transmit applies.
// Returns (0,"") on pass or (status, msg) to write. Per-leg arms are added as
// each leg moves behind the seam.
func (g *Gateway) fenceResponseSubject(leg, boundPatientRef, corrID string, res LegResult) (int, string) {
	return g.fenceResponseSubjectWith(leg, boundPatientRef, corrID, res, nil)
}

// fenceResponseSubjectWith is fenceResponseSubject with the answer's content
// defects handed to refuses (nil: every defect refuses): a Questionnaire in a
// package that carries a subject (RuleAnswerShape), and a PAS or inquiry
// answer whose subjects do not bind to one patient (RulePatientAnswer). A
// defect that refuses answers false for passes, and the answer is relayed. A
// repeated member name, the bound-member comparison of an answer this gateway
// built and the fence over this gateway's own side-effects refuse whatever
// refuses answers.
func (g *Gateway) fenceResponseSubjectWith(leg, boundPatientRef, corrID string, res LegResult, refuses func(rule string) bool) (int, string) {
	if refuses == nil {
		refuses = func(string) bool { return true }
	}
	responseFHIR, err := g.admit(res.Response, answerKey(leg, relay.OutcomeAnswered))
	if err != nil {
		return http.StatusInternalServerError, errOwnershipFault
	}
	// A response that repeats a member name can be read two ways; the fence
	// would judge one of them and the requester might read the other.
	if err := scanMessage(responseFHIR); errors.Is(err, relay.ErrDuplicateKey) {
		return http.StatusForbidden, "response repeats a member name"
	}
	switch leg {
	case "coverage-eligibility":
		// The answer this gateway built from the payer's records. A payer's own
		// answer, relayed, is fenced by fenceEligibilityAnswer instead.
		ref, err := ParseCoverageEligibilityResponsePatient(responseFHIR)
		if err != nil {
			return http.StatusInternalServerError, "parse response subject failed"
		}
		if ref != boundPatientRef {
			return http.StatusForbidden, "response patient does not match request patient"
		}
	case "dtr-questionnaire-fetch":
		// The response is a $questionnaire-package Bundle; walk its Questionnaire
		// entries (the wrapper has no top-level subject) and reject if any carries one.
		if packageQuestionnaireHasSubject(responseFHIR) && refuses(RuleAnswerShape) {
			return http.StatusForbidden, "questionnaire response unexpectedly carries a subject"
		}
	case "pas-claim", "pas-claim-update":
		// Converged conformant PAS leg, served by either an injected LegResponder (in-process,
		// SHN member namespace → fence strict) or the native-forward responder (verbatim relay
		// of a real RI answering in its OWN namespace → stand down, R-7). The responder declares
		// which via res.ResponseSubjectForeign; the SHN-produced EOB side-effect is ALWAYS fenced.
		var responseShape struct {
			ResourceType string `json:"resourceType"`
		}
		_ = json.Unmarshal(responseFHIR, &responseShape)
		if res.ResponseSubjectForeign || responseShape.ResourceType == "Bundle" {
			if pasResponseSubjectMismatch(responseFHIR) != nil && refuses(RulePatientAnswer) {
				return refusePASResponseSubjects(corrID, leg, http.StatusForbidden, responseFHIR)
			}
		}
		if !res.ResponseSubjectForeign {
			refs, err := ParsePASResponsePatients(responseFHIR)
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
		return fencePASSideEffects(boundPatientRef, res)
	case "pas-claim-inquire":
		// An inquiry's answer takes two shapes by IG line (inquire.go), and a
		// legitimate answer may name NO authorization at all — "nothing matched" is
		// an answer, not a subject violation. So the rule here is about the patients
		// the answer DOES name, and it reads ALL of them, as deep as the submit
		// legs' response check: every subject-bearing field of every entry of every
		// `return` Bundle, contained resources included. An answer whose Coverage
		// beneficiary or Patient entry names a different member than its
		// ClaimResponses is refused — one inquiry asks about one member.
		if !consistentPASInquiryAnswerSubjects(responseFHIR) && refuses(RulePatientAnswer) {
			return http.StatusForbidden, "PAS inquiry answer has inconsistent patient linkage"
		}
		// The payer answers in its own patient namespace, so the bound-member
		// comparison applies only where the answer is this gateway's own.
		//
		// A parse failure is a REFUSAL here, exactly as on the submit legs above.
		// It used to be swallowed, which made the fence fail open: an answer this
		// reader could not read was treated as an answer naming nobody, so nothing
		// was compared and the answer went out. The one error that is not a
		// failure is "this answer names no authorization at all" — an inquiry that
		// matched nothing is a legitimate answer, and it names no patient to
		// compare.
		if !res.ResponseSubjectForeign {
			refs, err := ParsePASResponsePatients(responseFHIR)
			switch {
			case errors.Is(err, ErrNoPASResponsePatient):
				// nothing matched; no patient to compare
			case err != nil:
				return http.StatusInternalServerError, "parse response subject failed"
			}
			for _, ref := range refs {
				if ref != boundPatientRef {
					return http.StatusForbidden, "response patient does not match request patient"
				}
			}
		}
		// The decision EOBs an inquiry records are this gateway's own, built from
		// the bound member — fenced unconditionally, like the submit leg's.
		return fencePASSideEffects(boundPatientRef, res)
	}
	return 0, ""
}

// fencePASSideEffects member-fences the prior-authorization side-effects this
// gateway produced itself. They are always built from the bound member, on every
// responder path, so they are fenced unconditionally — a relayed answer stands the
// RESPONSE fence down, never this one.
func fencePASSideEffects(boundPatientRef string, res LegResult) (int, string) {
	for _, se := range res.SideEffectFHIR {
		ref, err := parseEOBPatient(se)
		if err != nil {
			return http.StatusInternalServerError, "parse side-effect subject failed"
		}
		if ref != boundPatientRef {
			return http.StatusForbidden, "side-effect patient does not match request patient"
		}
	}
	return 0, ""
}
