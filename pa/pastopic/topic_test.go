package pastopic

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	"github.com/SmartHealthNetwork/shn-gateway/recording"
)

const (
	npiA     = "1234567893"
	npiB     = "1245319599"
	otherSys = "http://example.org/provider-ids"
)

// npiSystem is the NPI's identifier system, the one system the registry
// admits for a provider's attested identifiers today.
const npiSystem = "http://hl7.org/fhir/sid/us-npi"

// holder is a provider the registry attests two NPIs for, plus an identifier
// in another system: the registry attests NPIs only today, but the rule reads
// a provider's attested identifiers by structure, whatever their system, so
// one the registry comes to attest (an X12 sender id) is held to it too.
var holder = subscription.Holder{ID: "provider-x", ProviderIDs: []subscription.Identifier{
	{System: npiSystem, Value: npiA}, {System: npiSystem, Value: npiB}, {System: otherSys, Value: "L1"},
}}

func pair(sys, val string) subscription.FilterValue {
	return subscription.FilterValue{System: sys, HasSystem: true, Value: val}
}

func bare(val string) subscription.FilterValue { return subscription.FilterValue{Value: val} }

func org(vals ...subscription.FilterValue) subscription.FilterParam {
	return subscription.FilterParam{Name: FilterParameter, Values: vals}
}

func want403(t *testing.T, name string, rf *subscription.Refusal) {
	t.Helper()
	if rf == nil || rf.Status != http.StatusForbidden || rf.Primitive != recording.RefusalAuthority {
		t.Errorf("%s: %+v, want 403 authority", name, rf)
	}
}

// The filter rule (§7.1 rule 3): exactly one org-identifier, no modifier, no
// other parameter, and a system|value pair carried only when it equals an
// attested pair.
func TestCheckFilter_Shape(t *testing.T) {
	for name, params := range map[string][]subscription.FilterParam{
		"an attested NPI as system|value":    {org(pair(npiSystem, npiA))},
		"both attested NPIs":                 {org(pair(npiSystem, npiA), pair(npiSystem, npiB))},
		"an attested identifier elsewhere":   {org(pair(otherSys, "L1"))},
		"an NPI and an identifier elsewhere": {org(pair(npiSystem, npiB), pair(otherSys, "L1"))},
	} {
		if rf := (Topic{}).CheckFilter(holder, params); rf != nil {
			t.Errorf("%s: refused %+v", name, rf)
		}
	}
	for name, params := range map[string][]subscription.FilterParam{
		"no filter":                           nil,
		"an unattested NPI":                   {org(pair(npiSystem, "1003000126"))},
		"an attested value in another system": {org(pair(otherSys, npiA))},
		"the empty system":                    {org(pair("", npiA))},
		"one good, one unattested":            {org(pair(npiSystem, npiA), pair(npiSystem, "1003000126"))},
		"org-identifier twice":                {org(pair(npiSystem, npiA)), org(pair(npiSystem, npiB))},
		"a modifier":                          {{Name: FilterParameter, Modifier: "not", Values: []subscription.FilterValue{pair(npiSystem, npiA)}}},
		"the topic's canFilterBy name":        {{Name: "orgIdentifier", Values: []subscription.FilterValue{pair(npiSystem, npiA)}}},
		"another parameter beside it":         {org(pair(npiSystem, npiA)), {Name: "patient", Values: []subscription.FilterValue{bare("Patient/1")}}},
		"an org-identifier with no value":     {org()},
	} {
		want403(t, name, (Topic{}).CheckFilter(holder, params))
	}
	if rf := (Topic{}).CheckFilter(holder, nil); rf == nil || !strings.Contains(rf.Reason, "names no org-identifier") {
		t.Errorf("no filter: %+v, want the refusal naming the missing org-identifier", rf)
	}
}

