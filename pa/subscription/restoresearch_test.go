package subscription

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// E-20 restores a search answer's link url from the search the subscriber's
// gateway framed (Search): when every address the url names is a
// notification address for this subscriber and topic whose sealed value is
// one this gateway sealed for the search, the url becomes the endpoint the
// subscriber's system searched by, whatever Subscriptions the answer holds.
// So an empty answer's self link, and during a key overlap a link naming
// both keys' addresses, repeat the subscriber's search. Any other url naming
// a notification address keeps its value as written, and why is recorded
// (NotMadeLinkURL); every other parameter keeps its bytes.
func TestRestoreSearch_LinkURLFromTheSearch(t *testing.T) {
	const stated, counterpart = "https://notifier.example/fhir", "https://subscriber-gw.example/cdex/provider/payer-1"
	answer := func(entries [][]byte, qv string) []byte {
		var es []string
		for _, e := range entries {
			es = append(es, `{"fullUrl":"`+stated+`/Subscription/s1","resource":`+string(e)+`}`)
		}
		return []byte(`{"resourceType":"Bundle","type":"searchset","total":` + string(rune('0'+len(entries))) + `,"link":[{"relation":"self","url":"` + stated + `/Subscription?url=` + qv + `&status=active"}],"entry":[` + strings.Join(es, ",") + `]}`)
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
	addrOf := func(sealed string) string {
		return Address{Ingress: notifierIngress, Topic: binding.Topic, Subscriber: binding.Subscriber, SealedEndpoint: sealed}.String()
	}
	escaped := func(addrs ...string) string {
		var vs []string
		for _, a := range addrs {
			vs = append(vs, fhirSearchEscaper.Replace(a))
		}
		return url.QueryEscape(strings.Join(vs, ","))
	}
	restored := counterpart + "/Subscription?url=" + url.QueryEscape(fhirSearchEscaper.Replace(subEndpoint)) + "&status=active"

	t.Run("an answer with no Subscription", func(t *testing.T) {
		sub, _ := edgeKeys(t)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		out, made, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, answer(nil, escaped(addrOf(sealed[0]))))
		if err != nil {
			t.Fatal(err)
		}
		if got := selfOf(t, out); got != restored {
			t.Fatalf("self %q, want %q", got, restored)
		}
		if made != 1 || len(notMade) != 0 {
			t.Fatalf("made %d, not made %v: want the link alone, made", made, notMade)
		}
	})

	t.Run("a key overlap, the Subscription under one key", func(t *testing.T) {
		sub := mustKeys(t, "provider-1", curKey, predKey)
		_, not := edgeKeys(t)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		if len(sealed) != 2 {
			t.Fatalf("sealed %d values, want one per key", len(sealed))
		}
		stored, _, _ := throughBoth(t, sub, not, binding, baseSubscription)
		out, _, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, answer([][]byte{stored}, escaped(addrOf(sealed[0]), addrOf(sealed[1]))))
		if err != nil {
			t.Fatal(err)
		}
		if got := selfOf(t, out); got != restored {
			t.Fatalf("self %q, want %q", got, restored)
		}
		if len(notMade) != 0 {
			t.Fatalf("not made %v", notMade)
		}
	})

	t.Run("not framed for this search", func(t *testing.T) {
		sub, _ := edgeKeys(t)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		other := sub.SealEndpointForSearch(binding, "https://ehr.example/other")
		kid, body, _ := strings.Cut(sealed[0], ".")
		for name, qv := range map[string]string{
			"another endpoint's sealed value": escaped(addrOf(other[0])),
			"a sealed value changed":          escaped(addrOf(kid + ".AAAA" + body)),
			"another subscriber's address":    escaped(Address{Ingress: notifierIngress, Topic: binding.Topic, Subscriber: "provider-2", SealedEndpoint: sealed[0]}.String()),
			"another topic's address":         escaped(Address{Ingress: notifierIngress, Topic: "other", Subscriber: binding.Subscriber, SealedEndpoint: sealed[0]}.String()),
			"the search's and another":        escaped(addrOf(sealed[0]), addrOf(other[0])),
			// Values the loop rule reads as a notification address though
			// they are no exact tail, and values SearchURLValues does not
			// read: each is left as written, and recorded.
			"an address with a trailing slash":   escaped(addrOf(sealed[0]) + "/"),
			"an address with its key id escaped": escaped(strings.Replace(addrOf(sealed[0]), "/"+kid+"/", "/%"+strings.ToUpper(hexOf(kid[0]))+kid[1:]+"/", 1)),
			"a value that does not unescape":     escaped(addrOf(sealed[0])) + "%zz",
			"an empty part":                      escaped(addrOf(sealed[0])) + url.QueryEscape(",,"),
			"a part that is no http URL":         escaped(addrOf(sealed[0])) + url.QueryEscape(",ftp://x.example/y"),
		} {
			out, _, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, answer(nil, qv))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if want := counterpart + "/Subscription?url=" + qv + "&status=active"; selfOf(t, out) != want {
				t.Errorf("%s: self %q, want the url as written %q", name, selfOf(t, out), want)
			}
			if !slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}) {
				t.Errorf("%s: not made %v, want %s", name, notMade, NotMadeLinkURL)
			}
		}
		// A url that names no notification address is the subscriber's own
		// value as written: nothing to restore, nothing recorded.
		qv := escaped("https://elsewhere.example/x")
		out, _, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, answer(nil, qv))
		if err != nil {
			t.Fatal(err)
		}
		if want := counterpart + "/Subscription?url=" + qv + "&status=active"; selfOf(t, out) != want || len(notMade) != 0 {
			t.Errorf("a url naming no notification address: self %q, not made %v", selfOf(t, out), notMade)
		}
	})

	t.Run("a link outside the stated base", func(t *testing.T) {
		// A link the provider's server wrote at another base is not
		// re-keyed; when its url names a notification address, why is
		// recorded, never passed over.
		sub, _ := edgeKeys(t)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"https://internal.example/fhir/Subscription?url=` + escaped(addrOf(sealed[0])) + `"}]}`)
		out, made, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, body)
		if err != nil {
			t.Fatal(err)
		}
		if made != 0 || string(out) != string(body) {
			t.Fatalf("made %d: %s, want the answer as written", made, out)
		}
		if !slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}) {
			t.Fatalf("not made %v, want %s", notMade, NotMadeLinkURL)
		}
		// One whose url names no address: nothing recorded.
		plain := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"https://internal.example/fhir/Subscription?url=` + escaped("https://elsewhere.example/x") + `"}]}`)
		if _, _, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, plain); err != nil || len(notMade) != 0 {
			t.Fatalf("a link naming no address: %v, not made %v", err, notMade)
		}
		// A link that is no absolute http or https address (a relative one,
		// a urn), or any link when the provider's gateway stated no base, is
		// neither re-keyed nor restored: recorded the same way.
		for name, c := range map[string]struct{ base, link string }{
			"a relative link":  {stated, "Subscription?url=" + escaped(addrOf(sealed[0]))},
			"a urn link":       {stated, "urn:x:Subscription?url=" + escaped(addrOf(sealed[0]))},
			"no stated base":   {"", stated + "/Subscription?url=" + escaped(addrOf(sealed[0]))},
			"a relative plain": {stated, "Subscription?url=" + escaped("https://elsewhere.example/x")},
		} {
			body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"` + c.link + `"}]}`)
			out, made, notMade, err := RestoreSearch(sub, binding, c.base, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, body)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if made != 0 || string(out) != string(body) {
				t.Errorf("%s: made %d: %s, want the answer as written", name, made, out)
			}
			want := name != "a relative plain"
			if got := slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}); got != want {
				t.Errorf("%s: not made %v, want %s recorded: %v", name, notMade, NotMadeLinkURL, want)
			}
		}
		// An entry's fullUrl is no link: one of the same form is never
		// recorded as a link url not restored.
		for name, c := range map[string]struct{ base, fullURL string }{
			"a relative fullUrl":         {stated, "Subscription?url=" + escaped(addrOf(sealed[0]))},
			"a fullUrl, no stated base":  {"", stated + "/Subscription?url=" + escaped(addrOf(sealed[0]))},
			"a fullUrl outside the base": {stated, "https://internal.example/fhir/Subscription?url=" + escaped(addrOf(sealed[0]))},
		} {
			body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"entry":[{"fullUrl":"` + c.fullURL + `","resource":{"resourceType":"OperationOutcome"}}]}`)
			_, _, notMade, err := RestoreSearch(sub, binding, c.base, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, body)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}) {
				t.Errorf("%s: not made %v, want no %s for a fullUrl", name, notMade, NotMadeLinkURL)
			}
		}
	})

	t.Run("several url parameters", func(t *testing.T) {
		// Every url parameter of a link is read, with or without a modifier,
		// never only the first: when the link is left with any of them
		// naming a notification address, or unreadable, why is recorded.
		// Only a url parameter without a modifier is restored.
		sub := mustKeys(t, "provider-1", curKey, predKey)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		addr, plain := escaped(addrOf(sealed[0])), escaped("https://elsewhere.example/x")
		end := url.QueryEscape(fhirSearchEscaper.Replace(subEndpoint))
		for name, c := range map[string]struct {
			query, want string
			recorded    bool
		}{
			"no address, then the search's address":    {"url=" + plain + "&url=" + addr, "url=" + plain + "&url=" + addr, true},
			"the search's address, then no address":    {"url=" + addr + "&url=" + plain, "url=" + addr + "&url=" + plain, true},
			"a modifier naming an address":             {"url:below=" + addr + "&status=active", "url:below=" + addr + "&status=active", true},
			"a restored url and a modifier naming one": {"url=" + addr + "&url:below=" + addr, "url=" + end + "&url:below=" + addr, true},
			"a modifier naming none":                   {"url:below=" + plain + "&status=active", "url:below=" + plain + "&status=active", false},
			"a url whose name is escaped, naming one":  {"u%72l=" + addr, "u%72l=" + addr, true},
			"a name that cannot be read, naming one":   {"%zz=" + addr, "%zz=" + addr, true},
			"a name that cannot be read, naming none":  {"%zz=1&url=" + addr, "%zz=1&url=" + end, false},
			"two addresses, each restored":             {"url=" + addr + "&url=" + escaped(addrOf(sealed[1])), "url=" + end + "&url=" + end, false},
			"another parameter naming an address":      {"url=" + addr + "&_id=" + addr + "&urls=" + addr, "url=" + end + "&_id=" + addr + "&urls=" + addr, false},
			"a value that is no URL, then an address":  {"url=" + url.QueryEscape("not-a-url") + "&url=" + addr, "url=" + url.QueryEscape("not-a-url") + "&url=" + addr, true},
			"an exact modifier naming an address":      {"url:exact=" + addr, "url:exact=" + addr, true},
		} {
			body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"` + stated + `/Subscription?` + c.query + `"}]}`)
			out, made, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: subEndpoint, Sealed: sealed}, body)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if want := counterpart + "/Subscription?" + c.want; selfOf(t, out) != want || made != 1 {
				t.Errorf("%s: made %d, self %q, want %q", name, made, selfOf(t, out), want)
			}
			if got := slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}); got != c.recorded {
				t.Errorf("%s: not made %v, want %s recorded: %v", name, notMade, NotMadeLinkURL, c.recorded)
			}
		}
	})

	t.Run("an endpoint read as an address, restored", func(t *testing.T) {
		// The subscriber's own endpoint can read as a notification address
		// to namesAddress (any topic, any participant) and still pass its
		// gateway's loop rule, which reads only real topics and registered
		// participants. Restored to it, the link is not recorded: only a
		// value the link keeps as written is.
		sub, _ := edgeKeys(t)
		const ep = "https://ehr.example/notify/alerts/inbox/AAAAAAAAAAA"
		if !namesAddress(ep) {
			t.Fatalf("%s: want an endpoint namesAddress reads as an address", ep)
		}
		sealed := sub.SealEndpointForSearch(binding, ep)
		body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"` + stated + `/Subscription?url=` + escaped(addrOf(sealed[0])) + `"}]}`)
		out, made, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{Endpoint: ep, Sealed: sealed}, body)
		if err != nil {
			t.Fatal(err)
		}
		if want := counterpart + "/Subscription?url=" + url.QueryEscape(fhirSearchEscaper.Replace(ep)); selfOf(t, out) != want || made != 1 {
			t.Fatalf("made %d, self %q, want %q", made, selfOf(t, out), want)
		}
		if len(notMade) != 0 {
			t.Fatalf("not made %v, want none for a restored link", notMade)
		}
	})

	t.Run("no search framed", func(t *testing.T) {
		// A page's answer (the request carried no url): only the answer's
		// own restored Subscriptions can restore a link, as Restore does,
		// and a link they cannot is recorded.
		sub, _ := edgeKeys(t)
		sealed := sub.SealEndpointForSearch(binding, subEndpoint)
		qv := escaped(addrOf(sealed[0]))
		out, _, notMade, err := RestoreSearch(sub, binding, stated, counterpart, Search{}, answer(nil, qv))
		if err != nil {
			t.Fatal(err)
		}
		if want := counterpart + "/Subscription?url=" + qv + "&status=active"; selfOf(t, out) != want {
			t.Errorf("self %q, want the url as written", selfOf(t, out))
		}
		if !slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}) {
			t.Errorf("not made %v, want %s", notMade, NotMadeLinkURL)
		}
		// Every url parameter is read on a page's answer too, a modifier's
		// included: an address after one naming none, or under url:exact,
		// is recorded.
		for _, q := range []string{"url=" + escaped("https://elsewhere.example/x") + "&url=" + qv, "url:exact=" + qv} {
			body := []byte(`{"resourceType":"Bundle","type":"searchset","total":0,"link":[{"relation":"self","url":"` + stated + `/Subscription?` + q + `"}]}`)
			out, made, notMade, err := Restore(sub, binding, stated, counterpart, body)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if want := counterpart + "/Subscription?" + q; selfOf(t, out) != want || made != 1 {
				t.Errorf("%s: made %d, self %q, want the url as written", q, made, selfOf(t, out))
			}
			if !slices.Contains(notMade, NotMade{Edit: EditEndpointRestore, Reason: NotMadeLinkURL}) {
				t.Errorf("%s: not made %v, want %s", q, notMade, NotMadeLinkURL)
			}
		}
	})
}

// hexOf is c as two hex digits, for a percent-escape.
func hexOf(c byte) string {
	return strconv.FormatInt(int64(c)>>4, 16) + strconv.FormatInt(int64(c)&15, 16)
}
