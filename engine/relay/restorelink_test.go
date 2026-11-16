package relay

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// E-20's check of a search answer's link: its url parameter may change only
// to the endpoint E-20 restored, in the same change, in the channel.endpoint
// of the Subscriptions that held the link's notification addresses, written
// as one value; every other parameter keeps its bytes and place. A restore to
// another endpoint, of an address no restored Subscription held, or with any
// other parameter changed, is refused by the core, signed content or not.
func TestSubscriptionLinkURLRestoreCheck(t *testing.T) {
	const (
		addr     = "https://provider-gw.example/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"
		addr2    = "https://provider-gw.example/notify/cdex-task/payer/BBBBBBBBBBB/c2VhbGVk"
		endpoint = "https://payer-sys.example/notify?tenant=a"
		other    = "https://evil.example/notify"
		stated   = "https://provider.example/fhir"
		base     = "https://payer-gw.example/cdex/provider/p1"
	)
	q := func(v string) string { return url.QueryEscape(v) }
	bundle := func(eps []string, link string) []byte {
		var entries []string
		for i, ep := range eps {
			entries = append(entries, `{"fullUrl":"`+stated+`/Subscription/s`+string(rune('1'+i))+`","resource":{"resourceType":"Subscription","id":"s1","channel":{"type":"rest-hook","endpoint":"`+ep+`"}}}`)
		}
		return []byte(`{"resourceType":"Bundle","type":"searchset","link":[{"relation":"self","url":"` + link + `"}],"entry":[` + strings.Join(entries, ",") + `]}`)
	}
	oldLink := stated + "/Subscription?url=" + q(addr) + "&status=requested,active"
	apply := func(t *testing.T, body []byte, restoreEndpoints map[int]string, link string) error {
		t.Helper()
		b := NewBody(body, OriginPeerFrame)
		d := mustDoc(t, b)
		var ops []Op
		for i, ep := range restoreEndpoints {
			ops = append(ops, d.Replace(at(t, d, "entry", string(rune('0'+i)), "resource", "channel", "endpoint"), []byte(`"`+ep+`"`)))
		}
		ops = append(ops, d.Replace(at(t, d, "link", "0", "url"), []byte(`"`+link+`"`)))
		_, err := Apply(b, fhirJSON, EditSubscriptionEndpointRestore, ops...)
		return err
	}
	good := base + "/Subscription?url=" + q(endpoint) + "&status=requested,active"
	if err := apply(t, bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, good); err != nil {
		t.Fatalf("the restore: %v", err)
	}
	// During a key overlap the link names two addresses, each one a
	// restored Subscription held, both restored to one endpoint.
	two := stated + "/Subscription?url=" + q(addr+","+addr2)
	if err := apply(t, bundle([]string{addr, addr2}, two), map[int]string{0: endpoint, 1: endpoint}, base+"/Subscription?url="+q(endpoint)); err != nil {
		t.Fatalf("the restore of two addresses: %v", err)
	}
	// The base re-key alone, the query as written.
	if err := apply(t, bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, base+"/Subscription?url="+q(addr)+"&status=requested,active"); err != nil {
		t.Fatalf("the base re-key alone: %v", err)
	}
	for name, c := range map[string]struct {
		body     []byte
		restored map[int]string
		link     string
	}{
		"a restore to another endpoint":         {bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, base + "/Subscription?url=" + q(other) + "&status=requested,active"},
		"a restore with nothing opened":         {bundle([]string{addr}, oldLink), nil, good},
		"a restore of an address no entry held": {bundle([]string{addr2}, oldLink), map[int]string{0: endpoint}, good},
		"a restore to two endpoints":            {bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, base + "/Subscription?url=" + q(endpoint+","+other) + "&status=requested,active"},
		"two addresses restored apart":          {bundle([]string{addr, addr2}, two), map[int]string{0: endpoint, 1: other}, base + "/Subscription?url=" + q(endpoint)},
		"another parameter changed":             {bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, base + "/Subscription?url=" + q(endpoint) + "&status=active"},
		"a parameter added":                     {bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, good + "&_count=1"},
		"a parameter renamed to url":            {bundle([]string{addr}, stated+"/Subscription?uri="+q(addr)), map[int]string{0: endpoint}, base + "/Subscription?url=" + q(endpoint)},
		"a query added where none was":          {bundle([]string{addr}, stated+"/Subscription"), map[int]string{0: endpoint}, base + "/Subscription?url=" + q(endpoint)},
		"a value that does not unescape":        {bundle([]string{addr}, oldLink), map[int]string{0: endpoint}, base + "/Subscription?url=%ZZ&status=requested,active"},
	} {
		if err := apply(t, c.body, c.restored, c.link); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check of E-20 to refuse it", name, err)
		}
	}
}

