//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// completePASRequest retains only clinical evidence selected by the request. Reads
// use the configured holder-local SoR authority, never a reference URL transport,
// and each record read is named as the build names the patient
// (evidenceReadByMember). The complete graph then follows the existing transform
// and validation path.
//
// edits are the registered input edits made to a record the completed request
// carries (E-06, only when it changed a record), for the request's seal
// (moduleBuild.InputEdits); nil when none was made.
func (g *Gateway) completePASRequest(ctx context.Context, body []byte, sorID, member string) ([]byte, []relay.EditID, error) {
	if g.cfg.OriginationProfile != "provider-data" {
		return body, nil, nil
	}
	if g.cfg.SoR == nil {
		return nil, nil, errors.New("PAS evidence unavailable")
	}
	var note inputEditNote
	out, err := retainPASRequestEvidence(ctx, body, evidenceReadByMember(ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext, sorID, member, &note))
	if err != nil {
		return nil, nil, err
	}
	return out, note.list(), nil
}

// inputEditNote collects the registered input edits (relay.InputEdited) a
// build's reads made, each once, in the order made: only an edit that changed
// a record the build then carries.
type inputEditNote struct{ edits []relay.EditID }

// made notes id; a nil note notes nothing.
func (n *inputEditNote) made(id relay.EditID) {
	if n != nil && !slices.Contains(n.edits, id) {
		n.edits = append(n.edits, id)
	}
}

// list is the edits noted, nil when none.
func (n *inputEditNote) list() []relay.EditID {
	if n == nil || len(n.edits) == 0 {
		return nil
	}
	return slices.Clone(n.edits)
}

// evidencePrepareFailed reports whether err is this gateway's own failure to
// prepare an evidence record it read (rekeyEvidenceSubject), answered as the
// gateway's own failure, never as the record's.
func evidencePrepareFailed(err error) bool { return errors.Is(err, errEvidencePrepare) }

// evidenceReadByMember is read with each record it answers named as the PAS
// build names the patient. The system of record holds the member's Patient
// under sorID and the build names that patient member (patientRef), so a
// clinical record the system answers about them names Patient/<sorID> where
// the request names Patient/<member>. Each such record's patient reference,
// at the member E-06's registry names for its type (evidencePatientElement:
// a NutritionOrder's patient, every other registered type's subject), is
// re-pointed to Patient/<member> by registered edit E-06
// (rekeyEvidenceSubject), exactly as the supplemental report's is. A record of
// a type outside the registry is never re-keyed: it is answered as held. A record
// whose subject names anything else (another patient, Patient/<member> under
// another record id, a versioned or absolute reference, a Group or any other
// non-Patient reference, a contained or urn reference) is refused as a record
// the build cannot carry (notThePatientsRecord, the fidelity of the gateway's
// own edit): E-06 re-points only the exact record id, and cannot faithfully
// make any other subject the request's patient. A record naming no
// subject.reference is refused the same way (recordNamesNoSubject): a record
// that does not say whose it is cannot be filled as this patient's. An answer
// that is not the resource the reference names is the system's invalid answer
// (recordUnreadable), and a record the system does not hold is answered as
// not held, which the completion refuses (recordNotHeld). Each is decided
// here, before E-06, so the answer does
// not depend on which id the system holds the member under. One a signature
// covers is refused with its *relay.SignedContentError. Contained resources
// are not edited. With sorID equal to member, or unknown (""),
// nothing is edited and the completion's own subject check decides. Each
// record E-06 changed is noted on note, so the request that carries it names
// the edit (relay.InputEdited); a record already naming the member notes
// nothing.
func evidenceReadByMember(read func(context.Context, string) ([]byte, bool, error), sorID, member string, note *inputEditNote) func(context.Context, string) ([]byte, bool, error) {
	if sorID == "" || sorID == member {
		return read
	}
	return func(ctx context.Context, ref string) ([]byte, bool, error) {
		raw, found, err := read(ctx, ref)
		if err != nil || !found {
			return raw, found, err
		}
		if err := heldRecordFault(raw, ref); err != nil {
			return nil, false, err
		}
		// E-06 re-keys only at the element its registry names for the
		// record's type; a type outside the registry is never re-keyed.
		typ, _, _ := strings.Cut(ref, "/")
		element, registered := evidencePatientElement[typ]
		if !registered {
			return raw, true, nil
		}
		named, edited, err := rekeyEvidenceSubject(raw, "Patient/"+sorID, member, element)
		var signed *relay.SignedContentError
		switch {
		case errors.As(err, &signed):
			return nil, false, signed
		case errors.Is(err, errEvidenceOtherSubject):
			// A decided answer, never "not held": the record's subject is
			// not the request's patient as the build can name it.
			return nil, false, notThePatientsRecord(ref)
		case errors.Is(err, errEvidenceNoSubject):
			// Decided too: absent is not a match, and not "not held".
			return nil, false, recordNamesNoPatient(ref, patientElementOf(ref))
		case errors.Is(err, errEvidenceUnreadable):
			// heldRecordFault read it, under the relay's limits, as the
			// re-point reads it: not reached unless the two disagree.
			return nil, false, recordUnreadable(ref)
		case errors.Is(err, errEvidencePrepare):
			// This gateway's own failure to prepare the record, never the
			// record's: answered as such, not as evidence it cannot carry.
			return nil, false, err
		case err != nil:
			// rekeyEvidenceSubject answers only the errors above.
			return nil, false, fmt.Errorf("%w: %v", errEvidencePrepare, err)
		}
		if edited {
			note.made(relay.EditEvidenceSubjectRekey)
		}
		return named, true, nil
	}
}

