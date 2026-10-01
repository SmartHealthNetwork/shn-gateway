// ingress_crd.go — CRD (CDS Hooks order-select) ingress: parse the conformant inbound request,
// prove every patient reference resolves to ONE pci before any leg, and prepare the EHR's own
// bytes for the network (the callback removed; absent prefetch obtained from the participant's
// system of record only when it opts in to enrichment). On the ingress the payload is EXTERNAL (the EHR), so cross-field patient
// consistency is not automatic the way it is for the /scenario Originator.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ingressCDSRequest is the conformant inbound CDS Hooks order-select request (pinned
// conformant shape). Only the patient-bearing fields are modelled; the full request is forwarded opaque.
type ingressCDSRequest struct {
	Hook         string `json:"hook"`
	HookInstance string `json:"hookInstance"`
	FHIRServer   string `json:"fhirServer"`
	Context      struct {
		UserID      string `json:"userId"`
		PatientID   string `json:"patientId"`
		DraftOrders struct {
			Entry []struct {
				Resource json.RawMessage `json:"resource"`
			} `json:"entry"`
		} `json:"draftOrders"`
	} `json:"context"`
	Prefetch map[string]json.RawMessage `json:"prefetch"`
}

// patientRefOf pulls a subject/beneficiary/patient reference from an arbitrary FHIR resource.
func patientRefOf(resource json.RawMessage) string {
	var r struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
		Beneficiary struct {
			Reference string `json:"reference"`
		} `json:"beneficiary"`
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	_ = decodeMessage(resource, &r)
	switch {
	case r.Subject.Reference != "":
		return r.Subject.Reference
	case r.Beneficiary.Reference != "":
		return r.Beneficiary.Reference
	case r.Patient.Reference != "":
		return r.Patient.Reference
	}
	return ""
}

// memberForPCI re-reads the bare context.patientId member (already validated by the subject fence).
func (g *Gateway) memberForPCI(body []byte) string {
	var req ingressCDSRequest
	_ = decodeMessage(body, &req)
	return strings.TrimPrefix(req.Context.PatientID, "Patient/")
}

// crdAnswerOutcome certifies the payer's CDS Hooks answer at line (the same
// rules the payer's gateway applies: a broken rule is a 502 naming it) and
// labels the exchange from the answer's coverage information, read without
// changing a byte: denied (not covered), pa-required, approved (covered or
// conditional without prior authorization), or answered (no coverage
// information). Returns (outcome, 0, "") or ("", status, msg).
func (g *Gateway) crdAnswerOutcome(ctx context.Context, respJSON []byte, line string) (string, int, string) {
	if refused := certifyCDSHooksAnswer(ctx, g.policy(), g.emitFinding, respJSON, line, "peer"); refused.Status != 0 {
		return "", refused.Status, refused.Message
	}
	obs, err := shnsdk.ParseCRDResponse(respJSON)
	if err != nil {
		// Below strict an unreadable answer is relayed as it is: the exchange
		// has no coverage information to label it by.
		if g.policy().Decide(KindCDSEnvelope, "response.json", VerdictInvalid) == Record {
			return "answered", 0, ""
		}
		exchangeOf(ctx).refusing(RefusalConformance)
		return "", http.StatusBadGateway, "payer CRD response is not a valid CDS Hooks response: response.json"
	}
	cov, ok := obs.Primary()
	switch {
	case !ok:
		return "answered", 0, ""
	case cov.Covered == shnsdk.CoveredNotCovered:
		return "denied", 0, ""
	case cov.PARequired():
		return "pa-required", 0, ""
	}
	return "approved", 0, ""
}

// ingressCRDSubjectPCI parses the request and returns the single bound pci, the patient
// context.patientId names (RuleSubjectPCI, network level: refused at every level). A request
// whose subject or prefetch cannot be read refuses at every level; a value of the wrong type
// elsewhere is the request's own shape (RuleRequestShape). Every
// other patient reference present (each draftOrders entry subject, each prefetch resource's
// subject/beneficiary/patient) must resolve to the SAME pci: that is the payload's own
// consistency (RulePatientMixed), not checked at none, recorded at observe and refused
// (403) at strict. A reference is relative (Patient/<id>) or absolute on the request's own
// fhirServer. Returns (pci, 0, "") on success or ("", status, msg) to write.
func (g *Gateway) ingressCRDSubjectPCIContext(ctx context.Context, body []byte) (string, int, string) {
	// The subject (context.patientId) and the prefetch the request is routed
	// by (its coverage) must read; a value of the wrong type anywhere else is
	// the request's own shape (RuleRequestShape).
	var req ingressCDSRequest
	var core struct {
		Context struct {
			PatientID string `json:"patientId"`
		} `json:"context"`
		Prefetch map[string]json.RawMessage `json:"prefetch"`
	}
	if err := decodeContent(body, &req, &core, func() bool { return g.guard(ctx, KindContent, RuleRequestShape, body) }); err != nil {
		return "", http.StatusBadRequest, "parse cds request failed"
	}
	if req.Context.PatientID == "" {
		return "", http.StatusBadRequest, "missing context.patientId"
	}
	member := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	pci, found, readErr := g.resolveSubjectPCI(ctx, member, body)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return "", status, msg
	}
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	// Every other patient reference must resolve to the SAME pci.
	if !g.policy().Runs(KindContent, RulePatientMixed) {
		return pci, 0, ""
	}
	var refs []string
	// A draft order is the order this PA is FOR — it MUST carry a patient subject. A
	// missing/renamed subject is a defect here (not skipped), so at strict an order with no
	// recognizable patient can never ride into the sealed request behind the bound subject's
	// authority.
	for _, e := range req.Context.DraftOrders.Entry {
		ref := patientRefOf(e.Resource)
		if ref == "" {
			if g.guard(ctx, KindContent, RulePatientMixed, body) {
				return "", http.StatusForbidden, "draft order missing patient subject"
			}
			return pci, 0, ""
		}
		refs = append(refs, ref)
	}
	// A prefetch value that is a Bundle has no top-level patient reference, so
	// it is not read here: every prefetch value, Bundle or not, is fenced
	// entry by entry by ingressEnsureSelfContainedContext before it is sent.
	for _, res := range req.Prefetch {
		// A bare Patient resource's identity is its `id`, NOT a subject/beneficiary/patient
		// reference, so patientRefOf can't see it. The prefetch Patient is therefore a
		// subject to fence explicitly. Resolve its id and require it bind to the same pci,
		// else a kept `prefetch.patient:{id:B}` for a different person rides into A's
		// sealed exchange.
		if id := patientResourceID(res); id != "" {
			refs = append(refs, "Patient/"+strings.TrimPrefix(id, "Patient/"))
			continue
		}
		if ref := patientRefOf(res); ref != "" {
			refs = append(refs, ref)
		}
	}
	base := strings.TrimRight(req.FHIRServer, "/")
	for _, ref := range refs {
		// An absolute reference on the EHR's own server names the EHR's
		// patient, as a relative one does (the prefetch fence reads it the
		// same way).
		if base != "" && strings.Contains(base, "://") {
			if rest, ok := strings.CutPrefix(ref, base+"/"); ok && strings.HasPrefix(rest, "Patient/") {
				ref = rest
			}
		}
		m := strings.TrimPrefix(ref, "Patient/")
		rp, ok, readErr := g.resolveSubjectPCI(ctx, m, body)
		if readErr != nil {
			// The consistency check could not finish: strict keeps the system of
			// record's failure; below strict the request is carried.
			if g.guardUnavailable(ctx, KindContent, RulePatientMixed, body) {
				status, msg := SoRFailureResponse(readErr)
				return "", status, msg
			}
			return pci, 0, ""
		}
		if !ok || rp != pci {
			if g.guard(ctx, KindContent, RulePatientMixed, body) {
				return "", http.StatusForbidden, "inconsistent patient reference in ingress payload"
			}
			return pci, 0, ""
		}
	}
	return pci, 0, ""
}

