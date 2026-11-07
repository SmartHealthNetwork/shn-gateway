//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/dtraddress"
)

const (
	rekeyBase    = "https://payer.example/fhir/dtr"
	rekeyIngress = "https://provider-gw.example"
)

var (
	rekeyTagA    = strings.Repeat("A", dtraddress.TagLen)
	rekeyTagTrue = strings.Repeat("T", dtraddress.TagLen)
)

// adaptiveRekeyGateway is a provider's gateway whose payer's gateway declares
// v1opsub (when restores), and the payer's id.
func adaptiveRekeyGateway(t *testing.T, restores bool) (*Gateway, string) {
	t.Helper()
	env := newInProcessExchange(t)
	g := env.originator
	g.cfg.IngressBaseURL = rekeyIngress + "/"
	e, _ := g.cfg.Reg.Lookup(env.payerID)
	if restores {
		e.RequestFrames = append(slices.Clone(e.RequestFrames), shnsdk.RequestFrameV1OpSub)
	}
	g.cfg.Reg.Set(env.payerID, e)
	return g, env.payerID
}

func adaptiveQ(id, ext string) string {
	return `{"resourceType":"Questionnaire","id":"` + id + `","status":"active","extension":[{"url":"` + questionnaireAdaptiveExtension + `",` + ext + `}]}`
}

func packageOf(entries ...string) []byte {
	var es []string
	for _, e := range entries {
		es = append(es, `{"resource":`+e+`}`)
	}
	return []byte(`{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(es, ",") + `]}`)
}

func statedTags(base, tags string) statedAnswer {
	a := map[string]string{}
	if base != "" {
		a[shnsdk.FrameHeaderOperationBase] = base
	}
	if tags != "" {
		a[shnsdk.FrameHeaderOperationAddressTags] = tags
	}
	return statedAnswer{status: 200, address: a}
}

// The re-key (E-12) of a package answer: a tagged address on a top-level
// Questionnaire, and a contained copy of it, are re-keyed to this gateway's
// per-payer address with the remainder kept; a true is made explicit as the
// payer's DTR endpoint; a false and an untagged contained address are
// carried as sent, the latter recorded.
func TestDTRRekey_APackageAnswer(t *testing.T) {
	g, payer := adaptiveRekeyGateway(t, true)
	contained := `{"resourceType":"QuestionnaireResponse","status":"in-progress","contained":[` +
		adaptiveQ("c", `"valueUrl":"`+rekeyBase+`/forms/a"`) + `,` + adaptiveQ("d", `"valueUrl":"`+rekeyBase+`/forms/b"`) + `]}`
	answer := packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`/forms/a"`), contained, adaptiveQ("t", `"valueBoolean":true`), adaptiveQ("f", `"valueBoolean":false`))
	x := &exchangeRecorder{}
	ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
	p := g.rekeyAnswer(ctx, relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), "application/fhir+json"),
		rekeyContext{payer: payer, line: "2.2", stated: statedTags(rekeyBase, "/forms/a="+rekeyTagA+" *="+rekeyTagTrue)})
	got := string(relay.BytesForTest(p))
	rekeyed := rekeyIngress + "/dtr/payer/" + payer + "/2.2/" + rekeyTagA + "/forms/a"
	explicit := rekeyIngress + "/dtr/fetch-server/" + payer + "/2.2/" + rekeyTagTrue
	want := strings.NewReplacer(
		`"valueUrl":"`+rekeyBase+`/forms/a"`, `"valueUrl":"`+rekeyed+`"`,
		`"valueBoolean":true`, `"valueUrl":"`+explicit+`"`,
	).Replace(string(answer))
	if got != want {
		t.Fatalf("re-keyed\n%s\nwant\n%s", got, want)
	}
	if !slices.Equal(p.Edits(), []relay.EditID{relay.EditDTRAdaptiveAddressRekey}) {
		t.Fatalf("edits %v", p.Edits())
	}
	if !slices.Equal(x.rec.EditsNotMade, []string{EditNotMadeRekeyContainedInPackageResponse}) {
		t.Fatalf("not made %v", x.rec.EditsNotMade)
	}
}

