package relay

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// adaptiveQuestionnaire is a package answer's Questionnaire whose
// questionnaireAdaptive extension carries ext (the extension's members after
// its url), laid out with the spacing a payer's writer might use, plus a
// second, unrelated extension. sig, when set, is the Bundle's signature.
func adaptiveBundle(ext, sig string) Body {
	b := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q",` +
		`"extension":[ {"url" : "` + sdcAdaptiveExtension + `" ,  ` + ext + ` }, {"url":"http://example.org/other","valueUrl":"https://payer.example/other"}],` +
		`"status":"active"}}]` + sig + `}`
	return NewBody([]byte(b), OriginPeerFrame)
}

const (
	payerAddress   = `"https://payer.example/fhir/dtr/forms/a"`
	networkAddress = `"https://provider-gw.example/dtr/payer/payer-1/2.2/AbCdEfGhIjKlMnOpQrStUv/forms/a"`
)

var adaptivePath = []string{"entry", "0", "resource", "extension", "0"}

// The re-key (E-12) and its restore (E-13) change a questionnaireAdaptive
// address, and nothing else: a valueUrl's string, or, by one closed name
// change, the boolean true into the URL form (the re-key) and back (the
// restore). Every other byte of the extension is kept, and every other change
// either makes is refused, as is the form change made by any other edit.
func TestAdaptiveAddressEdits(t *testing.T) {
	url := func(v string) string { return `"valueUrl"  :  ` + v }
	boolean := func(v string) string { return `"valueBoolean"  :  ` + v }
	replaceAt := func(path []string, v string) func(d *Document) []Op {
		return func(d *Document) []Op { return []Op{d.Replace(at(t, d, path...), []byte(v))} }
	}
	// replaceExt replaces the whole extension with the original's bytes, ext
	// swapped for to: the one form a pair change writes.
	replaceExt := func(from, to string) func(d *Document) []Op {
		return func(d *Document) []Op {
			orig := `{"url" : "` + sdcAdaptiveExtension + `" ,  ` + from + ` }`
			return []Op{d.Replace(at(t, d, adaptivePath...), []byte(strings.Replace(orig, from, to, 1)))}
		}
	}
	valueURL := append(slices.Clone(adaptivePath), "valueUrl")
	type row struct {
		name   string
		body   Body
		edit   EditID
		ops    func(d *Document) []Op
		want   string // the extension's members after the url, "" = refused
		signed []string
	}
	rows := []row{
		{"E-12 re-keys a URL", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey, replaceAt(valueURL, networkAddress), url(networkAddress), nil},
		{"E-12 makes true explicit, keeping every other byte", adaptiveBundle(boolean("true"), ""), EditDTRAdaptiveAddressRekey,
			replaceExt(boolean("true"), url(networkAddress)), url(networkAddress), nil},
		{"E-13 restores a URL", adaptiveBundle(url(networkAddress), ""), EditDTRAdaptiveAddressRestore, replaceAt(valueURL, payerAddress), url(payerAddress), nil},
		{"E-13 restores true, keeping every other byte", adaptiveBundle(url(networkAddress), ""), EditDTRAdaptiveAddressRestore,
			replaceExt(url(networkAddress), boolean("true")), boolean("true"), nil},
		{"E-12 under the payer's Bundle signature", adaptiveBundle(url(payerAddress), `,"signature":{`+signatureElement+`}`), EditDTRAdaptiveAddressRekey,
			replaceAt(valueURL, networkAddress), url(networkAddress), []string{"Bundle.signature"}},
		{"E-12 makes true explicit under a signature", adaptiveBundle(boolean("true"), `,"signature":{`+signatureElement+`}`), EditDTRAdaptiveAddressRekey,
			replaceExt(boolean("true"), url(networkAddress)), url(networkAddress), []string{"Bundle.signature"}},
		{"E-13 restores true under a signature", adaptiveBundle(url(networkAddress), `,"signature":{`+signatureElement+`}`), EditDTRAdaptiveAddressRestore,
			replaceExt(url(networkAddress), boolean("true")), boolean("true"), []string{"Bundle.signature"}},

		// The refusals.
		{"E-12 on another extension's valueUrl", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey,
			replaceAt([]string{"entry", "0", "resource", "extension", "1", "valueUrl"}, networkAddress), "", nil},
		{"another name change", adaptiveBundle(boolean("true"), ""), EditDTRAdaptiveAddressRekey,
			replaceExt(boolean("true"), `"valueUri"  :  `+networkAddress), "", nil},
		{"the pair with another byte changed", adaptiveBundle(boolean("true"), ""), EditDTRAdaptiveAddressRekey,
			replaceExt(boolean("true"), url(networkAddress)+` `), "", nil},
		{"another edit making the pair", adaptiveBundle(boolean("true"), ""), EditDTRAnswerLineNames,
			replaceExt(boolean("true"), url(networkAddress)), "", nil},
		{"another edit restoring the pair", adaptiveBundle(url(networkAddress), ""), EditPASPendReturnRekey,
			replaceExt(url(networkAddress), boolean("true")), "", nil},
		{"E-12 turning false into a URL", adaptiveBundle(boolean("false"), ""), EditDTRAdaptiveAddressRekey,
			replaceExt(boolean("false"), url(networkAddress)), "", nil},
		{"E-12 turning a URL into a boolean", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey,
			replaceExt(url(payerAddress), boolean("true")), "", nil},
		{"E-13 turning a boolean into a URL", adaptiveBundle(boolean("true"), ""), EditDTRAdaptiveAddressRestore,
			replaceExt(boolean("true"), url(payerAddress)), "", nil},
		{"E-13 writing false", adaptiveBundle(url(networkAddress), ""), EditDTRAdaptiveAddressRestore,
			replaceExt(url(networkAddress), boolean("false")), "", nil},
		{"E-12 writing a number as the URL", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey, replaceAt(valueURL, `42`), "", nil},
		{"E-12 changing another value", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey,
			replaceAt([]string{"entry", "0", "resource", "status"}, `"retired"`), "", nil},
		{"E-12 removing the address", adaptiveBundle(url(payerAddress), ""), EditDTRAdaptiveAddressRekey,
			func(d *Document) []Op { return []Op{d.RemoveMember(at(t, d, adaptivePath...), "valueUrl")} }, "", nil},
		{"another edit on the signed address", adaptiveBundle(url(payerAddress), `,"signature":{`+signatureElement+`}`), EditDTRAnswerLineNames,
			replaceAt(valueURL, networkAddress), "", nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := Apply(r.body, fhirJSON, r.edit, r.ops(mustDoc(t, r.body))...)
			if r.want == "" {
				if err == nil {
					t.Fatalf("made: %s", BytesForTest(p))
				}
				rule := map[string]string{
					"another edit making the pair":     "a questionnaireAdaptive address is added, removed or changed;",
					"another edit restoring the pair":  "a questionnaireAdaptive address is added, removed or changed;",
					"E-12 writing a number as the URL": "may write only a string as a questionnaireAdaptive valueUrl",
					"E-12 removing the address":        "may only replace a questionnaireAdaptive address",
				}[r.name]
				if rule == "" {
					rule = "changes something other than a questionnaireAdaptive address"
				}
				if strings.HasPrefix(r.name, "another edit on the signed") {
					if !errors.Is(err, ErrSignedContent) {
						t.Fatalf("refused as %v, want signed content", err)
					}
				} else if !errors.Is(err, ErrInvalidEdit) || !strings.Contains(err.Error(), rule) {
					t.Fatalf("refused as %v, want the rule %q", err, rule)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if p.Ownership() != OwnershipEdited || !slices.Equal(p.Edits(), []EditID{r.edit}) {
				t.Fatalf("want Edited by %s, got %v %v", r.edit, p.Ownership(), p.Edits())
			}
			want := `{"url" : "` + sdcAdaptiveExtension + `" ,  ` + r.want + ` }`
			if got := string(BytesForTest(p)); !strings.Contains(got, want) {
				t.Fatalf("extension not written as\n%s\nin\n%s", want, got)
			}
			if !slices.Equal(p.SignedEdited(), r.signed) {
				t.Fatalf("signed %v, want %v", p.SignedEdited(), r.signed)
			}
		})
	}
}

// The extension is decided by structure: a questionnaireAdaptive extension on
// a Questionnaire at any depth (a QuestionnaireResponse's contained one among
// them) takes the edits; one on another resource does not.
func TestAdaptiveAddressByStructure(t *testing.T) {
	contained := NewBody([]byte(`{"resourceType":"QuestionnaireResponse","contained":[{"resourceType":"Questionnaire","id":"q",`+
		`"extension":[{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+payerAddress+`}]}],"questionnaire":"#q","status":"in-progress"}`), OriginPeerFrame)
	if _, err := Apply(contained, fhirJSON, EditDTRAdaptiveAddressRekey,
		mustDoc(t, contained).Replace(at(t, mustDoc(t, contained), "contained", "0", "extension", "0", "valueUrl"), []byte(networkAddress))); err != nil {
		t.Fatalf("a contained Questionnaire's address: %v", err)
	}
	other := NewBody([]byte(`{"resourceType":"PlanDefinition","extension":[{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+payerAddress+`}]}`), OriginPeerFrame)
	if _, err := Apply(other, fhirJSON, EditDTRAdaptiveAddressRekey,
		mustDoc(t, other).Replace(at(t, mustDoc(t, other), "extension", "0", "valueUrl"), []byte(networkAddress))); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("an extension on another resource: %v, want refused", err)
	}
}

