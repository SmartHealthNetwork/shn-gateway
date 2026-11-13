package relay

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// toyInputs are the inputs headerRow's value, "https://payer.example/Task/1",
// is made from: E-16's base and Task location, which the core's check of it
// reads.
var toyInputs = HeaderInputs{TaskLocationInputBase: "https://payer.example", TaskLocationInputTail: "Task/1"}

// headerRow is a registrable header edit, as data: E-16's id, one listed
// header of an answer, the inputs the core's check reads, no path, dropping
// nothing.
func headerRow() Edit {
	return Edit{ID: EditCDexTaskLocationRekey, Name: "header", Role: RoleRequester, Direction: DirectionResponse,
		Kind: EditKindSetHeader, Header: "Location", HeaderInputs: []string{TaskLocationInputBase, TaskLocationInputTail}}
}

// A header edit is registrable as one listed header of an answer, naming the
// inputs the core's check of its id reads, at no JSON path, dropping no
// header: its ETag is kept, since the body is not changed. Each departure is
// refused, and the ETag exemption is the header kind's alone: a body edit of
// an answer that keeps its ETag is still refused, and a row of another kind
// may name neither a header nor header inputs.
func TestValidateEdits_AHeaderEdit(t *testing.T) {
	// The headers a header edit may set: a closed list, a reviewed change.
	if got := EditableHeaders(); !slices.Equal(got, []string{"Location"}) {
		t.Fatalf("EditableHeaders() = %q, want [Location]", got)
	}
	if err := ValidateEdits([]Edit{headerRow()}); err != nil {
		t.Fatalf("a header edit: %v", err)
	}
	if got := EditDropsAnswerHeaders([]Edit{headerRow()}, EditCDexTaskLocationRekey); len(got) != 0 {
		t.Fatalf("a header edit drops %v, want its ETag kept", got)
	}
	for name, c := range map[string]struct {
		mutate func(*Edit)
		want   string
	}{
		"made on a request":            {func(e *Edit) { e.Direction = DirectionRequest }, "is not made on an answer"},
		"no header":                    {func(e *Edit) { e.Header = "" }, "not a header a header edit may set"},
		"not a field name":             {func(e *Edit) { e.Header = "Loc ation" }, "not a header a header edit may set"},
		"the listed header misspelled": {func(e *Edit) { e.Header = "location" }, "not a header a header edit may set"},
		"the ETag":                     {func(e *Edit) { e.Header = "ETag" }, "not a header a header edit may set"},
		"the Content-Type":             {func(e *Edit) { e.Header = "Content-Type" }, "not a header a header edit may set"},
		"the Content-Length":           {func(e *Edit) { e.Header = "Content-Length" }, "not a header a header edit may set"},
		"the edit disclosure":          {func(e *Edit) { e.Header = "X-SHN-Edited" }, "not a header a header edit may set"},
		"an input missing":             {func(e *Edit) { e.HeaderInputs = []string{TaskLocationInputBase} }, "the ones the core's check of it reads"},
		"its inputs in another order":  {func(e *Edit) { e.HeaderInputs = []string{TaskLocationInputTail, TaskLocationInputBase} }, "the ones the core's check of it reads"},
		"an input named twice":         {func(e *Edit) { e.HeaderInputs = []string{TaskLocationInputBase, TaskLocationInputBase} }, "the ones the core's check of it reads"},
		"an extra input":               {func(e *Edit) { e.HeaderInputs = append(e.HeaderInputs, "provider") }, "the ones the core's check of it reads"},
		"a JSON path":                  {func(e *Edit) { e.Paths = []string{"$.id"} }, "names a JSON path"},
		"drops the ETag":               {func(e *Edit) { e.DropsAnswerHeaders = []string{ETagHeader} }, "drops [ETag]"},
		"a body edit of an answer that keeps its ETag": {func(e *Edit) {
			e.ID, e.Kind, e.Header, e.HeaderInputs = EditPASPendReturnRekey, EditKindReplaceValue, "", nil
		}, "does not drop its ETag"},
		"a body edit naming a header": {func(e *Edit) {
			e.ID, e.Kind, e.HeaderInputs, e.DropsAnswerHeaders = EditPASPendReturnRekey, EditKindReplaceValue, nil, []string{ETagHeader}
		}, "names a header"},
		"a body edit naming header inputs": {func(e *Edit) {
			e.ID, e.Kind, e.Header, e.DropsAnswerHeaders = EditPASPendReturnRekey, EditKindReplaceValue, "", []string{ETagHeader}
		}, "names a header"},
		"a header edit's id as a body edit": {func(e *Edit) {
			e.Kind, e.Header, e.HeaderInputs, e.DropsAnswerHeaders = EditKindReplaceValue, "", nil, []string{ETagHeader}
		}, "only one, is of kind set-header"},
		"a body edit's id as a header edit": {func(e *Edit) { e.ID = EditPASPendReturnRekey }, "only one, is of kind set-header"},
	} {
		row := headerRow()
		c.mutate(&row)
		if err := ValidateEdits([]Edit{row}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, c.want)
		}
	}
	// The body edit made valid again: it drops its ETag.
	body := headerRow()
	body.ID, body.Kind, body.Header, body.HeaderInputs, body.DropsAnswerHeaders = EditPASPendReturnRekey, EditKindReplaceValue, "", nil, []string{ETagHeader}
	if err := ValidateEdits([]Edit{body}); err != nil {
		t.Fatalf("a body edit that drops its ETag: %v", err)
	}
}