// patientResourceID returns the `id` of a prefetch resource iff it is a Patient resource (whose
// identity is its id, not a reference). "" for any other resource type.
func patientResourceID(resource json.RawMessage) string {
	var r struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	_ = decodeMessage(resource, &r)
	if r.ResourceType == "Patient" {
		return r.ID
	}
	return ""
}

// crdIngressRequest is a CDS Hooks request ready to leave the provider: the
// EHR's bytes, relayed exactly or with the registered edits, and the prefetch
// values it carries.
type crdIngressRequest struct {
	// request is the EHR's request, exact or with the registered edits:
	// the callback removed and, under enrichment, absent prefetch values
	// added.
	request relay.Payload
	// values holds the prefetch values the request is routed by, by key: every
	// value it carries, and each value obtained from the system of record. An
	// obtained value is also carried onward only under enrichment
	// (Config.EnrichNativeRequests); without it the only obtained value is the
	// coverage, read to route by. Under enrichment the obtained coverage held
	// here is the one routed by (routingCoverageChoice's, or the routing read
	// when the template's search found none or the system could not answer
	// it), not always the value carried. A null value is the JSON literal
	// null.
	values map[string][]byte
	// coverageFromSoR is true when the coverage value was read from the
	// system of record rather than sent by the EHR.
	coverageFromSoR bool
	// fhirServer is set when the coverage was read through the request's own
	// fhirServer: a payor Organization that read does not resolve is read
	// there once more, within the same time budget.
	fhirServer *fhirServerRouting
	// sentPayor is where a payor Organization that a coverage the EHR sent
	// names by reference alone, and that the request does not resolve, is
	// read, only to route (sentPayorResolver).
	sentPayor sentPayorRead
	// coverageStatus and coverageMsg are set when the coverage value was
	// left out because the system of record could not provide it.
	coverageStatus int
	coverageMsg    string
	// carried records the finding for each prefetch value left out below
	// strict (RulePrefetchFill), once the request is routed: a request
	// refused before it is carried records none.
	carried carriedFindings
}

// carriedFindings are the findings for what an ingress request is carried
// without below strict. They are recorded only once the request is routed.
type carriedFindings []func(ctx context.Context)

// record records each finding under ctx, the routed request's context, so the
// finding carries the leg's correlation id. Call once, when the request is
// routed.
func (c carriedFindings) record(ctx context.Context) {
	for _, f := range c {
		f(ctx)
	}
}

// PrefetchObtainedEvent is the observer event kind for a prefetch value the
// gateway tried to obtain from the participant's system of record.
const PrefetchObtainedEvent = "prefetch.obtained"

// prefetchObtained is the metadata a PrefetchObtainedEvent carries: never a
// value, a resource or a response. Query is the read or search exactly as it
// is sent to the system of record, query values URL-encoded
// ("ServiceRequest?patient=Patient%2Fp1&status=active,completed"); it
// narrows as the advertised prefetch template does, and keeps the include the
// template leaves out (searchIncludes).
type prefetchObtained struct {
	Key string `json:"key"`
	// Operation names the DTR operation the value was obtained for
	// (questionnaire-package); "" for a CDS Hooks prefetch value.
	Operation   string        `json:"operation,omitempty"`
	Source      string        `json:"source"`
	Query       string        `json:"query"`
	Outcome     SearchOutcome `json:"outcome"`
	Reason      string        `json:"reason,omitempty"`
	Count       int           `json:"count"`
	Pages       int           `json:"pages"`
	RetrievedAt time.Time     `json:"retrievedAt"`
	// sentPayor marks the read of the payor Organization of a coverage the
	// EHR sent (recordFHIRServerRead), logged as that read; never recorded.
	sentPayor bool
}

