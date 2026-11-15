package subscription

import (
	"strings"
	"testing"
)

// ContextDeclared holds each notification-event's additional-context to the
// topic's notification shape: a topic that declares none (the HRex Task
// topic's, taskShape) admits no additional-context part, whatever it holds;
// one that declares a type admits a reference to that type alone, read from
// the reference. A notification with no additional-context, or with an
// additional-context outside a notification-event, is declared.
func TestNotificationContextDeclared(t *testing.T) {
	status := statusEntry("event-notification", testCanonical)
	with := func(part string) string {
		return notification(strings.Replace(status, `{"name":"focus"}]`, `{"name":"focus","valueReference":{"reference":"Task/t1"}},`+part+`]`, 1))
	}
	declaring := NotificationShape{Topic: testCanonical, Focus: []string{"Task"}, AdditionalContext: []string{"Coverage"}}
	for name, row := range map[string]struct {
		body     string
		shape    NotificationShape
		declared bool
	}{
		"no additional-context":                         {with(`{"name":"event-number","valueString":"2"}`), taskShape, true},
		"a handshake":                                   {notification(statusEntry("handshake", testCanonical)), taskShape, true},
		"an additional-context Patient":                 {with(`{"name":"additional-context","valueReference":{"reference":"Patient/p1"}}`), taskShape, false},
		"an additional-context Task":                    {with(`{"name":"additional-context","valueReference":{"reference":"Task/t2"}}`), taskShape, false},
		"an additional-context naming no resource":      {with(`{"name":"additional-context","valueString":"x"}`), taskShape, false},
		"an additional-context, member in another case": {with(`{"Name":"additional-context","ValueReference":{"reference":"Patient/p1"}}`), taskShape, false},
		"a declared type":                               {with(`{"name":"additional-context","valueReference":{"reference":"Coverage/c1"}}`), declaring, true},
		"an undeclared type":                            {with(`{"name":"additional-context","valueReference":{"reference":"Patient/p1"}}`), declaring, false},
		"a type read no one way":                        {with(`{"name":"additional-context","valueReference":{"reference":"#c1"}}`), declaring, false},
		"no valueReference, a type declared":            {with(`{"name":"additional-context","valueString":"Coverage/c1"}`), declaring, false},
		"additional-context outside an event": {notification(strings.Replace(status, `"parameter":[`,
			`"parameter":[{"name":"other","part":[{"name":"additional-context","valueReference":{"reference":"Patient/p1"}}]},`, 1)), taskShape, true},
	} {
		t.Run(name, func(t *testing.T) {
			n, rf := ParseNotification([]byte(row.body), topic)
			if rf != nil {
				t.Fatalf("not a notification: %s", rf.Reason)
			}
			if got := n.ContextDeclared(row.shape); got != row.declared {
				t.Fatalf("ContextDeclared %v, want %v", got, row.declared)
			}
		})
	}
}
