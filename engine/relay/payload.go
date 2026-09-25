package relay

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"slices"
	"strconv"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

var (
	// ErrUnsetOwnership: Transmit was handed a Payload that no constructor
	// made (the zero value, or the result of a refused build).
	ErrUnsetOwnership = errors.New("relay: payload has no ownership")
	// ErrNoOwnershipCheck: Transmit was called without a permission check.
	ErrNoOwnershipCheck = errors.New("relay: transmit needs a permission check")
	// ErrUnknownEdit: Apply names an edit that is not registered, or has no
	// change to apply.
	ErrUnknownEdit = errors.New("relay: unregistered edit")
	// ErrInvalidEdit: an edit operation cannot be applied to the body (a
	// missing or wrong-kind target, an invalid value, overlapping
	// operations, or a name the object already has). This is a fault in the
	// gateway, not in the peer's message.
	ErrInvalidEdit = errors.New("relay: edit cannot be applied")
	// ErrUnknownBuilder: Authored names a builder that is not registered.
	ErrUnknownBuilder = errors.New("relay: unregistered builder")
	// ErrReservedBuilder: Authored names the builder id reserved for tests.
	ErrReservedBuilder = errors.New("relay: reserved builder id")
	// ErrContentType: Authored was given an empty or malformed media type.
	ErrContentType = errors.New("relay: invalid content type")
	// ErrEmbedMismatch: a declared embedded span is not a byte-identical,
	// whole JSON value copied from its source.
	ErrEmbedMismatch = errors.New("relay: embedded span does not match its source")
)

// Ownership says whose message a Payload is. The zero value is invalid.
type Ownership uint8

const (
	// OwnershipRelayed: a peer's message, byte for byte.
	OwnershipRelayed Ownership = iota + 1
	// OwnershipEdited: a peer's message with registered, verified edits.
	OwnershipEdited
	// OwnershipAuthored: the gateway's own message from a registered
	// builder.
	OwnershipAuthored
)

func (o Ownership) String() string {
	switch o {
	case 0:
		return "unset"
	case OwnershipRelayed:
		return "relayed"
	case OwnershipEdited:
		return "edited"
	case OwnershipAuthored:
		return "authored"
	}
	return "Ownership(" + strconv.Itoa(int(o)) + ")"
}

// EditID names a registered edit. The set is closed; each edit is
// disclosed to participants.
type EditID string

const (
	// EditCDSCallbackStrip removes a CDS Hooks request's fhirServer and
	// fhirAuthorization, so a payer never receives a route into the
	// provider's system.
	EditCDSCallbackStrip EditID = "E-01"
	// EditCDSPrefetchObtain adds a prefetch value the request left out,
	// read from the provider's own system.
	EditCDSPrefetchObtain EditID = "E-02"
	// EditPayorEdgeRestamp maps the payer identity tokens of Coverage (and
	// a claim's insurer) to the identity the payer's own system expects.
	EditPayorEdgeRestamp EditID = "E-03"
	// EditDTRCoverageObtain adds the patient's Coverage to a questionnaire
	// package request that carried none, read from the provider's own
	// system.
	EditDTRCoverageObtain EditID = "E-04"
	// EditDTRPatientObtain adds the provider's own Patient record for the bound
	// patient, as a referenced resource, to a questionnaire package request
	// that carried no Patient, so a payer that does not hold the member binds
	// the same subject from the request. An enrichment: not applied to
	// Da Vinci-native traffic unless the participant opts in.
	EditDTRPatientObtain EditID = "E-05"
)

var editIDs = []EditID{EditCDSCallbackStrip, EditCDSPrefetchObtain, EditPayorEdgeRestamp, EditDTRCoverageObtain, EditDTRPatientObtain}

// EditIDs returns every registered edit id, in order.
func EditIDs() []EditID { return slices.Clone(editIDs) }