// A value with no system, as the PAS specification names the filter's values
// (spec-58), is carried when it equals the value of an identifier attested for
// the provider, in whatever system it is attested: an attested NPI, or an
// identifier the registry comes to attest in another system. Anything else is
// refused 403, recorded authority, a value attested only for another provider
// among them.
func TestCheckFilter_ValueWithNoSystem(t *testing.T) {
	for name, params := range map[string][]subscription.FilterParam{
		"an attested NPI":                          {org(bare(npiA))},
		"two attested NPIs":                        {org(bare(npiA), bare(npiB))},
		"an attested identifier in another system": {org(bare("L1"))},
		"a bare NPI beside an attested pair":       {org(pair(otherSys, "L1"), bare(npiB))},
	} {
		if rf := (Topic{}).CheckFilter(holder, params); rf != nil {
			t.Errorf("%s: refused %+v", name, rf)
		}
	}
	for name, params := range map[string][]subscription.FilterParam{
		"an unattested value":                  {org(bare("1003000126"))},
		"one attested NPI, one unattested":     {org(bare(npiA), bare("1003000126"))},
		"an attested bare NPI, a crossed pair": {org(bare(npiA), pair(otherSys, npiA))},
		"the empty value":                      {org(bare(""))},
		"a value differing only in case":       {org(bare("l1"))},
		"another provider's NPI":               {org(bare(npiA))},
	} {
		h := holder
		if name == "another provider's NPI" {
			h = subscription.Holder{ID: "provider-y", ProviderIDs: []subscription.Identifier{{System: npiSystem, Value: npiB}}}
		}
		rf := (Topic{}).CheckFilter(h, params)
		want403(t, name, rf)
		if rf != nil && name != "an attested bare NPI, a crossed pair" && !strings.Contains(rf.Reason, "not an identifier attested for the subscriber") {
			t.Errorf("%s: %+v, want the refusal naming the unattested value", name, rf)
		}
	}
	// A provider the registry attests no identifier for has none a value
	// with no system can equal.
	want403(t, "no attested identifiers", (Topic{}).CheckFilter(subscription.Holder{ID: "provider-z"}, []subscription.FilterParam{org(bare(npiA))}))
}

func TestTopic_Constants(t *testing.T) {
	top := Topic{}
	if top.Token() != "pas" || top.Canonical() != Canonical || !slices.Equal(top.Contents(), []string{"full-resource", "id-only"}) || top.Extensions() != nil ||
		top.SubscribeLeg() != "pas-subscription-subscribe" || top.ContentLeg() != "pas-subscription-notify" || top.SignalLeg() != "pas-subscription-signal" {
		t.Fatal("the topic's constants")
	}
}

// contentNotification is a full-resource PAS notification whose second entry
// carries resource, as br-payer posts its decision.
func contentNotification(t *testing.T, resource string) *subscription.Notification {
	t.Helper()
	n, rf := subscription.ParseNotification([]byte(notificationBody(resource)), Topic{})
	if rf != nil {
		t.Fatalf("fixture: %+v", rf)
	}
	return n
}

// notificationBody is contentNotification's bytes.
func notificationBody(resource string) string {
	return `{"resourceType":"Bundle","type":"history","entry":[{"fullUrl":"urn:uuid:0b6a1f8e-2c3d-4e5f-8a9b-0c1d2e3f4a5b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},{"name":"topic","valueCanonical":"` + Canonical + `"},` +
		`{"name":"type","valueCode":"event-notification"},{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus"}]}]}},` +
		`{"request":{"method":"PUT"},"resource":` + resource + `}]}`
}

// claimResponse is a ClaimResponse naming receiver codes in its
// TransmissionIdentifiers extension (none when codes is empty).
func claimResponse(codes ...string) string {
	ext := ""
	if len(codes) > 0 {
		var subs []string
		for _, c := range codes {
			subs = append(subs, `{"url":"applicationSenderCode","valueString":"PAYER-1"},{"url":"`+ReceiverCode+`","valueString":"`+c+`"}`)
		}
		ext = `"extension":[{"url":"` + TransmissionIdentifiersURL + `","extension":[` + strings.Join(subs, ",") + `]}],`
	}
	return `{"resourceType":"ClaimResponse","id":"cr1",` + ext + `"patient":{"reference":"Patient/p1"},"outcome":"complete"}`
}