// E-20 is held to its places on every change, signed content or not: on an
// unsigned answer, a change anywhere else is refused by the core. A
// Subscription's resourceType is read folded, as the subscriber's gateway
// reads it, so a restore the gateway makes is never refused here.
func TestSubscriptionRestoreHeldToItsPlacesUnsigned(t *testing.T) {
	apply := func(t *testing.T, raw string, op func(d *Document) Op) error {
		t.Helper()
		b := NewBody([]byte(raw), OriginPeerFrame)
		_, err := Apply(b, fhirJSON, EditSubscriptionEndpointRestore, op(mustDoc(t, b)))
		return err
	}
	const sub = `{"resourceType":"Subscription","status":"active","channel":{"type":"rest-hook","endpoint":"x","header":["a"]}}`
	for name, c := range map[string]struct {
		raw string
		op  func(t *testing.T, d *Document) Op
	}{
		"a Subscription's status": {sub, func(t *testing.T, d *Document) Op { return d.Replace(at(t, d, "status"), []byte(`"off"`)) }},
		"a Subscription's channel type": {sub, func(t *testing.T, d *Document) Op {
			return d.Replace(at(t, d, "channel", "type"), []byte(`"email"`))
		}},
		"the removal of channel.endpoint": {sub, func(t *testing.T, d *Document) Op {
			return d.RemoveMember(at(t, d, "channel"), "endpoint")
		}},
		"a nested Subscription's endpoint": {`{"resourceType":"Parameters","parameter":[{"resource":` + sub + `}]}`, func(t *testing.T, d *Document) Op {
			return d.Replace(at(t, d, "parameter", "0", "resource", "channel", "endpoint"), []byte(`"y"`))
		}},
		"a link url in a root that is not a Bundle": {`{"resourceType":"Parameters","link":[{"url":"a"}]}`, func(t *testing.T, d *Document) Op {
			return d.Replace(at(t, d, "link", "0", "url"), []byte(`"b"`))
		}},
		"a fullUrl in a root that is not a Bundle": {`{"resourceType":"Parameters","entry":[{"fullUrl":"a"}]}`, func(t *testing.T, d *Document) Op {
			return d.Replace(at(t, d, "entry", "0", "fullUrl"), []byte(`"b"`))
		}},
		"an endpoint replaced with a number": {sub, func(t *testing.T, d *Document) Op {
			return d.Replace(at(t, d, "channel", "endpoint"), []byte(`1`))
		}},
	} {
		if err := apply(t, c.raw, func(d *Document) Op { return c.op(t, d) }); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check of E-20 to refuse it", name, err)
		}
	}
	// The resourceType in another case, at the root and as an entry's
	// resource: restored, as the subscriber's gateway reads it.
	folded := strings.Replace(sub, `"resourceType"`, `"resourcetype"`, 1)
	if err := apply(t, folded, func(d *Document) Op { return d.Replace(at(t, d, "channel", "endpoint"), []byte(`"y"`)) }); err != nil {
		t.Errorf("a folded resourceType at the root: %v", err)
	}
	if err := apply(t, `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":`+folded+`}]}`, func(d *Document) Op {
		return d.Replace(at(t, d, "entry", "0", "resource", "channel", "endpoint"), []byte(`"y"`))
	}); err != nil {
		t.Errorf("a folded resourceType in an entry: %v", err)
	}
}

