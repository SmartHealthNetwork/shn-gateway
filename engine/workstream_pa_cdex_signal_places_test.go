//go:build !shn_coreonly

package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// unreadableAddress is a value a lenient URL reader takes for an http or
// https address, or a network-path reference, that is not an absolute http
// or https URL with a host written plainly. Every other value is carried by
// E-17's rule: re-keyed when under the stated base, otherwise as sent.
func TestUnreadableAddress(t *testing.T) {
	for v, want := range map[string]bool{
		signalProviderBase + "/Task/t1":                 false,
		"HTTPS://PROVIDER.EXAMPLE/fhir/Task/t1":         false,
		"http://provider.example:8080/fhir/Task/t1?x=1": false,
		"https://elsewhere.example/fhir/Task/t1":        false,
		"Task/t1":                                       false,
		"/fhir/Task/t1":                                 false,
		"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b": false,
		"":                                          false,
		"Task/t1 ":                                  false,
		"https:provider.example/fhir/Task/t1":       true,
		"HTTPS:provider.example/fhir/Task/t1":       true,
		"https:/provider.example/fhir/Task/t1":      true,
		"https:///provider.example/fhir/Task/t1":    true,
		`https:\\provider.example\fhir\Task\t1`:     true,
		`https://provider.example\fhir\Task\t1`:     true,
		" " + signalProviderBase + "/Task/t1":       true,
		signalProviderBase + "/Task/t1\n":           true,
		"\x00" + signalProviderBase + "/Task/t1":    true,
		"ht\ttps://provider.example/fhir/Task/t1":   true,
		"https://provider.example/fhir/Ta\rsk/t1":   true,
		"//provider.example/fhir/Task/t1":           true,
		`\\provider.example\fhir\Task\t1`:           true,
		"https://provider.example/fhir/Task/t1\x7f": true,
		"https://":                                    true,
		"http://127.0.0.1:61034/fhir/Task/t1":         false,
		"http://[2001:db8::1]:8080/fhir/Task/t1":      false,
		"https://provider_x.example/fhir/Task/t1":     false,
		"http://u@provider.example/fhir/Task/t1":      true,
		"http://:@provider.example/fhir/Task/t1":      true,
		"http://provider.example./fhir/Task/t1":       true,
		"http://provider..example/fhir/Task/t1":       true,
		"http://provider.example:080/fhir/Task/t1":    true,
		"http://provider.example:/fhir/Task/t1":       true,
		"http://provider.example:123456/fhir/Task/t1": true,
		"http://\uff50rovider.example/fhir/Task/t1":   true,
		"http://%70rovider.example/fhir/Task/t1":      true,
		"http://2130706433/fhir/Task/t1":              true,
		"http://0x7f.0.0.1/fhir/Task/t1":              true,
		"http://127.000.0.1/fhir/Task/t1":             true,
		"http://[2001:DB8:0:0::1]/fhir/Task/t1":       true,
		"http://[2001:db8::1%25eth0]/fhir/Task/t1":    true,
		"http://[::ffff:127.0.0.1]/fhir/Task/t1":      true,
		"http://[::ffff:7f00:1]/fhir/Task/t1":         true,
		"http://provider.example:65536/fhir/Task/t1":  true,
		"http://provider.example:65535/fhir/Task/t1":  false,
		"http:": true,
	} {
		if got := unreadableAddress(v); got != want {
			t.Errorf("unreadableAddress(%q) = %v, want %v", v, got, want)
		}
	}
}

// E-17's places include the Bundle's links, each entry's response.location
// and each entry's links: each address there under the stated base is
// re-keyed like the others, its relation and status carried.
func TestCDexSignal_E17RekeysLinksAndLocations(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	body := strings.Replace(event, `"type":"history",`, `"type":"history","link":[{"relation":"self","url":"`+signalProviderBase+`/Subscription/s1/$status"}],`, 1)
	body = strings.Replace(body, `{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1",`+
		`"response":{"status":"200","location":"`+signalProviderBase+`/Task/t1/_history/2"},"link":[{"relation":"alternate","url":"`+signalProviderBase+`/Task/t1"}],`, 1)
	if strings.Count(body, `"link"`) != 2 {
		t.Fatal("the links are not in the body")
	}
	got, edits, notMade := delivered(t, signalPayerIngress, signalProviderBase, body)
	if want := strings.ReplaceAll(body, signalProviderBase, signalPayerBase); got != want {
		t.Fatalf("delivered\n%s\nwant\n%s", got, want)
	}
	if !slices.Equal(edits, []relay.EditID{relay.EditCDexTaskNotificationAddressRekey}) || len(notMade) != 0 {
		t.Fatalf("edits %v, not made %v", edits, notMade)
	}
}

