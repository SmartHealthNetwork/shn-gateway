// ingress_dtr.go — the DTR $questionnaire-package ingress: the EHR's own
// operation input (its Parameters) is carried to the payer exactly, or, when
// the participant opts in to enrichment, with the registered edits that add,
// from its system of record, the patient's Coverage when the request carries
// none and the patient's own record when the request carries no Patient. The
// ingress
// binds every resource the request carries to one patient, routes by every
// coverage, and names the operation in the request frame. It does not invoke
// the Populator: the EHR's own DTR application populates.
package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// errFramedDTRUnsupported refuses a framed DTR operation to a recipient that
// has not declared shnsdk.RequestFrameV1Op: such a recipient drops the
// operation header and would misread the body.
var errFramedDTRUnsupported = shnsdk.ErrFramedDTRUnsupported

// framedDTRRefusal refuses, before anything is sent, a DTR operation to a
// recipient whose registry entry does not declare shnsdk.RequestFrameV1Op
// (502, naming the upgrade the recipient needs). It returns (0, "") when the
// recipient declares it.
func (g *Gateway) framedDTRRefusal(recipient string) (int, string) {
	if entry, ok := g.cfg.Reg.Lookup(recipient); ok && shnsdk.SupportsRequestFrameV1Op(entry.RequestFrames) {
		return 0, ""
	}
	return http.StatusBadGateway, errFramedDTRUnsupported.Error()
}

// dtrPackageContentType is the media type a $questionnaire-package input is
// carried with.
const dtrPackageContentType = "application/fhir+json"

// dtrIngressRequest is an EHR's $questionnaire-package request ready to leave
// the provider.
type dtrIngressRequest struct {
	// request is the EHR's Parameters: exact, or, only when the participant
	// opts in to enrichment (Config.EnrichNativeRequests), with the patient's
	// Coverage (relay.EditDTRCoverageObtain) and the provider's own Patient
	// record (relay.EditDTRPatientObtain) appended.
	request relay.Payload
	// member is the patient every resource names; pci is the network's
	// identifier for that patient.
	member, pci string
	// coverages are the Coverage resources the request is routed by, in
	// order: the EHR's, or the one obtained (carried onward only under
	// enrichment).
	coverages [][]byte
	// resources are every resource parameter the EHR sent (top level and
	// parts), for payor lookups.
	resources [][]byte
	// obtained holds what the system of record returned with an obtained
	// Coverage: the records its search included (payor Organizations).
	obtained [][]byte
	// coverageObtained is true when the Coverage the request is routed by was
	// read from the system of record (appended or only routed by), set once
	// its search answered: its payor is looked up among obtained, then in
	// that system.
	coverageObtained bool
	// carried records the finding for a Patient left out below strict
	// (RulePrefetchFill), once the request is routed: a request refused
	// before it is carried records none.
	carried carriedFindings
}

// dtrPackageParam is one top-level parameter of the request, read in place.
type dtrPackageParam struct {
	name     string
	resource relay.NodeID
	hasRes   bool
}

