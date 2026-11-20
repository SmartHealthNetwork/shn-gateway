//go:build !shn_coreonly

package engine

// workstream_pa_subscriber_test.go — the subscriber's half of a topic
// Subscription at the engine's level: what cannot come through a real
// notifier (a framed location the SDK does not read, E-23's own check
// refusing its value), the disclosure of E-20 and E-23 on one answer whatever
// order their edits were made in, the page form, the scope a request takes,
// and the subscribe frame on another leg. The interactions themselves run
// through both gateways in test/adversarial (cdex_task_subscriber_test.go).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// restoredRequest is a request whose context holds x.
func recordedRequest(x *exchangeRecorder) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/cdex/provider/p/Subscription", nil)
	return r.WithContext(context.WithValue(r.Context(), exchangeRecorderKey{}, x))
}

const subscriberBase = "https://payer-gw.example/cdex/provider/p"

// E-23 on a create's or update's 2xx: written at the per-counterpart base
// from the framed location; with none framed, no Location and the reason
// recorded; with one the SDK does not read, none either, and recorded; and a
// value the core's check refuses is the edit's own failure, nothing written.
func TestSubscriber_TheLocationEdit(t *testing.T) {
	g := &Gateway{}
	// An answer with nothing for E-20 (no Subscription, no address), so
	// only E-23 is under test.
	raw := []byte(`{"resourceType":"OperationOutcome","issue":[]}`)
	for name, c := range map[string]struct {
		tail, want, notMade string
		writes              bool
	}{
		"a create's location":         {"Subscription/s1/_history/1", subscriberBase + "/Subscription/s1/_history/1", "", true},
		"none framed":                 {"", "", EditNotMadeLocationNoTail, true},
		"one the SDK does not read":   {"Task/s1", "", EditNotMadeLocationUnreadableTail, true},
		"a dot segment":               {"Subscription/..", "", EditNotMadeLocationUnreadableTail, true},
		"a read writes no location":   {"Subscription/s1", "", "", false},
		"a read records no reason":    {"", "", "", false},
		"an update's unversioned one": {"Subscription/s1", subscriberBase + "/Subscription/s1", "", true},
	} {
		x := &exchangeRecorder{}
		stated := statedAnswer{status: http.StatusCreated, address: map[string]string{}}
		if c.tail != "" {
			stated.address[shnsdk.FrameHeaderSubscriptionLocation] = c.tail
		}
		p, err := g.restoredAnswer(recordedRequest(x), raw, mediaFHIRJSON, subscription.Keys{}, subscription.Binding{}, subscription.Search{}, stated, subscriberBase, c.writes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, value, set := p.EditedHeader()
		if value != c.want || set != (c.want != "") {
			t.Errorf("%s: Location %q (%v), want %q", name, value, set, c.want)
		}
		var wantNotMade []string
		if c.notMade != "" {
			wantNotMade = []string{c.notMade}
		}
		if !slices.Equal(x.rec.EditsNotMade, wantNotMade) {
			t.Errorf("%s: not made %v, want %v", name, x.rec.EditsNotMade, wantNotMade)
		}
	}
	// The core's check refuses a value that is not the base and the tail:
	// the edit fails, and nothing is written.
	prev := subscriptionLocationValue
	subscriptionLocationValue = func(base, tail string) string { return base + "/x/" + tail }
	defer func() { subscriptionLocationValue = prev }()
	stated := statedAnswer{status: http.StatusCreated, address: map[string]string{shnsdk.FrameHeaderSubscriptionLocation: "Subscription/s1"}}
	if _, err := g.restoredAnswer(recordedRequest(&exchangeRecorder{}), raw, mediaFHIRJSON, subscription.Keys{}, subscription.Binding{}, subscription.Search{}, stated, subscriberBase, true); !errors.Is(err, relay.ErrVerifyMismatch) {
		t.Fatalf("a value the core refuses: %v", err)
	}
}

// E-20 records why it made no change, by the library's reason, and an
// answer it cannot read is carried as sent, recorded.
func TestSubscriber_TheRestoreRecordsWhyNot(t *testing.T) {
	g := &Gateway{}
	keys, err := subscription.DeriveKeys("payer", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := subscription.Binding{Subscriber: "payer", Notifier: "provider", Topic: "cdex-task"}
	for name, c := range map[string]struct {
		raw     string
		notMade []string
	}{
		"a foreign endpoint": {`{"resourceType":"Subscription","channel":{"endpoint":"https://x.example/y"}}`, []string{EditNotMadeRestoreNotAnAddress}},
		"no endpoint":        {`{"resourceType":"Subscription","channel":{}}`, []string{EditNotMadeRestoreNoEndpoint}},
		"not JSON":           {`<Subscription/>`, []string{EditNotMadeRestoreUnreadableAnswer}},
		"no stated base":     {`{"resourceType":"Bundle","entry":[{"fullUrl":"https://p.example/fhir/Subscription/1"}]}`, []string{EditNotMadeRestoreNoStatedBase}},
		"an unreadable list": {`{"resourceType":"Bundle","entry":{}}`, []string{EditNotMadeRestoreUnreadableBundle}},
	} {
		x := &exchangeRecorder{}
		p, err := g.restoredAnswer(recordedRequest(x), []byte(c.raw), mediaFHIRJSON, keys, b, subscription.Search{}, statedAnswer{status: http.StatusOK}, subscriberBase, false)
		if err != nil || p.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("%s: %v %v, want carried as sent", name, p, err)
		}
		if !slices.Equal(x.rec.EditsNotMade, c.notMade) {
			t.Errorf("%s: not made %v, want %v", name, x.rec.EditsNotMade, c.notMade)
		}
	}
	// Every library reason has its recorded value, in the closed list.
	for _, why := range []string{subscription.NotMadeNoEndpoint, subscription.NotMadeForeignAddress, subscription.NotMadeAnotherBinding,
		subscription.NotMadeNoSealedHeaders, subscription.NotMadeKeyRetired, subscription.NotMadeSealInvalid,
		subscription.NotMadeNoStatedBase, subscription.NotMadeUnreadable, subscription.NotMadeLinkURL} {
		if v, ok := restoreNotMade[why]; !ok || v != "E-20:"+why || !slices.Contains(EditNotMadeValues, v) {
			t.Errorf("%s: recorded as %q (%v)", why, v, ok)
		}
	}
}

// applyRestore makes exactly the changes the library names, by span, and
// refuses a span the answer does not hold or a removal of a value that is no
// member's.
func TestSubscriber_ApplyRestore(t *testing.T) {
	raw := []byte(`{"resourceType":"Subscription","channel":{"endpoint":"a","header":["h"]}}`)
	at := func(s string) (int, int) {
		i := slices.Index([]byte(string(raw)), s[0])
		for j := range raw {
			if string(raw[j:min(len(raw), j+len(s))]) == s {
				i = j
				break
			}
		}
		return i, i + len(s)
	}
	es, ee := at(`"a"`)
	hs, he := at(`["h"]`)
	p, err := applyRestore(raw, mediaFHIRJSON, []subscription.RestoreEdit{{Start: es, End: ee, Value: []byte(`"https://payer.example/n"`)}, {Start: hs, End: he, Remove: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustTransmitPayload(t, p)); got != `{"resourceType":"Subscription","channel":{"endpoint":"https://payer.example/n"}}` {
		t.Fatalf("restored %s", got)
	}
	for name, e := range map[string]subscription.RestoreEdit{
		"a span the answer does not hold": {Start: es + 1, End: ee, Value: []byte(`"x"`)},
		"a removal of the root":           {Start: 0, End: len(raw), Remove: true},
	} {
		if _, err := applyRestore(raw, mediaFHIRJSON, []subscription.RestoreEdit{e}, nil); err == nil {
			t.Errorf("%s: made", name)
		}
	}
}

// mustTransmitPayload is p's bytes, as the writer of the answer reads them.
func mustTransmitPayload(t *testing.T, p relay.Payload) []byte {
	t.Helper()
	b, err := relay.Transmit(p, func(relay.Payload) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// X-SHN-Edited names E-20 and E-23 on one answer in the registry's order,
// whatever order the edits were made in, and names the signature E-20 changed
// on E-20 alone: a header edit changes no signed content.
func TestSubscriber_TheDisclosureIsIndependentOfOrder(t *testing.T) {
	const sig = `"signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-13T00:00:00Z","who":{"reference":"Organization/p"}}`
	body := relay.NewBody([]byte(`{"resourceType":"Subscription",`+sig+`,"channel":{"endpoint":"a"}}`), relay.OriginPeerFrame)
	doc, err := relay.Doc(body)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := doc.Member(doc.Root(), "channel")
	ep, _ := doc.Member(ch, "endpoint")
	p, err := relay.Apply(body, mediaFHIRJSON, relay.EditSubscriptionEndpointRestore, doc.Replace(ep, []byte(`"https://payer.example/n"`)))
	if err != nil {
		t.Fatal(err)
	}
	p, err = headerEdited(p, relay.EditSubscriptionLocationRekey, "Location", subscriberBase+"/Subscription/s1",
		relay.HeaderInputs{relay.SubscriptionLocationInputBase: subscriberBase, relay.SubscriptionLocationInputTail: "Subscription/s1"})
	if err != nil {
		t.Fatal(err)
	}
	first := http.Header{}
	setEditedHeader(first, p)
	got := [][]string{first.Values(EditedHeader)}
	want := []string{`subscription-endpoint-restore; signed="Signature in Subscription"`, "subscription-location-rekey"}
	if !slices.Equal(got[0], want) {
		t.Fatalf("X-SHN-Edited %q, want %q", got[0], want)
	}
	// The same edits listed in the other order disclose the same.
	h := http.Header{}
	setEditedHeader(h, reorderedEdits(t))
	if !slices.Equal(h.Values(EditedHeader), []string{"pas-pend-return-rekey", "subscription-endpoint-restore"}) {
		t.Fatalf("reordered: %q", h.Values(EditedHeader))
	}
}

// reorderedEdits is a payload whose edits are listed E-20 first, then E-10:
// the reverse of the registry's order.
func reorderedEdits(t *testing.T) relay.Payload {
	t.Helper()
	unsigned := relay.NewBody([]byte(`{"resourceType":"Subscription","channel":{"endpoint":"a"},"x":"b"}`), relay.OriginPeerFrame)
	doc, err := relay.Doc(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := doc.Member(doc.Root(), "channel")
	ep, _ := doc.Member(ch, "endpoint")
	x, _ := doc.Member(doc.Root(), "x")
	p, err := relay.ApplyChanges(unsigned, mediaFHIRJSON,
		relay.Change{Edit: relay.EditSubscriptionEndpointRestore, Ops: []relay.Op{doc.Replace(ep, []byte(`"n"`))}},
		relay.Change{Edit: relay.EditPASPendReturnRekey, Ops: []relay.Op{doc.Replace(x, []byte(`"c"`))}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Edits(), []relay.EditID{relay.EditSubscriptionEndpointRestore, relay.EditPASPendReturnRekey}) {
		t.Fatalf("fixture: edits %v", p.Edits())
	}
	return p
}

// A page is told by its parameter's name, never by a substring.
func TestSubscriber_PageQuery(t *testing.T) {
	for raw, want := range map[string]bool{
		"_getpages=a":                         true,
		"_getpages=a&_count=2":                true,
		"_count=2&_getpages=a":                true,
		"url=x&status=_getpages":              false,
		"x_getpages=a":                        false,
		"url=https%3A%2F%2Fx%2F_getpages%3Da": false,
		"":                                    false,
	} {
		if got := pageQuery(raw); got != want {
			t.Errorf("pageQuery(%q) = %v, want %v", raw, got, want)
		}
	}
}

// Each request takes the Subscription permission of its interaction.
func TestSubscriber_TheScopeEachRequestTakes(t *testing.T) {
	for _, c := range []struct {
		method, rest string
		need         taskPermission
	}{
		{http.MethodPost, "Subscription", taskCreate},
		{http.MethodGet, "Subscription", taskSearch},
		{http.MethodHead, "Subscription", taskSearch},
		{http.MethodGet, "Subscription/s1", taskRead},
		{http.MethodGet, "Subscription/s1/$status", taskRead},
		{http.MethodGet, "Subscription/s1/_history/1", taskRead},
		{http.MethodPut, "Subscription/s1", taskUpdate},
		{http.MethodPatch, "Subscription/s1", taskUpdate},
		{http.MethodDelete, "Subscription/s1", taskDelete},
	} {
		if need, _ := subscriptionNeed(c.method, c.rest); need != c.need {
			t.Errorf("%s %s takes %v, want %v", c.method, c.rest, need, c.need)
		}
	}
	for granted, want := range map[string]bool{
		"system/Subscription.cruds":           true,
		"system/Subscription.*":               true,
		"system/Subscription.read":            false,
		"system/Subscription.write":           true,
		"system/Subscription.cud":             true,
		"system/Subscription.rs":              false,
		"system/Subscription.c?criteria=x":    false,
		"system/Task.write system/Task.read":  false,
		"system/subscription.c":               false,
		"system/Task.c system/Subscription.c": true,
		// A wildcard permits the Subscription routes for what it grants.
		"system/*.c":                 true,
		"system/*.write":             true,
		"system/*.*":                 true,
		"system/*.rs":                false,
		"system/*.c?x=1":             false,
		"system/*.rc":                false,
		"patient/*.write":            false,
		"system/Task.r system/*.cud": true,
	} {
		if got := subscriptionScopesPermit(granted, taskCreate); got != want {
			t.Errorf("%q permits a create: %v, want %v", granted, got, want)
		}
	}
	if subscriptionChallengeScope(taskSearch) != SubscriptionReadScope || subscriptionChallengeScope(taskDelete) != SubscriptionWriteScope {
		t.Error("challenge scopes")
	}
}

// A registered client's Subscription scopes and wildcards are advertised as
// listed, each once, one with a query or a misspelled one never; and the boot
// rule refuses a misspelled Subscription scope.
func TestSubscriber_ScopesAdvertisedAndChecked(t *testing.T) {
	clients := map[string]IngressClientRegistration{
		"a": {Scopes: []string{TaskWriteScope, "system/Subscription.cruds", "system/Subscription.r?x=1"}},
		"b": {Scopes: []string{TaskWriteScope, "system/Subscription.read"}},
		"m": {Module: "x", Scopes: []string{"system/Subscription.*"}},
		// A wildcard is advertised as a resource scope is: one that grants
		// r once, as a read scope and a Subscription scope both, and one
		// that grants only create as a Subscription scope.
		"w": {Scopes: []string{"system/*.rs", "system/*.write", "system/*.c?x=1"}},
	}
	got := ingressScopes("payer", clients, false)
	if want := []string{TaskWriteScope, "system/*.rs", "system/*.write", "system/Subscription.cruds", "system/Subscription.read"}; !slices.Equal(got, want) {
		t.Fatalf("advertised %q, want %q", got, want)
	}
	if err := CheckPayerIngressScopes([]string{TaskWriteScope, "system/Subscription.xyz"}); err == nil {
		t.Fatal("a misspelled Subscription scope registered")
	}
	if err := CheckPayerIngressScopes([]string{TaskWriteScope, "system/Subscription.cruds"}); err != nil {
		t.Fatal(err)
	}
}

// The subscribe frame carries the interaction and each value set, nothing
// else; it rides only on a subscribe leg (errSubscribeUndefined, in
// roundTripInner).
func TestSubscriber_TheFrameHeaders(t *testing.T) {
	f := &subscribeRequest{request: "GET Subscription/s1"}
	if h := f.frameHeaders(); len(h) != 1 || h[shnsdk.FrameHeaderSubscriptionRequest] != "GET Subscription/s1" {
		t.Fatalf("headers %v", h)
	}
	f = &subscribeRequest{request: "PUT Subscription/s1", endpoint: "e", channel: "c", ifMatch: `W/"1"`, contentType: "application/fhir+json"}
	if h := f.frameHeaders(); len(h) != 5 || h[shnsdk.FrameHeaderIfMatch] != `W/"1"` || h[shnsdk.FrameHeaderSubscriptionEndpoint] != "e" ||
		h[shnsdk.FrameHeaderSubscriptionChannel] != "c" || h["Content-Type"] != "application/fhir+json" {
		t.Fatalf("headers %v", h)
	}
}

// A page is served only at the base's Subscription, where E-20 writes its
// link: one asked for at the base itself (rest "", "GET [base]?_getpages=…",
// the frame's own page form) is no interaction the subscriber's system makes,
// and is refused 400 on routing with nothing sent; a page whose _getpages is
// escaped (%5Fgetpages) is no page, and is refused as a search outside its set
// (403 on authority).
func TestSubscriber_APageIsAskedForAtSubscription(t *testing.T) {
	for _, c := range []struct {
		rest, query string
		status      int
		rule        string
	}{
		{"", "_getpages=pg1&_getpagesoffset=2&_count=2&_bundletype=searchset", http.StatusBadRequest, RefusalRouting},
		{"Subscription", "%5Fgetpages=pg1", http.StatusForbidden, RefusalAuthority},
	} {
		x := &exchangeRecorder{}
		r := httptest.NewRequest(http.MethodGet, "/cdex/provider/p/"+c.rest+"?"+c.query, nil)
		r = r.WithContext(context.WithValue(r.Context(), exchangeRecorderKey{}, x))
		rec := httptest.NewRecorder()
		(&Gateway{}).serveSubscriptions(rec, r, cdexTopic, "provider", subscriberBase, c.rest)
		if rec.Code != c.status || x.guardRule != c.rule {
			t.Errorf("rest %q, query %q: %d on %q %s, want %d on %q", c.rest, c.query, rec.Code, x.guardRule, rec.Body, c.status, c.rule)
		}
	}
}
