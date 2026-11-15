package relay

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// e21In are E-21's check's inputs for the subscriber payer-1 at the gateway
// whose ingress is https://gw.example, and addr one of its addresses there,
// as a query carries it.
var e21In = QueryInputs{QueryInputIngress: "https://gw.example", QueryInputTopic: "cdex-task", QueryInputSubscriber: "payer-1"}

const addr = "https%3A%2F%2Fgw.example%2Fnotify%2Fcdex-task%2Fpayer-1%2Fkkkkkkkkkkk%2Fs"

// queryRow is E-21's registry row as a registry holds it.
func queryRow() Edit {
	return Edit{ID: EditSubscriptionURLSearchRekey, Name: "subscription-url-search-rekey", Role: RoleRecipient, Direction: DirectionRequest,
		Kind: EditKindRewriteQuery, Paths: []string{"the url search parameter"}}
}

// A query edit's row is one only of kind rewrite-query, made by a recipient
// on its forward, naming no header and dropping none; any other is refused,
// and a row of that kind for another id is.
func TestValidateEdits_AQueryEdit(t *testing.T) {
	if err := ValidateEdits([]Edit{queryRow()}); err != nil {
		t.Fatalf("the row: %v", err)
	}
	for name, c := range map[string]struct {
		mut  func(e *Edit)
		want string
	}{
		"another kind":             {func(e *Edit) { e.Kind = EditKindReplaceValue }, "a query edit's row, and only one, is of kind rewrite-query"},
		"a requester's":            {func(e *Edit) { e.Role = RoleRequester }, "is not made on a request a recipient forwards"},
		"on an answer":             {func(e *Edit) { e.Direction = DirectionResponse }, "is not made on a request a recipient forwards"},
		"a header named":           {func(e *Edit) { e.Header = "Location" }, "names a header or header inputs"},
		"header inputs named":      {func(e *Edit) { e.HeaderInputs = []string{"x"} }, "names a header or header inputs"},
		"an answer header dropped": {func(e *Edit) { e.DropsAnswerHeaders = []string{ETagHeader} }, "drops"},
		"another id of the kind":   {func(e *Edit) { e.ID = EditCDexTaskReturnRekey }, "a query edit's row, and only one, is of kind rewrite-query"},
	} {
		e := queryRow()
		c.mut(&e)
		if err := ValidateEdits([]Edit{e}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// E-21 writes the url parameter, first, before the search as framed, which
// names none: every other parameter keeps its bytes and its place, and the
// payload names the edit and carries the verified query; the body is the
// request's, empty.
func TestQueryEdited_Accepts(t *testing.T) {
	src := Exact(NewBody(nil, OriginPeerFrame), "")
	from := "status=requested,active&_count=10"
	to := "url=" + addr + "&" + from
	p, err := QueryEdited(src, EditSubscriptionURLSearchRekey, from, to, e21In)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{EditSubscriptionURLSearchRekey}) || p.Len() != 0 {
		t.Fatalf("%v %v %d bytes", p.Ownership(), p.Edits(), p.Len())
	}
	if q, ok := p.EditedQuery(); !ok || q != to {
		t.Fatalf("EditedQuery %q %v, want %q", q, ok, to)
	}
	if s := fmt.Sprint(p); strings.Contains(s, "gw.example") {
		t.Fatalf("the payload prints its query: %s", s)
	}
	// A search framed with no other parameter.
	if _, err := QueryEdited(src, EditSubscriptionURLSearchRekey, "", "url="+addr, e21In); err != nil {
		t.Fatalf("an empty search: %v", err)
	}
	// Two addresses during a sealing-key overlap, by an escaped comma or a
	// plain one; an ingress written with its ending slash.
	two := addr + "%2C" + strings.ReplaceAll(addr, "%2Fkkkkkkkkkkk%2Fs", "%2Fkkkkkkkkkk2%2Fs2")
	if _, err := QueryEdited(src, EditSubscriptionURLSearchRekey, from, "url="+two+"&"+from, e21In); err != nil {
		t.Fatalf("two addresses: %v", err)
	}
	slash := QueryInputs{QueryInputIngress: "https://gw.example/", QueryInputTopic: "cdex-task", QueryInputSubscriber: "payer-1"}
	if _, err := QueryEdited(src, EditSubscriptionURLSearchRekey, from, "url="+addr+"&"+from, slash); err != nil {
		t.Fatalf("an ingress with its ending slash: %v", err)
	}
	// The same query is no edit.
	same, err := QueryEdited(src, EditSubscriptionURLSearchRekey, from, from, e21In)
	if err != nil || same.Ownership() != OwnershipRelayed {
		t.Fatalf("the same query: %v %v", same.Ownership(), err)
	}
	if _, ok := same.EditedQuery(); ok {
		t.Fatal("an unedited payload names a query")
	}
}

// Every rewrite but writing one url first is refused by the core's check, a
// search framed with a url of its own is, and the edit is made only on a
// request relayed exactly, and never by ApplyChanges.
func TestQueryEdited_Refuses(t *testing.T) {
	src := Exact(NewBody(nil, OriginPeerFrame), "")
	const from = "status=active"
	for name, c := range map[string]struct{ from, to string }{
		"another parameter changed":    {from, "url=" + addr + "&status=off"},
		"a parameter added":            {from, "url=" + addr + "&status=active&_summary=count"},
		"a parameter removed":          {from, "url=" + addr},
		"the url empty":                {from, "url=&status=active"},
		"no url written":               {from, "status=active&_count=1"},
		"the url written last":         {from, "status=active&url=" + addr},
		"the url renamed":              {from, "uri=" + addr + "&status=active"},
		"the url given a modifier":     {from, "url:below=" + addr + "&status=active"},
		"two urls written":             {from, "url=" + addr + "&url=" + addr + "&status=active"},
		"more than url on an empty":    {"", "url=" + addr + "&status=active"},
		"a space":                      {from, "url=" + addr + " &status=active"},
		"a fragment":                   {from, "url=" + addr + "#f&status=active"},
		"a control character":          {from, "url=" + addr + "\n&status=active"},
		"a framed url":                 {"url=a&status=active", "url=" + addr + "&url=a&status=active"},
		"a framed url in another case": {"URL=a", "url=" + addr + "&URL=a"},
		"a framed url escaped":         {"%75rl=a", "url=" + addr + "&%75rl=a"},
		"a framed url with a modifier": {"url:below=a", "url=" + addr + "&url:below=a"},
		// The url names this gateway's notification address for the
		// authenticated subscriber, and nothing else.
		"another subscriber's address": {from, "url=" + strings.Replace(addr, "payer-1", "payer-2", 1) + "&" + from},
		"another topic's address":      {from, "url=" + strings.Replace(addr, "cdex-task", "pas", 1) + "&" + from},
		"another gateway's address":    {from, "url=" + strings.Replace(addr, "gw.example", "gw2.example", 1) + "&" + from},
		"the payer's own endpoint":     {from, "url=https%3A%2F%2Fpayer-sys.example%2Fnotify&" + from},
		"an address with no key id":    {from, "url=" + strings.Replace(addr, "%2Fkkkkkkkkkkk%2Fs", "%2Fs", 1) + "&" + from},
		"a key id too short":           {from, "url=" + strings.Replace(addr, "%2Fkkkkkkkkkkk%2F", "%2Fkkkkkkkkkk%2F", 1) + "&" + from},
		"a key id too long":            {from, "url=" + strings.Replace(addr, "%2Fkkkkkkkkkkk%2F", "%2Fkkkkkkkkkkkk%2F", 1) + "&" + from},
		"an address with a third tail": {from, "url=" + addr + "%2Fx&" + from},
		"an address and another":       {from, "url=" + addr + "%2Chttps%3A%2F%2Fpayer-sys.example%2Fnotify&" + from},
		"three addresses":              {from, "url=" + addr + "%2C" + addr + "%2C" + addr + "&" + from},
		"a stray backslash":            {from, "url=" + addr + "%5Cx&" + from},
		"a query in the address":       {from, "url=" + addr + "%3Fx%3D1&" + from},
	} {
		if _, err := QueryEdited(src, EditSubscriptionURLSearchRekey, c.from, c.to, e21In); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want ErrVerifyMismatch", name, err)
		}
	}
	for name, in := range map[string]QueryInputs{
		"no inputs":     nil,
		"no subscriber": {QueryInputIngress: "https://gw.example", QueryInputTopic: "cdex-task"},
		"no ingress":    {QueryInputTopic: "cdex-task", QueryInputSubscriber: "payer-1"},
		"no topic":      {QueryInputIngress: "https://gw.example", QueryInputSubscriber: "payer-1"},
	} {
		if _, err := QueryEdited(src, EditSubscriptionURLSearchRekey, from, "url="+addr+"&"+from, in); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want ErrVerifyMismatch: the check is never skipped", name, err)
		}
	}
	if _, err := QueryEdited(src, EditCDexTaskReturnRekey, from, "url=b&status=active", e21In); !errors.Is(err, ErrInvalidEdit) {
		t.Errorf("another edit: %v, want ErrInvalidEdit", err)
	}
	if _, err := QueryEdited(src, "E-99", from, "url=b&status=active", e21In); !errors.Is(err, ErrUnknownEdit) {
		t.Errorf("an unregistered edit: %v, want ErrUnknownEdit", err)
	}
	authored, err := Authored(BuilderGatewayRefusal, []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := QueryEdited(authored, EditSubscriptionURLSearchRekey, from, "url=b&status=active", e21In); !errors.Is(err, ErrInvalidEdit) {
		t.Errorf("an authored payload: %v, want ErrInvalidEdit", err)
	}
	body := NewBody([]byte(`{"a":"b"}`), OriginPeerFrame)
	d := mustDoc(t, body)
	if _, err := Apply(body, fhirJSON, EditSubscriptionURLSearchRekey, d.Replace(at(t, d, "a"), []byte(`"c"`))); !errors.Is(err, ErrInvalidEdit) {
		t.Errorf("ApplyChanges: %v, want ErrInvalidEdit", err)
	}
}