// Each address the re-key cannot make is carried as sent, and the reason is
// recorded.
func TestDTRRekey_EveryReasonItIsNotMade(t *testing.T) {
	top := func(v string) []byte { return packageOf(adaptiveQ("a", `"valueUrl":"`+v+`"`)) }
	round := dtraddress.Subpath{Family: dtraddress.FamilyPayer, Line: "2.2", Tag: rekeyTagA, Remainder: "/forms/a"}
	for name, row := range map[string]struct {
		restores bool
		answer   []byte
		stated   statedAnswer
		rc       func(*rekeyContext)
		want     string
	}{
		"toward an older payer gateway": {false, top(rekeyBase + "/forms/a"), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyPayerGatewayOlder},
		"no stated base":                {true, top(rekeyBase + "/forms/a"), statedTags("", "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyNotUnderBase},
		"elsewhere":                     {true, top("https://elsewhere.example/forms/a"), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyNotUnderBase},
		"no tag for it":                 {true, top(rekeyBase + "/forms/a"), statedTags(rekeyBase, "/forms/z="+rekeyTagA), nil, EditNotMadeRekeyNotUnderBase},
		"an escaped address": {true, packageOf(adaptiveQ("a", `"valueUrl":"`+strings.ReplaceAll(rekeyBase, "/", `\/`)+`\/forms\/a"`)),
			statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyEscapedAddress},
		"the base spelled differently": {true, top("https://PAYER.example/fhir/dtr/forms/a"), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyBaseSpelledDifferently},
		"an unclean remainder":         {true, top(rekeyBase + "/forms/a/"), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyUncleanRemainder},
		"an unreadable tags header":    {true, top(rekeyBase + "/forms/a"), statedTags(rekeyBase, "/forms/a=short"), nil, EditNotMadeRekeyUnreadableTag},
		"not an address":               {true, packageOf(adaptiveQ("a", `"valueUrl":42`)), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyUnreadableAddress},
		"a request carrying a Questionnaire": {true, top(rekeyBase + "/forms/a"), statedTags(rekeyBase, "/forms/a="+rekeyTagA),
			func(rc *rekeyContext) { rc.requestCarriedQuestionnaire = true }, EditNotMadeRekeyRequestCarriedAQuestionnaire},
		"a round's answer naming another address": {true, top(rekeyBase + "/forms/b"), statedTags(rekeyBase, "/forms/b="+rekeyTagA),
			func(rc *rekeyContext) { rc.nextQuestion, rc.round = true, &round }, EditNotMadeRekeyNotTheRoundsAddress},
		"an unreadable answer": {true, []byte(`not json`), statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil, EditNotMadeRekeyUnreadableAnswer},
	} {
		t.Run(name, func(t *testing.T) {
			g, payer := adaptiveRekeyGateway(t, row.restores)
			x := &exchangeRecorder{}
			ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
			rc := rekeyContext{payer: payer, line: "2.2", stated: row.stated}
			if row.rc != nil {
				row.rc(&rc)
			}
			p := g.rekeyAnswer(ctx, relay.Exact(relay.NewBody(row.answer, relay.OriginPeerFrame), "application/fhir+json"), rc)
			if got := relay.BytesForTest(p); string(got) != string(row.answer) || p.Ownership() != relay.OwnershipRelayed {
				t.Fatalf("changed: %s", got)
			}
			if !slices.Equal(x.rec.EditsNotMade, []string{row.want}) {
				t.Fatalf("not made %v, want only %s", x.rec.EditsNotMade, row.want)
			}
		})
	}
}

// An address this gateway cannot write one of its own for (its ingress base
// unusable) is carried and recorded as such, a URL and a true alike; an empty
// answer carries nothing to re-key, and an answer whose only addresses are
// false has nothing the re-key could make, so neither records anything.
func TestDTRRekey_WhatItCannotWriteOrHasNothingToRekey(t *testing.T) {
	for name, row := range map[string]struct {
		ingress string
		answer  []byte
		stated  statedAnswer
		want    []string
	}{
		"a URL, no usable ingress base": {"", packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`/forms/a"`)), statedTags(rekeyBase, "/forms/a="+rekeyTagA),
			[]string{EditNotMadeRekeyAddressNotWritable}},
		"a true, no usable ingress base": {"provider-gw.example/", packageOf(adaptiveQ("a", `"valueBoolean":true`)), statedTags(rekeyBase, dtraddress.BoolRemainder+"="+rekeyTagTrue),
			[]string{EditNotMadeRekeyAddressNotWritable}},
		"an empty answer":       {rekeyIngress + "/", nil, statedTags(rekeyBase, "/forms/a="+rekeyTagA), nil},
		"only a false address":  {rekeyIngress + "/", packageOf(adaptiveQ("a", `"valueBoolean":false`)), statedTags("", ""), nil},
		"no adaptive extension": {rekeyIngress + "/", packageOf(`{"resourceType":"Questionnaire","id":"a","status":"active"}`), statedTags("", ""), nil},
	} {
		t.Run(name, func(t *testing.T) {
			// The payer's gateway does not declare v1opsub for the last
			// three rows: nothing is recorded even so.
			g, payer := adaptiveRekeyGateway(t, row.want != nil)
			g.cfg.IngressBaseURL = row.ingress
			x := &exchangeRecorder{}
			ctx := context.WithValue(context.Background(), exchangeRecorderKey{}, x)
			p := g.rekeyAnswer(ctx, relay.Exact(relay.NewBody(row.answer, relay.OriginPeerFrame), "application/fhir+json"),
				rekeyContext{payer: payer, line: "2.2", stated: row.stated})
			if got := relay.BytesForTest(p); !bytes.Equal(got, row.answer) || len(p.Edits()) != 0 {
				t.Fatalf("changed: %s, edits %v", got, p.Edits())
			}
			if !slices.Equal(x.rec.EditsNotMade, row.want) {
				t.Fatalf("not made %v, want %v", x.rec.EditsNotMade, row.want)
			}
		})
	}
}

