package subscription

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

// A Subscription is read only as FHIR JSON: any other media type, or none, is
// 415; a body that is not one unambiguous JSON Subscription is 400; both are
// the fidelity of SHN's own edit.
func TestParseSubscription(t *testing.T) {
	for _, ct := range []string{"application/fhir+json", "application/json", "application/fhir+json; charset=UTF-8; fhirVersion=4.0", "Application/FHIR+JSON"} {
		if _, r := ParseSubscription([]byte(baseSubscription), ct); r != nil {
			t.Errorf("content type %q: %+v", ct, r)
		}
	}
	for _, ct := range []string{"", "application/fhir+xml", "application/xml", "text/plain", "application/json+fhir", "not a media type;;"} {
		_, r := ParseSubscription([]byte(baseSubscription), ct)
		wantRefusal(t, "content type "+ct, r, http.StatusUnsupportedMediaType, recording.RefusalFidelity)
	}
	for name, body := range map[string]string{
		"not JSON":                            `<Subscription/>`,
		"two values":                          baseSubscription + `{}`,
		"a repeated member":                   edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "status": "active",`),
		"a member repeated in case":           edit(t, baseSubscription, `"reason": "decisions",`, `"reason": "decisions", "Reason": "x",`),
		"a member repeated through an escape": edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "st\u0061tus": "active",`),
		"a repeated channel member":           edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "type": "websocket",`),
		"invalid UTF-8":                       edit(t, baseSubscription, `decisions`, "decisions\xff"),
		"another resource":                    edit(t, baseSubscription, `"resourceType": "Subscription"`, `"resourceType": "Task"`),
		"no resourceType":                     edit(t, baseSubscription, `"resourceType": "Subscription",`, ``),
		"an array":                            `[` + baseSubscription + `]`,
		"a string":                            `"Subscription"`,
		"nested past the depth bound":         `{"resourceType":"Subscription","x":` + strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2) + `}`,
	} {
		_, r := ParseSubscription([]byte(body), "application/fhir+json")
		wantRefusal(t, name, r, http.StatusBadRequest, recording.RefusalFidelity)
	}
}

// The closed shape: each rule's refusal, 403 authority, beside its
// control row.
func TestCheckShape(t *testing.T) {
	const backportProfile = `"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription"`
	const content = `"valueCode": "full-resource"`
	const heartbeat = `{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-heartbeat-period", "valueUnsignedInt": 60}`
	accepted := map[string]string{
		"the control":                         baseSubscription,
		"rule 1: the canonical with |version": edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "http://example.org/SubscriptionTopic/test|1.0.0"`),
		"rule 1: the profile with |version":   edit(t, baseSubscription, backportProfile, `"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscription|1.1.0"`),
		"rule 2: id-only":                     edit(t, baseSubscription, content, `"valueCode": "id-only"`),
		"rule 4: the topic's own extension":   edit(t, baseSubscription, heartbeat, heartbeat+`, {"url": "`+testTopicExt+`", "valueString": "x"}`),
		"rule 4: the topic's own, top level":  edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "extension": [{"url": "`+testTopicExt+`", "valueString": "x"}],`),
		"rule 4: every Backport extension in its place": edit(t, baseSubscription, heartbeat, heartbeat+
			`, {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-timeout", "valueUnsignedInt": 30}`+
			`, {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-max-count", "valuePositiveInt": 5}`),
	}
	for name, body := range accepted {
		if r := CheckShape(mustSubscription(t, body), topic, subscriberA); r != nil {
			t.Errorf("%s: refused %+v", name, r)
		}
	}
	refused := map[string]string{
		// Rule 1.
		"rule 1: no backport profile":        edit(t, baseSubscription, backportProfile, `"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-subscription"`),
		"rule 1: no meta":                    edit(t, baseSubscription, `"meta": {`, `"metaX": {`),
		"rule 1: a profile that is a prefix": edit(t, baseSubscription, backportProfile, `"http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-subscriptionX"`),
		"rule 1: the wrong canonical":        edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "http://example.org/SubscriptionTopic/other"`),
		"rule 1: a canonical prefix":         edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "http://example.org/SubscriptionTopic/test2"`),
		"rule 1: an empty version":           edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "http://example.org/SubscriptionTopic/test|"`),
		"rule 1: a query after the canonical": edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`,
			`"criteria": "http://example.org/SubscriptionTopic/test?org-identifier=1234567893"`),
		"rule 1: a query after the version": edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "http://example.org/SubscriptionTopic/test|1?x=1"`),
		"rule 1: a classic criteria":        edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test"`, `"criteria": "ClaimResponse?status=active"`),
		"rule 1: no criteria":               edit(t, baseSubscription, `"criteria": "http://example.org/SubscriptionTopic/test",`, ``),
		// Rule 2.
		"rule 2: a websocket channel":    edit(t, baseSubscription, `"type": "rest-hook"`, `"type": "websocket"`),
		"rule 2: an email channel":       edit(t, baseSubscription, `"type": "rest-hook"`, `"type": "email"`),
		"rule 2: no channel type":        edit(t, baseSubscription, `"type": "rest-hook",`, ``),
		"rule 2: an XML payload":         edit(t, baseSubscription, `"payload": "application/fhir+json"`, `"payload": "application/fhir+xml"`),
		"rule 2: a JSON payload":         edit(t, baseSubscription, `"payload": "application/fhir+json"`, `"payload": "application/json"`),
		"rule 2: no payload":             edit(t, baseSubscription, `"payload": "application/fhir+json",`, ``),
		"rule 2: empty content":          edit(t, baseSubscription, content, `"valueCode": "empty"`),
		"rule 2: an unknown content":     edit(t, baseSubscription, content, `"valueCode": "everything"`),
		"rule 2: no content":             edit(t, baseSubscription, `"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content"`, `"url": "`+testTopicExt+`"`),
		"rule 2: a content with no code": edit(t, baseSubscription, content, `"valueString": "full-resource"`),
		"rule 2: two contents": edit(t, baseSubscription, `"valueCode": "full-resource"}]`,
			`"valueCode": "full-resource"}, {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content", "valueCode": "id-only"}]`),
		// Rule 4.
		"rule 4: an extension outside the allowlist": edit(t, baseSubscription, heartbeat, heartbeat+`, {"url": "https://hapifhir.io/fhir/StructureDefinition/subscription-payload-search-criteria", "valueString": "Patient?"}`),
		"rule 4: an unknown top-level extension":     edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "extension": [{"url": "http://example.org/x", "valueString": "x"}],`),
		"rule 4: a Backport extension out of place":  edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "extension": [`+heartbeat+`],`),
		"rule 4: a modifierExtension, top level":     edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "modifierExtension": [{"url": "`+testTopicExt+`", "valueString": "x"}],`),
		"rule 4: a modifierExtension in channel":     edit(t, baseSubscription, `"type": "rest-hook",`, `"type": "rest-hook", "modifierExtension": [],`),
		"rule 4: a modifierExtension inside an allowed extension": edit(t, baseSubscription, heartbeat,
			`{"url": "`+testTopicExt+`", "modifierExtension": [{"url": "x"}]}`),
		"rule 4: an extension on an element":                 edit(t, baseSubscription, `"status": "requested",`, `"status": "requested", "_status": {"extension": [{"url": "http://example.org/x"}]},`),
		"rule 4: an extension not an array":                  edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "extension": {"url": "`+testTopicExt+`"},`),
		"rule 4: an extension in contained":                  edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "contained": [{"resourceType": "Basic", "extension": [{"url": "http://example.org/x"}]}],`),
		"rule 4: a valueString beside the valueCode content": edit(t, baseSubscription, content, `"valueString": "id-only", "valueCode": "full-resource"`),
		"rule 2: a content whose code is not a string":       edit(t, baseSubscription, content, `"valueCode": 1`),
		"rule 4: a heartbeat with a second value":            edit(t, baseSubscription, `"valueUnsignedInt": 60}`, `"valueUnsignedInt": 60, "valueInteger": 1}`),
		"rule 4: a heartbeat whose value is a string":        edit(t, baseSubscription, `"valueUnsignedInt": 60}`, `"valueUnsignedInt": "60"}`),
		"rule 4: a heartbeat with an _value":                 edit(t, baseSubscription, `"valueUnsignedInt": 60}`, `"valueUnsignedInt": 60, "_valueUnsignedInt": {"extension": []}}`),
		"rule 4: a heartbeat with a nested extension":        edit(t, baseSubscription, `"valueUnsignedInt": 60}`, `"valueUnsignedInt": 60, "extension": [{"url": "x"}]}`),
		"rule 4: a timeout with a second value": edit(t, baseSubscription, heartbeat, heartbeat+
			`, {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-timeout", "valueUnsignedInt": 30, "valueString": "x"}`),
		"rule 4: a max-count with another value type": edit(t, baseSubscription, heartbeat, heartbeat+
			`, {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-max-count", "valueUnsignedInt": 5}`),
		"rule 4: a filter with a second value":                  edit(t, baseSubscription, `"valueString": "org-identifier=1234567893"}`, `"valueUri": "org-identifier=9999999999", "valueString": "org-identifier=1234567893"}`),
		"rule 4: a modifierExtension spelled in another case":   edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "ModifierExtension": [],`),
		"rule 4: an extension spelled in another case":          edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "Extension": [{"url": "http://example.org/x"}],`),
		"rule 4: a modifierExtension spelled by a Unicode fold": edit(t, baseSubscription, `"id": "sub-1",`, `"id": "sub-1", "modifierExtenſion": [],`),
		"rule 2: a _payload that is not an object": edit(t, edit(t, baseSubscription, `"_payload": {`, `"_payload": [{`), `"valueCode": "full-resource"}]
    },`, `"valueCode": "full-resource"}]
    }],`),
		// Rule 3, through the topic.
		"rule 3: an identifier not attested": edit(t, baseSubscription, `org-identifier=1234567893`, `org-identifier=1111111111`),
		"rule 3: another parameter":          edit(t, baseSubscription, `org-identifier=1234567893`, `org-identifier=1234567893&patient=1`),
		"rule 3: a malformed filter":         edit(t, baseSubscription, `org-identifier=1234567893`, `org-identifier=1234567893,`),
	}
	// Rule 3: no filter at all.
	refused["rule 3: no filter"] = edit(t, baseSubscription, `"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": "org-identifier=1234567893"`,
		`"url": "`+testTopicExt+`", "valueString": "x"`)
	for name, body := range refused {
		wantRefusal(t, name, CheckShape(mustSubscription(t, body), topic, subscriberA), http.StatusForbidden, recording.RefusalAuthority)
	}
	// A _payload or its extension list that is not what FHIR writes is
	// refused as such, never read as no content.
	for name, row := range map[string]struct{ body, reason string }{
		"a _payload array": {edit(t, edit(t, baseSubscription, `"_payload": {`, `"_payload": [{`), `"valueCode": "full-resource"}]
    },`, `"valueCode": "full-resource"}]
    }],`), "channel._payload is not an object"},
		"a _payload.extension object": {edit(t, baseSubscription, `"extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content", "valueCode": "full-resource"}]`,
			`"extension": {"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content", "valueCode": "full-resource"}`), "channel._payload.extension is not an array"},
	} {
		r := CheckShape(mustSubscription(t, row.body), topic, subscriberA)
		if r == nil || !strings.Contains(r.Reason, row.reason) {
			t.Errorf("%s: %+v, want a refusal naming %q", name, r, row.reason)
		}
	}
	// empty is never carried, even for a topic that would list it.
	wantRefusal(t, "rule 2: empty, a topic listing it", CheckShape(mustSubscription(t, edit(t, baseSubscription, content, `"valueCode": "empty"`)), testTopic{listsEmpty: true}, subscriberA),
		http.StatusForbidden, recording.RefusalAuthority)
	// The CDex-like topic carries id-only alone.
	cdex := baseSubscription
	wantRefusal(t, "a CDex-like topic, full-resource", CheckShape(mustSubscription(t, cdex), cdexTopic, subscriberA), http.StatusForbidden, recording.RefusalAuthority)
	if r := CheckShape(mustSubscription(t, edit(t, cdex, content, `"valueCode": "id-only"`)), cdexTopic, subscriberA); r != nil {
		t.Errorf("a CDex-like topic, id-only: %+v", r)
	}
}