// ingressEnsureSelfContainedContext prepares the EHR's CDS Hooks request for
// the network without re-encoding it.
//
//   - fhirServer and fhirAuthorization are removed (the callback-strip
//     edit): the payer never receives a route or a credential into the
//     provider's system. The gateway calls fhirServer (unless its
//     participant turns the read off, Config.FHIRServerRead) in two cases
//     only, both only to route, and never carries the answers: when the
//     request carries no coverage and the system of record names no patient
//     for the member (a Coverage search and at most one payor Organization
//     read), and when a coverage the request carries names its payor
//     Organization by reference alone and nothing the request carries
//     resolves it, for any member, unless fhirServer is the system of record's own FHIR base and that system
//     names the patient by context.patientId and holds the Organization (one
//     Organization read; sentPayorResolver).
//   - Every prefetch member the request carries (a resource, a Bundle or
//     null), advertised or not, is kept exactly, after the patient fence.
//     All of them are fenced before anything is obtained.
//   - Only when the participant opts in (Config.EnrichNativeRequests), each
//     advertised key the request leaves out is obtained from the
//     participant's own system of record and inserted (the prefetch-obtain
//     edit): the patient is read; the others are searched. A search with no
//     match inserts null. A search the system cannot answer leaves the key
//     out. The inserted coverage is the template's search (status=active);
//     the request is routed on its routing choice or, when it found none or
//     the system could not answer it, on the routing read below (every
//     Coverage), so a member with no active coverage is routed as without
//     the opt-in and carries null, and one whose system cannot answer the
//     template's search is routed as without the opt-in and carries no
//     coverage. The request is refused (coverageStatus) only when the
//     routing read fails too.
//     When the system of record's Patient id is not context.patientId,
//     nothing is obtained: an absent patient or coverage refuses the request
//     with 422 before any read, and an absent history key is left out (the
//     reason is recorded).
//   - Without the opt-in (the default) nothing is inserted. A request that
//     leaves out its coverage still has one searched for, only to route by,
//     under the system's own id for the patient (obtainRoutingCoverage: every
//     Coverage, routed on the active ones, else on the others); it is refused
//     as above when none can be found. No other key is read.
//
// Every value, kept or obtained, must be about the bound patient: a kept
// value that is not is refused with 403 (at strict; RulePatientMixed), an
// obtained one with 502 at every level (the fill fence).
//
// Obtaining the coverage is routing: the request is routed by it, so a
// coverage the request leaves out and this gateway cannot obtain refuses the
// request at every level, with the status strict has always given. Any other
// value that cannot be obtained under enrichment (RulePrefetchFill) refuses
// at strict; below strict that key alone is left out and every other key is
// still obtained.
func (g *Gateway) ingressEnsureSelfContainedContext(ctx context.Context, leg string, raw []byte, member string) (crdIngressRequest, int, string) {
	var out crdIngressRequest
	body := relay.NewBody(raw, relay.OriginIngressRequest)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return out, http.StatusBadRequest, "parse cds request failed"
	}
	root := doc.Root()

	var strip []relay.Op
	var bases []string
	// The EHR's server and its token, kept only for the Coverage read that
	// routes a request carrying none (fhirserver_read.go); both are removed
	// from what is carried.
	var fhirServer fhirServerRead
	fhirServerNamed := false
	for _, key := range []string{"fhirServer", "fhirAuthorization"} {
		n, ok := doc.Member(root, key)
		if !ok {
			continue
		}
		if key == "fhirServer" {
			// A null or empty fhirServer names no server, like a null
			// fhirAuthorization: the request is one that names none.
			fhirServerNamed = doc.Kind(n) != relay.KindNull
			if doc.Kind(n) == relay.KindString {
				// The EHR's own server: absolute references on it are the
				// EHR's records, resolved like relative ones.
				if s, err := doc.StringValue(n); err == nil {
					fhirServerNamed = s != ""
					if s != "" {
						bases = append(bases, s)
						fhirServer.base = s
					}
				}
			}
		}
		if key == "fhirAuthorization" && doc.Kind(n) != relay.KindNull {
			// A null fhirAuthorization is no authorization: none is sent.
			fhirServer.auth.present = true
			if doc.Kind(n) == relay.KindObject {
				for field, dst := range map[string]*string{"access_token": &fhirServer.auth.token, "token_type": &fhirServer.auth.tokenType} {
					if v, ok := doc.Member(n, field); ok && doc.Kind(v) == relay.KindString {
						*dst, _ = doc.StringValue(v)
					}
				}
			}
		}
		strip = append(strip, doc.RemoveMember(root, key))
	}
	prefetch, hasPrefetch := doc.Member(root, "prefetch")
	if hasPrefetch && doc.Kind(prefetch) != relay.KindObject {
		// No object to carry a coverage in or to add one to, and the coverage
		// is what the request is routed by (crdIngressRecipient): refused at
		// every level.
		return out, http.StatusBadRequest, "prefetch is not an object"
	}
	carriesCoverage := false
	if hasPrefetch {
		_, carriesCoverage = doc.Member(prefetch, "coverage")
	}
	// enrich is the participant's opt-in to the prefetch fill (E-02,
	// Config.EnrichNativeRequests). Without it nothing is inserted: the only
	// value obtained from the system of record is the coverage the request is
	// routed by, when the request does not carry one, and it is not added to
	// the request.
	enrich := g.cfg.EnrichNativeRequests
	// unfilled decides a prefetch value other than the coverage that this
	// gateway could not obtain from the participant's system of record
	// (RulePrefetchFill), and reports whether the request is refused (strict,
	// with the refusal strict has always given). Below strict only that key is
	// left out; every other key, the coverage included, is still obtained, and
	// the finding is recorded once the request is routed (carriedFindings), so
	// a request refused afterwards records none. Obtaining the coverage is
	// routing, not a fill: its failure refuses at every level.
	unfilled := func(unavailable bool) bool {
		v := VerdictInvalid
		if unavailable {
			v = VerdictUnavailable
		}
		pol := g.policy()
		if !pol.Runs(KindContent, RulePrefetchFill) {
			return false
		}
		if pol.Decide(KindContent, RulePrefetchFill, v) == Refuse {
			return guardDefect(ctx, pol, g.emitFinding, KindContent, RulePrefetchFill, v, raw)
		}
		out.carried = append(out.carried, func(routed context.Context) {
			guardDefect(routed, pol, g.emitFinding, KindContent, RulePrefetchFill, v, raw)
		})
		return false
	}

	sor := ReadSystemOfRecord(g.cfg.SoR)
	sorID := ""
	// sorNamed is whether the system of record answered for the patient;
	// without its answer nothing can be obtained.
	sorNamed := true
	ref, found, err := sor.PatientFHIRRefContext(ctx, member)
	if err == nil && found {
		id, ok := strings.CutPrefix(ref, "Patient/")
		if !ok || !fhirIDRE.MatchString(id) {
			err = &SoRReadError{Kind: SoRInvalidResponse}
		} else {
			sorID = id
		}
	}
	if err != nil {
		// A request carrying no coverage needs one obtained to be routed.
		// Without enrichment nothing else is obtained, so a request that
		// carries its coverage has nothing left unfilled.
		if !carriesCoverage || (enrich && unfilled(true)) {
			status, msg := SoRFailureResponse(err)
			return out, status, msg
		}
		sorNamed = false
	}
	fence := newPatientFence(shnsdk.MemberSystem, member, bases, sorID, member).forPrefetch()
	// memberHeld reports, once, whether the system of record holds the member:
	// only asked when it cannot name the patient (sorID == "").
	var heldKnown, heldValue bool
	memberHeld := func() (bool, error) {
		if !heldKnown {
			_, _, found, err := sor.ResolvePatientContext(ctx, member)
			if err != nil {
				return false, err
			}
			heldKnown, heldValue = true, found
		}
		return heldValue, nil
	}
	// Where a payor the EHR's own coverage names by reference alone is read
	// to route. "Organization/<id>" is an id on the EHR's server: the request's
	// fhirServer, or, when it names none, the participant's own system of
	// record. So the system of record is read only when it names the patient
	// by context.patientId (its records are the request's) and the request
	// names no fhirServer or one at the system's own FHIR base
	// (sameFHIRBase); a fhirServer at another base is the only place that id
	// is read, through fhirServer when this gateway reads it.
	sorBase := !fhirServerNamed || sameFHIRBase(fhirServer.base, sorFHIRBase(g.cfg.SoR))
	out.sentPayor = sentPayorRead{leg: leg, base: fhirServer.base, sor: sorID != "" && sorID == member && sorBase}
	if g.readsFHIRServer() && fhirServerNamed {
		read := fhirServer
		read.patient = member
		out.sentPayor.fhirServer = &read
	}

	// Every member the request carries under prefetch — advertised or not,
	// whatever its name — goes to the payer, so every one is fenced, and all
	// of them before anything is obtained: a refused request searches nothing.
	// The fence over values the request itself carries is the payload's own
	// consistency (RulePatientMixed): not checked at none, recorded at observe,
	// refused at strict.
	out.values = map[string][]byte{}
	if hasPrefetch {
		checkKept := g.policy().Runs(KindContent, RulePatientMixed)
		for _, m := range doc.Members(prefetch) {
			start, end := doc.Span(m.Value)
			value := raw[start:end]
			if checkKept {
				if err := fence.check(value); err != nil && g.guard(ctx, KindContent, RulePatientMixed, raw) {
					return out, http.StatusForbidden, "prefetch " + m.Name + " refused: " + err.Error()
				}
			}
			out.values[m.Name] = value
		}
	}

	type insert struct {
		key   string
		value []byte
	}
	var inserts []insert
	for _, key := range pinnedPrefetchKeys {
		if hasPrefetch {
			if _, ok := doc.Member(prefetch, key); ok {
				continue
			}
		}
		if !enrich && key != "coverage" {
			// Not filled without the participant's opt-in: the request is
			// carried without the key, and nothing is read for it.
			continue
		}
		if !sorNamed {
			// Left out: the system of record could not answer for the patient
			// (recorded once, above). The request carries its own coverage.
			continue
		}
		if sorID == "" {
			// A member the system of record does not hold — bound by member id
			// and the Patient the request carries — has nothing to read: the
			// patient and the coverage must come with the request, and a history
			// key is left out with the reason recorded, as when the system names
			// the patient differently. A member the system holds but cannot name
			// (or any member under Config.RequireKnownMembers) is an inconsistent
			// system of record, refused.
			inconsistent := g.cfg.RequireKnownMembers
			if !inconsistent && key != prefetchPatientKey && key != "coverage" {
				held, readErr := memberHeld()
				if readErr != nil {
					status, msg := SoRFailureResponse(readErr)
					return out, status, msg
				}
				inconsistent = held
			}
			if key == "coverage" {
				// Nothing to route by in the request or the system of record:
				// read it through the request's own fhirServer when this
				// gateway does (only to route; nothing is added), or refuse.
				switch {
				case !g.readsFHIRServer():
					return out, fhirServerRefused, crdNoCoverageReadOff
				case !fhirServerNamed:
					return out, fhirServerRefused, crdNoCoverageToRouteBy
				}
				fhirServer.patient = member
				routing := &fhirServerRouting{leg: leg, read: fhirServer, deadline: time.Now().Add(g.fhirServerBudget())}
				value, status, msg := g.coverageFromFHIRServer(ctx, routing, fence)
				if status != 0 {
					return out, status, msg
				}
				out.values[key] = value
				out.fhirServer = routing
				continue
			}
			if key == prefetchPatientKey || inconsistent {
				if unfilled(false) {
					return out, http.StatusUnprocessableEntity, "patient not found in system of record"
				}
				continue
			}
			query := prefetchSearchQuery(key, member)
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchNotRun, Reason: historyMemberNotHeld})
			continue
		}
		if sorID != member && enrich {
			// Values from the system of record would name the patient by an id
			// the request does not use. The payer ties the patient and the
			// coverage to the request's patient, so without them the request is
			// refused before anything is read; a history value is left out.
			// Without enrichment the coverage is only read to route by, never
			// inserted, so the system's own id for the patient serves.
			if key == prefetchPatientKey || key == "coverage" {
				if key == "coverage" || unfilled(false) {
					return out, http.StatusUnprocessableEntity, patientNamedDifferently
				}
				continue
			}
			query := prefetchSearchQuery(key, sorID)
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchNotRun, Reason: historyNamedDifferently})
			continue
		}
		obtainFence := fence
		if !enrich && sorID != member {
			// Read only to route by, under the system's own id: in that system
			// Patient/<member> is another patient (or none), so the coverage is
			// fenced to the system's id alone.
			obtainFence = newPatientFence(shnsdk.MemberSystem, member, bases, sorID).forPrefetch()
		}
		var value []byte
		var outcome SearchOutcome
		var status int
		var msg string
		if !enrich {
			// The coverage read only to route by (the one key read without
			// the opt-in): every Coverage, routed on routingCoverageChoice's.
			value, outcome, status, msg = g.obtainRoutingCoverage(ctx, leg, sorID, obtainFence)
		} else {
			value, outcome, status, msg = g.obtainPrefetch(ctx, leg, key, sorID, obtainFence)
		}
		if status != 0 {
			// The fill fence refuses at every level: SHN never inserts
			// another patient's record. So does failing to obtain the
			// coverage, which the request is routed by. Any other failure to
			// fill refuses at strict; below strict that key is left out.
			if msg == fillFencedBinary || msg == fillFencedOtherPatient || key == "coverage" || unfilled(true) {
				return out, status, msg
			}
			continue
		}
		if value == nil && enrich && key == "coverage" {
			// The system could not answer the template's search: the key is
			// left out, as a history the system cannot search is, and the
			// request is routed by the routing read, as without the opt-in.
			value, outcome, status, msg = g.obtainRoutingCoverage(ctx, leg, sorID, obtainFence)
			if status != 0 {
				return out, status, msg
			}
			if value != nil {
				out.values[key] = value
				out.coverageFromSoR = true
				continue
			}
		}
		if value == nil {
			if key == "coverage" {
				out.coverageStatus, out.coverageMsg = coverageOmitted(outcome)
			}
			continue
		}
		if enrich {
			inserts = append(inserts, insert{key, value})
		}
		if enrich && key == "coverage" {
			// The coverage carried under the opt-in is the template's search
			// (status=active), as the system answered it. When that search
			// found a Coverage the request is routed on its routing choice
			// (routingCoverageChoice), which picks the payer the routing read
			// would; when it found none (a member with no active coverage), on
			// the routing read (obtainRoutingCoverage), and the value carried
			// stays null. The opt-in never leaves a member with less to route
			// by than the default, except when the system of record names the
			// patient by another id (refused above, patientNamedDifferently).
			if string(value) == "null" {
				value, outcome, status, msg = g.obtainRoutingCoverage(ctx, leg, sorID, obtainFence)
				if status != 0 {
					return out, status, msg
				}
				if value == nil {
					out.coverageStatus, out.coverageMsg = coverageOmitted(outcome)
					continue
				}
			} else {
				value = routingCoverages(value)
			}
		}
		out.values[key] = value
		if key == "coverage" {
			out.coverageFromSoR = true
		}
	}
	var changes []relay.Change
	if len(strip) > 0 {
		changes = append(changes, relay.Change{Edit: relay.EditCDSCallbackStrip, Ops: strip})
	}
	if len(inserts) > 0 {
		var ops []relay.Op
		target := prefetch
		if !hasPrefetch {
			op, h := doc.EnsureObjectMember(root, "prefetch")
			ops = append(ops, op)
			target = relay.NodeID(h)
		}
		for _, in := range inserts {
			ops = append(ops, doc.InsertMember(target, in.key, in.value))
		}
		changes = append(changes, relay.Change{Edit: relay.EditCDSPrefetchObtain, Ops: ops})
	}
	if len(changes) == 0 {
		out.request = relay.Exact(body, "application/json")
		return out, 0, ""
	}
	out.request, err = relay.ApplyChanges(body, "application/json", changes...)
	var signed *relay.SignedContentError
	switch {
	case errors.As(err, &signed):
		return out, http.StatusUnprocessableEntity, signed.Error()
	case err != nil:
		return out, http.StatusInternalServerError, "prepare cds request failed"
	}
	return out, 0, ""
}

