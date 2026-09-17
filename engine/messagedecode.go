package engine

import (
	"encoding/json"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// decodeMessage reads a participant's message body (or a value inside one)
// for an authority or routing decision: the subject fences, the attestation
// fence, recipient routing and leg selection. It goes through relay.Decode,
// so a repeated member name, or a member whose name matches a field only when
// case is ignored, is refused: every such decision reads the one
// interpretation the recipient will read. Decode reads a body the same way
// whichever side it came from.
func decodeMessage(b []byte, v any) error {
	return relay.Decode(relay.NewBody(b, relay.OriginPeerFrame), v)
}

// scanMessage checks that b is one well-formed JSON document with no repeated
// member names (exactly, or equal under case folding), without decoding it.
func scanMessage(b []byte) error {
	var whole json.RawMessage
	return decodeMessage(b, &whole)
}
