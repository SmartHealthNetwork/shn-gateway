//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// pendRekeyTask is a pended answer's PAS Task whose payer-url is written as
// value (a member and its JSON value, e.g. `"valueUrl":"https://..."`).
func pendRekeyTask(value string) string {
	return `{"fullUrl":"https://payer.example/fhir/Task/rekey","resource":{"resourceType":"Task","id":"rekey","status":"requested","intent":"order",` +
		`"code":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"attachment-request-questionnaire"}]},` +
		`"input":[{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url"}]},` + value + `}]}}`
}

// withEntry returns answer, a Bundle, with entry appended to its entry array,
// every other byte as it was.
func withEntry(t *testing.T, answer []byte, entry string) []byte {
	t.Helper()
	i := bytes.Index(answer, []byte(`"entry"`))
	j := bytes.IndexByte(answer[max(i, 0):], '[')
	if i < 0 || j < 0 {
		t.Fatalf("answer has no entry array: %s", answer)
	}
	at := i + j + 1
	return slices.Concat(answer[:at], []byte(entry+","), answer[at:])
}

// withBundleSignature returns answer, a Bundle, signed as a whole.
func withBundleSignature(t *testing.T, answer []byte) []byte {
	t.Helper()
	i := bytes.LastIndexByte(answer, '}')
	return slices.Concat(answer[:i], []byte(`,"signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-10-29T00:00:00Z","who":{"identifier":{"system":"urn:ietf:rfc:3986","value":"urn:oid:2.16.840.1.113883.6.300"}}}`), answer[i:])
}

// onlyPayerURLsRekeyed reports whether got is answer with every PAS Task's
// payer-url valueUrl set to want, at least one changed, and nothing else
// changed (byte-level fidelity is the relay library's own rows).
func onlyPayerURLsRekeyed(t *testing.T, answer, got []byte, want string) bool {
	t.Helper()
	var a, g any
	if json.Unmarshal(answer, &a) != nil || json.Unmarshal(got, &g) != nil {
		return false
	}
	changed := 0
	var walk func(v any)
	walk = func(v any) {
		switch o := v.(type) {
		case map[string]any:
			if o["resourceType"] == "Task" {
				inputs, _ := o["input"].([]any)
				for _, in := range inputs {
					m, _ := in.(map[string]any)
					typ, _ := m["type"].(map[string]any)
					codings, _ := typ["coding"].([]any)
					for _, c := range codings {
						cm, _ := c.(map[string]any)
						if cm["system"] == pasTempCodesSystem && cm["code"] == "payer-url" {
							if u, ok := m["valueUrl"].(string); ok && u != want {
								m["valueUrl"] = want
								changed++
							}
						}
					}
				}
			}
			for _, x := range o {
				walk(x)
			}
		case []any:
			for _, x := range o {
				walk(x)
			}
		}
	}
	walk(a)
	ab, _ := json.Marshal(a)
	gb, _ := json.Marshal(g)
	return changed > 0 && bytes.Equal(ab, gb) && len(got) != 0
}

// relayedPASAnswer is answer as a test-double provider gateway relays a
// PAS answer from the payer "payer": each PAS Task's payer-url valueUrl
// re-keyed to the gateway's return path for that payer, every other byte as
// it was. It is computed by substituting each payer-url's own JSON string
// token, independently of the gateway's edit, and fails when a payer-url's
// value also appears elsewhere in answer (the substitution would be ambiguous).
func relayedPASAnswer(t *testing.T, answer []byte) []byte {
	t.Helper()
	ret, err := cdexroute.ReturnBase(testDoubleIngressBaseURL, "payer")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if json.Unmarshal(answer, &doc) != nil {
		return answer
	}
	counts := map[string]int{}
	var walk func(v any)
	walk = func(v any) {
		switch o := v.(type) {
		case map[string]any:
			if o["resourceType"] == "Task" {
				inputs, _ := o["input"].([]any)
				for _, in := range inputs {
					m, _ := in.(map[string]any)
					typ, _ := m["type"].(map[string]any)
					codings, _ := typ["coding"].([]any)
					for _, c := range codings {
						cm, _ := c.(map[string]any)
						if cm["system"] == pasTempCodesSystem && cm["code"] == "payer-url" {
							if u, ok := m["valueUrl"].(string); ok {
								counts[u]++
							}
							break
						}
					}
				}
			}
			for _, x := range o {
				walk(x)
			}
		case []any:
			for _, x := range o {
				walk(x)
			}
		}
	}
	walk(doc)
	out := answer
	for v, n := range counts {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		tok := bytes.TrimSpace(b.Bytes())
		if got := bytes.Count(answer, tok); got != n {
			t.Fatalf("fixture: payer-url %s appears %d times, %d of them payer-urls", tok, got, n)
		}
		out = bytes.ReplaceAll(out, tok, []byte(`"`+ret+`"`))
	}
	return out
}

