package subscription

import (
	"testing"
)

const testSealed = "ocurkBVBmv4.NDl-amuZ2jPykJWQ0VqMeEyx0N0SE76GGCiX2s1iW-Y8"

// An address is written as E-19 writes it and read back as it was written,
// the subscriber's id escaped and unescaped as a path segment.
func TestAddress_RoundTrip(t *testing.T) {
	for _, a := range []Address{
		{Ingress: notifierIngress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: testSealed},
		{Ingress: notifierIngress, Topic: "cdex-task", Subscriber: "a/b c,d", SealedEndpoint: testSealed},
		{Ingress: "https://payer-gw.example", Topic: "pas", Subscriber: "p", SealedEndpoint: testSealed},
	} {
		got, ok := ParseAddress(a.String(), a.Ingress)
		if !ok || got != a {
			t.Fatalf("ParseAddress(%q) = %+v, %v; want %+v", a.String(), got, ok, a)
		}
	}
	a := Address{Ingress: notifierIngress + "/", Topic: "test", Subscriber: "provider-1", SealedEndpoint: testSealed}
	if want := notifierIngress + "/notify/test/provider-1/ocurkBVBmv4/NDl-amuZ2jPykJWQ0VqMeEyx0N0SE76GGCiX2s1iW-Y8"; a.String() != want {
		t.Fatalf("String = %q, want %q", a.String(), want)
	}
	if got, ok := ParseAddress(a.String(), a.Ingress); !ok || got.SealedEndpoint != testSealed {
		t.Fatalf("an ingress with an ending slash: %+v, %v", got, ok)
	}
}

// Only a notification address of this gateway's ingress, in exactly the
// address's shape, is read as one.
func TestParseAddress_Rejects(t *testing.T) {
	good := Address{Ingress: notifierIngress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: testSealed}.String()
	if _, ok := ParseAddress(good, notifierIngress); !ok {
		t.Fatal("control: the address is not read")
	}
	const rest = "/notify/test/provider-1/ocurkBVBmv4/NDl-amuZ2jPykJWQ0VqMeEyx0N0SE76GGCiX2s1iW-Y8"
	for name, raw := range map[string]string{
		"another host":              "https://other-gw.example/ingress" + rest,
		"another port":              "https://payer-gw.example:8443/ingress" + rest,
		"another scheme":            "http://payer-gw.example/ingress" + rest,
		"another ingress path":      "https://payer-gw.example/ingress2" + rest,
		"no ingress path":           "https://payer-gw.example" + rest,
		"a query":                   good + "?x=1",
		"an empty query":            good + "?",
		"a fragment":                good + "#f",
		"userinfo":                  "https://u@payer-gw.example/ingress" + rest,
		"a segment more":            good + "/x",
		"an ending slash":           good + "/",
		"a segment less":            notifierIngress + "/notify/test/provider-1/ocurkBVBmv4",
		"not notify":                notifierIngress + "/notifx/test/provider-1/ocurkBVBmv4/NDl",
		"an empty topic":            notifierIngress + "/notify//provider-1/ocurkBVBmv4/NDl",
		"an empty subscriber":       notifierIngress + "/notify/test//ocurkBVBmv4/NDl",
		"a short key id":            notifierIngress + "/notify/test/provider-1/ocurkBVBmv/NDl",
		"a key id off the alphabet": notifierIngress + "/notify/test/provider-1/ocurkBVBm.4/NDl",
		"an empty sealed segment":   notifierIngress + "/notify/test/provider-1/ocurkBVBmv4/",
		"a sealed segment with '='": notifierIngress + "/notify/test/provider-1/ocurkBVBmv4/NDl=",
		"a bad escape":              notifierIngress + "/notify/test/provider%zz/ocurkBVBmv4/NDl",
		"relative":                  rest,
		"not a URL":                 "::",
	} {
		if a, ok := ParseAddress(raw, notifierIngress); ok {
			t.Errorf("%s: read %q as %+v", name, raw, a)
		}
	}
}

// The loop rule: the four segments in order, whatever the host and
// whatever comes before or after them, read unescaped.
func TestLooksLikeNotificationAddress(t *testing.T) {
	isParticipant := func(s string) bool { return s == "provider-1" || s == "payer-1" || s == "p/1" }
	isTopic := func(s string) bool { return s == "test" || s == "cdex-task" }
	for raw, want := range map[string]bool{
		notifierIngress + "/notify/test/provider-1/ocurkBVBmv4/NDl":               true,
		"https://anywhere.example/notify/test/provider-1/ocurkBVBmv4":             true,
		"https://anywhere.example/a/b/notify/cdex-task/payer-1/ocurkBVBmv4/x?y=1": true,
		"https://anywhere.example/%6Eotify/test/provider-1/ocurkBVBmv4":           true,
		"https://anywhere.example/notify/test/provider%2D1/ocurkBVBmv4":           true,
		"https://anywhere.example/notify/test/stranger/ocurkBVBmv4":               false,
		"https://anywhere.example/notify/other/provider-1/ocurkBVBmv4":            false,
		"https://anywhere.example/notify/test/provider-1/short":                   false,
		"https://anywhere.example/notify%2Ftest%2Fprovider-1%2FocurkBVBmv4%2FNDl": true,
		"https://anywhere.example/x/notify%2ftest/provider-1/ocurkBVBmv4":         true,
		"https://anywhere.example/notify%2Ftest%2Fstranger%2FocurkBVBmv4":         false,
		// A participant id holding a slash is one segment only when each
		// segment is unescaped on its own.
		"https://anywhere.example/notify/test/p%2F1/ocurkBVBmv4":      true,
		"https://anywhere.example/notify/provider-1/test/ocurkBVBmv4": false,
		"https://anywhere.example/notify/test/provider-1":             false,
		"https://ehr.example/api/pas/notification":                    false,
		"::": false,
		"https://anywhere.example/notify/test/provider-1%zz/ocurkBVBmv4": false,
	} {
		if got := LooksLikeNotificationAddress(raw, isParticipant, isTopic); got != want {
			t.Errorf("LooksLikeNotificationAddress(%q) = %v, want %v", raw, got, want)
		}
	}
}
