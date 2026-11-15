package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// signedSubscription is a payer's topic Subscription carrying a Signature
// element in an extension, which covers the whole Subscription; header is its
// channel.header member, or "" for none.
func signedSubscription(header string) Body {
	h := ""
	if header != "" {
		h = `,"header":` + header
	}
	return NewBody([]byte(`{"resourceType":"Subscription","id":"s1","status":"requested",`+
		`"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`+
		`"criteria":"http://hl7.org/fhir/us/davinci-hrex/SubscriptionTopic/Task",`+
		`"channel":{"type":"rest-hook","endpoint":"https://payer.example/notify","payload":"application/fhir+json"`+h+`}}`), OriginPeerFrame)
}

const (
	notifyAddress = `"https://provider-gw.example/ingress/notify/cdex-task/payer-1/kid/sealed"`
	gatewayHeader = `["Authorization: Bearer kid.tag","X-SHN-Channel: kid.sealedheaders"]`
)

// TestSignedContentSubscriptionRekeyExemption: E-19 re-keys a signed
// Subscription's channel.endpoint and channel.header (D132, N2): a replace of
// the endpoint with a string, a replace of the header array with an array of
// strings, and the insert of a header array into a channel with none, each
// disclosed by the signature's carrier. Any other change E-19 makes to signed
// content is refused, as is every other edit's change of the channel.
func TestSignedContentSubscriptionRekeyExemption(t *testing.T) {
	withHeader := signedSubscription(`["Authorization: Bearer s3cret"]`)
	noHeader := signedSubscription("")
	// The channel as the subscriber's gateway seals it: no endpoint, no header.
	sealedChannel := NewBody([]byte(strings.Replace(string(noHeader.bytes()), `"endpoint":"https://payer.example/notify",`, ``, 1)), OriginPeerFrame)
	if strings.Contains(string(sealedChannel.bytes()), "endpoint") {
		t.Fatal("fixture: the sealed channel states an endpoint")
	}
	channel := func(d *Document) NodeID { return at(t, d, "channel") }
	type row struct {
		name string
		body Body
		edit EditID
		ops  func(d *Document) []Op
		made bool
	}
	rows := []row{
		{"the endpoint and the header replaced", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(notifyAddress)), d.Replace(at(t, d, "channel", "header"), []byte(gatewayHeader))}
		}, true},
		{"the endpoint replaced and the header inserted", noHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(notifyAddress)), d.InsertMember(channel(d), "header", []byte(gatewayHeader))}
		}, true},
		{"the endpoint and the header inserted into a sealed channel", sealedChannel, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(channel(d), "endpoint", []byte(notifyAddress)), d.InsertMember(channel(d), "header", []byte(gatewayHeader))}
		}, true},
		{"the endpoint alone", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(notifyAddress))}
		}, true},
		// Rejection rows: every other change E-19 could make to the signed
		// Subscription.
		{"the endpoint written as an object", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(`{"url":"x"}`))}
		}, false},
		{"the header written as a string", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "header"), []byte(`"Authorization: x"`))}
		}, false},
		{"the header written as an array holding an object", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "header"), []byte(`[{"a":"b"}]`))}
		}, false},
		{"one header value replaced", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "header", "0"), []byte(`"X-SHN-Channel: x"`))}
		}, false},
		{"a header inserted as a string", noHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(channel(d), "header", []byte(`"Authorization: x"`))}
		}, false},
		{"an endpoint inserted as an object", sealedChannel, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(channel(d), "endpoint", []byte(`{"url":"x"}`))}
		}, false},
		{"another channel member inserted", noHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(channel(d), "headers", []byte(gatewayHeader))}
		}, false},
		{"the channel type replaced", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "type"), []byte(`"websocket"`))}
		}, false},
		{"the criteria replaced", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "criteria"), []byte(`"http://example.org/other"`))}
		}, false},
		{"the header removed", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.RemoveMember(channel(d), "header")}
		}, false},
		{"a member inserted at the root", withHeader, EditSubscriptionEndpointRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(d.Root(), "header", []byte(gatewayHeader))}
		}, false},
	}
	// Every other edit made by ApplyChanges is refused on the signed
	// channel.endpoint and channel.header.
	for _, id := range EditIDs() {
		if _, exempt := signedExempt[id]; exempt || slices.Contains(TransformEditIDs(), id) || slices.Contains(headerEdits, id) || slices.Contains(queryEdits, id) {
			continue
		}
		rows = append(rows, row{"other edit " + string(id) + " on the signed endpoint", withHeader, id, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "channel", "endpoint"), []byte(notifyAddress))}
		}, false})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, r.ops(mustDoc(t, r.body))...)
			if r.made {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !slices.Equal(p.Edits(), []EditID{r.edit}) || !slices.Equal(p.SignedEdited(), []string{"Signature in Subscription"}) {
					t.Fatalf("edits %v, signed %v; want %s under Signature in Subscription", p.Edits(), p.SignedEdited(), r.edit)
				}
				if out := string(mustTransmit(t, p)); !strings.Contains(out, `"endpoint":`+notifyAddress) {
					t.Fatalf("endpoint not re-keyed: %s", out)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != r.edit || se.Carrier != "Signature in Subscription" {
				t.Fatalf("want %s refused under Signature in Subscription, got %v", r.edit, err)
			}
		})
	}
	// The place is the root Subscription's own channel: the same change in a
	// resource that is not a Subscription, or in a Subscription inside a
	// Bundle, is refused.
	for name, raw := range map[string]string{
		"a Basic's channel": strings.Replace(string(withHeader.bytes()), `"resourceType":"Subscription"`, `"resourceType":"Basic"`, 1),
		"a Subscription inside a signed Bundle": `{"resourceType":"Bundle","type":"collection","signature":{` + signatureElement + `},"entry":[{"resource":` +
			`{"resourceType":"Subscription","channel":{"type":"rest-hook","endpoint":"https://payer.example/notify"}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBody([]byte(raw), OriginPeerFrame)
			d := mustDoc(t, b)
			var path []string
			if strings.Contains(raw, `"Bundle"`) {
				path = []string{"entry", "0", "resource", "channel", "endpoint"}
			} else {
				path = []string{"channel", "endpoint"}
			}
			_, err := Apply(b, fhirJSON, EditSubscriptionEndpointRekey, d.Replace(at(t, d, path...), []byte(notifyAddress)))
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != EditSubscriptionEndpointRekey {
				t.Fatalf("want E-19 refused, got %v", err)
			}
		})
	}
	// Unsigned, E-19 is an ordinary edit: made, and naming no signature.
	unsigned := NewBody([]byte(strings.Replace(string(withHeader.bytes()), `"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`, "", 1)), OriginPeerFrame)
	d := mustDoc(t, unsigned)
	p, err := Apply(unsigned, fhirJSON, EditSubscriptionEndpointRekey, d.Replace(at(t, d, "channel", "endpoint"), []byte(notifyAddress)))
	if err != nil || p.SignedEdited() != nil || !slices.Equal(p.Edits(), []EditID{EditSubscriptionEndpointRekey}) {
		t.Fatalf("unsigned: %v %v %v", p.Edits(), p.SignedEdited(), err)
	}
}
