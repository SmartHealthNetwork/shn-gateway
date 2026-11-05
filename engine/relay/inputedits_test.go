package relay

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// A build that carries a participant's record a registered input edit
// changed (E-06) is still the builder's own message: InputEdited notes the
// edit and changes nothing else. Each refusal row is the valid note with one
// thing wrong.

func TestInputEditedNotesTheEditOnTheBuildersMessage(t *testing.T) {
	src := builtAt20(t)
	p, err := InputEdited(src, EditEvidenceSubjectRekey)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ownership() != OwnershipAuthored || p.Builder() != BuilderSDKPASSubmit || p.Edits() != nil || p.ContentType() != fhirJSON {
		t.Fatalf("noted = %v: want authored by sdk-pas-submit with no edit", p)
	}
	if !slices.Equal(p.InputEdits(), []EditID{EditEvidenceSubjectRekey}) {
		t.Fatalf("input edits %v, want [E-06]", p.InputEdits())
	}
	got, err := Transmit(p, func(Payload) error { return nil })
	if err != nil || !bytes.Equal(got, []byte(`{"resourceType":"Bundle","line":"2.0"}`)) {
		t.Fatalf("sent %s, %v; want the build's bytes unchanged", got, err)
	}
	// The source is not changed by the note.
	if src.InputEdits() != nil {
		t.Fatalf("the source now names %v", src.InputEdits())
	}
	// Noting the same edit again names it once; no ids is the source.
	again, err := InputEdited(p, EditEvidenceSubjectRekey)
	if err != nil || !slices.Equal(again.InputEdits(), []EditID{EditEvidenceSubjectRekey}) {
		t.Fatalf("noted twice = %v, %v; want [E-06] once", again.InputEdits(), err)
	}
	if same, err := InputEdited(src); err != nil || same.InputEdits() != nil || same.Ownership() != OwnershipAuthored {
		t.Fatalf("no ids = %v, %v; want the source", same, err)
	}
}

func TestInputEditedRefuses(t *testing.T) {
	relayed := Exact(NewBody([]byte(`{"resourceType":"Bundle"}`), OriginIngressRequest), fhirJSON)
	for _, tc := range []struct {
		name string
		src  Payload
		ids  []EditID
		want error
	}{
		{"an edit made to the sent bytes, not a record a build carries", builtAt20(t), []EditID{EditCDSCallbackStrip}, ErrInvalidEdit},
		{"the registered transform", builtAt20(t), []EditID{EditRegisteredTransform}, ErrInvalidEdit},
		{"an unregistered id", builtAt20(t), []EditID{"E-99"}, ErrUnknownEdit},
		{"one good id beside an unregistered one", builtAt20(t), []EditID{EditEvidenceSubjectRekey, "E-99"}, ErrUnknownEdit},
		{"a participant's message, relayed", relayed, []EditID{EditEvidenceSubjectRekey}, ErrInvalidEdit},
		{"the zero payload", Payload{}, []EditID{EditEvidenceSubjectRekey}, ErrInvalidEdit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := InputEdited(tc.src, tc.ids...); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// A participant's message with registered edits is not the gateway's
	// own either.
	body := NewBody([]byte(`{"a":"x"}`), OriginIngressRequest)
	doc, err := Doc(body)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := doc.Member(doc.Root(), "a")
	edited, err := Apply(body, "application/json", EditCDSCallbackStrip, doc.Replace(a, []byte(`"y"`)))
	if err != nil || edited.Ownership() != OwnershipEdited {
		t.Fatalf("setup: %v, %v", edited, err)
	}
	if _, err := InputEdited(edited, EditEvidenceSubjectRekey); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("an edited participant message: err = %v, want ErrInvalidEdit", err)
	}
}

// The registered transform keeps the input edits a build carries: the
// builder's message, carrying a record E-06 changed, then E-09.
func TestTransformedKeepsTheInputEdits(t *testing.T) {
	src, err := InputEdited(builtAt20(t), EditEvidenceSubjectRekey)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := toLine21([]byte(`{"resourceType":"Bundle","line":"2.0"}`))
	p, err := Transformed(src, EditRegisteredTransform, out, toLine21)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ownership() != OwnershipEdited || p.Builder() != BuilderSDKPASSubmit ||
		!slices.Equal(p.Edits(), []EditID{EditRegisteredTransform}) || !slices.Equal(p.InputEdits(), []EditID{EditEvidenceSubjectRekey}) {
		t.Fatalf("transformed = %v edits %v inputs %v: want edited [E-09] by sdk-pas-submit carrying [E-06]", p, p.Edits(), p.InputEdits())
	}
	// Input edits are noted before the build is carried: the transform's
	// result is no longer the gateway's message as built.
	if _, err := InputEdited(p, EditEvidenceSubjectRekey); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("noting after the transform: err = %v, want ErrInvalidEdit", err)
	}
}

// The input edits are registered edits, none of them a transform.
func TestInputEditIDsAreRegisteredAndNotTransforms(t *testing.T) {
	ids := InputEditIDs()
	if !slices.Equal(ids, []EditID{EditEvidenceSubjectRekey}) {
		t.Fatalf("input edits %v, want [E-06]", ids)
	}
	for _, id := range ids {
		if !slices.Contains(EditIDs(), id) || slices.Contains(TransformEditIDs(), id) {
			t.Errorf("%s: registered %t, transform %t; want registered, not a transform", id, slices.Contains(EditIDs(), id), slices.Contains(TransformEditIDs(), id))
		}
	}
}

// REJECTION: a workstream rule lists only input edits (InputEditIDs), and only
// beside the builders whose messages carry them.
func TestCheckRuleInputEdits(t *testing.T) {
	authoredBy := func(inputs ...EditID) Rule {
		return Rule{Allowed: []Ownership{OwnershipAuthored}, Builders: []BuilderID{BuilderSDKPASSubmit}, InputEdits: inputs}
	}
	if err := checkRule(authoredBy(EditEvidenceSubjectRekey)); err != nil {
		t.Fatalf("an input edit beside its builder: %v", err)
	}
	for name, r := range map[string]Rule{
		"an edit to the payload itself":  authoredBy(EditCDSCallbackStrip),
		"the registered transform":       authoredBy(EditRegisteredTransform),
		"an unregistered id":             authoredBy("E-99"),
		"an input edit with no builders": {Allowed: []Ownership{OwnershipRelayed}, InputEdits: []EditID{EditEvidenceSubjectRekey}},
	} {
		if err := checkRule(r); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
}