// withoutPayerURL is a carried leg whose payer answer names no payer-url: the
// submit and update answer with its Task's payer-url input taken out (the
// inquiry answer names none).
func withoutPayerURL(t *testing.T, leg carriedAnswerLeg) carriedAnswerLeg {
	t.Helper()
	const input = `{"type":{"coding":[{"system":"http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes","code":"payer-url","display":"Payer URL"}]},"valueUrl":"http://localhost:8081/fhir"},`
	base := leg.answer
	leg.answer = func(t *testing.T) []byte {
		a := bytes.Replace(base(t), []byte(input), nil, 1)
		if bytes.Contains(a, []byte("payer-url")) {
			t.Fatalf("fixture: %s's answer still names a payer-url", leg.leg)
		}
		return a
	}
	return leg
}

// signedOnce returns answer signed as a whole, unless it already is.
func signedOnce(t *testing.T, answer []byte) []byte {
	t.Helper()
	var top map[string]json.RawMessage
	if json.Unmarshal(answer, &top) == nil && top["signature"] != nil {
		return answer
	}
	return withBundleSignature(t, answer)
}

const pendRekeyBase = "https://provider-gw.example/davinci"

func rekeyRun(t *testing.T, leg carriedAnswerLeg, level ConformanceEnforcement, base string) (*httptest.ResponseRecorder, ExchangeRecord, []byte) {
	t.Helper()
	return rekeyRunTaking(t, leg, level, base, []string{legCDexSubmit})
}

// rekeyRunTaking is rekeyRun with the routed payer declaring legs.
func rekeyRunTaking(t *testing.T, leg carriedAnswerLeg, level ConformanceEnforcement, base string, legs []string) (*httptest.ResponseRecorder, ExchangeRecord, []byte) {
	t.Helper()
	_, rec, _, xr, answer := carriedAnswerRun(t, leg, level, "", shnsdk.NewFakeValidator(), func(g *Gateway) {
		g.cfg.IngressBaseURL = base
		payer, ok := g.cfg.Reg.Lookup("payer")
		if !ok {
			t.Fatal("fixture: the routed payer is not in the registry")
		}
		payer.Legs = legs
		g.cfg.Reg.Set("payer", payer)
	})
	return rec, xr, answer
}