// prepareDTRPackageRequest reads the EHR's $questionnaire-package Parameters
// without re-encoding them and prepares them for the network:
//
//   - the patient is the one every coverage beneficiary and order subject
//     names; a request naming none is refused (422 or, when a coverage or
//     order names no patient, 403), and the patient must be known to the
//     system of record (403 otherwise): the subject, refused at every level;
//   - a request naming several patients (403), a coverage or order naming
//     none beside one that names the subject (403), and a resource the
//     request carries, at the top level or in a part, that the fence does
//     not bind to that patient by its binding path (403) are the payload's
//     own consistency (RulePatientMixed): refused at strict, recorded and
//     carried at observe, carried at none;
//   - a request carrying no coverage parameter is routed by the patient's
//     Coverage from the system of record's Coverage search, recorded as a
//     PrefetchObtainedEvent, and gains it (the dtr-coverage-obtain edit) only
//     under enrichment. It is refused when the system holds no Coverage
//     (422), when the Coverages it chooses (routingCoverageChoice) name
//     different payers (422), when it cannot search (422) or is unavailable
//     (503), and, under enrichment, when the system
//     names the patient by another id (422); a Coverage about another patient
//     is a 502. The request is routed by that Coverage, so these refuse at
//     every level.
//
// Nothing else changes: the EHR's parameters keep their order, repeats,
// values (a canonical's |version included), meta and unknown members.
func (g *Gateway) prepareDTRPackageRequest(ctx context.Context, raw []byte) (dtrIngressRequest, int, string) {
	var out dtrIngressRequest
	parseFailed := func() (dtrIngressRequest, int, string) {
		return out, http.StatusBadRequest, "parse questionnaire-package parameters failed"
	}
	body := relay.NewBody(raw, relay.OriginIngressRequest)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject || docText(doc, doc.Root(), "resourceType") != "Parameters" {
		return parseFailed()
	}
	root := doc.Root()
	paramArr, hasParams := doc.Member(root, "parameter")
	var params []dtrPackageParam
	if hasParams {
		if doc.Kind(paramArr) != relay.KindArray {
			return parseFailed()
		}
		for _, p := range doc.Elems(paramArr) {
			if doc.Kind(p) != relay.KindObject {
				return parseFailed()
			}
			res, hasRes := doc.Member(p, "resource")
			params = append(params, dtrPackageParam{name: docText(doc, p, "name"), resource: res, hasRes: hasRes})
		}
	}
	span := func(n relay.NodeID) []byte {
		s, e := doc.Span(n)
		return raw[s:e]
	}

	// boundElsewhere reports whether any coverage or order parameter names a
	// patient. It is read once, when a parameter naming none is met.
	var bound *bool
	boundElsewhere := func() bool {
		if bound == nil {
			found := false
			for _, p := range params {
				if !p.hasRes || (p.name != "coverage" && p.name != "order") || doc.Kind(p.resource) != relay.KindObject {
					continue
				}
				if _, ok := patientMember(patientRefOf(span(p.resource))); ok {
					found = true
					break
				}
			}
			bound = &found
		}
		return *bound
	}

	// The patient: every coverage beneficiary and order subject. The
	// request's subject is the patient its first coverage names, else its
	// first order's (the coverage is the operation's input about the member
	// the questionnaire is for); a request naming none cannot be bound
	// (RuleSubjectPCI, refused at every level).
	patients := map[string]bool{}
	subject, coverageSubject := "", ""
	hasCoverage := false
	for _, p := range params {
		// A coverage parameter is the EHR's own, whatever it carries, so the
		// gateway never adds one beside it; one that carries no resource
		// cannot be bound or routed and is refused.
		//
		// This check and the not-a-Coverage one below stay refusals at every
		// level: the request is routed by the payer each Coverage names
		// (dtrIngressRecipient) and its subject is read from the Coverage's
		// beneficiary, so a coverage parameter that is not a Coverage is
		// routing and subject, not the request's shape (RuleRequestShape).
		// Carried, it would either be routed by nothing or have a Coverage
		// from the system of record added beside the EHR's own.
		if p.name == "coverage" && !p.hasRes {
			return out, http.StatusBadRequest, "coverage parameter carries no resource"
		}
		if !p.hasRes || (p.name != "coverage" && p.name != "order") {
			continue
		}
		if doc.Kind(p.resource) != relay.KindObject {
			return parseFailed()
		}
		if p.name == "coverage" {
			if docText(doc, p.resource, "resourceType") != "Coverage" {
				return out, http.StatusBadRequest, "questionnaire-package coverage parameter is not a Coverage"
			}
			hasCoverage = true
			out.coverages = append(out.coverages, span(p.resource))
		}
		member, ok := patientMember(patientRefOf(span(p.resource)))
		if !ok {
			// A coverage or order naming no patient beside one that names the
			// subject is the payload's own consistency (RulePatientMixed): not
			// checked at none, recorded at observe, refused at strict, and
			// carried below strict behind the subject the others bind. When no
			// coverage or order names a patient the subject cannot be read,
			// which refuses at every level.
			if !boundElsewhere() || g.guard(ctx, KindContent, RulePatientMixed, raw) {
				return out, http.StatusForbidden, "questionnaire-package " + p.name + " names no patient"
			}
			continue
		}
		patients[member] = true
		if subject == "" {
			subject = member
		}
		if p.name == "coverage" && coverageSubject == "" {
			coverageSubject = member
		}
	}
	if coverageSubject != "" {
		subject = coverageSubject
	}
	switch len(patients) {
	case 0:
		return out, http.StatusUnprocessableEntity, "cannot bind the request to a patient"
	case 1:
	default:
		// Another patient named inside one request: the payload's own
		// consistency (RulePatientMixed). Below strict the request is carried
		// bound to its subject.
		if g.guard(ctx, KindContent, RulePatientMixed, raw) {
			return out, http.StatusForbidden, "inconsistent patient reference in ingress payload"
		}
	}
	out.member = subject
	pci, found, err := g.resolveSubjectPCI(ctx, out.member, raw)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return out, status, msg
	}
	if !found {
		return out, http.StatusForbidden, "request patient does not resolve"
	}
	out.pci = pci
	// The payer binds the request's patient by the member id the request
	// names, so every resource is fenced to that id alone.
	fence := newPatientFence(shnsdk.MemberSystem, out.member, nil, out.member)

	// Every resource the request carries, at any depth of parts. The fence
	// over resources the request itself carries is the payload's own
	// consistency (RulePatientMixed): not checked at none, recorded at
	// observe, refused at strict; below strict the resource is carried as
	// sent.
	checkCarried := g.policy().Runs(KindContent, RulePatientMixed)
	var walk func(arr relay.NodeID, path string) (int, string)
	walk = func(arr relay.NodeID, path string) (int, string) {
		if doc.Kind(arr) != relay.KindArray {
			return http.StatusBadRequest, "parse questionnaire-package parameters failed"
		}
		for _, p := range doc.Elems(arr) {
			if doc.Kind(p) != relay.KindObject {
				return http.StatusBadRequest, "parse questionnaire-package parameters failed"
			}
			name := path + docText(doc, p, "name")
			if res, ok := doc.Member(p, "resource"); ok {
				value := span(res)
				if checkCarried {
					if err := fence.check(value); err != nil && g.guard(ctx, KindContent, RulePatientMixed, raw) {
						return http.StatusForbidden, "parameter " + name + " refused: " + err.Error()
					}
				}
				out.resources = append(out.resources, value)
			}
			if parts, ok := doc.Member(p, "part"); ok {
				if status, msg := walk(parts, name+"."); status != 0 {
					return status, msg
				}
			}
		}
		return 0, ""
	}
	if hasParams {
		if status, msg := walk(paramArr, ""); status != 0 {
			return out, status, msg
		}
	}

	var changes []relay.Change
	element := func(name string, resource []byte) []byte {
		e := make([]byte, 0, len(resource)+len(name)+24)
		e = append(e, `{"name":"`...)
		e = append(e, name...)
		e = append(e, `","resource":`...)
		e = append(e, resource...)
		return append(e, '}')
	}
	if !hasCoverage {
		// The Coverage obtained for a request that carries none is what the
		// request is routed by (dtrIngressRecipient): without it there is no
		// payer to carry the request to. Every failure to obtain it is
		// therefore routing and refuses at every level, not a fill carried as
		// sent below strict (RulePrefetchFill), which would only turn the
		// system of record's own answer into "no coverage".
		// It is appended to the request (E-04) only when the participant opts in
		// to enrichment (Config.EnrichNativeRequests) and the coverage
		// template's search finds one; otherwise the request is carried as
		// sent and the Coverage is only routed by.
		appended, routed, status, msg := g.obtainDTRCoverage(ctx, &out, fence)
		if status != 0 {
			return out, status, msg
		}
		if appended != nil {
			changes = append(changes, relay.Change{Edit: relay.EditDTRCoverageObtain, Ops: []relay.Op{doc.AppendElement(paramArr, element("coverage", appended))}})
		}
		out.coverages = [][]byte{routed}
	}
	// E-05 is an enrichment: off on native traffic unless the participant opts in
	// (Config.EnrichNativeRequests).
	if g.cfg.EnrichNativeRequests && !carriesPatient(raw, out.member) {
		patient, status, msg := g.obtainDTRPatient(ctx, out.member, fence)
		// The fill fence refuses at every level: SHN never inserts another
		// patient's record. Any other failure to fill refuses at strict
		// (RulePrefetchFill); below strict the request is carried without
		// the Patient, binding by member id alone on both sides as it does
		// when the system of record does not hold the patient, and the
		// finding is recorded once the request is routed (carriedFindings),
		// as the CDS Hooks ingress records its fill findings.
		if status != 0 {
			if msg == fillFencedBinary || msg == fillFencedOtherPatient {
				return out, status, msg
			}
			pol := g.policy()
			if pol.Decide(KindContent, RulePrefetchFill, VerdictUnavailable) == Refuse {
				guardDefect(ctx, pol, g.emitFinding, KindContent, RulePrefetchFill, VerdictUnavailable, raw)
				return out, status, msg
			}
			if pol.Runs(KindContent, RulePrefetchFill) {
				out.carried = append(out.carried, func(routed context.Context) {
					guardDefect(routed, pol, g.emitFinding, KindContent, RulePrefetchFill, VerdictUnavailable, raw)
				})
			}
		}
		if patient != nil {
			changes = append(changes, relay.Change{Edit: relay.EditDTRPatientObtain, Ops: []relay.Op{doc.AppendElement(paramArr, element("referenced", patient))}})
		}
	}
	if len(changes) == 0 {
		out.request = relay.Exact(body, dtrPackageContentType)
		return out, 0, ""
	}
	out.request, err = relay.ApplyChanges(body, dtrPackageContentType, changes...)
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return out, http.StatusUnprocessableEntity, signed.Error()
	case err != nil:
		return out, http.StatusInternalServerError, "prepare questionnaire-package request failed"
	}
	return out, 0, ""
}

