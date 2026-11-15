package subscription

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testTopic is the stand-in topic the library's rows use: a PAS-like topic
// with both contents and a content leg, whose filter is one org-identifier
// naming the subscriber's attested provider identifiers. cdexLike is the
// CDex-like one: id-only, and no content leg.
type testTopic struct{ cdexLike, listsEmpty bool }

const (
	testCanonical = "http://example.org/SubscriptionTopic/test"
	testTopicExt  = "http://example.org/StructureDefinition/test-topic-extension"
)

func (t testTopic) Token() string {
	if t.cdexLike {
		return "test-task"
	}
	return "test"
}
func (testTopic) Canonical() string { return testCanonical }
func (t testTopic) Contents() []string {
	switch {
	case t.cdexLike:
		return []string{"id-only"}
	case t.listsEmpty:
		return []string{"full-resource", "id-only", "empty"}
	}
	return []string{"full-resource", "id-only"}
}
func (testTopic) Extensions() []string { return []string{testTopicExt} }
func (testTopic) CheckFilter(subscriber Holder, params []FilterParam) *Refusal {
	if len(params) != 1 || params[0].Name != "org-identifier" || params[0].Modifier != "" {
		return forbidden("the test topic's filter is exactly one org-identifier")
	}
	return MatchIdentifiers(params[0].Values, subscriber.ProviderIDs)
}
func (testTopic) SubscribeLeg() string { return "test-subscribe" }
func (t testTopic) ContentLeg() string {
	if t.cdexLike {
		return ""
	}
	return "test-notify"
}
func (testTopic) SignalLeg() string                           { return "test-signal" }
func (testTopic) CheckContent(Holder, *Notification) *Refusal { return nil }
func (testTopic) NotificationShape() NotificationShape        { return taskShape }

// taskShape is a notification shape whose focus is a Task and that declares
// no additional-context, as the HRex Task topic's is.
var taskShape = NotificationShape{Topic: "http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task", Focus: []string{"Task"}}

var (
	topic       Topic = testTopic{}
	cdexTopic   Topic = testTopic{cdexLike: true}
	npi               = Identifier{System: "http://hl7.org/fhir/sid/us-npi", Value: "1234567893"}
	subscriberA       = Holder{ID: "provider-1", ProviderIDs: []Identifier{npi}}
	binding           = Binding{Subscriber: "provider-1", Notifier: "payer-1", Topic: "test"}
)

const (
	notifierIngress = "https://payer-gw.example/ingress"
	subEndpoint     = "https://ehr.example/api/pas/notification?ehr=https%3A%2F%2Fehr.example%2Ffhir"
	subHeaders      = `["Authorization: Bearer s3cret", "X-Custom: a"]`
)

// baseSubscription is a Subscription in the shape the reference provider
// builds, on the test topic, with a heartbeat period and two header values.
const baseSubscription = `{
  "resourceType": "Subscription",
  "id": "sub-1",
  "meta": {
    "profile": ["http://example.org/StructureDefinition/test-subscription", "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"]
  },
  "status": "requested",
  "reason": "decisions",
  "criteria": "http://example.org/SubscriptionTopic/test",
  "_criteria": {
    "extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": "org-identifier=1234567893"}]
  },
  "channel": {
    "extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period", "valueUnsignedInt": 60}],
    "type": "rest-hook",
    "endpoint": "https://ehr.example/api/pas/notification?ehr=https%3A%2F%2Fehr.example%2Ffhir",
    "payload": "application/fhir+json",
    "_payload": {
      "extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content", "valueCode": "full-resource"}]
    },
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]
  }
}`

// edit returns s with old replaced by new exactly once.
func edit(t testing.TB, s, old, new string) string {
	t.Helper()
	if strings.Count(s, old) != 1 {
		t.Fatalf("%q is not in the fixture exactly once", old)
	}
	return strings.Replace(s, old, new, 1)
}

func mustSubscription(t testing.TB, body string) *Subscription {
	t.Helper()
	s, r := ParseSubscription([]byte(body), "application/fhir+json")
	if r != nil {
		t.Fatalf("ParseSubscription: %+v", r)
	}
	return s
}

// jsonEqual reports whether a and b are the same JSON value.
func jsonEqual(t testing.TB, a, b []byte) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, a)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return reflect.DeepEqual(va, vb)
}

// wantRefusal fails unless r is a refusal with this status and primitive.
func wantRefusal(t testing.TB, name string, r *Refusal, status int, primitive string) {
	t.Helper()
	if r == nil || r.Status != status || r.Primitive != primitive || r.Reason == "" {
		t.Errorf("%s: refusal %+v, want %d %s with a reason", name, r, status, primitive)
	}
}
