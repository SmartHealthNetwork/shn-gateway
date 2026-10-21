// workstream_pa_fhirread.go — self-contained json-level FHIR subject readers for the (C)
// outbound fence (fenceResponseSubject). These were originally added to the
// LOCAL sdk/ module but the gateway's standalone Docker build resolves
// shn-sdk from the published v0.7.0 proxy (no go.work), which does not
// carry them. Relocating them here makes the gateway closure complete
// against shn-sdk v0.7.0 (Ruling: no SDK release this slice). They promote
// to shn-sdk with the LegResponder promotion.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
)

// ParseCoverageEligibilityResponsePatient extracts patient.reference from a
// CoverageEligibilityResponse JSON (e.g. "Patient/MBR-COVERED"). It errors if
// the resourceType is not CoverageEligibilityResponse or the patient reference
// is absent. Used by the (C) outbound fence (coverage-eligibility leg): the
// engine compares the returned ref to the inbound member-namespace ref so a
// connector cannot swap the patient between the request it was handed and the
// response it returned. Exported because test/adversarial uses it directly.
func ParseCoverageEligibilityResponsePatient(data []byte) (string, error) {
	var probe struct {
		ResourceType string                     `json:"resourceType"`
		Patient      map[string]json.RawMessage `json:"patient"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	if probe.ResourceType != "CoverageEligibilityResponse" {
		return "", fmt.Errorf("engine: expected CoverageEligibilityResponse, got %q", probe.ResourceType)
	}
	var ref string
	if raw, ok := probe.Patient["reference"]; ok {
		if err := json.Unmarshal(raw, &ref); err != nil {
			return "", fmt.Errorf("engine: CoverageEligibilityResponse patient.reference is not a string")
		}
	}
	if ref == "" {
		if len(probe.Patient) == 0 {
			return "", errMissingEligibilityPatient
		}
		return "", errNoEligibilityPatientRef
	}
	return ref, nil
}

// A CoverageEligibilityResponse whose patient is missing (absent, null or
// empty) cannot be read for its patient at all; one that names its patient
// otherwise than by reference (an identifier only) is readable, and its
// patient cannot be compared with a request's.
var (
	errMissingEligibilityPatient = errors.New("engine: CoverageEligibilityResponse missing patient.reference")
	errNoEligibilityPatientRef   = errors.New("engine: CoverageEligibilityResponse names its patient by no reference")
)

// ErrNoPASResponsePatient is what ParsePASResponsePatients returns for a response
// that carries NO ClaimResponse at all — the shape an inquiry that matched nothing
// legitimately answers with. It exists so that caller can tell "there is nothing to
// compare" apart from "I could not read this", and treat only the first as ordinary.
//
// It is deliberately NOT raised for a ClaimResponse that carries no patient. That
// resource is malformed — PAS puts ClaimResponse.patient at 1..1 — and an answer
// carrying one is an answer whose subject cannot be established. Widening the
// sentinel to cover it would hand the fence a decision by ABSENCE: the very shape
// that answer takes would be its exemption from being compared.
var ErrNoPASResponsePatient = errors.New("engine: PAS response carries no ClaimResponse")

// ParsePASResponsePatients returns every patient reference in a PAS response: a
// bare polling ClaimResponse carries one; a response Bundle carries a
// ClaimResponse (+ possibly other resources) — collect every ClaimResponse's
// .patient.reference. A prior-authorization inquiry's answer takes one more
// shape: at PAS 2.2.1 the operation returns a Parameters whose output parameters
// are 0..* response Bundles, so every one of THOSE Bundles' ClaimResponses is
// collected too. Reading only the first would leave the other answers' patients
// unfenced.
//
// The output parameter names it reads are the leg's own closed pair
// (pasInquiryOutputCarries) — the name the operation declares AND the name the
// Da Vinci reference payer sends. That pair is shared deliberately: this fence
// and the leg's ledger reader must see the SAME Bundles, or an answer could be
// read for a decision while its patients went uncompared. The Parameters shape
// arises only for the inquiry operation, which is why the inquiry leg's rule is
// the right one here.
//
// Errors with ErrNoPASResponsePatient if none found. json-level, no FHIR lib.
// Used by the (C) outbound fence (the PAS legs) and by test/adversarial.
// Exported because test/adversarial uses it directly.
func ParsePASResponsePatients(b []byte) ([]string, error) {
	refs, sawResponse, err := parsePASResponsePatients(b)
	switch {
	case err != nil:
		return nil, err
	case !sawResponse:
		return nil, ErrNoPASResponsePatient
	case len(refs) == 0:
		// A ClaimResponse is here and states no patient. PAS puts that element at
		// 1..1, so this is a malformed answer, and saying so is what keeps the
		// fence from treating an unreadable subject as no subject.
		return nil, fmt.Errorf("engine: PAS response ClaimResponse states no patient reference")
	}
	return refs, nil
}

// parsePASResponsePatients reports the patient references AND whether the response
// carried a ClaimResponse at all, which is what separates the two failures above.
func parsePASResponsePatients(b []byte) (refs []string, sawResponse bool, err error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Patient      struct {
			Reference string `json:"reference"`
		} `json:"patient"`
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				Patient      struct {
					Reference string `json:"reference"`
				} `json:"patient"`
			} `json:"resource"`
		} `json:"entry"`
		Parameter []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := decodeMessage(b, &probe); err != nil {
		return nil, false, fmt.Errorf("engine: parse PAS response: %w", err)
	}
	switch probe.ResourceType {
	case "ClaimResponse":
		sawResponse = true
		if probe.Patient.Reference != "" {
			refs = append(refs, probe.Patient.Reference)
		}
	case "Bundle":
		for _, e := range probe.Entry {
			if e.Resource.ResourceType != "ClaimResponse" {
				continue
			}
			sawResponse = true
			if e.Resource.Patient.Reference != "" {
				refs = append(refs, e.Resource.Patient.Reference)
			}
		}
	case "Parameters":
		for _, p := range probe.Parameter {
			carries, _ := pasInquiryOutputCarries(p.Name)
			if !carries || len(p.Resource) == 0 {
				continue
			}
			inner, innerSaw, err := parsePASResponsePatients(p.Resource)
			if err != nil {
				return nil, sawResponse, err
			}
			sawResponse = sawResponse || innerSaw
			refs = append(refs, inner...)
		}
	}
	return refs, sawResponse, nil
}

// questionnaireHasSubject reports whether a Questionnaire JSON carries any
// subject element. A DTR Questionnaire is patient-agnostic (the FHIR
// Questionnaire resource has no subject); the gateway (C) fence rejects one
// that unexpectedly names a subject. Unexported: only fenceResponseSubject uses
// it (fence_test.go drives it indirectly via fenceResponseSubject).
func questionnaireHasSubject(b []byte) bool {
	var probe struct {
		Subject *json.RawMessage `json:"subject"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false // malformed → no valid subject; egress-$validate catches shape
	}
	return probe.Subject != nil
}

