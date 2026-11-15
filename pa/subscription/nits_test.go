package subscription

import (
	"strings"
	"testing"
)

// E-19 is not made on sealed values that are not sealed values, nor for an
// address of another gateway, and the error says so; zero Keys say no keys.
// The rows the engine refuses 403 on, at the library's level.
func TestRekeyForNotifier_RefusesWhatIsNotSealed(t *testing.T) {
	sub, not := edgeKeys(t)
	sealedOnly, se, sh, err := SealOutbound(sub, binding, mustSubscription(t, baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	s := mustSubscription(t, string(sealedOnly))
	a := Address{Ingress: notifierIngress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: se}
	const want = "not a sealed value, or the address is not one of this gateway"
	for name, c := range map[string]struct {
		a  Address
		sh string
	}{
		"a sealed endpoint with no key id":    {Address{a.Ingress, a.Topic, a.Subscriber, strings.SplitN(se, ".", 2)[1]}, sh},
		"a sealed endpoint of no base64url":   {Address{a.Ingress, a.Topic, a.Subscriber, strings.SplitN(se, ".", 2)[0] + ".!!!"}, sh},
		"a sealed endpoint of only its IV":    {Address{a.Ingress, a.Topic, a.Subscriber, strings.SplitN(se, ".", 2)[0] + ".AAAAAAAAAAAAAAAAAAAAAA"}, sh},
		"an empty sealed endpoint":            {Address{a.Ingress, a.Topic, a.Subscriber, ""}, sh},
		"sealed headers with no key id":       {a, strings.SplitN(sh, ".", 2)[1]},
		"sealed headers shorter than the tag": {a, strings.SplitN(sh, ".", 2)[0] + ".AAAA"},
		"empty sealed headers":                {a, ""},
		"a topic that does not read back":     {Address{a.Ingress, "", a.Subscriber, a.SealedEndpoint}, sh},
		"an ingress with a query":             {Address{a.Ingress + "?x=1", a.Topic, a.Subscriber, a.SealedEndpoint}, sh},
	} {
		out, err := RekeyForNotifier(not, c.a, c.sh, s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, %s; want an error saying %q", name, err, out, want)
		}
	}
	if _, err := RekeyForNotifier(Keys{}, a, sh, s); err == nil || !strings.Contains(err.Error(), "no keys") {
		t.Errorf("zero Keys: %v, want no keys", err)
	}
}

// SealOutbound names the size limit for an endpoint too long to seal, and
// says no keys only for zero Keys.
func TestSealOutbound_NamesTheSizeLimit(t *testing.T) {
	sub, _ := edgeKeys(t)
	long := edit(t, baseSubscription, subEndpoint, "https://ehr.example/"+strings.Repeat("a", maxSealedLen))
	if _, _, _, err := SealOutbound(sub, binding, mustSubscription(t, long)); err == nil || !strings.Contains(err.Error(), "too long to seal") || strings.Contains(err.Error(), "no keys") {
		t.Fatalf("a too-long endpoint: %v, want the size limit named", err)
	}
	if _, _, _, err := SealOutbound(Keys{}, binding, mustSubscription(t, baseSubscription)); err == nil || !strings.Contains(err.Error(), "no keys") {
		t.Fatalf("zero Keys: %v, want no keys", err)
	}
}

// Restore reads a Bundle's member names folded, as a lenient reader would
// take them: an "Entry" and a "Resource" are restored, never passed over with
// nothing recorded, and an entry that is not an object is recorded unreadable.
func TestRestore_ReadsMemberNamesFolded(t *testing.T) {
	sub, not := edgeKeys(t)
	own, _, _ := throughBoth(t, sub, not, binding, baseSubscription)
	const base, counterpart = "http://payer.example/fhir", "https://provider-gw.example/pas/payer/payer-1"
	for name, bundle := range map[string]string{
		"Entry":    `{"resourceType":"Bundle","type":"searchset","Entry":[{"fullUrl":"http://payer.example/fhir/Subscription/sub-1","resource":` + string(own) + `}]}`,
		"Resource": `{"resourceType":"Bundle","type":"searchset","entry":[{"FullUrl":"http://payer.example/fhir/Subscription/sub-1","Resource":` + string(own) + `}]}`,
		"Link":     `{"resourceType":"Bundle","type":"searchset","Link":[{"relation":"self","URL":"http://payer.example/fhir/Subscription?url=x"}],"entry":[{"fullUrl":"http://payer.example/fhir/Subscription/sub-1","resource":` + string(own) + `}]}`,
	} {
		out, made, notMade, err := Restore(sub, binding, base, counterpart, []byte(bundle))
		if err != nil || len(notMade) != 0 {
			t.Fatalf("%s: %v, not made %v", name, err, notMade)
		}
		if made < 2 || strings.Contains(string(out), "X-SHN-Channel") || !strings.Contains(string(out), counterpart) {
			t.Fatalf("%s: made %d\n%s\nwant the Subscription restored and its fullUrl re-keyed", name, made, out)
		}
	}
	_, _, notMade, err := Restore(sub, binding, base, counterpart, []byte(`{"resourceType":"Bundle","type":"searchset","entry":["x"]}`))
	if err != nil || len(notMade) != 1 || notMade[0].Reason != NotMadeUnreadable {
		t.Fatalf("an entry that is not an object: %v, not made %v; want unreadable", err, notMade)
	}
}