// One address restored to two endpoints in one change set: no link is
// restored to either. Two links, one restored to each endpoint, so the
// refusal does not rest on which of the two a map happens to keep.
func TestSubscriptionLinkURLRestoreRefusesAnAddressRestoredTwoWays(t *testing.T) {
	const addr = "https://provider-gw.example/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"
	entry := `{"resource":{"resourceType":"Subscription","channel":{"endpoint":"` + addr + `"}}}`
	link := `{"relation":"self","url":"https://p.example/fhir/Subscription?url=` + url.QueryEscape(addr) + `"}`
	b := NewBody([]byte(`{"resourceType":"Bundle","type":"searchset","link":[`+link+`,`+link+`],"entry":[`+entry+`,`+entry+`]}`), OriginPeerFrame)
	d := mustDoc(t, b)
	const one, two = "https://a.example/x", "https://b.example/x"
	_, err := Apply(b, fhirJSON, EditSubscriptionEndpointRestore,
		d.Replace(at(t, d, "entry", "0", "resource", "channel", "endpoint"), []byte(`"`+one+`"`)),
		d.Replace(at(t, d, "entry", "1", "resource", "channel", "endpoint"), []byte(`"`+two+`"`)),
		d.Replace(at(t, d, "link", "0", "url"), []byte(`"https://g.example/p/Subscription?url=`+url.QueryEscape(one)+`"`)),
		d.Replace(at(t, d, "link", "1", "url"), []byte(`"https://g.example/p/Subscription?url=`+url.QueryEscape(two)+`"`)))
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Errorf("an address restored two ways, a link to each: %v, want refused", err)
	}
	// One link, restored to the endpoint the later of the two entries was
	// restored to: refused, the address being restored two ways, whichever
	// value a walk of the restores would keep last.
	b1 := NewBody([]byte(`{"resourceType":"Bundle","type":"searchset","link":[`+link+`],"entry":[`+entry+`,`+entry+`]}`), OriginPeerFrame)
	d1 := mustDoc(t, b1)
	for _, target := range []string{one, two} {
		_, err = Apply(b1, fhirJSON, EditSubscriptionEndpointRestore,
			d1.Replace(at(t, d1, "entry", "0", "resource", "channel", "endpoint"), []byte(`"`+one+`"`)),
			d1.Replace(at(t, d1, "entry", "1", "resource", "channel", "endpoint"), []byte(`"`+two+`"`)),
			d1.Replace(at(t, d1, "link", "0", "url"), []byte(`"https://g.example/p/Subscription?url=`+url.QueryEscape(target)+`"`)))
		if !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("one link restored to %s: %v, want refused", target, err)
		}
	}
	// Nor is a link restored to an empty value for such an address.
	b2 := NewBody([]byte(`{"resourceType":"Bundle","type":"searchset","link":[`+link+`],"entry":[`+entry+`,`+entry+`]}`), OriginPeerFrame)
	d2 := mustDoc(t, b2)
	_, err = Apply(b2, fhirJSON, EditSubscriptionEndpointRestore,
		d2.Replace(at(t, d2, "entry", "0", "resource", "channel", "endpoint"), []byte(`"`+one+`"`)),
		d2.Replace(at(t, d2, "entry", "1", "resource", "channel", "endpoint"), []byte(`"`+two+`"`)),
		d2.Replace(at(t, d2, "link", "0", "url"), []byte(`"https://g.example/p/Subscription?url="`)))
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Errorf("a link restored to empty: %v, want refused", err)
	}
}