// The provider's gateway re-keys a pended PAS answer's payer-url to its own
// return path for the payer the leg was routed to, on every carried PAS leg
// and at every conformance level, by default (D127): one value changes, the
// forward names the edit in X-SHN-Edited, and the exchange records E-10.
func TestPASIngress_PendPayerURLIsRekeyed(t *testing.T) {
	want, err := cdexroute.ReturnBase(pendRekeyBase, "payer")
	if err != nil {
		t.Fatal(err)
	}
	pend := func(t *testing.T) []byte {
		return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
	}
	for _, leg := range carriedAnswerLegs() {
		leg.answer = pend
		if leg.leg == "pas-claim-inquire" {
			leg.answer = func(t *testing.T) []byte {
				return withEntry(t, levelInquiryPayerAnswer(t), pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
			}
		}
		for _, level := range allLevels {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				rec, xr, answer := rekeyRun(t, leg, level, pendRekeyBase)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body)
				}
				if !onlyPayerURLsRekeyed(t, answer, rec.Body.Bytes(), want) {
					t.Fatalf("want every payer-url re-keyed to %s and nothing else changed, got %s", want, rec.Body)
				}
				want := "pas-pend-return-rekey"
				if leg.leg == "pas-claim-inquire" {
					// The inquiry answer is a signed Bundle.
					want += `; signed="Bundle.signature"`
				}
				if h := rec.Header().Values(EditedHeader); !slices.Equal(h, []string{want}) {
					t.Fatalf("%s = %q, want %q", EditedHeader, h, want)
				}
				if !slices.Equal(xr.AnswerEdits, []string{"E-10"}) || xr.AnswerOwnership != "edited" || len(xr.EditsNotMade) != 0 {
					t.Fatalf("record: answer edits %v ownership %q not made %v", xr.AnswerEdits, xr.AnswerOwnership, xr.EditsNotMade)
				}
			})
		}
	}
}

// A payer-url under a signature is re-keyed all the same (D127), and the
// header names every signature that covered it.
func TestPASIngress_SignedPendPayerURLIsRekeyedAndDisclosed(t *testing.T) {
	for _, leg := range carriedAnswerLegs() {
		base := leg.answer
		leg.answer = func(t *testing.T) []byte {
			return signedOnce(t, withEntry(t, base(t), pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`)))
		}
		for _, level := range allLevels {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				rec, xr, _ := rekeyRun(t, leg, level, pendRekeyBase)
				if rec.Code != http.StatusOK || bytes.Contains(rec.Body.Bytes(), []byte(`"valueUrl":"https://payer.example/fhir"`)) {
					t.Fatalf("answer %d %s, want the signed payer-url re-keyed", rec.Code, rec.Body)
				}
				want := []string{`pas-pend-return-rekey; signed="Bundle.signature"`}
				if h := rec.Header().Values(EditedHeader); !slices.Equal(h, want) {
					t.Fatalf("%s = %q, want %q", EditedHeader, h, want)
				}
				if !slices.Equal(xr.AnswerEdits, []string{"E-10"}) {
					t.Fatalf("answer edits %v", xr.AnswerEdits)
				}
			})
		}
	}
}

// An answer with no payer-url, or one already naming the return path, is
// relayed exactly: no header, no edit.
func TestPASIngress_AnswerWithoutPayerURLToRekeyIsExact(t *testing.T) {
	ret, _ := cdexroute.ReturnBase(pendRekeyBase, "payer")
	for name, entry := range map[string]string{
		"no Task":                     "",
		"payer-url already re-keyed":  pendRekeyTask(`"valueUrl":"` + ret + `"`),
		"payer-url of another system": strings.Replace(pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`), "CodeSystem/PASTempCodes\",\"code\":\"payer-url", "CodeSystem/other\",\"code\":\"payer-url", 1),
	} {
		for _, leg := range carriedAnswerLegs() {
			leg := withoutPayerURL(t, leg)
			if entry != "" {
				base := leg.answer
				leg.answer = func(t *testing.T) []byte { return withEntry(t, base(t), entry) }
			}
			t.Run(name+"/"+leg.leg, func(t *testing.T) {
				rec, xr, answer := rekeyRun(t, leg, EnforcementNone, pendRekeyBase)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want it relayed exactly", rec.Code, rec.Body)
				}
				if h := rec.Header().Values(EditedHeader); len(h) != 0 || len(xr.AnswerEdits) != 0 || len(xr.EditsNotMade) != 0 {
					t.Fatalf("header %q edits %v not made %v", h, xr.AnswerEdits, xr.EditsNotMade)
				}
			})
		}
	}
}

