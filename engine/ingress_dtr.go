// ingress_dtr.go contains the legacy DTR preparation helper and operation
// framing guard. Authenticated native DTR ingress carries the participant's
// Parameters without implicit source enrichment (PCV-08/10). A participant
// constructing its own Parameters can use the explicit SDK builder.
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
	// request is the EHR's Parameters, exact or with the patient's Coverage
	// (relay.EditDTRCoverageObtain) and, for explicit source assembly, the
	// provider's own Patient record (relay.EditDTRPatientObtain) appended.
	request relay.Payload
	// member is the patient every resource names; pci is the network's
	// identifier for that patient.
	member, pci string
	// coverages are the Coverage resources the request carries onward, in
	// order: the EHR's, or the one obtained.
	coverages [][]byte
	// resources are every resource parameter the EHR sent (top level and
	// parts), for payor lookups.
	resources [][]byte
	// obtained holds what the system of record returned with an obtained
	// Coverage: the records its search included (payor Organizations).
	obtained [][]byte
}

// dtrPackageParam is one top-level parameter of the request, read in place.
type dtrPackageParam struct {
	name     string
	resource relay.NodeID
	hasRes   bool
}

// prepareDTRPackageRequest is the legacy, currently uncalled source-assembly
// helper. It reads the EHR's Parameters without re-encoding them:
//
//   - the patient is the one every coverage beneficiary and order subject
//     names; a request naming none is refused (422), one naming several is
//     refused (403), and the patient must be known to the system of record
//     (403 otherwise);
//   - every resource the request carries, at the top level or in a part, is
//     fenced to that patient by its binding path (403 on a mismatch);
//   - a request carrying no coverage parameter gains one: the patient's
//     Coverage from the system of record's Coverage search (the
//     dtr-coverage-obtain edit), recorded as a PrefetchObtainedEvent. It is
//     refused when the system names the patient by another id (422), holds no
//     Coverage (422), holds Coverages naming different payers (422), cannot
//     search (422) or is unavailable (503); a Coverage about another patient
//     is a 502.
//
// Nothing else changes: the EHR's parameters keep their order, repeats,
// values (a canonical's |version included), meta and unknown members.
func (g *Gateway) prepareDTRPackageRequest(ctx context.Context, raw []byte) (dtrIngressRequest, int, string) {
	return g.assembleDTRPackageRequest(ctx, raw, false)
}

// assembleDTRPackageRequest retains authenticated source assembly separately
// from native admission. includePatient explicitly requests the real source
// Patient; AcceptUnknownMembers never requests an insertion.
func (g *Gateway) assembleDTRPackageRequest(ctx context.Context, raw []byte, includePatient bool) (dtrIngressRequest, int, string) {
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

	// The patient: every coverage beneficiary and order subject.
	patients := map[string]bool{}
	hasCoverage := false
	for _, p := range params {
		// A coverage parameter is the EHR's own, whatever it carries, so the
		// gateway never adds one beside it; one that carries no resource
		// cannot be bound or routed and is refused.
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
		ref := patientRefOf(span(p.resource))
		if ref == "" {
			return out, http.StatusForbidden, "questionnaire-package " + p.name + " names no patient"
		}
		member, ok := patientMember(ref)
		if !ok {
			return out, http.StatusForbidden, "questionnaire-package " + p.name + " names no patient"
		}
		patients[member] = true
	}
	switch len(patients) {
	case 0:
		return out, http.StatusUnprocessableEntity, "cannot bind the request to a patient"
	case 1:
	default:
		return out, http.StatusForbidden, "inconsistent patient reference in ingress payload"
	}
	for m := range patients {
		out.member = m
	}
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

	// Every resource the request carries, at any depth of parts.
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
				if err := fence.check(value); err != nil {
					return http.StatusForbidden, "parameter " + name + " refused: " + err.Error()
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
		coverage, status, msg := g.obtainDTRCoverage(ctx, &out, fence)
		if status != 0 {
			return out, status, msg
		}
		changes = append(changes, relay.Change{Edit: relay.EditDTRCoverageObtain, Ops: []relay.Op{doc.AppendElement(paramArr, element("coverage", coverage))}})
		out.coverages = [][]byte{coverage}
	}
	if includePatient && !carriesPatient(raw, out.member) {
		patient, status, msg := g.obtainDTRPatient(ctx, out.member, fence)
		if status != 0 {
			return out, status, msg
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
// by explicit source assembly: the system of record's own record for
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
			return nil, http.StatusBadGateway, "system of record returned a Binary resource"
		}
		return nil, http.StatusBadGateway, "system of record returned another patient's resource"
	}
	return patient, 0, ""
}