// Rejection rows: a link or location one level down, in the
// SubscriptionStatus, is no place of E-17's, and is carried as sent.
func TestCDexSignal_E17LinkPlacesAreTheBundles(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	nested := signalProviderBase + "/Task/t9"
	body := strings.Replace(event, `"resource":{"resourceType":"Parameters",`, `"resource":{"resourceType":"Parameters","link":[{"url":"`+nested+`"}],"response":{"location":"`+nested+`"},`, 1)
	got, _, _ := delivered(t, signalPayerIngress, signalProviderBase, body)
	if strings.Count(got, `"`+nested+`"`) != 2 {
		t.Fatalf("a nested link or location was re-keyed: %s", got)
	}
}

// A notification E-17 cannot read one way only is an edit the payer's
// gateway cannot make: the delivery fails, nothing is delivered, and no
// not-made reason is recorded for it (the handler refuses it on fidelity).
func TestCDexSignal_E17RefusesWhatItCannotRead(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	for name, body := range map[string]string{
		"a member repeated":                    strings.Replace(event, `"type":"history"`, `"type":"history","type":"batch"`, 1),
		"a member name repeated under folding": strings.Replace(event, `{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+signalProviderBase+`/Task/t1","FULLURL":"x",`, 1),
		"not JSON":                             "notify",
		"a lone surrogate":                     strings.Replace(event, `"type":"history",`, `"type":"history","x":"\ud800",`, 1),
		"nesting deeper than relay reads":      strings.Replace(event, `"type":"history",`, `"type":"history","x":`+strings.Repeat(`{"a":`, 70)+`0`+strings.Repeat(`}`, 70)+`,`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if body == event {
				t.Fatal("unchanged")
			}
			g := &Gateway{cfg: Config{IngressBaseURL: signalPayerIngress}}
			x := &exchangeRecorder{}
			_, err := g.deliveredNotification(x, legCDexTaskSignal, []byte(body), "application/fhir+json",
				deliveryInput{notifier: "provider", statedBase: signalProviderBase, hasStatedBase: true})
			if err == nil {
				t.Fatal("delivered a notification E-17 cannot read")
			}
			if len(x.rec.EditsNotMade) != 0 {
				t.Fatalf("not made %v, want none", x.rec.EditsNotMade)
			}
			if got := readNotification([]byte(body), relay.OriginPeerFrame, signalProviderBase); got != notificationUnreadable {
				t.Fatalf("readNotification %v, want unreadable", got)
			}
		})
	}
}