// obtainDTRPatient reads the Patient a request carrying none is sent with
// under Config.EnrichNativeRequests: the system of record's own record for
// the bound patient, by the reference the system names it by, recorded as a
// PrefetchObtainedEvent (key patient, operation questionnaire-package). The
// payer's side, which may not hold the member, derives the subject from
// this record exactly as this side derives it from the same record. Nothing
// is added when the system does not hold the patient, or names it by another
// id (the request's references would not resolve to it): the request is sent
// as it is and binds by member id alone on both sides. A record about another
// patient, or not a Patient, is refused.
func (g *Gateway) obtainDTRPatient(ctx context.Context, member string, fence patientFence) ([]byte, int, string) {
	const leg = "dtr-questionnaire-fetch"
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	if !found {
		return nil, 0, ""
	}
	id, ok := strings.CutPrefix(ref, "Patient/")
	if !ok || !fhirIDRE.MatchString(id) {
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	if id != member {
		return nil, 0, ""
	}
	query := "Patient/" + id
	record := func(outcome SearchOutcome, reason string, count int) {
		g.recordPrefetch(leg, prefetchObtained{Key: prefetchPatientKey, Operation: shnsdk.FrameOperationQuestionnairePackage,
			Query: query, Outcome: outcome, Reason: reason, Count: count})
	}
	patient, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, query)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		outcome := SearchMalformed
		if status == http.StatusServiceUnavailable {
			outcome = SearchUnavailable
		}
		record(outcome, string(safeSoRError(err).Kind), 0)
		return nil, status, msg
	}
	if !found {
		record(SearchZero, "", 0)
		return nil, http.StatusUnprocessableEntity, "patient not found in system of record"
	}
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if decodeMessage(patient, &head) != nil || head.ResourceType != "Patient" {
		record(SearchMalformed, "not a Patient", 0)
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	record(SearchOK, "", 1)
	if err := fence.check(patient); err != nil {
		var ce *CompartmentError
		if errors.As(err, &ce) && ce.Reason == opaqueContentReason {
			return nil, http.StatusBadGateway, fillFencedBinary
		}
		return nil, http.StatusBadGateway, fillFencedOtherPatient
	}
	return patient, 0, ""
}

