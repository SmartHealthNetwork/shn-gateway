// Package relay carries participants' message bodies through a gateway
// without changing them by accident.
//
// A body read from a peer is a Body: immutable, with no exported way to get
// its bytes back. Code that needs to look inside reads it with Decode (values
// only) or Doc (a read-only structural view). Nothing produced that way can
// be sent.
//
// The only type a gateway sends is a Payload, and its bytes are reachable
// only through Transmit, which first runs the caller's permission check.
// A Payload is made in exactly one of three ways, and records which:
//
//   - Exact: the peer's bytes, unchanged (OwnershipRelayed);
//   - Apply / ApplyChanges: the peer's bytes with registered edits, checked
//     independently after the edit and refused inside signed content
//     (OwnershipEdited);
//   - Authored: the gateway's own message, from a registered builder, with
//     any embedded peer content proven byte-identical to its source
//     (OwnershipAuthored).
//
// The zero Payload has no ownership and is refused by Transmit.
//
// Neither Body nor Payload prints or marshals its bytes: fmt shows a short
// summary and encoding/json yields {}.
package relay

import (
	"fmt"
	"strconv"
)

// Origin says where a Body came from.
type Origin uint8

const (
	// OriginIngressRequest is a request a participant's own system sent to
	// its gateway.
	OriginIngressRequest Origin = iota + 1
	// OriginUpstreamResponse is a response from the participant's own system
	// to a request its gateway made.
	OriginUpstreamResponse
	// OriginPeerFrame is a body that arrived from another participant's
	// gateway through the network.
	OriginPeerFrame
)

func (o Origin) String() string {
	switch o {
	case OriginIngressRequest:
		return "ingress-request"
	case OriginUpstreamResponse:
		return "upstream-response"
	case OriginPeerFrame:
		return "peer-frame"
	}
	return "invalid"
}

// Body is an immutable message body received from a peer. The zero Body is
// empty.
type Body struct {
	c *bodyContent
}

// bodyContent sits behind a pointer so that printing a struct that holds a
// Body shows an address, never the bytes.
type bodyContent struct {
	b      sealedBytes
	origin Origin
}

// sealedBytes keeps bytes one more pointer away. fmt prints the contents of
// a pointer it is handed with an unsupported verb, but only one level deep,
// so a value holding a Body or Payload in an unexported field still prints
// no bytes.
type sealedBytes struct{ p *[]byte }

func seal(b []byte) sealedBytes { return sealedBytes{p: &b} }

func (s sealedBytes) get() []byte {
	if s.p == nil {
		return nil
	}
	return *s.p
}

// NewBody seals a copy of b.
func NewBody(b []byte, o Origin) Body {
	return Body{c: &bodyContent{b: seal(append([]byte{}, b...)), origin: o}}
}

// Origin reports where the body came from (0 for the zero Body).
func (b Body) Origin() Origin {
	if b.c == nil {
		return 0
	}
	return b.c.origin
}

// Len reports the body's length in bytes.
func (b Body) Len() int { return len(b.bytes()) }

func (b Body) bytes() []byte {
	if b.c == nil {
		return nil
	}
	return b.c.b.get()
}

// Format prints a summary for every verb; the bytes are never printed.
func (b Body) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "relay.Body{%s %s bytes}", b.Origin(), strconv.Itoa(b.Len()))
}
