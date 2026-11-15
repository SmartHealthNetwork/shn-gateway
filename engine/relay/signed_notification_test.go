package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// notificationBody is an id-only CDex Task notification, its addresses into
// the provider's server absolute, with sig ("" for none) as its
// Bundle.signature member.
func notificationBody(sig string) Body {
	const base = "https://provider.example/fhir"
	return NewBody([]byte(`{"resourceType":"Bundle","type":"history","link":[{"relation":"self","url":"`+base+`/Subscription/s1/$status"}],"entry":[`+
		`{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[`+
		`{"name":"subscription","valueReference":{"reference":"`+base+`/Subscription/s1"}},`+
		`{"name":"topic","valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"},`+
		`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},`+
		`{"name":"focus","valueReference":{"reference":"`+base+`/Task/t1"}},`+
		`{"name":"additional-context","valueReference":{"reference":"`+base+`/Task/t2"}}]}]},`+
		`"request":{"method":"GET","url":"`+base+`/Subscription/s1/$status"}},`+
		`{"fullUrl":"`+base+`/Task/t1","request":{"method":"PUT","url":"Task/t1"},`+
		`"response":{"status":"200","location":"`+base+`/Task/t1/_history/2"},"link":[{"relation":"alternate","url":"`+base+`/Task/t1"}]}]`+sig+`}`), OriginPeerFrame)
}

// E-17 (cdex-task-notification-address-rekey) is made even where a signature
// covers a CDex Task notification's addresses into the provider's server (the
// CDex design §15.7; the lead, applying it as E-12's re-key): the replace,
// with a string, of the SubscriptionStatus's subscription reference and
// notification-event focus reference, each entry's request.url, fullUrl and
// response.location, and the url of each of the Bundle's links and of each
// entry's links, each disclosed by the signature's carrier; and on an
// unsigned notification, with no carrier. Any other change E-17 makes to
// signed content is refused, as is the same change by an edit that is not
// exempt.
func TestSignedContentNotificationAddressExemption(t *testing.T) {
	signed := notificationBody(`,"signature":{` + signatureElement + `}`)
	unsigned := notificationBody("")
	const rekeyed = `"https://payer-gw.example/cdex/provider/provider/Task/t1"`
	subscriptionRef := []string{"entry", "0", "resource", "parameter", "0", "valueReference", "reference"}
	focusRef := []string{"entry", "0", "resource", "parameter", "2", "part", "1", "valueReference", "reference"}
	requestURL := []string{"entry", "0", "request", "url"}
	fullURL := []string{"entry", "1", "fullUrl"}
	location := []string{"entry", "1", "response", "location"}
	bundleLink := []string{"link", "0", "url"}
	entryLink := []string{"entry", "1", "link", "0", "url"}
	type row struct {
		name   string
		body   Body
		edit   EditID
		path   []string
		value  string
		made   bool
		signed []string
	}
	rows := []row{
		{"the subscription reference", signed, EditCDexTaskNotificationAddressRekey, subscriptionRef, rekeyed, true, []string{"Bundle.signature"}},
		{"the focus reference", signed, EditCDexTaskNotificationAddressRekey, focusRef, rekeyed, true, []string{"Bundle.signature"}},
		{"an entry's request.url", signed, EditCDexTaskNotificationAddressRekey, requestURL, rekeyed, true, []string{"Bundle.signature"}},
		{"an entry's fullUrl", signed, EditCDexTaskNotificationAddressRekey, fullURL, rekeyed, true, []string{"Bundle.signature"}},
		{"an entry's response.location", signed, EditCDexTaskNotificationAddressRekey, location, rekeyed, true, []string{"Bundle.signature"}},
		{"the Bundle's link url", signed, EditCDexTaskNotificationAddressRekey, bundleLink, rekeyed, true, []string{"Bundle.signature"}},
		{"an entry's link url", signed, EditCDexTaskNotificationAddressRekey, entryLink, rekeyed, true, []string{"Bundle.signature"}},
		{"the focus reference, unsigned", unsigned, EditCDexTaskNotificationAddressRekey, focusRef, rekeyed, true, nil},
		// Rejection rows: any other signed value, or a value that is not a
		// string.
		{"the topic", signed, EditCDexTaskNotificationAddressRekey, []string{"entry", "0", "resource", "parameter", "1", "valueCanonical"}, rekeyed, false, nil},
		{"an additional-context reference", signed, EditCDexTaskNotificationAddressRekey,
			[]string{"entry", "0", "resource", "parameter", "2", "part", "2", "valueReference", "reference"}, rekeyed, false, nil},
		{"an event number", signed, EditCDexTaskNotificationAddressRekey, []string{"entry", "0", "resource", "parameter", "2", "part", "0", "valueString"}, rekeyed, false, nil},
		{"an entry's response.status", signed, EditCDexTaskNotificationAddressRekey, []string{"entry", "1", "response", "status"}, `"201"`, false, nil},
		{"the Bundle's link relation", signed, EditCDexTaskNotificationAddressRekey, []string{"link", "0", "relation"}, rekeyed, false, nil},
		{"an entry's link relation", signed, EditCDexTaskNotificationAddressRekey, []string{"entry", "1", "link", "0", "relation"}, rekeyed, false, nil},
		{"an entry's request.method", signed, EditCDexTaskNotificationAddressRekey, []string{"entry", "0", "request", "method"}, rekeyed, false, nil},
		{"the Bundle's type", signed, EditCDexTaskNotificationAddressRekey, []string{"type"}, `"collection"`, false, nil},
		{"the focus reference with an object", signed, EditCDexTaskNotificationAddressRekey, focusRef, `{"x":1}`, false, nil},
		// The same place, by an edit that is not exempt.
		{"the focus reference by E-01", signed, EditCDSCallbackStrip, focusRef, rekeyed, false, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, mustDoc(t, r.body).Replace(at(t, mustDoc(t, r.body), r.path...), []byte(r.value)))
			if r.made {
				if err != nil || !slices.Equal(p.Edits(), []EditID{r.edit}) || !slices.Equal(p.SignedEdited(), r.signed) {
					t.Fatalf("%v %v %v", p.Edits(), p.SignedEdited(), err)
				}
				if out := string(mustTransmit(t, p)); !strings.Contains(out, strings.Trim(r.value, `"`)) {
					t.Fatalf("the address is not re-keyed: %s", out)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != r.edit {
				t.Fatalf("%v, want a SignedContentError naming %s", err, r.edit)
			}
		})
	}
}