// patientNamedDifferently refuses a request whose absent patient or coverage
// would have to come from a system of record that names the patient by an id
// other than context.patientId.
const patientNamedDifferently = "system of record names the patient differently from context.patientId; supply patient and coverage prefetch in the request"

// crdNoCoverageToRouteBy refuses a request that carries no coverage and names
// no fhirServer, for a member the system of record names no patient for (one
// it does not hold, or holds but cannot name): the request is routed by its
// coverage, and this gateway has none to read. It names what the participant
// sends instead. crdNoCoverageReadOff is the same refusal at a gateway whose
// participant turned the fhirServer read off. Both are CDS Hooks' 412: the
// service could not obtain the data the request left out.
const (
	crdNoCoverageToRouteBy = "no coverage to route by: send prefetch.coverage or fhirServer (this gateway's system of record names no patient for this member)"
	crdNoCoverageReadOff   = "no coverage to route by: send prefetch.coverage (this gateway's system of record names no patient for this member, and it does not read fhirServer)"
)

// historyNamedDifferently is the recorded reason a history key is left out
// for the same cause.
const historyNamedDifferently = "patient named differently in the system of record"

// historyMemberNotHeld is the recorded reason a history key is left out for a
// member the system of record does not hold (bound by the member id and the Patient the
// request carries).
const historyMemberNotHeld = "member not held by the system of record"

