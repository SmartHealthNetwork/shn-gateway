// gateway/engine/relayresp.go
package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// relayResponse is the sealed response payload for a recipient's NON-2xx answer.
// Only non-2xx responses are wrapped; a success answer stays bare FHIR and the
// originator reads a bare payload as an implicit 200. The wrapper rides inside the
// sealed ciphertext, so the status inherits the response-leg token's
// sha256(ciphertext) binding (tamper-proof; the Hub never sees it).
//
// The "__shn"-prefixed keys cannot collide with a bare FHIR body: FHIR JSON never
// uses a "__"-prefixed top-level field (its primitive-extension siblings are a
// single "_", e.g. "_status").
type relayResponse struct {
	Status  int             `json:"__shnStatus"`
	Body    json.RawMessage `json:"__shnBody,omitempty"`
	BodyB64 bool            `json:"__shnBodyB64,omitempty"`
}

// wrapRelayResponse builds the sealed payload for a non-2xx recipient answer.
// A valid-JSON body is embedded verbatim (no base64 inflation); anything else is
// base64'd with BodyB64=true.
func wrapRelayResponse(status int, body []byte) []byte {
	rr := relayResponse{Status: status}
	if json.Valid(body) {
		rr.Body = json.RawMessage(body)
	} else {
		rr.BodyB64 = true
		// json.Marshal produces a correctly-escaped JSON string (idiomatic + collision-proof).
		enc, _ := json.Marshal(base64.StdEncoding.EncodeToString(body))
		rr.Body = json.RawMessage(enc)
	}
	out, err := json.Marshal(rr)
	if err != nil { // body was declared valid JSON or a quoted string — marshal cannot fail
		return body
	}
	return out
}

// unwrapRelayResponse reports whether payload is a relay wrapper and, if so, the
// carried status + verbatim body. A bare (non-wrapper) payload yields (200, payload, false).
func unwrapRelayResponse(payload []byte) (status int, body []byte, wrapped bool) {
	var rr relayResponse
	if err := json.Unmarshal(payload, &rr); err != nil || rr.Status == 0 {
		return 200, payload, false
	}
	if rr.BodyB64 {
		var s string
		if err := json.Unmarshal(rr.Body, &s); err == nil {
			if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
				return rr.Status, dec, true
			}
		}
		return rr.Status, nil, true
	}
	return rr.Status, []byte(rr.Body), true
}

// RelayError carries a recipient's non-2xx application answer back up the
// origination call chain as a typed sentinel — mirroring errAuthorizationDenied
// (gateway.go). Signatures stay ([]byte, error); every OriginateLeg caller's
// existing `if err != nil` guard aborts the exchange correctly, and the ingress
// handlers unwrap it (errors.As) to surface the recipient's real status + body.
type RelayError struct {
	Status int
	Body   []byte
}

func (e *RelayError) Error() string {
	return fmt.Sprintf("recipient answered %d (%d bytes)", e.Status, len(e.Body))
}
