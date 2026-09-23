package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolveSubjectPCI is a local source-action lookup through this holder's own
// record. Native carriage uses verified exchange context instead. The deprecated
// compatibility flag never supplies identity from payload bytes.
func (g *Gateway) resolveSubjectPCI(ctx context.Context, member string, _ []byte) (pci string, found bool, readErr error) {
	pci, _, found, readErr = ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	return pci, found, readErr
}

// PatientDemographics reads the two demographics the patient identifier is derived from
// off a FHIR Patient: birthDate and the family name of the first name, both required and
// read verbatim. It is the one read of a Patient's demographics, for a holder's own record
// only. Carried demographics are never a source of missing exchange identity.
func PatientDemographics(patientJSON []byte) (Demo, bool) {
	var p struct {
		ResourceType string `json:"resourceType"`
		BirthDate    string `json:"birthDate"`
		Name         []struct {
			Family string `json:"family"`
		} `json:"name"`
	}
	if json.Unmarshal(patientJSON, &p) != nil || p.ResourceType != "Patient" || p.BirthDate == "" || len(p.Name) == 0 || p.Name[0].Family == "" {
		return Demo{}, false
	}
	return Demo{BirthDate: p.BirthDate, FamilyName: p.Name[0].Family}, true
}

// carriesPatient reports whether the request carries a Patient resource for the member
// anywhere, with demographics or not.
func carriesPatient(payload []byte, member string) bool {
	carried := false
	forEachCarriedPatient(payload, member, func(map[string]any) { carried = true })
	return carried
}

