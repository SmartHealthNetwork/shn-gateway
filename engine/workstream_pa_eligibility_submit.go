package engine

// workstream_pa_eligibility_submit.go — the input of CoverageEligibilityRequest/$submit,
// the base R4 operation a system submits coverage eligibility requests with
// (IHE FAIS ITI-124), read as the requests it submits, and the payer those
// requests name. Reading never changes the bytes carried.
//
// The operation's input is one request, or a Bundle: one request with the
// resources it references (IHE FAIS's CoverageEligibilityRequest Bundle), or a
// batch of requests, each one alone or in such a Bundle of its own. The input
// is the request or Bundle itself, or a Parameters whose one `resource`
// parameter holds it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const resourceEligibilityRequest = "CoverageEligibilityRequest"

// maxEligibilityBatch is the most coverage eligibility requests one input
// submits: the leg's own patient and the most other patients the Hub records
// on one leg's involved list (maxInvolved). A larger batch could not be
// recorded under every patient it names, so it is refused before anything is
// read from it, at every level, as a body over the size limit is (a request
// count, which never under-counts its patients).
const maxEligibilityBatch = maxInvolved + 1

// eligibilityBatchTooLarge is the refusal of an input submitting n requests,
// more than maxEligibilityBatch.
func eligibilityBatchTooLarge(n int) string {
	return fmt.Sprintf("the batch carries %d coverage eligibility requests; a gateway reads at most %d in one batch, so send them in batches of at most %d", n, maxEligibilityBatch, maxEligibilityBatch)
}

// eligibilitySubmission is one request an operation input submits: its exact
// bytes, the entries of the Bundle that carries it with the resources it
// references (none for a request sent alone or alone in a batch), the bytes
// its patient is bound from (that Bundle, or the request), and whether it was
// submitted in a Bundle (the operation's Bundle forms, read from the start by
// the rules a request sent alone keeps for its own reading).
type eligibilitySubmission struct {
	request []byte
	entries []bundleRefEntry
	scope   []byte
	bundled bool
}

