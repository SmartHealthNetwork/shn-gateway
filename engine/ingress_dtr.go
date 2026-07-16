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

// dtrFromPackageParams extracts the questionnaire canonical, a patient reference (from the
// coverage beneficiary, if present, for per-patient authz), the raw Coverage resource bytes, and
// the raw `order` resource bytes from a $questionnaire-package request. The Coverage + order are
// carried VERBATIM through the dtr-questionnaire-fetch leg so the native-forward rebuild can
// re-emit them (FR-G28); coverage/order are nil when absent (the sandbox / 8-UC-demo path carries
// neither). A request is valid when it carries EITHER a `questionnaire` canonical (the SDC /
// br-payer path) OR an `order` (a partner whose $questionnaire-package keys off the
// CRD-updated order's coverage-assertion-id and has no `questionnaire` param support).
func dtrFromPackageParams(body []byte) (canonical, patientRef string, coverage, order json.RawMessage, ok bool) {
	var p dtrPackageParams
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", nil, nil, false
	}
	for _, param := range p.Parameter {
		switch param.Name {
		case "questionnaire":
			canonical = param.ValueCanonical
		case "coverage":
			patientRef = patientRefOf(param.Resource) // beneficiary.reference
			coverage = param.Resource                 // carried verbatim through the leg
		case "order":
			order = param.Resource // carried verbatim; the payer re-emits it as the `order` param
			if patientRef == "" {
				patientRef = orderSubjectOf(param.Resource) // subject.reference, for authz when no coverage
			}
		}
	}
	if canonical == "" && len(order) == 0 {
		return "", "", nil, nil, false
	}
	return canonical, patientRef, coverage, order, true
}

// orderSubjectOf reads subject.reference from an order (ServiceRequest) resource; "" if absent.
func orderSubjectOf(resource json.RawMessage) string {
	var probe struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	_ = json.Unmarshal(resource, &probe)
	return probe.Subject.Reference
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
