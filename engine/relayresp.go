// gateway/engine/relayresp.go
package engine

import "fmt"

// RelayError carries a recipient's non-2xx application answer back up the
// origination call chain as a typed sentinel — mirroring errAuthorizationDenied
// (gateway.go). It is fed by roundTripInner decoding a frame-negotiated recipient's
// v1 message frame: a non-2xx frame status becomes this sentinel.
// Signatures stay ([]byte, error); every OriginateLeg caller's existing
// `if err != nil` guard aborts the exchange correctly, and the ingress handlers
// unwrap it (errors.As) to surface the recipient's real status + body.
type RelayError struct {
	Status int
	Body   []byte
	// ContentType is the framed answer's allowlisted Content-Type (empty when the
	// frame carried none); origination/ingress relay writers use it verbatim.
	ContentType string
}

func (e *RelayError) Error() string {
	return fmt.Sprintf("recipient answered %d (%d bytes)", e.Status, len(e.Body))
}
