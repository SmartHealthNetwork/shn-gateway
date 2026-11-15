package subscription

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The two gateways' keys: the subscriber's seals, the notifier's issues the
// credential.
func edgeKeys(t testing.TB) (sub, not Keys) {
	return mustKeys(t, "provider-1", curKey, nil), mustKeys(t, "payer-1", altKey, nil)
}

// throughBoth is a Subscription as the notifier's system stores it: E-18 at
// the subscriber's gateway, then E-19 at the notifier's.
func throughBoth(t testing.TB, sub, not Keys, b Binding, body string) (stored []byte, a Address, sealedHeaders string) {
	t.Helper()
	out, se, sh, err := SealOutbound(sub, b, mustSubscription(t, body))
	if err != nil {
		t.Fatalf("SealOutbound: %v", err)
	}
	a = Address{Ingress: notifierIngress, Topic: b.Topic, Subscriber: b.Subscriber, SealedEndpoint: se}
	stored, err = RekeyForNotifier(not, a, sh, mustSubscription(t, string(out)))
	if err != nil {
		t.Fatalf("RekeyForNotifier: %v", err)
	}
	return stored, a, sh
}

// E-18 seals the channel: it removes channel.endpoint and channel.header, and
// every other byte stays as it was; the sealed values open to the endpoint and
// headers written, and the body it sends holds neither.
func TestSealOutbound(t *testing.T) {
	sub, _ := edgeKeys(t)
	body, se, sh, err := SealOutbound(sub, binding, mustSubscription(t, baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	want := edit(t, edit(t, baseSubscription, `
    "endpoint": "`+subEndpoint+`",`, ``), `,
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, ``)
	if string(body) != want {
		t.Fatalf("body\n%s\nwant every byte but channel.endpoint and channel.header\n%s", body, want)
	}
	if strings.Contains(string(body), "ehr.example/api") || strings.Contains(string(body), "s3cret") {
		t.Fatalf("the body sent holds the endpoint or a header value: %s", body)
	}
	if ep, err := sub.OpenEndpoint(binding, se); err != nil || ep != subEndpoint {
		t.Fatalf("sealed endpoint opens to %q, %v", ep, err)
	}
	if hs, err := sub.OpenHeaders(binding, se, sh); err != nil || !slices.Equal(hs, []string{"Authorization: Bearer s3cret", "X-Custom: a"}) {
		t.Fatalf("sealed headers open to %q, %v", hs, err)
	}
	// The two members anywhere in channel, apart or side by side, first,
	// last or alone: only they go, each with one comma.
	compact := func(channel string) string {
		return `{"resourceType":"Subscription","channel":{` + channel + `}}`
	}
	ep := `"endpoint":"` + subEndpoint + `"`
	for name, c := range map[string]struct{ channel, want string }{
		"endpoint first, header last": {ep + `,"type":"rest-hook","header":["a: b"]`, `"type":"rest-hook"`},
		"header first, endpoint last": {`"header":["a: b"],"type":"rest-hook",` + ep, `"type":"rest-hook"`},
		"side by side in the middle":  {`"type":"rest-hook",` + ep + `,"header":["a: b"],"payload":"application/fhir+json"`, `"type":"rest-hook","payload":"application/fhir+json"`},
		"side by side at the end":     {`"type":"rest-hook",` + ep + `,"header":["a: b"]`, `"type":"rest-hook"`},
		"side by side at the start":   {ep + `,"header":["a: b"],"type":"rest-hook"`, `"type":"rest-hook"`},
		"alone":                       {ep + `,"header":["a: b"]`, ``},
		"no header":                   {`"type":"rest-hook",` + ep, `"type":"rest-hook"`},
		"apart in the middle":         {`"type":"rest-hook",` + ep + `,"payload":"application/fhir+json","header":["a: b"],"x":1`, `"type":"rest-hook","payload":"application/fhir+json","x":1`},
	} {
		body, se, sh, err := SealOutbound(sub, binding, mustSubscription(t, compact(c.channel)))
		if err != nil || string(body) != compact(c.want) {
			t.Errorf("%s: %v\n%s\nwant\n%s", name, err, body, compact(c.want))
			continue
		}
		hs, err := sub.OpenHeaders(binding, se, sh)
		wantHeaders := []string{"a: b"}
		if !strings.Contains(c.channel, "header") {
			wantHeaders = []string{}
		}
		if err != nil || !slices.Equal(hs, wantHeaders) {
			t.Errorf("%s: sealed headers %q, %v", name, hs, err)
		}
	}
	// No header seals an empty set.
	noHeader := edit(t, baseSubscription, `,
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, ``)
	body, se, sh, err = SealOutbound(sub, binding, mustSubscription(t, noHeader))
	if err != nil || string(body) != want {
		t.Fatalf("no header: %v\n%s", err, body)
	}
	if hs, err := sub.OpenHeaders(binding, se, sh); err != nil || hs == nil || len(hs) != 0 {
		t.Fatalf("no header seals an empty set: %q, %v", hs, err)
	}
	for name, body := range map[string]string{
		"no endpoint":           edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`",`, ``),
		"an empty endpoint":     edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`"`, `"endpoint": ""`),
		"an endpoint not text":  edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`"`, `"endpoint": 1`),
		"a header not an array": edit(t, baseSubscription, `"header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, `"header": "a: b"`),
		"a header not text":     edit(t, baseSubscription, `"X-Custom: a"`, `1`),
		"a _header":             edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "_header": [{"extension": []}],`),
		// A member a lenient reader takes for the header, which would
		// cross the network in clear.
		"a header in another case": edit(t, baseSubscription, `"header": [`, `"Header": [`),
		// Extensions on the endpoint would cross the network beside the
		// notification address E-19 writes, and attach to it.
		"a _endpoint":                 edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "_endpoint": {"extension": []},`),
		"a _endpoint in another case": edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "_Endpoint": {"extension": []},`),
	} {
		if out, _, _, err := SealOutbound(sub, binding, mustSubscription(t, body)); err == nil {
			t.Errorf("%s: sealed\n%s", name, out)
		}
	}
	if _, _, _, err := SealOutbound(Keys{}, binding, mustSubscription(t, baseSubscription)); err == nil {
		t.Fatal("zero Keys sealed")
	}
	prev := randReader
	randReader = strings.NewReader("")
	_, _, _, err = SealOutbound(sub, binding, mustSubscription(t, baseSubscription))
	randReader = prev
	if err == nil {
		t.Fatal("sealed with no nonce")
	}
}