// The places E-20's check reads by structure, each with its row: a
// Subscription only as the root or an entry's resource, a url only in a root
// Bundle's link array, a header replaced only where it was an array, a
// channel's endpoint only by its exact name, and a link restored only to an
// endpoint a restorable Subscription's channel was restored to.
func TestSubscriptionRestorePlacesByStructure(t *testing.T) {
	const addr = "https://provider-gw.example/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"
	q := url.QueryEscape
	for name, c := range map[string]struct {
		raw string
		ops func(t *testing.T, d *Document) []Op
	}{
		"a Subscription under an entry's request": {`{"resourceType":"Bundle","entry":[{"request":{"resourceType":"Subscription","channel":{"endpoint":"x"}}}]}`, func(t *testing.T, d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "request", "channel", "endpoint"), []byte(`"y"`))}
		}},
		"a url in a root Bundle's other array": {`{"resourceType":"Bundle","foo":[{"url":"a"}]}`, func(t *testing.T, d *Document) []Op {
			return []Op{d.Replace(at(t, d, "foo", "0", "url"), []byte(`"b"`))}
		}},
		"a header string replaced with an array": {`{"resourceType":"Subscription","channel":{"endpoint":"x","header":"a"}}`, func(t *testing.T, d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "header"), []byte(`["b"]`))}
		}},
		"an Endpoint in another case": {`{"resourceType":"Subscription","channel":{"Endpoint":"x"}}`, func(t *testing.T, d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "Endpoint"), []byte(`"y"`))}
		}},
		"a link restored to a fullUrl's replacement": {`{"resourceType":"Bundle","link":[{"url":"https://p.example/fhir/Subscription?url=` + q(addr) + `"}],"entry":[{"fullUrl":"` + addr + `"}]}`, func(t *testing.T, d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry", "0", "fullUrl"), []byte(`"https://evil.example/x"`)),
				d.Replace(at(t, d, "link", "0", "url"), []byte(`"https://g.example/p/Subscription?url=`+q("https://evil.example/x")+`"`))}
		}},
	} {
		b := NewBody([]byte(c.raw), OriginPeerFrame)
		d := mustDoc(t, b)
		if _, err := Apply(b, fhirJSON, EditSubscriptionEndpointRestore, c.ops(t, d)...); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check of E-20 to refuse it", name, err)
		}
	}
}

// E-20's check costs one pass over the restores however many links an
// answer carries, and nothing per edit in the width of the root Bundle: the
// time stays near linear in the answer, where a check quadratic in either is
// not (wantLinearCost). It is timed with the model it reads, built in each
// call, since the model keeps what the check decides (the Apply around it has
// costs of its own, which its own rows hold).
func TestSubscriptionRestoreCheckIsLinear(t *testing.T) {
	const addr = "https://provider-gw.example/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"
	wantLinearCost(t, "E-20 restore", 32768, func(n int) func() {
		var entries, links, wide []string
		for i := 0; i < n; i++ {
			entries = append(entries, `{"resource":{"resourceType":"Subscription","channel":{"endpoint":"`+addr+`"}}}`)
			links = append(links, `{"relation":"self","url":"https://p.example/fhir/Subscription?url=`+url.QueryEscape(addr)+`"}`)
			wide = append(wide, `"x`+strconv.Itoa(i)+`":0`)
		}
		b := NewBody([]byte(`{`+strings.Join(wide, ",")+`,"type":"searchset","link":[`+strings.Join(links, ",")+`],"entry":[`+strings.Join(entries, ",")+`],"resourceType":"Bundle"}`), OriginPeerFrame)
		d := mustDoc(t, b)
		var ops []splice.Op
		endpoints, urls := eachAt(t, d, []string{"entry"}, "resource", "channel", "endpoint"), eachAt(t, d, []string{"link"}, "url")
		for i := 0; i < n; i++ {
			ops = append(ops, d.Replace(endpoints[i], []byte(`"https://payer.example/x"`)).op)
			ops = append(ops, d.Replace(urls[i], []byte(`"https://g.example/p/Subscription?url=`+url.QueryEscape("https://payer.example/x")+`"`)).op)
		}
		call := func() {
			m, err := buildModel(d.d, b.bytes(), ops)
			if err != nil {
				t.Fatal(err)
			}
			for _, eff := range m.effects {
				if eff != nil && !m.subscriptionChannelRestore(*eff) {
					t.Fatal("a restore the check refuses")
				}
			}
		}
		call()
		return call
	})
}

