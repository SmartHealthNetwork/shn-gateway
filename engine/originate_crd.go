// originate_crd.go — the CDS Hooks requests this gateway originates for its own
// participant (the provider's scenario flows), built from the participant's own
// records.
//
// An originated request carries:
//   - the hook of the workflow it checks: order-sign for a signed order that is
//     going to prior authorization, order-select for an order being chosen
//     (the no-authorization coverage check), order-dispatch for a dispatched
//     order;
//   - the system of record's Patient, read by id, and its Coverage search
//     result (the matching Coverages with their payor Organizations), both as
//     the system returned them;
//   - the history prefetch values the provider ingress advertises, searched the
//     same way (null for no match, left out when the system cannot answer);
//   - no fhirServer and no fhirAuthorization.
//
// The network names a patient by the member id: the payer resolves the
// request's patient by that id. When the system of record names the patient by
// another id, this gateway, as the author of its own request, names the patient
// by the member id in the records it carries: the Patient's id, and the Patient
// reference on each record's binding path (an order's subject, a Coverage's
// beneficiary). Nothing else in a record changes, and the change is checked
// (namePatientByMember). History values are then left out, as the provider
// ingress does.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The CDS Hooks hooks an originated request fires.
const (
	// hookOrderSign: a signed order the provider is taking to prior
	// authorization.
	hookOrderSign = "order-sign"
	// hookOrderSelect: an order being chosen, checked for coverage only.
	hookOrderSelect = "order-select"
	// hookOrderDispatch: an order being dispatched to a performer.
	hookOrderDispatch = "order-dispatch"
)

// crdOriginRecords are the participant's records an originated CDS Hooks
// request carries, the patient named by the member id.
type crdOriginRecords struct {
	member string
	// sorID is the system of record's id for the patient.
	sorID string
	// patient is the system of record's Patient.
	patient []byte
	// coverage is the Coverage search result: a searchset Bundle, or the
	// JSON literal null when the system holds no Coverage.
	coverage []byte
	// history holds the other obtained prefetch values by key: a searchset
	// Bundle, or null.
	history map[string][]byte
}

// renamed reports whether the system of record names the patient by an id
// other than the member id.
func (r crdOriginRecords) renamed() bool { return r.sorID != r.member }

// originCRDRecords reads the records an originated CDS Hooks request on leg
// carries for member: the patient, the coverage and the history values. Each
// read is recorded as a PrefetchObtainedEvent, and every value is fenced to
// the member before it is used. A non-zero status refuses the request:
//   - 422 when the system of record holds no Patient or no Coverage for the
//     member, or cannot search for coverage;
//   - 503 when the system of record is unavailable;
//   - 502 when it answers about another patient.
func (g *Gateway) originCRDRecords(ctx context.Context, leg, member string) (crdOriginRecords, int, string) {
	out := crdOriginRecords{member: member, history: map[string][]byte{}}
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return out, status, msg
	}
	if !found {
		return out, http.StatusUnprocessableEntity, "patient not found in system of record"
	}
	sorID, ok := strings.CutPrefix(ref, "Patient/")
	if !ok || !fhirIDRE.MatchString(sorID) {
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return out, status, msg
	}
	out.sorID = sorID
	fence := newPatientFence(shnsdk.MemberSystem, member, nil, sorID, member).forPrefetch()

	patient, _, status, msg := g.obtainPrefetch(ctx, leg, prefetchPatientKey, sorID, fence)
	if status != 0 {
		return out, status, msg
	}
	coverage, outcome, status, msg := g.obtainPrefetch(ctx, leg, "coverage", sorID, fence)
	switch {
	case status != 0:
		return out, status, msg
	case coverage == nil:
		status, msg := coverageOmitted(outcome)
		return out, status, msg
	case string(coverage) == "null":
		return out, http.StatusUnprocessableEntity, "no coverage in request or system of record"
	}
	if out.renamed() {
		if patient, err = namePatientByMember(patient, sorID, member); err != nil {
			return out, http.StatusBadGateway, "name the patient in the system of record's Patient: " + err.Error()
		}
		if coverage, err = namePatientByMember(coverage, sorID, member); err != nil {
			return out, http.StatusBadGateway, "name the patient in the system of record's coverage: " + err.Error()
		}
	}
	out.patient, out.coverage = patient, coverage

	for _, key := range pinnedPrefetchKeys {
		if key == prefetchPatientKey || key == "coverage" {
			continue
		}
		if out.renamed() {
			query, _ := SoRSearchQuery(prefetchSearchTypes[key], sorID)
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchNotRun, Reason: historyNamedDifferently})
			continue
		}
		value, _, status, msg := g.obtainPrefetch(ctx, leg, key, sorID, fence)
		if status != 0 {
			return out, status, msg
		}
		if value != nil {
			out.history[key] = value
		}
	}
	return out, 0, ""
}