// No change outside the two registered ones can change the number, order or
// form of the questionnaireAdaptive addresses: not another edit by any
// operation (a member removed and another inserted, a replaced ancestor that
// swaps two forms, an extension's url flipped, an extension appended or
// dropped), and not the re-key or the restore anywhere but a Questionnaire's
// own adaptive extension. Each refusal is pinned to the rule that makes it.
func TestAdaptiveAddressFormIsClosedToEveryOtherChange(t *testing.T) {
	const formRule = "a questionnaireAdaptive address is added, removed or changed;"
	ext := func(form string) string { return `{"url":"` + sdcAdaptiveExtension + `",` + form + `}` }
	bool_, url_ := `"valueBoolean":true`, `"valueUrl":`+payerAddress
	doc := func(s string) Body { return NewBody([]byte(s), OriginPeerFrame) }
	q := doc(`{"resourceType":"Questionnaire","extension":[` + ext(bool_) + `,{"url":"http://example.org/other","valueUrl":"https://x.example/o"}]}`)
	swap := doc(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire","extension":[` + ext(bool_) + `]}},` +
		`{"resource":{"resourceType":"Basic","extension":[` + ext(url_) + `]}}]}`)
	plan := doc(`{"resourceType":"PlanDefinition","extension":[{"url":"` + sdcAdaptiveExtension + `","valueBoolean":true}]}`)
	// The Questionnaire has an extension array too, so only the array's name
	// decides that a modifierExtension is not the address.
	modifier := doc(`{"resourceType":"Questionnaire","extension":[{"url":"http://example.org/other","valueUrl":"https://x.example/o"}],` +
		`"modifierExtension":[{"url":"` + sdcAdaptiveExtension + `","valueBoolean":true}]}`)
	other := doc(`{"resourceType":"Questionnaire","extension":[{"url":"http://example.org/other","valueBoolean":true}]}`)
	urlNum := doc(`{"resourceType":"Questionnaire","extension":[{"url":"` + sdcAdaptiveExtension + `","valueUrl":42}]}`)
	qr := doc(`{"resourceType":"QuestionnaireResponse","contained":[{"resourceType":"Questionnaire","id":"q","extension":[` + ext(url_) + `]}],"status":"completed"}`)
	for name, row := range map[string]struct {
		body Body
		edit EditID
		ops  func(d *Document) []Op
		want string // "" = made
	}{
		"another edit removing the boolean and inserting a URL": {q, EditDTRAnswerLineNames, func(d *Document) []Op {
			e := at(t, d, "extension", "0")
			return []Op{d.RemoveMember(e, "valueBoolean"), d.InsertMember(e, "valueUrl", []byte(networkAddress))}
		}, formRule},
		"another edit swapping two forms through an ancestor": {swap, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "entry"), []byte(`[{"resource":{"resourceType":"Questionnaire","extension":[`+ext(url_)+`]}},`+
				`{"resource":{"resourceType":"Basic","extension":[`+ext(bool_)+`]}}]`))}
		}, formRule},
		"another edit flipping an extension's url to the adaptive one": {q, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "extension", "1", "url"), []byte(`"`+sdcAdaptiveExtension+`"`))}
		}, formRule},
		"another edit appending an adaptive extension": {q, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.AppendElement(at(t, d, "extension"), []byte(ext(url_)))}
		}, formRule},
		"another edit dropping the adaptive extension": {q, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "extension"), []byte(`[{"url":"http://example.org/other","valueUrl":"https://x.example/o"}]`))}
		}, formRule},
		"another edit carrying a resource whose address is unchanged": {qr, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "status"), []byte(`"amended"`))}
		}, ""},
		"another edit replacing a resource that holds the address, re-pointing it": {qr, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "contained"), []byte(`[{"resourceType":"Questionnaire","id":"q","extension":[`+ext(`"valueUrl":`+networkAddress)+`]}]`))}
		}, formRule},
		"another edit replacing a resource that holds the address, address kept": {qr, EditDTRAnswerLineNames, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "contained"), []byte(`[{"resourceType":"Questionnaire","id":"q2","extension":[`+ext(url_)+`]}]`))}
		}, ""},
		"E-12's pair on another extension": {other, EditDTRAdaptiveAddressRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "extension", "0"), []byte(`{"url":"http://example.org/other","valueUrl":`+networkAddress+`}`))}
		}, "changes something other than a questionnaireAdaptive address"},
		"E-12's pair on a PlanDefinition": {plan, EditDTRAdaptiveAddressRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "extension", "0"), []byte(`{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+networkAddress+`}`))}
		}, "changes something other than a questionnaireAdaptive address"},
		"E-12's pair on a modifierExtension": {modifier, EditDTRAdaptiveAddressRekey, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "modifierExtension", "0"), []byte(`{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+networkAddress+`}`))}
		}, "changes something other than a questionnaireAdaptive address"},
		"E-13 restoring a valueUrl that is not a string": {urlNum, EditDTRAdaptiveAddressRestore, func(d *Document) []Op {
			return []Op{d.Replace(at(t, d, "extension", "0"), []byte(`{"url":"`+sdcAdaptiveExtension+`","valueBoolean":true}`))}
		}, "changes something other than a questionnaireAdaptive address"},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Apply(row.body, fhirJSON, row.edit, row.ops(mustDoc(t, row.body))...)
			if row.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("made: %s", BytesForTest(p))
			}
			if !errors.Is(err, ErrInvalidEdit) || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("refused as %v, want the rule %q", err, row.want)
			}
		})
	}
}

