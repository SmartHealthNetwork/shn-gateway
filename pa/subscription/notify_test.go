package subscription

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// statusEntry is a SubscriptionStatus entry as the capture recorded br-payer
// writing it (the notifications design, §3 "Captured"): a relative fullUrl
// (Parameters/<uuid>), subscription Subscription/{id}, the topic, the type,
// and a focus part with no value.
func statusEntry(typ, topicValue string) string {
	return `{"fullUrl":"Parameters/6f1c2a7e-0d3b-4c1f-9a51-2b7e8d4c0a19","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/sub-1"}},` +
		`{"name":"topic","valueCanonical":"` + topicValue + `"},` +
		`{"name":"status","valueCode":"active"},` +
		`{"name":"type","valueCode":"` + typ + `"},` +
		`{"name":"events-since-subscription-start","valueString":"1"},` +
		`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus"}]}]}}`
}

// notification is a history Bundle, as br-payer sends one, with the status
// entry and the entries after it.
func notification(status string, rest ...string) string {
	return `{"resourceType":"Bundle","type":"history","entry":[` + strings.Join(append([]string{status}, rest...), ",") + `]}`
}

// The second entries the capture recorded: a full-resource event's carries
// the PAS Response Bundle under request.method PUT with no request.url and no
// fullUrl; an id-only event's is request.method PUT alone.
const (
	fullResourceEntry = `{"request":{"method":"PUT"},"resource":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"http://payer.example/fhir/ClaimResponse/cr-1","resource":{"resourceType":"ClaimResponse","id":"cr-1"}}]}}`
	idOnlyEntry       = `{"request":{"method":"PUT"}}`
)

func mustNotification(t *testing.T, body string, tp Topic) *Notification {
	t.Helper()
	n, r := ParseNotification([]byte(body), tp)
	if r != nil {
		t.Fatalf("ParseNotification: %+v\n%s", r, body)
	}
	return n
}

// The route's structural minimum accepts br-payer's own shape, which is not a
// valid R4 Bundle, and the canonical with or without |version; it keeps the
// bytes as received.
func TestParseNotification_Accepts(t *testing.T) {
	for name, body := range map[string]string{
		"br-payer's full-resource event": notification(statusEntry("event-notification", testCanonical), fullResourceEntry),
		"br-payer's id-only event":       notification(statusEntry("event-notification", testCanonical), idOnlyEntry),
		"a handshake":                    notification(statusEntry("handshake", testCanonical)),
		"the canonical with |version":    notification(statusEntry("heartbeat", testCanonical+"|1.0.0")),
		"an event-notification Bundle":   strings.Replace(notification(statusEntry("heartbeat", testCanonical)), `"history"`, `"subscription-notification"`, 1),
	} {
		n := mustNotification(t, body, topic)
		if string(n.Body()) != body {
			t.Errorf("%s: Body is not the bytes as received", name)
		}
	}
}

// Anything else is refused 400, routing.
func TestParseNotification_Refuses(t *testing.T) {
	for name, body := range map[string]string{
		"the wrong topic":                                 notification(statusEntry("event-notification", "http://example.org/SubscriptionTopic/other"), idOnlyEntry),
		"a topic that is a prefix":                        notification(statusEntry("event-notification", testCanonical+"2")),
		"an empty version":                                notification(statusEntry("event-notification", testCanonical+"|")),
		"not a Bundle":                                    `{"resourceType":"Parameters","parameter":[]}`,
		"a ClaimResponse":                                 `{"resourceType":"ClaimResponse","id":"cr-1"}`,
		"a first entry not Parameters":                    notification(fullResourceEntry, statusEntry("event-notification", testCanonical)),
		"a first entry with no resource":                  notification(idOnlyEntry, statusEntry("event-notification", testCanonical)),
		"a first entry another resource naming the topic": notification(strings.Replace(statusEntry("handshake", testCanonical), `"resourceType":"Parameters"`, `"resourceType":"Basic"`, 1)),
		"another resource holding the entries":            strings.Replace(notification(statusEntry("handshake", testCanonical)), `"resourceType":"Bundle"`, `"resourceType":"Basic"`, 1),
		"no entry":                                        `{"resourceType":"Bundle","type":"history","entry":[]}`,
		"no entry member":                                 `{"resourceType":"Bundle","type":"history"}`,
		"no topic":                                        notification(strings.Replace(statusEntry("handshake", testCanonical), `{"name":"topic","valueCanonical":"`+testCanonical+`"},`, ``, 1)),
		"two topics":                                      notification(strings.Replace(statusEntry("handshake", testCanonical), `"parameter":[`, `"parameter":[{"name":"topic","valueCanonical":"`+testCanonical+`"},`, 1)),
		"a topic as another type":                         notification(strings.Replace(statusEntry("handshake", testCanonical), `"valueCanonical"`, `"valueUri"`, 1)),
		"not JSON":                                        `<Bundle/>`,
		"a repeated member":                               `{"resourceType":"Bundle","resourceType":"Bundle"}`,
		"empty":                                           ``,
	} {
		_, r := ParseNotification([]byte(body), topic)
		wantRefusal(t, name, r, http.StatusBadRequest, recording.RefusalRouting)
	}
}

