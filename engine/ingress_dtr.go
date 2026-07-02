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

// dtrFromPackageParams extracts the questionnaire canonical (required), a patient reference
// (from the coverage beneficiary, if present, for per-patient authz), and the raw Coverage
// resource bytes (the `coverage` param's resource, if present) from a $questionnaire-package
// request. The Coverage is carried VERBATIM through the dtr-questionnaire-fetch leg so the
// native-forward rebuild can re-emit it as the payer-required `coverage` parameter
// (FR-G28); it is nil when the request carried no coverage (the sandbox / 8-UC-demo path).
func dtrFromPackageParams(body []byte) (canonical, patientRef string, coverage json.RawMessage, ok bool) {
	var p dtrPackageParams
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", nil, false
	}
	for _, param := range p.Parameter {
		switch param.Name {
		case "questionnaire":
			canonical = param.ValueCanonical
		case "coverage":
			patientRef = patientRefOf(param.Resource) // beneficiary.reference
			coverage = param.Resource                 // carried verbatim through the leg
		}
	}
	if canonical == "" {
		return "", "", nil, false
	}
	return canonical, patientRef, coverage, true
}

// dtrParamResources flattens a $questionnaire-package request's parameter[].resource blobs into a
// resource list for resolverFromResources: an EXTERNAL payor Organization the partner carries
// alongside the coverage parameter resolves from here, not the provider SoR (Finding 1).
func dtrParamResources(body []byte) [][]byte {
	var p dtrPackageParams
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	out := make([][]byte, 0, len(p.Parameter))
	for _, param := range p.Parameter {
		if len(param.Resource) > 0 {
			out = append(out, param.Resource)
		}
	}
	return out
}
