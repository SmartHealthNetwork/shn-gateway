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
// request that names none is the older questionnaire request envelope. Every
// patient the request names is bound to the token's subject before the
// responder sees it; the answer is fenced and relayed.
func (g *Gateway) handleDTRInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, reqJSON []byte, answerTok string) {
	ctx := r.Context()

	// An SDC adaptive $next-question round carries a QuestionnaireResponse ABOUT A PATIENT
	// (its answers so far), and a package request carries the patient's Coverage and
	// orders — so both get the (A) bind every patient-bearing leg gets (the conformant PAS
	// bind's rule): every carried subject must resolve to the token's subject,
	// independently of what the responder does with it. Fails closed BEFORE the responder
	// (and before any partner forward) sees the bytes.
	var (
		nextQuestionSubject string
		isNextQuestion      bool
	)
	switch RequestFrameOperation(ctx) {
	case shnsdk.FrameOperationQuestionnairePackage:
		if status, msg := g.bindPackageParameters(ctx, reqJSON, tok.Subject); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	case shnsdk.FrameOperationNextQuestion:
		subject, ok := framedNextQuestionSubject(reqJSON)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse next-question input failed"})
			return
		}
		nextQuestionSubject, isNextQuestion = subject, true
	case "":
		// The older request envelope, still accepted from requesters that do not yet
		// name the operation. A framed operation never reads it.
		nextQuestionSubject, isNextQuestion = nextQuestionRequestSubject(reqJSON)
		if !isNextQuestion {
			if status, msg := g.bindLegacyPackageRequest(ctx, reqJSON, tok.Subject); status != 0 {
				writeJSON(w, status, map[string]string{"error": msg})
				return
			}
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported DTR operation"})
		return
	}
	if isNextQuestion {
		if status, msg := g.bindNextQuestionSubjectContext(ctx, nextQuestionSubject, tok.Subject); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}

	result, err := g.cfg.Responder.Handle(ctx, "dtr-questionnaire-fetch", env.Metadata.CorrelationID, tok.Subject, reqJSON)
	if err != nil {
		// build/marshal fault (gateway's own) → 500
		g.responderFailed(w, "dtr-questionnaire-fetch", err)
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errOwnershipFault})
		return
	}
	if isNextQuestion {
		// (C) for the adaptive round: the answered QuestionnaireResponse must be about the
		// SAME patient the request carried — a partner (or a relay) must not swap the subject.
		if status, msg := fenceNextQuestionSubject(nextQuestionSubject, result); status != 0 {
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	} else if status, msg := g.fenceResponseSubject("dtr-questionnaire-fetch", "", result); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
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
			writeJSON(w, status, map[string]string{"error": msg})
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
type packagePatients struct {
	d       *relay.Document
	members map[string]bool
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
		// so it is refused rather than left unbound.
		id := docText(d, n, "id")
		if id == "" {
			return &errPatientNoID
		}
		p.members[id] = true
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
			return &errNotPatientRef
		}
		p.members[member] = true
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
			if top && docText(d, param, "name") == "coverage" {
				if d.Kind(res) != relay.KindObject || docText(d, res, "resourceType") != "Coverage" {
					return 0, &errNotCoverage
				}
				if _, ok := d.Member(res, "beneficiary"); !ok {
					return 0, &errNotPatientRef
				}
				coverages++
			}
			if refused := p.resource(res); refused != nil {
				return 0, refused
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

// bindPackagePatients binds the patients a questionnaire request names to
// the token's subject: they must all be one member, whom this holder's own
// system of record resolves to tokenSubject.
func (g *Gateway) bindPackagePatients(ctx context.Context, members map[string]bool, tokenSubject string) (int, string) {
	if len(members) != 1 {
		return http.StatusForbidden, "questionnaire-package request covers more than one patient"
	}
	for member := range members {
		return g.bindNextQuestionSubjectContext(ctx, "Patient/"+member, tokenSubject)
	}
	return 0, ""
}

// bindPackageParameters is the (A) inbound bind for a framed
// $questionnaire-package: the input must carry at least one coverage (the
// input profile's cardinality), and every patient it names — each coverage
// beneficiary, each order's subject and patient, every Patient resource and
// every other resource's patient reference — must be the token's subject.
func (g *Gateway) bindPackageParameters(ctx context.Context, body []byte, tokenSubject string) (int, string) {
	d, err := relay.Doc(relay.NewBody(body, relay.OriginPeerFrame))
	if err != nil || d.Kind(d.Root()) != relay.KindObject || docText(d, d.Root(), "resourceType") != "Parameters" {
		return http.StatusBadRequest, "parse questionnaire-package parameters failed"
	}
	p := &packagePatients{d: d, members: map[string]bool{}}
	coverages, refused := p.parameters(d.Root())
	if refused != nil {
		return refused.status, refused.msg
	}
	if coverages == 0 {
		return http.StatusBadRequest, "questionnaire-package request has no coverage"
	}
	return g.bindPackagePatients(ctx, p.members, tokenSubject)
}

// bindLegacyPackageRequest is the (A) inbound bind for the older
// questionnaire request envelope: every patient its carried coverage and
// order name must be the token's subject. An envelope that carries neither
// (a request by canonical alone) names no patient and is not bound.
func (g *Gateway) bindLegacyPackageRequest(ctx context.Context, body []byte, tokenSubject string) (int, string) {
	parseFailed := func() (int, string) { return http.StatusBadRequest, "parse questionnaire fetch failed" }
	d, err := relay.Doc(relay.NewBody(body, relay.OriginPeerFrame))
	if err != nil || d.Kind(d.Root()) != relay.KindObject {
		return parseFailed()
	}
	p := &packagePatients{d: d, members: map[string]bool{}}
	for _, key := range []string{"coverage", "order"} {
		res, ok := d.Member(d.Root(), key)
		if !ok || d.Kind(res) == relay.KindNull {
			continue
		}
		if refused := p.resource(res); refused != nil {
			if *refused == errParseParameters {
				return parseFailed()
			}
			return refused.status, refused.msg
		}
	}
	if len(p.members) == 0 {
		return 0, ""
	}
	return g.bindPackagePatients(ctx, p.members, tokenSubject)
}

// bindNextQuestionSubject is the (A) inbound bind for an adaptive $next-question round:
// the carried QuestionnaireResponse's subject ("Patient/<member>", the member namespace) must
// resolve — via this holder's OWN SystemOfRecord, never the payload — to the token's subject
// PCI. Mirrors conformantPASBind: unknown member → 400, mismatch → 403.
func (g *Gateway) bindNextQuestionSubjectContext(ctx context.Context, subject, tokenSubject string) (int, string) {
	member, ok := patientMember(subject)
	if !ok {
		return http.StatusBadRequest, "next-question request carries no patient subject"
	}
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return status, msg
	}
	if !found {
		return http.StatusBadRequest, "unknown member"
	}
	if pci != tokenSubject {
		return http.StatusForbidden, "token subject does not match request patient"
	}
	return 0, ""
}

// fenceNextQuestionSubject is the (C) outbound fence for an adaptive $next-question round:
// the answered questionnaire-response must name the patient the request carried. A
// non-2xx relay (result.Status set) carries no QuestionnaireResponse and passes through to
// respondLegError untouched; an unparseable 2xx is refused (502) rather than relayed blind.
func fenceNextQuestionSubject(requestSubject string, res LegResult) (int, string) {
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
		return http.StatusBadGateway, "next-question response is not a questionnaire-response"
	}
	subj, serr := questionnaireResponseSubject(qr)
	if serr != nil || subj != requestSubject {
		return http.StatusForbidden, "response patient does not match request patient"
	}
	return 0, ""
}

// fenceResponseSubject is the (C) outbound fence: a connector must not swap the
// patient between the request it was handed and the response it returned. It
// compares the response resource's patient ref to boundPatientRef (the inbound
// member-namespace ref, e.g. "Patient/<member>", already proven by (A) to resolve
// to pci == tok.Subject). NOT compared to tok.Subject, which is a derived PCI.
// The response is read after the same ownership check its transmit applies.
// Returns (0,"") on pass or (status, msg) to write. Per-leg arms are added as
// each leg moves behind the seam.
func (g *Gateway) fenceResponseSubject(leg, boundPatientRef string, res LegResult) (int, string) {
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
		if packageQuestionnaireHasSubject(responseFHIR) {
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
		if (res.ResponseSubjectForeign || responseShape.ResourceType == "Bundle") && !consistentPASResponseSubjects(responseFHIR) {
			return http.StatusForbidden, "PAS response has inconsistent patient linkage"
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
