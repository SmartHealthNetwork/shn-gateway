package subscription

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// statusAnswerEntry is a SubscriptionStatus Parameters as HAPI FHIR writes it (the
// notifications design §3, the capture): subscription, topic, status, type,
// events-since-subscription-start, and a notification-event whose focus part
// has no value.
const statusAnswerEntry = `{"fullUrl":"Parameters/3f1d","resource":{"resourceType":"Parameters","meta":{"profile":["http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-status-r4"]},` +
	`"parameter":[{"name":"subscription","valueReference":{"reference":"Subscription/sub-1"}},` +
	`{"name":"topic","valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"},` +
	`{"name":"status","valueCode":"active"},{"name":"type","valueCode":"query-status"},` +
	`{"name":"events-since-subscription-start","valueString":"3"},` +
	`{"name":"notification-event","part":[{"name":"event-number","valueString":"3"},{"name":"timestamp","valueInstant":"2026-11-13T10:00:00Z"},{"name":"focus"}]}]}}`

const answerSubscription = `{"resourceType":"Subscription","id":"sub-1","status":"active","criteria":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task","channel":{"type":"rest-hook","endpoint":"https://gw.example/notify/x","header":["Authorization: Bearer a.b"]}}`

const answerOutcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Successfully deleted 1 resource(s)"}]}`

func searchset(entries ...string) string {
	return `{"resourceType":"Bundle","type":"searchset","total":` + "1" + `,"link":[{"relation":"self","url":"https://ehr.example/fhir/Subscription?url=x"},{"relation":"next","url":"https://ehr.example/fhir?_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset"}],"entry":[` + strings.Join(entries, ",") + `]}`
}

func entryOf(resource string, mode string) string {
	return `{"fullUrl":"https://ehr.example/fhir/X/1","resource":` + resource + `,"search":{"mode":"` + mode + `"}}`
}

func historyOf(entries ...string) string {
	return `{"resourceType":"Bundle","type":"history","entry":[` + strings.Join(entries, ",") + `]}`
}

func statusSearchset(entries ...string) string {
	return `{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`
}

const patientJSON = `{"resourceType":"Patient","id":"p1","name":[{"family":"Doe"}]}`

// The interactions, by the frame's subscriptionRequest.
var (
	read, vread, create, update = shnsdk.SubscriptionRead, shnsdk.SubscriptionVRead, shnsdk.SubscriptionCreate, shnsdk.SubscriptionUpdate
	del, status, search, page   = shnsdk.SubscriptionDelete, shnsdk.SubscriptionStatus, shnsdk.SubscriptionSearch, shnsdk.SubscriptionPage
	every                       = []shnsdk.SubscriptionInteraction{read, vread, create, update, del, status, search, page}
)

// Each interaction's own answers pass, and the Subscriptions an answer
// carries are returned for the ownership test; every other interaction
// refuses the same answer (the cross-interaction smuggle).
// withFocus is statusAnswerEntry with its focus part valued with ref, a
// Reference's JSON, and parts appended to the notification-event.
func withFocus(ref string, parts ...string) string {
	focus := `{"name":"focus"}`
	if ref != "" {
		focus = `{"name":"focus","valueReference":` + ref + `}`
	}
	all := append([]string{focus}, parts...)
	return strings.Replace(statusAnswerEntry, `{"name":"focus"}`, strings.Join(all, ","), 1)
}

