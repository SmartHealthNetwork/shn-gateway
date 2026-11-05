package relay

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// pendTask is a pended PAS answer's Task: input 0 is its payer-url, input 1
// another url-valued input. The %s marks are where a test puts a signature.
const pendTask = `{"resourceType":"Task","id":"t1",%s"status":"requested",
  "input":[
    {"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},
     "valueUrl":"https://payer.example/fhir"},
    {"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"questionnaire-context"}]},
     "valueUrl":"https://payer.example/Questionnaire/q"},
    {"type":{"coding":[{"system":"http://example.org/not-pas","code":"payer-url"}]},
     "valueUrl":"https://payer.example/other"},
    {"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},
     "valueString":"https://payer.example/string-form"}]}`

const signatureElement = `"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-10-29T00:00:00Z","who":{"reference":"Organization/payer"}`

func pendBundle(bundleSig, taskSig, provenance string) []byte {
	task := strings.Replace(pendTask, "%s", taskSig, 1)
	b := `{"resourceType":"Bundle","type":"collection","entry":[` +
		`{"fullUrl":"https://payer.example/fhir/ClaimResponse/cr1","resource":{"resourceType":"ClaimResponse","id":"cr1","status":"active"}},` +
		`{"fullUrl":"https://payer.example/fhir/Task/t1","resource":` + task + `}` + provenance + `]` + bundleSig + `}`
	return []byte(b)
}

const rekeyed = `"https://provider-gw.example/cdex/payer/payer-1"`

