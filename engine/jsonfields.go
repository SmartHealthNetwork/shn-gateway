package engine

import (
	"bytes"
	"encoding/json"
)

// jsonfields.go holds small readers and writers of FHIR JSON fields that the
// core and module 1 both use. They name no workstream.

func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// questionnaireResponseSubject returns the QR's subject.reference.
func questionnaireResponseSubject(qrJSON []byte) (string, error) {
	var probe struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(qrJSON, &probe); err != nil {
		return "", err
	}
	return probe.Subject.Reference, nil
}
