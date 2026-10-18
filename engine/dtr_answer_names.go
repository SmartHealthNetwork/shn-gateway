package engine

import (
	"encoding/json"
	"slices"
)

// The DTR $questionnaire-package answer names two of its parameters by line.
// What each published package states (its output Parameters profile, which
// conformance certifies, and its OperationDefinition):
//
//	line   OperationDefinition out        output profile slices
//	2.0.1  return 0..*, outcome 0..1      PackageBundle 1..*, operationOutcome 0..*
//	2.1.0  PackageBundle 0..*, outcome 0..1  PackageBundle 0..*, operationOutcome 0..1
//	2.2.0  packagebundle 0..*, outcome 0..1  packagebundle 0..*, outcome 0..1
//
// Every line slices its parameters openly by name, so a name from another line
// validates and is silently missed by a reader that looks for its own. Only
// the names one line family uses and the other does not are line-specific:
// PackageBundle and operationOutcome (2.0.1 and 2.1.0), packagebundle (2.2.0).
// outcome is every line's OperationDefinition name and is never renamed.
// return, the 2.0.1 OperationDefinition's package name, is not the name its
// certified profile gives the package and is carried as sent.

// dtrNameFamily is the set of DTR lines that share their answer's names.
type dtrNameFamily int

const (
	dtrNamesNone dtrNameFamily = iota
	dtrNames20                 // 2.0 and 2.1
	dtrNames22                 // 2.2
)

// dtrLineSpecificNames maps each line-specific answer parameter name to the
// family that uses it.
var dtrLineSpecificNames = map[string]dtrNameFamily{
	"PackageBundle":    dtrNames20,
	"operationOutcome": dtrNames20,
	"packagebundle":    dtrNames22,
}

// dtrNameInto maps a line-specific name to its name in the other family; a
// name absent here keeps its name across lines.
var dtrNameInto = map[dtrNameFamily]map[string]string{
	dtrNames22: {"PackageBundle": "packagebundle", "operationOutcome": "outcome"},
	dtrNames20: {"packagebundle": "PackageBundle"},
}

// dtrFamilyOf is the name family of a DTR line ("2.0", "2.1" or "2.2").
func dtrFamilyOf(line string) dtrNameFamily {
	switch line {
	case "2.0", "2.1":
		return dtrNames20
	case "2.2":
		return dtrNames22
	}
	return dtrNamesNone
}

// dtrFamilyLines are the lines of a family, in order.
func dtrFamilyLines(f dtrNameFamily) []string {
	switch f {
	case dtrNames20:
		return []string{"2.0", "2.1"}
	case dtrNames22:
		return []string{"2.2"}
	}
	return nil
}

// dtrAnswerRename is one parameter name E-08 changes: the parameter's index in
// the answer's parameter array, its name as the payer wrote it, and the name
// the requester's line gives it.
type dtrAnswerRename struct {
	Index    int
	From, To string
}

// dtrAnswerPlan is what E-08 would do to one answer for a requester on
// targetLine: the answer's own name family and the renames that carry it into
// the target line. An empty Renames means no edit: the answer is carried as
// sent.
type dtrAnswerPlan struct {
	Source  dtrNameFamily
	Target  string
	Renames []dtrAnswerRename
}

// planDTRAnswerNames reads answer's top-level parameter names and decides the
// renames that carry it to a requester on targetLine. It renames only when
// every line-specific name in the answer belongs to one family and that family
// is not targetLine's. A body that is not a Parameters, names of both
// families, no line-specific name, or a target in the answer's own family
// plans no rename. Only the names change; nothing is read past them.
func planDTRAnswerNames(answer []byte, targetLine string) dtrAnswerPlan {
	plan := dtrAnswerPlan{Target: targetLine}
	target := dtrFamilyOf(targetLine)
	if target == dtrNamesNone {
		return plan
	}
	var params struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name string `json:"name"`
		} `json:"parameter"`
	}
	if json.Unmarshal(answer, &params) != nil || params.ResourceType != "Parameters" {
		return plan
	}
	var families []dtrNameFamily
	for _, p := range params.Parameter {
		if f, ok := dtrLineSpecificNames[p.Name]; ok && !slices.Contains(families, f) {
			families = append(families, f)
		}
	}
	if len(families) != 1 || families[0] == target {
		return plan
	}
	plan.Source = families[0]
	into := dtrNameInto[target]
	for i, p := range params.Parameter {
		if to, ok := into[p.Name]; ok {
			plan.Renames = append(plan.Renames, dtrAnswerRename{Index: i, From: p.Name, To: to})
		}
	}
	return plan
}