// A payer-url that is not a structural valueUrl is carried as sent at every
// level, and the exchange records why it was not re-keyed: unknown is not
// empty.
func TestPASIngress_NonstructuralPayerURLIsCarriedAndRecorded(t *testing.T) {
	for _, leg := range carriedAnswerLegs() {
		leg := withoutPayerURL(t, leg)
		base := leg.answer
		leg.answer = func(t *testing.T) []byte {
			return withEntry(t, base(t), pendRekeyTask(`"valueString":"https://payer.example/fhir"`))
		}
		for _, level := range allLevels {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				rec, xr, answer := rekeyRun(t, leg, level, pendRekeyBase)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want it relayed exactly", rec.Code, rec.Body)
				}
				if h := rec.Header().Values(EditedHeader); len(h) != 0 || len(xr.AnswerEdits) != 0 {
					t.Fatalf("header %q edits %v", h, xr.AnswerEdits)
				}
				if !slices.Equal(xr.EditsNotMade, []string{EditNotMadeNoStructuralPayerURL}) {
					t.Fatalf("not made %v, want %q", xr.EditsNotMade, EditNotMadeNoStructuralPayerURL)
				}
			})
		}
	}
}

// The value written is the return path for the payer the leg was routed to,
// never one the answer names: a payer-url naming another payer's return path
// is re-keyed to the routed payer's.
func TestPASIngress_RekeyValueIsTheRoutedPayers(t *testing.T) {
	other, _ := cdexroute.ReturnBase(pendRekeyBase, "other-payer")
	mine, _ := cdexroute.ReturnBase(pendRekeyBase, "payer")
	leg := carriedAnswerLegs()[0]
	leg.answer = func(t *testing.T) []byte {
		return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"`+other+`"`))
	}
	rec, _, _ := rekeyRun(t, leg, EnforcementNone, pendRekeyBase)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"valueUrl":"`+mine+`"`)) || bytes.Contains(rec.Body.Bytes(), []byte(other)) {
		t.Fatalf("answer %d %s, want the routed payer's return path", rec.Code, rec.Body)
	}
}

// A gateway with no return path to write cannot make the edit: the answer is
// refused as a fidelity refusal of the gateway's own edit, never relayed with
// the payer's address off the network.
func TestPASIngress_RekeyWithoutAReturnPathIsAFidelityRefusal(t *testing.T) {
	leg := carriedAnswerLegs()[0]
	leg.answer = func(t *testing.T) []byte {
		return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
	}
	rec, xr, _ := rekeyRun(t, leg, EnforcementNone, "")
	if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("payer.example/fhir")) {
		t.Fatalf("answer %d %s, want a 502 fidelity refusal", rec.Code, rec.Body)
	}
	wantRefusal(t, xr, http.StatusBadGateway, RefusedByProviderGateway, RefusalFidelity)
	if len(rec.Header().Values(EditedHeader)) != 0 {
		t.Fatal("a refusal names no edit")
	}
}

// An edit is justified only by carrying the message: a return path to a payer
// that does not take the leg always refuses, so the re-key is made only for a
// payer whose registry entry lists it. Any other payer's payer-url is carried as
// sent and recorded, at every level. Unknown is not empty: a payer with no list
// is not read as one that takes the leg.
func TestPASIngress_PayerThatDoesNotTakeTheLegKeepsItsPayerURL(t *testing.T) {
	const payerURL = `"valueUrl":"https://payer.example/fhir"`
	for name, legs := range map[string][]string{
		"a list without the leg": {"crd-order-select"},
		"an empty list":          {},
		"no list":                nil,
	} {
		for _, leg := range carriedAnswerLegs() {
			leg.answer = func(t *testing.T) []byte { return withEntry(t, levelPASPayerAnswer, pendRekeyTask(payerURL)) }
			if leg.leg == "pas-claim-inquire" {
				leg.answer = func(t *testing.T) []byte { return withEntry(t, levelInquiryPayerAnswer(t), pendRekeyTask(payerURL)) }
			}
			for _, level := range allLevels {
				t.Run(name+"/"+leg.leg+"/"+level.String(), func(t *testing.T) {
					rec, xr, answer := rekeyRunTaking(t, leg, level, pendRekeyBase, legs)
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
						t.Fatalf("answer %d %s, want the payer's answer exactly", rec.Code, rec.Body)
					}
					if h := rec.Header().Values(EditedHeader); len(h) != 0 {
						t.Fatalf("%s = %q on an answer not edited", EditedHeader, h)
					}
					if !slices.Equal(xr.EditsNotMade, []string{EditNotMadePayerDoesNotTake}) || len(xr.AnswerEdits) != 0 {
						t.Fatalf("not made %v, edits %v, want %q", xr.EditsNotMade, xr.AnswerEdits, EditNotMadePayerDoesNotTake)
					}
				})
			}
		}
	}
}

