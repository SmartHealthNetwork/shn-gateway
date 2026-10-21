package relay

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A registered transform (E-09) carries the gateway's own message to the
// peer's IG line: the builder's message, then the edit, verified by running
// the registered steps again. Each row below is the valid transform with one
// thing wrong.

// toLine21 is a deterministic stand-in for the registered steps: it rewrites
// the whole document.
func toLine21(b []byte) ([]byte, error) {
	return bytes.Replace(b, []byte(`"line":"2.0"`), []byte(`"line":"2.1"`), 1), nil
}

func builtAt20(t *testing.T) Payload {
	t.Helper()
	p, err := Authored(BuilderSDKPASSubmit, []byte(`{"resourceType":"Bundle","line":"2.0"}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTransformedKeepsTheBuilder(t *testing.T) {
	src := builtAt20(t)
	out, _ := toLine21([]byte(`{"resourceType":"Bundle","line":"2.0"}`))
	p, err := Transformed(src, EditRegisteredTransform, out, toLine21)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{EditRegisteredTransform}) || p.Builder() != BuilderSDKPASSubmit || p.ContentType() != fhirJSON {
		t.Fatalf("transformed = %v: want edited [E-09] by sdk-pas-submit", p)
	}
	got, err := Transmit(p, func(Payload) error { return nil })
	if err != nil || !bytes.Equal(got, out) {
		t.Fatalf("sent %s, %v; want %s", got, err, out)
	}
	// A walk that changes nothing is no edit: the builder's message as built.
	same, err := Transformed(src, EditRegisteredTransform, []byte(`{"resourceType":"Bundle","line":"2.0"}`), func(b []byte) ([]byte, error) { return b, nil })
	if err != nil || same.Ownership() != OwnershipAuthored || same.Edits() != nil || same.Builder() != BuilderSDKPASSubmit {
		t.Fatalf("an unchanged walk = %v, %v; want the authored payload", same, err)
	}
}

func TestTransformedRefuses(t *testing.T) {
	src := builtAt20(t)
	out, _ := toLine21([]byte(`{"resourceType":"Bundle","line":"2.0"}`))
	calls := 0
	flaky := func(b []byte) ([]byte, error) {
		calls++
		if calls%2 == 0 {
			return append(bytes.Clone(out), ' '), nil
		}
		return bytes.Clone(out), nil
	}
	exact := Exact(NewBody([]byte(`{"line":"2.0"}`), OriginPeerFrame), fhirJSON)
	for _, r := range []struct {
		name     string
		src      Payload
		id       EditID
		out      []byte
		rederive func([]byte) ([]byte, error)
		want     error
		says     string
	}{
		{"output the steps did not make", src, EditRegisteredTransform, []byte(`{"resourceType":"Bundle","line":"2.2"}`), toLine21, ErrVerifyMismatch, "not what the registered steps make"},
		{"steps that are not deterministic", src, EditRegisteredTransform, out, flaky, ErrVerifyMismatch, "not deterministic"},
		{"steps that refuse", src, EditRegisteredTransform, out, func([]byte) ([]byte, error) { return nil, errors.New("gated") }, ErrVerifyMismatch, "refused"},
		{"no steps to verify against", src, EditRegisteredTransform, out, nil, ErrInvalidEdit, "no steps"},
		{"a participant's message", exact, EditRegisteredTransform, out, toLine21, ErrInvalidEdit, "only the gateway's own message"},
		{"an edit that is not a transform", src, EditCDSCallbackStrip, out, toLine21, ErrInvalidEdit, "not a registered transform"},
		{"an unregistered edit", src, "E-99", out, toLine21, ErrUnknownEdit, ""},
		{"an output that is not one JSON document", src, EditRegisteredTransform, []byte(`{"a":1}{`), func([]byte) ([]byte, error) { return []byte(`{"a":1}{`), nil }, ErrInvalidJSON, ""},
	} {
		t.Run(r.name, func(t *testing.T) {
			calls = 0
			p, err := Transformed(r.src, r.id, r.out, r.rederive)
			if !errors.Is(err, r.want) || !strings.Contains(err.Error(), r.says) {
				t.Fatalf("got %v, want %v saying %q", err, r.want, r.says)
			}
			if p.Ownership() != 0 {
				t.Fatalf("a refused transform returned a payload: %v", p)
			}
		})
	}
	// ApplyChanges never makes a transform.
	body := NewBody([]byte(`{"a":1}`), OriginPeerFrame)
	d, err := Doc(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(body, fhirJSON, EditRegisteredTransform, d.RemoveMember(d.Root(), "a")); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("Apply with E-09: %v, want ErrInvalidEdit", err)
	}
}

// The ownership check keeps a transform's two halves together: a row lists
// the transform only with the builders whose messages it carries, a
// transformed payload must name a builder the row lists, and no other edited
// payload names one.
func TestTransformedOwnership(t *testing.T) {
	key := Key{"pas-claim", RoleRequester, DirectionRequest, OutcomeOriginated}
	table := map[Key]Rule{key: {Allowed: []Ownership{OwnershipAuthored, OwnershipEdited}, Edits: []EditID{EditRegisteredTransform}, Builders: []BuilderID{BuilderSDKPASSubmit}}}
	src := builtAt20(t)
	out, _ := toLine21([]byte(`{"resourceType":"Bundle","line":"2.0"}`))
	p, err := Transformed(src, EditRegisteredTransform, out, toLine21)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkIn(table, key)(p); err != nil {
		t.Fatalf("a transformed submission on its row: %v", err)
	}
	if err := checkIn(table, key)(src); err != nil {
		t.Fatalf("the submission as built on its row: %v", err)
	}
	upd, err := Authored(BuilderSDKPASUpdate, []byte(`{"resourceType":"Bundle","line":"2.0"}`), fhirJSON)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Transformed(upd, EditRegisteredTransform, out, toLine21)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkIn(table, key)(other); !errors.Is(err, ErrOwnershipRefused) || !strings.Contains(err.Error(), "builder not permitted") {
		t.Fatalf("another builder's transformed message: %v", err)
	}
	// Neither half alone: a transform with its builder dropped, and a builder
	// on an edit that is not a transform.
	noBuilder := Payload{c: &sealed{b: seal(out), own: OwnershipEdited, edits: []EditID{EditRegisteredTransform}, contentType: fhirJSON}}
	if err := checkIn(table, key)(noBuilder); !errors.Is(err, ErrOwnershipRefused) || !strings.Contains(err.Error(), "names no builder") {
		t.Fatalf("a transform without its builder: %v", err)
	}
	strip := Key{"crd-order-select", RoleRequester, DirectionRequest, OutcomeCarried}
	stripTable := map[Key]Rule{strip: {Allowed: []Ownership{OwnershipEdited}, Edits: []EditID{EditCDSCallbackStrip}}}
	withBuilder := Payload{c: &sealed{b: seal(out), own: OwnershipEdited, edits: []EditID{EditCDSCallbackStrip}, builder: BuilderSDKPASSubmit, contentType: fhirJSON}}
	if err := checkIn(stripTable, strip)(withBuilder); !errors.Is(err, ErrOwnershipRefused) || !strings.Contains(err.Error(), "without a transform") {
		t.Fatalf("a builder on a relay edit: %v", err)
	}
	// A row may not list the transform without the builders it carries.
	if err := checkRule(Rule{Allowed: []Ownership{OwnershipEdited}, Edits: []EditID{EditRegisteredTransform}}); err == nil {
		t.Fatal("a row listing E-09 without builders was accepted")
	}
}