// TestSignedContentRekeyExemption: the PAS pend return-path re-key (E-10) and
// the attachment-request Task's (E-11) are made even where a signature covers
// the payer-url they change (D127). With the questionnaireAdaptive address's
// re-key and restore (E-12, E-13, D128; their rows are in
// adaptive_test.go), they are the only edits that are: every other edit of
// signed content is still refused, and each of the two is refused on any other
// signed value.
func TestSignedContentRekeyExemption(t *testing.T) {
	if got := slices.Sorted(maps.Keys(signedExempt)); !slices.Equal(got, []EditID{EditPASPendReturnRekey, EditCDexTaskReturnRekey, EditDTRAdaptiveAddressRekey, EditDTRAdaptiveAddressRestore}) {
		t.Fatalf("signed-content exempt edits %v, want exactly E-10, E-11, E-12 and E-13", got)
	}
	bundleSigned := NewBody(pendBundle(`,"signature":{`+signatureElement+`}`, "", ""), OriginPeerFrame)
	provSigned := NewBody(pendBundle("", "", `,{"resource":{"resourceType":"Provenance","target":[{"reference":"Task/t1"}],`+
		`"recorded":"2026-10-29T00:00:00Z","signature":[{`+signatureElement+`}]}}`), OriginPeerFrame)
	elemSigned := NewBody(pendBundle("", `"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`, ""), OriginPeerFrame)
	unsigned := NewBody(pendBundle("", "", ""), OriginPeerFrame)
	bothSigned := NewBody(pendBundle(`,"signature":{`+signatureElement+`}`, `"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`, ""), OriginPeerFrame)

	payerURL := []string{"entry", "1", "resource", "input", "0", "valueUrl"}
	type row struct {
		name    string
		body    Body
		edit    EditID
		ops     func(d *Document) []Op
		carrier string // "" = must be made
		signed  []string
	}
	replace := func(path []string, v string) func(d *Document) []Op {
		return func(d *Document) []Op { return []Op{d.Replace(at(t, d, path...), []byte(v))} }
	}
	rows := []row{
		// A whole-Bundle signature is one signature: its own Signature
		// element is not counted again as "Signature in Bundle".
		{"E-10 on the payer-url under Bundle.signature", bundleSigned, EditPASPendReturnRekey, replace(payerURL, rekeyed), "", []string{"Bundle.signature"}},
		// Two signatures over the payer-url are each named once.
		{"E-10 on the payer-url under Bundle.signature and a Task Signature", bothSigned, EditPASPendReturnRekey, replace(payerURL, rekeyed), "", []string{"Bundle.signature", "Signature in Task"}},
		{"E-10 on the payer-url of a Provenance-signed Task", provSigned, EditPASPendReturnRekey, replace(payerURL, rekeyed), "", []string{"Provenance.signature"}},
		{"E-10 on the payer-url of a Task holding a Signature", elemSigned, EditPASPendReturnRekey, replace(payerURL, rekeyed), "", []string{"Signature in Task"}},
		{"E-10 on unsigned content", unsigned, EditPASPendReturnRekey, replace(payerURL, rekeyed), "", nil},

		{"E-10 on another signed value of the Task", bundleSigned, EditPASPendReturnRekey,
			replace([]string{"entry", "1", "resource", "status"}, `"cancelled"`), "Bundle.signature", nil},
		{"E-10 on a signed url input that is not payer-url", bundleSigned, EditPASPendReturnRekey,
			replace([]string{"entry", "1", "resource", "input", "1", "valueUrl"}, rekeyed), "Bundle.signature", nil},
		{"E-10 on a signed payer-url code of another system", bundleSigned, EditPASPendReturnRekey,
			replace([]string{"entry", "1", "resource", "input", "2", "valueUrl"}, rekeyed), "Bundle.signature", nil},
		{"E-10 on a signed payer-url that is not a valueUrl", bundleSigned, EditPASPendReturnRekey,
			replace([]string{"entry", "1", "resource", "input", "3", "valueString"}, rekeyed), "Bundle.signature", nil},
		{"E-10 writing a signed payer-url that is not a string", bundleSigned, EditPASPendReturnRekey,
			replace(payerURL, `{"url":"x"}`), "Bundle.signature", nil},
		{"E-10 on a signed payer-url outside a Task", bundleSigned, EditPASPendReturnRekey,
			replace([]string{"entry", "0", "resource", "id"}, rekeyed), "Bundle.signature", nil},
		{"E-10 removing the signed payer-url", bundleSigned, EditPASPendReturnRekey, func(d *Document) []Op {
			return []Op{d.RemoveMember(at(t, d, "entry", "1", "resource", "input", "0"), "valueUrl")}
		}, "Bundle.signature", nil},
		{"E-10 adding a member beside the signed payer-url", bundleSigned, EditPASPendReturnRekey, func(d *Document) []Op {
			return []Op{d.InsertMember(at(t, d, "entry", "1", "resource", "input", "0"), "valueString", []byte(rekeyed))}
		}, "Bundle.signature", nil},
		{"E-10 on the covered payer-url with a covered sibling replaced", provSigned, EditPASPendReturnRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, payerURL...), []byte(rekeyed)), d.Replace(at(t, d, "entry", "1", "resource", "status"), []byte(`"x"`))}
		}, "Provenance.signature", nil},
	}
	// The attachment-request Task's re-key (E-11) is exempt at exactly the
	// same place as E-10, on the same terms: every E-10 row holds for it.
	for _, r := range slices.Clone(rows) {
		if r.edit == EditPASPendReturnRekey {
			r.name = strings.Replace(r.name, "E-10", "E-11", 1)
			r.edit = EditCDexTaskReturnRekey
			rows = append(rows, r)
		}
	}
	// Every other edit made by ApplyChanges, on exactly the value the exempt
	// edits may change, is still refused (D127's row).
	for _, id := range EditIDs() {
		if _, exempt := signedExempt[id]; exempt || slices.Contains(TransformEditIDs(), id) {
			continue
		}
		rows = append(rows, row{"other edit " + string(id) + " on the signed payer-url", bundleSigned, id, replace(payerURL, rekeyed), "Bundle.signature", nil})
		rows = append(rows, row{"other edit " + string(id) + " on the Provenance-signed payer-url", provSigned, id, replace(payerURL, rekeyed), "Provenance.signature", nil})
		rows = append(rows, row{"other edit " + string(id) + " on the Signature-holding Task's payer-url", elemSigned, id, replace(payerURL, rekeyed), "Signature in Task", nil})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, r.ops(mustDoc(t, r.body))...)
			if r.carrier == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{r.edit}) {
					t.Fatalf("want Edited by %s, got %v %v", r.edit, p.Ownership(), p.Edits())
				}
				if !slices.Equal(p.SignedEdited(), r.signed) {
					t.Fatalf("SignedEdited = %v, want %v", p.SignedEdited(), r.signed)
				}
				out := string(mustTransmit(t, p))
				if !strings.Contains(out, `"valueUrl":`+rekeyed) || strings.Contains(out, `"valueUrl":"https://payer.example/fhir"`) {
					t.Fatalf("payer-url not re-keyed: %s", out)
				}
				return
			}
			var se *SignedContentError
			if !errors.As(err, &se) || !errors.Is(err, ErrSignedContent) {
				t.Fatalf("want *SignedContentError, got %v", err)
			}
			if se.Edit != r.edit || se.Carrier != r.carrier {
				t.Fatalf("got (%s, %s), want (%s, %s)", se.Edit, se.Carrier, r.edit, r.carrier)
			}
		})
	}
	t.Run("another change in the same set touching signed content is refused", func(t *testing.T) {
		d := mustDoc(t, bundleSigned)
		_, err := ApplyChanges(bundleSigned, fhirJSON,
			Change{Edit: EditPASPendReturnRekey, Ops: []Op{d.Replace(at(t, d, payerURL...), []byte(rekeyed))}},
			Change{Edit: EditPayorEdgeRestamp, Ops: []Op{d.Replace(at(t, d, "entry", "0", "resource", "id"), []byte(`"x"`))}})
		var se *SignedContentError
		if !errors.As(err, &se) || se.Edit != EditPayorEdgeRestamp {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a same-value re-key is no edit", func(t *testing.T) {
		d := mustDoc(t, bundleSigned)
		p, err := Apply(bundleSigned, fhirJSON, EditPASPendReturnRekey, d.Replace(at(t, d, payerURL...), []byte(`"https://payer.example/fhir"`)))
		if err != nil || p.Ownership() != OwnershipRelayed || p.SignedEdited() != nil {
			t.Fatalf("got %v %v %v", p.Ownership(), p.SignedEdited(), err)
		}
	})
	t.Run("SignedEdited returns a copy", func(t *testing.T) {
		d := mustDoc(t, bundleSigned)
		p, err := Apply(bundleSigned, fhirJSON, EditPASPendReturnRekey, d.Replace(at(t, d, payerURL...), []byte(rekeyed)))
		if err != nil {
			t.Fatal(err)
		}
		p.SignedEdited()[0] = "X"
		if p.SignedEdited()[0] != "Bundle.signature" || len(p.SignedEdited()) != 1 {
			t.Fatal("SignedEdited must return a copy")
		}
	})
}