// Another payer taking the leg says nothing of the routed one's.
func TestPASIngress_RekeyReadsTheRoutedPayer(t *testing.T) {
	leg := carriedAnswerLegs()[0]
	leg.answer = func(t *testing.T) []byte {
		return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
	}
	_, rec, _, xr, answer := carriedAnswerRun(t, leg, EnforcementNone, "", shnsdk.NewFakeValidator(), func(g *Gateway) {
		g.cfg.IngressBaseURL = pendRekeyBase
		payer, ok := g.cfg.Reg.Lookup("payer")
		if !ok {
			t.Fatal("fixture: the routed payer is not in the registry")
		}
		other := payer
		other.ID = "payer-other"
		g.cfg.Reg.Set("payer-other", other)
		payer.Legs = nil
		g.cfg.Reg.Set("payer", payer)
	})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) || !slices.Equal(xr.EditsNotMade, []string{EditNotMadePayerDoesNotTake}) {
		t.Fatalf("answer %d %s, not made %v, want it carried", rec.Code, rec.Body, xr.EditsNotMade)
	}
}

// A payer that does not take the leg records nothing about a re-key the answer
// never called for, and a payer-url the gateway could not have re-keyed anyway
// is recorded as that. A mixed answer states both.
func TestPASIngress_NonTakerRecordsOnlyWhatTheAnswerCalledFor(t *testing.T) {
	const structural, nonstructural = `"valueUrl":"https://payer.example/fhir"`, `"valueString":"https://payer.example/fhir"`
	for name, row := range map[string]struct {
		entries []string
		want    []string
	}{
		"no payer-url":            {nil, nil},
		"not a structural value":  {[]string{nonstructural}, []string{EditNotMadeNoStructuralPayerURL}},
		"both, in the one answer": {[]string{nonstructural, structural}, []string{EditNotMadeNoStructuralPayerURL, EditNotMadePayerDoesNotTake}},
	} {
		for _, leg := range carriedAnswerLegs() {
			leg := withoutPayerURL(t, leg)
			base := leg.answer
			leg.answer = func(t *testing.T) []byte {
				a := base(t)
				for _, e := range row.entries {
					a = withEntry(t, a, pendRekeyTask(e))
				}
				return a
			}
			t.Run(name+"/"+leg.leg, func(t *testing.T) {
				rec, xr, answer := rekeyRunTaking(t, leg, EnforcementNone, pendRekeyBase, nil)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
					t.Fatalf("answer %d %s, want it relayed exactly", rec.Code, rec.Body)
				}
				if !slices.Equal(xr.EditsNotMade, row.want) {
					t.Fatalf("not made %v, want %v", xr.EditsNotMade, row.want)
				}
			})
		}
	}
}

// A payer this gateway's registry cannot name is unknown: its payer-url is
// carried and recorded, never re-keyed on a guess. It answered, so this is a
// registry that lost it after the leg was routed.
func TestPendReturnRekey_PayerNotInTheRegistryIsCarried(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{Reg: shnsdk.NewRegistry(), IngressBaseURL: pendRekeyBase, ExchangeObserved: got.observe}}
	answer := withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
	g.recordExchange(RouteDTR, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		carried, err := g.pendReturnRekey(r.Context(), answer, "application/fhir+json", "payer")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(relay.BytesForTest(carried), answer) {
			t.Fatalf("carried %s, want the payer's answer exactly", relay.BytesForTest(carried))
		}
		w.WriteHeader(http.StatusOK)
	})(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if xr := got.only(t); !slices.Equal(xr.EditsNotMade, []string{EditNotMadePayerDoesNotTake}) {
		t.Fatalf("not made %v", xr.EditsNotMade)
	}
}