// coverageOmitted is the refusal for a request whose coverage the system of
// record could not provide.
func coverageOmitted(o SearchOutcome) (int, string) {
	switch o {
	case SearchUnavailable:
		return http.StatusServiceUnavailable, "coverage unavailable from system of record"
	case SearchUnsupported:
		return http.StatusUnprocessableEntity, "coverage not in request, and the system of record cannot search for it"
	}
	return http.StatusUnprocessableEntity, "coverage unreadable from system of record"
}

// The fill fence's refusals: this gateway never inserts, into a request it
// carries, a record it read that is not the bound patient's own. That is SHN's
// own edit to the message, so it refuses at every level.
const (
	fillFencedBinary       = "system of record returned a Binary resource"
	fillFencedOtherPatient = "system of record returned another patient's resource"
)

// obtainPrefetch reads the value of an advertised prefetch key from the
// system of record for the patient sorID, fences it, and records the
// attempt (PrefetchObtainedEvent and a log line). It returns the value to
// insert (the JSON literal null for a search with no match), or nil with the
// search outcome when the key is left out; a non-zero status refuses the
// request.
func (g *Gateway) obtainPrefetch(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	refused := func(err error) ([]byte, SearchOutcome, int, string) {
		status, msg := fillFenceRefusal(err)
		return nil, "", status, msg
	}
	if key == prefetchPatientKey {
		query := "Patient/" + sorID
		patient, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, query)
		if err != nil {
			status, msg := SoRFailureResponse(err)
			outcome := SearchMalformed
			if status == http.StatusServiceUnavailable {
				outcome = SearchUnavailable
			}
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: outcome, Reason: string(safeSoRError(err).Kind)})
			return nil, "", status, msg
		}
		if !found {
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchZero})
			return nil, "", http.StatusUnprocessableEntity, "patient not found in system of record"
		}
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(patient, &head) != nil || head.ResourceType != "Patient" {
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchMalformed, Reason: "not a Patient"})
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, "", status, msg
		}
		g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchOK, Count: 1})
		if err := fence.check(patient); err != nil {
			return refused(err)
		}
		return patient, SearchOK, 0, ""
	}
	resourceType, ok := prefetchSearchTypes[key]
	if !ok {
		return nil, SearchUnsupported, 0, ""
	}
	s := searchSystemOfRecord(ctx, g.cfg.SoR, resourceType, sorID)
	g.recordPrefetch(leg, prefetchObtained{Key: key, Query: s.Query, Outcome: s.Outcome, Reason: s.Reason, Count: s.Count, Pages: s.Pages})
	switch s.Outcome {
	case SearchOK:
		if err := fence.check(s.Value); err != nil {
			return refused(err)
		}
		return s.Value, SearchOK, 0, ""
	case SearchZero:
		return []byte("null"), SearchZero, 0, ""
	}
	return nil, s.Outcome, 0, ""
}

// fillFenceRefusal is the refusal for a record the fill fence refused.
func fillFenceRefusal(err error) (int, string) {
	var ce *CompartmentError
	if errors.As(err, &ce) && ce.Reason == opaqueContentReason {
		return http.StatusBadGateway, fillFencedBinary
	}
	return http.StatusBadGateway, fillFencedOtherPatient
}