// HeaderEdited makes the header edit on a peer's message relayed exactly: the
// result is the edit's, with the same sealed bytes and media type, and the
// header its row names, spelled as the row spells it, set to the value the
// core's check of the edit, by its id, passed.
func TestHeaderEdited_Accepts(t *testing.T) {
	body := []byte(`{"resourceType":"Task","id":"t-1"}`)
	src := Exact(NewBody(body, OriginPeerFrame), "application/fhir+json")
	p, err := HeaderEdited([]Edit{headerRow()}, src, EditCDexTaskLocationRekey, "location", "https://payer.example/Task/1", toyInputs)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{EditCDexTaskLocationRekey}) || p.Builder() != "" {
		t.Fatalf("payload %v, want the header edit alone", p)
	}
	if !bytes.Equal(p.c.b.get(), body) || p.ContentType() != "application/fhir+json" {
		t.Fatalf("payload bytes or media type changed: %s %q", p.c.b.get(), p.ContentType())
	}
	name, value, ok := p.EditedHeader()
	if !ok || name != "Location" || value != "https://payer.example/Task/1" {
		t.Fatalf("EditedHeader = %q %q %v", name, value, ok)
	}
	if _, _, ok := src.EditedHeader(); ok {
		t.Fatal("the source payload names a header")
	}
	if _, _, ok := (Payload{}).EditedHeader(); ok {
		t.Fatal("the zero payload names a header")
	}
	if s := fmtPayload(p); strings.Contains(s, "payer.example") || !strings.Contains(s, "+Location") {
		t.Fatalf("payload prints %q: the header's name, never its value", s)
	}
}

// fmtPayload is the payload as %v prints it.
func fmtPayload(p Payload) string { return fmt.Sprintf("%v", p) }