// An address into the provider's own server, at one of E-17's places, that
// E-17 cannot write at the payer gateway's base is refused, never carried:
// readNotification reads it against the stated base by its parsed authority
// (scheme, host without case, port with the scheme's default), never by a
// string prefix. Another server's address is carried and recorded; with no
// stated base, none is judged.
func TestReadNotificationJudgesTheProvidersOwnAddresses(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	at := func(addr string) string {
		return strings.Replace(event, `{"fullUrl":"`+signalProviderBase+`/Task/t1",`, `{"fullUrl":"`+addr+`",`, 1)
	}
	for name, row := range map[string]struct {
		body, base string
		want       notificationRead
	}{
		"under the base":                   {event, signalProviderBase, notificationPlacesRead},
		"under the base, host in capitals": {at("https://PROVIDER.EXAMPLE/fhir/Task/t1"), signalProviderBase, notificationPlacesRead},
		"under the base, default port":     {at("https://provider.example:443/fhir/Task/t1"), signalProviderBase, notificationPlacesRead},
		"another host":                     {at("https://other.example/fhir/Task/t1"), signalProviderBase, notificationPlacesRead},
		"another port":                     {at("https://provider.example:8443/fhir/Task/t1"), signalProviderBase, notificationPlacesRead},
		"another scheme":                   {at("http://provider.example/fhir/Task/t1"), signalProviderBase, notificationPlacesRead},
		"a sibling path":                   {at("https://provider.example/fhirX/Task/t1"), signalProviderBase, notificationAddressNotRekeyable},
		"a path outside the base":          {at("https://provider.example/other/Task/t1"), signalProviderBase, notificationAddressNotRekeyable},
		"a fragment":                       {at(signalProviderBase + "/Task/t1#x"), signalProviderBase, notificationAddressNotRekeyable},
		"a dot segment":                    {at(fmt.Sprintf("%s/../other/Task/t1", signalProviderBase)), signalProviderBase, notificationAddressNotRekeyable},
		"an escaped base path":             {at("https://provider.example/%66hir/Task/t1"), signalProviderBase, notificationAddressNotRekeyable},
		"the default port, outside":        {at("https://provider.example:443/other/Task/t1"), signalProviderBase, notificationAddressNotRekeyable},
		"no stated base":                   {at("https://provider.example/other/Task/t1"), "", notificationPlacesRead},
		"unreadable before not re-keyable": {at(" https://provider.example/other/Task/t1"), signalProviderBase, notificationAddressUnreadable},
	} {
		t.Run(name, func(t *testing.T) {
			if got := readNotification([]byte(row.body), relay.OriginPeerFrame, row.base); got != row.want {
				t.Fatalf("readNotification %v, want %v", got, row.want)
			}
		})
	}
}

// withIdentities is event with a uri at each identity member the closed
// shape admits (the subscription package's identity registry): the Bundle's and
// the SubscriptionStatus's implicitRules, meta.source (one written as the
// provider's base with a request id after #) and meta.tag or meta.security
// system, the Bundle's meta.profile (the Backport's canonical), and an error
// parameter's coding system, each naming the provider's server.
func withIdentities(event, base string) string {
	s := strings.Replace(event, `"type":"history",`, `"type":"history","implicitRules":"`+base+`/rules","meta":{"source":"`+base+`#req-1",`+
		`"profile":["http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-notification-r4"],`+
		`"tag":[{"system":"`+base+`/CodeSystem/t","code":"x"}]},`, 1)
	s = strings.Replace(s, `"resource":{"resourceType":"Parameters",`, `"resource":{"resourceType":"Parameters","implicitRules":"https://provider.example/ig/rules",`+
		`"meta":{"source":"`+base+`/Parameters","security":[{"system":"`+base+`/CodeSystem/s","code":"y"}]},`, 1)
	return strings.Replace(s, `{"name":"notification-event",`, `{"name":"error","valueCodeableConcept":{"coding":[{"system":"`+base+`/CodeSystem/e","code":"z"}]}},{"name":"notification-event",`, 1)
}

// An identity member names rather than locates, so it is carried as sent:
// never re-keyed (that would change what it asserts), never refused (one on
// the provider's server outside its base, or with a request id after #,
// included), and recorded nowhere, while each locator beside it is re-keyed.
func TestCDexSignal_E17CarriesIdentityMembersAsSent(t *testing.T) {
	event := signalEvent(signalProviderBase + "/Task/t1")
	body := withIdentities(event, signalProviderBase)
	if strings.Count(body, "provider.example") < 11 {
		t.Fatal("the identity members are not in the body")
	}
	got, edits, notMade := delivered(t, signalPayerIngress, signalProviderBase, body)
	want := withIdentities(strings.ReplaceAll(event, signalProviderBase, signalPayerBase), signalProviderBase)
	if got != want {
		t.Fatalf("delivered\n%s\nwant\n%s", got, want)
	}
	if !slices.Equal(edits, []relay.EditID{relay.EditCDexTaskNotificationAddressRekey}) || len(notMade) != 0 {
		t.Fatalf("edits %v, not made %v, want E-17 and nothing recorded", edits, notMade)
	}
	if got := readNotification([]byte(body), relay.OriginPeerFrame, signalProviderBase); got != notificationPlacesRead {
		t.Fatalf("readNotification %v, want read", got)
	}
}