// forEachCarriedPatient calls fn for every Patient resource the request carries for the
// member, at any depth. A Patient is the member's when its id or its member identifier is
// the member id. An empty or unreadable request carries none.
func forEachCarriedPatient(payload []byte, member string, fn func(patient map[string]any)) {
	var doc any
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil {
		return
	}
	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["resourceType"] == "Patient" && patientIsMember(v, member) {
				fn(v)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
}

// patientIsMember reports whether a decoded Patient names the member by id or by member
// identifier.
func patientIsMember(patient map[string]any, member string) bool {
	if patient["id"] == member {
		return true
	}
	ids, _ := patient["identifier"].([]any)
	for _, id := range ids {
		if m, ok := id.(map[string]any); ok && m["system"] == shnsdk.MemberSystem && m["value"] == member {
			return true
		}
	}
	return false
}

// PatientReference names a patient in its producer holder and identifier system.
// Relative FHIR references use System "fhir-relative"; absolute FHIR references
// use the full endpoint base as System, with "Patient/id" as Value. Identifiers
// retain their exact system/value. Equal lexical ids in different namespaces
// never establish equal people.
type PatientReference struct{ Holder, System, Value string }

// SubjectReferenceResolver is an authoritative integration capability, not a
// demographic matching algorithm. found=false means linkage unavailable. The
// integration must resolve only namespaces it is actually authorized to know.
type SubjectReferenceResolver interface {
	ResolveSubject(context.Context, PatientReference) (pci string, found bool, err error)
}

func (g *Gateway) checkSubjectConsistency(ctx context.Context, in CheckInput) CheckResult {
	return g.checkSubjectConsistencyResource(ctx, in, nil)
}

func (g *Gateway) checkSubjectConsistencyResource(ctx context.Context, in CheckInput, selected map[string]any) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("identity_unavailable")
	}
	root, ok := deepDocument(in)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	if in.Exchange.subjectPCI == "" || g.cfg.SubjectReferenceResolver == nil {
		return deepUnavailable("identity_unavailable")
	}
	holder := in.Exchange.holder
	if in.Direction == "response" {
		holder = in.Exchange.recipient
	}
	if holder == "" {
		return deepUnavailable("identity_unavailable")
	}
	result := CheckResult{State: CheckValid}
	checked := 0
	resolve := func(ref PatientReference) bool {
		if ctx.Err() != nil {
			result = deepUnavailable("identity_unavailable")
			return false
		}
		pci, found, err := g.cfg.SubjectReferenceResolver.ResolveSubject(ctx, ref)
		if err != nil || !found || pci == "" {
			return false
		}
		checked++
		if pci != in.Exchange.subjectPCI {
			result = checkResult("patient.consistency", false)
		}
		return true
	}
	var patient func(map[string]any, string) bool
	patient = func(p map[string]any, fallback string) bool {
		if !resourceIs(p, "Patient") {
			return false
		}
		found := false
		identifiers, _ := p["identifier"].([]any)
		for _, v := range identifiers {
			i, _ := v.(map[string]any)
			sys, _ := i["system"].(string)
			value, _ := i["value"].(string)
			if sys != "" && value != "" {
				found = resolve(PatientReference{holder, sys, value}) || found
			}
		}
		if found {
			return true
		}
		if fallback == "" {
			return false
		}
		ref, ok := patientReference(holder, fallback)
		return ok && resolve(ref)
	}
	visitRef := func(raw any, owner map[string]any, ownerURL string, scope *identityBundleScope) {
		ref, ok := raw.(map[string]any)
		if !ok {
			result = checkResult("patient.consistency", false)
			return
		}
		value, _ := ref["reference"].(string)
		if value == "" {
			i, _ := ref["identifier"].(map[string]any)
			sys, _ := i["system"].(string)
			value, _ := i["value"].(string)
			if sys != "" && value != "" && resolve(PatientReference{holder, sys, value}) {
				return
			}
			result = deepUnavailable("identity_unavailable")
			return
		}
		if strings.HasPrefix(value, "#") {
			contained, _ := owner["contained"].([]any)
			var target map[string]any
			for _, v := range contained {
				r, _ := v.(map[string]any)
				if r["id"] == strings.TrimPrefix(value, "#") {
					if target != nil {
						result = deepUnavailable("identity_unavailable")
						return
					}
					target = r
				}
			}
			if target == nil || (resourceIs(target, "Patient") && !patient(target, "")) {
				result = deepUnavailable("identity_unavailable")
			}
			return
		}
		// FHIR relative references inherit the producing entry's REST base.
		// Never discard a foreign base and reinterpret its lexical id locally.
		if relativeRef.MatchString(value) && ownerURL != "" {
			u, err := url.Parse(ownerURL)
			if err == nil && (u.Scheme == "https" || u.Scheme == "http") {
				parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
				if len(parts) >= 3 {
					u.Path = strings.Join(parts[:len(parts)-2], "/") + "/" + value
					u.RawPath = ""
					value = u.String()
				}
			}
		}
		if scope != nil && scope.ambiguous[value] {
			result = deepUnavailable("identity_unavailable")
			return
		}
		if scope != nil && scope.byURL[value] != nil {
			p := scope.byURL[value]
			if resourceIs(p, "Patient") && !patient(p, value) {
				result = deepUnavailable("identity_unavailable")
			}
			return
		}
		p, ok := patientReference(holder, value)
		if !ok {
			if typ, known := foreignType(value); known && typ != "Patient" {
				return
			}
			if parts := relativeRef.FindStringSubmatch(value); len(parts) > 1 && parts[1] != "Patient" {
				return
			}
			result = deepUnavailable("identity_unavailable")
			return
		}
		if !resolve(p) {
			result = deepUnavailable("identity_unavailable")
		}
	}
	roots := []any{root}
	// A supported DTR operation's Parameters is a transport wrapper, not a
	// patient-compartment resource. Walk its children with their original
	// Bundle and contained-resource scopes; nested unknown wrappers still fail.
	_, dtrWrapper := dtrIdentityParameters(in, root)
	// Recognized inquiry envelopes are transport wrappers; compare the subjects
	// in their returned Bundles, each within its own reference scope.
	if bundles, ok := inquiryReplyBundles(in); ok {
		roots = bundles
	}
	walkIdentityResources(roots, "", nil, nil, func(r map[string]any, ownerURL string, scope *identityBundleScope, owner map[string]any) {
		// The walker visits the root before its children. Skip only that proven
		// transport wrapper, never an unknown nested resource.
		if dtrWrapper {
			dtrWrapper = false
			return
		}
		// Keep the complete Bundle namespace for lookup, but only the selected
		// resource contributes patient-binding obligations to this consumer.
		if selected != nil && !reflect.DeepEqual(r, selected) {
			return
		}
		rt, _ := r["resourceType"].(string)
		if rt == "Patient" {
			return
		} // Non-binding subscriber/related patients are not the subject.
		// CDex data-request Tasks bind the declared patient in Task.for. Generic
		// FHIR Task has no patient-compartment path, so apply this only to the
		// federated-query operation's top-level Task. Resolve through the
		// producer's holder and reference namespace, never a lexical ID match.
		if in.Exchange.legType == "federated-query" && resourceIs(root, "Task") && rt == "Task" && reflect.DeepEqual(r, root) {
			if target, present := r["for"]; present {
				visitRef(target, r, ownerURL, scope)
			} else {
				result = deepUnavailable("identity_unavailable")
			}
		}
		paths, known := patientBindingPaths[rt]
		if !known {
			result = deepUnavailable("identity_unavailable")
			return
		}
		for _, path := range paths {
			var refs []any
			collect(r, strings.Split(path, "."), &refs)
			for _, ref := range refs {
				visitRef(ref, owner, ownerURL, scope)
			}
		}
	})
	if resourceIs(root, "Patient") && !patient(root, "") {
		result = deepUnavailable("identity_unavailable")
	}
	if strings.HasPrefix(in.Exchange.legType, "crd-") {
		c, _ := root["context"].(map[string]any)
		id, _ := c["patientId"].(string)
		if id != "" {
			if !strings.Contains(id, "/") {
				id = "Patient/" + id
			}
			visitRef(map[string]any{"reference": id}, root, "", nil)
		}
	}
	if in.Exchange.legType == "patient-dtr" {
		ref, _ := root["patientRef"].(string)
		if ref != "" {
			visitRef(map[string]any{"reference": ref}, root, "", nil)
		}
	}
	if checked == 0 && result.State == CheckValid {
		return deepUnavailable("identity_unavailable")
	}
	return result
}