// notThePatientsRecord refuses ref, a clinical record the participant's
// system answered whose subject is not exactly the request's patient (another
// patient, or a reference form E-06 does not re-point): the build will not
// carry it as this patient's evidence. It is the fidelity of the gateway's own
// build (errRecordNotCarried: 502, recorded Rule fidelity at the build stage,
// at every conformance level), the same refusal whichever request or
// resume reaches it.
func notThePatientsRecord(ref string) error {
	return fmt.Errorf("%w: %s's subject is not exactly the request's patient", errRecordNotCarried, ref)
}

// recordNamesNoSubject refuses ref, a clinical record the participant's system
// answered that names no subject.reference (no subject, or one with no string
// reference): a record that does not say whose it is cannot be filled as this
// patient's evidence. It is the fidelity of the gateway's own build, as
// notThePatientsRecord is, and agrees with the supplemental report's refusal
// of the same shape.
func recordNamesNoSubject(ref string) error {
	return fmt.Errorf("%w: %s names no subject.reference", errRecordNotCarried, ref)
}

// evidenceNotHeldError refuses ref, a clinical record the request references
// that the participant's system definitely does not hold (its read answered
// absence: 404 or 410, not a read that failed to answer): the build fills
// evidence only from the participant's own system, and refuses when that
// system cannot supply it. It is the fidelity of the gateway's own build, as
// errRecordNotCarried is (errors.Is reports it as one), with its own message.
type evidenceNotHeldError struct{ ref string }

func (e *evidenceNotHeldError) Error() string {
	return "a record the request references cannot be filled: " + e.ref + " is not held by the system of record"
}

func (e *evidenceNotHeldError) Is(target error) bool { return target == errRecordNotCarried }

func recordNotHeld(ref string) error { return &evidenceNotHeldError{ref: ref} }

// evidencePatientElement is, for each type registered edit E-06 re-keys, the
// member naming the record's patient (FHIR R4: a NutritionOrder's patient,
// every other registered type's subject), read from E-06's registered places
// ("<Type>.<element>.reference", package paedits), so the registry is the one
// list. A type outside it is never re-keyed.
var evidencePatientElement = func() map[string]string {
	out := map[string]string{}
	for _, e := range paRelayEdits {
		if e.ID != relay.EditEvidenceSubjectRekey {
			continue
		}
		for _, place := range e.Paths {
			if parts := strings.Split(place, "."); len(parts) == 3 && parts[2] == "reference" {
				out[parts[0]] = parts[1]
			}
		}
	}
	return out
}()

// patientElementOf is the member naming the patient of the record ref
// (<type>/<id>) names, by E-06's registry: "subject" for a type outside it.
func patientElementOf(ref string) string {
	typ, _, _ := strings.Cut(ref, "/")
	if element, ok := evidencePatientElement[typ]; ok {
		return element
	}
	return "subject"
}