// packageQuestionnaireHasSubject reports whether ANY Questionnaire entry in a
// $questionnaire-package collection Bundle carries a subject. The DTR-fetch leg
// response is now a package Bundle; the bare-resource questionnaireHasSubject
// would probe the Bundle wrapper (which has no subject) and silently pass, so the
// (C) subject fence must walk the package's Questionnaire entries. A partner could
// include several Questionnaires, so it checks every one. Unexported: only
// fenceResponseSubject uses it.
//
// unwrapQuestionnairePackage is called first so that br-payer's Parameters wrapper
// (dtr-qpackage-output-parameters) is normalised to its inner Bundle before the walk.
// Without this, the fence would see no entries in the Parameters wrapper and return
// false — silently becoming vacuous against br-payer's real response shape.
func packageQuestionnaireHasSubject(b []byte) bool {
	b = unwrapQuestionnairePackage(b)
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		return false // malformed → no valid subject; egress-$validate catches shape
	}
	for _, e := range bundle.Entry {
		var probe struct {
			ResourceType string `json:"resourceType"`
		}
		if err := json.Unmarshal(e.Resource, &probe); err != nil {
			continue
		}
		if probe.ResourceType == "Questionnaire" && questionnaireHasSubject(e.Resource) {
			return true
		}
	}
	return false
}