// The exemption's place is structural all the way down: a payer-url-typed
// valueUrl outside a Task's input array, or one that is not a string, is no
// place E-10 or E-11 may change on signed content.
func TestSignedContentRekeyExemptionPlace(t *testing.T) {
	const payerURLInput = `{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},"valueUrl":%s}`
	signed := func(resource string) Body {
		return NewBody([]byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":`+resource+`}],"signature":{`+signatureElement+`}}`), OriginPeerFrame)
	}
	in := func(v string) string { return strings.Replace(payerURLInput, "%s", v, 1) }
	for _, row := range []struct {
		name string
		body Body
		path []string
	}{
		{"an input array of a resource that is not a Task", signed(`{"resourceType":"Basic","input":[` + in(`"https://payer.example/fhir"`) + `]}`),
			[]string{"entry", "0", "resource", "input", "0", "valueUrl"}},
		{"a Task's output array", signed(`{"resourceType":"Task","output":[` + in(`"https://payer.example/fhir"`) + `]}`),
			[]string{"entry", "0", "resource", "output", "0", "valueUrl"}},
		{"a payer-url valueUrl that is not a string", signed(`{"resourceType":"Task","input":[` + in(`5`) + `]}`),
			[]string{"entry", "0", "resource", "input", "0", "valueUrl"}},
	} {
		for _, id := range []EditID{EditPASPendReturnRekey, EditCDexTaskReturnRekey} {
			t.Run(string(id)+" "+row.name, func(t *testing.T) {
				d := mustDoc(t, row.body)
				_, err := Apply(row.body, fhirJSON, id, d.Replace(at(t, d, row.path...), []byte(rekeyed)))
				var se *SignedContentError
				if !errors.As(err, &se) || se.Edit != id || se.Carrier != "Bundle.signature" {
					t.Fatalf("want %s refused on Bundle.signature, got %v", id, err)
				}
			})
		}
	}
	// A carrier repeats a resource type only when it reads as a FHIR type
	// name: anything else the payload calls itself is not carried into a
	// header or a refusal.
	t.Run("a signature in a resource whose type is no FHIR type name", func(t *testing.T) {
		b := NewBody([]byte(`{"resourceType":"X\", y=\"z","sig":{`+signatureElement+`},"contained":[{"resourceType":"Task","input":[`+
			in(`"https://payer.example/fhir"`)+`]}]}`), OriginPeerFrame)
		d := mustDoc(t, b)
		p, err := Apply(b, fhirJSON, EditPASPendReturnRekey, d.Replace(at(t, d, "contained", "0", "input", "0", "valueUrl"), []byte(rekeyed)))
		if err != nil || !slices.Equal(p.SignedEdited(), []string{"Signature"}) {
			t.Fatalf("got %v %v, want the re-key made under carrier Signature", p.SignedEdited(), err)
		}
	})
}

// TestSignedContentRekeyExemption_BareTask: a payer's attachment-request Task
// travels alone, not in a Bundle. E-11 re-keys its signed payer-url, the Task
// itself holding the Signature, and is refused on any other signed value of
// it.
func TestSignedContentRekeyExemption_BareTask(t *testing.T) {
	signed := NewBody([]byte(strings.Replace(pendTask, "%s",
		`"extension":[{"url":"http://example.org/sig","valueSignature":{`+signatureElement+`}}],`, 1)), OriginPeerFrame)
	d := mustDoc(t, signed)
	p, err := Apply(signed, fhirJSON, EditCDexTaskReturnRekey, d.Replace(at(t, d, "input", "0", "valueUrl"), []byte(rekeyed)))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if !slices.Equal(p.Edits(), []EditID{EditCDexTaskReturnRekey}) || !slices.Equal(p.SignedEdited(), []string{"Signature in Task"}) {
		t.Fatalf("edits %v, signed %v", p.Edits(), p.SignedEdited())
	}
	if out := string(mustTransmit(t, p)); !strings.Contains(out, `"valueUrl":`+rekeyed) {
		t.Fatalf("payer-url not re-keyed: %s", out)
	}
	for name, path := range map[string][]string{
		"another url input":                  {"input", "1", "valueUrl"},
		"a payer-url code of another system": {"input", "2", "valueUrl"},
		"a payer-url that is not a valueUrl": {"input", "3", "valueString"},
		"the Task's status":                  {"status"},
	} {
		t.Run(name, func(t *testing.T) {
			d := mustDoc(t, signed)
			_, err := Apply(signed, fhirJSON, EditCDexTaskReturnRekey, d.Replace(at(t, d, path...), []byte(rekeyed)))
			var se *SignedContentError
			if !errors.As(err, &se) || se.Edit != EditCDexTaskReturnRekey || se.Carrier != "Signature in Task" {
				t.Fatalf("want E-11 refused on signed %s, got %v", name, err)
			}
		})
	}
}