func patientReference(holder, value string) (PatientReference, bool) {
	if m := relativeRef.FindStringSubmatch(value); len(m) > 1 && m[1] == "Patient" {
		return PatientReference{holder, "fhir-relative", value}, true
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return PatientReference{}, false
	}
	pos := strings.LastIndex(u.Path, "/Patient/")
	if pos < 0 {
		return PatientReference{}, false
	}
	ref := u.Path[pos+1:]
	if m := relativeRef.FindStringSubmatch(ref); len(m) < 2 || m[1] != "Patient" {
		return PatientReference{}, false
	}
	u.Path = u.Path[:pos]
	u.RawPath = ""
	return PatientReference{holder, u.String(), ref}, true
}

type identityBundleScope struct {
	byURL     map[string]map[string]any
	ambiguous map[string]bool
}

// walkIdentityResources retains the nearest Bundle's entry namespace. Contained
// resources share their owning resource's contained scope, never another entry's.
func walkIdentityResources(v any, ownerURL string, scope *identityBundleScope, containedOwner map[string]any, fn func(map[string]any, string, *identityBundleScope, map[string]any)) {
	switch v := v.(type) {
	case map[string]any:
		owner := containedOwner
		if _, ok := v["resourceType"].(string); ok {
			if owner == nil {
				owner = v
			}
			if resourceIs(v, "Bundle") {
				scope = &identityBundleScope{byURL: map[string]map[string]any{}, ambiguous: map[string]bool{}}
				entries, _ := v["entry"].([]any)
				for _, raw := range entries {
					e, _ := raw.(map[string]any)
					full, _ := e["fullUrl"].(string)
					if full == "" {
						continue
					}
					if _, exists := scope.byURL[full]; exists {
						scope.ambiguous[full] = true
					}
					scope.byURL[full], _ = e["resource"].(map[string]any)
				}
			}
			fn(v, ownerURL, scope, owner)
		}
		for _, k := range pasSortedKeys(v) {
			if k == "entry" && resourceIs(v, "Bundle") {
				entries, _ := v[k].([]any)
				for _, raw := range entries {
					e, _ := raw.(map[string]any)
					full, _ := e["fullUrl"].(string)
					walkIdentityResources(e, full, scope, nil, fn)
				}
			} else if k == "contained" {
				walkIdentityResources(v[k], ownerURL, scope, owner, fn)
			} else {
				walkIdentityResources(v[k], ownerURL, scope, nil, fn)
			}
		}
	case []any:
		for _, child := range v {
			walkIdentityResources(child, ownerURL, scope, containedOwner, fn)
		}
	}
}

// dtrIdentityParameters recognizes only the outer supported operation envelope.
// It does not exempt patient-free content or certify the IG profile.
func dtrIdentityParameters(in CheckInput, root map[string]any) ([]any, bool) {
	if in.Exchange.legType != "dtr-questionnaire-fetch" || (in.Direction != "request" && in.Direction != "response") {
		return nil, false
	}
	contract, line, ok := strings.Cut(in.DeclaredVersion, "@")
	if !ok || contract != "pa.dtr" {
		return nil, false
	}
	if _, ok := shnsdk.DTRLineDef(line); !ok {
		return nil, false
	}
	params, ok := parameterShapes(root, in.Direction == "request")
	if !ok {
		return nil, false
	}
	switch in.Exchange.operation {
	case shnsdk.FrameOperationQuestionnairePackage:
		if in.Direction == "response" && !dtrPackageOutputParameters(params, line) {
			return nil, false
		}
	case shnsdk.FrameOperationNextQuestion:
		if !nextQuestionShape(root, in.Direction) {
			return nil, false
		}
	default:
		return nil, false
	}
	return params, true
}

// dtrPackageOutputParameters proves only the published output envelope and
// primary resource shapes. The walker still checks every child in its original
// namespace; profile validity and clinical content remain separate checks.
func dtrPackageOutputParameters(params []any, line string) bool {
	packages := 0
	for _, raw := range params {
		p := raw.(map[string]any) // parameterShapes established object/name shape.
		for key := range p {
			if key == "part" || strings.HasPrefix(key, "value") {
				return false // A resource parameter cannot also assert a value/part.
			}
		}
		r, ok := p["resource"].(map[string]any)
		if !ok {
			return false
		}
		name := p["name"].(string)
		packageName := (line == "2.0" && name == "return") ||
			((line == "2.0" || line == "2.1") && name == "PackageBundle") ||
			(line == "2.2" && name == "packagebundle")
		if !packageName {
			outcomeName := (line == "2.2" && name == "outcome") ||
				((line == "2.0" || line == "2.1") && name == "operationOutcome")
			if !outcomeName || !resourceIs(r, "OperationOutcome") {
				return false
			}
			continue
		}
		entries, ok := bundleEntries(r, true)
		if !ok || r["type"] != "collection" {
			return false
		}
		for _, raw := range entries {
			e := raw.(map[string]any)
			child, ok := e["resource"].(map[string]any)
			if !ok || !shapeString(child["resourceType"]) {
				return false
			}
		}
		packages++
	}
	return packages > 0
}