// recordPrefetch reports one prefetch attempt on the observer seam and in
// the holder log. Neither carries a value, a resource or an identifier
// beyond the search the gateway ran.
func (g *Gateway) recordPrefetch(leg string, p prefetchObtained) {
	if p.Source == "" {
		p.Source = sourceSystemOfRecord
	}
	clock := g.cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	p.RetrievedAt = clock().UTC()
	from := "the system of record"
	if p.Source == sourceFHIRServer {
		from = "fhirServer"
	}
	switch {
	case p.sentPayor && (p.Outcome == SearchOK || p.Outcome == SearchZero):
		log.Printf("gateway: %s, read through %s only to route: %s", sentPayorReason, from, p.Outcome)
	case p.sentPayor:
		log.Printf("gateway: %s, not read through %s: %s (%s)", sentPayorReason, from, p.Outcome, strings.TrimPrefix(p.Reason, sentPayorReason+": "))
	case p.Outcome == SearchNotRun && p.Source != sourceFHIRServer:
		log.Printf("gateway: prefetch %s left out: %s", p.Key, p.Reason)
	case p.Outcome == SearchOK || p.Outcome == SearchZero:
		log.Printf("gateway: prefetch %s from %s: %s, %d matches in %d pages", p.Key, from, p.Outcome, p.Count, p.Pages)
	default:
		log.Printf("gateway: prefetch %s not obtained from %s: %s (%s)", p.Key, from, p.Outcome, p.Reason)
	}
	if g.cfg.Observer == nil {
		return
	}
	detail, err := json.Marshal(p)
	if err != nil {
		return
	}
	g.observe(ObserverEvent{Kind: PrefetchObtainedEvent, LegType: leg, Direction: "sor", Op: p.Key, Detail: string(detail)})
}

// The sources a prefetch.obtained event names.
const (
	sourceSystemOfRecord = "system-of-record"
	sourceFHIRServer     = "fhirServer"
)

// readsFHIRServer reports whether this gateway reads a Coverage through a
// request's own fhirServer.
func (g *Gateway) readsFHIRServer() bool {
	return g.cfg.FHIRServerRead != FHIRServerReadOff
}

// fhirServerRouting is a coverage read through the request's own fhirServer:
// the read, the leg it serves and the deadline every read for it shares.
// sentPayor marks the read of the payor Organization of a coverage the EHR
// itself sent (sentPayorResolver), recorded and logged as that.
type fhirServerRouting struct {
	leg       string
	read      fhirServerRead
	deadline  time.Time
	sentPayor bool
}

// fhirServerBudget is the time every read through fhirServer for one request
// shares.
func (g *Gateway) fhirServerBudget() time.Duration {
	if g.cfg.fhirServerBudget > 0 {
		return g.cfg.fhirServerBudget
	}
	return fhirServerReadTimeout
}

func (g *Gateway) fhirServerReader() fhirServerReader {
	return fhirServerReader{mode: g.cfg.FHIRServerRead, resolve: g.cfg.fhirServerResolve, roots: g.cfg.fhirServerRoots,
		dial: g.cfg.fhirServerDial, dialed: g.cfg.fhirServerDialed}
}

// coverageFromFHIRServer searches the bound patient's Coverage through
// the request's own fhirServer (fhirserver_read.go), fences it to the patient
// and records the read (a prefetch.obtained event with source fhirServer, and
// a log line naming only the host). It returns the searchset to route by, or
// a refusal: nothing it returns is added to the request.
func (g *Gateway) coverageFromFHIRServer(ctx context.Context, routing *fhirServerRouting, fence patientFence) ([]byte, int, string) {
	ctx, cancel := context.WithDeadline(ctx, routing.deadline)
	defer cancel()
	log.Printf("gateway: coverage to route by: reading it through fhirServer %s (only to route; nothing is added)", fhirServerHost(routing.read.base))
	value, count, query, status, msg := g.fhirServerReader().read(ctx, routing.read)
	switch {
	case status != 0:
	case count == 0:
		status, msg = fhirServerRefused, fhirServerNoCoverage
	default:
		// Routing on another patient's coverage would send the request to
		// that patient's payer: refused at every level.
		// Only a Coverage that names another patient is another patient's;
		// any other answer the check refuses (a patient it cannot identify
		// among them) is one the read cannot use.
		if err := fence.check(value); err != nil {
			var ce *CompartmentError
			if errors.As(err, &ce) && ce.ResourceType == "Coverage" && ce.Reason == fenceAnotherPatientReference {
				status, msg = http.StatusBadGateway, fhirServerOtherPatient
			} else {
				status, msg = fhirServerRefused, fhirServerNotSearchset
			}
		}
	}
	g.recordFHIRServerRead(routing, query, status, msg, count)
	if status != 0 {
		return nil, status, msg
	}
	return routingCoverages(value), 0, ""
}

// fhirServerPayor returns the resolver of a payor Organization the coverage
// read did not resolve: one read of it on the request's fhirServer, within the
// coverage read's deadline, recorded like it. A second Organization, or a
// refused read, resolves nothing and leaves the refusal in *status and *msg.
func (g *Gateway) fhirServerPayor(ctx context.Context, routing *fhirServerRouting) (resolve func(string) ([]byte, bool), status *int, msg *string) {
	status, msg = new(int), new(string)
	var readID string
	var org []byte
	resolve = func(ref string) ([]byte, bool) {
		id, ok := payorOrganizationID(ref, routing.read.base)
		switch {
		case !ok || *status != 0:
			return nil, false
		case readID == id:
			return org, true
		case readID != "":
			*status, *msg = http.StatusUnprocessableEntity, fhirServerTwoPayors
			return nil, false
		}
		readID = id
		ctx, cancel := context.WithDeadline(ctx, routing.deadline)
		defer cancel()
		value, query, st, m := g.fhirServerReader().readOrganization(ctx, routing.read, id)
		count := 0
		if st == 0 {
			count = 1
		}
		g.recordFHIRServerRead(routing, query, st, m, count)
		if st != 0 {
			*status, *msg = st, m
			return nil, false
		}
		org = value
		return org, true
	}
	return resolve, status, msg
}

// sentPayorRead is where a CDS Hooks request's payor reference is read when
// the coverage is the EHR's own: leg is the leg the read serves, base the
// request's fhirServer (an absolute reference on it is the EHR's own record),
// sor whether the system of record is read (it names the patient by
// context.patientId, and the request names no fhirServer or one at the
// system's own FHIR base), and fhirServer the read through the request's own
// fhirServer, nil when this gateway does not read it
// (CDS_FHIR_SERVER_READ=off) or the request names none.
type sentPayorRead struct {
	leg        string
	base       string
	sor        bool
	fhirServer *fhirServerRead
}