// qrContextTypes are the types a QR's qr-context may name that a request
// can carry: each is in E-06's registry (evidencePatientElement), which names
// the member its patient is in, read by structure, never by text. A type
// outside it is not read for a context.
var qrContextTypes = map[string]bool{
	"ServiceRequest": true, "DeviceRequest": true, "MedicationRequest": true, "NutritionOrder": true, "Encounter": true,
}

// recordNamesNoPatient refuses ref, a record whose patient member (element:
// subject, or a NutritionOrder's patient) names no reference: as
// recordNamesNoSubject, which it is for subject.
func recordNamesNoPatient(ref, element string) error {
	if element == "subject" {
		return recordNamesNoSubject(ref)
	}
	return fmt.Errorf("%w: %s names no %s.reference", errRecordNotCarried, ref, element)
}

// contextNotHeldError refuses ref, a record the provider's
// QuestionnaireResponse names in a qr-context that the participant's system
// definitely does not hold. It says what evidenceNotHeldError says, but what
// is missing is what the gateway read from its own participant's system (its
// $populate answer named the record), not what the caller's request named:
// so it is not an evidenceNotHeldError, and buildFidelityStatus answers it
// errRecordNotCarried's 502 where a record the request names is 422. The two
// are told apart by where the reference came from, a qr-context occurrence
// the policy names, never by text.
type contextNotHeldError struct{ ref string }

func (e *contextNotHeldError) Error() string {
	return "a record the request references cannot be filled: " + e.ref + " is not held by the system of record"
}

func (e *contextNotHeldError) Is(target error) bool { return target == errRecordNotCarried }

func contextNotHeld(ref string) error { return &contextNotHeldError{ref: ref} }

// contextReferenceNotCarried refuses owner's reference to ref, made by the
// QR (a context of a type no request carries) or by a record read for one of
// its contexts (a reference outside the records a context carries): the
// fidelity of the gateway's own build, 502.
func contextReferenceNotCarried(owner, ref string) error {
	return fmt.Errorf("%w: %s references %s, which a prior-authorization request this gateway builds cannot carry", errRecordNotCarried, owner, ref)
}

// appointmentContextNotCarried refuses an Appointment a QR names in a
// qr-context: an Appointment names its patient as one of its participants,
// not in a member of its own, so the build cannot bind it to the request's
// patient yet. The fidelity of the gateway's own build, 502.
func appointmentContextNotCarried(ref string) error {
	return fmt.Errorf("%w: %s is an Appointment, which a prior-authorization request this gateway builds cannot carry yet: an Appointment names its patient as one of its participants", errRecordNotCarried, ref)
}

// contextRequesterRead reads the records the requester of record (a QR
// context's record, as contextRequesterView gives it) leads to, by the
// requester rule of the request's builder: the records, or why they cannot
// be carried (fidelity, the reason), or the system's failure to answer
// (readErr).
type contextRequesterRead func(ctx context.Context, record []byte) (records [][]byte, fidelity string, readErr error)

// contextRequesterView is raw, a QR context's record of type typ (decoded,
// r), as the requester rule reads it: the request types name the clinician
// who ordered in requester, a NutritionOrder in orderer (FHIR R4). An
// Encounter names none; the records its participants, service provider or
// location name are not carried, so a context Encounter referencing one is
// refused by name.
func contextRequesterView(typ string, r map[string]any, raw []byte) []byte {
	if typ != "NutritionOrder" {
		return raw
	}
	view, err := json.Marshal(map[string]any{"requester": r["orderer"]})
	if err != nil {
		return raw
	}
	return view
}

// unresolvedContext is the first of the QR's contexts policy names that the
// request, as built, does not carry: one that resolves to an entry already
// is no record to read. ok is false when every one resolves.
func unresolvedContext(body []byte, policy *authoredPASReferencePolicy) (string, bool) {
	var bundle map[string]any
	if decodePASObject(body, &bundle) != nil {
		return "", false
	}
	entries, _ := bundle["entry"].([]any)
	graph := &pasGraph{Bundle: bundle, Entries: entries, ByURL: map[string]*pasGraphEntry{}}
	for i, raw := range entries {
		e, _ := raw.(map[string]any)
		full, _ := e["fullUrl"].(string)
		r, _ := e["resource"].(map[string]any)
		if full != "" && r != nil {
			graph.ByURL[full] = &pasGraphEntry{FullURL: full, Resource: r, Index: i}
		}
	}
	owner := graph.ByURL[policy.owner]
	for _, ref := range policy.named {
		if owner == nil || graph.Resolve(ref, owner, nil, false) != "" {
			return ref, true
		}
	}
	return "", false
}

