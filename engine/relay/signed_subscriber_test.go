package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// subLocationRow is E-23's registry row, as the PA workstream declares it.
func subLocationRow() Edit {
	return Edit{ID: EditSubscriptionLocationRekey, Name: "subscription-location-rekey", Legs: []string{"cdex-task-subscribe"},
		Role: RoleRequester, Direction: DirectionResponse, Kind: EditKindSetHeader, Header: "Location",
		HeaderInputs: []string{SubscriptionLocationInputBase, SubscriptionLocationInputTail}}
}

var subLocationInputs = HeaderInputs{SubscriptionLocationInputBase: "https://payer-gw.example/cdex/provider/p1", SubscriptionLocationInputTail: "Subscription/s1"}

// E-23's check: the value is exactly the base, "/", then a Subscription
// location; every other value or input is refused by the core, whatever the
// caller wrote.
func TestSubscriptionLocationCheck(t *testing.T) {
	src := Exact(NewBody([]byte(`{"resourceType":"Subscription","id":"s1"}`), OriginPeerFrame), fhirJSON)
	const ok = "https://payer-gw.example/cdex/provider/p1/Subscription/s1"
	p, err := HeaderEdited([]Edit{subLocationRow()}, src, EditSubscriptionLocationRekey, "Location", ok, subLocationInputs)
	if err != nil {
		t.Fatal(err)
	}
	if name, value, set := p.EditedHeader(); !set || name != "Location" || value != ok || !slices.Equal(p.Edits(), []EditID{EditSubscriptionLocationRekey}) || p.SignedEdited() != nil {
		t.Fatalf("made %v %q %q %v", set, name, value, p.Edits())
	}
	versioned := HeaderInputs{SubscriptionLocationInputBase: subLocationInputs[SubscriptionLocationInputBase], SubscriptionLocationInputTail: "Subscription/s1/_history/2"}
	if _, err := HeaderEdited([]Edit{subLocationRow()}, src, EditSubscriptionLocationRekey, "Location", ok+"/_history/2", versioned); err != nil {
		t.Fatalf("a versioned location: %v", err)
	}
	tail := func(v string) HeaderInputs {
		return HeaderInputs{SubscriptionLocationInputBase: subLocationInputs[SubscriptionLocationInputBase], SubscriptionLocationInputTail: v}
	}
	base := func(v string) HeaderInputs {
		return HeaderInputs{SubscriptionLocationInputBase: v, SubscriptionLocationInputTail: "Subscription/s1"}
	}
	for name, c := range map[string]struct {
		value string
		in    HeaderInputs
	}{
		"another value":           {ok + "x", subLocationInputs},
		"a Task location":         {"https://payer-gw.example/cdex/provider/p1/Task/s1", tail("Task/s1")},
		"a dot segment id":        {"https://payer-gw.example/cdex/provider/p1/Subscription/..", tail("Subscription/..")},
		"a dot segment version":   {ok + "/_history/.", tail("Subscription/s1/_history/.")},
		"an id that is not one":   {ok + "%2F", tail("Subscription/s1%2F")},
		"an id too long":          {"https://payer-gw.example/cdex/provider/p1/Subscription/" + strings.Repeat("a", 65), tail("Subscription/" + strings.Repeat("a", 65))},
		"another segment":         {ok + "/x", tail("Subscription/s1/x")},
		"a type-level location":   {"https://payer-gw.example/cdex/provider/p1/Subscription", tail("Subscription")},
		"a query in the tail":     {ok + "?a=1", tail("Subscription/s1?a=1")},
		"a base with a query":     {"https://payer-gw.example/x?y/Subscription/s1", base("https://payer-gw.example/x?y")},
		"a base ending in /":      {"https://payer-gw.example/x//Subscription/s1", base("https://payer-gw.example/x/")},
		"a base that is relative": {"/x/Subscription/s1", base("/x")},
	} {
		if _, err := HeaderEdited([]Edit{subLocationRow()}, src, EditSubscriptionLocationRekey, "Location", c.value, c.in); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check to refuse it", name, err)
		}
	}
}