// The refusals of a coverage the EHR sent whose payor Organization the
// request names by reference alone and this gateway could not read. Each is
// the refusal of any coverage without a payer (noPayerIdentifier) followed by
// ": " and the reason, as a PAS Bundle's is (pasRecipient). Only a payor that
// nothing could resolve (not the request, the system of record or the
// request's fhirServer) adds, after the reason, a remedy the EHR applies, and
// only one that resolves that reference (unresolvedPayor):
//   - noPayerSendPayor, for "Organization/<id>": the payor Organization sent
//     with the coverage resolves it by its type and id (entryAnswers);
//   - noPayerUnreadRef, for an absolute reference that a valid Bundle fullUrl
//     can equal (absoluteOrganizationRef): the Organization as an entry of a
//     Bundle the request carries, with that fullUrl, resolves it;
//   - noPayerIdentifierOnlyRef, for any other reference to an Organization
//     (versioned, with a fragment, a leading slash or a dot segment): no
//     valid resource the EHR sends with it resolves it, so only a payor
//     identifier.
const (
	noPayerSendPayor         = noPayerIdentifier + ": Coverage.payor is a reference to an Organization the gateway could not read; send the payor Organization with the coverage, or a payor identifier"
	noPayerUnreadRef         = noPayerIdentifier + ": Coverage.payor is a reference to an Organization the gateway could not resolve; send the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor identifier"
	noPayerIdentifierOnlyRef = noPayerIdentifier + ": Coverage.payor is a reference to an Organization the gateway does not read; send a payor identifier with the coverage"
	noPayerSentTwoRefs       = noPayerIdentifier + ": the request's coverages name more than one payor Organization by reference alone"
)

// unresolvedPayor classifies the last payor reference a sent-payor resolver
// (sentPayorResolver, keptPayorResolver) could not resolve, by the remedy
// that would resolve it.
type unresolvedPayor int

const (
	// payorNoRemedy: no reference went unresolved, or the one that did is
	// not a reference to an Organization (a RelatedPerson, a Patient, a
	// urn:uuid): the refusal stays the bare noPayerIdentifier.
	payorNoRemedy unresolvedPayor = iota
	// payorRelativeOrganization: "Organization/<id>" (payorOrganizationID
	// with no base), which the payor Organization sent with the coverage
	// resolves (entryAnswers matches its type and id).
	payorRelativeOrganization
	// payorFullURLOrganization: an absolute reference a valid Bundle fullUrl
	// can equal (absoluteOrganizationRef), which an entry with that fullUrl of
	// a Bundle the request carries resolves (carriedRefs, entryAnswers).
	payorFullURLOrganization
	// payorOtherOrganization: any other reference to an Organization
	// (namesOrganization). Nothing sent with the coverage resolves it.
	payorOtherOrganization
)

// classifyUnresolvedPayor is the class of a payor reference nothing resolved.
func classifyUnresolvedPayor(ref string) unresolvedPayor {
	if _, ok := payorOrganizationID(ref, ""); ok {
		return payorRelativeOrganization
	}
	switch {
	case absoluteOrganizationRef(ref):
		return payorFullURLOrganization
	case namesOrganization(ref):
		return payorOtherOrganization
	}
	return payorNoRemedy
}

// refusal is the answer to a sent coverage refused msg: the remedy for an
// Organization reference nothing resolved, when msg is the bare refusal of a
// coverage without a payer; otherwise msg itself (a resolved Organization
// with no payer identifier keeps the bare text).
func (u unresolvedPayor) refusal(msg string) string {
	if msg != noPayerIdentifier {
		return msg
	}
	switch u {
	case payorRelativeOrganization:
		return noPayerSendPayor
	case payorFullURLOrganization:
		return noPayerUnreadRef
	case payorOtherOrganization:
		return noPayerIdentifierOnlyRef
	}
	return msg
}

// fhirServerReadPrefix begins every refusal of the fhirServer read (the
// fhirServer* reasons): the request had no coverage to route by.
const fhirServerReadPrefix = "no coverage to route by: "

// sentPayorRefusal is a refused fhirServer read of the payor Organization of a
// coverage the EHR sent, as the request is answered: the request carried a
// coverage, so the read's reason follows the prefix of any coverage without a
// payer instead ("no payer identifier on member coverage: fhirServer holds no
// Organization for the coverage's payor"), and two Organizations by reference
// alone are that refusal's own reason (noPayerSentTwoRefs). The status is the
// read's.
func sentPayorRefusal(msg string) string {
	if msg == fhirServerTwoPayors {
		return noPayerSentTwoRefs
	}
	return noPayerIdentifier + ": " + strings.TrimPrefix(msg, fhirServerReadPrefix)
}

// sentPayorReason is the recorded reason (prefetch.obtained, key coverage) of
// a read of the payor Organization of a coverage the EHR sent: no coverage was
// obtained, only that Organization, only to route.
const sentPayorReason = "payor Organization of the coverage the EHR sent"

// sentPayorResolver resolves a payor Organization that a coverage the EHR
// sent names by reference alone, when the request's own values do not: the
// network's routing read, done only to choose the payer; nothing it reads is
// added to or changed in what is carried. "Organization/<id>" is an id on the
// EHR's server, so where it is read depends on the request's fhirServer
// (sentPayorRead.sor): with none, or one at the system of record's own FHIR
// base, from the system of record when that system names the patient by
// context.patientId; a fhirServer at another base is never answered from the
// system of record. Otherwise, or when the system of record holds no such
// Organization, it is read once on the request's own fhirServer with its own
// fhirAuthorization, within the fhirServer budget (fhirServerPayor; a second
// Organization is refused). A reference written on another server, versioned,
// with a fragment, a leading slash or a dot segment is never read
// (payorOrganizationID). *unresolved
// classifies the last reference asked, when nothing resolved it, by the
// remedy that would (classifyUnresolvedPayor: "Organization/<id>", an
// absolute reference a carried Bundle's entry resolves by fullUrl, another
// reference to an Organization, or none of these, which keeps the bare
// refusal); *status and *msg hold a refused fhirServer read
// (the read's own reason; the request is answered with sentPayorRefusal's
// text), *readErr a failed
// system-of-record read, which ends the resolution: fhirServer is not read
// after it.
func (g *Gateway) sentPayorResolver(ctx context.Context, sp sentPayorRead) (resolve func(string) ([]byte, bool), unresolved *unresolvedPayor, status *int, msg *string, readErr *error) {
	unresolved, status, msg, readErr = new(unresolvedPayor), new(int), new(string), new(error)
	var fromSoR, fromFHIRServer func(string) ([]byte, bool)
	if sp.sor {
		fromSoR, readErr = sorReferenceCallback(ctx, g.cfg.SoR)
	}
	if sp.fhirServer != nil {
		routing := &fhirServerRouting{leg: sp.leg, read: *sp.fhirServer, deadline: time.Now().Add(g.fhirServerBudget()), sentPayor: true}
		fromFHIRServer, status, msg = g.fhirServerPayor(ctx, routing)
	}
	resolve = func(ref string) ([]byte, bool) {
		if id, ok := payorOrganizationID(ref, sp.base); ok {
			if fromSoR != nil {
				if b, ok := fromSoR("Organization/" + id); ok {
					return b, true
				}
				if *readErr != nil {
					return nil, false
				}
			}
			if fromFHIRServer != nil {
				if b, ok := fromFHIRServer(ref); ok {
					return b, true
				}
				if *status != 0 {
					return nil, false
				}
			}
		}
		// The remedy is the answer only for an Organization reference nothing
		// could read, and names what resolves that reference; any other
		// unresolved payor (a RelatedPerson, a Patient) keeps the bare
		// refusal.
		*unresolved = classifyUnresolvedPayor(ref)
		return nil, false
	}
	return resolve, unresolved, status, msg, readErr
}