// Only the re-key and the restore may change an adaptive address's value:
// every other edit in the closed set, made on the address itself or on a
// resource that holds it, is refused when the address's bytes would differ,
// whatever value it writes. So the restore's guarantee holds against the whole
// edit set, not only against E-12 and E-13's own code.
func TestAdaptiveAddressValueIsClosedToEveryOtherEdit(t *testing.T) {
	const rule = "a questionnaireAdaptive address is added, removed or changed;"
	body := NewBody([]byte(`{"resourceType":"QuestionnaireResponse","contained":[{"resourceType":"Questionnaire","id":"q","extension":[`+
		`{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+payerAddress+`},{"url":"`+sdcAdaptiveExtension+`","valueBoolean":true}]}],"status":"completed"}`), OriginPeerFrame)
	for _, id := range EditIDs() {
		// A transform and a header edit are never made by Apply (each has its
		// own refusal rows).
		if adaptiveEdits[id] || slices.Contains(TransformEditIDs(), id) || slices.Contains(headerEdits, id) {
			continue
		}
		for name, ops := range map[string]func(d *Document) []Op{
			"the URL re-pointed": func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "contained", "0", "extension", "0", "valueUrl"), []byte(networkAddress))}
			},
			"the boolean turned false": func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "contained", "0", "extension", "1", "valueBoolean"), []byte(`false`))}
			},
			"the URL re-pointed through the resource holding it": func(d *Document) []Op {
				return []Op{d.Replace(at(t, d, "contained"), []byte(`[{"resourceType":"Questionnaire","id":"q","extension":[`+
					`{"url":"`+sdcAdaptiveExtension+`","valueUrl":"https://payer.example/fhir/dtr/forms/b"},{"url":"`+sdcAdaptiveExtension+`","valueBoolean":true}]}]`))}
			},
		} {
			t.Run(string(id)+"/"+name, func(t *testing.T) {
				p, err := Apply(body, fhirJSON, id, ops(mustDoc(t, body))...)
				if err == nil {
					t.Fatalf("made: %s", BytesForTest(p))
				}
				if !errors.Is(err, ErrInvalidEdit) || !strings.Contains(err.Error(), rule) {
					t.Fatalf("refused as %v, want the rule %q", err, rule)
				}
			})
		}
	}
}

// E-12's string replace of an address's valueUrl frees only those bytes:
// another edit in the same change cannot add a member to that extension or
// change its other member.
func TestAdaptiveAddressURLReplaceFreesOnlyTheURL(t *testing.T) {
	const rule = "a questionnaireAdaptive address is added, removed or changed;"
	one := NewBody([]byte(`{"resourceType":"Questionnaire","extension":[{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+payerAddress+`}]}`), OriginPeerFrame)
	both := NewBody([]byte(`{"resourceType":"Questionnaire","extension":[{"url":"`+sdcAdaptiveExtension+`","valueUrl":`+payerAddress+`,"valueBoolean":true}]}`), OriginPeerFrame)
	for name, row := range map[string]struct {
		body  Body
		other func(d *Document) Op
	}{
		"another edit inserting a member beside it": {one, func(d *Document) Op {
			return d.InsertMember(at(t, d, "extension", "0"), "valueBoolean", []byte(`false`))
		}},
		"another edit changing its other member": {both, func(d *Document) Op {
			return d.Replace(at(t, d, "extension", "0", "valueBoolean"), []byte(`false`))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			d := mustDoc(t, row.body)
			p, err := ApplyChanges(row.body, fhirJSON,
				Change{Edit: EditDTRAdaptiveAddressRekey, Ops: []Op{d.Replace(at(t, d, "extension", "0", "valueUrl"), []byte(networkAddress))}},
				Change{Edit: EditDTRAnswerLineNames, Ops: []Op{row.other(d)}})
			if err == nil {
				t.Fatalf("made: %s", BytesForTest(p))
			}
			if !errors.Is(err, ErrInvalidEdit) || !strings.Contains(err.Error(), rule) {
				t.Fatalf("refused as %v, want the rule %q", err, rule)
			}
		})
	}
	d := mustDoc(t, both)
	if _, err := Apply(both, fhirJSON, EditDTRAdaptiveAddressRekey, d.Replace(at(t, d, "extension", "0", "valueUrl"), []byte(networkAddress))); err != nil {
		t.Fatalf("the URL alone: %v", err)
	}
}

