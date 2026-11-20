package subscription

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// Resources reads every resource a notification carries, wherever a reader
// could take one: the entry's resource, under any case of its name; a
// resource array's elements; a SubscriptionStatus parameter's resource; an
// entry's response.outcome; a contained resource; a nested Bundle's entries.
// Each is its exact bytes, with whether it is an entry's resource (the
// notification's focus) and the outermost resource it sits in. The entry's
// own members beside the resource (fullUrl, request, response) are read as
// they are. A resourceType that is not a string with a value cannot be read:
// 403 on authority.
func TestNotificationResources(t *testing.T) {
	cr := func(id string) string { return `{"resourceType":"ClaimResponse","id":"` + id + `"}` }
	type want struct {
		typ   string
		entry bool
		outer int
	}
	status := statusEntry("event-notification", testCanonical)
	for name, row := range map[string]struct {
		body string
		want []want
	}{
		"a decision, with fullUrl, request and response": {notification(status, `{"fullUrl":"http://payer.example/fhir/Bundle/d1","request":{"method":"PUT","url":"Bundle/d1"},"response":{"status":"200"},"resource":{"resourceType":"Bundle","id":"d1","entry":[{"resource":`+cr("cr1")+`}]}}`),
			[]want{{"Bundle", true, 0}, {"ClaimResponse", false, 0}}},
		"an entry's resource in another case":   {notification(status, `{"Resource":`+cr("cr1")+`}`), []want{{"ClaimResponse", true, 0}}},
		"a resource array":                      {notification(status, `{"resource":[`+cr("cr1")+`,`+cr("cr2")+`]}`), []want{{"ClaimResponse", false, 0}, {"ClaimResponse", false, 1}}},
		"a SubscriptionStatus parameter's":      {notification(strings.Replace(status, `{"name":"status","valueCode":"active"},`, `{"name":"status","valueCode":"active"},{"name":"x","resource":`+cr("cr1")+`},`, 1)), []want{{"ClaimResponse", false, 0}}},
		"an entry's response.outcome":           {notification(status, `{"request":{"method":"PUT"},"response":{"status":"200","outcome":`+cr("cr1")+`}}`), []want{{"ClaimResponse", false, 0}}},
		"contained, its member in another case": {notification(status, `{"resource":{"resourceType":"ClaimResponse","id":"cr1","Contained":[`+cr("cr2")+`]}}`), []want{{"ClaimResponse", true, 0}, {"ClaimResponse", false, 0}}},
		"a nested Bundle's entry":               {notification(status, `{"resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Bundle","entry":[{"resource":`+cr("cr1")+`}]}}]}}`), []want{{"Bundle", true, 0}, {"Bundle", false, 0}, {"ClaimResponse", false, 0}}},
		"a resourceType member in another case": {notification(status, `{"resource":{"ResourceType":"ClaimResponse","id":"cr1"}}`), []want{{"ClaimResponse", true, 0}}},
		"an id-only event":                      {notification(status, idOnlyEntry), nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, rf := mustNotification(t, row.body, testTopic{}).Resources()
			if rf != nil {
				t.Fatalf("refused: %+v", rf)
			}
			if len(got) != len(row.want) {
				t.Fatalf("read %d resources %+v, want %d", len(got), got, len(row.want))
			}
			for i, w := range row.want {
				g := got[i]
				if g.Type != w.typ || g.Entry != w.entry || g.Outer != w.outer || !strings.Contains(row.body, string(g.Raw)) || g.Raw[0] != '{' {
					t.Fatalf("resource %d: %s entry=%v outer=%d %s; want %s entry=%v outer=%d", i, g.Type, g.Entry, g.Outer, g.Raw, w.typ, w.entry, w.outer)
				}
			}
		})
	}
	for name, body := range map[string]string{
		"a resourceType that is an object": notification(status, `{"resource":{"resourceType":{"x":"ClaimResponse"}}}`),
		"a resourceType that is empty":     notification(status, `{"request":{"method":"PUT"},"response":{"outcome":{"resourceType":""}}}`),
		"a resourceType that is a number":  notification(strings.Replace(status, `{"name":"status","valueCode":"active"},`, `{"name":"x","resource":{"resourceType":1}},`, 1)),
	} {
		_, rf := mustNotification(t, body, testTopic{}).Resources()
		if rf == nil || rf.Status != http.StatusForbidden || rf.Primitive != recording.RefusalAuthority {
			t.Errorf("%s: %+v, want 403 on authority", name, rf)
		}
	}
	// A type is compared under the folding member names are read by.
	if !(Resource{Type: "claimresponse"}).TypeIs("ClaimResponse") || (Resource{Type: "ClaimResponses"}).TypeIs("ClaimResponse") {
		t.Error("TypeIs does not fold as member names are folded")
	}
}