// E-20's check of a link restored from the search the subscriber's gateway
// framed (the restore's inputs, SubscriptionRestoreInputs): the url may change
// to the endpoint input, written as one value, when every address it named is
// a notification address for the subscriber and topic inputs whose key id and
// sealed value are one of the sealed inputs; with no restored Subscription in
// the answer, and whatever precedes an address's tail. A restore to any other
// value, of an address the search did not frame, or with any other parameter
// changed, is refused; so are inputs on any other edit, and inputs other than
// the four E-20 reads.
func TestSubscriptionLinkURLRestoreFromTheSearch(t *testing.T) {
	const (
		sealed1  = "AAAAAAAAAAA.c2VhbGVk"
		sealed2  = "BBBBBBBBBBB.c2VhbGVkMg"
		addr     = "https://provider-gw.example/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"
		addr2    = "https://provider-gw.example/notify/cdex-task/payer/BBBBBBBBBBB/c2VhbGVkMg"
		endpoint = "https://payer-sys.example/notify?tenant=a"
		stated   = "https://provider.example/fhir"
		base     = "https://payer-gw.example/cdex/provider/p1"
	)
	q := func(v string) string { return url.QueryEscape(v) }
	in := SubscriptionRestoreInputs{Subscriber: "payer", Topic: "cdex-task", Sealed: []string{sealed1, sealed2}, Endpoint: endpoint}
	empty := func(link string) []byte {
		return []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"` + link + `"}]}`)
	}
	apply := func(t *testing.T, body []byte, link string, in *SubscriptionRestoreInputs) error {
		t.Helper()
		b := NewBody(body, OriginPeerFrame)
		d := mustDoc(t, b)
		_, err := ApplyChanges(b, fhirJSON, Change{Edit: EditSubscriptionEndpointRestore, Ops: []Op{d.Replace(at(t, d, "link", "0", "url"), []byte(`"`+link+`"`))}, Restore: in})
		return err
	}
	oldLink := stated + "/Subscription?url=" + q(addr) + "&status=requested,active"
	good := base + "/Subscription?url=" + q(endpoint) + "&status=requested,active"
	if err := apply(t, empty(oldLink), good, &in); err != nil {
		t.Fatalf("the restore of an empty answer's link: %v", err)
	}
	two := stated + "/Subscription?url=" + q(addr+","+addr2)
	if err := apply(t, empty(two), base+"/Subscription?url="+q(endpoint), &in); err != nil {
		t.Fatalf("the restore of both keys' addresses: %v", err)
	}
	// What precedes the tail is not read: the payer's gateway does not
	// know the provider gateway's ingress.
	elsewhere := strings.Replace(oldLink, url.QueryEscape("https://provider-gw.example"), url.QueryEscape("https://other-gw.example/x"), 1)
	if err := apply(t, empty(elsewhere), good, &in); err != nil {
		t.Fatalf("an address at another ingress: %v", err)
	}
	for name, c := range map[string]struct {
		link, to string
		in       *SubscriptionRestoreInputs
	}{
		"no inputs":                      {oldLink, good, nil},
		"a restore to another value":     {oldLink, base + "/Subscription?url=" + q("https://evil.example/x") + "&status=requested,active", &in},
		"a restore to two values":        {oldLink, base + "/Subscription?url=" + q(endpoint+",https://evil.example/x") + "&status=requested,active", &in},
		"a sealed value not framed":      {strings.Replace(oldLink, "c2VhbGVk", "b3RoZXI", 1), good, &in},
		"a key id not framed":            {strings.Replace(oldLink, "AAAAAAAAAAA", "CCCCCCCCCCC", 1), good, &in},
		"another subscriber":             {strings.Replace(oldLink, "%2Fpayer%2F", "%2Fpayer-2%2F", 1), good, &in},
		"another topic":                  {strings.Replace(oldLink, "cdex-task", "pas", 1), good, &in},
		"a framed address and another":   {stated + "/Subscription?url=" + q(addr+",https://evil.example/x"), base + "/Subscription?url=" + q(endpoint), &in},
		"not an address":                 {stated + "/Subscription?url=" + q("https://elsewhere.example/x"), base + "/Subscription?url=" + q(endpoint), &in},
		"an address with a query":        {stated + "/Subscription?url=" + q(addr+"?a=b"), base + "/Subscription?url=" + q(endpoint), &in},
		"an address that is not http":    {stated + "/Subscription?url=" + q(strings.Replace(addr, "https:", "ftp:", 1)), base + "/Subscription?url=" + q(endpoint), &in},
		"an address with no host":        {stated + "/Subscription?url=" + q("https:///notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"), base + "/Subscription?url=" + q(endpoint), &in},
		"a query that ends in a tail":    {stated + "/Subscription?url=" + q("https://evil.example/x?y=/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"), base + "/Subscription?url=" + q(endpoint), &in},
		"a space before the tail":        {stated + "/Subscription?url=" + q("https://provider-gw.example/a b/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"), base + "/Subscription?url=" + q(endpoint), &in},
		"a backslash before the tail":    {stated + "/Subscription?url=" + q(`https://provider-gw.example/a\\b/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk`), base + "/Subscription?url=" + q(endpoint), &in},
		"a fragment that ends in a tail": {stated + "/Subscription?url=" + q("https://evil.example/x#/notify/cdex-task/payer/AAAAAAAAAAA/c2VhbGVk"), base + "/Subscription?url=" + q(endpoint), &in},
		"another parameter changed":      {oldLink, base + "/Subscription?url=" + q(endpoint) + "&status=active", &in},
		"empty inputs":                   {oldLink, good, &SubscriptionRestoreInputs{}},
		"an empty endpoint input":        {oldLink, base + "/Subscription?url=&status=requested,active", &SubscriptionRestoreInputs{Subscriber: "payer", Topic: "cdex-task", Sealed: []string{sealed1}}},
	} {
		if err := apply(t, empty(c.link), c.to, c.in); !errors.Is(err, ErrVerifyMismatch) {
			t.Errorf("%s: %v, want the core's check of E-20 to refuse it", name, err)
		}
	}
	// Two changes of one set may not both carry inputs.
	b2 := NewBody(empty(oldLink), OriginPeerFrame)
	d2 := mustDoc(t, b2)
	if _, err := ApplyChanges(b2, fhirJSON,
		Change{Edit: EditSubscriptionEndpointRestore, Ops: []Op{d2.Replace(at(t, d2, "link", "0", "url"), []byte(`"`+good+`"`))}, Restore: &in},
		Change{Edit: EditSubscriptionEndpointRestore, Restore: &in}); !errors.Is(err, ErrInvalidEdit) {
		t.Errorf("two changes carrying inputs: %v, want ErrInvalidEdit", err)
	}
	// The inputs are E-20's alone.
	b := NewBody([]byte(`{"resourceType":"Patient","id":"a"}`), OriginPeerFrame)
	d := mustDoc(t, b)
	if _, err := ApplyChanges(b, fhirJSON, Change{Edit: EditCDSCallbackStrip, Ops: []Op{d.Replace(at(t, d, "id"), []byte(`"b"`))}, Restore: &in}); !errors.Is(err, ErrInvalidEdit) {
		t.Errorf("restore inputs on another edit: %v, want ErrInvalidEdit", err)
	}
}
