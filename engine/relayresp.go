// gateway/engine/relayresp.go
package engine

import "fmt"

// RelayError carries a recipient's non-2xx application answer back up the
// origination call chain as a typed sentinel — mirroring errAuthorizationDenied
// (gateway.go). The byte-returning compatibility wrappers convert an
// ApplicationReply with a non-2xx status to this sentinel. The message API
// returns the application answer directly.
// Signatures stay ([]byte, error); every OriginateLeg caller's existing
// `if err != nil` guard aborts the exchange correctly, and the ingress handlers
// unwrap it (errors.As) to surface the recipient's real status + body.
type RelayError struct {
	Status int
	Body   []byte
	// ContentType is the framed answer's allowlisted Content-Type (empty when the
	// frame carried none); origination/ingress relay writers use it verbatim.
	ContentType string
	// leg is the leg the answer came back on.
	leg string
}

func (e *RelayError) Error() string {
	return fmt.Sprintf("recipient answered %d (%d bytes)", e.Status, len(e.Body))
}