// E-17's place is the first entry's SubscriptionStatus alone: the same
// parameter in a later entry's Parameters, or a focus part outside a
// notification-event, is not its place.
func TestSignedContentNotificationAddressPlaceIsTheStatus(t *testing.T) {
	sig := `,"signature":{` + signatureElement + `}`
	for name, body := range map[string]string{
		"a subscription parameter in a later entry": `{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","parameter":[]}},` +
			`{"resource":{"resourceType":"Parameters","parameter":[{"name":"subscription","valueReference":{"reference":"https://p.example/fhir/Subscription/s1"}}]}}]` + sig + `}`,
		"a focus part of another parameter": `{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","parameter":[` +
			`{"name":"other","part":[{"name":"focus","valueReference":{"reference":"https://p.example/fhir/Task/t1"}}]}]}}]` + sig + `}`,
		"a subscription parameter of another resource": `{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Basic","parameter":[` +
			`{"name":"subscription","valueReference":{"reference":"https://p.example/fhir/Subscription/s1"}}]}}]` + sig + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBody([]byte(body), OriginPeerFrame)
			d := mustDoc(t, b)
			var path []string
			if strings.Contains(name, "later entry") {
				path = []string{"entry", "1", "resource", "parameter", "0", "valueReference", "reference"}
			} else if strings.Contains(name, "another parameter") {
				path = []string{"entry", "0", "resource", "parameter", "0", "part", "0", "valueReference", "reference"}
			} else {
				path = []string{"entry", "0", "resource", "parameter", "0", "valueReference", "reference"}
			}
			_, err := Apply(b, fhirJSON, EditCDexTaskNotificationAddressRekey, d.Replace(at(t, d, path...), []byte(`"https://payer-gw.example/x"`)))
			var se *SignedContentError
			if !errors.As(err, &se) {
				t.Fatalf("%v, want a SignedContentError", err)
			}
		})
	}
}

// E-17's link and location places are the root Bundle's and its entries'
// own: a link or a response one level down, in the SubscriptionStatus or in a
// link's own object, is not its place.
func TestSignedContentNotificationLinkPlacesAreTheBundles(t *testing.T) {
	sig := `,"signature":{` + signatureElement + `}`
	const u = `"https://p.example/fhir/Task/t1"`
	for name, row := range map[string]struct {
		body string
		path []string
	}{
		"a link of the SubscriptionStatus": {`{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","link":[{"url":` + u + `}]}}]` + sig + `}`,
			[]string{"entry", "0", "resource", "link", "0", "url"}},
		"a response of the SubscriptionStatus": {`{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters","response":{"location":` + u + `}}}]` + sig + `}`,
			[]string{"entry", "0", "resource", "response", "location"}},
		"a link inside a link": {`{"resourceType":"Bundle","type":"history","link":[{"relation":"self","link":[{"url":` + u + `}]}],"entry":[]` + sig + `}`,
			[]string{"link", "0", "link", "0", "url"}},
		"a link of a Bundle that is no root": {`{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Bundle","link":[{"url":` + u + `}]}}]` + sig + `}`,
			[]string{"entry", "0", "resource", "link", "0", "url"}},
		"a location of an entry's request": {`{"resourceType":"Bundle","type":"history","entry":[{"request":{"method":"GET","location":` + u + `}}]` + sig + `}`,
			[]string{"entry", "0", "request", "location"}},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBody([]byte(row.body), OriginPeerFrame)
			d := mustDoc(t, b)
			_, err := Apply(b, fhirJSON, EditCDexTaskNotificationAddressRekey, d.Replace(at(t, d, row.path...), []byte(`"https://payer-gw.example/x"`)))
			var se *SignedContentError
			if !errors.As(err, &se) {
				t.Fatalf("%v, want a SignedContentError", err)
			}
		})
	}
}