// N6 (§7.3), a network rule: a content notification is the subscriber's
// only when every ClaimResponse it carries names, as its
// applicationReceiverCode, an identifier attested for the subscriber (a bare
// value equal to an attested value, or a system|value pair equal to an
// attested pair). A code naming another identifier, a ClaimResponse with no
// code, and a notification with no ClaimResponse are each refused 403 on
// authority, the absent code beside the mismatched one.
func TestCheckContent_TheReceiverIsTheSubscriber(t *testing.T) {
	inBundle := func(resources ...string) string {
		var entries []string
		for i, r := range resources {
			entries = append(entries, `{"fullUrl":"https://payer.example/fhir/ClaimResponse/cr`+string(rune('1'+i))+`","resource":`+r+`}`)
		}
		return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
	}
	for name, c := range map[string]struct {
		resource string
		ok       bool
	}{
		"a bare attested NPI, in the PAS Response Bundle": {inBundle(claimResponse(npiA)), true},
		"a system|value attested pair":                    {inBundle(claimResponse(npiSystem + "|" + npiA)), true},
		"a bare ClaimResponse naming an attested NPI":     {claimResponse(npiA), true},
		"two ClaimResponses, each naming an attested NPI": {inBundle(claimResponse(npiA), claimResponse(npiB)), true},
		"another provider's NPI":                          {inBundle(claimResponse("1111111111")), false},
		"an attested value under another system":          {inBundle(claimResponse(otherSys + "|" + npiA)), false},
		"no applicationReceiverCode":                      {inBundle(claimResponse()), false},
		"one of two ClaimResponses naming none":           {inBundle(claimResponse(npiA), claimResponse()), false},
		"one of two ClaimResponses naming another's":      {inBundle(claimResponse(npiA), claimResponse("1111111111")), false},
		"no ClaimResponse":                                {`{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"https://payer.example/fhir/Patient/p1","resource":{"resourceType":"Patient","id":"p1"}}]}`, false},
		// Every ClaimResponse wherever it sits, its names in any case.
		"another's, contained in an attested one":                {strings.Replace(claimResponse(npiA), `"outcome":"complete"`, `"outcome":"complete","contained":[`+claimResponse("1111111111")+`]`, 1), false},
		"another's, in a nested Bundle":                          {inBundle(claimResponse(npiA), inBundle(claimResponse("1111111111"))), false},
		"another's, its type in another case":                    {inBundle(claimResponse(npiA), strings.Replace(claimResponse("1111111111"), `"ClaimResponse"`, `"claimResponse"`, 1)), false},
		"another's, its extension member in another case":        {inBundle(claimResponse(npiA), strings.Replace(claimResponse("1111111111"), `"extension":[{"url"`, `"Extension":[{"url"`, 1)), false},
		"a second receiver spelling url URL, another's":          {strings.Replace(claimResponse(npiA), `{"url":"`+ReceiverCode+`","valueString":"`+npiA+`"}`, `{"url":"`+ReceiverCode+`","valueString":"`+npiA+`"},{"URL":"`+ReceiverCode+`","valueString":"1111111111"}`, 1), false},
		"a receiver's valueString beside a valueCode, another's": {strings.Replace(claimResponse(npiA), `"valueString":"`+npiA+`"}`, `"valueString":"`+npiA+`","valueCode":"1111111111"}`, 1), false},
		"a receiver's valueCode alone":                           {strings.Replace(claimResponse(npiA), `{"url":"`+ReceiverCode+`","valueString":"`+npiA+`"}`, `{"url":"`+ReceiverCode+`","valueCode":"`+npiA+`"}`, 1), false},
		"a receiver's valueString beside a ValueCode, another's": {strings.Replace(claimResponse(npiA), `"valueString":"`+npiA+`"}`, `"valueString":"`+npiA+`","ValueCode":"1111111111"}`, 1), false},
		"an id beside the receiver's valueString, no value[x]":   {strings.Replace(claimResponse(npiA), `"valueString":"`+npiA+`"}`, `"valueString":"`+npiA+`","id":"r1"}`, 1), true},
		"a receiver named by no string":                          {strings.Replace(claimResponse(npiA), `{"url":"`+ReceiverCode+`","valueString":"`+npiA+`"}`, `{"url":"`+ReceiverCode+`","valueString":"`+npiA+`"},{"url":"`+ReceiverCode+`","valueIdentifier":{"value":"1111111111"}}`, 1), false},
		"a resourceType that is no type's name":                  {inBundle(claimResponse(npiA), `{"resourceType":["ClaimResponse"]}`), false},
	} {
		rf := (Topic{}).CheckContent(holder, contentNotification(t, c.resource))
		if c.ok {
			if rf != nil {
				t.Errorf("%s: %+v, want carried", name, rf)
			}
			continue
		}
		if rf == nil || rf.Status != http.StatusForbidden || rf.Primitive != recording.RefusalAuthority {
			t.Errorf("%s: %+v, want 403 on authority", name, rf)
		}
	}
	// Beside an attested decision, a ClaimResponse in a SubscriptionStatus
	// parameter's resource, an entry's response.outcome, or a later entry's
	// Resource spelled in another case: another provider's is refused, an
	// attested one carried.
	for name, place := range map[string]func(cr string) string{
		"a SubscriptionStatus parameter's resource": func(cr string) string {
			return strings.Replace(notificationBody(inBundle(claimResponse(npiA))), `{"name":"type"`, `{"name":"x","resource":`+cr+`},{"name":"type"`, 1)
		},
		"an entry's response.outcome": func(cr string) string {
			return strings.TrimSuffix(notificationBody(inBundle(claimResponse(npiA))), `}]}`) + `,"response":{"status":"200","outcome":` + cr + `}}]}`
		},
		"a later entry's Resource": func(cr string) string {
			return strings.TrimSuffix(notificationBody(inBundle(claimResponse(npiA))), `]}`) + `,{"Resource":` + cr + `}]}`
		},
	} {
		for _, c := range []struct {
			code string
			ok   bool
		}{{npiB, true}, {"1111111111", false}} {
			n, rf := subscription.ParseNotification([]byte(place(claimResponse(c.code))), Topic{})
			if rf != nil {
				t.Fatalf("%s: fixture: %+v", name, rf)
			}
			rf = (Topic{}).CheckContent(holder, n)
			if c.ok != (rf == nil) || (rf != nil && (rf.Status != http.StatusForbidden || !strings.Contains(rf.Reason, "1111111111"))) {
				t.Errorf("%s, code %s: %+v, want carried %v (refused 403 naming the code otherwise)", name, c.code, rf, c.ok)
			}
		}
	}
	// A provider the registry attests nothing for is no receiver.
	if rf := (Topic{}).CheckContent(subscription.Holder{ID: "provider-z"}, contentNotification(t, claimResponse(npiA))); rf == nil {
		t.Error("a provider with no attested identifiers: carried, want refused")
	}
}

