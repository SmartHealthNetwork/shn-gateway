package subscription

import (
	"strings"
	"testing"
)

// CheckShape pins the subscription reference's type, a uri, to Subscription:
// another type, or one that is no string, is refused, and none, or
// Subscription by name or by its canonical, passes.
func TestNotificationCheckShapePinsTheSubscriptionType(t *testing.T) {
	event := `{"resourceType":"Bundle","type":"history","entry":[{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},` +
		`{"name":"topic","valueCanonical":"` + testCanonical + `"},` +
		`{"name":"type","valueCode":"event-notification"},` +
		`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus","valueReference":{"reference":"Task/t1"}}]}]}},` +
		`{"fullUrl":"https://provider.example/fhir/Task/t1","request":{"method":"PUT","url":"Task/t1"}}]}`
	for name, row := range map[string]struct {
		typ  string
		pass bool
	}{
		"none":                           {"", true},
		"Subscription":                   {`,"type":"Subscription"`, true},
		"Subscription's canonical":       {`,"type":"http://hl7.org/fhir/StructureDefinition/Subscription"`, true},
		"another type":                   {`,"type":"Patient"`, false},
		"a uri on the provider's server": {`,"type":"https://provider.example/fhir/StructureDefinition/x"`, false},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(event, `{"reference":"Subscription/s1"}`, `{"reference":"Subscription/s1"`+row.typ+`}`, 1)
			n, rf := ParseNotification([]byte(body), testTopic{})
			if rf != nil {
				t.Fatalf("not a notification: %s", rf.Reason)
			}
			if r := n.CheckShape(NotificationShape{Topic: testCanonical, Focus: []string{"Task"}}, "https://provider.example/fhir"); (r == nil) != row.pass {
				t.Fatalf("CheckShape %v, want pass %v", r, row.pass)
			}
		})
	}
}

// CheckShape judges a notification's addresses by path segment, below the
// provider's stated FHIR base: a Task's id or the base's own path naming a
// resource type's name names nothing, while a segment that is one, or a
// query naming one, is refused; with no stated base, every segment of an
// address is judged. The message is the notification's own, never a $status
// answer's.
func TestNotificationCheckShapeJudgesAddressesBySegment(t *testing.T) {
	const base = "https://provider.example/Organization/9/fhir"
	event := func(fullUrl string) string {
		return `{"resourceType":"Bundle","type":"history","entry":[{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[` +
			`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},` +
			`{"name":"topic","valueCanonical":"` + testCanonical + `"},{"name":"type","valueCode":"event-notification"},` +
			`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus","valueReference":{"reference":"Task/t1"}}]}]}},` +
			`{"fullUrl":"` + fullUrl + `","request":{"method":"PUT","url":"Task/t1"}}]}`
	}
	shape := NotificationShape{Topic: testCanonical, Focus: []string{"Task"}}
	for name, row := range map[string]struct {
		fullUrl, base string
		pass          bool
	}{
		"a Task whose id names a type":           {base + "/Task/Patient-1", base, true},
		"a base whose path names a type":         {base + "/Task/t1", base, true},
		"a relative Task whose id names a type":  {"Task/Claim-123", base, true},
		"a Patient segment below the base":       {base + "/Patient/p1", base, false},
		"an escaped Patient segment":             {base + "/%50atient/p1", base, false},
		"a query naming a Patient":               {base + "/Task?subject=Patient/p1", base, false},
		"another server's Patient":               {"https://other.example/fhir/Patient/p1", base, false},
		"no stated base, the base's path judged": {base + "/Task/t1", "", false},
		"a Patient behind an escaped slash":      {base + "/Patient%2Fp1", base, false},
		"a Patient behind a backslash":           {base + "/Patient\\\\p1", base, false},
		"a Patient behind a path parameter":      {base + "/Patient;x/p1", base, false},
		"another server's Patient, escaped":      {"https://other.example/fhir/Patient%2Fp1", base, false},
		"a Task id with a dot":                   {base + "/Task/Patient.1", base, true},
	} {
		t.Run(name, func(t *testing.T) {
			n, rf := ParseNotification([]byte(event(row.fullUrl)), testTopic{})
			if rf != nil {
				t.Fatalf("not a notification: %s", rf.Reason)
			}
			r := n.CheckShape(shape, row.base)
			if (r == nil) != row.pass {
				t.Fatalf("CheckShape %v, want pass %v", r, row.pass)
			}
			if r != nil && (strings.Contains(r.Reason, "Subscription interaction") || !strings.HasPrefix(r.Reason, "a notification")) {
				t.Fatalf("the reason is not the notification's own: %s", r.Reason)
			}
		})
	}
}