// A gateway with no return path does not refuse an answer it would not edit.
func TestPASIngress_NoReturnPathRefusesNoAnswerItWouldNotEdit(t *testing.T) {
	leg := carriedAnswerLegs()[0]
	leg.answer = func(t *testing.T) []byte {
		return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
	}
	rec, xr, answer := rekeyRunTaking(t, leg, EnforcementNone, "", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) || !slices.Equal(xr.EditsNotMade, []string{EditNotMadePayerDoesNotTake}) {
		t.Fatalf("answer %d %s, not made %v", rec.Code, rec.Body, xr.EditsNotMade)
	}
}

// The edit is registered exactly where it is made.
func TestPendRekeyOwnership(t *testing.T) {
	own := relay.LegOwnership()
	for _, leg := range []string{"pas-claim", "pas-claim-inquire", "pas-claim-update"} {
		r := own[relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}]
		if !slices.Equal(r.Edits, []relay.EditID{relay.EditPASPendReturnRekey}) || !slices.Contains(r.Allowed, relay.OwnershipEdited) {
			t.Fatalf("%s answer rule %+v", leg, r)
		}
	}
	if by, ok := relay.EditReceivedBy(relayEdits, relay.EditPASPendReturnRekey); !ok || by != 0 {
		t.Fatalf("E-10 is received by no other party, got %v %v", by, ok)
	}
}

// An answer that cannot be read as JSON, relayed below strict, cannot be
// searched for a payer-url: it is carried as sent, and the exchange records
// that whether it named one is unknown, never that it named none.
func TestPASIngress_UnreadableAnswerIsCarriedAndRecorded(t *testing.T) {
	leg := carriedAnswerLegs()[0]
	leg.answer = func(*testing.T) []byte { return []byte(`not a JSON answer`) }
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			rec, xr, answer := rekeyRun(t, leg, level, pendRekeyBase)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
				t.Fatalf("answer %d %s, want it relayed exactly", rec.Code, rec.Body)
			}
			if h := rec.Header().Values(EditedHeader); len(h) != 0 || len(xr.AnswerEdits) != 0 {
				t.Fatalf("header %q edits %v", h, xr.AnswerEdits)
			}
			if !slices.Equal(xr.EditsNotMade, []string{EditNotMadeUnreadableAnswer}) {
				t.Fatalf("not made %v, want %q", xr.EditsNotMade, EditNotMadeUnreadableAnswer)
			}
		})
	}
}

// New's ingress-base check accepts exactly the bases a documentation return
// path can be built under, so a gateway serving the ingress without the test
// bypass always has a return path to write.
func TestIngressBaseCheckMatchesReturnPath(t *testing.T) {
	for _, base := range []string{
		"https://gw.test", "http://provider-ingress:8080", "https://gw.test/davinci/",
		"", "gw.test", "ftp://gw.test", "http://", "https://", "https://gw.test/fhir?tenant=1",
		"https://gw.test/fhir?", "https://gw.test/fhir#x", "https://gw.test/fhir#", "https://gw.test#", "https://u:p@gw.test", "https://u@gw.test/fhir", "https://gw.test/%zz",
	} {
		_, rerr := cdexroute.ReturnBase(base, "payer")
		cerr := checkIngressBase(base)
		if base == "" {
			cerr = errNoReturnPath // newIngressAuthServer refuses an empty base before the check
		}
		if (rerr == nil) != (cerr == nil) {
			t.Errorf("base %q: return path error %v, ingress check error %v", base, rerr, cerr)
		}
	}
}