// Payload is a message a gateway may send. Make one with Exact, Apply,
// ApplyChanges or Authored; read its bytes only through Transmit.
type Payload struct {
	c *sealed
}

// sealed sits behind a pointer so that printing a struct that holds a
// Payload shows an address, never the bytes.
type sealed struct {
	b           sealedBytes
	own         Ownership
	edits       []EditID
	builder     BuilderID
	contentType string
}

// Ownership reports how the payload was made (0 for the zero Payload).
func (p Payload) Ownership() Ownership {
	if p.c == nil {
		return 0
	}
	return p.c.own
}

// Edits returns a copy of the edits applied, in the order declared; nil
// unless the payload is OwnershipEdited.
func (p Payload) Edits() []EditID {
	if p.c == nil {
		return nil
	}
	return slices.Clone(p.c.edits)
}

// Builder returns the builder id of an OwnershipAuthored payload.
func (p Payload) Builder() BuilderID {
	if p.c == nil {
		return ""
	}
	return p.c.builder
}

// ContentType returns the media type the payload is sent with.
func (p Payload) ContentType() string {
	if p.c == nil {
		return ""
	}
	return p.c.contentType
}

// Len returns the payload's length in bytes.
func (p Payload) Len() int {
	if p.c == nil {
		return 0
	}
	return len(p.c.b.get())
}

// Format prints a summary for every verb; the bytes are never printed.
func (p Payload) Format(f fmt.State, _ rune) {
	if p.c == nil {
		fmt.Fprint(f, "relay.Payload{unset}")
		return
	}
	parts := []string{p.c.own.String()}
	if len(p.c.edits) > 0 {
		ids := make([]string, len(p.c.edits))
		for i, e := range p.c.edits {
			ids[i] = string(e)
		}
		parts = append(parts, "["+strings.Join(ids, ",")+"]")
	}
	if p.c.builder != "" {
		parts = append(parts, string(p.c.builder))
	}
	if p.c.contentType != "" {
		parts = append(parts, p.c.contentType)
	}
	parts = append(parts, strconv.Itoa(len(p.c.b.get()))+" bytes")
	fmt.Fprint(f, "relay.Payload{"+strings.Join(parts, " ")+"}")
}

// Exact relays body unchanged.
func Exact(body Body, contentType string) Payload {
	return Payload{c: &sealed{b: seal(body.bytes()), own: OwnershipRelayed, contentType: contentType}}
}

// Change is one registered edit and the operations that carry it out. The
// operations must come from Doc on the same Body.
type Change struct {
	Edit EditID
	Ops  []Op
}

// Apply applies one registered edit to body. See ApplyChanges.
func Apply(body Body, contentType string, id EditID, ops ...Op) (Payload, error) {
	return ApplyChanges(body, contentType, Change{Edit: id, Ops: ops})
}

// spliceHook performs the splice; tests replace it to inject faults.
var spliceHook = func(d *splice.Doc, ops ...splice.Op) ([]byte, []splice.Edit, error) {
	return d.Apply(ops...)
}