// dtrCoverageNamedDifferently refuses a request without coverage whose
// patient the system of record names by another id.
const dtrCoverageNamedDifferently = "system of record names the patient differently from the request; supply the coverage parameter in the request"

// dtrNoCoverageToRouteBy refuses a request without coverage for a member the
// system of record names no patient for (one it does not hold, or holds but
// cannot name): nothing to route by. It names what the participant sends
// instead.
const dtrNoCoverageToRouteBy = "no coverage to route by: send the coverage parameter (this gateway's system of record names no patient for this member)"

// obtainDTRCoverage reads the Coverage a request without one is routed by
// and, under enrichment, sent with: the system of record's Coverage search for
// the patient, recorded as a PrefetchObtainedEvent (key coverage, operation
// questionnaire-package). It returns the Coverage to append (nil for none) and
// the Coverage the request is routed by.
//
// Under enrichment the Coverage appended is the coverage prefetch value's
// search, narrowed as its advertised template is (status=active), with the
// payors; the request is routed on it, and it is appended. When that search
// finds none (a member with no active coverage), or the system cannot answer
// it, nothing is appended, and the request is routed as without enrichment,
// by the routing read, and refused only when that read fails too: the opt-in
// never leaves a member with less to route by than the default, except when
// the system of record names the patient by another id (refused above). Read
// only to route by, the search asks for every Coverage with its payor.
//
// Either way the request is routed on the Coverages routingCoverageChoice
// picks from the search (the active ones, else the others), accepted only
// when they name one payer; the first is used (the routing rule of the
// member's own coverage). The Coverage is the system's bytes; the records the
// search included are kept in out.obtained for the payor lookup. The system's
// id for the patient is read here, only for a request that needs a Coverage.
func (g *Gateway) obtainDTRCoverage(ctx context.Context, out *dtrIngressRequest, fence patientFence) (appended, routed []byte, status int, msg string) {
	const leg = "dtr-questionnaire-fetch"
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, out.member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, nil, status, msg
	}
	sorID := ""
	if found {
		id, ok := strings.CutPrefix(ref, "Patient/")
		if !ok || !fhirIDRE.MatchString(id) {
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, nil, status, msg
		}
		sorID = id
	}
	switch {
	case sorID == "":
		return nil, nil, http.StatusUnprocessableEntity, dtrNoCoverageToRouteBy
	case sorID != out.member && g.cfg.EnrichNativeRequests:
		// An appended Coverage would name the patient by an id the request
		// does not use.
		return nil, nil, http.StatusUnprocessableEntity, dtrCoverageNamedDifferently
	case sorID != out.member:
		// Read only to route by, never appended: the system's own id for the
		// patient serves. In that system Patient/<member> is another patient
		// (or none), so the Coverage is fenced to the system's id alone.
		fence = newPatientFence(shnsdk.MemberSystem, out.member, nil, sorID)
	}
	search := func(filters ...SearchDateRange) sorSearchset {
		s := runSoRSearch(ctx, g.cfg.SoR, "Coverage", sorID, false, filters...)
		g.recordPrefetch(leg, prefetchObtained{Key: "coverage", Operation: shnsdk.FrameOperationQuestionnairePackage,
			Query: s.Query, Outcome: s.Outcome, Reason: s.Reason, Count: s.Count, Pages: s.Pages})
		return s
	}
	if g.cfg.EnrichNativeRequests {
		s := search(prefetchSearchFilters("Coverage")...)
		switch s.Outcome {
		case SearchOK:
			coverage, status, msg := g.dtrRoutingCoverage(ctx, out, fence, s)
			if status != 0 {
				return nil, nil, status, msg
			}
			return coverage, coverage, 0, ""
		default:
			// No active coverage, or a system that could not answer the
			// template's search: nothing is appended, and the request is
			// routed by the routing read below, as without the opt-in.
		}
	}
	s := search()
	switch s.Outcome {
	case SearchOK:
	case SearchZero:
		return nil, nil, http.StatusUnprocessableEntity, "no coverage in request or system of record"
	default:
		status, msg := coverageOmitted(s.Outcome)
		return nil, nil, status, msg
	}
	coverage, status, msg := g.dtrRoutingCoverage(ctx, out, fence, s)
	if status != 0 {
		return nil, nil, status, msg
	}
	return nil, coverage, 0, ""
}