// E-19 sets channel.endpoint to the notification address and channel.header
// to exactly the credential and the sealed headers, and nothing else.
func TestRekeyForNotifier(t *testing.T) {
	sub, not := edgeKeys(t)
	sealedOnly, se, sh, err := SealOutbound(sub, binding, mustSubscription(t, baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	a := Address{Ingress: notifierIngress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: se}
	out, err := RekeyForNotifier(not, a, sh, mustSubscription(t, string(sealedOnly)))
	if err != nil {
		t.Fatal(err)
	}
	headers := `["Authorization: ` + not.Credential(a, sh) + `","X-SHN-Channel: ` + sh + `"]`
	// The sealed channel states neither: E-19 adds the endpoint, then the
	// header, after channel's last member.
	want := edit(t, string(sealedOnly), `"valueCode": "full-resource"}]
    }`, `"valueCode": "full-resource"}]
    },"endpoint":"`+a.String()+`","header":`+headers)
	if string(out) != want {
		t.Fatalf("body\n%s\nwant\n%s", out, want)
	}
	// A header already there is replaced, not added to.
	out, err = RekeyForNotifier(not, a, sh, mustSubscription(t, baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	want = edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`"`, `"endpoint": "`+a.String()+`"`)
	want = edit(t, want, subHeaders, headers)
	if string(out) != want {
		t.Fatalf("a header there\n%s\nwant\n%s", out, want)
	}
	for name, body := range map[string]string{
		"an endpoint not a string":        edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`"`, `"endpoint": 1`),
		"an endpoint in another case":     edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`"`, `"Endpoint": "`+subEndpoint+`"`),
		"a header in another case":        edit(t, baseSubscription, `"header": [`, `"Header": [`),
		"no channel":                      `{"resourceType":"Subscription","status":"requested"}`,
		"a channel that is not an object": `{"resourceType":"Subscription","channel":[]}`,
		"a _header":                       edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "_header": [],`),
	} {
		if out, err := RekeyForNotifier(not, a, sh, mustSubscription(t, body)); err == nil {
			t.Errorf("%s: re-keyed\n%s", name, out)
		}
	}
	if _, err := RekeyForNotifier(Keys{}, a, sh, mustSubscription(t, baseSubscription)); err == nil {
		t.Fatal("zero Keys re-keyed")
	}
}

// headerAsObject wraps channel.header's array in an object.
func headerAsObject(s string) string {
	i := strings.Index(s, `"header":[`)
	j := strings.Index(s[i:], "]") + i
	return s[:i] + `"header":{"v":` + s[i+9:j+1] + `}` + s[j+1:]
}

// §7.2's ownership test.
func TestOwns(t *testing.T) {
	sub, not := edgeKeys(t)
	stored, a, sh := throughBoth(t, sub, not, binding, baseSubscription)
	own := mustSubscription(t, string(stored))
	if !not.Owns(own, notifierIngress, "provider-1", topic) {
		t.Fatal("the subscriber's own Subscription is not its")
	}
	otherSub, _, _ := throughBoth(t, sub, not, Binding{"provider-2", "payer-1", "test"}, baseSubscription)
	otherTopic, _, _ := throughBoth(t, sub, not, Binding{"provider-1", "payer-1", "test-task"}, baseSubscription)
	cred := not.Credential(a, sh)
	tag := cred[len(cred)-22:]
	flipped := cred[:len(cred)-22] + string(rune(tag[0]^1)) + tag[1:]
	for name, row := range map[string]struct {
		body    string
		ingress string
		who     string
		keys    Keys
	}{
		"another subscriber's":         {string(otherSub), notifierIngress, "provider-1", not},
		"another topic's":              {string(otherTopic), notifierIngress, "provider-1", not},
		"another ingress":              {string(stored), "https://other-gw.example/ingress", "provider-1", not},
		"asked for another subscriber": {string(stored), notifierIngress, "provider-2", not},
		"a credential that fails":      {strings.Replace(string(stored), cred, flipped, 1), notifierIngress, "provider-1", not},
		"another gateway's keys":       {string(stored), notifierIngress, "provider-1", mustKeys(t, "payer-1", predKey, nil)},
		"a header not an array":        {headerAsObject(string(stored)), notifierIngress, "provider-1", not},
		"no credential":                {strings.Replace(string(stored), `"Authorization: `+cred+`",`, ``, 1), notifierIngress, "provider-1", not},
		"no sealed headers":            {strings.Replace(string(stored), `,"X-SHN-Channel: `+sh+`"`, ``, 1), notifierIngress, "provider-1", not},
		"two credentials":              {strings.Replace(string(stored), `"Authorization: `+cred+`",`, `"Authorization: `+cred+`","authorization: `+cred+`",`, 1), notifierIngress, "provider-1", not},
		"other sealed headers":         {strings.Replace(string(stored), `X-SHN-Channel: `+sh, `X-SHN-Channel: `+sh+"x", 1), notifierIngress, "provider-1", not},
		"a foreign endpoint":           {baseSubscription, notifierIngress, "provider-1", not},
		"no endpoint":                  {edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`",`, ``), notifierIngress, "provider-1", not},
	} {
		s, r := ParseSubscription([]byte(row.body), "application/fhir+json")
		if r != nil {
			t.Fatalf("%s: %+v", name, r)
		}
		if row.keys.Owns(s, row.ingress, row.who, topic) {
			t.Errorf("%s: owned", name)
		}
	}
	// A credential issued under the predecessor verifies only while the
	// predecessor is configured.
	before := mustKeys(t, "payer-1", predKey, nil)
	old, _, _ := throughBoth(t, sub, before, binding, baseSubscription)
	if !mustKeys(t, "payer-1", altKey, predKey).Owns(mustSubscription(t, string(old)), notifierIngress, "provider-1", topic) {
		t.Error("a predecessor's credential, the predecessor set: not owned")
	}
	if not.Owns(mustSubscription(t, string(old)), notifierIngress, "provider-1", topic) {
		t.Error("a predecessor's credential, no predecessor: owned")
	}
}

// E-20 restores a single Subscription to what the subscriber wrote.
func TestRestore_ASubscription(t *testing.T) {
	sub, not := edgeKeys(t)
	stored, _, _ := throughBoth(t, sub, not, binding, baseSubscription)
	out, made, notMade, err := Restore(sub, binding, "", "", stored)
	if err != nil || made != 1 || len(notMade) != 0 {
		t.Fatalf("made %d, not made %v, %v", made, notMade, err)
	}
	if !jsonEqual(t, out, []byte(baseSubscription)) {
		t.Fatalf("restored\n%s\nwant what the subscriber wrote\n%s", out, baseSubscription)
	}
	// A Subscription that wrote no header is restored with none.
	noHeader := edit(t, baseSubscription, `,
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, ``)
	stored, _, _ = throughBoth(t, sub, not, binding, noHeader)
	out, made, _, err = Restore(sub, binding, "", "", stored)
	if err != nil || made != 1 || !jsonEqual(t, out, []byte(noHeader)) {
		t.Fatalf("no header: made %d, %v\n%s", made, err, out)
	}
	// Each one left, and why.
	retired := mustKeys(t, "provider-1", predKey, nil)
	retiredStored, _, _ := throughBoth(t, retired, not, binding, baseSubscription)
	otherStored, _, _ := throughBoth(t, sub, not, Binding{"provider-2", "payer-1", "test"}, baseSubscription)
	ownStored, _, sh := throughBoth(t, sub, not, binding, baseSubscription)
	for name, row := range map[string]struct {
		body []byte
		keys Keys
		want string
	}{
		"a foreign address":               {[]byte(baseSubscription), sub, NotMadeForeignAddress},
		"no endpoint":                     {[]byte(edit(t, baseSubscription, `"endpoint": "`+subEndpoint+`",`, ``)), sub, NotMadeNoEndpoint},
		"a retired key":                   {retiredStored, sub, NotMadeKeyRetired},
		"another subscriber's address":    {otherStored, sub, NotMadeAnotherBinding},
		"no sealed headers":               {[]byte(strings.Replace(string(ownStored), `,"X-SHN-Channel: `+sh+`"`, ``, 1)), sub, NotMadeNoSealedHeaders},
		"sealed headers that do not open": {[]byte(strings.Replace(string(ownStored), `X-SHN-Channel: `+sh, `X-SHN-Channel: `+sh[:len(sh)-2], 1)), sub, NotMadeSealInvalid},
		"another holder's keys, same kid": {ownStored, mustKeys(t, "provider-9", curKey, nil), NotMadeSealInvalid},
	} {
		out, made, notMade, err := Restore(row.keys, binding, "", "", row.body)
		if err != nil || made != 0 || !slices.Equal(notMade, []NotMade{{Edit: EditEndpointRestore, Reason: row.want}}) || string(out) != string(row.body) {
			t.Errorf("%s: made %d, not made %v, %v; changed=%v", name, made, notMade, err, string(out) != string(row.body))
		}
	}
	// Within the overlap the predecessor restores; past it, it does not.
	if _, made, _, _ := Restore(mustKeys(t, "provider-1", curKey, predKey), binding, "", "", retiredStored); made != 1 {
		t.Error("the predecessor set: not restored")
	}
	for name, body := range map[string]string{"not JSON": "<x/>", "a repeated member": `{"a":1,"a":2}`} {
		if _, _, _, err := Restore(sub, binding, "", "", []byte(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, _, _, err := Restore(sub, binding, "https://payer.example/fhir", "", stored); err == nil {
		t.Error("a stated base with no counterpart base: no error")
	}
	// Another resource is carried as it is.
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[]}`)
	if out, made, notMade, err := Restore(sub, binding, "", "", oo); err != nil || made != 0 || notMade != nil || string(out) != string(oo) {
		t.Errorf("an OperationOutcome: %s %d %v %v", out, made, notMade, err)
	}
}

// E-20 on a searchset: every match entry that is the subscriber's is
// restored, a foreign one and a retired one left with a NotMade, an outcome
// entry carried, and each fullUrl and link under the stated base re-keyed.
func TestRestore_ASearchset(t *testing.T) {
	sub, not := edgeKeys(t)
	own, _, _ := throughBoth(t, sub, not, binding, baseSubscription)
	retired, _, _ := throughBoth(t, mustKeys(t, "provider-1", predKey, nil), not, binding, baseSubscription)
	const base, counterpart = "http://payer.example/fhir", "https://provider-gw.example/pas/payer/payer-1"
	bundle := `{"resourceType":"Bundle","type":"searchset","total":3,
"link":[{"relation":"self","url":"http://payer.example/fhir/Subscription?url=x&_count=2"},{"relation":"next","url":"http://payer.example/fhir?_getpages=abc&_getpagesoffset=2&_count=2&_bundletype=searchset"},{"relation":"other","url":"http://elsewhere.example/fhir?x=1"}],
"entry":[
{"fullUrl":"http://payer.example/fhir/Subscription/sub-1","resource":` + string(own) + `,"search":{"mode":"match"}},
{"fullUrl":"http://payer.example/fhir/Subscription/sub-2","resource":` + baseSubscription + `,"search":{"mode":"match"}},
{"fullUrl":"http://payer.example/fhir/Subscription/sub-3","resource":` + string(retired) + `,"search":{"mode":"match"}},
{"fullUrl":"Subscription/sub-4","resource":{"resourceType":"Basic"},"search":{"mode":"include"}},
{"fullUrl":"urn:uuid:9b1c","resource":{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]},"search":{"mode":"outcome"}}]}`
	out, made, notMade, err := Restore(sub, binding, base, counterpart, []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	// One Subscription restored, three fullUrls and two links re-keyed.
	if made != 6 {
		t.Errorf("made %d, want 6", made)
	}
	wantNot := []NotMade{{EditEndpointRestore, NotMadeForeignAddress}, {EditEndpointRestore, NotMadeKeyRetired}}
	if !slices.Equal(notMade, wantNot) {
		t.Errorf("not made %v, want %v", notMade, wantNot)
	}
	var got struct {
		Link  []struct{ URL string }
		Entry []struct {
			FullURL  string
			Resource json.RawMessage
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{counterpart + "/Subscription?url=x&_count=2", counterpart + "/Subscription?_getpages=abc&_getpagesoffset=2&_count=2&_bundletype=searchset", "http://elsewhere.example/fhir?x=1"} {
		if got.Link[i].URL != want {
			t.Errorf("link %d = %q, want %q", i, got.Link[i].URL, want)
		}
	}
	for i, want := range []string{counterpart + "/Subscription/sub-1", counterpart + "/Subscription/sub-2", counterpart + "/Subscription/sub-3", "Subscription/sub-4", "urn:uuid:9b1c"} {
		if got.Entry[i].FullURL != want {
			t.Errorf("entry %d fullUrl = %q, want %q", i, got.Entry[i].FullURL, want)
		}
	}
	if !jsonEqual(t, got.Entry[0].Resource, []byte(baseSubscription)) {
		t.Errorf("the subscriber's Subscription is not restored:\n%s", got.Entry[0].Resource)
	}
	if !jsonEqual(t, got.Entry[1].Resource, []byte(baseSubscription)) || !jsonEqual(t, got.Entry[2].Resource, retired) {
		t.Error("an entry left was changed")
	}
	if !strings.Contains(string(out), `"search":{"mode":"outcome"}`) || !strings.Contains(string(out), `{"severity":"information","code":"informational"}`) {
		t.Error("the outcome entry was not carried")
	}
	// An address whose remainder holds a dot segment, raw or
	// percent-encoded, is not re-keyed: a client would resolve it above the
	// per-counterpart base, into another counterpart's.
	for _, dotted := range []string{base + "/../provider-y/Subscription/s1", base + "/%2e%2e/provider-y/Subscription/s1", base + "/%2E./Subscription/s1"} {
		b := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"` + dotted + `","resource":{"resourceType":"OperationOutcome"},"search":{"mode":"outcome"}}],` +
			`"link":[{"relation":"next","url":"` + dotted + `?_getpages=abc"}]}`)
		out, made, _, err := Restore(sub, binding, base, counterpart, b)
		if err != nil || made != 0 || string(out) != string(b) {
			t.Errorf("%s: made %d, %v, out %s; want it carried as sent", dotted, made, err, out)
		}
	}
	// With no stated base, nothing is re-keyed, and that is recorded once.
	_, made, notMade, err = Restore(sub, binding, "", "", []byte(bundle))
	if err != nil || made != 1 || !slices.Contains(notMade, NotMade{EditEndpointRestore, NotMadeNoStatedBase}) || len(notMade) != 3 {
		t.Errorf("no stated base: made %d, not made %v, %v", made, notMade, err)
	}
	// A urn:uuid fullUrl names no server: with no stated base it is neither
	// re-keyed nor recorded.
	urnOnly := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"urn:uuid:9b1c","resource":{"resourceType":"OperationOutcome"}}]}`)
	if out, made, notMade, err := Restore(sub, binding, "", "", urnOnly); err != nil || made != 0 || len(notMade) != 0 || string(out) != string(urnOnly) {
		t.Errorf("a urn:uuid fullUrl, no stated base: made %d, not made %v, %v", made, notMade, err)
	}
	// An entry or link list that is not an array is recorded, never read as
	// empty.
	for name, body := range map[string]string{
		"an entry object": `{"resourceType":"Bundle","entry":{"fullUrl":"http://payer.example/fhir/Subscription/1","resource":` + string(own) + `}}`,
		"a link object":   `{"resourceType":"Bundle","link":{"relation":"next","url":"http://payer.example/fhir?_getpages=a"},"entry":[]}`,
	} {
		_, made, notMade, err := Restore(sub, binding, base, counterpart, []byte(body))
		if err != nil || made != 0 || !slices.Equal(notMade, []NotMade{{EditEndpointRestore, NotMadeUnreadable}}) {
			t.Errorf("%s: made %d, not made %v, %v", name, made, notMade, err)
		}
	}
	// A base that is a prefix of another path segment is not under it.
	_, made, _, _ = Restore(sub, binding, "http://payer.example/fhi", counterpart, []byte(bundle))
	if made != 1 {
		t.Errorf("a base that is only a text prefix re-keyed %d", made-1)
	}
}

// underBase is a structural match.
func TestUnderBase(t *testing.T) {
	const base, cp = "https://payer.example/fhir", "https://gw.example/p"
	for v, want := range map[string]string{
		"https://payer.example/fhir":                     cp,
		"https://payer.example/fhir/Subscription/1":      cp + "/Subscription/1",
		"https://payer.example/fhir?_getpages=a":         cp + "?_getpages=a",
		"https://PAYER.example:443/fhir/Subscription/1":  cp + "/Subscription/1",
		"https://payer.example/fhir/Subscription?url=a?": cp + "/Subscription?url=a?",
		"https://payer.example/fhir2/Subscription/1":     "",
		"http://payer.example/fhir/Subscription/1":       "",
		"https://payer.example:8443/fhir":                "",
		"https://payer.example/fhir/Subscription/1#x":    "",
		"https://u@payer.example/fhir":                   "",
	} {
		got, ok := underBase(v, base, cp+"/")
		if (want == "") == ok || got != want {
			t.Errorf("underBase(%q) = %q, %v; want %q", v, got, ok, want)
		}
	}
}

// E-21 writes the notification address E-19 would write: one value, two in
// an overlap, as one comma-separated value with each address's own commas
// escaped; anything it cannot scope is refused, never sent without url.
func TestRekeySearchURL(t *testing.T) {
	sub := mustKeys(t, "provider-1", curKey, predKey)
	vals := sub.SealEndpointForSearch(binding, subEndpoint)
	q := url.Values{"url": {subEndpoint}, "status": {"requested,active"}}
	addr := func(ingress, se string) string {
		return Address{Ingress: ingress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: se}.String()
	}
	out, r := RekeySearchURL(notifierIngress, binding, vals[:1], q)
	if r != nil || !slices.Equal(out["url"], []string{addr(notifierIngress, vals[0])}) || !slices.Equal(out["status"], q["status"]) {
		t.Fatalf("one value: %v, %+v", out, r)
	}
	out, r = RekeySearchURL(notifierIngress, binding, vals, q)
	if r != nil || !slices.Equal(out["url"], []string{addr(notifierIngress, vals[0]) + "," + addr(notifierIngress, vals[1])}) {
		t.Fatalf("two values: %v, %+v", out, r)
	}
	if q["url"][0] != subEndpoint {
		t.Fatal("q was changed")
	}
	out, _ = RekeySearchURL(notifierIngress, binding, []string{vals[0], vals[0]}, q)
	if !slices.Equal(out["url"], []string{addr(notifierIngress, vals[0])}) {
		t.Fatalf("a value given twice: %v", out["url"])
	}
	// The address E-19 writes is the one E-21 names.
	stored, _, _ := throughBoth(t, mustKeys(t, "provider-1", curKey, nil), mustKeys(t, "payer-1", altKey, nil), binding, baseSubscription)
	if ep, _ := mustSubscription(t, string(stored)).Endpoint(); ep != addr(notifierIngress, vals[0]) {
		t.Fatalf("E-19 wrote %q; E-21 names %q", ep, addr(notifierIngress, vals[0]))
	}
	// Escapes: a comma, a bar, a dollar sign and a backslash in the ingress.
	odd := "https://payer-gw.example/a,b|c$d\\e"
	out, _ = RekeySearchURL(odd, binding, vals[:1], q)
	if want := strings.NewReplacer(`\`, `\\`, `,`, `\,`, `|`, `\|`, `$`, `\$`).Replace(addr(odd, vals[0])); out["url"][0] != want || !strings.Contains(want, `\,`) {
		t.Fatalf("escaped %q, want %q", out["url"][0], want)
	}
	// No url at all in the query: E-21 still writes one.
	if out, r := RekeySearchURL(notifierIngress, binding, vals[:1], url.Values{"_id": {"x"}}); r != nil || len(out["url"]) != 1 {
		t.Fatalf("a query with no url: %v, %+v", out, r)
	}
	for name, ses := range map[string][]string{
		"no sealed endpoint":       nil,
		"an empty list":            {},
		"an empty value":           {""},
		"a malformed value":        {"not-sealed"},
		"one malformed of two":     {vals[0], "AAAAAAAAAAA.!!"},
		"a sealed value too short": {"AAAAAAAAAAA.AAAA"},
		"a value with a space":     {vals[0] + " " + vals[1]},
	} {
		out, r := RekeySearchURL(notifierIngress, binding, ses, q)
		wantRefusal(t, name, r, http.StatusForbidden, recording.RefusalAuthority)
		if out != nil {
			t.Errorf("%s: returned a query %v", name, out)
		}
	}
}

// SearchURLValues reads a url search value into the endpoints it names,
// splitting at unescaped commas, and is the inverse of the escaping E-21
// writes. Each endpoint is an absolute http or https URL with no backslash,
// space or control character.
func TestSearchURLValues(t *testing.T) {
	for v, want := range map[string][]string{
		subEndpoint:                                     {subEndpoint},
		"https://a.example,https://b.example":           {"https://a.example", "https://b.example"},
		`https://a.example/x\,y`:                        {"https://a.example/x,y"},
		`https://a.example/\$\|`:                        {`https://a.example/$|`},
		`https://a.example/a\,b,http://c.example:8080/`: {"https://a.example/a,b", "http://c.example:8080/"},
	} {
		got, err := SearchURLValues(v)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("SearchURLValues(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	for _, v := range []string{"", ",", "a,", ",a", "a,,b", `a\`, `a\x`, `a\ b`,
		// Endpoints that are not absolute http or https URLs, or hold a
		// backslash (escaped or not), a space or a control character.
		"a", "a,b", "https://a.example,b", "/n", "//a.example/n", "ftp://a.example/n", "mailto:a@a.example", "https:a.example",
		"https://", "https:///n", "urn:uuid:1", `https://a.example/\\`, `https://a.example/a\\b,https://b.example`,
		"https://a.example/a b", " https://a.example", "https://a.example/a\tb", "https://a.example/\x7f", "https://a.example/\x00",
	} {
		if got, err := SearchURLValues(v); err == nil {
			t.Errorf("SearchURLValues(%q) = %q; want an error", v, got)
		}
	}
	// What E-21's escaping writes reads back as the values it escaped.
	odd := []string{`https://e.example/a,b|c$d`, "https://f.example"}
	esc := make([]string, len(odd))
	for i, v := range odd {
		esc[i] = fhirSearchEscaper.Replace(v)
	}
	if got, err := SearchURLValues(strings.Join(esc, ",")); err != nil || !slices.Equal(got, odd) {
		t.Errorf("round trip: %q, %v", got, err)
	}
}
