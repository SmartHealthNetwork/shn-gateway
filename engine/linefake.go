package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// LineFakeCall records a validation attempt without retaining the payload.
type LineFakeCall struct {
	ResourceType, Profile string
	Valid                 bool
	Issues                []string
}

// LineFakeValidator is a hermetic test double for selected PAS and DTR
// cardinalities and required bindings. It does not perform full FHIR validation.
// QR minima apply only to explicit or in-band DTR profile assertions.
// PAS checks item details, update relationships, response request references,
// and, at 2.2, ClaimTypes, AdditionalInformation document cardinality, request
// ClaimFirst, response identifiers and outcomes. DTR checks QR context and item
// minima before 2.2, coverage at 2.2, any supplied answer information-origin
// source at 2.2, and, against the $questionnaire-package output profile, the
// line's package and outcome parameters and the package's Questionnaire and
// QuestionnaireResponse entries. Optional slices are not synthesized or required.
// Unknown extensions and elements outside this scope are tolerated.
// Use NewLineFakeValidator to fix the validation line for its lifetime.
type LineFakeValidator struct {
	// Line is read-only metadata for callers. Validation uses the constructor's
	// private copy, so assigning this field cannot change the selected rules.
	Line  string
	line  string
	mu    sync.Mutex
	calls []LineFakeCall
}

var _ shnsdk.Validator = (*LineFakeValidator)(nil)

// NewLineFakeValidator fixes the rule line. Unsupported lines fail on Validate.
func NewLineFakeValidator(line string) *LineFakeValidator {
	return &LineFakeValidator{Line: line, line: line}
}

// Validate checks one complete JSON resource and records the result. Invalid
// JSON and unsupported lines are errors; scoped rule violations are invalid results.
// The profile selects PAS request/response Bundle and Claim update semantics.
func (v *LineFakeValidator) Validate(_ context.Context, payload []byte, profile string) (shnsdk.Result, error) {
	resource, err := decodeLineFake(v.line, payload)
	if err == nil && profile == "" {
		resource = unwrapLineFakeValidate(resource)
	}
	var issues []string
	if err != nil {
		issues = []string{err.Error()}
	} else {
		issues = lineFakeResourceIssues(v.line, resource, profile)
	}
	result := shnsdk.Result{Valid: err == nil && len(issues) == 0, Issues: issues}
	resourceType, _ := resource["resourceType"].(string)
	call := LineFakeCall{ResourceType: resourceType, Profile: profile, Valid: result.Valid, Issues: append([]string(nil), issues...)}
	v.mu.Lock()
	v.calls = append(v.calls, call)
	v.mu.Unlock()
	return result, err
}

// Calls returns an isolated snapshot, including a copy of every issue slice.
func (v *LineFakeValidator) Calls() []LineFakeCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := append([]LineFakeCall(nil), v.calls...)
	for i := range calls {
		calls[i].Issues = append([]string(nil), calls[i].Issues...)
	}
	return calls
}

// lineMinimaIssues applies the profile-empty scoped rules without recording calls.
func lineMinimaIssues(line string, payload []byte) []string {
	resource, err := decodeLineFake(line, payload)
	if err != nil {
		return []string{err.Error()}
	}
	return lineFakeResourceIssues(line, unwrapLineFakeValidate(resource), "")
}