// dtrRoutingCoverage is the Coverage a request is routed by, of the matches
// of s, a Coverage search with at least one match: the first of the ones
// routingCoverageChoice picks, accepted only when they name one payer. Every
// match is fenced; the records the search included are kept in out.obtained
// for the payor lookup.
func (g *Gateway) dtrRoutingCoverage(ctx context.Context, out *dtrIngressRequest, fence patientFence, s sorSearchset) ([]byte, int, string) {
	out.coverageObtained = true
	record := func(m searchMatch) []byte { return s.pages[m.page][m.start:m.end] }
	for _, m := range s.includes {
		out.obtained = append(out.obtained, record(m))
	}
	all := make([][]byte, 0, len(s.matches))
	for _, m := range s.matches {
		c := record(m)
		if err := fence.check(c); err != nil {
			return nil, http.StatusBadGateway, fillFencedOtherPatient
		}
		all = append(all, c)
	}
	var covs [][]byte
	for _, i := range routingCoverageChoice(all) {
		covs = append(covs, all[i])
	}
	if len(covs) > 1 {
		refs, _, readErr := g.dtrPayorRefs(ctx, out)
		var first shnsdk.PayerIdentifier
		for i, c := range covs {
			pid, perr := shnsdk.ParseCoveragePayer(c, refs.resolve)
			if *readErr != nil {
				status, msg := SoRFailureResponse(*readErr)
				return nil, status, msg
			}
			if perr != nil || (i > 0 && pid != first) {
				return nil, http.StatusUnprocessableEntity, "ambiguous coverage for routing: the member's Coverage records do not name one payer"
			}
			first = pid
		}
	}
	return covs[0], 0, ""
}

