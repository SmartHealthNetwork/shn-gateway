package subscription

import (
	"strings"
	"testing"
)

// The subscriber's gateway seals the whole channel (E-18 widened): the body it
// sends may state no channel.endpoint. E-19 then adds it, written from the
// sealed value alone, after channel's last member and before the header it
// adds; every other byte is as it was. One a body still states is overwritten.
func TestRekeyForNotifier_WritesAnEndpointTheBodyLeftOut(t *testing.T) {
	sub, not := edgeKeys(t)
	_, se, sh, err := SealOutbound(sub, binding, mustSubscription(t, baseSubscription))
	if err != nil {
		t.Fatal(err)
	}
	a := Address{Ingress: notifierIngress, Topic: "test", Subscriber: "provider-1", SealedEndpoint: se}
	headers := `["Authorization: ` + not.Credential(a, sh) + `","X-SHN-Channel: ` + sh + `"]`
	noEndpoint := edit(t, baseSubscription, `
    "endpoint": "`+subEndpoint+`",`, ``)
	noEither := edit(t, noEndpoint, `,
    "header": ["Authorization: Bearer s3cret", "X-Custom: a"]`, ``)
	for name, c := range map[string]struct{ in, want string }{
		"no endpoint, a header": {noEndpoint, edit(t, edit(t, noEndpoint, subHeaders, headers), headers+`
  }`, headers+`,"endpoint":"`+a.String()+`"
  }`)},
		"neither": {noEither, edit(t, noEither, `"valueCode": "full-resource"}]
    }`, `"valueCode": "full-resource"}]
    },"endpoint":"`+a.String()+`","header":`+headers)},
	} {
		out, err := RekeyForNotifier(not, a, sh, mustSubscription(t, c.in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(out) != c.want {
			t.Fatalf("%s:\n%s\nwant\n%s", name, out, c.want)
		}
		s := mustSubscription(t, string(out))
		if !not.Owns(s, notifierIngress, "provider-1", topic) {
			t.Fatalf("%s: the re-keyed Subscription is not the subscriber's", name)
		}
		if strings.Contains(string(out), "ehr.example/api") {
			t.Fatalf("%s: the subscriber's endpoint is in the body", name)
		}
	}
}