// signedSubscriber is a Subscription whose extension carries a Signature
// element, which covers the whole Subscription, as its system wrote it.
func signedSubscriber() Body {
	return NewBody([]byte(`{"resourceType":"Subscription","id":"s1","status":"requested",`+
		`"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`+
		`"criteria":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task",`+
		`"channel":{"type":"rest-hook","endpoint":"https://payer.example/notify","payload":"application/fhir+json","header":["Authorization: Bearer s3cret"]}}`), OriginPeerFrame)
}

// E-18 seals a signed Subscription's channel all the same (D132, N2): the
// removal of channel.endpoint and channel.header, each disclosed by the
// signature's carrier. Any other change E-18 makes to signed content is
// refused, as is the same removal by any other edit.
func TestSignedContentSubscriptionSealExemption(t *testing.T) {
	b := signedSubscriber()
	channel := func(d *Document) NodeID { return at(t, d, "channel") }
	type row struct {
		name string
		body Body
		edit EditID
		ops  func(d *Document) []Op
		made bool
	}
	rows := []row{
		{"endpoint and header removed", b, EditSubscriptionHeaderSeal, func(d *Document) []Op {
			return []Op{d.RemoveMember(channel(d), "endpoint"), d.RemoveMember(channel(d), "header")}
		}, true},
		{"the endpoint alone", b, EditSubscriptionHeaderSeal, func(d *Document) []Op { return []Op{d.RemoveMember(channel(d), "endpoint")} }, true},
		// Rejection rows.
		{"another channel member removed", b, EditSubscriptionHeaderSeal, func(d *Document) []Op { return []Op{d.RemoveMember(channel(d), "payload")} }, false},
		{"the endpoint replaced", b, EditSubscriptionHeaderSeal, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(`"x"`))}
		}, false},
		{"a root member removed", b, EditSubscriptionHeaderSeal, func(d *Document) []Op { return []Op{d.RemoveMember(d.Root(), "criteria")} }, false},
		{"a member inserted into channel", b, EditSubscriptionHeaderSeal, func(d *Document) []Op {
			return []Op{d.InsertMember(channel(d), "x", []byte(`"y"`))}
		}, false},
	}
	for _, id := range EditIDs() {
		if _, exempt := signedExempt[id]; exempt || slices.Contains(TransformEditIDs(), id) || slices.Contains(headerEdits, id) || slices.Contains(queryEdits, id) {
			continue
		}
		rows = append(rows, row{"other edit " + string(id) + " removing the header", b, id, func(d *Document) []Op {
			return []Op{d.RemoveMember(channel(d), "header")}
		}, false})
	}
	// E-19 and E-20 may not remove a root channel's endpoint either.
	for _, id := range []EditID{EditSubscriptionEndpointRekey, EditSubscriptionEndpointRestore} {
		rows = append(rows, row{"exempt " + string(id) + " removing the endpoint", b, id, func(d *Document) []Op {
			return []Op{d.RemoveMember(channel(d), "endpoint")}
		}, false})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, r.ops(mustDoc(t, r.body))...)
			if r.made {
				if err != nil || !slices.Equal(p.Edits(), []EditID{r.edit}) || !slices.Equal(p.SignedEdited(), []string{"Signature in Subscription"}) {
					t.Fatalf("%v %v %v", p.Edits(), p.SignedEdited(), err)
				}
				if out := string(mustTransmit(t, p)); strings.Contains(out, "payer.example/notify") {
					t.Fatalf("the endpoint is still there: %s", out)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != r.edit {
				t.Fatalf("want %s refused on signed content, got %v", r.edit, err)
			}
		})
	}
	// The place is the root Subscription's own channel.
	inBundle := NewBody([]byte(`{"resourceType":"Bundle","type":"collection","signature":{`+signatureElement+`},"entry":[{"resource":`+
		`{"resourceType":"Subscription","channel":{"type":"rest-hook","endpoint":"https://payer.example/notify"}}}]}`), OriginPeerFrame)
	d := mustDoc(t, inBundle)
	if _, err := Apply(inBundle, fhirJSON, EditSubscriptionHeaderSeal, d.RemoveMember(at(t, d, "entry", "0", "resource", "channel"), "endpoint")); err == nil {
		t.Fatal("E-18 removed a signed endpoint in a Bundle's entry")
	}
}

