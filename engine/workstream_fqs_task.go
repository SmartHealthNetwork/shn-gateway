//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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

// taskForRead reads a federated-query Task's Task.for, by its exact member
// names, by FHIR's reference grammar: namesNoOne reports a reference that
// names no Patient so ("urn:Patient/<id>", "Group/1", "Patient/1/x",
// "Patient/a%2Fb", a "#<id>" naming no contained Patient or more than one),
// or names one by an id that names no one ("Patient/"); conforms reports
// whether the Patient id it names is a FHIR id (a contained Patient's member
// identifier is not an id, and conforms). An absent or empty Task.for, or a
// Task that does not read so, is left to the reader that follows.
func taskForRead(task []byte) (namesNoOne, conforms bool) {
	var view struct {
		For *struct {
			Reference *string `json:"reference"`
		} `json:"for"`
		Contained []any `json:"contained"`
	}
	if decodeMessage(task, &view) != nil || view.For == nil || view.For.Reference == nil || *view.For.Reference == "" {
		return false, true
	}
	ref := *view.For.Reference
	if id, conforms, ok := readPatientReference(ref); ok {
		return memberNamesNoOne(id), conforms
	}
	member, ok := patientRefMember(ref, containedObjects(view.Contained))
	return !ok || memberNamesNoOne(member), true
}

// msgFederatedQueryPatientIDNotFHIRID is the refusal of a federated-query
// Task whose Task.for names its Patient by an id that is not a FHIR id: the
// request's own shape, refused only by a participant that opted into it
// (structural and strict).
const msgFederatedQueryPatientIDNotFHIRID = "federated query Task.for names its Patient by an id that is not a FHIR id"

// cdexTaskQuery is a federated-query Task read as its data request, and the
// member its patient names. It is the one reading of the Task's patient: the
// requester's gateway binds the patient it checks consent for with it, and
// the facility's gateway binds the patient it discloses for with it, so the
// two can never read different patients from the same bytes. An error is a
// Task that is not a data request the leg's scope admits (one patient, named
// document types), or whose patient cannot be read.
func cdexTaskQuery(task []byte) (shnsdk.ParsedCDexRequest, string, error) {
	// A Task.for that names no Patient by FHIR's reference grammar, or names
	// no one, is refused before the SDK's reader reads it, whatever that
	// reader answers for it (errMemberNamesNoOne).
	if namesNoOne, _ := taskForRead(task); namesNoOne {
		return shnsdk.ParsedCDexRequest{}, "", errMemberNamesNoOne
	}
	parsed, err := shnsdk.ParseCDexTaskDataRequest(relativeCDexPatients(task))
	if err != nil {
		return shnsdk.ParsedCDexRequest{}, "", err
	}
	return parsed, strings.TrimPrefix(parsed.PatientRef, "Patient/"), nil
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
		// A Patient reference names its id, whether or not it is a FHIR id
		// (a segment, so it holds no "/"). A contained Patient's member is
		// written into Patient/<member> and the query only in an id's shape:
		// anything else is left as sent, for the check.
		if id, _, ok := readPatientReference(ref); ok {
			return "Patient/" + id
		}
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