// eligibilitySubmissions is the requests an operation input submits, in
// document order. An error is an input that is none of the operation's forms,
// or one this gateway cannot read one way only. Every input is read by its
// exact member names. A request sent alone that the strict reader refuses
// only for its own limits (it is not well-formed UTF-8 or holds a lone
// surrogate escape, or it is past its depth or token bound) is still read, by
// its exact member names, when it repeats no member name (repeatsAName).
func eligibilitySubmissions(body []byte) ([]eligibilitySubmission, error) {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if err := decodeMessage(body, &head); err != nil {
		if readsTwoWays(err) || repeatsAName(body) {
			return nil, err
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(body, &top) != nil {
			return nil, err
		}
		if _, ok := top["resourceType"]; !ok {
			for k := range top {
				if strings.EqualFold(k, "resourceType") {
					return nil, fmt.Errorf("the request's type: %w", errNamedInAnotherCase)
				}
			}
		}
		if json.Unmarshal(top["resourceType"], &head.ResourceType) != nil || head.ResourceType != resourceEligibilityRequest {
			return nil, err
		}
	}
	switch head.ResourceType {
	case resourceEligibilityRequest:
		return []eligibilitySubmission{{request: body, scope: body}}, nil
	case "Bundle":
		return eligibilityBundleSubmissions(body)
	case "Parameters":
	default:
		return nil, errors.New("expected a CoverageEligibilityRequest, a Bundle of them, or a Parameters holding one")
	}
	// Every parameter's name is read by its exact member name: a reader that
	// folded it could take another parameter for the resource. Only the
	// resource parameter's own members are read further; any other parameter
	// is carried as sent, however it spells any member but its name.
	var params struct {
		Parameter []json.RawMessage `json:"parameter"`
	}
	if err := decodeMessage(body, &params); err != nil {
		return nil, err
	}
	var resource json.RawMessage
	for _, raw := range params.Parameter {
		var p struct {
			Name string `json:"name"`
		}
		if err := decodeMessage(raw, &p); err != nil {
			return nil, err
		}
		if p.Name != "resource" {
			continue
		}
		var value struct {
			Resource json.RawMessage `json:"resource"`
		}
		if err := decodeMessage(raw, &value); err != nil {
			return nil, err
		}
		if resource != nil {
			return nil, errors.New("the Parameters holds more than one resource parameter")
		}
		if value.Resource == nil {
			return nil, errors.New("the resource parameter holds no resource")
		}
		resource = value.Resource
	}
	if resource == nil {
		return nil, errors.New("the Parameters holds no resource parameter")
	}
	var inner struct {
		ResourceType string `json:"resourceType"`
	}
	if err := decodeMessage(resource, &inner); err != nil {
		return nil, err
	}
	switch inner.ResourceType {
	case resourceEligibilityRequest:
		return []eligibilitySubmission{{request: resource, scope: resource}}, nil
	case "Bundle":
		return eligibilityBundleSubmissions(resource)
	}
	return nil, errors.New("the resource parameter holds no CoverageEligibilityRequest or Bundle of them")
}

// eligibilityNamedInAnotherCase is the refusal of a request err finds naming
// only in another case a member its patient is read through (its type, the
// Parameters' parameter, any parameter's name and the resource parameter's
// resource, its patient, that patient's reference, and the contained
// resources a "#id" reference names): member
// names are case-sensitive, so the request names no patient, and the patient
// the exchange is for cannot be identified. It names the member and the name
// it stands for, never a value. Any other member is not read: a request
// naming one in another case is carried as sent.
func eligibilityNamedInAnotherCase(err error) (string, bool) {
	const noPatient = ": member names are case-sensitive, so the request names no patient to bind"
	var fe *relay.FieldNameError
	switch {
	case errors.As(err, &fe):
		switch fe.Field {
		case "resourceType", "parameter", "name", "resource", "patient", "reference", "contained":
			return "eligibility request names " + quotedBounded(fe.Member) + " for " + quotedBounded(fe.Field) + noPatient, true
		}
	case errors.Is(err, errNamedInAnotherCase):
		return "eligibility request names its type or patient only in another case" + noPatient, true
	case errors.Is(err, errReplacedCharacter):
		return "eligibility request names its patient by a value carrying a replaced character (not well-formed UTF-8 or a lone surrogate escape): readers read it two ways, so the request names no patient to bind", true
	}
	return "", false
}

// eligibilityBundleEntries is a Bundle's entries read by their exact member
// names: each entry's fullUrl and resource, and that resource's resourceType
// and id. ok is false for a Bundle any of whose entries cannot be read so (a
// member named in another case, a value of another JSON type), or carries a
// resource with no type, or a null one: a reader that skipped or folded it
// would read another set of resources, so there is no single reading of what
// the Bundle carries. An entry with no resource member at all is refused too
// unless resourceless is set (an answer's batch-response entry may carry only
// its outcome), when it is left out.
func eligibilityBundleEntries(bundle []byte, resourceless bool) ([]bundleRefEntry, bool) {
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if decodeMessage(bundle, &b) != nil {
		return nil, false
	}
	out := make([]bundleRefEntry, 0, len(b.Entry))
	for _, e := range b.Entry {
		if len(e.Resource) == 0 && resourceless {
			continue
		}
		if len(e.Resource) == 0 || string(e.Resource) == "null" {
			return nil, false
		}
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if decodeMessage(e.Resource, &head) != nil || head.ResourceType == "" {
			return nil, false
		}
		out = append(out, bundleRefEntry{e.FullURL, head.ResourceType, head.ID, e.Resource})
	}
	return out, true
}

// eligibilityBundleSubmissions is the requests a Bundle submits: the one
// request of a Bundle carrying it with the resources it references, or, for
// a Bundle whose entries are all requests or such Bundles, each of them in
// order (a batch). Its entries are read by their exact member names
// (eligibilityBundleEntries). Any other Bundle is refused: it is neither
// form, so there is no single reading of what it submits.
func eligibilityBundleSubmissions(bundle []byte) ([]eligibilitySubmission, error) {
	entries, ok := eligibilityBundleEntries(bundle, false)
	if !ok {
		return nil, errors.New("the Bundle's entries cannot be read one way only")
	}
	var requests, bundles int
	for _, e := range entries {
		switch e.typ {
		case resourceEligibilityRequest:
			requests++
		case "Bundle":
			bundles++
		}
	}
	others := len(entries) - requests - bundles
	switch {
	case requests == 1 && bundles == 0:
		for _, e := range entries {
			if e.typ == resourceEligibilityRequest {
				return []eligibilitySubmission{{request: e.resource, entries: entries, scope: bundle, bundled: true}}, nil
			}
		}
	case requests+bundles > 0 && others == 0:
		out := make([]eligibilitySubmission, 0, len(entries))
		for i, e := range entries {
			if e.typ == resourceEligibilityRequest {
				out = append(out, eligibilitySubmission{request: e.resource, scope: e.resource, bundled: true})
				continue
			}
			sub, ok := oneRequest(e.resource)
			if !ok {
				return nil, fmt.Errorf("entry[%d] is a Bundle that does not carry exactly one CoverageEligibilityRequest with the resources it references", i)
			}
			sub.scope, sub.bundled = e.resource, true
			out = append(out, sub)
		}
		return out, nil
	case requests+bundles == 0:
		return nil, errors.New("the Bundle holds no CoverageEligibilityRequest")
	}
	return nil, errors.New("the Bundle is neither one CoverageEligibilityRequest with the resources it references nor a batch of requests")
}

// oneRequest is the request of a Bundle that carries exactly one, with the
// resources it references and no Bundle among them, its entries read by
// their exact member names.
func oneRequest(bundle []byte) (eligibilitySubmission, bool) {
	entries, ok := eligibilityBundleEntries(bundle, false)
	if !ok {
		return eligibilitySubmission{}, false
	}
	var found *bundleRefEntry
	for i, e := range entries {
		switch e.typ {
		case "Bundle":
			return eligibilitySubmission{}, false
		case resourceEligibilityRequest:
			if found != nil {
				return eligibilitySubmission{}, false
			}
			found = &entries[i]
		}
	}
	if found == nil {
		return eligibilitySubmission{}, false
	}
	return eligibilitySubmission{request: found.resource, entries: entries}, true
}

// eligibilitySubmissionMember is the member a submitted request's patient
// names. A request sent alone is read by eligibilityRequestMember. A request
// submitted in a Bundle (the
// operation's Bundle forms) is read only by its exact member names, never by
// a reading that folds or skips one: its patient reference and its contained
// resources must read one way, and a reference that is empty names no
// patient. Its reference names:
//   - "#id": the contained Patient with that id, by its member identifier
//     (patientRefMember);
//   - anything else, as a FHIR reader of the Bundle resolves it: the Patient
//     entry it resolves to among the entries of the Bundle carrying the
//     request with the resources it references (by fullUrl, or type and id),
//     by that entry's id or, with none, its member identifier. An entry whose
//     id is not the id a well-formed Patient reference writes (a fullUrl that
//     disagrees with its resource) is refused, as is a reference an entry of
//     another type answers: the request could be read as naming another
//     patient, or none;
//   - a well-formed Patient reference no entry answers: the id it writes.
//
// Any other reference names no patient, and is refused.
func eligibilitySubmissionMember(sub eligibilitySubmission) (string, error) {
	if !sub.bundled {
		return eligibilityRequestMember(sub.request)
	}
	var req struct {
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if err := decodeMessage(sub.request, &req); err != nil {
		return "", fmt.Errorf("the request's patient cannot be read one way only: %w", err)
	}
	ref := req.Patient.Reference
	if ref == "" {
		return "", errors.New("the request names its patient by no reference")
	}
	if strings.HasPrefix(ref, "#") {
		contained, err := eligibilityContainedFor(sub.request, ref)
		if err != nil {
			return "", fmt.Errorf("the request's contained resources cannot be read one way only: %w", err)
		}
		if member, ok := patientRefMember(ref, containedObjects(contained)); ok {
			return member, nil
		}
		return "", fmt.Errorf("patient reference %s names no contained Patient", quotedBounded(ref))
	}
	patient, how := resolveEntry(sub.entries, ref, "Patient")
	switch how {
	case refAmbiguous:
		return "", fmt.Errorf("patient reference %s names more than one Bundle entry", quotedBounded(ref))
	case refNoEntry:
		for _, e := range sub.entries {
			if entryAnswers(ref, e.fullURL, e.typ, e.id) {
				return "", fmt.Errorf("patient reference %s names a Bundle entry that is no Patient", quotedBounded(ref))
			}
		}
		if member, ok := patientRefMember(ref, nil); ok {
			return member, nil
		}
		return "", fmt.Errorf("patient reference %s names no Patient the Bundle carries", quotedBounded(ref))
	}
	var p struct {
		ID         string `json:"id"`
		Identifier []any  `json:"identifier"`
	}
	if decodeMessage(patient, &p) != nil {
		return "", fmt.Errorf("patient reference %s names a Patient that cannot be read one way only", quotedBounded(ref))
	}
	if written := patientRefShape.FindStringSubmatch(ref); written != nil && p.ID != written[1] {
		return "", fmt.Errorf("patient reference %s names a Bundle entry whose id is not the reference's", quotedBounded(ref))
	}
	if p.ID != "" {
		return p.ID, nil
	}
	if m, ok := containedPatientMember(map[string]any{"identifier": p.Identifier}); ok {
		return m, nil
	}
	return "", fmt.Errorf("patient reference %s names a Patient with no id or member identifier", quotedBounded(ref))
}

// resolveEntry is the entry of type typ that ref names among a Bundle's
// entries (entryAnswers: a fullUrl, or Type/id). Several entries answering
// it resolve only when, for an Organization, they name one payer
// (resolveAmong's rule), and otherwise are ambiguous.
func resolveEntry(entries []bundleRefEntry, ref, typ string) (json.RawMessage, refResolution) {
	var matches []bundleRefEntry
	for _, e := range entries {
		if entryAnswers(ref, e.fullURL, e.typ, e.id) {
			matches = append(matches, e)
		}
	}
	switch {
	case len(matches) == 0:
		return nil, refNoEntry
	case len(matches) == 1:
		if matches[0].typ != typ {
			return nil, refNoEntry
		}
		return matches[0].resource, refResolved
	case typ == "Organization":
		res, how := resolveAmong(matches, ref)
		if how == refResolved {
			if t, _ := entryHead(res); t != typ {
				return nil, refNoEntry
			}
		}
		return res, how
	}
	return nil, refAmbiguous
}

// The routing refusals of an eligibility request. eligibilityNoPayer and
// eligibilityCoverageNoPayer name what is missing: neither its insurer nor
// any coverage it names in insurance has a payer identifier, inline or on an
// Organization the request contains or its Bundle carries; or its insurer has
// none and a coverage it names (which the refusal names) has none, so it is
// not routed by the others. eligibilityTwoPayers is a request whose coverages
// name more than one payer, and eligibilityBatchTwoPayers a batch whose
// requests do: one leg has one recipient.
//
// eligibilityInsurerUnresolved, eligibilityCoverageUnresolved and
// eligibilityPayorUnresolved are a request whose payer is named only by a
// reference of the expected type nothing resolves (the insurer, a coverage it
// names in insurance, or that coverage's payor): the request does not carry
// the resource, and this participant's system of record does not supply it
// for the request's patient (eligibilitySoRRead). Each is followed by what
// resolves a reference of that form (eligibilityRemedy) and the reference
// itself.
const (
	eligibilityNoPayer         = "no payer to route the eligibility request by: neither its insurer nor the payor of a Coverage it carries names a payer identifier, inline or on an Organization it carries"
	eligibilityCoverageNoPayer = "no payer to route the eligibility request by: its insurer names no payer identifier, and a coverage it names in insurance names none, as the payor of a Coverage it carries, inline or on an Organization it carries"
	eligibilityTwoPayers       = "the eligibility request names more than one payer: its coverages name different payors, and one request goes to one payer"
	eligibilityBatchTwoPayers  = "the eligibility requests name more than one payer: one submission goes to one payer, so send the requests for each payer separately"

	eligibilityInsurerUnresolved  = "no payer to route the eligibility request by: its insurer is a reference to an Organization the gateway could not resolve, and no coverage it names in insurance names a payer"
	eligibilityCoverageUnresolved = "no payer to route the eligibility request by: its insurer names no payer identifier, and a coverage it names in insurance is a reference to a Coverage the gateway could not resolve"
	eligibilityPayorUnresolved    = "no payer to route the eligibility request by: its insurer names no payer identifier, and the payor of a coverage it names in insurance is a reference to an Organization the gateway could not resolve"

	// The identifier that names the payer in place of an unresolved
	// reference, in eligibilityRemedy's text.
	eligibilityRemedyInsurer = "the payer identifier on the insurer reference"
	eligibilityRemedyPayor   = "a payor identifier"

	// eligibilityRemedyHeldCoverage is what resolves the payor of a Coverage
	// read from the system of record: its own resources, or that system.
	eligibilityRemedyHeldCoverage = "a Coverage read from this participant's system of record names its payor by a payor identifier, an Organization it contains, or a relative Organization/<id> that system holds with the request's patient"
)

// eligibilityRemedy is what resolves a reference to a resourceType that
// nothing resolved, by the reference's form, as the CDS Hooks route's
// refusals name it (classifyUnresolvedPayor): a relative "<resourceType>/<id>"
// resolves from the request's own resources or this participant's system of
// record; a "#id" only as a contained resource with that id; a urn, or an
// absolute reference, only as a Bundle entry whose fullUrl it is; any other
// reference naming resourceType (versioned, with a leading slash) only from
// the request's own resources. identifier is the identifier that names the
// payer instead. ok is false for a reference that does not name a
// resourceType (a reference to another type): its refusal is that of a
// request naming no payer.
func eligibilityRemedy(ref, resourceType, identifier string) (remedy string, ok bool) {
	switch _, relative := eligibilityRelativeID(ref, resourceType); {
	case relative:
		return "send the " + resourceType + " with the request (contained, or as an entry of its Bundle), " + identifier +
			", or hold the " + resourceType + " in this participant's system of record with the request's patient", true
	case strings.HasPrefix(ref, "#"):
		return "send the " + resourceType + " as a contained resource with that id, or " + identifier, true
	case strings.HasPrefix(ref, "urn:uuid:") || strings.HasPrefix(ref, "urn:oid:"):
		return "send the " + resourceType + " as an entry of the request's Bundle whose fullUrl is that reference, or " + identifier, true
	case !namesType(ref, resourceType):
		return "", false
	}
	if _, _, absolute := absoluteRefPath(ref); absolute {
		return "send the " + resourceType + " as an entry of the request's Bundle whose fullUrl is that reference, or " + identifier +
			"; only a relative " + resourceType + "/<id> reference is read from this participant's system of record", true
	}
	return "send the " + resourceType + " with the request (contained, or as an entry of its Bundle), or " + identifier +
		"; only a relative " + resourceType + "/<id> reference is read from this participant's system of record", true
}

// eligibilityReference is a Reference the request is routed by, read by its
// exact member names.
type eligibilityReference struct {
	Reference  string                 `json:"reference"`
	Identifier *eligibilityIdentifier `json:"identifier"`
}

type eligibilityIdentifier struct {
	System string `json:"system"`
	Value  string `json:"value"`
}

// eligibilityRecipient is the payer holder the submitted requests are routed
// to: the payer each names (eligibilityRequestPayer, members[i] being the
// member subs[i]'s patient names), which must be one payer for them all. No
// default: a request naming no payer that resolves is refused (422), as is a
// batch naming more than one payer, and a payer the network does not
// register; a failed read of the system of record is that system's failure.
// The requests are read, never changed.
func (g *Gateway) eligibilityRecipient(ctx context.Context, subs []eligibilitySubmission, members []string) (string, int, string) {
	if g.cfg.PayerRouter == nil {
		return "", http.StatusUnprocessableEntity, "no payer router configured"
	}
	var pid shnsdk.PayerIdentifier
	for i, sub := range subs {
		p, status, msg := eligibilityRequestPayer(sub, g.eligibilitySoRRead(ctx, members[i]))
		if status != 0 {
			if len(subs) > 1 {
				msg = "request " + strconv.Itoa(i) + " of the batch: " + msg
			}
			return "", status, msg
		}
		if i > 0 && p != pid {
			return "", http.StatusUnprocessableEntity, fmt.Sprintf("%s (request 0 names %s|%s, request %d names %s|%s)",
				eligibilityBatchTwoPayers, pid.System, pid.Value, i, p.System, p.Value)
		}
		pid = p
	}
	holder, found := g.cfg.PayerRouter.Resolve(pid)
	if !found {
		return "", http.StatusUnprocessableEntity, "no registered payer for identifier " + pid.System + "|" + pid.Value
	}
	return holder, 0, ""
}

// eligibilityScope is where a reference written in one resource resolves: a
// "#id" among that resource's contained resources, anything else among the
// entries of the Bundle carrying the request (none for a request sent alone),
// as a FHIR reader of the request resolves it; then, for a relative reference
// nothing the request carries answers, from the participant's system of
// record (sor, eligibilitySoRRead).
type eligibilityScope struct {
	byID    map[string][]eligibilityContained
	entries []bundleRefEntry
	sor     eligibilitySoRRead
}

// eligibilitySoRRead reads the resource of a type a relative reference
// ("Organization/<id>", "Coverage/<id>") names from the participant's system
// of record: nil when that system does not supply it, else the resource; a
// status and reason for a failed read.
type eligibilitySoRRead func(ref, resourceType string) (json.RawMessage, int, string)

// eligibilitySoRRead resolves, from the participant's system of record, a
// resource an eligibility request names by a relative reference that nothing
// it carries resolves: the network's routing read, done only to choose the
// payer; nothing it reads is added to or changed in what is carried. As for
// a $questionnaire-package request (keptPayorResolver), the system is read
// only when it names the patient by member (its records are the request's:
// an eligibility request names no other server), and the system's id for
// the patient is read once, only for such a reference; a system that cannot
// name the patient counts as one that does not hold it. A reference written
// absolute, versioned, with a fragment, a leading slash or a dot segment is
// never read (eligibilityRelativeID). A failed read is that system's failure,
// and so is an answer that is not the resource asked for.
func (g *Gateway) eligibilitySoRRead(ctx context.Context, member string) eligibilitySoRRead {
	asked, named := false, false
	// A reference the request names more than once is read once, and every
	// later ask answers as the read did.
	type read struct {
		res    json.RawMessage
		status int
		msg    string
	}
	reads := map[string]read{}
	return func(ref, resourceType string) (json.RawMessage, int, string) {
		id, ok := eligibilityRelativeID(ref, resourceType)
		if !ok {
			return nil, 0, ""
		}
		key := resourceType + "/" + id
		if r, ok := reads[key]; ok {
			return r.res, r.status, r.msg
		}
		res, status, msg := g.eligibilitySoRReadOnce(ctx, member, key, resourceType, id, &asked, &named)
		reads[key] = read{res, status, msg}
		return res, status, msg
	}
}

// eligibilitySoRReadOnce is one read of eligibilitySoRRead: the system's id
// for the patient first, once (*asked, *named), then key.
func (g *Gateway) eligibilitySoRReadOnce(ctx context.Context, member, key, resourceType, id string, asked, named *bool) (json.RawMessage, int, string) {
	sor := ReadSystemOfRecord(g.cfg.SoR)
	if !*asked {
		*asked = true
		// A failed read of the patient's id is on the sor.read observer
		// event: the system is then read for nothing.
		got, found, err := sor.PatientFHIRRefContext(ctx, member)
		*named = err == nil && found && got == "Patient/"+member
	}
	if !*named {
		return nil, 0, ""
	}
	b, found, err := sor.ResolveByReferenceContext(ctx, key)
	if err != nil {
		status, msg := SoRFailureResponse(safeSoRError(err))
		return nil, status, msg
	}
	if !found {
		return nil, 0, ""
	}
	var head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if decodeMessage(b, &head) != nil || head.ResourceType != resourceType || head.ID != id {
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	return b, 0, ""
}

// eligibilityRelativeID is the id of a relative reference "<resourceType>/<id>"
// with a valid id: no base, version, fragment, leading slash or dot segment.
func eligibilityRelativeID(ref, resourceType string) (string, bool) {
	id, ok := strings.CutPrefix(ref, resourceType+"/")
	if !ok || !fhirIDRE.MatchString(id) || id == "." || id == ".." {
		return "", false
	}
	return id, true
}

type eligibilityContained struct {
	resourceType string
	raw          json.RawMessage
}

// newEligibilityScope is the scope of a resource with contained among the
// Bundle entries, then sor. A contained resource whose resourceType and id
// cannot be read by their exact names carries no id a reference can name.
func newEligibilityScope(contained []json.RawMessage, entries []bundleRefEntry, sor eligibilitySoRRead) eligibilityScope {
	s := eligibilityScope{byID: map[string][]eligibilityContained{}, entries: entries, sor: sor}
	for _, c := range contained {
		var head struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
		}
		if decodeMessage(c, &head) == nil && head.ID != "" {
			s.byID[head.ID] = append(s.byID[head.ID], eligibilityContained{head.ResourceType, c})
		}
	}
	return s
}

// resolve is the resource of resourceType that ref names in the scope; a
// refusal when that reference names more than one resource, or the system of
// record's read of it fails. unresolved is true when nothing answers ref: no
// contained resource has its id, no Bundle entry answers it, and the system
// of record does not supply it (a reference naming a resource of another type
// is answered, by that resource). A reference a Bundle entry answers, of any
// type, is the entry's: it is never read from the system of record too.
func (s eligibilityScope) resolve(ref, resourceType string) (res json.RawMessage, unresolved bool, status int, msg string) {
	if id, ok := strings.CutPrefix(ref, "#"); ok {
		switch found := s.byID[id]; {
		case id == "":
		case len(found) > 1:
			return nil, false, http.StatusUnprocessableEntity, "ambiguous reference for routing: the eligibility request contains more than one resource with id " + quotedBounded(id)
		case len(found) == 1 && found[0].resourceType == resourceType:
			return found[0].raw, false, 0, ""
		case len(found) == 1:
			return nil, false, 0, ""
		}
		return nil, true, 0, ""
	}
	if ref == "" {
		return nil, false, 0, ""
	}
	res, how := resolveEntry(s.entries, ref, resourceType)
	switch how {
	case refAmbiguous:
		return nil, false, http.StatusUnprocessableEntity, "ambiguous reference for routing: more than one entry of the eligibility request's Bundle answers " + quotedBounded(ref)
	case refResolved:
		return res, false, 0, ""
	}
	for _, e := range s.entries {
		if entryAnswers(ref, e.fullURL, e.typ, e.id) {
			return nil, false, 0, ""
		}
	}
	if s.sor == nil {
		return nil, true, 0, ""
	}
	res, status, msg = s.sor(ref, resourceType)
	return res, res == nil && status == 0, status, msg
}

// fromSoR reports whether ref is resolved from the system of record: a
// relative reference nothing the request carries answers. It reads nothing.
func (s eligibilityScope) fromSoR(ref, resourceType string) bool {
	if _, ok := eligibilityRelativeID(ref, resourceType); !ok || s.sor == nil {
		return false
	}
	for _, e := range s.entries {
		if entryAnswers(ref, e.fullURL, e.typ, e.id) {
			return false
		}
	}
	return true
}

// payer is the payer identifier a payor or insurer Reference written in the
// scope's resource names: its own identifier, else the first identifier of
// the Organization it references. unresolved is true when it names no payer
// identifier and its reference resolves to no Organization.
func (s eligibilityScope) payer(ref *eligibilityReference) (pid shnsdk.PayerIdentifier, unresolved bool, status int, msg string) {
	var none shnsdk.PayerIdentifier
	if ref == nil {
		return none, false, 0, ""
	}
	if id := ref.Identifier; id != nil && id.System != "" && id.Value != "" {
		return shnsdk.PayerIdentifier{System: id.System, Value: id.Value}, false, 0, ""
	}
	org, unresolved, status, msg := s.resolve(ref.Reference, "Organization")
	if org == nil {
		return none, unresolved, status, msg
	}
	var o struct {
		Identifier []eligibilityIdentifier `json:"identifier"`
	}
	if decodeMessage(org, &o) != nil {
		return none, false, 0, ""
	}
	for _, id := range o.Identifier {
		if id.System != "" && id.Value != "" {
			return shnsdk.PayerIdentifier{System: id.System, Value: id.Value}, false, 0, ""
		}
	}
	return none, false, 0, ""
}

// eligibilityRequestPayer is the payer a submitted CoverageEligibilityRequest
// names: its insurer, the recipient of the request, by identifier or by an
// Organization it carries; else the first payor of each Coverage it carries
// and names in insurance, which must all name one payer. A reference resolves
// as a FHIR reader of the request resolves it (eligibilityScope): a "#id"
// among the referring resource's contained resources, anything else among the
// entries of the Bundle carrying the request, one resource of the expected
// type; a relative reference nothing the request carries answers, from the
// participant's system of record (sor). Every member is read by its exact
// name. The request's routing members that cannot be read one way only are
// refused 400; a resource whose members cannot be read so names no payer, as
// a reader taking member names exactly reads it. A payer named only by a
// reference nothing resolves is refused naming that reference and what
// resolves it.
func eligibilityRequestPayer(sub eligibilitySubmission, sor eligibilitySoRRead) (shnsdk.PayerIdentifier, int, string) {
	var none shnsdk.PayerIdentifier
	// resourceType is in the view so that a request naming it in another
	// case, which a FHIR reader sees no type on, does not read one way only.
	var req struct {
		ResourceType string                `json:"resourceType"`
		Insurer      *eligibilityReference `json:"insurer"`
		Contained    []json.RawMessage     `json:"contained"`
		Insurance    []struct {
			Coverage *eligibilityReference `json:"coverage"`
		} `json:"insurance"`
	}
	if err := decodeMessage(sub.request, &req); err != nil {
		return none, http.StatusBadRequest, "parse eligibility request failed"
	}
	scope := newEligibilityScope(req.Contained, sub.entries, sor)
	pid, insurerUnresolved, status, msg := scope.payer(req.Insurer)
	if status != 0 || pid != none {
		return pid, status, msg
	}
	// Every coverage the request names must name a payer, and all of them
	// the same one: a coverage naming none is not passed over for another.
	var found shnsdk.PayerIdentifier
	for i, ins := range req.Insurance {
		if ins.Coverage == nil {
			continue
		}
		at := "insurance[" + strconv.Itoa(i) + "].coverage " + quotedBounded(ins.Coverage.Reference)
		cov, unresolved, status, msg := scope.resolve(ins.Coverage.Reference, "Coverage")
		if status != 0 {
			return none, status, msg
		}
		if remedy, ok := eligibilityRemedy(ins.Coverage.Reference, "Coverage", eligibilityRemedyInsurer); unresolved && ok {
			return none, http.StatusUnprocessableEntity, eligibilityCoverageUnresolved + "; " + remedy + ": " + at
		}
		var c struct {
			Payor []*eligibilityReference `json:"payor"`
		}
		var pid shnsdk.PayerIdentifier
		var payorUnresolved bool
		held := scope.fromSoR(ins.Coverage.Reference, "Coverage")
		if cov != nil && decodeMessage(cov, &c) == nil && len(c.Payor) > 0 {
			// A Coverage the request contains shares the request's contained
			// resources, and is read for its payor alone, as before; a
			// Coverage entry, or one the system of record supplies, has its
			// own contained resources (none a reference can name when they
			// cannot be read one way only), and a supplied one is not among
			// the request's entries.
			covScope := scope
			if !strings.HasPrefix(ins.Coverage.Reference, "#") {
				var own struct {
					Contained []json.RawMessage `json:"contained"`
				}
				if decodeMessage(cov, &own) != nil {
					own.Contained = nil
				}
				entries := sub.entries
				if held {
					entries = nil
				}
				covScope = newEligibilityScope(own.Contained, entries, sor)
			}
			if pid, payorUnresolved, status, msg = covScope.payer(c.Payor[0]); status != 0 {
				return none, status, msg
			}
		}
		switch {
		case pid == none && payorUnresolved:
			remedy, ok := eligibilityRemedy(c.Payor[0].Reference, "Organization", eligibilityRemedyPayor)
			if !ok {
				return none, http.StatusUnprocessableEntity, eligibilityCoverageNoPayer + ": " + at
			}
			if held {
				// Nothing the request carries resolves a payor of a
				// Coverage read from the system of record.
				remedy = eligibilityRemedyHeldCoverage
			}
			return none, http.StatusUnprocessableEntity, eligibilityPayorUnresolved + "; " + remedy + ": " + at + ", payor " + quotedBounded(c.Payor[0].Reference)
		case pid == none:
			return none, http.StatusUnprocessableEntity, eligibilityCoverageNoPayer + ": " + at
		case found != none && pid != found:
			return none, http.StatusUnprocessableEntity, eligibilityTwoPayers
		}
		found = pid
	}
	if found == none {
		if remedy, ok := eligibilityRemedy(req.Insurer.Reference, "Organization", eligibilityRemedyInsurer); insurerUnresolved && ok {
			return none, http.StatusUnprocessableEntity, eligibilityInsurerUnresolved + "; " + remedy + ": insurer " + quotedBounded(req.Insurer.Reference)
		}
		return none, http.StatusUnprocessableEntity, eligibilityNoPayer
	}
	return found, 0, ""
}

// eligibilityAnswerResponse is one CoverageEligibilityResponse an answer
// gives, with the entries of the Bundle carrying it (none for an answer that
// is the response itself), which its references resolve among.
type eligibilityAnswerResponse struct {
	response []byte
	entries  []bundleRefEntry
}

// eligibilityAnswerResponses is the CoverageEligibilityResponses an answer
// gives: the answer itself, unless it is a Bundle, whose responses are its
// CoverageEligibilityResponse entries and those of the Bundles among its
// entries (each response with the resources it references), read by their
// exact member names. ok is false for a Bundle that holds none, or that
// cannot be read one way only (its type or entries named in another case
// included). A response is returned as its exact bytes, for the caller to
// read; an answer that is not a Bundle is read as the response it is.
func eligibilityAnswerResponses(answer []byte) ([]eligibilityAnswerResponse, bool) {
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if json.Unmarshal(answer, &head) != nil || head.ResourceType != "Bundle" {
		return []eligibilityAnswerResponse{{response: answer}}, true
	}
	if typ, _ := entryHead(answer); typ != "Bundle" {
		return nil, false
	}
	var out []eligibilityAnswerResponse
	var read func(bundle []byte, depth int) bool
	read = func(bundle []byte, depth int) bool {
		entries, ok := eligibilityBundleEntries(bundle, true)
		if !ok {
			return false
		}
		for _, e := range entries {
			switch {
			case e.typ == "CoverageEligibilityResponse":
				out = append(out, eligibilityAnswerResponse{e.resource, entries})
			case e.typ == "Bundle" && depth == 0:
				if !read(e.resource, depth+1) {
					return false
				}
			case e.typ == "Bundle":
				// A Bundle nested deeper than a batch of response Bundles
				// is no form of the operation's answer: it is not read
				// past, so the answer cannot be read one way only.
				return false
			}
		}
		return true
	}
	if !read(answer, 0) || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// patientRef is the reference a response's patient is named by, as the
// request's patient is read: in a Bundle, a reference that resolves to one
// Patient entry is "Patient/<that entry's id>".
func (a eligibilityAnswerResponse) patientRef(ref string) string {
	if len(a.entries) == 0 || strings.HasPrefix(ref, "#") {
		return ref
	}
	patient, how := resolveEntry(a.entries, ref, "Patient")
	if how != refResolved {
		return ref
	}
	if _, id := entryHead(patient); id != "" {
		return "Patient/" + id
	}
	return ref
}
