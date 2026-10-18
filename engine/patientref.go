package engine

// patientref.go — a patient reference read the same way in every form FHIR
// allows. A relative ("Patient/<id>") or absolute ("<base>/Patient/<id>", with
// or without "/_history/<version>") reference names the member by its Patient
// id; a contained one ("#<id>") names the contained Patient, whose member is
// its member identifier (its id is local to the resource carrying it). Only
// a well-formed reference names a patient: anything else (a trailing path, an
// operation, an escaped character, a reference that resolves to no patient or
// to more than one) names none, and the caller refuses it as it refused it
// before.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A FHIR id: letters, digits, "-" and ".", at most 64.
const fhirID = `[A-Za-z0-9\-.]{1,64}`

// patientRefShape is a well-formed relative or absolute Patient reference: an
// optional http(s) base of plain path segments, then Patient/<id>, then an
// optional /_history/<version>, and nothing else.
var patientRefShape = regexp.MustCompile(`^(?:https?://[A-Za-z0-9.\-]+(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~\-]+)*/)?Patient/(` + fhirID + `)(?:/_history/` + fhirID + `)?$`)

// localRefShape is a well-formed contained reference.
var localRefShape = regexp.MustCompile(`^#(` + fhirID + `)$`)

// patientRefMember returns the member a patient reference names, among the
// resource's contained resources; ok is false when it names none.
func patientRefMember(ref string, contained []map[string]any) (string, bool) {
	if m := localRefShape.FindStringSubmatch(ref); m != nil {
		id := m[1]
		var found map[string]any
		for _, c := range contained {
			if cid, _ := c["id"].(string); cid != id {
				continue
			}
			if found != nil {
				return "", false // one id naming two contained resources
			}
			found = c
		}
		if found == nil || found["resourceType"] != "Patient" {
			return "", false
		}
		return containedPatientMember(found)
	}
	if m := patientRefShape.FindStringSubmatch(ref); m != nil {
		return m[1], true
	}
	return "", false
}

// containedPatientMember is a contained Patient's member: its member
// identifier, which it must carry exactly once (a contained resource's id is
// local to the resource that carries it, never a member id). The value is an
// identifier's, not limited to a FHIR id's shape.
func containedPatientMember(patient map[string]any) (string, bool) {
	member := ""
	ids, _ := patient["identifier"].([]any)
	for _, id := range ids {
		m, ok := id.(map[string]any)
		if !ok || m["system"] != shnsdk.MemberSystem {
			continue
		}
		v, _ := m["value"].(string)
		if v == "" {
			continue // a blank value names no one
		}
		if member != "" && v != member {
			return "", false // two members: more than one patient
		}
		member = v
	}
	return member, member != ""
}

// containedObjects is a contained list's resources: its elements that are
// JSON objects. Anything else in it names no resource, and reading the
// request does not depend on it.
func containedObjects(list []any) []map[string]any {
	var out []map[string]any
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

var idShape = regexp.MustCompile(`^` + fhirID + `$`)

// eligibilityRequestMember is the member a CoverageEligibilityRequest's
// patient names, in any reference form. The SDK's reader reads the request
// first, so a request it refuses is refused as before and one it reads is
// read; the patient reference is then resolved in its well-formed forms. A
// request this gateway cannot read strictly (a repeated or case-folded member
// name, or any other shape the strict decode does not accept), and a
// reference that is neither well-formed nor contained, keep the SDK's
// reading, so nothing it read before is read differently or refused. An
// error is a request the SDK's reader refuses, or a contained reference that
// names no contained Patient or more than one.
func eligibilityRequestMember(cer []byte) (string, error) {
	literal, err := shnsdk.ParseEligibilityRequestMember(cer)
	if err != nil {
		return "", err
	}
	var req struct {
		Patient struct {
			Reference string `json:"reference"`
		} `json:"patient"`
		Contained []any `json:"contained"`
	}
	if decodeMessage(cer, &req) != nil {
		return literal, nil
	}
	ref := req.Patient.Reference
	if member, ok := patientRefMember(ref, containedObjects(req.Contained)); ok {
		return member, nil
	}
	// A contained reference that names no contained Patient, or more than
	// one, names no patient. Any other reference that is not a well-formed
	// one keeps the SDK's reading: a member id is an identifier value, not
	// limited to a FHIR id's shape, so it is read as before.
	if strings.HasPrefix(ref, "#") {
		return "", fmt.Errorf("patient reference %q names no patient", ref)
	}
	return literal, nil
}

// cdexTaskView is the federated-query Task as the SDK's reader
// (shnsdk.ParseCDexTaskDataRequest) reads it: the same fields. It is decoded
// strictly (decodeMessage), so only a Task with no repeated member name and
// every name exact is rewritten; for such a Task every reader, the SDK's
// included, reads the same values. Re-encoding this view, not the whole
// document, hands that reader exactly those values.
type cdexTaskView struct {
	ResourceType string `json:"resourceType"`
	Code         struct {
		Coding []cdexCoding `json:"coding"`
	} `json:"code"`
	For struct {
		Reference string `json:"reference"`
	} `json:"for"`
	Input []struct {
		Type struct {
			Coding []cdexCoding `json:"coding"`
		} `json:"type"`
		ValueString string `json:"valueString"`
	} `json:"input"`
}

// cdexCoding is a Coding as the SDK's reader takes it. The view names its
// members exactly, so the strict decode accepts the FHIR names and refuses
// any that only fold to them; the SDK's reader matches these names too.
type cdexCoding struct {
	System string `json:"system"`
	Code   string `json:"code"`
}

// relativeCDexPatients returns, for reading only, the federated-query Task as
// the SDK's reader sees it, with Task.for and each query's patient parameter
// in the relative form ("Patient/<member>") when they are well-formed
// references, so the narrowness check compares one patient however each was
// written. Anything else is left as sent, for the check to refuse. A Task
// whose member names are ambiguous (repeated, or matching a field only by case
// folding) is returned as sent, so the check reads it exactly as before: only
// an unambiguous Task, which every reader reads alike, is rewritten. The Task
// this gateway was sent is never changed.
func relativeCDexPatients(task []byte) []byte {
	var view cdexTaskView
	if decodeMessage(task, &view) != nil {
		return task
	}
	var holder struct {
		Contained []any `json:"contained"`
	}
	if decodeMessage(task, &holder) != nil {
		return task
	}
	relative := func(ref string) string {
		// Only a member in an id's shape is written into Patient/<member>
		// and the query: anything else is left as sent, for the check.
		if member, ok := patientRefMember(ref, containedObjects(holder.Contained)); ok && idShape.MatchString(member) {
			return "Patient/" + member
		}
		return ref
	}
	view.For.Reference = relative(view.For.Reference)
	for i, in := range view.Input {
		path, params, found := strings.Cut(in.ValueString, "?")
		if !found {
			continue
		}
		pairs := strings.Split(params, "&")
		for j, pair := range pairs {
			if v, isPatient := strings.CutPrefix(pair, "patient="); isPatient {
				pairs[j] = "patient=" + relative(v)
			}
		}
		view.Input[i].ValueString = path + "?" + strings.Join(pairs, "&")
	}
	out, err := json.Marshal(view)
	if err != nil {
		return task
	}
	return out
}
