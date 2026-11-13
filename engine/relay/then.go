package relay

import (
	"fmt"
	"slices"
)

// Then applies further registered changes to p, a payload relayed exactly or
// already edited: build is handed a Document over p's own bytes (as a Body of
// origin o) and returns the changes to make, each carried out by operations
// from that Document. The changes are checked as ApplyChanges checks them,
// against p's bytes. The result keeps p's edits, then the new ones in order,
// and names every signature an exempt edit of either changed. When build
// returns no change, or no change alters a byte, the result is p itself. A
// payload the gateway built (OwnershipAuthored) is refused: its own edits go
// through Transformed. So is one with a header edit (HeaderEdited): that edit
// is made last.
func Then(p Payload, o Origin, build func(d *Document) ([]Change, error)) (Payload, error) {
	if p.c == nil {
		return Payload{}, fmt.Errorf("%w: no payload to edit", ErrInvalidEdit)
	}
	if p.c.own != OwnershipRelayed && p.c.own != OwnershipEdited {
		return Payload{}, fmt.Errorf("%w: only a relayed or edited payload takes further edits", ErrInvalidEdit)
	}
	if p.c.header != nil {
		// A header edit is the answer's last: its row checked it on the
		// body as relayed, and the result would name it without its header.
		return Payload{}, fmt.Errorf("%w: a payload with a header edit takes no further edits", ErrInvalidEdit)
	}
	body := NewBody(p.c.b.get(), o)
	doc, err := Doc(body)
	if err != nil {
		return Payload{}, err
	}
	changes, err := build(doc)
	if err != nil {
		return Payload{}, err
	}
	if len(changes) == 0 {
		return p, nil
	}
	next, err := ApplyChanges(body, p.c.contentType, changes...)
	if err != nil {
		return Payload{}, err
	}
	if next.c.own != OwnershipEdited {
		return p, nil
	}
	edits := slices.Clone(p.c.edits)
	for _, e := range next.c.edits {
		if !slices.Contains(edits, e) {
			edits = append(edits, e)
		}
	}
	signed := slices.Clone(p.c.signed)
	for _, s := range next.c.signed {
		if !slices.Contains(signed, s) {
			signed = append(signed, s)
		}
	}
	return Payload{c: &sealed{b: next.c.b, own: OwnershipEdited, edits: edits, builder: p.c.builder, contentType: p.c.contentType, signed: signed}}, nil
}