// A round's answer re-keys the payer's base and the round's own address,
// and an address the payer signed is re-keyed all the same, the signature
// named.
func TestDTRRekey_ARoundsAnswerAndASignedAnswer(t *testing.T) {
	g, payer := adaptiveRekeyGateway(t, true)
	round := dtraddress.Subpath{Family: dtraddress.FamilyPayer, Line: "2.2", Tag: rekeyTagA, Remainder: "/forms/a"}
	answer := packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`"`), adaptiveQ("b", `"valueUrl":"`+rekeyBase+`/forms/a"`))
	p := g.rekeyAnswer(context.Background(), relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), "application/fhir+json"),
		rekeyContext{payer: payer, line: "2.2", nextQuestion: true, round: &round, stated: statedTags(rekeyBase, "="+rekeyTagTrue+" /forms/a="+rekeyTagA)})
	got := string(relay.BytesForTest(p))
	for _, want := range []string{rekeyIngress + "/dtr/payer/" + payer + "/2.2/" + rekeyTagTrue + `"`, rekeyIngress + "/dtr/payer/" + payer + "/2.2/" + rekeyTagA + "/forms/a"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s not in\n%s", want, got)
		}
	}
	signed := []byte(strings.TrimSuffix(string(packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`/forms/a"`))), "}") +
		`,"signature":{"type":[{"code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-05T00:00:00Z","who":{"reference":"Organization/payer"}}}`)
	p = g.rekeyAnswer(context.Background(), relay.Exact(relay.NewBody(signed, relay.OriginPeerFrame), "application/fhir+json"),
		rekeyContext{payer: payer, line: "2.2", stated: statedTags(rekeyBase, "/forms/a="+rekeyTagA)})
	if !slices.Equal(p.Edits(), []relay.EditID{relay.EditDTRAdaptiveAddressRekey}) || len(p.SignedEdited()) == 0 {
		t.Fatalf("a signed answer: edits %v, signed %v", p.Edits(), p.SignedEdited())
	}
}

