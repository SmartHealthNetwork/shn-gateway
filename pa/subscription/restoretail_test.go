package subscription

import (
	"bytes"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// E-20 reads the address by its tail alone: the
// subscriber's gateway does not know the notifier gateway's ingress, so
// whatever comes before "/notify/<topic>/<subscriber>/<kid>/<sealed>" is not
// read, and what the restore rests on is that the seals open under the binding
// naming the notifier that answered.
func TestRestore_ByTheAddressTail(t *testing.T) {
	sub, not := edgeKeys(t)
	stored, a, _ := throughBoth(t, sub, not, binding, baseSubscription)

	// A foreign prefix that still opens is restored.
	foreign := bytes.Replace(stored, []byte(notifierIngress), []byte("https://elsewhere.example/any/prefix"), 1)
	if bytes.Equal(foreign, stored) {
		t.Fatal("fixture: the stored Subscription does not name the notifier's ingress")
	}
	out, made, notMade, err := Restore(sub, binding, "", "", foreign)
	if err != nil || made != 1 || len(notMade) != 0 || !jsonEqual(t, out, []byte(baseSubscription)) {
		t.Fatalf("a foreign prefix: made %d, not made %v, %v\n%s", made, notMade, err, out)
	}

	// Another notifier's sealed value: sealed for payer-2, answered by
	// payer-1. Its address and its seals are well formed, and they do not
	// open: the bound data names another notifier.
	otherNotifier, _, _ := throughBoth(t, sub, not, Binding{Subscriber: binding.Subscriber, Notifier: "payer-2", Topic: binding.Topic}, baseSubscription)
	// The same Subscription restored under the binding naming the notifier
	// that sealed it opens: the refusal is the bound data's, nothing else.
	if _, made, _, _ := Restore(sub, Binding{Subscriber: binding.Subscriber, Notifier: "payer-2", Topic: binding.Topic}, "", "", otherNotifier); made != 1 {
		t.Fatal("fixture: the other notifier's Subscription does not restore under its own binding")
	}

	// A tampered seal: one character of the sealed endpoint changed.
	tamperedEndpoint := strings.Replace(string(stored), a.String(), a.String()[:len(a.String())-1]+otherB64(a.String()[len(a.String())-1]), 1)

	for name, row := range map[string]struct {
		body []byte
		want string
	}{
		"another notifier's sealed value": {otherNotifier, NotMadeSealInvalid},
		"a tampered sealed endpoint":      {[]byte(tamperedEndpoint), NotMadeSealInvalid},
		"a tail with a query":             {bytes.Replace(stored, []byte(a.String()), []byte(a.String()+"?x=1"), 1), NotMadeForeignAddress},
		"a tail with a fragment":          {bytes.Replace(stored, []byte(a.String()), []byte(a.String()+"#f"), 1), NotMadeForeignAddress},
		// A query that itself ends in a well-formed tail: the address's
		// own path does not end in one.
		"a tail inside a query":          {bytes.Replace(stored, []byte(a.String()), []byte("https://elsewhere.example/x?y="+strings.TrimPrefix(a.String(), notifierIngress)), 1), NotMadeForeignAddress},
		"a segment after the tail":       {bytes.Replace(stored, []byte(a.String()), []byte(a.String()+"/x"), 1), NotMadeForeignAddress},
		"another first segment":          {bytes.Replace(stored, []byte("/notify/"), []byte("/notice/"), 1), NotMadeForeignAddress},
		"no key id":                      {bytes.Replace(stored, []byte("/"+strings.SplitN(a.SealedEndpoint, ".", 2)[0]+"/"), []byte("/x/"), 1), NotMadeForeignAddress},
		"another topic in the tail":      {bytes.Replace(stored, []byte("/notify/test/"), []byte("/notify/test-task/"), 1), NotMadeAnotherBinding},
		"another subscriber in the tail": {bytes.Replace(stored, []byte("/provider-1/"), []byte("/provider-2/"), 1), NotMadeAnotherBinding},
	} {
		if bytes.Equal(row.body, stored) {
			t.Fatalf("%s: fixture unchanged", name)
		}
		out, made, notMade, err := Restore(sub, binding, "", "", row.body)
		if err != nil || made != 0 || !slices.Equal(notMade, []NotMade{{Edit: EditEndpointRestore, Reason: row.want}}) || !bytes.Equal(out, row.body) {
			t.Errorf("%s: made %d, not made %v, %v; carried unchanged %v", name, made, notMade, err, bytes.Equal(out, row.body))
		}
	}
}

// otherB64 is another base64url character than c.
func otherB64(c byte) string {
	if c == 'x' {
		return "y"
	}
	return "x"
}

// ParseAddressTail reads the five tail segments by structure, and nothing
// before them.
func TestParseAddressTail(t *testing.T) {
	sub, _ := edgeKeys(t)
	se := sub.SealEndpoint(binding, subEndpoint)
	a := Address{Ingress: notifierIngress, Topic: "pas", Subscriber: "provider 1", SealedEndpoint: se}
	got, ok := ParseAddressTail(a.String())
	if !ok || got != a {
		t.Fatalf("ParseAddressTail(%q) = %+v, %v; want %+v", a.String(), got, ok, a)
	}
	// The prefix is whatever comes before: another host, no path at all.
	for _, prefix := range []string{"https://other.example", "http://x", "https://h.example/a/notify/b"} {
		b := a
		b.Ingress = prefix
		if got, ok := ParseAddressTail(b.String()); !ok || got != b {
			t.Errorf("prefix %q: %+v, %v", prefix, got, ok)
		}
	}
	kid, sealed, _ := strings.Cut(se, ".")
	for _, raw := range []string{
		"",
		"notify/pas/provider-1/" + kid + "/" + sealed,
		notifierIngress + "/notify/pas/provider-1/" + kid,
		notifierIngress + "/notify//provider-1/" + kid + "/" + sealed,
		notifierIngress + "/notify/pas//" + kid + "/" + sealed,
		notifierIngress + "/notify/pas/provider-1/short/" + sealed,
		notifierIngress + "/notify/pas/provider-1/" + kid + "/",
		notifierIngress + "/notify/pas/provider-1/" + kid + "/" + sealed + "=",
		notifierIngress + "/notify/p%zz/provider-1/" + kid + "/" + sealed,
		notifierIngress + "/notify/pas/p%zz/" + kid + "/" + sealed,
		notifierIngress + "/notify/pas/provider-1/" + kid + "/" + sealed + "?",
		notifierIngress + "/notify/pas/provider-1/" + kid + "/" + sealed + "#",
		"https://h.example/%zz/notify/pas/provider-1/" + kid + "/" + sealed,
	} {
		if got, ok := ParseAddressTail(raw); ok {
			t.Errorf("ParseAddressTail(%q) = %+v, want none", raw, got)
		}
	}
}

// RestoreEdits names exactly the changes Restore makes: made in the body,
// they give Restore's bytes, with the same count and reasons.
func TestRestoreEdits_AreRestoresChanges(t *testing.T) {
	sub, not := edgeKeys(t)
	own, _, _ := throughBoth(t, sub, not, binding, baseSubscription)
	noHeader, _, _ := throughBoth(t, sub, not, binding, edit(t, baseSubscription, `,
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, ``))
	const base, counterpart = "http://payer.example/fhir", "https://gw.example/cdex/provider/p"
	bundle := `{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"http://payer.example/fhir?_getpages=a"}],"entry":[` +
		`{"fullUrl":"http://payer.example/fhir/Subscription/1","resource":` + string(own) + `},` +
		`{"fullUrl":"http://payer.example/fhir/Subscription/2","resource":` + string(noHeader) + `},` +
		`{"fullUrl":"http://payer.example/fhir/Subscription/3","resource":` + baseSubscription + `}]}`
	for name, body := range map[string][]byte{"a Subscription": own, "one with no header": noHeader, "a searchset": []byte(bundle)} {
		want, wantMade, wantNot, err := Restore(sub, binding, base, counterpart, body)
		if err != nil {
			t.Fatal(err)
		}
		edits, made, notMade, err := RestoreEdits(sub, binding, base, counterpart, body)
		if err != nil || made != wantMade || !slices.Equal(notMade, wantNot) {
			t.Fatalf("%s: made %d, not made %v, %v; want %d, %v", name, made, notMade, err, wantMade, wantNot)
		}
		root, err := scanJSON(body)
		if err != nil {
			t.Fatal(err)
		}
		var sp []splice
		for _, e := range edits {
			if !e.Remove {
				sp = append(sp, splice{start: e.Start, end: e.End, repl: e.Value})
				continue
			}
			obj, i := memberOf(root, e.Start, e.End)
			if obj == nil {
				t.Fatalf("%s: no member holds the removed value [%d,%d)", name, e.Start, e.End)
			}
			sp = append(sp, removeMember(obj, i))
		}
		got, err := applySplices(body, sp)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: the edits give\n%s\nwant Restore's\n%s", name, got, want)
		}
	}
	if _, _, _, err := RestoreEdits(sub, binding, base, "", own); err == nil {
		t.Fatal("a stated base with no counterpart base: no error")
	}
	if _, _, _, err := RestoreEdits(sub, binding, "", "", []byte("<x/>")); err == nil {
		t.Fatal("not JSON: no error")
	}
}

// memberOf is the object, and the member's index in it, whose member's value
// spans [start, end).
func memberOf(n *jnode, start, end int) (*jnode, int) {
	if n == nil {
		return nil, -1
	}
	for i, m := range n.members {
		if m.val.start == start && m.val.end == end {
			return n, i
		}
		if obj, j := memberOf(m.val, start, end); obj != nil {
			return obj, j
		}
	}
	for _, e := range n.elems {
		if obj, j := memberOf(e, start, end); obj != nil {
			return obj, j
		}
	}
	return nil, -1
}

// E-20 restores a search answer's link url only to the endpoint it restored,
// in this same answer, in the channel.endpoint of the Subscription that held
// the link's notification address; a link whose url names an address no
// restored Subscription held (another subscriber's, another notifier's, or
// one in an answer with no Subscription of the subscriber's), or addresses
// restored to two endpoints, keeps its url as written. Every other parameter
// keeps its bytes.
func TestRestore_LinkURLOnlyToTheRestoredEndpoint(t *testing.T) {
	sub, not := edgeKeys(t)
	stored, a, _ := throughBoth(t, sub, not, binding, baseSubscription)
	const stated, counterpart = "https://notifier.example/fhir", "https://subscriber-gw.example/cdex/provider/payer-1"
	answer := func(entries [][]byte, qv string) []byte {
		var es []string
		for _, e := range entries {
			es = append(es, `{"fullUrl":"`+stated+`/Subscription/s1","resource":`+string(e)+`}`)
		}
		return []byte(`{"resourceType":"Bundle","type":"searchset","link":[{"relation":"self","url":"` + stated + `/Subscription?url=` + qv + `&status=active"}],"entry":[` + strings.Join(es, ",") + `]}`)
	}
	selfOf := func(t *testing.T, out []byte) string {
		t.Helper()
		root, err := scanJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := root.memberFolded("link").elems[0].memberFolded("url").stringOf()
		return s
	}
	own := url.QueryEscape(a.String())
	out, _, _, err := Restore(sub, binding, stated, counterpart, answer([][]byte{stored}, own))
	if err != nil {
		t.Fatal(err)
	}
	root, err := scanJSON([]byte(baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	ownEndpoint, _ := root.member("channel").member("endpoint").stringOf()
	if want := counterpart + "/Subscription?url=" + url.QueryEscape(fhirSearchEscaper.Replace(ownEndpoint)) + "&status=active"; selfOf(t, out) != want {
		t.Fatalf("self %q, want %q", selfOf(t, out), want)
	}
	other, b, _ := throughBoth(t, sub, not, Binding{Subscriber: binding.Subscriber, Notifier: "payer-2", Topic: binding.Topic}, baseSubscription)
	for name, c := range map[string]struct {
		entries [][]byte
		qv      string
	}{
		"an answer with no Subscription":            {nil, own},
		"another notifier's address":                {[][]byte{stored}, url.QueryEscape(b.String())},
		"an address no entry held":                  {[][]byte{other}, own},
		"the subscriber's address and another's":    {[][]byte{stored}, url.QueryEscape(a.String() + "," + b.String())},
		"an address that is not a notification one": {[][]byte{stored}, url.QueryEscape("https://elsewhere.example/x")},
	} {
		body := answer(c.entries, c.qv)
		out, _, _, err := Restore(sub, binding, stated, counterpart, body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := counterpart + "/Subscription?url=" + c.qv + "&status=active"; selfOf(t, out) != want {
			t.Errorf("%s: self %q, want the url as written, %q", name, selfOf(t, out), want)
		}
	}
}