// contextNotReadOnProfile refuses ref, a QR context on an origination profile
// that reads no orders from a system of record (demo): the fidelity of the
// gateway's own build, 502.
func contextNotReadOnProfile(ref string) error {
	return fmt.Errorf("%w: %s cannot be filled: this gateway's demo profile reads no orders from a system of record", errRecordNotCarried, ref)
}

// contextRequesterNotCarried refuses ref, a record read for a QR context,
// whose requester's records cannot be carried (the requester rule's own
// refusal, why): the fidelity of the gateway's own build, 502, naming ref.
func contextRequesterNotCarried(ref, why string) error {
	return fmt.Errorf("%w: %s: %s", errRecordNotCarried, ref, why)
}

// evidenceUnreadableError is the participant's system answering a read of a
// clinical record with something other than that resource: not a JSON
// resource, or one whose resourceType or id is not the reference's. It is the
// system's invalid answer (a *SoRReadError of kind SoRInvalidResponse),
// answered 502 naming the record.
type evidenceUnreadableError struct{ ref string }

func (e *evidenceUnreadableError) Error() string {
	return e.ref + " as the system answered it is not that resource"
}

func (e *evidenceUnreadableError) Unwrap() error { return &SoRReadError{Kind: SoRInvalidResponse} }

func recordUnreadable(ref string) error { return &evidenceUnreadableError{ref: ref} }

// evidencePastLimitError refuses ref, a record read from the participant's
// system that the gateway's own build cannot carry within the gateway's own
// carrying limits: it nests past the request graph's bound (pasgraph.MaxDepth),
// it is past the relay's limits on a body the gateway transmits at its depth in
// the request, or it takes the request past the graph's size, reference or
// resource bounds, or the relay's limits on the request as a whole. It is the fidelity of the gateway's own build (rule fidelity, stage
// build, at every conformance level), answered 502 since what cannot be carried
// is what the gateway read from its own participant's system
// (evidenceFidelityRefusal), never under no named rule and never 500. The
// reason names the record and the limit, and says a retry cannot succeed until
// the record changes: it carries no Retry-After.
type evidencePastLimitError struct{ ref, limit string }

func (e *evidencePastLimitError) Error() string {
	return "PAS evidence " + e.ref + " read from the provider's system is past the gateway's own carrying limits: it " + e.limit + "; the request cannot be sent until that record changes"
}

func recordPastLimit(ref, limit string) error { return &evidencePastLimitError{ref: ref, limit: limit} }

// The limits a record read can take the request past, as a refusal names them.
var (
	limitGraphDepth     = fmt.Sprintf("nests deeper than the request graph's %d levels", pasGraphMaxDepth)
	limitGraphBytes     = fmt.Sprintf("takes the request past its %d MiB bound", pasGraphMaxBytes>>20)
	limitGraphRefs      = fmt.Sprintf("takes the request past its %d references", pasGraphMaxReferences)
	limitGraphResources = fmt.Sprintf("takes the request past its %d resources", pasGraphMaxResources)
)