// ApplyChanges applies registered edits to body, all resolved against the
// body as received, and returns the result as OwnershipEdited.
//
// The body is scanned strictly first (the scan refusals Decode lists). The
// edited bytes are then checked independently of the code that produced them (see
// model.check), and any disagreement is ErrVerifyMismatch:
//
//   - the output is the original with only the splice's reported spans
//     substituted;
//   - each reported span is either exactly a replaced value, or holds at
//     least one declared removal or addition and otherwise only whitespace
//     and commas;
//   - a fresh scan of the output holds exactly the declared values at the
//     declared places, every surviving member keeps its name token byte for
//     byte, and every value no operation addresses is unchanged.
//
// Not pinned byte for byte: the whitespace and the one comma that separate
// a removed or added member (or element) from its neighbours, the spelling
// of an inserted member's name (it is compared after unescaping), and the
// whitespace inside an object the change creates.
//
// A change that would alter content covered by an in-payload signature is
// refused with a *SignedContentError naming the change's edit. An
// operation that leaves the bytes as they were (a same-value replace, or
// ensuring an object that already exists) is not an edit: when no
// operation changes anything, the result is the body relayed exactly, and
// Edits lists only the changes that altered bytes.
func ApplyChanges(body Body, contentType string, changes ...Change) (Payload, error) {
	if len(changes) == 0 {
		return Payload{}, fmt.Errorf("%w: no change given", ErrUnknownEdit)
	}
	for _, c := range changes {
		if !slices.Contains(editIDs, c.Edit) {
			return Payload{}, fmt.Errorf("%w: %q", ErrUnknownEdit, c.Edit)
		}
	}
	src := body.bytes()
	doc, err := scan(src)
	if err != nil {
		return Payload{}, err
	}
	var ops []splice.Op
	for _, c := range changes {
		for _, op := range c.Ops {
			if op.op == nil {
				return Payload{}, fmt.Errorf("%w: zero operation in %s", ErrInvalidEdit, c.Edit)
			}
			if op.body != body.c {
				return Payload{}, fmt.Errorf("%w: an operation in %s was made from another body's Document", ErrInvalidEdit, c.Edit)
			}
			ops = append(ops, op.op)
		}
	}
	// The expected result is derived from the operations before the splice
	// runs, so a faulty splice cannot influence it.
	m, err := buildModel(doc, src, ops)
	if err != nil {
		return Payload{}, fmt.Errorf("%w: %v", ErrInvalidEdit, err)
	}
	out, spans, err := spliceHook(doc, ops...)
	if err != nil {
		// The splice's own errors are reported as text only: a name
		// collision here is the gateway's fault, not a duplicate in the
		// peer's message.
		return Payload{}, fmt.Errorf("%w: %v", ErrInvalidEdit, err)
	}
	if err := m.check(out, spans); err != nil {
		return Payload{}, err
	}
	if bytes.Equal(out, src) {
		return Exact(body, contentType), nil
	}
	cov := coverage(doc)
	var applied []EditID
	i := 0
	for _, c := range changes {
		changed := false
		for range c.Ops {
			eff := m.effects[i]
			i++
			if eff == nil {
				continue
			}
			changed = true
			if carrier, hit := cov.hit(*eff); hit {
				return Payload{}, &SignedContentError{Edit: c.Edit, Carrier: carrier}
			}
		}
		if changed && !slices.Contains(applied, c.Edit) {
			applied = append(applied, c.Edit)
		}
	}
	if len(applied) == 0 {
		// The output changed although no operation declares a change.
		return Payload{}, fmt.Errorf("%w: output changed but no operation did", ErrVerifyMismatch)
	}
	return Payload{c: &sealed{b: seal(out), own: OwnershipEdited, edits: applied, contentType: contentType}}, nil
}

// Embed declares that b[At:At+(End-Start)] is a copy of Source's bytes
// [Start, End), which must be one whole JSON value in Source.
type Embed struct {
	Source     Body
	Start, End int
	At         int
}

// Authored seals b as the gateway's own message from builder id.
//
// id must be registered (ErrUnknownBuilder); the id reserved for tests is
// refused (ErrReservedBuilder). contentType must be a valid media type
// (ErrContentType). A non-empty body with a JSON media type
// (application/json or any +json type) must be one well-formed document
// with no repeated member names (see Decode); other media types and empty
// bodies are not scanned. Each embed is verified exactly
// (ErrEmbedMismatch): the source span is a whole JSON value in its source,
// the bytes at At are identical to it, and, when b was scanned, the
// destination is a whole JSON value in b.
func Authored(id BuilderID, b []byte, contentType string, embeds ...Embed) (Payload, error) {
	if id == builderTestInjected {
		return Payload{}, fmt.Errorf("%w: %q", ErrReservedBuilder, id)
	}
	if _, ok := authoredBuilders[id]; !ok {
		return Payload{}, fmt.Errorf("%w: %q", ErrUnknownBuilder, id)
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return Payload{}, fmt.Errorf("%w: %q: %v", ErrContentType, contentType, err)
	}
	b = bytes.Clone(b)
	var doc *splice.Doc
	if len(b) > 0 && (mt == "application/json" || strings.HasSuffix(mt, "+json")) {
		if doc, err = scan(b); err != nil {
			return Payload{}, err
		}
	}
	if err := verifyEmbeds(b, doc, embeds); err != nil {
		return Payload{}, err
	}
	return Payload{c: &sealed{b: seal(b), own: OwnershipAuthored, builder: id, contentType: contentType}}, nil
}