// §5's no-content test, by structure, and the leg it picks.
func TestNoContentAndLeg(t *testing.T) {
	status := statusEntry("event-notification", testCanonical)
	for name, row := range map[string]struct {
		body      string
		noContent bool
	}{
		"br-payer's full-resource event":         {notification(status, fullResourceEntry), false},
		"br-payer's id-only event":               {notification(status, idOnlyEntry), true},
		"a handshake":                            {notification(statusEntry("handshake", testCanonical)), true},
		"a heartbeat":                            {notification(statusEntry("heartbeat", testCanonical)), true},
		"an id-only event naming its focus":      {notification(strings.Replace(status, `{"name":"focus"}`, `{"name":"focus","valueReference":{"reference":"ClaimResponse/cr-1"}}`, 1), `{"fullUrl":"http://payer.example/fhir/ClaimResponse/cr-1","request":{"method":"PUT","url":"ClaimResponse/cr-1"}}`), true},
		"a heartbeat carrying a resource":        {notification(statusEntry("heartbeat", testCanonical), fullResourceEntry), false},
		"an entry with response.outcome":         {notification(status, `{"response":{"status":"200","outcome":{"resourceType":"OperationOutcome"}}}`), false},
		"the status entry with response.outcome": {notification(strings.Replace(status, `{"fullUrl":"Parameters/`, `{"response":{"status":"200","outcome":{}},"fullUrl":"Parameters/`, 1)), false},
		"a resource-valued parameter":            {notification(strings.Replace(status, `"parameter":[`, `"parameter":[{"name":"x","resource":{"resourceType":"Patient"}},`, 1)), false},
		"a resource-valued part":                 {notification(strings.Replace(status, `{"name":"focus"}`, `{"name":"focus","resource":{"resourceType":"ClaimResponse"}}`, 1)), false},
		"a resource-valued part, deeper":         {notification(strings.Replace(status, `{"name":"focus"}`, `{"name":"focus","part":[{"name":"y","resource":{}}]}`, 1)), false},
		"a contained resource":                   {notification(strings.Replace(status, `"resourceType":"Parameters",`, `"resourceType":"Parameters","contained":[{"resourceType":"Patient"}],`, 1)), false},
		"a null resource in a later entry":       {notification(status, `{"resource":null}`), false},
		// Fail closed (M1): a single-object part, a list that is not an
		// array, a non-object element, and member names in another case or a
		// Unicode fold all count as content.
		"a single-object part holding a resource": {notification(strings.Replace(status, `{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus"}]}`,
			`{"name":"notification-event","part":{"name":"focus","resource":{"resourceType":"ClaimResponse","id":"cr-1"}}}`, 1)), false},
		"a single-object part, no resource":         {notification(strings.Replace(status, `{"name":"focus"}]`, `{"name":"focus","part":{"name":"x"}}]`, 1)), false},
		"a part element that is not an object":      {notification(strings.Replace(status, `{"name":"focus"}]`, `{"name":"focus"},"x"]`, 1)), false},
		"a later entry that is not an object":       {notification(status, `"x"`), false},
		"a later entry's Resource":                  {notification(status, `{"request":{"method":"PUT"},"Resource":{"resourceType":"Bundle"}}`), false},
		"a later entry's reſource (a Unicode fold)": {notification(status, `{"request":{"method":"PUT"},"reſource":{"resourceType":"Bundle"}}`), false},
		"a part's Resource":                         {notification(strings.Replace(status, `{"name":"focus"}`, `{"name":"focus","Resource":{"resourceType":"ClaimResponse"}}`, 1)), false},
		"a parameter's Part holding a resource":     {notification(strings.Replace(status, `"part":[{"name":"event-number"`, `"Part":[{"name":"x","resource":{}},{"name":"event-number"`, 1)), false},
		"the status's Contained":                    {notification(strings.Replace(status, `"resourceType":"Parameters",`, `"resourceType":"Parameters","Contained":[],`, 1)), false},
		"a Response with an Outcome":                {notification(status, `{"Response":{"status":"200","Outcome":{}}}`), false},
		"a response that is not an object":          {notification(status, `{"response":"200"}`), false},
		"a response with no outcome":                {notification(status, `{"response":{"status":"200"}}`), true},
		// The any-depth reading: a resource at a place no named check reads.
		"a resource as a root Bundle member":          {strings.Replace(notification(status, idOnlyEntry), `"type":"history",`, `"type":"history","x":{"resourceType":"Patient","id":"p1"},`, 1), false},
		"a resource as another member of entry[1]":    {notification(status, `{"request":{"method":"PUT"},"x":{"resourceType":"Patient","id":"p1"}}`), false},
		"a resource in entry[0].request":              {notification(strings.Replace(status, `{"fullUrl":"Parameters/`, `{"request":{"method":"GET","x":{"resourceType":"Patient","id":"p1"}},"fullUrl":"Parameters/`, 1)), false},
		"a resource in a focus part's valueReference": {notification(strings.Replace(status, `{"name":"focus"}`, `{"name":"focus","valueReference":{"reference":"Task/t1","x":{"resourceType":"Patient","id":"p1"}}}`, 1)), false},
		"a resource in the status's meta":             {notification(strings.Replace(status, `"resourceType":"Parameters",`, `"resourceType":"Parameters","meta":{"x":{"resourceType":"Patient","id":"p1"}},`, 1)), false},
		"a resourceType named in another case":        {notification(status, `{"request":{"method":"PUT"},"x":{"ResourceType":"Patient"}}`), false},
		"a contained member deep in an entry":         {notification(status, `{"request":{"method":"PUT","x":{"contained":[]}}}`), false},
		"a resourceType at the status's own level":    {notification(status), true},
	} {
		n := mustNotification(t, row.body, topic)
		if got := n.NoContent(); got != row.noContent {
			t.Errorf("%s: NoContent = %v, want %v", name, got, row.noContent)
		}
		wantLeg := "test-notify"
		if row.noContent {
			wantLeg = "test-signal"
		}
		if got := Leg(topic, n); got != wantLeg {
			t.Errorf("%s: Leg = %q, want %q", name, got, wantLeg)
		}
	}
	// The CDex-like topic has no content leg: a notification with content
	// rides none, and one without rides its signal leg.
	if got := Leg(cdexTopic, mustNotification(t, notification(status, fullResourceEntry), cdexTopic)); got != "" {
		t.Errorf("a CDex-like topic, content: Leg = %q, want none", got)
	}
	if got := Leg(cdexTopic, mustNotification(t, notification(status, idOnlyEntry), cdexTopic)); got != "test-signal" {
		t.Errorf("a CDex-like topic, no content: Leg = %q", got)
	}
}