// The package route's re-key (E-12) changes no line: through the route, with
// E-08 off and on, the answer the EHR receives carries the re-keyed address,
// the record names E-12, and the line it records is the one the same answer
// records unre-keyed (adaptAnswer's).
func TestDTRRekey_ThePackageRouteKeepsAdaptAnswersLine(t *testing.T) {
	answer := packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`/forms/a"`))
	run := func(t *testing.T, module2, tagged bool) (*httptest.ResponseRecorder, ExchangeRecord, string) {
		env := newInProcessExchange(t)
		g := env.originator
		g.cfg.TranslateIGLines = module2
		g.cfg.SoR = newPrefetchSoR().sor()
		g.cfg.ConformanceEnforcement = EnforcementNone
		g.cfg.IngressBaseURL = rekeyIngress + "/"
		var records []ExchangeRecord
		var mu sync.Mutex
		g.cfg.ExchangeObserved = func(x ExchangeRecord) { mu.Lock(); records = append(records, x); mu.Unlock() }
		declareFramedDTR(t, env, true)
		e, _ := g.cfg.Reg.Lookup(env.payerID)
		e.RequestFrames = append(slices.Clone(e.RequestFrames), shnsdk.RequestFrameV1OpSub)
		g.cfg.Reg.Set(env.payerID, e)
		lr := LegResult{Response: testResponse(answer), AnswerStatus: http.StatusOK}
		if tagged {
			lr.AddressHeaders = map[string]string{shnsdk.FrameHeaderOperationBase: rekeyBase, shnsdk.FrameHeaderOperationAddressTags: "/forms/a=" + rekeyTagA}
		}
		env.payerReturns(lr)
		req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
		req.Header.Set("Content-Type", "application/fhir+json")
		rec := httptest.NewRecorder()
		g.ingressRoute(RouteDTR)(rec, req)
		g.drainObserveChecks()
		mu.Lock()
		defer mu.Unlock()
		if len(records) != 1 {
			t.Fatalf("%d records", len(records))
		}
		return rec, records[0], env.payerID
	}
	for _, module2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "module 2 off", true: "module 2 on"}[module2], func(t *testing.T) {
			plainRec, plain, _ := run(t, module2, false)
			rec, x, payer := run(t, module2, true)
			if plainRec.Code != http.StatusOK || rec.Code != http.StatusOK {
				t.Fatalf("answers %d %d", plainRec.Code, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), rekeyIngress+"/dtr/payer/"+payer+"/") || !slices.Contains(x.Edits, string(relay.EditDTRAdaptiveAddressRekey)) {
				t.Fatalf("not re-keyed: edits %v, body %s", x.Edits, rec.Body)
			}
			// The re-key is disclosed to the DTR client; an answer it is not
			// made on discloses nothing.
			if got := rec.Header().Values(EditedHeader); !slices.Equal(got, []string{"dtr-adaptive-address-rekey"}) {
				t.Fatalf("%s = %q on the re-keyed answer", EditedHeader, got)
			}
			if got := plainRec.Header().Values(EditedHeader); len(got) != 0 {
				t.Fatalf("%s = %q on an answer not re-keyed", EditedHeader, got)
			}
			if x.AnswerLine == "" || x.AnswerLine != plain.AnswerLine {
				t.Fatalf("recorded line %q, the unre-keyed answer's %q", x.AnswerLine, plain.AnswerLine)
			}
		})
	}
}
