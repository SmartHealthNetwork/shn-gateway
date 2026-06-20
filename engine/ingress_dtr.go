// ingress_dtr.go — DTR ($questionnaire-package) ingress: terminate the inbound SDC fetch and
// drive the EXISTING dtr-questionnaire-fetch substrate leg (near-relay). The ingress does NOT
// invoke the Populator and there is no $populate route — br-provider's own DTR app populates
// locally. It extracts the questionnaire canonical (and, for per-patient authz, the coverage
// beneficiary) and relays the package Bundle response.
package engine

import (
	"encoding/json"
)

// dtrPackageParams models the patient-bearing + canonical fields of a $questionnaire-package
// Parameters request.
type dtrPackageParams struct {
	Parameter []struct {
		Name           string          `json:"name"`
		ValueCanonical string          `json:"valueCanonical"`
		Resource       json.RawMessage `json:"resource"`
	} `json:"parameter"`
}

// dtrFromPackageParams extracts the questionnaire canonical (required) and a patient reference
// (from the coverage beneficiary, if present) from a $questionnaire-package request.
func dtrFromPackageParams(body []byte) (canonical, patientRef string, ok bool) {
	var p dtrPackageParams
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", false
	}
	for _, param := range p.Parameter {
		switch param.Name {
		case "questionnaire":
			canonical = param.ValueCanonical
		case "coverage":
			patientRef = patientRefOf(param.Resource) // beneficiary.reference
		}
	}
	if canonical == "" {
		return "", "", false
	}
	return canonical, patientRef, true
}