// AdaptiveForm builds the one form change from the body's own bytes: the
// re-key's explicit address, and the restore back to the payer's true, byte
// for byte, the payer's spacing kept.
func TestAdaptiveForm_RoundTripsByteForByte(t *testing.T) {
	orig := adaptiveBundle(`"valueBoolean"  :  true`, "")
	d := mustDoc(t, orig)
	op, err := d.AdaptiveForm(at(t, d, adaptivePath...), "https://provider-gw.example/dtr/fetch-server/payer-1/2.2/AbCdEfGhIjKlMnOpQrStUv")
	if err != nil {
		t.Fatal(err)
	}
	rekeyed, err := Apply(orig, fhirJSON, EditDTRAdaptiveAddressRekey, op)
	if err != nil {
		t.Fatalf("re-key: %v", err)
	}
	back := NewBody(BytesForTest(rekeyed), OriginPeerFrame)
	bd := mustDoc(t, back)
	op, err = bd.AdaptiveForm(at(t, bd, adaptivePath...), "")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Apply(back, fhirJSON, EditDTRAdaptiveAddressRestore, op)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, want := string(BytesForTest(restored)), string(adaptiveBundleBytes(`"valueBoolean"  :  true`)); got != want {
		t.Fatalf("restored\n%s\nwant\n%s", got, want)
	}
	if _, err := d.AdaptiveForm(at(t, d, "entry", "0", "resource"), ""); !errors.Is(err, ErrInvalidEdit) {
		t.Fatalf("no valueUrl to restore: %v", err)
	}
}

func adaptiveBundleBytes(ext string) []byte {
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q",` +
		`"extension":[ {"url" : "` + sdcAdaptiveExtension + `" ,  ` + ext + ` }, {"url":"http://example.org/other","valueUrl":"https://payer.example/other"}],` +
		`"status":"active"}}]}`)
}

// JSONString writes an address back as the bytes it was read as: no HTML
// escaping of &, < or >, which json.Marshal would write as & and the
// like, so a restored address with one is byte-exact.
func TestJSONStringWritesNoHTMLEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"https://payer.example/a&b": `"https://payer.example/a&b"`,
		"<x>":                       `"<x>"`,
		`a"b\c`:                     `"a\"b\\c"`,
	} {
		got, err := JSONString(in)
		if err != nil || string(got) != want {
			t.Errorf("JSONString(%q) = %s %v, want %s", in, got, err, want)
		}
	}
}
