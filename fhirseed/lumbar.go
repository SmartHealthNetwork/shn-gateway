package fhirseed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// putClient is a bounded HTTP client for PutGlobalArtifact — a published library
// function must not hand a caller's context to http.DefaultClient's unbounded
// timeout. Mirrors (*Client).httpClient's role: a small, explicit client rather
// than the zero-value default.
var putClient = &http.Client{Timeout: 30 * time.Second}

// SandboxLumbarLibrary builds the operated-$populate prepop CQL Library the sandbox
// DTR questionnaire's cqf-library canonical points at
// (http://smarthealth.network/fhir/Library/LumbarMRICQL — see the SDK's sandbox
// questionnaire). Deployments running native DTR (PROVIDER_DTR_NATIVE) against the
// sandbox questionnaire world must install it into the engine's DEFAULT partition
// (a Library is a non-partitionable knowledge artifact) or $populate fails with
// "Could not load source for library LumbarMRICQL". The name/version/canonical tail
// MUST equal the CQL header `library LumbarMRICQL version '1.0.0'` or the
// clinical-reasoning engine cannot load the source. Distinct from
// CRPrepopLibraries(): those are the payer-engine prepop set, installed wholesale
// by existing consumers; this is the provider-tenant operated-CQL library.
//
// The retrieve uses a codesystem/code declaration (a direct code, not a ValueSet —
// no terminology expansion, so evaluation stays deterministic), built from the same
// shnsdk constants as the seeded Observations/Procedures/DiagnosticReports. Direct
// navigation (First([Resource: code]).value) is used throughout; the query-comprehension
// form (`First(...) O return …`) does not survive HAPI's CQL translator, so it is
// avoided everywhere. PriorSurgery covers every code in shnsdk.ProcedureValueSet by
// iteration, so a future additional SNOMED code is automatically included without a
// manual CQL edit.
func SandboxLumbarLibrary() ([]byte, error) {
	// PriorSurgery is built from ProcedureValueSet using named code aliases — the
	// retrieve form HAPI's CQL translator accepts (`exists [Procedure: "ProcSurgeryN"]`).
	// The inline `Code 'x' from "SNOMED"` retrieve-terminology form does not translate
	// and must not be used. Iterating the set means a future additional code is
	// automatically included; the drift guard in the tests catches any mismatch
	// between the set and the generated CQL.
	procCodeDecls := make([]string, 0, len(shnsdk.ProcedureValueSet))
	priorSurgeryParts := make([]string, 0, len(shnsdk.ProcedureValueSet))
	for i, code := range shnsdk.ProcedureValueSet {
		alias := fmt.Sprintf("ProcSurgery%d", i)
		procCodeDecls = append(procCodeDecls, fmt.Sprintf("code %q: '%s' from \"SNOMED\"", alias, code))
		priorSurgeryParts = append(priorSurgeryParts, fmt.Sprintf("exists [Procedure: %q]", alias))
	}
	procCodeBlock := strings.Join(procCodeDecls, "\n")
	priorSurgeryExpr := strings.Join(priorSurgeryParts, " or ")

	cql := fmt.Sprintf(`library LumbarMRICQL version '1.0.0'
using FHIR version '4.0.1'
include FHIRHelpers version '4.0.1'
codesystem "SHNClinical": '%s'
codesystem "SNOMED": '%s'
codesystem "LOINC": '%s'
codesystem "CPT": '%s'
code "CTWeeks": '%s' from "SHNClinical"
code "Neuro": '%s' from "SHNClinical"
code "PatientReported": '%s' from "SHNClinical"
code "ODI": '%s' from "LOINC"
code "ImagingCPT": '%s' from "CPT"
%s
context Patient
define "ConservativeTherapyWeeks": (First([Observation: "CTWeeks"]).value as FHIR.Quantity).value
define "NeuroDeficit": First([Observation: "Neuro"]).value as FHIR.boolean
define "PriorImaging": exists [DiagnosticReport: "ImagingCPT"]
define "PriorSurgery": %s
define "HighDisability": exists [Observation: "ODI"]
define "PatientReportedRequired": First([Observation: "PatientReported"]).value as FHIR.boolean
`,
		shnsdk.SystemSHNClinical,
		shnsdk.SystemSNOMED,
		shnsdk.SystemLOINC,
		shnsdk.SystemCPT,
		shnsdk.ConservativeTherapyWeeksCode,
		shnsdk.NeuroDeficitCode,
		shnsdk.PatientReportedCode,
		shnsdk.ODICode,
		shnsdk.ImagingCPT,
		procCodeBlock,
		priorSurgeryExpr,
	)

	lib := map[string]any{
		"resourceType": "Library",
		"id":           "LumbarMRICQL",
		"url":          "http://smarthealth.network/fhir/Library/LumbarMRICQL",
		"name":         "LumbarMRICQL",
		"version":      "1.0.0",
		"status":       "active",
		"type": map[string]any{"coding": []map[string]any{
			{"system": "http://terminology.hl7.org/CodeSystem/library-type", "code": "logic-library"}}},
		"content": []map[string]any{
			{"contentType": "text/cql", "data": base64.StdEncoding.EncodeToString([]byte(cql))},
		},
	}
	return json.Marshal(lib)
}

// PutGlobalArtifact $validates (FR-36; warnings OK — Valid counts only error/fatal)
// then PUTs a non-partitionable knowledge artifact (e.g. Library) to the given base,
// typically …/fhir/DEFAULT. Sibling of (*Client).InstallCRLibraries for artifacts
// callers build themselves, such as SandboxLumbarLibrary. No scoped id: DEFAULT is
// not partitioned, so the usual cross-partition id-uniqueness concern does not apply.
func PutGlobalArtifact(ctx context.Context, base string, v shnsdk.Validator, rtype, id string, body []byte) error {
	res, verr := v.Validate(ctx, body, "")
	if verr != nil {
		return fmt.Errorf("$validate %s/%s: %w", rtype, id, verr)
	}
	if !res.Valid {
		return fmt.Errorf("%s/%s failed validation: %v", rtype, id, res.Issues)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/"+rtype+"/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	resp, err := putClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT %s/%s: status %d: %s", rtype, id, resp.StatusCode, rb)
	}
	return nil
}
