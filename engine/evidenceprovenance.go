package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildEvidenceProvenance attributes evidence using the identity actually known
// by its producer (FR-32): a clinician NPI or an authenticated holder identifier.
// Neither business identifier asserts that a REST Practitioner/Organization
// resource exists. Patient resource attribution continues using BuildProvenance.
func buildEvidenceProvenance(target, system, value, policy, purpose string, recorded time.Time) ([]byte, error) {
	if strings.TrimSpace(value) == "" || (system != "http://hl7.org/fhir/sid/us-npi" && system != "http://smarthealth.network/ids/holder") {
		return nil, errors.New("missing evidence agent identity")
	}
	raw, err := shnsdk.BuildProvenanceWithPolicy(target, "", policy, purpose, recorded)
	if err != nil {
		return nil, err
	}
	var p map[string]json.RawMessage
	if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p["agent"], err = json.Marshal([]any{map[string]any{"who": map[string]any{"identifier": map[string]string{"system": system, "value": value}}}})
	if err != nil {
		return nil, err
	}
	return json.Marshal(p)
}