// consistentPASResponseSubjects checks the entire retained graph against its own
// ClaimResponse patient, independently of the authorized request's namespace.
func consistentPASResponseSubjects(raw []byte) bool {
	return pasResponseSubjectMismatch(raw) == nil
}

// pasSubjectRefusal is why a payer answer's subjects do not all bind to its
// ClaimResponse's patient: which entry and element named which subject, as
// written, what it resolved to, and what the ClaimResponse's patient is. Why is
// the cause as the refusal states it; the fields beside it are the same facts
// for a log record. No payer bytes beyond the reference and the identities.
type pasSubjectRefusal struct {
	Owner     string `json:"owner,omitempty"`     // "entry 3 (Coverage urn:uuid:…)"
	Path      string `json:"path,omitempty"`      // "/beneficiary"
	Reference string `json:"reference,omitempty"` // the reference, as the payer wrote it
	Identity  string `json:"identity,omitempty"`  // what it resolves to, when it does
	Expected  string `json:"expected,omitempty"`  // the ClaimResponse's patient identity
	Why       string `json:"why"`
}

func (r *pasSubjectRefusal) Error() string { return r.Why }

// pasResponseSubjectMismatch is why the answer's subjects do not all bind to its
// ClaimResponse's patient — nil when they do. A graph that does not close is
// stated with the closure walk's own wording; a ClaimResponse without a patient
// reference (absent, or an empty string) is refused at that guard.
func pasResponseSubjectMismatch(raw []byte) *pasSubjectRefusal {
	return pasAnswerSubjectMismatch(raw, "")
}

// pasAnswerSubjectMismatch is pasResponseSubjectMismatch for an answer
// exchanged at IG line. The graph is read by that line's own rule
// (pasGraph.ValidateAnswerAt) first, the same rule answer.shape reads it by, so a graph
// the line lets go unresolved is never refused here as a patient's. At 2.1.0,
// which lets an answer return anything the request sent as a reference only,
// a subject no entry resolves binds by the patient its Patient/<id> names, and
// the Bundle need not carry the ClaimResponse's patient; a Patient it does
// carry must still be that patient.
func pasAnswerSubjectMismatch(raw []byte, line string) *pasSubjectRefusal {
	g, err := readPASGraph(raw)
	if err == nil {
		err = g.ValidateAnswerAt(line)
		g.OpenSubjects = line == "2.1"
	}
	if err != nil {
		r := &pasSubjectRefusal{Why: "the response graph does not close: " + strings.TrimPrefix(strings.TrimPrefix(err.Error(), "engine: "), "PAS response graph: ")}
		if gr := pasGraphRefusalOf(err); gr != nil {
			r.Owner, r.Path, r.Reference = gr.Owner, gr.Path, gr.Reference
		}
		return r
	}
	patient, _ := g.Response.Resource["patient"].(map[string]any)
	ref, _ := patient["reference"].(string)
	if ref == "" {
		return &pasSubjectRefusal{Owner: g.Response.Label(), Path: "/patient", Why: g.Response.Label() + " names no patient by reference"}
	}
	expected, why := g.SubjectIdentity(g.Response, ref)
	if why != "" && g.OpenSubjects {
		if tail := pasgraph.PatientTail(ref); tail != "" && g.NamesNoEntry(tail) {
			expected, why = tail, ""
		}
	}
	if why == "" && expected == "" {
		why = "names no patient identity"
	}
	if why != "" {
		return &pasSubjectRefusal{Owner: g.Response.Label(), Path: "/patient", Reference: ref, Why: fmt.Sprintf("%s /patient %q %s", g.Response.Label(), ref, why)}
	}
	return pasGraphSubjectMismatch(g, expected)
}

