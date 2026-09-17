// ingress_crd.go — CRD (CDS Hooks order-select) ingress: parse the conformant inbound request,
// prove every patient reference resolves to ONE pci before any leg, and prepare the EHR's own
// bytes for the network (the callback removed, absent prefetch obtained from the participant's
// system of record). On the ingress the payload is EXTERNAL (the EHR), so cross-field patient
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
func crdAnswerOutcome(respJSON []byte, line string) (string, int, string) {
	if refused := certifyCDSHooksAnswer(respJSON, line, "payer"); refused.Status != 0 {
		return "", refused.Status, refused.Message
	}
	obs, err := shnsdk.ParseCRDResponse(respJSON)
	if err != nil {
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

// ingressCRDSubjectPCI parses the request and returns the single bound pci. Every patient
// reference present (context.patientId, each draftOrders entry subject, each prefetch
// resource's subject/beneficiary/patient) MUST resolve to the SAME pci; any divergence fails
// closed (403). A reference is relative (Patient/<id>) or absolute on the request's own
// fhirServer. Returns (pci, 0, "") on success or ("", status, msg) to write.
func (g *Gateway) ingressCRDSubjectPCIContext(ctx context.Context, body []byte) (string, int, string) {
	var req ingressCDSRequest
	if err := decodeMessage(body, &req); err != nil {
		return "", http.StatusBadRequest, "parse cds request failed"
	}
	if req.Context.PatientID == "" {
		return "", http.StatusBadRequest, "missing context.patientId"
	}
	member := strings.TrimPrefix(req.Context.PatientID, "Patient/")
	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if readErr != nil {
		status, msg := SoRFailureResponse(readErr)
		return "", status, msg
	}
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	// Every other patient reference must resolve to the SAME pci.
	var refs []string
	// A draft order is the order this PA is FOR — it MUST carry a patient subject. A
	// missing/renamed subject is REJECTED here (not skipped), so an order with no recognizable
	// patient can never ride into the sealed request behind the bound subject's authority.
	for _, e := range req.Context.DraftOrders.Entry {
		ref := patientRefOf(e.Resource)
		if ref == "" {
			return "", http.StatusForbidden, "draft order missing patient subject"
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
		rp, _, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, m)
		if readErr != nil {
			status, msg := SoRFailureResponse(readErr)
			return "", status, msg
		}
		if !ok || rp != pci {
			return "", http.StatusForbidden, "inconsistent patient reference in ingress payload"
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
	// the callback removed and absent prefetch values added.
	request relay.Payload
	// values holds every prefetch value the request carries onward, kept or
	// obtained, by key; a null value is the JSON literal null.
	values map[string][]byte
	// coverageFromSoR is true when the coverage value was read from the
	// system of record rather than sent by the EHR.
	coverageFromSoR bool
	// coverageStatus and coverageMsg are set when the coverage value was
	// left out because the system of record could not provide it.
	coverageStatus int
	coverageMsg    string
}

// PrefetchObtainedEvent is the observer event kind for a prefetch value the
// gateway tried to obtain from the participant's system of record.
const PrefetchObtainedEvent = "prefetch.obtained"

// prefetchObtained is the metadata a PrefetchObtainedEvent carries: never a
// value, a resource or a response. Query is the read or search exactly as it
// is sent to the system of record, query values URL-encoded
// ("ServiceRequest?patient=Patient%2Fp1"); the advertised prefetch template
// shows the same search unencoded.
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
}

// ingressEnsureSelfContainedContext prepares the EHR's CDS Hooks request for
// the network without re-encoding it.
//
//   - fhirServer and fhirAuthorization are removed (the callback-strip
//     edit): the payer never receives a route or a credential into the
//     provider's system, and the gateway never calls fhirServer.
//   - Every prefetch member the request carries (a resource, a Bundle or
//     null), advertised or not, is kept exactly, after the patient fence.
//     All of them are fenced before anything is obtained.
//   - Each advertised key the request leaves out is obtained from the
//     participant's own system of record and inserted (the prefetch-obtain
//     edit): the patient is read; the others are searched. A search with no
//     match inserts null. A search the system cannot answer leaves the key
//     out; for coverage, the request is then refused (coverageStatus).
//     When the system of record's Patient id is not context.patientId,
//     nothing is obtained: an absent patient or coverage refuses the request
//     with 422 before any read, and an absent history key is left out (the
//     reason is recorded).
//
// Every value, kept or obtained, must be about the bound patient: a kept
// value that is not is refused with 403, an obtained one with 502.
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
	for _, key := range []string{"fhirServer", "fhirAuthorization"} {
		n, ok := doc.Member(root, key)
		if !ok {
			continue
		}
		if key == "fhirServer" && doc.Kind(n) == relay.KindString {
			// The EHR's own server: absolute references on it are the
			// EHR's records, resolved like relative ones.
			if s, err := doc.StringValue(n); err == nil {
				bases = append(bases, s)
			}
		}
		strip = append(strip, doc.RemoveMember(root, key))
	}
	prefetch, hasPrefetch := doc.Member(root, "prefetch")
	if hasPrefetch && doc.Kind(prefetch) != relay.KindObject {
		return out, http.StatusBadRequest, "prefetch is not an object"
	}

	sor := ReadSystemOfRecord(g.cfg.SoR)
	ref, found, err := sor.PatientFHIRRefContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return out, status, msg
	}
	sorID := ""
	if found {
		id, ok := strings.CutPrefix(ref, "Patient/")
		if !ok || !fhirIDRE.MatchString(id) {
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return out, status, msg
		}
		sorID = id
	}
	fence := newPatientFence(shnsdk.MemberSystem, member, bases, sorID, member).forPrefetch()

	// Every member the request carries under prefetch — advertised or not,
	// whatever its name — goes to the payer, so every one is fenced, and all
	// of them before anything is obtained: a refused request searches nothing.
	out.values = map[string][]byte{}
	if hasPrefetch {
		for _, m := range doc.Members(prefetch) {
			start, end := doc.Span(m.Value)
			value := raw[start:end]
			if err := fence.check(value); err != nil {
				return out, http.StatusForbidden, "prefetch " + m.Name + " refused: " + err.Error()
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
		if sorID == "" {
			return out, http.StatusUnprocessableEntity, "patient not found in system of record"
		}
		if sorID != member {
			// Values from the system of record would name the patient by an id
			// the request does not use. The payer ties the patient and the
			// coverage to the request's patient, so without them the request is
			// refused before anything is read; a history value is left out.
			if key == prefetchPatientKey || key == "coverage" {
				return out, http.StatusUnprocessableEntity, patientNamedDifferently
			}
			query, _ := SoRSearchQuery(prefetchSearchTypes[key], sorID)
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchNotRun, Reason: historyNamedDifferently})
			continue
		}
		value, outcome, status, msg := g.obtainPrefetch(ctx, leg, key, sorID, fence)
		if status != 0 {
			return out, status, msg
		}
		if value == nil {
			if key == "coverage" {
				out.coverageStatus, out.coverageMsg = coverageOmitted(outcome)
			}
			continue
		}
		inserts = append(inserts, insert{key, value})
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

// historyNamedDifferently is the recorded reason a history key is left out
// for the same cause.
const historyNamedDifferently = "patient named differently in the system of record"

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

// obtainPrefetch reads the value of an advertised prefetch key from the
// system of record for the patient sorID, fences it, and records the
// attempt (PrefetchObtainedEvent and a log line). It returns the value to
// insert (the JSON literal null for a search with no match), or nil with the
// search outcome when the key is left out; a non-zero status refuses the
// request.
func (g *Gateway) obtainPrefetch(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	refused := func(err error) ([]byte, SearchOutcome, int, string) {
		var ce *CompartmentError
		if errors.As(err, &ce) && ce.Reason == opaqueContentReason {
			return nil, "", http.StatusBadGateway, "system of record returned a Binary resource"
		}
		return nil, "", http.StatusBadGateway, "system of record returned another patient's resource"
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

// recordPrefetch reports one prefetch attempt on the observer seam and in
// the holder log. Neither carries a value, a resource or an identifier
// beyond the search the gateway ran.
func (g *Gateway) recordPrefetch(leg string, p prefetchObtained) {
	p.Source = "system-of-record"
	clock := g.cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	p.RetrievedAt = clock().UTC()
	switch p.Outcome {
	case SearchNotRun:
		log.Printf("gateway: prefetch %s left out: %s", p.Key, p.Reason)
	case SearchOK, SearchZero:
		log.Printf("gateway: prefetch %s from the system of record: %s, %d matches in %d pages", p.Key, p.Outcome, p.Count, p.Pages)
	default:
		log.Printf("gateway: prefetch %s not obtained from the system of record: %s (%s)", p.Key, p.Outcome, p.Reason)
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