func TestCheckSubscribeAnswer_EachInteractionsOwnShape(t *testing.T) {
	ss := searchset(entryOf(answerSubscription, "match"), entryOf(answerSubscription, "match"))
	for _, c := range []struct {
		name string
		body string
		subs int
		ok   []shnsdk.SubscriptionInteraction
	}{
		{"a Subscription", answerSubscription, 1, []shnsdk.SubscriptionInteraction{read, vread, create, update}},
		{"a Subscription after a byte order mark", "\xEF\xBB\xBF" + answerSubscription, 1, []shnsdk.SubscriptionInteraction{read, vread, create, update}},
		{"a searchset after a byte order mark", "\xEF\xBB\xBF" + ss, 2, []shnsdk.SubscriptionInteraction{search, page}},
		{"no body", "", 0, []shnsdk.SubscriptionInteraction{create, update, del}},
		{"an OperationOutcome", answerOutcome, 0, []shnsdk.SubscriptionInteraction{create, update, del}},
		{"a searchset of Subscriptions", ss, 2, []shnsdk.SubscriptionInteraction{search, page}},
		{"a searchset with an outcome entry", searchset(entryOf(answerSubscription, "match"), entryOf(answerOutcome, "outcome")), 1, []shnsdk.SubscriptionInteraction{search, page}},
		{"an empty searchset", `{"resourceType":"Bundle","type":"searchset","total":0}`, 0, []shnsdk.SubscriptionInteraction{search, page}},
		{"a $status searchset (STU 1.1)", statusSearchset(statusAnswerEntry), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose focus is a Task", statusSearchset(withFocus(`{"reference":"Task/t1"}`)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose focus is a Task version", statusSearchset(withFocus(`{"reference":"Task/t1/_history/2"}`)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose focus is a Task by its address", statusSearchset(withFocus(`{"reference":"https://ehr.example/fhir/Task/t1"}`)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose focus is a Task, typed", statusSearchset(withFocus(`{"reference":"Task/t1","type":"Task"}`)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status with the Parameters' and Bundle's own members", strings.Replace(statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"parameter":[`, `"id":"st1","language":"en","parameter":[`, 1)), `"type":"searchset",`, `"type":"searchset","id":"b1","meta":{"lastUpdated":"2026-11-13T10:00:00Z","tag":[{"code":"x"}]},"timestamp":"2026-11-13T10:00:00Z","total":1,"link":[{"relation":"self","url":"https://ehr.example/fhir/Subscription/sub-1/$status"}],`, 1), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status history whose entry carries its own request and response", historyOf(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","request":{"method":"GET","url":"Subscription/sub-1/$status"},"response":{"status":"200"},`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status reporting an error", statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/subscription-error","code":"timeout"}],"text":"timed out"}}`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status counting events as a number", statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueString":"3"},{"name":"notification-event"`, `"valueInteger":3},{"name":"notification-event"`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status naming its topic with a version", statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"`, `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task|1.1.0"`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose subscription reference is typed", statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"reference":"Subscription/sub-1"}`, `{"reference":"Subscription/sub-1","type":"Subscription"}`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose Bundle and meta carry their own members", strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","id":"b1","meta":{"versionId":"1","lastUpdated":"2026-11-13T10:00:00Z","source":"#a","tag":[{"system":"http://x","code":"y","display":"z","userSelected":false}]},"timestamp":"2026-11-13T10:00:00Z","total":1,`, 1), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status whose focus is a Task, typed by canonical", statusSearchset(withFocus(`{"reference":"Task/t1","type":"http://hl7.org/fhir/StructureDefinition/Task"}`)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status history (before STU 1.1)", historyOf(statusAnswerEntry), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status, its status without a profile", historyOf(strings.Replace(statusAnswerEntry, `"meta":{"profile":["http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription-status-r4"]},`, "", 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status, a versioned profile", historyOf(strings.Replace(statusAnswerEntry, `status-r4"]`, `status-r4|1.1.0"]`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
		{"a $status naming the Subscription by its absolute address", statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/Subscription/sub-1"`, 1)), 0, []shnsdk.SubscriptionInteraction{status}},
	} {
		for _, in := range every {
			subs, r := AnswerSubscriptions([]byte(c.body), in, "sub-1", taskShape)
			want := false
			for _, o := range c.ok {
				want = want || o == in
			}
			if want {
				if r != nil || len(subs) != c.subs {
					t.Errorf("%s on a %s: %d Subscriptions, %+v; want %d and no refusal", c.name, in, len(subs), r, c.subs)
				}
				if r := CheckSubscribeAnswer([]byte(c.body), in, "sub-1", taskShape); r != nil {
					t.Errorf("%s on a %s: CheckSubscribeAnswer %+v", c.name, in, r)
				}
				continue
			}
			wantRefusal(t, c.name+" on a "+string(in), r, http.StatusBadGateway, recording.RefusalAuthority)
			if subs != nil {
				t.Errorf("%s on a %s: Subscriptions returned beside the refusal", c.name, in)
			}
		}
	}
}

// Every way to smuggle a resource or another shape through a Subscription
// interaction's answer is refused 502 on authority: the rejection rows, each
// on the interaction whose shape it imitates.
func TestCheckSubscribeAnswer_RefusesSmuggling(t *testing.T) {
	nonStatus := `{"resource":{"resourceType":"Parameters","parameter":[{"name":"result","valueString":"x"}]}}`
	for _, c := range []struct {
		name string
		in   shnsdk.SubscriptionInteraction
		body string
	}{
		{"a Patient on a read", read, patientJSON},
		{"no body on a read", read, ""},
		{"an OperationOutcome on a read", read, answerOutcome},
		{"a searchset on a read", read, searchset(entryOf(answerSubscription, "match"))},
		{"not JSON on a create", create, `<Subscription xmlns="http://hl7.org/fhir"/>`},
		{"an array on a read", read, `[` + answerSubscription + `]`},
		{"a Subscription on a delete", del, answerSubscription},
		{"a Patient on a create", create, patientJSON},
		{"an OperationOutcome with a contained resource on a create", create, strings.Replace(answerOutcome, `"issue"`, `"contained":[`+patientJSON+`],"issue"`, 1)},
		{"an OperationOutcome with a contained resource on an update", update, strings.Replace(answerOutcome, `"issue"`, `"contained":[`+patientJSON+`],"issue"`, 1)},
		{"an OperationOutcome with a contained resource on a delete", del, strings.Replace(answerOutcome, `"issue"`, `"contained":[`+patientJSON+`],"issue"`, 1)},
		{"a Patient entry in the searchset", search, searchset(entryOf(answerSubscription, "match"), entryOf(patientJSON, "include"))},
		{"a Patient entry under a folded member name", search, strings.Replace(searchset(entryOf(patientJSON, "match")), `"entry"`, `"Entry"`, 1)},
		{"a contained Patient in a Subscription", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","contained":[`+patientJSON+`],`, 1)},
		{"a contained Patient under a folded name", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","Contained":[`+patientJSON+`],`, 1)},
		{"a contained that is not an array", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","contained":`+patientJSON+`,`, 1)},
		{"a contained holding no resource type", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","contained":[{"id":"x","name":[{"family":"Doe"}]}],`, 1)},
		{"an empty contained", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","contained":[],`, 1)},
		{"a resource nested in a Subscription's extension", read, strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","extension":[{"url":"x","valueX":`+patientJSON+`}],`, 1)},
		{"a contained Patient in a searchset Subscription", search, searchset(entryOf(strings.Replace(answerSubscription, `"id":"sub-1",`, `"id":"sub-1","contained":[`+patientJSON+`],`, 1), "match"))},
		{"an entry that is not an array", search, `{"resourceType":"Bundle","type":"searchset","entry":` + entryOf(answerSubscription, "match") + `}`},
		{"an entry that is not an object", page, searchset(`"x"`)},
		{"an entry with no resource", search, searchset(`{"fullUrl":"https://ehr.example/fhir/X/1"}`)},
		{"an entry whose resource is not an object", search, searchset(`{"resource":[` + answerSubscription + `]}`)},
		{"an entry's response outcome", search, searchset(`{"resource":` + answerSubscription + `,"response":{"status":"200","outcome":` + patientJSON + `}}`)},
		{"a resource outside the entries", search, strings.Replace(searchset(entryOf(answerSubscription, "match")), `"total":1,`, `"total":1,"issues":`+patientJSON+`,`, 1)},
		{"a collection Bundle on a search", search, strings.Replace(searchset(entryOf(answerSubscription, "match")), `"searchset"`, `"collection"`, 1)},
		{"a history Bundle on a search", search, historyOf(entryOf(answerSubscription, "match"))},
		{"a Bundle with no type on a page", page, `{"resourceType":"Bundle","entry":[` + entryOf(answerSubscription, "match") + `]}`},
		{"a status Bundle on a search (the cross-interaction smuggle)", search, statusSearchset(statusAnswerEntry)},
		{"a status Bundle on a page", page, statusSearchset(statusAnswerEntry)},
		{"a searchset of Subscriptions on a $status", status, searchset(entryOf(answerSubscription, "match"))},
		{"a collection Bundle on a $status", status, strings.Replace(statusSearchset(statusAnswerEntry), `"searchset"`, `"collection"`, 1)},
		{"no entry on a $status", status, `{"resourceType":"Bundle","type":"searchset"}`},
		{"two status entries", status, statusSearchset(statusAnswerEntry, statusAnswerEntry)},
		{"a status naming another id", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"Subscription/sub-2"`, 1))},
		{"a status naming a version of the id", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"Subscription/sub-1/_history/2"`, 1))},
		{"a status naming the id by another type", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"Task/sub-1"`, 1))},
		{"a status naming the id by an address with a query", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/Subscription/sub-1?x=1"`, 1))},
		{"a status naming the id by an address with a fragment", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/Subscription/sub-1#x"`, 1))},
		{"a status naming the id by an address with an empty fragment", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/Subscription/sub-1#"`, 1))},
		{"a status naming an id the asked one prefixes, by its address", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/Subscription/sub-12"`, 1))},
		{"a status naming the id under another type's path", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https://ehr.example/fhir/XSubscription/sub-1"`, 1))},
		{"a status naming the id by an address of another scheme", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"ftp://ehr.example/fhir/Subscription/sub-1"`, 1))},
		{"a status naming the id by an address with no host", status, statusSearchset(strings.Replace(statusAnswerEntry, `"Subscription/sub-1"`, `"https:///fhir/Subscription/sub-1"`, 1))},
		// The topic's notification shape: a focus is a Task, read from the
		// reference itself; no additional-context is declared, so any is
		// refused; a reference whose type cannot be read is unknown.
		{"a status whose focus is a Patient", status, statusSearchset(withFocus(`{"reference":"Patient/p1"}`))},
		{"a status whose focus is a Patient by its address", status, statusSearchset(withFocus(`{"reference":"https://ehr.example/fhir/Patient/p1"}`))},
		{"a status whose focus is a Patient version", status, statusSearchset(withFocus(`{"reference":"Patient/p1/_history/1"}`))},
		{"a status whose focus names Task as its type but a Patient", status, statusSearchset(withFocus(`{"reference":"Patient/p1","type":"Task"}`))},
		{"a status whose focus is a Task typed as a Patient", status, statusSearchset(withFocus(`{"reference":"Task/t1","type":"Patient"}`))},
		{"a status whose focus is contained", status, statusSearchset(withFocus(`{"reference":"#t1"}`))},
		{"a status whose focus is a urn", status, statusSearchset(withFocus(`{"reference":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b"}`))},
		{"a status whose focus is typed with no reference", status, statusSearchset(withFocus(`{"type":"Task"}`))},
		{"a status whose focus is an empty reference", status, statusSearchset(withFocus(`{"reference":""}`))},
		{"a status whose focus has a query", status, statusSearchset(withFocus(`{"reference":"https://ehr.example/fhir/Task/t1?x=1"}`))},
		{"a status whose focus is a path above Task", status, statusSearchset(withFocus(`{"reference":"x/Task/t1"}`))},
		{"a status whose focus id is a dot segment", status, statusSearchset(withFocus(`{"reference":"Task/.."}`))},
		{"a status with an additional-context Task", status, statusSearchset(withFocus(`{"reference":"Task/t1"}`, `{"name":"additional-context","valueReference":{"reference":"Task/t2"}}`))},
		{"a status with an additional-context Patient", status, statusSearchset(withFocus(`{"reference":"Task/t1"}`, `{"name":"additional-context","valueReference":{"reference":"Patient/p1"}}`))},
		// The Parameters, its meta and the Bundle carry only their own
		// members: nothing beside the parameters can name anyone.
		{"a status with a top-level subject naming a Patient", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"parameter":[`, `"subject":{"reference":"Patient/p1","display":"Jane Doe"},"parameter":[`, 1))},
		{"a status with a top-level extension naming a Patient", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"parameter":[`, `"extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}],"parameter":[`, 1))},
		{"a status with narrative", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"parameter":[`, `"text":{"status":"generated","div":"<div xmlns=\"http://www.w3.org/1999/xhtml\">Jane Doe</div>"},"parameter":[`, 1))},
		{"a status whose meta carries an extension", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"meta":{`, `"meta":{"extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}],`, 1))},
		{"a status Bundle with a signature", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-13T00:00:00Z","who":{"reference":"Patient/p1"}},`, 1)},
		{"a status Bundle with an identifier", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","identifier":{"assigner":{"reference":"Patient/p1"}},`, 1)},
		{"a status Bundle whose meta carries an extension", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","meta":{"extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]},`, 1)},
		// Nothing in the answer may point at anything but the Subscription
		// asked about and its focus: no extension anywhere, no reference
		// object elsewhere, entries and links closed, every value member the
		// JSON type its FHIR type is written as, and no address naming
		// another resource type.
		{"a status Bundle whose meta tag carries an extension", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","meta":{"tag":[{"code":"x","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]}]},`, 1)},
		{"a status whose meta security carries an extension", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"meta":{`, `"meta":{"security":[{"code":"x","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]}],`, 1))},
		{"a status Bundle link carrying an extension", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","link":[{"relation":"self","url":"https://ehr.example/fhir/Subscription/sub-1/$status","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]}],`, 1)},
		{"a status entry carrying an extension", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}],`, 1))},
		{"a status entry carrying a modifierExtension", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","modifierExtension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}],`, 1))},
		{"a status entry's search carrying an extension", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","search":{"mode":"match","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]},`, 1))},
		{"a status entry carrying a member of no entry's", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","link":[],`, 1))},
		{"a history status entry whose request names a Patient", status, historyOf(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","request":{"method":"GET","url":"Patient/p1"},`, 1))},
		{"a history status entry whose response locates a Patient", status, historyOf(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","response":{"status":"200","location":"Patient/p1"},`, 1))},
		{"a status entry whose fullUrl names a Patient", status, statusSearchset(strings.Replace(statusAnswerEntry, `"fullUrl":"Parameters/3f1d"`, `"fullUrl":"https://ehr.example/fhir/Patient/p1"`, 1))},
		{"a status Bundle link naming a Patient in its query", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","link":[{"relation":"self","url":"https://ehr.example/fhir/Subscription?subject=Patient/p1"}],`, 1)},
		{"a status whose error carries an extension", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"text":"x","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]}}`, 1))},
		{"a status whose error coding carries an extension", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"coding":[{"code":"x","extension":[{"url":"http://x","valueReference":{"reference":"Patient/p1"}}]}]}}`, 1))},
		{"a status whose topic is an object naming a Patient", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"`, `"valueCanonical":{"reference":"Patient/p1"}`, 1))},
		{"a status parameter whose id is an object naming a Patient", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"status","valueCode":"active"}`, `{"name":"status","id":{"reference":"Patient/p1"},"valueCode":"active"}`, 1))},
		{"a status whose events count is a string where a number is", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueString":"3"},{"name":"notification-event"`, `"valueInteger":"3"},{"name":"notification-event"`, 1))},
		// Percent-escapes hide no type's name; the topic is the topic's own
		// canonical; a CodeableConcept and its codings carry their own
		// members only.
		{"a status entry whose fullUrl escapes a Patient's name", status, statusSearchset(strings.Replace(statusAnswerEntry, `"fullUrl":"Parameters/3f1d"`, `"fullUrl":"https://ehr.example/fhir/%50atient/123"`, 1))},
		{"a history status entry whose request escapes a Patient's name", status, historyOf(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","request":{"method":"GET","url":"https://ehr.example/fhir/Pati%65nt/123"},`, 1))},
		{"a status entry whose fullUrl cannot be unescaped", status, statusSearchset(strings.Replace(statusAnswerEntry, `"fullUrl":"Parameters/3f1d"`, `"fullUrl":"https://ehr.example/fhir/%zz"`, 1))},
		{"a status whose topic is a Patient's address", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"`, `"valueCanonical":"https://ehr.example/fhir/Patient/123"`, 1))},
		{"a status whose topic is another topic", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"`, `"valueCanonical":"http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic"`, 1))},
		{"a status whose topic is given as a uri, another one", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"`, `"valueUri":"urn:x"`, 1))},
		{"a status whose error names a Patient by identifier", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"coding":[{"code":"x"}],"identifier":{"system":"urn:mrn","value":"123"},"type":"Patient"}}`, 1))},
		{"a status whose error coding carries a member of no Coding's", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"coding":[{"code":"x","patient":"123"}]}}`, 1))},
		// The whole answer is closed at every depth to the Backport
		// SubscriptionStatus shape, every value of its type.
		{"a status Bundle whose id is an object naming a person", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","id":{"name":[{"family":"Doe"}],"birthDate":"1970-01-01"},`, 1)},
		{"a status Bundle whose meta tag carries a member of no Coding's", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","meta":{"tag":[{"code":"x","name":[{"family":"Doe"}]}]},`, 1)},
		{"a status whose meta security carries a member of no Coding's", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"meta":{`, `"meta":{"security":[{"code":"x","patient":{"reference":"Patient/p1"}}],`, 1))},
		{"a status Bundle whose total is a string", status, strings.Replace(statusSearchset(statusAnswerEntry), `"type":"searchset",`, `"type":"searchset","total":"Patient/p1",`, 1)},
		{"a status whose language is an object", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `"parameter":[`, `"language":{"family":"Doe"},"parameter":[`, 1))},
		{"a status entry whose search mode is an object", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"fullUrl":"Parameters/3f1d",`, `{"fullUrl":"Parameters/3f1d","search":{"mode":{"patient":"Patient/p1"}},`, 1))},
		{"a status Coding whose userSelected is a string", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"name":"type","valueCode":"query-status"}`, `{"name":"type","valueCode":"query-status"},{"name":"error","valueCodeableConcept":{"coding":[{"code":"x","userSelected":"Patient/p1"}]}}`, 1))},
		{"a status whose subscription reference is typed as a Patient", status, statusSearchset(strings.Replace(withFocus(`{"reference":"Task/t1"}`), `{"reference":"Subscription/sub-1"}`, `{"reference":"Subscription/sub-1","type":"Patient"}`, 1))},
		{"a status naming no subscription", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"name":"subscription","valueReference":{"reference":"Subscription/sub-1"}},`, ``, 1))},
		{"a non-status Parameters", status, historyOf(nonStatus)},
		{"a status Parameters, then a Patient", status, historyOf(statusAnswerEntry, `{"resource":`+patientJSON+`}`)},
		{"a status Parameters not first", status, historyOf(entryOf(answerSubscription, "match"), statusAnswerEntry)},
		{"a status Parameters naming another profile", status, historyOf(strings.Replace(statusAnswerEntry, `"]},`, `","http://example.org/other"]},`, 1))},
		{"a status Parameters with a resource parameter", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"status","valueCode":"active"}`, `{"name":"status","resource":`+patientJSON+`}`, 1))},
		{"a status Parameters whose part carries a resource", status, statusSearchset(strings.Replace(statusAnswerEntry, `{"name":"focus"}`, `{"name":"focus","resource":`+patientJSON+`}`, 1))},
		{"a status Parameters with an unknown parameter", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"status","valueCode":"active"}`, `{"name":"member","valueString":"M123"}`, 1))},
		{"a status Parameters with a parameter carrying another value type", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"status","valueCode":"active"}`, `{"name":"status","valueString":"active"}`, 1))},
		{"a status Parameters with a reference carrying an identifier", status, historyOf(strings.Replace(statusAnswerEntry, `{"reference":"Subscription/sub-1"}`, `{"reference":"Subscription/sub-1","identifier":{"value":"M123"}}`, 1))},
		{"a status Parameters with two subscriptions", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"status","valueCode":"active"}`, `{"name":"subscription","valueReference":{"reference":"Subscription/sub-1"}}`, 1))},
		{"a status Parameters with no type", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"type","valueCode":"query-status"},`, "", 1))},
		{"a status Parameters whose parameter is not an array", status, historyOf(`{"resource":{"resourceType":"Parameters","parameter":{"name":"subscription"}}}`)},
		{"a status Parameters whose part is not an array", status, historyOf(strings.Replace(statusAnswerEntry, `"part":[{"name":"event-number","valueString":"3"},{"name":"timestamp","valueInstant":"2026-11-13T10:00:00Z"},{"name":"focus"}]`, `"part":{"name":"focus"}`, 1))},
		{"a status Parameters with an unknown part", status, historyOf(strings.Replace(statusAnswerEntry, `{"name":"focus"}`, `{"name":"subject","valueString":"M123"}`, 1))},
		{"a status Parameters with a contained resource", status, historyOf(strings.Replace(statusAnswerEntry, `"parameter":[`, `"contained":[`+patientJSON+`],"parameter":[`, 1))},
		{"an interaction the check does not know", shnsdk.SubscriptionInteraction("patch"), answerSubscription},
	} {
		subs, r := AnswerSubscriptions([]byte(c.body), c.in, "sub-1", taskShape)
		wantRefusal(t, c.name, r, http.StatusBadGateway, recording.RefusalAuthority)
		if subs != nil {
			t.Errorf("%s: Subscriptions returned beside the refusal", c.name)
		}
		wantRefusal(t, c.name+" (CheckSubscribeAnswer)", CheckSubscribeAnswer([]byte(c.body), c.in, "sub-1", taskShape), http.StatusBadGateway, recording.RefusalAuthority)
	}
	// A $status with no id asked for names none.
	wantRefusal(t, "a $status with no id", CheckSubscribeAnswer([]byte(statusSearchset(statusAnswerEntry)), status, "", taskShape), http.StatusBadGateway, recording.RefusalAuthority)
}

// An answer that is not a 2xx: FHIR JSON passes only as an OperationOutcome
// that holds no resource; anything that is not FHIR JSON is carried.
func TestCheckErrorAnswer(t *testing.T) {
	const xmlOutcome = `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><severity value="error"/><code value="processing"/></issue></OperationOutcome>`
	const xmlPatient = `<?xml version="1.0" encoding="UTF-8"?><Patient xmlns="http://hl7.org/fhir"><id value="p1"/><name><family value="Doe"/></name></Patient>`
	for _, c := range []struct {
		name, body string
		refused    bool
	}{
		{"an OperationOutcome", answerOutcome, false},
		{"an OperationOutcome after a byte order mark and whitespace", "\xEF\xBB\xBF \n" + answerOutcome, false},
		// The reference provider's HAPI FHIR answer to an instance $status
		// it does not serve, as captured from it (400, application/fhir+json):
		// carried.
		{"the reference provider's not-supported answer to $status", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-supported","diagnostics":"Invalid request: The FHIR endpoint on this server does not know how to handle GET operation[Subscription/1716/$status] with parameters [[]]"}]}`, false},
		{"no body", "", false},
		{"whitespace only", " \r\n\t", false},
		{"proxy HTML", "<!DOCTYPE html><html><head><meta charset=utf-8></head><body>502 Bad Gateway</body></html>", false},
		{"well-formed XHTML", `<html xmlns="http://www.w3.org/1999/xhtml"><body>502</body></html>`, false},
		{"plain text", "upstream down", false},
		{"JSON naming no resource type", `{"error":"bad gateway"}`, false},
		{"a JSON array of resources", "[" + patientJSON + "]", true},
		{"a JSON object holding a resource", `{"error":"x","patient":` + patientJSON + `}`, true},
		{"a JSON array holding none", `[{"error":"x"}]`, false},
		{"UTF-16BE XML without a byte order mark", "\x00<\x00P", true},
		{"a DOCTYPE fixing the FHIR namespace", `<!DOCTYPE Patient [<!ATTLIST Patient xmlns CDATA #FIXED "http://hl7.org/fhir">]><Patient><name><family value="Doe"/></name></Patient>`, true},
		{"an html DOCTYPE page", "<!DOCTYPE html><html><body>502</body></html>", false},
		{"an XML OperationOutcome", xmlOutcome, false},
		{"an XML OperationOutcome by a prefix", `<f:OperationOutcome xmlns:f="http://hl7.org/fhir"><f:issue/></f:OperationOutcome>`, false},
		{"a Patient root in no namespace", `<Patient><name>Doe</name></Patient>`, true},
		{"an OperationOutcome root in no namespace", `<OperationOutcome><issue/></OperationOutcome>`, true},
		{"a patient root in lower case in no namespace", `<patient><name>Doe</name></patient>`, false},
		{"html in no namespace", `<html><body>502</body></html>`, false},
		{"an XML OperationOutcome with an element named contained below a child", `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><details><contained/></details></issue></OperationOutcome>`, true},
		{"an XML OperationOutcome holding a resource-type element deep", `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><details><Patient><name><family value="Doe"/></name></Patient></details></issue></OperationOutcome>`, true},
		{"an XML OperationOutcome holding a lower-case patient element", `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><details><patient value="x"/></details></issue></OperationOutcome>`, false},
		{"a JSON array holding a resource, then text", "[" + patientJSON + "] trailing", true},
		{"bracketed proxy text", "[error] upstream timed out", false},
		{"a JSON array holding none, then text", "[1,2] trailing", false},
		// A JSON object or array followed by more: the value judged alone,
		// and what follows refused only when it is FHIR JSON or JSON that
		// cannot be read.
		{"a JSON array of a status code, then its text", "[404] Not Found", false},
		{"a JSON error, then a PHP warning", phpWarningAfterJSON, false},
		{"an OperationOutcome, then a PHP warning", answerOutcome + phpWarning, false},
		{"an OperationOutcome holding a resource, then a PHP warning", `{"resourceType":"OperationOutcome","contained":[` + patientJSON + `],"issue":[]}` + phpWarning, true},
		{"a Patient, then text", patientJSON + " x", true},
		{"an OperationOutcome, then a Patient", answerOutcome + " " + patientJSON, true},
		{"a JSON error, then JSON that is not closed", `{"error":"x"} {"resourceType":"Patient"`, true},
		// An external DTD refuses only beside the FHIR namespace.
		{"Drupal's XHTML+RDFa error page", drupalErrorPage, false},
		{"an XHTML page naming its DTD by a relative system id", relativeDTDPage, false},
		{"a FHIR OperationOutcome under a DTD of its own", `<!DOCTYPE OperationOutcome SYSTEM "oo.dtd">` + xmlOutcome, true},
		// Every encoding that reads ASCII as ASCII is read; any other is
		// unknown.
		{"an XML error declared in ISO-8859-2", latin2Error, false},
		{"an XML error declared in Shift_JIS", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><error>\x83\x47\x83\x89\x81\x5b</error>", false},
		{"an XML OperationOutcome declared in windows-1250", `<?xml version="1.0" encoding="windows-1250"?>` + xmlOutcome, false},
		{"an XML OperationOutcome declared in ISO-8859-1 with a letter beyond ASCII", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><OperationOutcome xmlns=\"http://hl7.org/fhir\"><issue><severity value=\"error\"/><code value=\"processing\"/><diagnostics value=\"Ressource introuvable: r\xe9f\xe9rence\"/></issue></OperationOutcome>", false},
		{"an XML OperationOutcome declared in Shift_JIS with Japanese diagnostics", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><OperationOutcome xmlns=\"http://hl7.org/fhir\"><issue><severity value=\"error\"/><code value=\"processing\"/><diagnostics value=\"\x83\x47\x83\x89\x81\x5b\"/></issue></OperationOutcome>", false},
		{"an XML OperationOutcome declared in GB18030 holding a contained Patient", "<?xml version=\"1.0\" encoding=\"GB18030\"?><OperationOutcome xmlns=\"http://hl7.org/fhir\"><contained><Patient><name><family value=\"\x81\x30\x81\x30\"/></name></Patient></contained><issue/></OperationOutcome>", true},
		{"an XML error declared in UTF-16, in UTF-8's bytes", `<?xml version="1.0" encoding="UTF-16"?><error>down</error>`, true},
		{"an XML error declared in ISO-2022-JP", `<?xml version="1.0" encoding="ISO-2022-JP"?><error>down</error>`, true},
		{"a JSON array holding none, then one holding a resource", "[{}] [" + patientJSON + "]", true},
		{"a JSON array, then whitespace", "[1,2]\n", false},
		{"a Patient root in another namespace", `<Patient xmlns="urn:x"><name>Doe</name></Patient>`, true},
		{"a SOAP fault", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><soap:Fault/></soap:Body></soap:Envelope>`, false},
		{"an XML OperationOutcome with a resource deep", `<OperationOutcome xmlns="http://hl7.org/fhir"><issue><details><extension><Contained><Patient/></Contained></extension></details></issue></OperationOutcome>`, true},
		{"an OperationOutcome with a contained resource", strings.Replace(answerOutcome, `"issue"`, `"contained":[`+patientJSON+`],"issue"`, 1), true},
		{"an OperationOutcome with a resource in an issue", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"x","details":` + patientJSON + `}]}`, true},
		{"a Patient", patientJSON, true},
		{"a Patient after a byte order mark", "\xEF\xBB\xBF" + patientJSON, true},
		{"a Subscription", answerSubscription, true},
		{"a resourceType in another case", `{"ResourceType":"Patient","id":"p1"}`, true},
		{"an OperationOutcome read two ways", `{"resourceType":"OperationOutcome","resourceType":"Patient"}`, true},
		{"truncated FHIR JSON", `{"resourceType":"Patient","name":[{"family":"Doe"}]`, true},
		{"an XML Patient", xmlPatient, true},
		{"an XML OperationOutcome with a contained child", `<OperationOutcome xmlns="http://hl7.org/fhir"><contained><Patient><id value="p1"/></Patient></contained><issue/></OperationOutcome>`, true},
		{"an XML OperationOutcome with a contained child in another case", `<OperationOutcome xmlns="http://hl7.org/fhir"><Contained/><issue/></OperationOutcome>`, true},
		{"an XML OperationOutcome not well-formed", `<OperationOutcome xmlns="http://hl7.org/fhir"><issue>`, true},
		{"a JSON array holding a resource past the decoder's own nesting limit", strings.Repeat("[", 10001) + patientJSON + strings.Repeat("]", 10001), true},
		{"a FHIR Patient wrapped in an error element", `<error>` + xmlPatient[len(`<?xml version="1.0" encoding="UTF-8"?>`):] + `</error>`, true},
		{"a SOAP Body holding a FHIR Patient", `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><Patient xmlns="http://hl7.org/fhir"><name><family value="Doe"/></name></Patient></soap:Body></soap:Envelope>`, true},
		{"an error naming an Endpoint in no namespace", `<Error><Code>PermanentRedirect</Code><Message>use the endpoint</Message><Endpoint>bucket.s3.example</Endpoint></Error>`, false},
		{"a Patient element in no namespace under an error", `<error><Patient><name>Doe</name></Patient></error>`, false},
		{"a number, then a resource", `1 ` + patientJSON, true},
		{"true, then an array holding a resource", `true [` + patientJSON + `]`, true},
		{"a status line", "404 Not Found", false},
		{"a number, then JSON naming no resource", `404 {"error":"x"}`, false},
		{"text, then a resource", "Error: " + patientJSON, false},
	} {
		r := CheckErrorAnswer([]byte(c.body))
		if !c.refused {
			if r != nil {
				t.Errorf("%s: refused %+v, want carried", c.name, r)
			}
			continue
		}
		wantRefusal(t, c.name, r, http.StatusBadGateway, recording.RefusalAuthority)
	}
}

// A create's or an update's Subscription holds no resource: a contained
// resource of any type, or a resource-valued member anywhere, is refused 403
// on authority, decided by structure; a clean Subscription passes.
func TestCheckNoResource(t *testing.T) {
	if r := CheckNoResource(mustSubscription(t, baseSubscription)); r != nil {
		t.Fatalf("a clean Subscription: %+v", r)
	}
	for name, body := range map[string]string{
		"a contained Patient":            edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "contained": [`+patientJSON+`],`),
		"a contained Organization":       edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "contained": [{"resourceType":"Organization","id":"o1"}],`),
		"a contained of no type":         edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "contained": [{"id":"o1"}],`),
		"a contained in another case":    edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "Contained": [{"id":"o1"}],`),
		"a resource-valued member":       edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "note": `+patientJSON+`,`),
		"a resource deep in the channel": edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "x": [{"y": {"resourceType": "Practitioner"}}],`),
		"a resourceType in another case": edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "note": {"RESOURCETYPE": "Patient"},`),
	} {
		wantRefusal(t, name, CheckNoResource(mustSubscription(t, body)), http.StatusForbidden, recording.RefusalAuthority)
	}
}

// A 2xx that is a JSON object or array, after any run of scalars, followed
// by anything is refused as it always was: not one JSON object this network
// can read unambiguously, whatever its first value.
func TestCheckSubscribeAnswer_AValueFollowedByMoreIsNotOneObject(t *testing.T) {
	for _, body := range []string{`{"a":1} x`, `[{"a":1}] x`, "[404] Not Found", `{"a":1} {"b":2}`, `1 {"a":1} x`, answerSubscription + " x", answerSubscription + phpWarning} {
		r := CheckSubscribeAnswer([]byte(body), shnsdk.SubscriptionRead, "sub-1", taskShape)
		wantRefusal(t, body, r, http.StatusBadGateway, recording.RefusalAuthority)
		if r != nil && !strings.Contains(r.Reason, "not one JSON object this network can read unambiguously") {
			t.Errorf("%q: %s", body, r.Reason)
		}
	}
}

// brProviderAck is the reference PAS subscriber's answer to a notification
// that carries no ClaimResponse (a handshake, a heartbeat, an empty event),
// HAPI FHIR's rest-hook convention: the response to the transaction the
// notification is delivered as.
const brProviderAck = `{"resourceType":"Bundle","type":"transaction-response","entry":[]}`

// A no-content notification's answer holds no resource (N5): CheckErrorAnswer's
// rule, and an acknowledgement Bundle too, a transaction-response or
// batch-response whose entries hold only their response, any outcome an
// OperationOutcome holding none. Anything holding a resource is refused 502
// on authority, as is another Bundle type.
func TestCheckSignalAnswer(t *testing.T) {
	const xmlAck = `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><status value="200 OK"/><outcome><OperationOutcome><issue><severity value="information"/><code value="informational"/></issue></OperationOutcome></outcome></response></entry></Bundle>`
	outcome := `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}`
	for _, c := range []struct {
		name, body string
		refused    bool
	}{
		{"the reference subscriber's acknowledgement", brProviderAck, false},
		{"a transaction-response with no entry", `{"resourceType":"Bundle","type":"transaction-response"}`, false},
		{"a transaction-response whose entries hold their response", `{"resourceType":"Bundle","id":"r1","type":"transaction-response","link":[{"relation":"self","url":"https://x.test/fhir"}],"entry":[{"response":{"status":"200 OK","location":"Bundle/b1/_history/1","etag":"W/\"1\"","lastModified":"2026-11-19T00:00:00Z"}}]}`, false},
		{"a batch-response", `{"resourceType":"Bundle","type":"batch-response","entry":[{"response":{"status":"201 Created"}}]}`, false},
		{"an entry's response outcome, an OperationOutcome holding none", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","outcome":` + outcome + `}}]}`, false},
		{"member names in another case", `{"resourceType":"Bundle","Type":"transaction-response","Entry":[{"Response":{"status":"200 OK"}}]}`, false},
		{"an XML acknowledgement with an outcome", xmlAck, false},
		{"an XML batch-response", `<Bundle xmlns="http://hl7.org/fhir"><type value="batch-response"/><entry><response><status value="200 OK"/></response></entry></Bundle>`, false},
		// What CheckErrorAnswer carries, carried too.
		{"an OperationOutcome", answerOutcome, false},
		{"no body", "", false},
		{"proxy HTML", "<!DOCTYPE html><html><body>502 Bad Gateway</body></html>", false},
		// Refused: a resource anywhere, or another Bundle.
		{"a resource in an entry", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"resource":` + patientJSON + `,"response":{"status":"200 OK"}}]}`, true},
		{"an entry holding more than its response", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"fullUrl":"https://x.test/fhir/Patient/p1","response":{"status":"200 OK"}}]}`, true},
		{"a contained resource", `{"resourceType":"Bundle","type":"transaction-response","contained":[` + patientJSON + `],"entry":[]}`, true},
		{"a resource outside the entries", `{"resourceType":"Bundle","type":"transaction-response","signature":{"who":` + patientJSON + `},"entry":[]}`, true},
		{"an outcome holding a resource", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","outcome":{"resourceType":"OperationOutcome","contained":[` + patientJSON + `],"issue":[]}}}]}`, true},
		{"an outcome that is not an OperationOutcome", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","outcome":` + patientJSON + `}}]}`, true},
		{"a response holding a resource elsewhere", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","extension":[{"url":"x","valueReference":{"x":` + patientJSON + `}}]}}]}`, true},
		{"an entry's response not an object", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":"200 OK"}]}`, true},
		{"a searchset", `{"resourceType":"Bundle","type":"searchset","entry":[]}`, true},
		{"a Bundle of no type", `{"resourceType":"Bundle","entry":[]}`, true},
		{"a collection holding a Patient", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":` + patientJSON + `}]}`, true},
		{"a Bundle read two ways", `{"resourceType":"Bundle","type":"transaction-response","type":"searchset"}`, true},
		{"an XML entry with a resource", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><resource><Patient><id value="p1"/></Patient></resource><response><status value="200 OK"/></response></entry></Bundle>`, true},
		{"an XML entry holding more than its response", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><fullUrl value="https://x.test/fhir/Patient/p1"/><response><status value="200 OK"/></response></entry></Bundle>`, true},
		{"an XML outcome holding a contained Patient", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><outcome><OperationOutcome><contained><Patient/></contained><issue/></OperationOutcome></outcome></response></entry></Bundle>`, true},
		{"an XML outcome that is a Patient", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><outcome><Patient><id value="p1"/></Patient></outcome></response></entry></Bundle>`, true},
		{"an XML searchset", `<Bundle xmlns="http://hl7.org/fhir"><type value="searchset"/></Bundle>`, true},
		{"an XML Bundle of no type", `<Bundle xmlns="http://hl7.org/fhir"><entry><response><status value="200 OK"/></response></entry></Bundle>`, true},
		{"an XML Bundle with a resource outside its entries", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><signature><who><Patient/></who></signature></Bundle>`, true},
		{"an XML Bundle not well-formed", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry>`, true},
		{"a Patient", patientJSON, true},
		// A contained member is a resource whatever it holds, read the same
		// in JSON as in XML.
		{"a contained member at the root, holding no resourceType", `{"resourceType":"Bundle","type":"transaction-response","contained":[{"id":"p1","name":[{"family":"Doe"}]}],"entry":[]}`, true},
		{"an empty contained member in another case", `{"resourceType":"Bundle","type":"transaction-response","Contained":[],"entry":[]}`, true},
		{"a contained member in a response", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK","contained":[{"id":"p1"}]}}]}`, true},
		{"an XML contained element at the root", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><contained/></Bundle>`, true},
		{"entry as an object holding a Patient", `{"resourceType":"Bundle","type":"transaction-response","entry":{"resource":` + patientJSON + `}}`, true},
		{"an XML Bundle with two types", `<Bundle xmlns="http://hl7.org/fhir"><type value="searchset"/><type value="transaction-response"/></Bundle>`, true},
		{"an XML type in another namespace", `<Bundle xmlns="http://hl7.org/fhir"><x:type xmlns:x="urn:x" value="transaction-response"/></Bundle>`, true},
		{"an XML type whose value is in another namespace", `<Bundle xmlns="http://hl7.org/fhir"><type xmlns:x="urn:x" x:value="transaction-response"/></Bundle>`, true},
		{"an XML type nested below the root", `<Bundle xmlns="http://hl7.org/fhir"><type value="searchset"/><entry><response><type value="transaction-response"/></response></entry></Bundle>`, true},
		{"an XML response in another namespace", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><x:response xmlns:x="urn:x"><status value="200 OK"/></x:response></entry></Bundle>`, true},
		{"an XML OperationOutcome nested in an outcome's OperationOutcome", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><outcome><OperationOutcome><issue><details><OperationOutcome/></details></issue></OperationOutcome></outcome></response></entry></Bundle>`, true},
		{"an XML outcome's OperationOutcome in another namespace", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><outcome><OperationOutcome xmlns="urn:x"/></outcome></response></entry></Bundle>`, true},
		// Decided by structure: a reference is carried as written, whatever
		// it names, as an OperationOutcome's diagnostics are.
		{"a response location naming a Patient", `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"201 Created","location":"Patient/p1/_history/1","etag":"W/\"1\""}}]}`, false},
		{"an XML response location naming a Patient", `<Bundle xmlns="http://hl7.org/fhir"><type value="transaction-response"/><entry><response><status value="201 Created"/><location value="Patient/p1/_history/1"/></response></entry></Bundle>`, false},
		// Only the root's own type is the Bundle's: a nested one is neither
		// read as it nor counted beside it.
		{"an XML acknowledgement with a nested identifier type", `<Bundle xmlns="http://hl7.org/fhir"><identifier><type><text value="x"/></type></identifier><type value="transaction-response"/></Bundle>`, false},
		{"an XML Bundle whose only type is nested", `<Bundle xmlns="http://hl7.org/fhir"><identifier><type value="transaction-response"/></identifier></Bundle>`, true},
		{"an XML acknowledgement by a prefix", `<f:Bundle xmlns:f="http://hl7.org/fhir"><f:type value="transaction-response"/><f:entry><f:response><f:status value="200 OK"/></f:response></f:entry></f:Bundle>`, false},
	} {
		r := CheckSignalAnswer([]byte(c.body))
		if !c.refused {
			if r != nil {
				t.Errorf("%s: refused %+v, want carried", c.name, r)
			}
			continue
		}
		wantRefusal(t, c.name, r, http.StatusBadGateway, recording.RefusalAuthority)
	}
}

// Every answer that is not a Bundle reads exactly as CheckErrorAnswer reads
// it: the acknowledgement is the one thing CheckSignalAnswer adds.
func TestCheckSignalAnswerIsCheckErrorAnswerButForTheAcknowledgement(t *testing.T) {
	for _, body := range []string{answerOutcome, "", "upstream down", patientJSON, answerSubscription, `{"error":"x"}`, "[" + patientJSON + "]",
		`<OperationOutcome xmlns="http://hl7.org/fhir"><issue/></OperationOutcome>`, `<Patient xmlns="http://hl7.org/fhir"/>`, `{"resourceType":"OperationOutcome","contained":[` + patientJSON + `],"issue":[]}`} {
		if got, want := CheckSignalAnswer([]byte(body)), CheckErrorAnswer([]byte(body)); (got == nil) != (want == nil) {
			t.Errorf("%q: CheckSignalAnswer %+v, CheckErrorAnswer %+v", body, got, want)
		}
	}
	if CheckErrorAnswer([]byte(brProviderAck)) == nil {
		t.Fatal("control: CheckErrorAnswer carries the acknowledgement, so the row above cannot tell the two apart")
	}
}
