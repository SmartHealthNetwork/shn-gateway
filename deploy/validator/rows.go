// rows.go defines the finite readiness corpus checked before a validator
// process can serve requests.
package main

import "embed"

//go:embed testdata/*.json testdata/2.1/*.json testdata/2.2/*.json
var fixtures embed.FS

// warmup is one $validate the supervisor issues before a lane may call itself ready.
type warmup struct {
	identity     string // stable progress identity; unique across the readiness corpus
	file         string // embedded fixture path
	resourceType string // $validate route: {base}/{resourceType}/$validate
	profile      string // ?profile= canonical (unversioned — resolves to the line's baked package)
	mode         verdictMode
	line         string
}

// warmups returns the original four profile-resolution rows. Its legacy helper
// default remains 2.0, while readinessRows and supervisor admission reject an
// unknown lane before any request.
func warmups(line string) []warmup {
	dir := "testdata"
	switch line {
	case "2.1", "2.2":
		dir = "testdata/" + line
	}
	return []warmup{
		{identity: "init-pas-request-bundle", file: dir + "/claim-bundle.json", resourceType: "Bundle", profile: "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle", mode: verdictInitialize, line: line},
		{identity: "init-dtr-questionnaireresponse", file: dir + "/questionnaireresponse-autofill.json", resourceType: "QuestionnaireResponse", profile: "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse", mode: verdictInitialize, line: line},
		{identity: "init-pdex-explanationofbenefit", file: "testdata/eob-approved.json", resourceType: "ExplanationOfBenefit", profile: "http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization", mode: verdictInitialize, line: line},
		{identity: "init-cdex-task", file: "testdata/cdex-task-data-request.json", resourceType: "Task", profile: "http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-data-request", mode: verdictInitialize, line: line},
	}
}