// ParseFilters reads both forms, comma-separated values, modifiers and
// tokens, and refuses what cannot be read one way only.
func TestParseFilters(t *testing.T) {
	filtered := func(values ...string) *Subscription {
		exts := make([]string, len(values))
		for i, v := range values {
			exts[i] = `{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": ` + string(jsonString(v)) + `}`
		}
		return mustSubscription(t, edit(t, baseSubscription,
			`[{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": "org-identifier=1234567893"}]`,
			"["+strings.Join(exts, ",")+"]"))
	}
	for name, row := range map[string]struct {
		values []string
		want   []FilterParam
	}{
		"the bare form":    {[]string{"org-identifier=1234567893"}, []FilterParam{{Name: "org-identifier", Values: []FilterValue{{Value: "1234567893"}}}}},
		"the query form":   {[]string{"Bundle?org-identifier=1234567893"}, []FilterParam{{Name: "org-identifier", Values: []FilterValue{{Value: "1234567893"}}}}},
		"a token":          {[]string{"org-identifier=http://hl7.org/fhir/sid/us-npi|1234567893"}, []FilterParam{{Name: "org-identifier", Values: []FilterValue{{System: "http://hl7.org/fhir/sid/us-npi", HasSystem: true, Value: "1234567893"}}}}},
		"the empty system": {[]string{"org-identifier=|1234567893"}, []FilterParam{{Name: "org-identifier", Values: []FilterValue{{HasSystem: true, Value: "1234567893"}}}}},
		"comma-separated":  {[]string{"org-identifier=a,s|b,|c"}, []FilterParam{{Name: "org-identifier", Values: []FilterValue{{Value: "a"}, {System: "s", HasSystem: true, Value: "b"}, {HasSystem: true, Value: "c"}}}}},
		"a modifier":       {[]string{"Task?requester:identifier=s|p&status=requested"}, []FilterParam{{Name: "requester", Modifier: "identifier", Values: []FilterValue{{System: "s", HasSystem: true, Value: "p"}}}, {Name: "status", Values: []FilterValue{{Value: "requested"}}}}},
		"two extensions":   {[]string{"a=1", "b=2"}, []FilterParam{{Name: "a", Values: []FilterValue{{Value: "1"}}}, {Name: "b", Values: []FilterValue{{Value: "2"}}}}},
		"a chained name":   {[]string{"owner.identifier=x"}, []FilterParam{{Name: "owner.identifier", Values: []FilterValue{{Value: "x"}}}}},
		"no filter":        {nil, []FilterParam{}},
	} {
		got, r := ParseFilters(filtered(row.values...))
		if r != nil || !slices.EqualFunc(got, row.want, func(a, b FilterParam) bool {
			return a.Name == b.Name && a.Modifier == b.Modifier && slices.Equal(a.Values, b.Values)
		}) {
			t.Errorf("%s: %+v, %+v; want %+v", name, got, r, row.want)
		}
	}
	for name, v := range map[string]string{
		"no =":                        "org-identifier",
		"an empty value":              "org-identifier=",
		"an empty value in a list":    "org-identifier=a,,b",
		"a trailing comma":            "org-identifier=a,",
		"a system and no value":       "org-identifier=s|",
		"only a bar":                  "org-identifier=|",
		"two bars":                    "org-identifier=s|a|b",
		"an empty name":               "=a",
		"an empty modifier":           "org-identifier:=a",
		"an empty parameter":          "a=1&",
		"two question marks":          "Bundle?a=1?b=2",
		"a lower-case resource type":  "bundle?a=1",
		"a question mark alone":       "?a=1",
		"no parameter after the type": "Bundle?",
		"empty":                       "",
		"a percent escape":            "org-identifier=%31234567893",
		"a backslash escape":          `org-identifier=a\,b`,
		"a space":                     "org-identifier=a b",
		"a control character":         "org-identifier=a\tb",
		"a name with a space":         "org id=a",
		"a modifier with a colon":     "a:b:c=1",
	} {
		_, r := ParseFilters(filtered(v))
		wantRefusal(t, name, r, http.StatusForbidden, recording.RefusalAuthority)
	}
	_, r := ParseFilters(mustSubscription(t, edit(t, baseSubscription, `"valueString": "org-identifier=1234567893"`, `"valueCode": "org-identifier=1234567893"`)))
	wantRefusal(t, "no valueString", r, http.StatusForbidden, recording.RefusalAuthority)
	// Unknown is not empty: a criteria element or extension list that is
	// present but not readable is refused, never read as no filter.
	const criteriaExtensions = `[{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": "org-identifier=1234567893"}]`
	for name, body := range map[string]string{
		"_criteria.extension not an array": edit(t, baseSubscription, criteriaExtensions, `"x"`),
		"_criteria.extension an object":    edit(t, baseSubscription, criteriaExtensions, `{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria", "valueString": "org-identifier=1"}`),
		"_criteria not an object": edit(t, baseSubscription, `"_criteria": {
    "extension": `+criteriaExtensions+`
  }`, `"_criteria": [{"extension": `+criteriaExtensions+`}]`),
		"a criteria extension not an object": edit(t, baseSubscription, criteriaExtensions, `["org-identifier=1"]`),
	} {
		got, r := ParseFilters(mustSubscription(t, body))
		wantRefusal(t, name, r, http.StatusForbidden, recording.RefusalAuthority)
		if got != nil {
			t.Errorf("%s: read %+v", name, got)
		}
	}
	// No _criteria, or a _criteria with no extension, is no filter.
	for name, body := range map[string]string{
		"no _criteria":            edit(t, baseSubscription, `"_criteria": {`, `"_criteriaX": {`),
		"_criteria, no extension": edit(t, baseSubscription, `"extension": `+criteriaExtensions, `"id": "c"`),
	} {
		if got, r := ParseFilters(mustSubscription(t, body)); r != nil || got == nil || len(got) != 0 {
			t.Errorf("%s: %+v, %+v; want no filter", name, got, r)
		}
	}
	// A filter extension carrying a second value[x]: a reader that keeps the
	// first it meets would read the valueUri.
	twoValues := edit(t, baseSubscription, `"valueString": "org-identifier=1234567893"}`, `"valueUri": "org-identifier=9999999999", "valueString": "org-identifier=1234567893"}`)
	_, r = ParseFilters(mustSubscription(t, twoValues))
	wantRefusal(t, "a valueUri beside the valueString filter", r, http.StatusForbidden, recording.RefusalAuthority)
	// A plus sign, which a URL decoder reads as a space.
	_, r = ParseFilters(filtered("org-identifier=123+4567893"))
	wantRefusal(t, "a plus sign in the token", r, http.StatusForbidden, recording.RefusalAuthority)
	_, r = ParseFilters(filtered("org-identifier=http://hl7.org/fhir/sid/us-npi|1234567893+"))
	wantRefusal(t, "a plus sign after a system", r, http.StatusForbidden, recording.RefusalAuthority)
}