// signedAnswer is a notifier's signed searchset: the Bundle's signature
// covers it all, an entry's Subscription and the Bundle's fullUrl and link.
func signedAnswer() Body {
	return NewBody([]byte(`{"resourceType":"Bundle","type":"searchset","signature":{`+signatureElement+`},`+
		`"link":[{"relation":"next","url":"http://payer.example/fhir?_getpages=a"}],`+
		`"entry":[{"fullUrl":"http://payer.example/fhir/Subscription/s1","resource":{"resourceType":"Subscription","id":"s1",`+
		`"channel":{"type":"rest-hook","endpoint":"https://gw.example/notify/cdex-task/payer/kid/sealed","header":["Authorization: Bearer x","X-SHN-Channel: y"]}},"search":{"mode":"match"}},`+
		`{"fullUrl":"http://payer.example/fhir/Basic/b1","resource":{"resourceType":"Basic","channel":{"endpoint":"https://x.example"}}}]}`), OriginPeerFrame)
}

// E-20 restores a signed answer all the same (D132, N2): an entry's
// Subscription's channel.endpoint and channel.header, its header's removal,
// the entry's fullUrl and the link's url, each disclosed by the signature's
// carrier; a single signed Subscription too. Any other change E-20 makes to
// signed content is refused.
func TestSignedContentSubscriptionRestoreExemption(t *testing.T) {
	bundle := signedAnswer()
	single := signedSubscriber()
	type row struct {
		name string
		body Body
		ops  func(d *Document) []Op
		made bool
	}
	rows := []row{
		{"an entry's endpoint and header restored, its fullUrl and the link re-keyed", bundle, func(d *Document) []Op {
			return []Op{
				d.Replace(at(t, d, "entry", "0", "resource", "channel", "endpoint"), []byte(`"https://payer-sys.example/notify"`)),
				d.Replace(at(t, d, "entry", "0", "resource", "channel", "header"), []byte(`["Authorization: Bearer s3cret"]`)),
				d.Replace(at(t, d, "entry", "0", "fullUrl"), []byte(`"https://payer-gw.example/cdex/provider/p/Subscription/s1"`)),
				d.Replace(at(t, d, "link", "0", "url"), []byte(`"https://payer-gw.example/cdex/provider/p/Subscription?_getpages=a"`)),
			}
		}, true},
		{"an entry's header removed", bundle, func(d *Document) []Op {
			return []Op{d.RemoveMember(at(t, d, "entry", "0", "resource", "channel"), "header")}
		}, true},
		{"a single Subscription restored", single, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(`"https://payer-sys.example/notify"`)),
				d.RemoveMember(at(t, d, "channel"), "header")}
		}, true},
		{"another resource's fullUrl", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "1", "fullUrl"), []byte(`"https://payer-gw.example/cdex/provider/p/Basic/b1"`))}
		}, true},
		// Rejection rows.
		{"a Basic's channel endpoint", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "1", "resource", "channel", "endpoint"), []byte(`"x"`))}
		}, false},
		{"the endpoint as an object", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "resource", "channel", "endpoint"), []byte(`{"a":1}`))}
		}, false},
		{"the header as a string", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "resource", "channel", "header"), []byte(`"a: b"`))}
		}, false},
		{"one header value replaced", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "resource", "channel", "header", "0"), []byte(`"a: b"`))}
		}, false},
		{"the endpoint removed", bundle, func(d *Document) []Op {
			return []Op{d.RemoveMember(at(t, d, "entry", "0", "resource", "channel"), "endpoint")}
		}, false},
		{"a member inserted", bundle, func(d *Document) []Op {
			return []Op{d.InsertMember(at(t, d, "entry", "0", "resource", "channel"), "endpoint2", []byte(`"x"`))}
		}, false},
		{"the fullUrl as a number", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "fullUrl"), []byte(`1`))}
		}, false},
		{"the link's relation", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "link", "0", "relation"), []byte(`"prev"`))}
		}, false},
		{"the entry's search mode", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "search", "mode"), []byte(`"include"`))}
		}, false},
		{"the Subscription's id", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "resource", "id"), []byte(`"s2"`))}
		}, false},
		{"the Bundle's type", bundle, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "type"), []byte(`"collection"`))}
		}, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, EditSubscriptionEndpointRestore, r.ops(mustDoc(t, r.body))...)
			if r.made {
				if err != nil || !slices.Equal(p.Edits(), []EditID{EditSubscriptionEndpointRestore}) || len(p.SignedEdited()) != 1 {
					t.Fatalf("%v %v %v", p.Edits(), p.SignedEdited(), err)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != EditSubscriptionEndpointRestore {
				t.Fatalf("want E-20 refused on signed content, got %v", err)
			}
		})
	}
	// A Subscription nested in another resource, not an entry's own, is
	// not a place E-20 restores.
	nested := NewBody([]byte(`{"resourceType":"Bundle","type":"searchset","signature":{`+signatureElement+`},"entry":[{"resource":{"resourceType":"Basic",`+
		`"x":{"resourceType":"Subscription","channel":{"endpoint":"https://gw.example/notify/a/b/c/d"}}}}]}`), OriginPeerFrame)
	d := mustDoc(t, nested)
	var se *SignedContentError
	if _, err := Apply(nested, fhirJSON, EditSubscriptionEndpointRestore, d.Replace(at(t, d, "entry", "0", "resource", "x", "channel", "endpoint"), []byte(`"y"`))); !errors.As(err, &se) {
		t.Fatalf("a nested Subscription: %v, want refused", err)
	}
	// Nor in the lists of a root that is not a Bundle.
	notBundle := NewBody([]byte(`{"resourceType":"Parameters","signature":{`+signatureElement+`},"link":[{"url":"http://a.example/x"}],"entry":[{"fullUrl":"http://a.example/y"}]}`), OriginPeerFrame)
	d = mustDoc(t, notBundle)
	for _, path := range [][]string{{"link", "0", "url"}, {"entry", "0", "fullUrl"}} {
		if _, err := Apply(notBundle, fhirJSON, EditSubscriptionEndpointRestore, d.Replace(at(t, d, path...), []byte(`"y"`))); !errors.As(err, &se) {
			t.Fatalf("a %v in a root that is not a Bundle: %v, want refused", path, err)
		}
	}
	// Nor in a Bundle that is not the root.
	notRoot := NewBody([]byte(`{"resourceType":"Parameters","signature":{`+signatureElement+`},"parameter":[{"resource":{"resourceType":"Bundle","entry":[{"fullUrl":"http://a.example/x"}]}}]}`), OriginPeerFrame)
	d = mustDoc(t, notRoot)
	if _, err := Apply(notRoot, fhirJSON, EditSubscriptionEndpointRestore, d.Replace(at(t, d, "parameter", "0", "resource", "entry", "0", "fullUrl"), []byte(`"y"`))); err == nil {
		t.Fatal("a Bundle below the root: re-keyed on signed content")
	}
}