func decodeLineFake(line string, payload []byte) (map[string]any, error) {
	if _, ok := shnsdk.PASLineDef(line); !ok {
		return nil, fmt.Errorf("line %s: unsupported validation line", line)
	}
	if _, ok := shnsdk.DTRLineDef(line); !ok {
		return nil, fmt.Errorf("line %s: unsupported validation line", line)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var resource map[string]any
	if err := decoder.Decode(&resource); err != nil {
		return nil, fmt.Errorf("line %s: invalid JSON resource: %w", line, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("line %s: expected one complete JSON resource", line)
	}
	if resource == nil {
		return nil, fmt.Errorf("line %s: expected JSON resource object", line)
	}
	return resource, nil
}

// unwrapLineFakeValidate reads a $validate invocation with no ?profile= as HAPI
// does: a Parameters whose one parameter is "resource" is the request wrapping
// the resource to check (wrapValidateResource), and the check is of that
// resource. With a ?profile= HAPI judges the Parameters it was sent, wrapper or
// not, so Validate unwraps only when the profile is empty.
func unwrapLineFakeValidate(resource map[string]any) map[string]any {
	if resource["resourceType"] != "Parameters" {
		return resource
	}
	params := lineFakeObjects(resource["parameter"])
	if len(params) != 1 || params[0]["name"] != "resource" {
		return resource
	}
	if inner := lineFakeObject(params[0]["resource"]); inner != nil {
		return inner
	}
	return resource
}

const (
	lineFakePAS = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
	lineFakeDTR = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"
)

// lineFakeResourceIssues walks resources and Bundle entries, with recursion
// inside QuestionnaireResponse limited to item.item and item.answer.item.
func lineFakeResourceIssues(line string, resource map[string]any, profile string) []string {
	pas, _ := shnsdk.PASLineDef(line)
	dtr, _ := shnsdk.DTRLineDef(line)
	explicitProfile := profile
	profile, _, _ = strings.Cut(profile, "|")
	var issues []string
	issue := func(path string) { issues = append(issues, "line "+line+": "+path) }
	var answers func([]map[string]any, string)
	answers = func(items []map[string]any, path string) {
		for i, item := range items {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			for j, answer := range lineFakeObjects(item["answer"]) {
				answerPath := fmt.Sprintf("%s.answer[%d]", itemPath, j)
				if line == "2.2" {
					for _, origin := range lineFakeExtensions(answer, lineFakeDTR+"information-origin") {
						sources := lineFakeExtensions(origin, "source")
						sourcePath := answerPath + ".extension:information-origin.extension:source"
						if len(sources) != 1 {
							issue(sourcePath)
						} else {
							switch sources[0]["valueCode"] {
							case "auto-client", "auto-server", "override", "manual":
							default:
								issue(sourcePath + ".valueCode")
							}
						}
					}
				}
				answers(lineFakeObjects(answer["item"]), answerPath+".item")
			}
			answers(lineFakeObjects(item["item"]), itemPath+".item")
		}
	}
	var walk func(map[string]any, string)
	walk = func(r map[string]any, p string) {
		switch r["resourceType"] {
		case "Bundle":
			entries := lineFakeObjects(r["entry"])
			request, response := p == lineFakePAS+"profile-pas-request-bundle", p == lineFakePAS+"profile-pas-response-bundle"
			if p == "" {
				for _, e := range entries {
					switch lineFakeObject(e["resource"])["resourceType"] {
					case "Claim":
						request = true
					case "ClaimResponse":
						response = true
					}
				}
				// A response may carry its original Claim, in any entry order.
				if response {
					request = false
				}
			}
			if line == "2.2" && request && (len(entries) == 0 || lineFakeObject(entries[0]["resource"])["resourceType"] != "Claim") {
				issue("Bundle.ClaimFirst")
			}
			// profile-pas-request-bundle's entry:Claim slice is 1..1 at PAS 2.0.1 and 1..2 from
			// 2.1.0, where an update carries the prior Claim it names (sdk/linedef.go's
			// UpdateCarriesPriorClaim).
			if request {
				claims, most := 0, 2
				if line == "2.0" {
					most = 1
				}
				for _, e := range entries {
					if lineFakeObject(e["resource"])["resourceType"] == "Claim" {
						claims++
					}
				}
				if claims > most {
					issue("Bundle.entry:Claim")
				}
			}
			if pas.ResponseBundleIdentifierRequired && response && r["identifier"] == nil {
				issue("Bundle.identifier")
			}
			for _, e := range entries {
				walk(lineFakeObject(e["resource"]), p)
			}
		case "Claim":
			// The PAS 2.1+ Claim.item line-detail minima were verified against
			// profile-claim.json and profile-claim-update.json — the SUBMIT and
			// AMENDMENT profiles (sdk/linedef.go's own provenance note). An
			// INQUIRY's Claim is profile-claim-inquiry, a third profile that
			// states none of them: an inquiry names the lines it asks about, it
			// does not re-request them. Holding it to the submit minima would
			// make this stand-in stricter than the IG it models.
			if pas.ClaimItemLineDetailRequired && !lineFakeInquiryProfile(p) {
				for i, item := range lineFakeObjects(r["item"]) {
					path := fmt.Sprintf("Claim.item[%d]", i)
					if len(lineFakeExtensions(item, lineFakePAS+"extension-certificationType")) == 0 {
						issue(path + ".extension:certificationType")
					}
					if len(lineFakeExtensions(item, lineFakePAS+"extension-serviceItemRequestType")) == 0 {
						issue(path + ".extension:requestType")
					}
					if item["locationCodeableConcept"] == nil && item["locationAddress"] == nil && item["locationReference"] == nil {
						issue(path + ".location[x]")
					}
				}
			}
			related := lineFakeObjects(r["related"])
			update := p == lineFakePAS+"profile-claim-update" || ((p == "" || p == lineFakePAS+"profile-pas-request-bundle") && len(related) > 0)
			if pas.ClaimRelatedRelationshipRequired && update {
				for i, entry := range related {
					if entry["relationship"] == nil {
						issue(fmt.Sprintf("Claim.related[%d].relationship", i))
					}
				}
			}
			if line == "2.2" {
				if !hasRequiredClaimType(lineFakeObjects(lineFakeObject(r["type"])["coding"])) {
					issue("Claim.type")
				}
				for i, info := range lineFakeObjects(r["supportingInfo"]) {
					additional := false
					for _, coding := range lineFakeObjects(lineFakeObject(info["category"])["coding"]) {
						if coding["system"] == "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes" && coding["code"] == "additionalInformation" {
							additional = true
							break
						}
					}
					if additional && len(lineFakeExtensions(info, lineFakePAS+"extension-documentInformation")) != 1 {
						issue(fmt.Sprintf("Claim.supportingInfo[%d].extension:documentInformation", i))
					}
				}
			}
		case "ClaimResponse":
			if pas.ClaimResponseRequestRequired && r["request"] == nil {
				issue("ClaimResponse.request")
			}
			if line == "2.2" {
				switch r["outcome"] {
				case "complete", "error", "partial":
				default:
					issue("ClaimResponse.outcome")
				}
			}
		case "Parameters":
			// Scoped by the explicit profile or the resource's own meta.profile (a
			// wrapped check asserts an explicit profile there: validateRequest).
			scoped := p == lineFakeDTR+"dtr-qpackage-output-parameters"
			if declared, ok := lineFakeObject(r["meta"])["profile"].([]any); ok {
				for _, value := range declared {
					if value, ok := value.(string); ok && canonicalOf(value) == lineFakeDTR+"dtr-qpackage-output-parameters" {
						scoped = true
					}
				}
			}
			if !scoped {
				break
			}
			// The $questionnaire-package output profile: the line's own parameter
			// names, the package's cardinality (1..* at 2.0.1, 0..* after) and the
			// outcome's (0..* at 2.0.1, 0..1 after); each package a Bundle whose
			// Questionnaire and QuestionnaireResponse entries the line's
			// DTR-QPackageBundle bounds. Parameters of another line's names are
			// open slicing's unsliced extras: allowed, and counted for nothing.
			pkgName, outcomeName := "PackageBundle", "operationOutcome"
			if line == "2.2" {
				pkgName, outcomeName = "packagebundle", "outcome"
			}
			packages, outcomes := 0, 0
			for i, param := range lineFakeObjects(r["parameter"]) {
				switch param["name"] {
				case pkgName:
					packages++
					bundle := lineFakeObject(param["resource"])
					path := fmt.Sprintf("Parameters.parameter[%d].resource", i)
					if bundle["resourceType"] != "Bundle" {
						issue(path)
						continue
					}
					questionnaires, responses := 0, 0
					for _, e := range lineFakeObjects(bundle["entry"]) {
						switch lineFakeObject(e["resource"])["resourceType"] {
						case "Questionnaire":
							questionnaires++
						case "QuestionnaireResponse":
							responses++
						}
					}
					switch dtr.QuestionnairePackageReturnShape {
					case "qr-optional":
						if questionnaires != 1 || responses > 1 {
							issue(path + ".entry")
						}
					case "qr-required":
						if questionnaires != 1 || responses != 1 {
							issue(path + ".entry")
						}
					}
				case outcomeName:
					outcomes++
				}
			}
			if line == "2.0" && packages < 1 {
				issue("Parameters.parameter:" + pkgName)
			}
			if line != "2.0" && outcomes > 1 {
				issue("Parameters.parameter:" + outcomeName)
			}
		case "QuestionnaireResponse":
			scoped := false
			profiles := []string{explicitProfile}
			if declared, ok := lineFakeObject(r["meta"])["profile"].([]any); ok {
				for _, value := range declared {
					if value, ok := value.(string); ok {
						profiles = append(profiles, value)
					}
				}
			}
			for _, declared := range profiles {
				canonical, version, versioned := strings.Cut(declared, "|")
				if canonical != dtrQRCanonical {
					continue
				}
				scoped = true
				if versioned && version != dtr.PackageVersion {
					issue("QuestionnaireResponse.meta.profile:line")
				}
			}
			if !scoped {
				break
			}
			if dtr.SingleCoverageConstraint {
				if len(lineFakeExtensions(r, lineFakeDTR+"qr-coverage")) < 1 {
					issue("QuestionnaireResponse.extension:qr-coverage")
				}
			} else {
				if len(lineFakeExtensions(r, lineFakeDTR+"qr-context")) < 2 {
					issue("QuestionnaireResponse.extension:qr-context")
				}
				if len(lineFakeObjects(r["item"])) < 1 {
					issue("QuestionnaireResponse.item")
				}
			}
			answers(lineFakeObjects(r["item"]), "QuestionnaireResponse.item")
		}
	}
	walk(resource, profile)
	return issues
}

func hasRequiredClaimType(codings []map[string]any) bool {
	for _, c := range codings {
		if c["system"] != "http://terminology.hl7.org/CodeSystem/claim-type" {
			continue
		}
		switch c["code"] {
		case "institutional", "professional", "oral":
			return true
		}
	}
	return false
}

func lineFakeObject(value any) map[string]any { m, _ := value.(map[string]any); return m }
func lineFakeObjects(value any) []map[string]any {
	values, _ := value.([]any)
	var objects []map[string]any
	for _, v := range values {
		objects = append(objects, lineFakeObject(v))
	}
	return objects
}
func lineFakeExtensions(resource map[string]any, url string) []map[string]any {
	var found []map[string]any
	for _, e := range lineFakeObjects(resource["extension"]) {
		if e["url"] == url {
			found = append(found, e)
		}
	}
	return found
}