// The filter matcher (N4).
func TestMatchIdentifiers(t *testing.T) {
	const npiSys, otherSys = "http://hl7.org/fhir/sid/us-npi", "urn:oid:2.16.840.1.113883.4.4"
	one := []Identifier{{npiSys, "1234567893"}}
	two := []Identifier{{npiSys, "1234567893"}, {otherSys, "987654321"}}
	for name, row := range map[string]struct {
		values   []FilterValue
		attested []Identifier
		ok       bool
	}{
		"a bare attested value":                           {[]FilterValue{{Value: "1234567893"}}, one, true},
		"an explicit matching pair":                       {[]FilterValue{{System: npiSys, HasSystem: true, Value: "1234567893"}}, one, true},
		"a foreign system":                                {[]FilterValue{{System: otherSys, HasSystem: true, Value: "1234567893"}}, one, false},
		"the empty system":                                {[]FilterValue{{HasSystem: true, Value: "1234567893"}}, one, false},
		"a bare unattested value":                         {[]FilterValue{{Value: "1111111111"}}, one, false},
		"two systems: the first's own":                    {[]FilterValue{{System: npiSys, HasSystem: true, Value: "1234567893"}}, two, true},
		"two systems: the second's own":                   {[]FilterValue{{System: otherSys, HasSystem: true, Value: "987654321"}}, two, true},
		"two systems: the first's value under the second": {[]FilterValue{{System: otherSys, HasSystem: true, Value: "1234567893"}}, two, false},
		"two systems: the second's value under the first": {[]FilterValue{{System: npiSys, HasSystem: true, Value: "987654321"}}, two, false},
		"two systems: both bare":                          {[]FilterValue{{Value: "1234567893"}, {Value: "987654321"}}, two, true},
		"one of two unattested":                           {[]FilterValue{{Value: "1234567893"}, {Value: "1111111111"}}, two, false},
		"no value":                                        {nil, one, false},
		"nothing attested":                                {[]FilterValue{{Value: "1234567893"}}, nil, false},
		"the empty system, attested":                      {[]FilterValue{{HasSystem: true, Value: "x"}}, []Identifier{{"", "x"}}, true},
	} {
		r := MatchIdentifiers(row.values, row.attested)
		if row.ok && r != nil {
			t.Errorf("%s: refused %+v", name, r)
		}
		if !row.ok {
			wantRefusal(t, name, r, http.StatusForbidden, recording.RefusalAuthority)
		}
	}
}