// dtrCoverageNamedDifferently refuses a request without coverage whose
// patient the system of record names by another id.
const dtrCoverageNamedDifferently = "system of record names the patient differently from the request; supply the coverage parameter in the request"

// obtainDTRCoverage reads the Coverage a request without one is sent with:
// the system of record's Coverage search for the patient, recorded as a
// PrefetchObtainedEvent (key coverage, operation questionnaire-package). The
// Coverage is the system's bytes. Several Coverages are accepted only when
// they name one payer, and the first is used (the routing rule of the
// member's own coverage); the records the search included are kept in
// out.obtained for the payor lookup. The system's id for the patient is read
// here, only for a request that needs a Coverage.
func (g *Gateway) obtainDTRCoverage(ctx context.Context, out *dtrIngressRequest, fence patientFence) ([]byte, int, string) {
	const leg = "dtr-questionnaire-fetch"
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, out.member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	sorID := ""
	if found {
		id, ok := strings.CutPrefix(ref, "Patient/")
		if !ok || !fhirIDRE.MatchString(id) {
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, status, msg
		}
		sorID = id
	}
	switch {
	case sorID == "":
		return nil, http.StatusUnprocessableEntity, "patient not found in system of record"
	case sorID != out.member:
		return nil, http.StatusUnprocessableEntity, dtrCoverageNamedDifferently
	}
	s := runSoRSearch(ctx, g.cfg.SoR, "Coverage", sorID, false)
	g.recordPrefetch(leg, prefetchObtained{Key: "coverage", Operation: shnsdk.FrameOperationQuestionnairePackage,
		Query: s.Query, Outcome: s.Outcome, Reason: s.Reason, Count: s.Count, Pages: s.Pages})
	switch s.Outcome {
	case SearchOK:
	case SearchZero:
		return nil, http.StatusUnprocessableEntity, "no coverage in request or system of record"
	default:
		status, msg := coverageOmitted(s.Outcome)
		return nil, status, msg
	}
	record := func(m searchMatch) []byte { return s.pages[m.page][m.start:m.end] }
	for _, m := range s.includes {
		out.obtained = append(out.obtained, record(m))
	}
	covs := make([][]byte, 0, len(s.matches))
	for _, m := range s.matches {
		c := record(m)
		if err := fence.check(c); err != nil {
			return nil, http.StatusBadGateway, "system of record returned another patient's resource"
		}
		covs = append(covs, c)
	}
	if len(covs) > 1 {
		resolve, readErr := g.obtainedPayorResolver(ctx, out.obtained)
		var first shnsdk.PayerIdentifier
		for i, c := range covs {
			pid, perr := shnsdk.ParseCoveragePayer(c, resolve)
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

// obtainedPayorResolver resolves a payor reference of an obtained Coverage:
// among the records the system of record's search included, then in that
// system. The returned error pointer holds a failed read.
func (g *Gateway) obtainedPayorResolver(ctx context.Context, included [][]byte) (func(string) ([]byte, bool), *error) {
	local := resolverFromResources(included)
	fromSoR, readErr := sorReferenceCallback(ctx, g.cfg.SoR)
	return func(ref string) ([]byte, bool) {
		if b, ok := local(ref); ok {
			return b, true
		}
		return fromSoR(ref)
	}, readErr
}

// dtrIngressRecipient routes a prepared request by every coverage it
// carries: each must name a registered payer, and all of them the same one
// (422 otherwise). A kept coverage's payor Organization is looked up among
// the request's own resources; an obtained one's among the records the
// system of record returned with it, then in that system.
func (g *Gateway) dtrIngressRecipient(ctx context.Context, prepared dtrIngressRequest) (string, int, string) {
	local := resolverFromResources(prepared.resources)
	resolve := local
	readErr := new(error)
	if prepared.request.Ownership() == relay.OwnershipEdited {
		var fromObtained func(string) ([]byte, bool)
		fromObtained, readErr = g.obtainedPayorResolver(ctx, prepared.obtained)
		resolve = func(ref string) ([]byte, bool) {
			if b, ok := local(ref); ok {
				return b, true
			}
			return fromObtained(ref)
		}
	}
	recipient := ""
	for _, cov := range prepared.coverages {
		holder, _, status, msg := g.recipientForWith(cov, resolve)
		if *readErr != nil {
			status, msg := SoRFailureResponse(*readErr)
			return "", status, msg
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