// namesOrganization reports whether ref is a reference whose own type is
// Organization. A relative reference (a leading slash dropped) is one when
// its first segment is "Organization" and an id follows; an absolute URL
// (scheme://authority/path) when, its fragment and any "/_history/<v>" dropped,
// its path's second-to-last segment is "Organization" and an id follows. An
// "Organization" anywhere else (the host, a base path segment) is not the
// reference's type.
func namesOrganization(ref string) bool {
	if _, path, ok := absoluteRefPath(ref); ok {
		path, _, _ = strings.Cut(path, "#")
		parts := strings.Split(path, "/")
		if n := len(parts); n >= 4 && parts[n-2] == "_history" {
			parts = parts[:n-2]
		}
		n := len(parts)
		return n >= 2 && parts[n-2] == "Organization" && parts[n-1] != ""
	}
	parts := strings.Split(strings.TrimPrefix(ref, "/"), "/")
	return len(parts) >= 2 && parts[0] == "Organization" && parts[1] != ""
}

// absoluteOrganizationRef reports whether ref is an absolute reference to an
// Organization that a Bundle entry's fullUrl may equal: a fullUrl is an
// absolute URL agreeing with its resource's id and never version-specific
// (bdl-8), so ref ends "Organization/<id>" with a valid id, and carries no
// "/_history/", query, fragment or dot segment.
func absoluteOrganizationRef(ref string) bool {
	authority, path, ok := absoluteRefPath(ref)
	if !ok || authority == "" || strings.ContainsAny(path, "?#") {
		return false
	}
	parts := strings.Split(path, "/")
	n := len(parts)
	if n < 2 || parts[n-2] != "Organization" || !fhirIDRE.MatchString(parts[n-1]) {
		return false
	}
	for _, seg := range parts {
		if seg == "." || seg == ".." || seg == "_history" {
			return false
		}
	}
	return true
}

// absoluteRefPath splits an absolute reference "scheme://authority/path" into
// its authority and its path (after the authority's slash, with any query and
// fragment); ok is false for a reference that is not one.
func absoluteRefPath(ref string) (authority, path string, ok bool) {
	scheme, rest, found := strings.Cut(ref, "://")
	if !found || scheme == "" || strings.ContainsAny(scheme, "/?#") {
		return "", "", false
	}
	authority, path, found = strings.Cut(rest, "/")
	if !found {
		return "", "", false
	}
	return authority, path, true
}

// recordFHIRServerRead records one read as the system of record's searches
// are: the matches only of an answer it uses, and a page only for an answer
// it read (unavailable and not-run read none).
//
// The read of the payor Organization of a coverage the EHR sent
// (routing.sentPayor) is recorded under the same key, coverage, the value it
// serves, with sentPayorReason as its reason (followed by the read's own
// reason when refused), and logged as that read: no coverage was obtained.
func (g *Gateway) recordFHIRServerRead(routing *fhirServerRouting, query string, status int, msg string, count int) {
	outcome := fhirServerOutcome(status, msg, count)
	pages := 1
	if outcome == SearchNotRun || outcome == SearchUnavailable {
		pages = 0
	}
	if status != 0 {
		count = 0
	}
	reason := msg
	if routing.sentPayor {
		reason = sentPayorReason
		if msg != "" {
			reason += ": " + strings.TrimPrefix(msg, fhirServerReadPrefix)
		}
	}
	g.recordPrefetch(routing.leg, prefetchObtained{Key: "coverage", Source: sourceFHIRServer, Query: query, Outcome: outcome, Reason: reason, Count: count, Pages: pages,
		sentPayor: routing.sentPayor})
}

// fhirServerUnavailable are the refusals of a read the server did not
// complete, or answered with an error status (recorded unavailable).
var fhirServerUnavailable = map[string]bool{
	fhirServerNoAddress: true, fhirServerTLSFailed: true, fhirServerUnreachable: true, fhirServerTimedOut: true,
	fhirServerAnswered: true, fhirServerRefusedToken: true,
}

// fhirServerMalformed are the refusals of an answer the read cannot use: not
// a Coverage searchset or the Organization asked for, a redirect, or another
// patient's coverage (recorded malformed).
var fhirServerMalformed = map[string]bool{
	fhirServerNotSearchset: true, fhirServerNotOrganization: true, fhirServerRedirected: true, fhirServerOtherPatient: true,
}

// fhirServerOutcome is the recorded outcome of a fhirServer read, in the
// system-of-record search's terms: a read that found nothing (no Coverage, or
// no such Organization) is zero; one the server did not complete is
// unavailable; an answer over the size bound is bound; an answer the read
// cannot use is malformed; one the request's own values or the address fence
// refused before any request is not run.
func fhirServerOutcome(status int, msg string, count int) SearchOutcome {
	switch {
	case msg == fhirServerNoCoverage || msg == fhirServerNoOrganization || (status == 0 && count == 0):
		return SearchZero
	case status == 0:
		return SearchOK
	case fhirServerUnavailable[msg]:
		return SearchUnavailable
	case msg == fhirServerTooLarge:
		return SearchBound
	case fhirServerMalformed[msg]:
		return SearchMalformed
	}
	return SearchNotRun
}
