// fhirread.go — self-contained json-level FHIR subject readers for the (C)
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
	"net/url"
	"strconv"
	"strings"
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

// parseEOBPatient returns an ExplanationOfBenefit's .patient.reference. It
// errors if the resourceType is not ExplanationOfBenefit or the patient
// reference is absent. json-level, no FHIR lib. Unexported: only
// fenceResponseSubject uses it.
func parseEOBPatient(b []byte) (string, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
		Patient      struct {
			Reference string `json:"reference"`
		} `json:"patient"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return "", fmt.Errorf("engine: parse ExplanationOfBenefit: %w", err)
	}
	if probe.ResourceType != "ExplanationOfBenefit" {
		return "", fmt.Errorf("engine: expected ExplanationOfBenefit, got %q", probe.ResourceType)
	}
	if probe.Patient.Reference == "" {
		return "", fmt.Errorf("engine: ExplanationOfBenefit missing patient.reference")
	}
	return probe.Patient.Reference, nil
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
	g, err := readPASGraph(raw)
	if err == nil {
		err = g.validate()
	}
	if err != nil {
		r := &pasSubjectRefusal{Why: "the response graph does not close: " + strings.TrimPrefix(strings.TrimPrefix(err.Error(), "engine: "), "PAS response graph: ")}
		if gr := pasGraphRefusalOf(err); gr != nil {
			r.Owner, r.Path, r.Reference = gr.Owner, gr.Path, gr.Reference
		}
		return r
	}
	patient, _ := g.response.resource["patient"].(map[string]any)
	ref, _ := patient["reference"].(string)
	if ref == "" {
		return &pasSubjectRefusal{Owner: g.response.label(), Path: "/patient", Why: g.response.label() + " names no patient by reference"}
	}
	expected, why := g.subjectIdentity(g.response, ref)
	if why != "" {
		return &pasSubjectRefusal{Owner: g.response.label(), Path: "/patient", Reference: ref, Why: fmt.Sprintf("%s /patient %q %s", g.response.label(), ref, why)}
	}
	return g.subjectMismatch(expected)
}

// consistentPASGraphSubjects binds every primary subject in a validated graph,
// including contained evidence, before either request or response exchange.
func consistentPASGraphSubjects(g *pasGraph, expected string) bool {
	return g.subjectMismatch(expected) == nil
}

// subjectMismatch is the first subject in the graph, in Bundle order, that does
// not bind to expected — nil when every subject does and a Patient entry is that
// patient. A subject under an entry identified by a URN is read by the same
// identity rule as the closure walk (subjectIdentity), so a ClaimResponse that
// names its patient "Patient/x" from under a urn:uuid binds to the Bundle's one
// Patient whose RESTful identity ends in it.
func (g *pasGraph) subjectMismatch(expected string) *pasSubjectRefusal {
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
				return &pasSubjectRefusal{Owner: owner.label(), Path: path, Expected: expected, Why: owner.label() + " " + path + " names no subject"}
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
			return &pasSubjectRefusal{Owner: owner.label(), Path: path, Expected: expected, Why: owner.label() + " " + path + " is not a reference"}
		}
		literal, ok := ref["reference"].(string)
		// Identifier-only subjects have no exact graph identity to bind.
		if !ok || literal == "" {
			return &pasSubjectRefusal{Owner: owner.label(), Path: path, Expected: expected, Why: owner.label() + " " + path + " names a subject without a reference"}
		}
		identity, why := g.subjectIdentity(owner, literal)
		if why != "" {
			return &pasSubjectRefusal{Owner: owner.label(), Path: path, Reference: literal, Expected: expected, Why: fmt.Sprintf("%s %s names %q, which %s", owner.label(), path, literal, why)}
		}
		if identity != expected {
			return &pasSubjectRefusal{Owner: owner.label(), Path: path, Reference: literal, Identity: identity, Expected: expected, Why: fmt.Sprintf("%s %s names %q (%s), which is not the ClaimResponse's patient %s", owner.label(), path, literal, identity, expected)}
		}
		return nil
	}

	found := false
	var visit func(any, *pasGraphEntry, int, string) *pasSubjectRefusal
	visit = func(v any, owner *pasGraphEntry, depth int, path string) *pasSubjectRefusal {
		switch x := v.(type) {
		case map[string]any:
			if typ, ok := x["resourceType"].(string); ok && typ == "Patient" {
				identity := owner.fullURL
				if depth > 0 {
					id, ok := x["id"].(string)
					if !ok {
						return &pasSubjectRefusal{Owner: owner.label(), Path: path, Expected: expected, Why: owner.label() + " carries a Patient without an id at " + path}
					}
					identity += "#" + id
				}
				if identity != expected {
					if depth == 0 {
						return &pasSubjectRefusal{Owner: owner.label(), Identity: identity, Expected: expected, Why: fmt.Sprintf("%s is a Patient that is not the ClaimResponse's patient %s", owner.label(), expected)}
					}
					return &pasSubjectRefusal{Owner: owner.label(), Path: path, Identity: identity, Expected: expected, Why: fmt.Sprintf("%s carries a Patient (%s) at %s that is not the ClaimResponse's patient %s", owner.label(), identity, path, expected)}
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
			if typ, ok := x["type"].(string); ok && (typ == "Patient" || typ == "http://hl7.org/fhir/StructureDefinition/Patient") {
				if r := boundReference(x, owner, path); r != nil {
					return r
				}
			}
			for _, field := range pasSortedKeys(x) {
				if r := visit(x[field], owner, depth+1, pasReferencePath(path, field)); r != nil {
					return r
				}
			}
		case []any:
			for i, child := range x {
				if r := visit(child, owner, depth+1, pasReferencePath(path, strconv.Itoa(i))); r != nil {
					return r
				}
			}
		}
		return nil
	}
	for _, entry := range g.ordered() {
		if r := visit(entry.resource, entry, 0, ""); r != nil {
			return r
		}
	}
	if !found {
		return &pasSubjectRefusal{Expected: expected, Why: "no entry of the Bundle is the ClaimResponse's patient " + expected}
	}
	return nil
}

// subjectIdentity is the versionless identity a subject reference denotes when
// written inside owner, or why it denotes none: "#" and "#id" name the owner
// and its contained resource; a relative reference resolves against a RESTful
// owner's base (FHIR R4) or, under an owner identified by a URN, to the one
// entry whose RESTful identity ends in it (resolveUnderURN).
func (g *pasGraph) subjectIdentity(owner *pasGraphEntry, ref string) (string, string) {
	if ref == "#" {
		return owner.fullURL, ""
	}
	if strings.HasPrefix(ref, "#") {
		return owner.fullURL + ref, ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", "is not a resolvable resource identity"
	}
	if !u.IsAbs() {
		base, err := url.Parse(owner.fullURL)
		if err != nil {
			return "", "is relative, and " + owner.label() + " has no fullUrl to resolve it against"
		}
		if base.Scheme == "urn" {
			resolved, why := g.resolveUnderURN(ref, owner)
			if why != "" {
				return "", why
			}
			ref = resolved
		} else {
			typ, _ := owner.resource["resourceType"].(string)
			id, ok := owner.resource["id"].(string)
			if !ok || (base.Scheme != "http" && base.Scheme != "https") {
				return "", "is relative, and " + owner.label() + " has no RESTful fullUrl to resolve it against"
			}
			base.Path = strings.TrimSuffix(base.Path, "/"+typ+"/"+id) + "/" + u.Path
			ref = base.String()
		}
	}
	if pos := strings.Index(ref, "/_history/"); pos >= 0 {
		ref = ref[:pos]
	}
	return ref, ""
}