// The notification shape is the pinned PAS topics' (Basic-PASSubscriptionTopic.json
// in hl7.fhir.us.davinci-pas 2.0.1, sha256
// 1ba4159f2a1ea000dfb1685601bfc902c603a1c59ce2127118ee0a7aec694218; 2.1.0,
// sha256 a0daef9c5acf6954c15ebd713477ce49962e56c0c2ac4362d1381aee8f7892df;
// and 2.2.1, sha256
// da2aac8e480ce66fe27812af18e3c9bc190b08a33d5d7e1c850caf9097fb210e, as
// gateway/deploy/validator pins them): a notificationShape on Bundle at 2.1.0
// and 2.2.1, none at 2.0.1, whose one eventTrigger names ClaimResponse, and
// no additional-context. A tripwire: a change to either list, or to the pins,
// is a change to what an instance $status answer may carry.
func TestTopic_NotificationShapeIsThePinnedTopics(t *testing.T) {
	got := Topic{}.NotificationShape()
	if got.Topic != Canonical || !slices.Equal(got.Focus, []string{"Bundle", "ClaimResponse"}) || len(got.AdditionalContext) != 0 {
		t.Fatalf("notification shape %+v, want the topic's canonical, focus [Bundle ClaimResponse] and no additional-context", got)
	}
	pins, err := os.ReadFile("../../deploy/validator/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{
		"1ba4159f2a1ea000dfb1685601bfc902c603a1c59ce2127118ee0a7aec694218  hl7.fhir.us.davinci-pas-2.0.1.tgz",
		"a0daef9c5acf6954c15ebd713477ce49962e56c0c2ac4362d1381aee8f7892df  hl7.fhir.us.davinci-pas-2.1.0.tgz",
		"da2aac8e480ce66fe27812af18e3c9bc190b08a33d5d7e1c850caf9097fb210e  hl7.fhir.us.davinci-pas-2.2.1.tgz",
	} {
		if !strings.Contains(string(pins), pin) {
			t.Errorf("the validator no longer pins %q: re-read the topic's notification shape from the package it pins now", pin)
		}
	}
}