// dtrPayorRefs resolves the payor references of a $questionnaire-package
// request's coverages among the request's own resources and, for a Coverage
// the system of record supplied, the records that system's search included,
// all by the agreement rule; then, for such a Coverage, in that system, and
// for a coverage the EHR sent, as keptPayorResolver says (only to route). The
// returned unresolvedPayor classifies a payor reference nothing resolved; the
// error pointer holds a failed read.
func (g *Gateway) dtrPayorRefs(ctx context.Context, prepared *dtrIngressRequest) (*payorRefs, *unresolvedPayor, *error) {
	refs := &payorRefs{local: carriedRefs(prepared.resources), disagree: payorDisagreesInRequest}
	if !prepared.coverageObtained {
		var unresolved *unresolvedPayor
		var readErr *error
		refs.next, unresolved, readErr = g.keptPayorResolver(ctx, prepared.member)
		return refs, unresolved, readErr
	}
	refs.local = append(refs.local, carriedRefs(prepared.obtained)...)
	refs.disagree = payorDisagreesWithSearch
	var readErr *error
	refs.next, readErr = sorReferenceCallback(ctx, g.cfg.SoR)
	return refs, new(unresolvedPayor), readErr
}

// dtrIngressRecipient routes a prepared request by every coverage it
// carries: each must name a registered payer, and all of them the same one
// (422 otherwise). Each payor Organization is looked up as dtrPayorRefs
// says.
func (g *Gateway) dtrIngressRecipient(ctx context.Context, prepared dtrIngressRequest) (string, int, string) {
	refs, unresolved, readErr := g.dtrPayorRefs(ctx, &prepared)
	recipient := ""
	for _, cov := range prepared.coverages {
		holder, status, msg := g.recipientForCoverages([][]byte{cov}, refs)
		if *readErr != nil {
			status, msg := SoRFailureResponse(*readErr)
			return "", status, msg
		}
		if msg == noPayerIdentifier {
			// An Organization reference nothing resolved names the remedy
			// that resolves it (unresolvedPayor); a resolved Organization
			// with no payer identifier, or another kind of payor, keeps the
			// bare text.
			return "", status, unresolved.refusal(msg)
		}
		if status != 0 {
			return "", status, msg
		}
		if recipient != "" && holder != recipient {
			return "", http.StatusUnprocessableEntity, "coverages name more than one payer"
		}
		recipient = holder
	}
	if recipient == "" {
		return "", http.StatusUnprocessableEntity, "no coverage in request or system of record"
	}
	return recipient, 0, ""
}

