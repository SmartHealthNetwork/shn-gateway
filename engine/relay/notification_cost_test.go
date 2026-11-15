package relay

import (
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay/internal/splice"
)

// E-17's place check (its signed-content exemption) reads a notification's
// SubscriptionStatus once, however many focus parts its edits name and however
// many members the Parameters carries: the focus parts and the Parameters'
// other members grow together, and the time stays near linear in them, where
// a check reading the Parameters once per edit is quadratic (wantLinearCost).
// It is timed with the model it reads, built in each call, since the model
// keeps what the check decides.
func TestNotificationAddressCheckIsLinear(t *testing.T) {
	const base = "https://provider.example/fhir"
	wantLinearCost(t, "E-17 focus parts", 65536, func(n int) func() {
		var junk, parts []string
		for i := 0; i < n; i++ {
			junk = append(junk, `"x`+strconv.Itoa(i)+`":0`)
			parts = append(parts, `{"name":"focus","valueReference":{"reference":"`+base+`/Task/t`+strconv.Itoa(i)+`"}}`)
		}
		status := `{` + strings.Join(junk, ",") + `,"sig":{"type":0,"when":0,"who":0},"parameter":[{"name":"topic","valueCanonical":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task"},` +
			`{"name":"notification-event","part":[` + strings.Join(parts, ",") + `]}],"resourceType":"Parameters"}`
		b := NewBody([]byte(`{"resourceType":"Bundle","type":"history","entry":[{"resource":`+status+`}]}`), OriginPeerFrame)
		d := mustDoc(t, b)
		var ops []splice.Op
		for i, r := range eachAt(t, d, []string{"entry", "0", "resource", "parameter", "1", "part"}, "valueReference", "reference") {
			ops = append(ops, d.Replace(r, []byte(`"https://payer-gw.example/cdex/provider/p/Task/t`+strconv.Itoa(i)+`"`)).op)
		}
		call := func() {
			m, err := buildModel(d.d, b.bytes(), ops)
			if err != nil {
				t.Fatal(err)
			}
			for _, eff := range m.effects {
				if eff != nil && !m.signedExemption(EditCDexTaskNotificationAddressRekey, *eff) {
					t.Fatal("a focus re-key the check refuses")
				}
			}
		}
		call()
		return call
	})
}

// The same for an entry's links: the entry's other members and its link urls
// grow together, and the entry is read once, not once per edit.
func TestNotificationEntryLinkCheckIsLinear(t *testing.T) {
	const base = "https://provider.example/fhir"
	wantLinearCost(t, "E-17 entry links", 65536, func(n int) func() {
		var junk, links []string
		for i := 0; i < n; i++ {
			junk = append(junk, `"x`+strconv.Itoa(i)+`":0`)
			links = append(links, `{"relation":"r","url":"`+base+`/Task/t`+strconv.Itoa(i)+`"}`)
		}
		entry := `{` + strings.Join(junk, ",") + `,"link":[` + strings.Join(links, ",") + `]}`
		b := NewBody([]byte(`{"resourceType":"Bundle","type":"history","entry":[{"resource":{"resourceType":"Parameters"}},`+entry+`],"signature":{"type":0,"when":0,"who":0}}`), OriginPeerFrame)
		d := mustDoc(t, b)
		var ops []splice.Op
		for i, u := range eachAt(t, d, []string{"entry", "1", "link"}, "url") {
			ops = append(ops, d.Replace(u, []byte(`"https://payer-gw.example/cdex/provider/p/Task/t`+strconv.Itoa(i)+`"`)).op)
		}
		call := func() {
			m, err := buildModel(d.d, b.bytes(), ops)
			if err != nil {
				t.Fatal(err)
			}
			for _, eff := range m.effects {
				if eff != nil && !m.signedExemption(EditCDexTaskNotificationAddressRekey, *eff) {
					t.Fatal("a link re-key the check refuses")
				}
			}
		}
		call()
		return call
	})
}