// heldRecordFault decides raw, the system's answer to a read of ref
// (<type>/<id>), before its subject is read, the same on every path (the
// E-06 wrapper and the completion's own check): nil when it is that resource
// as bytes the build can carry; recordUnreadable when it is not a JSON object,
// cannot be decoded, or is another resourceType or id; errRecordNotCarried,
// naming ref, when its bytes cannot be carried as read
// (uncarriableRecordBytes: the splice's own rule, the fidelity of the build);
// and recordPastLimit past the graph's own bound or the relay's limits on the
// record as the request carries it: the gateway's own limits.
func heldRecordFault(raw []byte, ref string) error {
	r := bytes.Trim(raw, " \t\r\n")
	if len(r) == 0 || r[0] != '{' || !json.Valid(r) {
		return recordUnreadable(ref)
	}
	if why := uncarriableRecordBytes(r); why != "" {
		return fmt.Errorf("%w: %s %s", errRecordNotCarried, ref, why)
	}
	var m map[string]any
	if err := decodePASObject(r, &m); err != nil {
		var c *pasgraph.CauseError
		if errors.As(err, &c) && c.Cause() == pasgraph.RefusalBound {
			return recordPastLimit(ref, limitGraphDepth)
		}
		return recordUnreadable(ref)
	}
	typ, id, ok := strings.Cut(ref, "/")
	if !ok || m["resourceType"] != typ || m["id"] != id {
		return recordUnreadable(ref)
	}
	// The record as the request carries it, at its depth in the Bundle (the
	// resource of an entry of its entry array), under the relay's own limits
	// on what a gateway transmits: over one is the gateway's limit.
	nested := slices.Concat([]byte(`{"entry":[{"resource":`), r, []byte(`}]}`))
	if _, err := relay.Doc(relay.NewBody(nested, relay.OriginUpstreamResponse)); err != nil {
		var limit *relay.LimitError
		if errors.As(err, &limit) {
			return recordPastLimit(ref, limit.Limit()+" as the request carries it")
		}
		return recordUnreadable(ref)
	}
	return nil
}

func retainPASRequestEvidence(ctx context.Context, body []byte, read func(context.Context, string) ([]byte, bool, error)) ([]byte, error) {
	return retainPASRequestEvidenceWithPolicy(ctx, body, read, nil)
}

// completeAuthoredPASRequest is completePASRequest for a request whose
// questionnaire response the build links (authoredPASReferencePolicyFor),
// with the same input edits.
//
// A request on another origination profile (demo) reads no records from a
// system of record, so a QR naming a context beside the exchange's own order
// and Coverage is refused there, on the fidelity of the gateway's own build:
// nothing can supply the record the request would have to carry.
//
// requesterOf reads the records a context's requester leads to, by the
// caller's own requester rule (module 1 supplies its own); nil reads none, so
// a context naming a requester the request does not carry is refused.
func (g *Gateway) completeAuthoredPASRequest(ctx context.Context, body, sourceQR, sourceOrder []byte, coverageRef string, sorID, member string, requesterOf contextRequesterRead) ([]byte, []relay.EditID, error) {
	if len(sourceQR) != 0 && g.cfg.OriginationProfile != "provider-data" {
		if policy, err := authoredPASReferencePolicyFor(body, sourceQR, sourceOrder, coverageRef); err == nil {
			if ref, ok := unresolvedContext(body, policy); ok {
				return nil, nil, contextNotReadOnProfile(ref)
			}
		}
	}
	if len(sourceQR) == 0 || g.cfg.OriginationProfile != "provider-data" {
		return g.completePASRequest(ctx, body, sorID, member)
	}
	if g.cfg.SoR == nil {
		return nil, nil, errors.New("PAS evidence unavailable")
	}
	policy, err := authoredPASReferencePolicyFor(body, sourceQR, sourceOrder, coverageRef)
	if err != nil {
		return nil, nil, err
	}
	sor := ReadSystemOfRecord(g.cfg.SoR)
	var note inputEditNote
	// A QR context's record is read as the clinical evidence is: through
	// E-06, which re-keys it at the element its type names its patient by.
	read := evidenceReadByMember(sor.ResolveByReferenceContext, sorID, member, &note)
	policy.readContext = read
	policy.requesterOf = requesterOf
	out, err := retainPASRequestEvidenceWithPolicy(ctx, body, read, policy)
	if err != nil {
		return nil, nil, err
	}
	return out, note.list(), nil
}

