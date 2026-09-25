package engine

import (
	"encoding/json"
	"errors"

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

// valueTypeError reports whether err is only a value that does not fit its
// field: the body is one JSON document with no repeated member name, and every
// member name matches its field exactly.
func valueTypeError(err error) bool {
	var te *json.UnmarshalTypeError
	return errors.As(err, &te)
}

// decodeContent is decodeMessage for a request whose fields outside core are
// the participant's own content. When decoding fails only because a value does
// not fit its field (valueTypeError), core (the fields on the subject and
// routing path, decoded on their own) reads, and refuses (the content rule's
// decision, RuleRequestShape) does not refuse, v holds every value that fit and
// the error is nil. Every other failure is returned as decodeMessage returns
// it: a body that is not one JSON document or repeats a member name, a member
// name that reaches a field only by case folding, and a core field that cannot
// be read.
func decodeContent(b []byte, v, core any, refuses func() bool) error {
	err := decodeMessage(b, v)
	if err == nil || !valueTypeError(err) || decodeMessage(b, core) != nil || refuses() {
		return err
	}
	return nil
}