// consistentPASGraphSubjects binds every primary subject in a validated graph,
// including contained evidence, before either request or response exchange.
func consistentPASGraphSubjects(g *pasGraph, expected string) bool {
	return pasGraphSubjectMismatch(g, expected) == nil
}

// pasGraphSubjectMismatch is the first subject in g, in Bundle order, that does
// not bind to expected — nil when every subject does and a Patient entry is that
// patient. An entry-level Coverage's party (pasgraph.EntryCoverageParties) is another
// person: its identity is not checked, and only that Coverage's subscriber or
// policyHolder naming it is exempt from binding as a typed Patient reference.
// A subject under an entry identified by a URN is read by the same identity
// rule as the closure walk (Graph.SubjectIdentity), so a ClaimResponse that
// names its patient "Patient/x" from under a urn:uuid binds to the Bundle's one
// Patient whose RESTful identity ends in it.
func pasGraphSubjectMismatch(g *pasGraph, expected string) *pasSubjectRefusal {
	if expected == "" {
		return &pasSubjectRefusal{Why: "no patient identity to bind to"}
	}
	// R4 primary subject references use these field names across resources:
	// patient/subject, Coverage.beneficiary, Task.for, and subject[x]'s Reference choice. ResearchSubject.individual
	// and EnrollmentRequest.candidate are resource-specific roles; an Encounter
	// participant.individual may instead identify a clinician. Apply the
	// rule to every object, including contained resources and nested elements;
	// an unfamiliar resource type cannot silently exempt an explicit subject.
	subjectFields := map[string]bool{"patient": true, "subject": true, "beneficiary": true, "for": true, "subjectReference": true, "patientReference": true}
	var boundReference func(any, *pasGraphEntry, string) *pasSubjectRefusal
	boundReference = func(value any, owner *pasGraphEntry, path string) *pasSubjectRefusal {
		if refs, ok := value.([]any); ok {
			if len(refs) == 0 {
				return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Expected: expected, Why: owner.Label() + " " + path + " names no subject"}
			}
			for i, ref := range refs {
				if r := boundReference(ref, owner, pasReferencePath(path, strconv.Itoa(i))); r != nil {
					return r
				}
			}
			return nil
		}
		ref, ok := value.(map[string]any)
		if !ok {
			return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Expected: expected, Why: owner.Label() + " " + path + " is not a reference"}
		}
		literal, ok := ref["reference"].(string)
		// Identifier-only subjects have no exact graph identity to bind.
		if !ok || literal == "" {
			return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Expected: expected, Why: owner.Label() + " " + path + " names a subject without a reference"}
		}
		identity, why := g.SubjectIdentity(owner, literal)
		if why != "" && g.OpenSubjects {
			if tail := pasgraph.PatientTail(literal); tail != "" && g.NamesNoEntry(tail) {
				if tail == pasgraph.PatientTail(expected) {
					return nil
				}
				return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Reference: literal, Expected: expected, Why: fmt.Sprintf("%s %s names %q, which no entry of the Bundle resolves and which is not the ClaimResponse's patient %s", owner.Label(), path, literal, expected)}
			}
		}
		if why != "" {
			return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Reference: literal, Expected: expected, Why: fmt.Sprintf("%s %s names %q, which %s", owner.Label(), path, literal, why)}
		}
		if !g.SameSubject(identity, expected) {
			return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Reference: literal, Identity: identity, Expected: expected, Why: fmt.Sprintf("%s %s names %q (%s), which is not the ClaimResponse's patient %s", owner.Label(), path, literal, identity, expected)}
		}
		return nil
	}

	found := false
	// role is what v is to an entry-level Coverage's party rule
	// (pasgraph.EntryCoverageParties): a party is another person, so its identity is
	// not checked (the rule itself requires that it holds as a contained
	// resource); the slot naming it is the one typed Patient reference that
	// names someone other than the patient.
	var visit func(v any, owner *pasGraphEntry, depth int, path string, role pasgraph.CoveragePartyRole) *pasSubjectRefusal
	visit = func(v any, owner *pasGraphEntry, depth int, path string, role pasgraph.CoveragePartyRole) *pasSubjectRefusal {
		switch x := v.(type) {
		case map[string]any:
			rt, isResource := x["resourceType"].(string)
			if role != pasgraph.RoleCoverageParty && isResource && rt == "Patient" {
				identity := owner.FullURL
				if depth > 0 {
					id, ok := x["id"].(string)
					if !ok {
						return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Expected: expected, Why: owner.Label() + " carries a Patient without an id at " + path}
					}
					identity += "#" + id
				}
				if !g.SameSubject(identity, expected) {
					if depth == 0 {
						return &pasSubjectRefusal{Owner: owner.Label(), Identity: identity, Expected: expected, Why: fmt.Sprintf("%s is a Patient that is not the ClaimResponse's patient %s", owner.Label(), expected)}
					}
					return &pasSubjectRefusal{Owner: owner.Label(), Path: path, Identity: identity, Expected: expected, Why: fmt.Sprintf("%s carries a Patient (%s) at %s that is not the ClaimResponse's patient %s", owner.Label(), identity, path, expected)}
				}
				found = true
			}
			for _, field := range pasSortedKeys(x) {
				subjectField := subjectFields[field] || (field == "individual" && x["resourceType"] == "ResearchSubject") || (field == "candidate" && x["resourceType"] == "EnrollmentRequest")
				if subjectField {
					if r := boundReference(x[field], owner, pasReferencePath(path, field)); r != nil {
						return r
					}
				}
			}
			// A typed Patient Reference is also subject-bearing in polymorphic
			// paths (e.g. actor or extension.valueReference). Do not let an
			// identifier-only form bypass binding simply because its role differs.
			// The one exception is an entry-level Coverage's slot naming its own
			// party: that reference names another person, never the patient.
			if typ, ok := x["type"].(string); ok && (typ == "Patient" || typ == "http://hl7.org/fhir/StructureDefinition/Patient") && role != pasgraph.RoleCoveragePartySlot {
				if r := boundReference(x, owner, path); r != nil {
					return r
				}
			}
			var parties pasgraph.EntryParties
			if depth == 0 && isResource && rt == "Coverage" {
				parties = pasgraph.EntryCoverageParties(x)
			}
			for _, field := range pasSortedKeys(x) {
				fieldPath := pasReferencePath(path, field)
				if list, ok := x[field].([]any); ok && field == "contained" && parties.Any() {
					for i, c := range list {
						if r := visit(c, owner, depth+2, pasReferencePath(fieldPath, strconv.Itoa(i)), parties.RoleOf(i)); r != nil {
							return r
						}
					}
					continue
				}
				if r := visit(x[field], owner, depth+1, fieldPath, parties.SlotRole(field, x[field])); r != nil {
					return r
				}
			}
		case []any:
			for i, child := range x {
				if r := visit(child, owner, depth+1, pasReferencePath(path, strconv.Itoa(i)), pasgraph.RoleNone); r != nil {
					return r
				}
			}
		}
		return nil
	}
	for _, entry := range g.Ordered() {
		if r := visit(entry.Resource, entry, 0, "", pasgraph.RoleNone); r != nil {
			return r
		}
	}
	if !found && !g.OpenSubjects {
		return &pasSubjectRefusal{Expected: expected, Why: "no entry of the Bundle is the ClaimResponse's patient " + expected}
	}
	return nil
}