func retainPASRequestEvidenceWithPolicy(ctx context.Context, body []byte, read func(context.Context, string) ([]byte, bool, error), policy *authoredPASReferencePolicy) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, errors.New("invalid or incomplete PAS request evidence") }
	if len(body) > pasGraphMaxBytes {
		return fail()
	}
	var bundle map[string]any
	if decodePASObject(body, &bundle) != nil || bundle["resourceType"] != "Bundle" || bundle["type"] != "collection" {
		return fail()
	}
	entries, ok := bundle["entry"].([]any)
	if !ok || len(entries) == 0 || len(entries) > pasGraphMaxResources {
		return fail()
	}
	graph := &pasGraph{Bundle: bundle, Entries: entries, ByURL: map[string]*pasGraphEntry{}}
	// patients are the request's Patient entries, by fullUrl. One is the
	// member; any other must be a party the member's Coverage names, a
	// dependent's parent carried as an entry of its own
	// (shnsdk.CoverageEntryParties). A record read from the system below may
	// add none (sealed).
	var patients []string
	sealed := false
	add := func(index int, v any) bool {
		e, ok := v.(map[string]any)
		if !ok {
			return false
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return false
		}
		full, ok := e["fullUrl"].(string)
		if !ok {
			return false
		}
		typ, tok := r["resourceType"].(string)
		id, iok := r["id"].(string)
		u, err := url.Parse(full)
		if err != nil || !tok || !iok || !pasSafeResourceID(id) || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) || graph.ByURL[full] != nil {
			return false
		}
		graph.ByURL[full] = &pasGraphEntry{FullURL: full, Resource: r, Index: index}
		if typ == "Patient" {
			if sealed {
				return false
			}
			patients = append(patients, full)
		}
		return true
	}
	for i, e := range entries {
		if !add(i, e) {
			return fail()
		}
	}
	sealed = true
	patient := ""
	parties := shnsdk.CoverageEntryParties(bundle)
	for _, full := range patients {
		if parties[full] {
			continue
		}
		if patient != "" {
			return fail()
		}
		patient = full
	}
	if patient == "" || !policy.Matches(graph) {
		return fail()
	}
	// contextRecords are the records read for one of the QR's contexts, by
	// fullUrl: what they reference is carried by a context's rules.
	contextRecords := map[string]bool{}
	refs := 0
	// resources counts every object naming a resourceType, contained ones
	// included, as the graph's own validation counts them.
	resources := 0
	total := len(body)
	had := len(entries)
	// added is each record read from the participant's system, as read: it
	// is spliced into the request's bytes, which stay as built.
	var added []bundleEntryBytes
	// lastRead is the last record read and added, which a request past its
	// size bound once spliced is refused naming.
	lastRead := ""
	for i := 0; i < len(entries); i++ {
		entry := entries[i].(map[string]any)
		owner := graph.ByURL[entry["fullUrl"].(string)]
		// A record read from the participant's system (an entry the
		// completion added) that takes the request past a bound of the graph
		// is refused naming it; the request as built is the gateway's own.
		fromRead := i >= had
		ownerRef := owner.Resource["resourceType"].(string) + "/" + owner.Resource["id"].(string)
		// selected is each reference the record makes, and whether it is one
		// of the QR's contexts the policy names (read as that, by its path).
		type selection struct {
			ref     string
			context bool
		}
		var selected []selection
		ownerIsContext := contextRecords[owner.FullURL]
		pastRefs, pastResources := false, false
		var walk func(any, int, string) bool
		walk = func(v any, depth int, path string) bool {
			if depth > pasGraphMaxDepth {
				return false
			}
			switch x := v.(type) {
			case map[string]any:
				if _, ok := x["resourceType"].(string); ok {
					resources++
					if resources > pasGraphMaxResources {
						pastResources = true
						return false
					}
				}
				if ref, exists := x["reference"]; exists {
					s, ok := ref.(string)
					refs++
					if ok && refs > pasGraphMaxReferences {
						pastRefs = true
					}
					if !ok || refs > pasGraphMaxReferences {
						return false
					}
					selected = append(selected, selection{s, policy.namesContext(owner, path, x)})
				}
				keys := make([]string, 0, len(x))
				for k := range x {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if !walk(x[k], depth+1, pasReferencePath(path, k)) {
						return false
					}
				}
			case []any:
				for n, c := range x {
					if !walk(c, depth+1, pasReferencePath(path, strconv.Itoa(n))) {
						return false
					}
				}
			}
			return true
		}
		if !walk(owner.Resource, 0, "") {
			if fromRead && pastRefs {
				return nil, recordPastLimit(ownerRef, limitGraphRefs)
			}
			if fromRead && pastResources {
				return nil, recordPastLimit(ownerRef, limitGraphResources)
			}
			return fail()
		}
		for _, sel := range selected {
			ref := sel.ref
			if strings.HasPrefix(ref, "#") {
				continue
			}
			if graph.Resolve(ref, owner, nil, false) == "" {
				continue
			}
			// Only unversioned relative identities are eligible for local lookup.
			// Absolute references stay exact and are never reduced to a different authority.
			parts := strings.Split(ref, "/")
			if len(parts) != 2 || !pasSafeResourceID(parts[1]) {
				if sel.context || ownerIsContext {
					return nil, contextReferenceNotCarried(ownerRef, ref)
				}
				return fail()
			}
			// patientAt is the member of the record read that names its
			// patient, by its type: a clinical record's subject, or a QR
			// context's (evidencePatientElement, the E-06 registry).
			patientAt, readRecord := "subject", read
			switch {
			case sel.context && parts[0] == "Appointment":
				return nil, appointmentContextNotCarried(ref)
			case sel.context:
				element, ok := evidencePatientElement[parts[0]]
				if !ok || !qrContextTypes[parts[0]] || policy.readContext == nil {
					return nil, contextReferenceNotCarried(ownerRef, ref)
				}
				patientAt, readRecord = element, policy.readContext
			case parts[0] == "Condition", parts[0] == "ClinicalImpression", parts[0] == "Goal", parts[0] == "Observation", parts[0] == "DiagnosticReport":
			case ownerIsContext:
				// A QR context's record carries only its requester's records
				// and clinical evidence it references; anything else it names
				// that the request does not carry cannot be carried.
				return nil, contextReferenceNotCarried(ownerRef, ref)
			default:
				return fail()
			}
			base := strings.TrimSuffix(owner.FullURL, "/"+owner.Resource["resourceType"].(string)+"/"+owner.Resource["id"].(string))
			full := base + "/" + ref
			if len(entries) >= pasGraphMaxResources {
				if fromRead {
					// The records this record read references take the
					// request past its resource bound.
					return nil, recordPastLimit(ownerRef, limitGraphResources)
				}
				return fail()
			}
			if err := ctx.Err(); err != nil {
				return nil, safeSoRError(err)
			}
			raw, found, err := readRecord(ctx, ref)
			var signed *relay.SignedContentError
			if errors.As(err, &signed) {
				// E-06 would change a record a signature covers: a fidelity
				// refusal of SHN's own edit.
				return nil, signed
			}
			if errors.Is(err, errEvidencePrepare) {
				return nil, err
			}
			if errors.Is(err, errRecordNotCarried) {
				// A record the build will not carry (notThePatientsRecord,
				// recordNamesNoSubject, heldRecordFault's bytes): the fidelity
				// of the build, never a failed read.
				return nil, err
			}
			var unreadable *evidenceUnreadableError
			if errors.As(err, &unreadable) {
				return nil, err
			}
			var past *evidencePastLimitError
			if errors.As(err, &past) {
				return nil, err
			}
			if err != nil {
				return nil, safeReadErr(err)
			}
			if !found {
				// A definite absence, never a read that failed to answer
				// (that is err, above): the build cannot fill it. A QR
				// context's absence is what the gateway read from its own
				// participant's system (the $populate answer named it), not
				// what the caller's request named.
				if sel.context {
					return nil, contextNotHeld(ref)
				}
				return nil, recordNotHeld(ref)
			}
			// The record is decided before the request's size bound, in the
			// order the E-06 wrapper decides it, so a record's answer does not
			// depend on which id the system holds the member under.
			if err := heldRecordFault(raw, ref); err != nil {
				return nil, err
			}
			var r map[string]any
			if decodePASObject(raw, &r) != nil {
				return nil, recordUnreadable(ref)
			}
			subject, ok := r[patientAt].(map[string]any)
			if !ok {
				return nil, recordNamesNoPatient(ref, patientAt)
			}
			s, ok := subject["reference"].(string)
			if !ok {
				return nil, recordNamesNoPatient(ref, patientAt)
			}
			if s != patient && base+"/"+s != patient {
				// Not the request's patient's record: the fidelity
				// of the build, whatever the request's legs before it.
				return nil, notThePatientsRecord(ref)
			}
			total += len(raw)
			if total > pasGraphMaxBytes {
				return nil, recordPastLimit(ref, limitGraphBytes)
			}
			e := map[string]any{"fullUrl": full, "resource": r}
			if !add(len(entries), e) {
				return fail()
			}
			entries = append(entries, e)
			encodedURL, err := json.Marshal(full)
			if err != nil {
				return fail()
			}
			added = append(added, bundleEntryBytes{fullURL: encodedURL, resource: raw})
			lastRead = ref
			if !sel.context {
				continue
			}
			// A QR context's record carries the records its requester leads
			// to, by the rule the exchange's own order's requester is read by
			// (contextRequesterRead); one the request already carries is
			// not read into it twice.
			contextRecords[full] = true
			if policy.requesterOf == nil {
				continue
			}
			records, fidelity, readErr := policy.requesterOf(ctx, contextRequesterView(parts[0], r, raw))
			switch {
			case fidelity != "":
				return nil, contextRequesterNotCarried(ref, fidelity)
			case readErr != nil:
				return nil, safeReadErr(readErr)
			}
			for _, rec := range records {
				var head struct {
					ResourceType string `json:"resourceType"`
					ID           string `json:"id"`
				}
				if decodeMessage(rec, &head) != nil {
					return nil, recordUnreadable(ref)
				}
				recRef := head.ResourceType + "/" + head.ID
				recFull := base + "/" + recRef
				if graph.ByURL[recFull] != nil {
					continue
				}
				if err := heldRecordFault(rec, recRef); err != nil {
					return nil, err
				}
				var rr map[string]any
				if decodePASObject(rec, &rr) != nil {
					return nil, recordUnreadable(recRef)
				}
				if len(entries) >= pasGraphMaxResources {
					return nil, recordPastLimit(ref, limitGraphResources)
				}
				total += len(rec)
				if total > pasGraphMaxBytes {
					return nil, recordPastLimit(recRef, limitGraphBytes)
				}
				re := map[string]any{"fullUrl": recFull, "resource": rr}
				if !add(len(entries), re) {
					return fail()
				}
				entries = append(entries, re)
				encodedRec, err := json.Marshal(recFull)
				if err != nil {
					return fail()
				}
				added = append(added, bundleEntryBytes{fullURL: encodedRec, resource: rec})
				lastRead = recRef
			}
		}
	}
	graph.Entries = entries
	bundle["entry"] = entries
	// Every reference now resolves, a QR's contexts included: nothing the
	// request names is left for the payer to find missing (PAS spec-13).
	if graph.Validate() != nil || !consistentPASGraphSubjects(graph, patient) {
		return fail()
	}
	// The request leaves as built, with the records read added: it is
	// never re-encoded, so a participant's record the build carried as
	// written stays so.
	result, err := spliceBundleEntries(body, had, added)
	if errors.Is(err, errRecordNotCarried) {
		// A record read that the splice cannot carry as read: the fidelity
		// of the gateway's own edit (evidenceFidelityRefusal), not a graph fault.
		return nil, err
	}
	if err == nil && len(result) > pasGraphMaxBytes && lastRead != "" {
		// The records read took the request, as spliced, past its size bound.
		return nil, recordPastLimit(lastRead, limitGraphBytes)
	}
	if err != nil || len(result) > pasGraphMaxBytes {
		return fail()
	}
	if lastRead != "" {
		// Each record read is within the relay's limits as the request
		// carries it; together they may take the request past them (its
		// tokens), which the build's own seal would refuse unnamed.
		if _, err := relay.Doc(relay.NewBody(result, relay.OriginUpstreamResponse)); err != nil {
			var limit *relay.LimitError
			if !errors.As(err, &limit) {
				return fail()
			}
			// Only when the request as built is within the relay's limits
			// do the records read take it past them; past them already, it
			// is the gateway's own build.
			if _, err := relay.Doc(relay.NewBody(body, relay.OriginUpstreamResponse)); err != nil {
				return fail()
			}
			return nil, recordPastLimit(lastRead, "takes the request past the relay's limits: the request "+limit.Limit())
		}
	}
	return result, nil
}