// The endpoint rule.
func TestCheckEndpoint(t *testing.T) {
	isParticipant := func(s string) bool { return s == "provider-1" }
	isTopic := func(s string) bool { return s == "test" }
	loop := func(s string) bool { return LooksLikeNotificationAddress(s, isParticipant, isTopic) }
	for _, ep := range []string{subEndpoint, "https://ehr.example", "https://ehr.example:8443/hook", "HTTPS://ehr.example/hook"} {
		if r := CheckEndpoint(ep, loop); r != nil {
			t.Errorf("%q: refused %+v", ep, r)
		}
	}
	for name, ep := range map[string]string{
		"http":              "http://ehr.example/hook",
		"no scheme":         "//ehr.example/hook",
		"relative":          "/api/pas/notification",
		"no host":           "https:///hook",
		"opaque":            "https:ehr.example",
		"another scheme":    "wss://ehr.example/hook",
		"userinfo":          "https://user:pw@ehr.example/hook",
		"a user alone":      "https://user@ehr.example/hook",
		"a fragment":        "https://ehr.example/hook#f",
		"an empty fragment": "https://ehr.example/hook#",
		"empty":             "",
		"not a URL":         "https://ehr example/%zz",
		"a space":           "https://ehr.example/a b",
		"a trailing space":  "https://ehr.example/hook ",
		"a backslash":       `https://ehr.example/a\b`,
		// url.Parse takes a backslash in a query; the endpoint rule does not.
		"a backslash in the query": `https://ehr.example/hook?a=b\c`,
	} {
		wantRefusal(t, name, CheckEndpoint(ep, loop), http.StatusUnprocessableEntity, recording.RefusalIntegrity)
	}
	for name, ep := range map[string]string{
		"a notification address":                        notifierIngress + "/notify/test/provider-1/ocurkBVBmv4/NDl",
		"a notification address, any host":              "https://elsewhere.example/x/notify/test/provider-1/ocurkBVBmv4/NDl?q=1",
		"a notification address behind escaped slashes": "https://elsewhere.example/notify%2Ftest%2Fprovider-1%2FocurkBVBmv4%2FNDl",
	} {
		wantRefusal(t, name, CheckEndpoint(ep, loop), http.StatusUnprocessableEntity, recording.RefusalRouting)
	}
	wantRefusal(t, "no loop rule", CheckEndpoint(subEndpoint, nil), http.StatusUnprocessableEntity, recording.RefusalRouting)
}

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The search rule, named parameter by named parameter.
func TestCheckSearch(t *testing.T) {
	for _, raw := range []string{
		"url=https://x",
		"url=https://x&status=requested,active",
		"url=https://x&_id=sub-1",
		"url=https://x&_count=10",
		"url=https://x&_elements=channel",
		"url=https://x&_elements=id,channel,status",
		"url=https://x&status=active&_id=a&_count=5&_elements=channel",
	} {
		if r := CheckSearch(query(t, raw)); r != nil {
			t.Errorf("%q: refused %+v", raw, r)
		}
	}
	// A modifier is refused as a modifier, before the parameter's name is
	// read.
	for _, raw := range []string{"url:below=a", "url:above=a", "url:missing=false", "url=a&status:not=off", "url=a&_id:exact=x"} {
		if r := CheckSearch(query(t, raw)); r == nil || !strings.Contains(r.Reason, "modifier") {
			t.Errorf("%s: %+v, want a refusal naming the modifier", raw, r)
		}
	}
	for _, raw := range []string{
		"", "status=active", "_id=sub-1", "url=", "url=a&url=b",
		"url=a&_summary=count", "url=a&_summary=true", "url=a&_total=accurate",
		"url:below=a", "url:above=a", "url:missing=false", "url=a&url:below=b",
		"url=a&status:not=off", "url=a&_id:exact=x", "url=a&_count:x=1",
		"url=a&_elements=id,status", "url=a&_elements=", "url=a&_elements=channel&_elements=id",
		"url=a&_sort=status", "url=a&_include=Subscription:x", "url=a&_format=json", "url=a&criteria=x",
		"url=a&endpoint=x", "url=a&payload=x", "url=a&type=rest-hook", "url=a&_lastUpdated=gt2020", "url=a&_has:x=1", "url=a&=1",
	} {
		wantRefusal(t, raw, CheckSearch(query(t, raw)), http.StatusForbidden, recording.RefusalAuthority)
	}
}