// originOrder names the patient by the member id in an order read from the
// system of record (the order is returned unchanged when the system names the
// patient by the member id).
func originOrder(recs crdOriginRecords, order []byte) ([]byte, error) {
	if !recs.renamed() {
		return order, nil
	}
	return namePatientByMember(order, recs.sorID, recs.member)
}

// hookInstanceFor is the request's hookInstance: a UUID derived from the
// exchange's correlation id.
func hookInstanceFor(correlationID string) string {
	h := correlationID
	if len(h) != 32 || !isLowerHex(h) {
		sum := sha256.Sum256([]byte(correlationID))
		h = hex.EncodeToString(sum[:16])
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resourceTypeAndID reads a resource's type and id.
func resourceTypeAndID(resource []byte) (string, string, error) {
	var head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if err := decodeMessage(resource, &head); err != nil {
		return "", "", err
	}
	if head.ResourceType == "" || head.ID == "" {
		return "", "", errors.New("resource has no type or id")
	}
	return head.ResourceType, head.ID, nil
}

// errNoOrderingClinician refuses an originated ordering request that has no
// ordering clinician to name.
var errNoOrderingClinician = errors.New("order names no requester; configure NPI")

// orderRequester returns the order's requester when it names a Practitioner or
// PractitionerRole, "" otherwise.
func orderRequester(order []byte) string {
	var o struct {
		Requester struct {
			Reference string `json:"reference"`
		} `json:"requester"`
	}
	if decodeMessage(order, &o) != nil {
		return ""
	}
	ref := o.Requester.Reference
	if strings.HasPrefix(ref, "Practitioner/") || strings.HasPrefix(ref, "PractitionerRole/") {
		return ref
	}
	return ""
}

// orderingUser is the CDS Hooks userId of an originated ordering request: the
// order's requester when it names a Practitioner or PractitionerRole;
// otherwise, when an NPI is configured, the Practitioner named by it;
// otherwise errNoOrderingClinician (nothing stands in for the clinician).
func (g *Gateway) orderingUser(order []byte) (string, error) {
	if ref := orderRequester(order); ref != "" {
		return ref, nil
	}
	if g.cfg.NPI != "" {
		return "Practitioner/" + g.cfg.NPI, nil
	}
	return "", errNoOrderingClinician
}

// npiSystem is the NPI identifier system.
const npiSystem = "http://hl7.org/fhir/sid/us-npi"

// attestingNPI returns the NPI of the clinician who attests for order: the
// NPI the caller sent; otherwise the configured NPI; otherwise the NPI of the
// order's requester (a Practitioner, or a PractitionerRole's practitioner),
// read from the system of record. With none of them the attestation is
// refused (422); a failed read is the system-of-record failure.
func (g *Gateway) attestingNPI(ctx context.Context, order []byte, sent string) (string, int, string) {
	if sent != "" {
		return sent, 0, ""
	}
	if g.cfg.NPI != "" {
		return g.cfg.NPI, 0, ""
	}
	refuse := func() (string, int, string) {
		return "", http.StatusUnprocessableEntity, "attestation names no clinician NPI: send npi, configure NPI, or name the order's requester with an NPI"
	}
	ref := orderRequester(order)
	sor := ReadSystemOfRecord(g.cfg.SoR)
	for hop := 0; hop < 2 && ref != ""; hop++ {
		rec, found, err := sor.ResolveByReferenceContext(ctx, ref)
		if err != nil {
			status, msg := SoRFailureResponse(err)
			return "", status, msg
		}
		if !found {
			return refuse()
		}
		var r struct {
			ResourceType string `json:"resourceType"`
			Identifier   []struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
			Practitioner struct {
				Reference string `json:"reference"`
			} `json:"practitioner"`
		}
		if decodeMessage(rec, &r) != nil {
			return refuse()
		}
		switch {
		case r.ResourceType == "Practitioner" && strings.HasPrefix(ref, "Practitioner/"):
			for _, id := range r.Identifier {
				if id.System == npiSystem && id.Value != "" {
					return id.Value, 0, ""
				}
			}
			return refuse()
		case r.ResourceType == "PractitionerRole" && strings.HasPrefix(ref, "PractitionerRole/") && strings.HasPrefix(r.Practitioner.Reference, "Practitioner/"):
			ref = r.Practitioner.Reference
		default:
			return refuse()
		}
	}
	return refuse()
}

// collectionBundle wraps resources in a collection Bundle, each entry's
// fullUrl its Type/id (the form a payer resolves a context reference by).
// Each resource is copied byte for byte.
func collectionBundle(resources ...[]byte) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"resourceType":"Bundle","type":"collection","entry":[`)
	for i, r := range resources {
		rt, id, err := resourceTypeAndID(r)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		fullURL, _ := json.Marshal(rt + "/" + id)
		b.WriteString(`{"fullUrl":`)
		b.Write(fullURL)
		b.WriteString(`,"resource":`)
		b.Write(bytes.TrimSpace(r))
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.Bytes(), nil
}

// originatedOrderingRequest builds an order-sign or order-select request for
// order from recs.
func (g *Gateway) originatedOrderingRequest(hook, correlationID string, recs crdOriginRecords, order []byte) ([]byte, error) {
	rt, id, err := resourceTypeAndID(order)
	if err != nil {
		return nil, fmt.Errorf("order: %w", err)
	}
	draft, err := collectionBundle(order)
	if err != nil {
		return nil, err
	}
	user, err := g.orderingUser(order)
	if err != nil {
		return nil, err
	}
	in := shnsdk.CRDRequestInputs{
		Hook:         hook,
		HookInstance: hookInstanceFor(correlationID),
		UserID:       user,
		DraftOrders:  draft,
		Patient:      recs.patient,
		Coverage:     recs.coverage,
		Prefetch:     recs.history,
	}
	if hook == hookOrderSelect {
		in.Selections = []string{rt + "/" + id}
	}
	return shnsdk.BuildCRDRequest(in)
}

// originatedDispatchRequest builds an order-dispatch request for order,
// dispatched to performer. The payer resolves the dispatched order from the
// request's device history. For an order the system of record holds, that
// history is the system's search result and must hold the order (or, when the
// search was left out, the order itself in a collection Bundle). For an order
// this gateway authored (authored), the device history is that order in a
// collection Bundle: the system of record does not hold it.
func (g *Gateway) originatedDispatchRequest(correlationID string, recs crdOriginRecords, order []byte, performer string, authored bool) ([]byte, error) {
	rt, id, err := resourceTypeAndID(order)
	if err != nil {
		return nil, fmt.Errorf("order: %w", err)
	}
	prefetch := cloneValues(recs.history)
	switch history, obtained := prefetch["deviceHistory"]; {
	case obtained && !authored && !bundleHolds(history, rt, id):
		return nil, fmt.Errorf("the system of record's device history does not hold the dispatched order %s/%s", rt, id)
	case !obtained || authored:
		history, err := collectionBundle(order)
		if err != nil {
			return nil, err
		}
		prefetch["deviceHistory"] = history
	}
	return shnsdk.BuildCRDRequest(shnsdk.CRDRequestInputs{
		Hook:             hookOrderDispatch,
		HookInstance:     hookInstanceFor(correlationID),
		DispatchedOrders: []string{rt + "/" + id},
		Performer:        performer,
		Patient:          recs.patient,
		Coverage:         recs.coverage,
		Prefetch:         prefetch,
	})
}

func cloneValues(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// bundleHolds reports whether bundle holds a resource rt/id.
func bundleHolds(bundle []byte, rt, id string) bool {
	if len(bundle) == 0 {
		return false
	}
	var b struct {
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if decodeMessage(bundle, &b) != nil {
		return false
	}
	for _, e := range b.Entry {
		if e.Resource.ResourceType == rt && e.Resource.ID == id {
			return true
		}
	}
	return false
}

// payerUpdatedOrder returns the order the payer returned for the order
// rt/id — the order with the payer's coverage information — and that
// coverage information, read from the answer without changing a byte. ok is
// false when the answer returns no such order.
func payerUpdatedOrder(answer []byte, rt, id string) (order []byte, coverage []shnsdk.CoverageInformation, ok bool) {
	obs, err := shnsdk.ParseCRDResponse(answer)
	if err != nil {
		return nil, nil, false
	}
	for _, o := range obs.Orders {
		if o.ResourceType == rt && o.ID == id && len(o.Order) > 0 {
			return append([]byte(nil), o.Order...), o.Coverage, true
		}
	}
	return nil, nil, false
}

// crdOriginated is what an originated CDS Hooks exchange produced: the
// payer's coverage answer and, when the payer returned it, the order with the
// payer's coverage information on it.
type crdOriginated struct {
	coverage shnsdk.CardCoverage
	// updatedOrder is the payer's updated order (nil when the payer returned
	// none for the order sent).
	updatedOrder []byte
	// assertionID is the payer's coverage-assertion-id for the order ("" when
	// it stated none).
	assertionID string
}

// readOriginatedAnswer reads the payer's answer to an originated request for
// order.
func readOriginatedAnswer(answer, order []byte) (crdOriginated, error) {
	cov, err := crdCoverage(answer)
	if err != nil {
		return crdOriginated{}, err
	}
	out := crdOriginated{coverage: cov}
	rt, id, err := resourceTypeAndID(order)
	if err != nil {
		return out, nil
	}
	if updated, cis, ok := payerUpdatedOrder(answer, rt, id); ok {
		out.updatedOrder = updated
		for _, ci := range cis {
			if ci.CoverageAssertionID != "" {
				out.assertionID = ci.CoverageAssertionID
				break
			}
		}
	}
	return out, nil
}

// patientRefRE matches a relative Patient reference (optionally versioned)
// and captures the id. An absolute reference names a server the gateway does
// not resolve (the prefetch fence refuses one that names a Patient), so it is
// never renamed.
var patientRefRE = regexp.MustCompile(`^Patient/([A-Za-z0-9\-.]{1,64})(?:/_history/[A-Za-z0-9\-.]{1,64})?$`)

// namePatientByMember returns record — a resource, or a Bundle of resources —
// with the patient the system of record names sorID named by member:
//   - a Patient resource (the record, or an entry's resource) whose id is
//     sorID gets the id member (its links, which name other records, stay);
//   - each relative Patient reference to sorID on another resource's binding
//     path (patientBindingPaths: an order's subject, a Coverage's
//     beneficiary, …) becomes "Patient/<member>", in contained resources too
//     (a contained Patient's id is local to its container and stays).
//
// Only those string values change; every other byte of the record is kept.
// The result is checked independently of how it was made: outside the
// changed values the bytes are the record's, and decoding the result gives
// exactly the record's content with those values changed.
func namePatientByMember(record []byte, sorID, member string) ([]byte, error) {
	if sorID == member {
		return record, nil
	}
	doc, err := relay.Doc(relay.NewBody(record, relay.OriginUpstreamResponse))
	if err != nil {
		return nil, err
	}
	root := doc.Root()
	if doc.Kind(root) != relay.KindObject {
		return nil, errors.New("record is not a resource")
	}
	var edits []patientRename
	var visit func(res relay.NodeID, path []any, contained bool) error
	visit = func(res relay.NodeID, path []any, contained bool) error {
		rt, _ := stringMember(doc, res, "resourceType")
		if rt == "Patient" && !contained {
			if id, ok := doc.Member(res, "id"); ok && doc.Kind(id) == relay.KindString {
				if v, _ := doc.StringValue(id); v == sorID {
					s, e := doc.Span(id)
					edits = append(edits, patientRename{s, e, member, appendPath(path, "id")})
				}
			}
		}
		// A Patient is named by its id; its links name other records and stay.
		paths := patientBindingPaths[rt]
		if rt == "Patient" {
			paths = nil
		}
		for _, p := range paths {
			for _, ref := range referenceNodes(doc, res, strings.Split(p, "."), path) {
				v, _ := doc.StringValue(ref.node)
				if m := patientRefRE.FindStringSubmatch(v); m != nil && m[1] == sorID {
					s, e := doc.Span(ref.node)
					edits = append(edits, patientRename{s, e, "Patient/" + member, ref.path})
				}
			}
		}
		if list, ok := doc.Member(res, "contained"); ok && doc.Kind(list) == relay.KindArray && !contained {
			for i, c := range doc.Elems(list) {
				if doc.Kind(c) == relay.KindObject {
					if err := visit(c, appendPath(path, "contained", i), true); err != nil {
						return err
					}
				}
			}
		}
		if rt == "Bundle" {
			entries, _ := doc.Member(res, "entry")
			for i, e := range doc.Elems(entries) {
				if r, ok := doc.Member(e, "resource"); ok && doc.Kind(r) == relay.KindObject {
					if err := visit(r, appendPath(path, "entry", i, "resource"), false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := visit(root, nil, false); err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return record, nil
	}
	slices.SortFunc(edits, func(a, b patientRename) int { return a.start - b.start })
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		if e.start < at {
			return nil, errors.New("overlapping patient names")
		}
		out.Write(record[at:e.start])
		out.Write(jsonString(e.value))
		at = e.end
	}
	out.Write(record[at:])
	if renameFaultHook != nil {
		renameFaultHook(out.Bytes())
	}
	if err := checkPatientRenames(record, out.Bytes(), edits); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// renameFaultHook, when set by a test, changes the renamed bytes before they
// are checked, to prove the check catches it.
var renameFaultHook func([]byte)

// patientRename replaces the string value record[start:end], at path, with
// value.
type patientRename struct {
	start, end int
	value      string
	path       []any
}

type refNode struct {
	node relay.NodeID
	path []any
}

func appendPath(path []any, more ...any) []any {
	return append(slices.Clone(path), more...)
}

// referenceNodes returns the reference string of each Reference at the
// dotted element path under obj, following arrays at any step.
func referenceNodes(doc *relay.Document, obj relay.NodeID, steps []string, path []any) []refNode {
	var out []refNode
	var walk func(n relay.NodeID, steps []string, path []any)
	walk = func(n relay.NodeID, steps []string, path []any) {
		switch doc.Kind(n) {
		case relay.KindArray:
			for i, e := range doc.Elems(n) {
				walk(e, steps, appendPath(path, i))
			}
			return
		case relay.KindObject:
		default:
			return
		}
		if len(steps) == 0 {
			if r, ok := doc.Member(n, "reference"); ok && doc.Kind(r) == relay.KindString {
				out = append(out, refNode{r, appendPath(path, "reference")})
			}
			return
		}
		if next, ok := doc.Member(n, steps[0]); ok {
			walk(next, steps[1:], appendPath(path, steps[0]))
		}
	}
	walk(obj, steps, path)
	return out
}

func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// checkPatientRenames verifies out against record and the declared renames:
// the bytes between the renamed values are the record's, and out decodes to
// the record's content with exactly the declared values changed.
func checkPatientRenames(record, out []byte, edits []patientRename) error {
	if _, err := relay.Doc(relay.NewBody(out, relay.OriginUpstreamResponse)); err != nil {
		return fmt.Errorf("renamed record is not readable: %w", err)
	}
	ri, oi := 0, 0
	for _, e := range edits {
		n := e.start - ri
		if oi+n > len(out) || !bytes.Equal(out[oi:oi+n], record[ri:e.start]) {
			return errors.New("renamed record changed bytes outside the patient names")
		}
		oi += n
		v := jsonString(e.value)
		if oi+len(v) > len(out) || !bytes.Equal(out[oi:oi+len(v)], v) {
			return errors.New("renamed record does not hold the declared name")
		}
		oi += len(v)
		ri = e.end
	}
	if !bytes.Equal(out[oi:], record[ri:]) {
		return errors.New("renamed record changed bytes outside the patient names")
	}
	want, err := decodeGenericJSON(record)
	if err != nil {
		return err
	}
	for _, e := range edits {
		if !setAt(want, e.path, e.value) {
			return errors.New("a declared patient name has no place in the record")
		}
	}
	got, err := decodeGenericJSON(out)
	if err != nil {
		return err
	}
	if !sameJSONValue(got, want) {
		return errors.New("renamed record differs from the record beyond the patient names")
	}
	return nil
}

// sameJSONValue reports whether two decoded JSON values (UseNumber) are equal:
// same members, same elements in order, same lexemes.
func sameJSONValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameJSONValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameJSONValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

func decodeGenericJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// setAt sets the string at path in v, reporting whether a string was there.
func setAt(v any, path []any, value string) bool {
	if len(path) == 0 {
		return false
	}
	for _, step := range path[:len(path)-1] {
		switch s := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return false
			}
			v = m[s]
		case int:
			a, ok := v.([]any)
			if !ok || s >= len(a) {
				return false
			}
			v = a[s]
		}
	}
	m, ok := v.(map[string]any)
	key, isKey := path[len(path)-1].(string)
	if !ok || !isKey {
		return false
	}
	if _, isString := m[key].(string); !isString {
		return false
	}
	m[key] = value
	return true
}

// draftOrderContext returns the member's one draft order — a DeviceRequest or
// ServiceRequest whose status is draft — found by the system of record's
// bounded patient search and returned exactly as the system holds it. The
// network never changes an order's status. A non-zero status refuses:
//   - 422 when the connector cannot search, or several draft orders exist;
//   - 502 when there is no draft order, or it carries no product coding, or
//     the search answer is unusable;
//   - 503 when the system of record is unavailable.
func (g *Gateway) draftOrderContext(ctx context.Context, member string) ([]byte, int, string) {
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	if !found {
		return nil, http.StatusUnprocessableEntity, "patient not found in system of record"
	}
	sorID, ok := strings.CutPrefix(ref, "Patient/")
	if !ok || !fhirIDRE.MatchString(sorID) {
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	var drafts [][]byte
	for _, rt := range []string{"DeviceRequest", "ServiceRequest"} {
		s := runSoRSearch(ctx, g.cfg.SoR, rt, sorID, false)
		switch s.Outcome {
		case SearchOK:
			for _, m := range s.matches {
				rec := s.pages[m.page][m.start:m.end]
				var head struct {
					Status string `json:"status"`
				}
				if decodeMessage(rec, &head) == nil && head.Status == "draft" {
					drafts = append(drafts, rec)
				}
			}
		case SearchZero:
		case SearchUnsupported:
			return nil, http.StatusUnprocessableEntity, "the system of record cannot search for the member's draft order"
		case SearchUnavailable:
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRUnavailable})
			return nil, status, msg
		default:
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, status, msg
		}
	}
	switch len(drafts) {
	case 0:
		return nil, http.StatusBadGateway, "no draft order for member in system of record"
	case 1:
	default:
		return nil, http.StatusUnprocessableEntity, "several draft orders for member in system of record"
	}
	if _, _, _, err := shnsdk.ParseOrderProductCoding(drafts[0]); err != nil {
		return nil, http.StatusBadGateway, "draft order has no recognized product coding"
	}
	return append([]byte(nil), drafts[0]...), 0, ""
}