// noLoop is a loop rule that finds no notification address.
func noLoop(string) bool { return false }

// The subscriber's gateway opens a notification's sealed values under the
// leg's own binding only, and delivers only to an endpoint §6.5 accepts.
func TestOpenDelivery(t *testing.T) {
	sub := mustKeys(t, "provider-1", curKey, nil)
	se := sub.SealEndpoint(binding, subEndpoint)
	sh, err := sub.SealHeaders(binding, se, []string{"Authorization: Bearer s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	d, r := sub.OpenDelivery(binding, se, sh, noLoop)
	if r != nil || d.Endpoint != subEndpoint || len(d.Headers) != 1 || d.Headers[0] != "Authorization: Bearer s3cret" {
		t.Fatalf("OpenDelivery = %+v, %+v", d, r)
	}
	otherEndpoint := sub.SealEndpoint(binding, subEndpoint+"x")
	for name, row := range map[string]struct {
		b      Binding
		se, sh string
	}{
		"another notifier":            {Binding{"provider-1", "payer-2", "test"}, se, sh},
		"another topic":               {Binding{"provider-1", "payer-1", "test-task"}, se, sh},
		"another subscriber":          {Binding{"provider-2", "payer-1", "test"}, se, sh},
		"another endpoint":            {binding, otherEndpoint, sh},
		"an endpoint that is no seal": {binding, "x", sh},
		"headers that are no seal":    {binding, se, "x"},
		"no headers":                  {binding, se, ""},
		"a retired key":               {binding, mustKeys(t, "provider-1", predKey, nil).SealEndpoint(binding, subEndpoint), sh},
	} {
		d, r := sub.OpenDelivery(row.b, row.se, row.sh, noLoop)
		wantRefusal(t, name, r, http.StatusForbidden, recording.RefusalAuthority)
		if d.Endpoint != "" || d.Headers != nil {
			t.Errorf("%s: delivered %+v", name, d)
		}
	}
	// A value a search sealed opens under the same key, but an endpoint §6.5
	// refuses is never delivered to.
	for name, row := range map[string]struct {
		endpoint  string
		loop      func(string) bool
		primitive string
	}{
		"an http endpoint a search sealed":    {"http://ehr.example/hook", noLoop, recording.RefusalIntegrity},
		"a userinfo endpoint a search sealed": {"https://u:p@ehr.example/hook", noLoop, recording.RefusalIntegrity},
		"a notification address":              {subEndpoint, func(string) bool { return true }, recording.RefusalRouting},
		"no loop rule":                        {subEndpoint, nil, recording.RefusalRouting},
	} {
		vals := sub.SealEndpointForSearch(binding, row.endpoint)
		hs, err := sub.SealHeaders(binding, vals[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		d, r := sub.OpenDelivery(binding, vals[0], hs, row.loop)
		wantRefusal(t, name, r, http.StatusUnprocessableEntity, row.primitive)
		if d.Endpoint != "" || d.Headers != nil {
			t.Errorf("%s: delivered %+v", name, d)
		}
	}
}
