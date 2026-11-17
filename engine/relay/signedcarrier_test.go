package relay

import (
	"errors"
	"math/rand/v2"
	"testing"
)

// TestSignedCarrier names the signature that covers a value as a whole, and
// nothing for a value no signature covers.
func TestSignedCarrier(t *testing.T) {
	const bare = `{"data":"c2ln"}`
	signedProv := func(target string) string {
		return `{"resourceType":"Provenance","id":"p1","target":[{"reference":"` + target + `"}],"recorded":"2026-01-01T00:00:00Z","agent":[{"who":{"reference":"Practitioner/1"}}],"signature":[` + bare + `]}`
	}
	for _, row := range []struct {
		name, doc string
		path      []string
		want      string
	}{
		{"an unsigned Task", `{"resourceType":"Task","id":"t1","status":"requested","intent":"order"}`, nil, ""},
		{"a Task with a Signature in an extension, lacking members R4 requires", `{"resourceType":"Task","id":"t1","extension":[{"url":"urn:x","valueSignature":` + bare + `}]}`, nil, "Signature in Task"},
		{"a Task with a Signature in an input, lacking members R4 requires", `{"resourceType":"Task","id":"t1","input":[{"type":{"text":"x"},"valueSignature":` + bare + `}]}`, nil, "Signature in Task"},
		{"a Task with a Signature in a modifier extension of its input", `{"resourceType":"Task","id":"t1","input":[{"type":{"text":"x"},"valueString":"y","modifierExtension":[{"url":"urn:x","valueSignature":` + bare + `}]}]}`, nil, "Signature in Task"},
		{"a Task an object that is no Signature by its place does not mark", `{"resourceType":"Task","id":"t1","input":[{"type":{"text":"x"},"valueReference":{"reference":"Patient/1","type":"Patient"}}]}`, nil, ""},
		{"a Task whose contained resource is signed: not the Task", `{"resourceType":"Task","id":"t1","contained":[{"resourceType":"Basic","id":"b1","extension":[{"url":"urn:x","valueSignature":` + bare + `}]}]}`, nil, ""},
		{"that contained resource itself", `{"resourceType":"Task","id":"t1","contained":[{"resourceType":"Basic","id":"b1","extension":[{"url":"urn:x","valueSignature":` + bare + `}]}]}`, []string{"contained", "0"}, "Signature in Basic"},
		{"a Task a contained signed Provenance targets as #", `{"resourceType":"Task","id":"t1","contained":[` + signedProv("#") + `]}`, nil, "Provenance.signature"},
		{"a Bundle with its own signature named in another case", `{"resourceType":"Bundle","type":"collection","SIGNATURE":` + bare + `}`, nil, "Bundle.signature"},
	} {
		t.Run(row.name, func(t *testing.T) {
			d, err := Doc(NewBody([]byte(row.doc), OriginPeerFrame))
			if err != nil {
				t.Fatal(err)
			}
			n := d.Root()
			for _, p := range row.path {
				if d.Kind(n) == KindArray {
					n = d.Elems(n)[int(p[0]-'0')]
					continue
				}
				n, _ = d.Member(n, p)
			}
			if got := d.SignedCarrier(n); got != row.want {
				t.Fatalf("SignedCarrier = %q, want %q", got, row.want)
			}
		})
	}
}

// TestSignedCarrierAgreesWithApply holds SignedCarrier to Apply on random
// documents: for every value in one, a change Apply makes there (an addition
// inside an object or an array, a scalar replaced) is refused as signed
// content exactly when SignedCarrier names a signature covering the value,
// and the refusal names that signature.
func TestSignedCarrierAgreesWithApply(t *testing.T) {
	r := rand.New(rand.NewPCG(20261117, 1))
	values, signed := 0, 0
	for c := range 1500 {
		src := randomSignedDoc(r)
		body := NewBody([]byte(src), OriginPeerFrame)
		d, err := Doc(body)
		if err != nil {
			t.Fatalf("case %d: %v\n%s", c, err, src)
		}
		for i := 0; i < d.Len(); i++ {
			n := NodeID(i)
			var op Op
			switch d.Kind(n) {
			case KindObject:
				if _, taken := d.Member(n, "x-shn-probe"); taken {
					continue
				}
				op = d.InsertMember(n, "x-shn-probe", []byte(`1`))
			case KindArray:
				op = d.AppendElement(n, []byte(`1`))
			default:
				s, e := d.Span(n)
				if src[s:e] == `"x-shn-probe"` {
					continue
				}
				op = d.Replace(n, []byte(`"x-shn-probe"`))
			}
			values++
			want := d.SignedCarrier(n)
			_, err := Apply(body, "application/fhir+json", EditDTRCoverageObtain, op)
			var sce *SignedContentError
			switch {
			case want == "" && err != nil:
				t.Fatalf("case %d node %d: SignedCarrier names nothing, Apply refuses: %v\n%s", c, i, err, src)
			case want != "" && !errors.As(err, &sce):
				t.Fatalf("case %d node %d: SignedCarrier names %q, Apply: %v\n%s", c, i, want, err, src)
			case want != "" && sce.Carrier != want:
				t.Fatalf("case %d node %d: SignedCarrier names %q, Apply's refusal %q\n%s", c, i, want, sce.Carrier, src)
			}
			if want != "" {
				signed++
			}
		}
	}
	if signed == 0 || signed == values {
		t.Fatalf("%d of %d values covered: the documents do not vary", signed, values)
	}
	t.Logf("%d values, %d covered", values, signed)
}