// keptPayorResolver resolves a payor Organization that a coverage the EHR
// sent names by reference alone, when the request's own resources do not
// (dtrPayorRefs asks it only then):
// the network's routing read, done only to choose the payer; nothing it reads
// is added to or changed in what is carried. It reads the Organization from
// the system of record only when that system names the patient by the
// request's member id (its records are the request's: a $questionnaire-package
// request names no fhirServer, so the system of record is the EHR's own server
// for it); the system's id for the patient is read once, only for such a
// reference. A system that cannot name the patient (its read fails) counts as
// one that does not hold it, as on a CDS Hooks request: the reference is
// unresolved, and the request is refused 422 naming the remedy. A reference
// written absolute, versioned, with a fragment, a leading slash or a dot
// segment is never read (payorOrganizationID). *unresolved classifies the
// last reference asked, when nothing resolved it, by the remedy that would
// (classifyUnresolvedPayor). *readErr holds a failed read of the Organization in the
// system of record.
func (g *Gateway) keptPayorResolver(ctx context.Context, member string) (resolve func(string) ([]byte, bool), unresolved *unresolvedPayor, readErr *error) {
	unresolved, readErr = new(unresolvedPayor), new(error)
	asked := false
	var fromSoR func(string) ([]byte, bool)
	var sorErr *error
	resolve = func(ref string) ([]byte, bool) {
		if id, ok := payorOrganizationID(ref, ""); ok && *readErr == nil {
			if !asked {
				asked = true
				named, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
				switch {
				case err != nil:
					// Not held, as far as this request can tell (the failed
					// read is on the sor.read observer event): nothing is
					// read from the system of record.
				case found && named == "Patient/"+member:
					fromSoR, sorErr = sorReferenceCallback(ctx, g.cfg.SoR)
				}
			}
			if fromSoR != nil {
				if b, ok := fromSoR("Organization/" + id); ok {
					return b, true
				}
				if *sorErr != nil {
					*readErr = *sorErr
					return nil, false
				}
			}
		}
		*unresolved = classifyUnresolvedPayor(ref)
		return nil, false
	}
	return resolve, unresolved, readErr
}
