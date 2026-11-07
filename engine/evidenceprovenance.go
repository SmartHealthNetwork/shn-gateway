package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildEvidenceProvenance attributes evidence using the identity actually known
// by its producer (FR-32): a clinician NPI or an authenticated holder identifier.
// Neither business identifier asserts that a REST Practitioner/Organization
// resource exists. The Provenance has an id, id or, when id is empty,
// shnsdk.BuildProvenanceRecord's default for target: an amendment carries it
// as written, under a fullUrl built from that id.
func buildEvidenceProvenance(id, target, system, value, policy, purpose string, recorded time.Time) ([]byte, error) {
	if strings.TrimSpace(value) == "" || (system != "http://hl7.org/fhir/sid/us-npi" && system != "http://smarthealth.network/ids/holder") {
		return nil, errors.New("missing evidence agent identity")
	}
	return shnsdk.BuildProvenanceRecord(shnsdk.ProvenanceInputs{
		ID: id, Target: target, AgentIdentifier: &shnsdk.ProvenanceIdentifier{System: system, Value: value},
		Policy: policy, PurposeOfUse: purpose, Recorded: recorded,
	})
}

// amendmentProvenanceID is the id of the Provenance an amendment sent under
// correlation corr carries: a Provenance records one activity, and each
// amendment is one, so two amendments attributing one report never share an
// id. It is "prov-<corr>" when that is a FHIR id, else "prov-" and the first
// 16 hex digits of the SHA-256 of corr.
func amendmentProvenanceID(corr string) string {
	if id := "prov-" + corr; idShape.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(corr))
	return "prov-" + hex.EncodeToString(sum[:8])
}