// A header edit is made on a body-edited payload (E-20, then E-23): the
// result names both, keeps the body edit's signatures and the header's value,
// and the signature is the body edit's alone (SignedEdited is the body's).
func TestHeaderEditedOnABodyEditedPayload(t *testing.T) {
	b := signedSubscriber()
	d := mustDoc(t, b)
	restored, err := Apply(b, fhirJSON, EditSubscriptionEndpointRestore, d.Replace(at(t, d, "channel", "endpoint"), []byte(`"https://payer-sys.example/x"`)))
	if err != nil {
		t.Fatal(err)
	}
	const loc = "https://payer-gw.example/cdex/provider/p1/Subscription/s1"
	p, err := HeaderEdited([]Edit{subLocationRow()}, restored, EditSubscriptionLocationRekey, "Location", loc, subLocationInputs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Edits(), []EditID{EditSubscriptionEndpointRestore, EditSubscriptionLocationRekey}) || !slices.Equal(p.SignedEdited(), []string{"Signature in Subscription"}) {
		t.Fatalf("edits %v, signed %v", p.Edits(), p.SignedEdited())
	}
	if name, value, ok := p.EditedHeader(); !ok || name != "Location" || value != loc {
		t.Fatalf("header %v %q %q", ok, name, value)
	}
	if string(mustTransmit(t, p)) != string(mustTransmit(t, restored)) {
		t.Fatal("the header edit changed the body")
	}
	// A header edit is still made once: on top of one, it is refused.
	if _, err := HeaderEdited([]Edit{subLocationRow()}, p, EditSubscriptionLocationRekey, "Location", loc, subLocationInputs); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("a second header edit: %v", err)
	}
	// And then takes no body edit after it.
	if _, err := Then(p, OriginPeerFrame, func(*Document) ([]Change, error) { return nil, nil }); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("Then after the header edit: %v", err)
	}
}
