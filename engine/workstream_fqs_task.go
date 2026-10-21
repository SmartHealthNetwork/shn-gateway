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

// cdexTaskQuery is a federated-query Task read as its data request, and the
// member its patient names. It is the one reading of the Task's patient: the
// requester's gateway binds the patient it checks consent for with it, and
// the facility's gateway binds the patient it discloses for with it, so the
// two can never read different patients from the same bytes. An error is a
// Task that is not a data request the leg's scope admits (one patient, named
// document types), or whose patient cannot be read.
func cdexTaskQuery(task []byte) (shnsdk.ParsedCDexRequest, string, error) {
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
