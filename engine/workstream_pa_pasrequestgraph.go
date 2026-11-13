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
// the request names Patient/<member>. Each such record's root
// subject.reference is re-pointed to Patient/<member> by registered edit E-06
// (rekeyEvidenceSubject), exactly as the supplemental report's is. A record
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
		named, edited, err := rekeyEvidenceSubject(raw, "Patient/"+sorID, member)
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
			return nil, false, recordNamesNoSubject(ref)
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
// build (errRecordNotCarried: 422, recorded Rule fidelity at the build stage,
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

// errEvidenceBound is a record read for the request that nests past the
// request graph's own bound (pasgraph.MaxDepth), or past the relay's limits on
// a body the gateway transmits, at its depth in the request: the gateway's own
// limit,
// refused as the completion refuses any other graph it will not read, never
// blamed on the participant's system.
var errEvidenceBound = errors.New("PAS evidence record past the request graph's bound")

// heldRecordFault decides raw, the system's answer to a read of ref
// (<type>/<id>), before its subject is read, the same on every path (the
// E-06 wrapper and the completion's own check): nil when it is that resource
// as bytes the build can carry; recordUnreadable when it is not a JSON object,
// cannot be decoded, or is another resourceType or id; errRecordNotCarried,
// naming ref, when its bytes cannot be carried as read
// (uncarriableRecordBytes: the splice's own rule, the fidelity of the build);
// and errEvidenceBound past the graph's own bound or the relay's limits on the
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
			return errEvidenceBound
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
			return errEvidenceBound
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
func (g *Gateway) completeAuthoredPASRequest(ctx context.Context, body, sourceQR, sourceOrder []byte, coverageRef string, sorID, member string) ([]byte, []relay.EditID, error) {
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
	var note inputEditNote
	out, err := retainPASRequestEvidenceWithPolicy(ctx, body, evidenceReadByMember(ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext, sorID, member, &note), policy)
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
	refs := 0
	total := len(body)
	had := len(entries)
	// added is each record read from the participant's system, as read: it
	// is spliced into the request's bytes, which stay as built.
	var added []bundleEntryBytes
	for i := 0; i < len(entries); i++ {
		entry := entries[i].(map[string]any)
		owner := graph.ByURL[entry["fullUrl"].(string)]
		var selected []string
		var walk func(any, int, string) bool
		walk = func(v any, depth int, path string) bool {
			if depth > pasGraphMaxDepth {
				return false
			}
			switch x := v.(type) {
			case map[string]any:
				if ref, exists := x["reference"]; exists {
					s, ok := ref.(string)
					refs++
					if !ok || refs > pasGraphMaxReferences {
						return false
					}
					if !policy.Allows(owner, path, x) {
						selected = append(selected, s)
					}
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
			return fail()
		}
		for _, ref := range selected {
			if strings.HasPrefix(ref, "#") {
				continue
			}
			if graph.Resolve(ref, owner, nil, false) == "" {
				continue
			}
			// Only unversioned relative clinical identities are eligible for local lookup.
			// Absolute references stay exact and are never reduced to a different authority.
			parts := strings.Split(ref, "/")
			if len(parts) != 2 || !pasSafeResourceID(parts[1]) {
				return fail()
			}
			switch parts[0] {
			case "Condition", "ClinicalImpression", "Goal", "Observation", "DiagnosticReport":
			default:
				return fail()
			}
			base := strings.TrimSuffix(owner.FullURL, "/"+owner.Resource["resourceType"].(string)+"/"+owner.Resource["id"].(string))
			full := base + "/" + ref
			if len(entries) >= pasGraphMaxResources {
				return fail()
			}
			if err := ctx.Err(); err != nil {
				return nil, safeSoRError(err)
			}
			raw, found, err := read(ctx, ref)
			var signed *relay.SignedContentError
			if errors.As(err, &signed) {
				// E-06 would change a record a signature covers (D113).
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
			if errors.Is(err, errEvidenceBound) {
				return fail()
			}
			if err != nil {
				return nil, safeSoRError(err)
			}
			if !found {
				// A definite absence, never a read that failed to answer
				// (that is err, above): the build cannot fill it.
				return nil, recordNotHeld(ref)
			}
			// The record is decided before the request's size bound, in the
			// order the E-06 wrapper decides it, so a record's answer does not
			// depend on which id the system holds the member under.
			switch err := heldRecordFault(raw, ref); {
			case errors.Is(err, errEvidenceBound):
				return fail()
			case err != nil:
				return nil, err
			}
			var r map[string]any
			if decodePASObject(raw, &r) != nil {
				return nil, recordUnreadable(ref)
			}
			subject, ok := r["subject"].(map[string]any)
			if !ok {
				return nil, recordNamesNoSubject(ref)
			}
			s, ok := subject["reference"].(string)
			if !ok {
				return nil, recordNamesNoSubject(ref)
			}
			if s != patient && base+"/"+s != patient {
				// Not the request's patient's record: the fidelity
				// of the build, whatever the request's legs before it.
				return nil, notThePatientsRecord(ref)
			}
			total += len(raw)
			if total > pasGraphMaxBytes {
				return fail()
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
		}
	}
	graph.Entries = entries
	bundle["entry"] = entries
	if graph.ValidateWithReferencePolicy(policy) != nil || !consistentPASGraphSubjects(graph, patient) {
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
	if err != nil || len(result) > pasGraphMaxBytes {
		return fail()
	}
	return result, nil
}