// valueSpans maps the start offset of every value in d to its end.
func valueSpans(d *splice.Doc) map[int]int {
	m := make(map[int]int, d.Len())
	for i := 0; i < d.Len(); i++ {
		s, e := d.Span(splice.NodeID(i))
		m[s] = e
	}
	return m
}

// verifyEmbeds checks every embed, scanning each distinct source once.
func verifyEmbeds(b []byte, doc *splice.Doc, embeds []Embed) error {
	if len(embeds) == 0 {
		return nil
	}
	var dst map[int]int
	if doc != nil {
		dst = valueSpans(doc)
	}
	type source struct {
		spans map[int]int
		err   error
	}
	sources := map[*bodyContent]*source{}
	for i, e := range embeds {
		src := sources[e.Source.c]
		if src == nil {
			src = &source{}
			if d, err := scan(e.Source.bytes()); err != nil {
				src.err = err
			} else {
				src.spans = valueSpans(d)
			}
			sources[e.Source.c] = src
		}
		if err := verifyEmbed(b, dst, e, src.spans, src.err); err != nil {
			return fmt.Errorf("%w: embed %d: %v", ErrEmbedMismatch, i, err)
		}
	}
	return nil
}

func verifyEmbed(b []byte, dst map[int]int, e Embed, srcSpans map[int]int, srcErr error) error {
	src := e.Source.bytes()
	if e.Start < 0 || e.End > len(src) || e.Start >= e.End {
		return fmt.Errorf("source span [%d,%d) outside a %d-byte source", e.Start, e.End, len(src))
	}
	n := e.End - e.Start
	if e.At < 0 || e.At > len(b)-n {
		return fmt.Errorf("destination %d+%d outside a %d-byte body", e.At, n, len(b))
	}
	if !bytes.Equal(b[e.At:e.At+n], src[e.Start:e.End]) {
		return fmt.Errorf("bytes at %d differ from the source span", e.At)
	}
	if srcErr != nil {
		return fmt.Errorf("source: %v", srcErr)
	}
	if end, ok := srcSpans[e.Start]; !ok || end != e.End {
		return fmt.Errorf("source span [%d,%d) is not a whole JSON value", e.Start, e.End)
	}
	if dst != nil {
		if end, ok := dst[e.At]; !ok || end != e.At+n {
			return fmt.Errorf("destination [%d,%d) is not a whole JSON value", e.At, e.At+n)
		}
	}
	return nil
}

// Transmit returns a copy of p's bytes for sending, after check admits p.
// A Payload that no constructor made is refused with ErrUnsetOwnership
// before check runs; a nil check is ErrNoOwnershipCheck; a refusal from
// check is returned wrapped. This is the only way to read a Payload's
// bytes.
func Transmit(p Payload, check func(Payload) error) ([]byte, error) {
	if p.Ownership() == 0 {
		return nil, ErrUnsetOwnership
	}
	if check == nil {
		return nil, ErrNoOwnershipCheck
	}
	if err := check(p); err != nil {
		return nil, fmt.Errorf("relay: transmit refused (%s): %w", p.Ownership(), err)
	}
	return bytes.Clone(p.c.b.get()), nil
}