// A header edit changes no byte of a body: Apply refuses its id, whatever the
// operation.
func TestApplyRefusesAHeaderEdit(t *testing.T) {
	in := NewBody([]byte(`{"a":1}`), OriginPeerFrame)
	d, err := Doc(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.Member(0, "a")
	if p, err := Apply(in, "application/json", EditCDexTaskLocationRekey, d.Replace(a, []byte(`2`))); !errors.Is(err, ErrInvalidEdit) || p.Ownership() != 0 {
		t.Fatalf("Apply made a header edit's id on a body: %v %v", p, err)
	}
	// The same operation under a body edit's id is made: the refusal is the id's.
	if _, err := Apply(in, "application/json", EditPASPendReturnRekey, d.Replace(a, []byte(`2`))); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// HeaderEdited refuses: an id outside the closed set or the registry; an id of
// another kind; a header other than the row's; a source that is not a peer's
// message relayed exactly; a value that is empty or not a field value a
// response carries; a value the core's check of the edit refuses; and a row
// that names other inputs than that check reads, since neither the row nor the
// caller can supply a check.
func TestHeaderEdited_Refuses(t *testing.T) {
	src := Exact(NewBody([]byte(`{"resourceType":"Task"}`), OriginPeerFrame), "application/fhir+json")
	in := NewBody([]byte(`{"a":1}`), OriginPeerFrame)
	d, err := Doc(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.Member(0, "a")
	edited, err := Apply(in, "application/json", EditPASPendReturnRekey, d.Replace(a, []byte(`2`)))
	if err != nil || edited.Ownership() != OwnershipEdited {
		t.Fatal(edited, err)
	}
	authored, err := Authored(BuilderGatewayRefusal, []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	body := Edit{ID: EditPASPendReturnRekey, Direction: DirectionResponse, Kind: EditKindReplaceValue, DropsAnswerHeaders: []string{ETagHeader}}
	row := func(mutate func(*Edit)) []Edit { e := headerRow(); mutate(&e); return []Edit{e} }
	headerEditedSrc, err := HeaderEdited([]Edit{headerRow()}, src, EditCDexTaskLocationRekey, "Location", "https://payer.example/Task/1", toyInputs)
	if err != nil {
		t.Fatal(err)
	}
	const ok = "https://payer.example/Task/1"
	H := []Edit{headerRow()}
	for name, c := range map[string]struct {
		rows   []Edit
		src    Payload
		id     EditID
		header string
		value  string
		in     HeaderInputs
		want   error
	}{
		"an id outside the closed set":     {H, src, "E-99", "Location", ok, toyInputs, ErrUnknownEdit},
		"an id the registry lacks":         {[]Edit{body}, src, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrUnknownEdit},
		"an id of another kind":            {[]Edit{headerRow(), body}, src, EditPASPendReturnRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"a body edit's id on a header row": {row(func(e *Edit) { e.ID = EditPASPendReturnRekey }), src, EditPASPendReturnRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		// The row read is checked again: a registry that skipped
		// ValidateEdits cannot set an unlisted header or skip its check.
		"a row naming other inputs":          {row(func(e *Edit) { e.HeaderInputs = []string{TaskLocationInputBase} }), src, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"a row setting the Content-Type":     {row(func(e *Edit) { e.Header = "Content-Type" }), src, EditCDexTaskLocationRekey, "Content-Type", ok, toyInputs, ErrInvalidEdit},
		"a row setting X-SHN-Edited":         {row(func(e *Edit) { e.Header = "X-SHN-Edited" }), src, EditCDexTaskLocationRekey, "X-SHN-Edited", ok, toyInputs, ErrInvalidEdit},
		"a row setting the ETag":             {row(func(e *Edit) { e.Header = "ETag" }), src, EditCDexTaskLocationRekey, "ETag", ok, toyInputs, ErrInvalidEdit},
		"a row on a request":                 {row(func(e *Edit) { e.Direction = DirectionRequest }), src, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"a row dropping the ETag":            {row(func(e *Edit) { e.DropsAnswerHeaders = []string{ETagHeader} }), src, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"another header":                     {H, src, EditCDexTaskLocationRekey, "Content-Location", ok, toyInputs, ErrInvalidEdit},
		"no inputs":                          {H, src, EditCDexTaskLocationRekey, "Location", ok, nil, ErrInvalidEdit},
		"another input":                      {H, src, EditCDexTaskLocationRekey, "Location", ok, HeaderInputs{TaskLocationInputBase: "https://payer.example", "other": "Task/1"}, ErrInvalidEdit},
		"an extra input":                     {H, src, EditCDexTaskLocationRekey, "Location", ok, HeaderInputs{TaskLocationInputBase: "https://payer.example", TaskLocationInputTail: "Task/1", "x": ""}, ErrInvalidEdit},
		"an edited source":                   {H, edited, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"a header-edited source":             {H, headerEditedSrc, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"the gateway's own message":          {H, authored, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"no payload":                         {H, Payload{}, EditCDexTaskLocationRekey, "Location", ok, toyInputs, ErrInvalidEdit},
		"an empty value":                     {H, src, EditCDexTaskLocationRekey, "Location", "", toyInputs, ErrInvalidEdit},
		"a value with a line break":          {H, src, EditCDexTaskLocationRekey, "Location", ok + "\r\nX-Other: 1", toyInputs, ErrInvalidEdit},
		"a value with a NUL":                 {H, src, EditCDexTaskLocationRekey, "Location", ok + "\x00", toyInputs, ErrInvalidEdit},
		"a value with a leading space":       {H, src, EditCDexTaskLocationRekey, "Location", " " + ok, toyInputs, ErrInvalidEdit},
		"a value with a trailing tab":        {H, src, EditCDexTaskLocationRekey, "Location", ok + "\t", toyInputs, ErrInvalidEdit},
		"a value that is not UTF-8":          {H, src, EditCDexTaskLocationRekey, "Location", ok + "\xff", toyInputs, ErrInvalidEdit},
		"a DEL in the value":                 {H, src, EditCDexTaskLocationRekey, "Location", ok + "\x7f", toyInputs, ErrInvalidEdit},
		"a value the core's check refuses":   {H, src, EditCDexTaskLocationRekey, "Location", "https://payer.example/Task/2", toyInputs, ErrVerifyMismatch},
		"inputs the value was not made from": {H, src, EditCDexTaskLocationRekey, "Location", ok, HeaderInputs{TaskLocationInputBase: "https://other.example", TaskLocationInputTail: "Task/1"}, ErrVerifyMismatch},
	} {
		p, err := HeaderEdited(c.rows, c.src, c.id, c.header, c.value, c.in)
		if !errors.Is(err, c.want) || p.Ownership() != 0 {
			t.Errorf("%s: %v %v, want %v and no payload", name, p, err, c.want)
		}
	}
	// The same call with an accepted value is made: each refusal above is the
	// one mutation's.
	if _, err := HeaderEdited([]Edit{headerRow()}, src, EditCDexTaskLocationRekey, "Location", ok, toyInputs); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// A payload with a header edit takes no further edit, from any constructor
// that takes a payload: Then would rebuild it without its header, and
// Transformed, InputEdited and HeaderEdited take only another kind of payload.
func TestAHeaderEditedPayloadTakesNoFurtherEdit(t *testing.T) {
	body := NewBody([]byte(`{"a":1}`), OriginPeerFrame)
	p, err := HeaderEdited([]Edit{headerRow()}, Exact(body, "application/json"), EditCDexTaskLocationRekey, "Location", "https://payer.example/Task/1", toyInputs)
	if err != nil {
		t.Fatal(err)
	}
	change := func(d *Document) ([]Change, error) {
		a, _ := d.Member(0, "a")
		return []Change{{Edit: EditPASPendReturnRekey, Ops: []Op{d.Replace(a, []byte(`2`))}}}, nil
	}
	if got, err := Then(p, OriginPeerFrame, change); !errors.Is(err, ErrInvalidEdit) || got.Ownership() != 0 {
		t.Errorf("Then: %v %v, want the header-edited payload refused", got, err)
	}
	if got, err := Then(p, OriginPeerFrame, func(*Document) ([]Change, error) { return nil, nil }); !errors.Is(err, ErrInvalidEdit) || got.Ownership() != 0 {
		t.Errorf("Then with no change: %v %v, want refused", got, err)
	}
	if got, err := Transformed(p, EditRegisteredTransform, []byte(`{"a":2}`), func(b []byte) ([]byte, error) { return []byte(`{"a":2}`), nil }); !errors.Is(err, ErrInvalidEdit) || got.Ownership() != 0 {
		t.Errorf("Transformed: %v %v, want refused", got, err)
	}
	if got, err := InputEdited(p, EditEvidenceSubjectRekey); !errors.Is(err, ErrInvalidEdit) || got.Ownership() != 0 {
		t.Errorf("InputEdited: %v %v, want refused", got, err)
	}
	// The control: Then takes the same change on the payload as relayed.
	if got, err := Then(Exact(body, "application/json"), OriginPeerFrame, change); err != nil || got.Ownership() != OwnershipEdited {
		t.Fatalf("control: %v %v", got, err)
	}
}