func TestCheckPageQuery(t *testing.T) {
	for _, raw := range []string{"_getpages=abc", "_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset",
		"_getpages=abc&_getpagesoffset=20&_count=20&_bundletype=searchset&_elements=channel,status", "_getpages=abc&_elements=channel"} {
		if r := CheckPageQuery(query(t, raw)); r != nil {
			t.Errorf("%q: refused %+v", raw, r)
		}
	}
	for _, raw := range []string{"", "_getpages=", "_getpages=a&_getpages=b", "_getpagesoffset=20", "_getpages:x=a", "_getpages=a&url=b", "_getpages=a&_summary=count", "_getpages=a&_total=accurate",
		// _elements: keeping channel, once, element names only.
		"_getpages=a&_elements=id", "_getpages=a&_elements=status,meta", "_getpages=a&_elements=", "_getpages=a&_elements=channel,",
		"_getpages=a&_elements=*", "_getpages=a&_elements=channel,*", "_getpages=a&_elements=channel&_elements=channel",
		"_getpages=a&_elements=channel&_elements=status", "_getpages=a&_elements:x=channel", "_getpages=a&_elements=channel%20x"} {
		wantRefusal(t, raw, CheckPageQuery(query(t, raw)), http.StatusForbidden, recording.RefusalAuthority)
	}
}

// The id rule, at its 64-character boundary.
func TestCheckID(t *testing.T) {
	for _, id := range []string{"a", "sub-1", "A.b-9", strings.Repeat("a", 64), "...", ".a", "a."} {
		if r := CheckID(id); r != nil {
			t.Errorf("%q: refused %+v", id, r)
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 65), ".", "..", "a b", "a/b", "a_b", "a%2Fb", "a?b", "a#b", "é", "a\x00"} {
		wantRefusal(t, id, CheckID(id), http.StatusBadRequest, recording.RefusalRouting)
	}
}
