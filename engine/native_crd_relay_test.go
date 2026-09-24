package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer gateway relays its payer's CDS Hooks answer exactly: the bytes and
// the media type the payer answered with, once the answer meets the CDS Hooks
// response rules at the served CRD line.

// crdTopic is a CRD card topic.
const crdTopic = `{"system":"http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp","code":"coverage-info"}`

// crdCard is a valid CRD card with extra members.
const crdCard = `{"summary":"Prior authorization required","indicator":"warning","source":{"label":"Example Health Plan","topic":` + crdTopic + `},"x-payer-note":{"n":1}}`

// The controlled CDS backend fixture explicitly asserts its output line at
// each service URL. Receive capability alone cannot do that for a real payer.
func declaredCDSFixtureOutput(t *testing.T, base string) NativeResponseDeclarations {
	t.Helper()
	rows := []string{
		responseBinding("crd-order-select", base+"/cds-services/order-select-crd", "pa.crd@2.0"),
		responseBinding("crd-order-select", base+"/cds-services/order-sign-crd", "pa.crd@2.0"),
		responseBinding("crd-order-dispatch", base+"/cds-services/order-dispatch-crd", "pa.crd@2.0"),
	}
	declarations, err := ParseNativeResponseDeclarations("[" + strings.Join(rows, ",") + "]")
	if err != nil {
		t.Fatal(err)
	}
	return declarations
}

// unusualAnswer is a payer answer in the payer's own layout: whitespace,
// escapes, number forms, member order and members this gateway does not know.
// The escapes are built at run time so no editor can fold them.
func unusualAnswer() []byte {
	bs := string([]byte{'\\'})
	return []byte("{\n  \"systemActions\" : [ {\"type\":\"update\",\"description\":\"Add coverage information " + bs + "u00e9 " + bs + "/ " + bs + "\"\",\n" +
		"    \"resource\":{\"resourceType\":\"ServiceRequest\",\"id\":\"sr1\",\"quantityQuantity\":{\"value\":1.50E0},\"extension\":[{\"url\":\"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information\",\"extension\":[{\"url\":\"covered\",\"valueCode\":\"covered\"},{\"url\":\"coverage-assertion-id\",\"valueString\":\"a-1\"},{\"url\":\"x-unknown\",\"valueInteger\":7}]}]}} ],\n" +
		"  \"cards\" : [ " + crdCard + " ],\n  \"x-extension\" : null\n}\n")
}

// relayCase runs one CRD leg against a payer answering answer with
// contentType and returns what the responder produced and what the payer
// received.
func relayCase(t *testing.T, leg, hook string, contentType string, answer []byte) (LegResult, *cdsPayer, []byte) {
	t.Helper()
	p := newCDSPayer(t, referencePayerServices...)
	p.respond(http.StatusOK, contentType, answer)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil,
		WithConformancePolicy(NewConformancePolicy(EnforcementStrict)))
	req := cdsRequest(hook)
	res, err := n.Handle(context.Background(), leg, "corr", "pci", req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return res, p, req
}

func assertRelayed(t *testing.T, res LegResult, answer []byte, contentType string) {
	t.Helper()
	if res.Status != 0 {
		t.Fatalf("refused: %d %s", res.Status, res.Message)
	}
	if res.Response.Ownership() != relay.OwnershipRelayed {
		t.Fatalf("answer ownership %v, want relayed", res.Response.Ownership())
	}
	if got := relay.BytesForTest(res.Response); !bytes.Equal(got, answer) {
		t.Fatalf("answer changed:\n got %q\nwant %q", got, answer)
	}
	if res.Response.ContentType() != contentType {
		t.Fatalf("content type %q, want %q", res.Response.ContentType(), contentType)
	}
	if err := relay.Check(answerKey("crd-order-select", relay.OutcomeAnswered))(res.Response); err != nil {
		t.Fatalf("the answer is not permitted on the response transmit: %v", err)
	}
}

func TestNativeCRD_OrderSelectRelaysPayerBytes(t *testing.T) {
	for name, answer := range map[string][]byte{
		"the reference payer's recorded answer": realCRDAnswer(t),
		"an answer in the payer's own layout":   unusualAnswer(),
	} {
		for _, hook := range []string{"order-sign", "order-select"} {
			t.Run(name+"/"+hook, func(t *testing.T) {
				res, p, req := relayCase(t, "crd-order-select", hook, "application/json; charset=utf-8", answer)
				assertRelayed(t, res, answer, "application/json; charset=utf-8")
				if sent := p.sent(hook + "-crd"); len(sent) != 1 || !bytes.Equal(sent[0], req) {
					t.Fatalf("the payer received %q", sent)
				}
			})
		}
	}
	t.Run("an answer without a media type is relayed as the JSON the payer sent", func(t *testing.T) {
		p := newCDSPayer(t, referencePayerServices...)
		p.respond(http.StatusOK, "", realCRDAnswer(t))
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
		res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		if err != nil || res.Status != 0 || !bytes.Equal(relay.BytesForTest(res.Response), realCRDAnswer(t)) {
			t.Fatalf("got %v %+v", err, res)
		}
		if ct := res.Response.ContentType(); ct != "application/json" {
			t.Fatalf("media type %q, want application/json for a CDS Hooks answer sent without one", ct)
		}
	})
	t.Run("a non-2xx answer is relayed as the payer's error", func(t *testing.T) {
		body := []byte(`{"error":"Mismatched hook"}`)
		p := newCDSPayer(t, referencePayerServices...)
		p.respond(http.StatusBadRequest, "application/json", body)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
		res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		if err != nil || res.Status != http.StatusBadRequest || !bytes.Equal(relay.BytesForTest(res.Response), body) || res.Response.ContentType() != "application/json" {
			t.Fatalf("got %v %d %q %q", err, res.Status, relay.BytesForTest(res.Response), res.Response.ContentType())
		}
	})
}

func TestNativeCRD_OrderDispatchRelaysPayerBytes(t *testing.T) {
	for name, answer := range map[string][]byte{
		"the reference payer's recorded answer": realCRDAnswer(t),
		"an answer in the payer's own layout":   unusualAnswer(),
	} {
		t.Run(name, func(t *testing.T) {
			res, p, req := relayCase(t, "crd-order-dispatch", "order-dispatch", "application/json", answer)
			assertRelayed(t, res, answer, "application/json")
			if err := relay.Check(answerKey("crd-order-dispatch", relay.OutcomeAnswered))(res.Response); err != nil {
				t.Fatalf("the answer is not permitted on the dispatch response transmit: %v", err)
			}
			if sent := p.sent("order-dispatch-crd"); len(sent) != 1 || !bytes.Equal(sent[0], req) {
				t.Fatalf("the payer received %q", sent)
			}
		})
	}
}

func TestNativeCRD_EmptyCardsWithSystemActionsSucceeds(t *testing.T) {
	for name, answer := range map[string]string{
		"coverage information in a system action": string(realCRDAnswer(t)),
		"no card and no system action":            `{"cards":[]}`,
		"a delete action without a resource id":   `{"cards":[],"systemActions":[{"type":"delete","description":"remove the draft"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			res, _, _ := relayCase(t, "crd-order-select", "order-sign", "application/json", []byte(answer))
			assertRelayed(t, res, []byte(answer), "application/json")
		})
	}
}

// certifierRow is an answer that breaks one CDS Hooks response rule.
type certifierRow struct {
	rule, answer string
}

// withCard is an answer carrying card, and with systemActions actions.
func withCard(card string) string { return `{"cards":[` + card + `]}` }

func withAction(action string) string {
	return `{"cards":[],"systemActions":[` + action + `]}`
}

// cardWith is the valid card with extra members.
func cardWith(members string) string {
	return `{"summary":"Prior authorization required","indicator":"warning","source":{"label":"Example Health Plan","topic":` + crdTopic + `},` + members + `}`
}

func certifierRows() []certifierRow {
	return []certifierRow{
		{"response.json", `{"cards":[],"cards":[]}`},
		{"response.object", `[]`},
		{"response.cards", `{"systemActions":[]}`},
		{"response.systemActions", `{"cards":[],"systemActions":{}}`},
		{"card.object", withCard(`"a card"`)},
		{"card.uuid", withCard(cardWith(`"uuid":1`))},
		{"card.summary", withCard(`{"summary":"","indicator":"info","source":{"label":"P","topic":` + crdTopic + `}}`)},
		{"card.summary.length", withCard(`{"summary":"` + strings.Repeat("é", 140) + `","indicator":"info","source":{"label":"P","topic":` + crdTopic + `}}`)},
		{"card.detail", withCard(cardWith(`"detail":{"text":"x"}`))},
		{"card.indicator", withCard(`{"summary":"s","indicator":"hard-stop","source":{"label":"P","topic":` + crdTopic + `}}`)},
		{"card.source", withCard(`{"summary":"s","indicator":"info"}`)},
		{"card.source.label", withCard(`{"summary":"s","indicator":"info","source":{"topic":` + crdTopic + `}}`)},
		{"card.source.topic", withCard(`{"summary":"s","indicator":"info","source":{"label":"P"}}`)},
		{"card.suggestions", withCard(cardWith(`"suggestions":{}`))},
		{"suggestion.label", withCard(cardWith(`"selectionBehavior":"any","suggestions":[{}]`))},
		{"suggestion.uuid", withCard(cardWith(`"selectionBehavior":"any","suggestions":[{"label":"a","uuid":1}]`))},
		{"suggestion.isRecommended", withCard(cardWith(`"selectionBehavior":"any","suggestions":[{"label":"a","isRecommended":"yes"}]`))},
		{"suggestion.actions", withCard(cardWith(`"selectionBehavior":"any","suggestions":[{"label":"a","actions":{}}]`))},
		{"card.selectionBehavior", withCard(cardWith(`"suggestions":[{"label":"a"}]`))},
		{"card.selectionBehavior.at-most-one", withCard(cardWith(`"selectionBehavior":"at-most-one","suggestions":[{"label":"a","isRecommended":true},{"label":"b","isRecommended":true}]`))},
		{"action.object", withAction(`"update"`)},
		{"action.type", withAction(`{"type":"replace","description":"d","resource":{"resourceType":"ServiceRequest"}}`)},
		{"action.description", withAction(`{"type":"update","resource":{"resourceType":"ServiceRequest"}}`)},
		{"action.resource", withAction(`{"type":"update","description":"d"}`)},
		{"card.links", withCard(cardWith(`"links":{}`))},
		{"link.label", withCard(cardWith(`"links":[{"url":"https://payer.example","type":"absolute"}]`))},
		{"link.url", withCard(cardWith(`"links":[{"label":"l","type":"absolute"}]`))},
		{"link.type", withCard(cardWith(`"links":[{"label":"l","url":"https://payer.example","type":"relative"}]`))},
		{"link.appContext", withCard(cardWith(`"links":[{"label":"l","url":"https://payer.example","type":"absolute","appContext":"c"}]`))},
		{"card.overrideReasons", withCard(cardWith(`"overrideReasons":[{"code":"c"}]`))},
		{"overrideReason.display", withCard(cardWith(`"overrideReasons":[{"code":"c","system":"https://payer.example/reasons"}]`))},
	}
}

// certifierRuleNotReachable are the refusing rules no payer answer can break:
// the served line is always a CRD line this gateway knows.
var certifierRuleNotReachable = []string{"line"}

func TestNativeCRD_MalformedEnvelopeRefused(t *testing.T) {
	rows := certifierRows()
	covered := map[string]bool{}
	for _, r := range rows {
		covered[r.rule] = true
	}
	for _, rule := range shnsdk.CDSHooksRules() {
		if rule.Severity != shnsdk.SeverityError || slices.Contains(certifierRuleNotReachable, rule.ID) {
			continue
		}
		if !covered[rule.ID] {
			t.Errorf("no refusal row for rule %s", rule.ID)
		}
	}
	for _, leg := range []string{"crd-order-select", "crd-order-dispatch"} {
		t.Run(leg+"/valid control", func(t *testing.T) {
			answer := []byte(withCard(crdCard))
			status, body, _ := nativeCRDPolicyCase(t, leg, answer)
			if status != 202 || !bytes.Equal(body, answer) {
				t.Fatalf("valid control status=%d body=%s", status, body)
			}
		})
	}
	for _, r := range rows {
		for _, leg := range []string{"crd-order-select", "crd-order-dispatch"} {
			t.Run(leg+"/"+r.rule, func(t *testing.T) {
				if v := shnsdk.CheckCDSHooksResponse([]byte(r.answer), "2.2"); !slices.ContainsFunc(v, func(v shnsdk.Violation) bool { return v.Rule == r.rule }) {
					t.Fatalf("the row does not break %s: %v", r.rule, v)
				}
				status, body, p := nativeCRDPolicyCase(t, leg, []byte(r.answer))
				wantRule := map[string]string{"response.json": "json.duplicate_key", "response.object": "json.object", "response.cards": "response.cards", "card.object": "card.object"}[r.rule]
				if wantRule == "" {
					wantRule = "cds.response"
				}
				var carrier map[string]any
				if status != http.StatusBadGateway || json.Unmarshal(body, &carrier) != nil || len(carrier) != 5 || carrier["category"] != "conformance_invalid" || carrier["rule"] != wantRule || carrier["gateway"] != "payer" || carrier["level"] != "strict" || carrier["direction"] != "response" {
					t.Fatalf("mutation=%s actual status=%d carrier=%s; want502 invalid %s (original invalidity obligation retained)", r.rule, status, body, wantRule)
				}
				if p.sentAny() != 1 {
					t.Fatalf("%d requests reached the payer, want 1", p.sentAny())
				}
			})
		}
	}
	t.Run("active checker retains subrule paths", func(t *testing.T) {
		answer := withCard(`{"summary":"s","indicator":"info","source":{}}`)
		violations := shnsdk.CheckCDSHooksResponse([]byte(answer), "2.0")
		for _, want := range []shnsdk.Violation{{Rule: "card.source.label", Path: "cards[0].source.label", Severity: shnsdk.SeverityError}, {Rule: "card.source.topic", Path: "cards[0].source.topic", Severity: shnsdk.SeverityError}} {
			if !slices.Contains(violations, want) {
				t.Fatalf("missing %+v in %+v", want, violations)
			}
		}
	})
	t.Run("registered policy bounds a long list", func(t *testing.T) {
		cards := strings.TrimSuffix(strings.Repeat(`"x",`, 7), ",")
		answer := []byte(`{"cards":[` + cards + `]}`)
		if got := shnsdk.CheckCDSHooksResponse(answer, "2.0"); len(got) != 7 {
			t.Fatalf("expected seven independent card violations: %+v", got)
		}
		status, body, _ := nativeCRDPolicyCase(t, "crd-order-select", answer)
		want := `{"category":"conformance_invalid","rule":"card.object","gateway":"payer","level":"strict","direction":"response"}`
		if status != 502 || string(body) != want {
			t.Fatalf("status=%d carrier=%s; want bounded safe carrier %s", status, body, want)
		}
	})
}

func TestNativeCRD_HookNeverRewritten(t *testing.T) {
	for _, hook := range []string{"order-select", "order-sign", "order-dispatch"} {
		t.Run(hook, func(t *testing.T) {
			p := newCDSPayer(t, referencePayerServices...)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
			// The EHR's own layout, with the hook last.
			req := []byte("{ \"hookInstance\" : \"h\", \"context\" : { \"patientId\" : \"p1\" },\n  \"hook\" : \"" + hook + "\" }")
			if res, err := n.Handle(context.Background(), legFor(hook), "c", "pci", req); err != nil || res.Status != 0 {
				t.Fatalf("got %v %+v", err, res)
			}
			sent := p.sent(hook + "-crd")
			if len(sent) != 1 || !bytes.Equal(sent[0], req) {
				t.Fatalf("the payer received %q, want the request exactly", sent)
			}
		})
	}
	t.Run("with the payer identity mapped, only the identity changes", func(t *testing.T) {
		own := shnsdk.PayerIdentifier{System: "urn:shn:payer", Value: "OWN"}
		backend := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00001"}
		p := newCDSPayer(t, referencePayerServices...)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil, WithPayorEdgeIdentity(own, backend))
		for _, hook := range []string{"order-sign", "order-dispatch"} {
			req := []byte(`{"hook":"` + hook + `","context":{"patientId":"p1"},"prefetch":{"coverage":{"resourceType":"Coverage","id":"c1","payor":[{"identifier":{"system":"urn:shn:payer","value":"OWN"}}]}}}`)
			if res, err := n.Handle(context.Background(), legFor(hook), "c", "pci", req); err != nil || res.Status != 0 {
				t.Fatalf("%s: %v %+v", hook, err, res)
			}
			want := bytes.Replace(bytes.Replace(req, []byte("urn:shn:payer"), []byte(backend.System), 1), []byte(`"OWN"`), []byte(`"00001"`), 1)
			if sent := p.sent(hook + "-crd"); len(sent) != 1 || !bytes.Equal(sent[0], want) {
				t.Fatalf("%s: the payer received %q\nwant %q", hook, sent, want)
			}
		}
	})
}

// validatorFunc is a validator whose verdict tests choose.
type validatorFunc func(resourceJSON []byte) (shnsdk.Result, error)

func (f validatorFunc) Validate(_ context.Context, resourceJSON []byte, _ string) (shnsdk.Result, error) {
	return f(resourceJSON)
}

// embeddedValidationTails are the payer gateway's two CRD tails: each
// validates what the payer's answer embeds, and only observes the outcome.
var embeddedValidationTails = []struct {
	leg    string
	handle func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token)
}{
	{"crd-order-select", func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
		req := bytes.Replace(conformantCRD("MBR-COVERED", "72148"), []byte(`"order-select"`), []byte(`"order-sign"`), 1)
		g.handleCRDNativeInbound(w, r, env, tok, req, "")
	}},
	{"crd-order-dispatch", func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token) {
		req := []byte(`{"hook":"order-dispatch","hookInstance":"h-1","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/o1"},` +
			`"prefetch":{"deviceHistory":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","status":"active","intent":"order",` +
			`"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431"}]},"subject":{"reference":"Patient/MBR-COVERED"}}}]},` +
			`"coverage":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"reference":"Organization/o2"}]}}]}}}`)
		g.handleCRDDispatchInbound(w, r, env, tok, req, "")
	}},
}

func TestNativeCRD_EmbeddedValidationObservesOnly(t *testing.T) {
	const covInfo = "ext-coverage-information"
	answer := realCRDAnswer(t)
	for _, verdict := range []struct {
		name    string
		v       validatorFunc
		outcome string
	}{
		{"invalid", func(b []byte) (shnsdk.Result, error) {
			return shnsdk.Result{Valid: !bytes.Contains(b, []byte(covInfo)), Issues: []string{"x"}}, nil
		}, "invalid"},
		{"unavailable", func(b []byte) (shnsdk.Result, error) {
			if bytes.Contains(b, []byte(covInfo)) {
				return shnsdk.Result{}, context.DeadlineExceeded
			}
			return shnsdk.Result{Valid: true}, nil
		}, "unavailable"},
		{"valid", func([]byte) (shnsdk.Result, error) { return shnsdk.Result{Valid: true}, nil }, "valid"},
	} {
		for _, tail := range embeddedValidationTails {
			t.Run(tail.leg+"/"+verdict.name, func(t *testing.T) {
				g, requester := newInboundTestGateway(t, true)
				p := newCDSPayer(t, referencePayerServices...)
				g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil,
					WithDeclaredContractVersions([]string{"pa.crd@2.0"}),
					WithNativeResponseDeclarations(declaredCDSFixtureOutput(t, p.srv.URL)),
					WithConformancePolicy(NewConformancePolicy(EnforcementObserve)))
				g.cfg.ConformanceEnforcement = EnforcementObserve
				g.startCertification()
				var mu sync.Mutex
				var validated [][]byte
				g.cfg.Validator = syntheticEvidenceValidatorFunc(func(b []byte) (shnsdk.Result, error) {
					mu.Lock()
					validated = append(validated, bytes.Clone(b))
					mu.Unlock()
					return verdict.v(b)
				})
				var events []ObserverEvent
				g.cfg.Observer = func(e ObserverEvent) {
					mu.Lock()
					events = append(events, e)
					mu.Unlock()
				}
				pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
				env := shnsdk.Envelope{}
				env.Metadata.CorrelationID, env.Metadata.Sender = "corr-1", requester.ID
				rec := httptest.NewRecorder()
				r := newSignedInboundRequest(t, g, requester.ID)
				r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ExchangeContext{
					holder: requester.ID, recipient: g.cfg.HolderID, legType: tail.leg,
					subjectPCI: pci, correlationID: env.Metadata.CorrelationID,
					contractVersion: "pa.crd@2.0", policy: g.policy(),
				}))
				tail.handle(g, rec, r, env, shnsdk.Token{Subject: pci})
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d %s", rec.Code, rec.Body.String())
				}
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(body, answer) {
					t.Fatalf("the requester got %v %d %s", err, hdr.Status, body)
				}
				observationFlush(t, g)
				var embedded []byte
				for _, b := range validated {
					if bytes.Contains(b, []byte(covInfo)) {
						embedded = b
					}
				}
				if len(embedded) == 0 || embedded[0] != '{' {
					t.Fatalf("the embedded order was not validated as sent: %s", embedded)
				}
				var gotResource, answerDoc map[string]any
				if err := json.Unmarshal(embedded, &gotResource); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(answer, &answerDoc); err != nil {
					t.Fatal(err)
				}
				actions, _ := answerDoc["systemActions"].([]any)
				if len(actions) != 1 {
					t.Fatalf("expected one system action, got %d", len(actions))
				}
				// Compare decoded JSON values without reflection beside relay's
				// opaque payload type; the exact full answer is asserted above.
				gotJSON, err := json.Marshal(gotResource)
				if err != nil {
					t.Fatal(err)
				}
				wantJSON, err := json.Marshal(actions[0].(map[string]any)["resource"])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("checker saw a different embedded resource: %s", embedded)
				}
				wantState := CheckState(verdict.outcome)
				found := false
				for _, e := range events {
					if e.Kind != ConformanceObservedEvent {
						continue
					}
					var f ConformanceFinding
					if err := json.Unmarshal([]byte(e.Detail), &f); err != nil {
						t.Fatal(err)
					}
					if f.Direction != "response" || f.Rule != "fhir.profile" {
						continue
					}
					found = true
					if f.State != wantState || f.LegType != tail.leg || f.CorrelationID != "corr-1" ||
						f.Gateway != "payer" || f.Level != "observe" || f.Action != "not_enforced" ||
						f.PayloadSHA256 != sha256hex(answer) {
						t.Errorf("wrong response finding: %+v", f)
					}
				}
				if !found {
					t.Fatal("missing response profile observation")
				}
			})
		}
	}
	t.Run("a refused answer is not validated", func(t *testing.T) {
		g, requester := newInboundTestGateway(t, true)
		p := newCDSPayer(t, referencePayerServices...)
		p.respond(http.StatusOK, "application/json", []byte(withCard(`{"summary":"s","indicator":"info"}`)))
		g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil,
			WithDeclaredContractVersions([]string{"pa.crd@2.0"}),
			WithNativeResponseDeclarations(declaredCDSFixtureOutput(t, p.srv.URL)),
			WithConformancePolicy(NewConformancePolicy(EnforcementStrict)))
		calls := 0
		g.cfg.Validator = syntheticEvidenceValidatorFunc(func(b []byte) (shnsdk.Result, error) {
			calls++
			return shnsdk.Result{Valid: true}, nil
		})
		pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
		env := shnsdk.Envelope{}
		env.Metadata.CorrelationID, env.Metadata.Sender = "corr-1", requester.ID
		req := bytes.Replace(conformantCRD("MBR-COVERED", "72148"), []byte(`"order-select"`), []byte(`"order-sign"`), 1)
		rec := httptest.NewRecorder()
		r := newSignedInboundRequest(t, g, requester.ID)
		r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ExchangeContext{
			holder: requester.ID, recipient: g.cfg.HolderID, legType: "crd-order-select",
			subjectPCI: pci, correlationID: env.Metadata.CorrelationID,
			contractVersion: "pa.crd@2.0", policy: g.policy(),
		}))
		g.handleCRDNativeInbound(rec, r, env, shnsdk.Token{Subject: pci}, req, "")
		hdr, _, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil || hdr.Status != http.StatusBadGateway {
			t.Fatalf("got %v %d", err, hdr.Status)
		}
		if calls != 2 { // the order and the coverage the request carried; nothing from the answer
			t.Fatalf("validator ran %d times, want 2", calls)
		}
	})
}

// A real external payer's answer: a system action with no description, which
// CDS Hooks 2.0 marks REQUIRED. At strict it is refused, naming the rule and
// where it broke. At none the payer's bytes relay exactly and the finding is
// the record.
const externalPayerDescriptionlessAnswer = `{"cards":[],"systemActions":[{"type":"update","resource":{"resourceType":"ServiceRequest","id":"sr-1","status":"draft","intent":"order","subject":{"reference":"Patient/abby"}}}]}`

func TestCDSCertifierDescriptionMissing(t *testing.T) {
	for _, tc := range []struct {
		level      ConformanceEnforcement
		wantStatus int
		wantMsg    string
	}{
		{EnforcementStrict, http.StatusBadGateway, "action.description at systemActions[0].description"},
		{EnforcementNone, 0, ""},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			var findings []ConformanceFinding
			got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(tc.level),
				func(f ConformanceFinding) { findings = append(findings, f) },
				[]byte(externalPayerDescriptionlessAnswer), "2.0", "payer")

			if got.Status != tc.wantStatus {
				t.Fatalf("at %s want status %d, got %d (%s)", tc.level, tc.wantStatus, got.Status, got.Message)
			}
			if tc.wantMsg != "" && !strings.Contains(got.Message, tc.wantMsg) {
				t.Fatalf("the refusal must name the rule and path: %q", got.Message)
			}
			if len(findings) == 0 {
				t.Fatal("every violation is recorded at both levels")
			}
			f := findings[0]
			if f.Kind != string(KindCDSEnvelope) || f.Rule != "action.description" || f.Path != "systemActions[0].description" {
				t.Fatalf("the finding must name the rule and path: %+v", f)
			}
			if f.Level != tc.level.String() {
				t.Fatalf("finding level = %q, want %q", f.Level, tc.level)
			}
			if len(f.Issues) != 0 {
				t.Fatalf("a finding carries no diagnostic text — issue text reaches only the refusal body, bounded: %+v", f)
			}
			wantDecision := "refused"
			if tc.level == EnforcementNone {
				wantDecision = "relayed"
			}
			if f.Decision != wantDecision {
				t.Fatalf("finding decision = %q, want %q", f.Decision, wantDecision)
			}
		})
	}
}

// The three structural rules refuse at every level: the reader that follows
// the certifier needs the shape.
func TestCDSCertifierStructuralRulesRefuseAtNone(t *testing.T) {
	got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(EnforcementNone), nil,
		[]byte(`not json at all`), "2.0", "payer")
	if got.Status != http.StatusBadGateway {
		t.Fatalf("an unreadable answer must refuse at none too, got %d", got.Status)
	}
}

// A responder the engine never wired can still select strict explicitly and
// never panics when it emits.
func TestCDSCertifierUnwiredEmitterIsSafe(t *testing.T) {
	got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(EnforcementStrict), nil,
		[]byte(externalPayerDescriptionlessAnswer), "2.0", "peer")
	if got.Status != http.StatusBadGateway {
		t.Fatalf("an unwired responder with an explicit strict policy must refuse, got %d", got.Status)
	}
}

// action.resourceId is the one SHOULD-level (warning) CDS Hooks rule: a
// delete action naming no resource never refuses, at either level — but it is
// still recorded. Deleting the advisory emit(...) call would leave this
// green-at-zero-findings instead of red.
func TestCDSCertifierAdvisoryRecordsAtBothLevels(t *testing.T) {
	answer := []byte(`{"cards":[],"systemActions":[{"type":"delete","description":"remove the draft"}]}`)
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementNone} {
		t.Run(level.String(), func(t *testing.T) {
			var findings []ConformanceFinding
			got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(level),
				func(f ConformanceFinding) { findings = append(findings, f) },
				answer, "2.0", "peer")
			if got.Status != 0 {
				t.Fatalf("an advisory (SHOULD) violation must never refuse, at any level: got %d %s", got.Status, got.Message)
			}
			if len(findings) != 1 {
				t.Fatalf("want exactly one recorded finding, got %d: %+v", len(findings), findings)
			}
			f := findings[0]
			if f.Rule != "action.resourceId" || f.Decision != Record.String() {
				t.Fatalf("want rule action.resourceId decision %q, got %+v", Record.String(), f)
			}
			if f.Level != level.String() {
				t.Fatalf("finding level = %q, want %q", f.Level, level)
			}
		})
	}
}

// A card whose source has neither label nor topic breaks two distinct rules:
// the certifier must emit one finding PER violation, not one finding for the
// whole answer (§5). This also pins that a finding carries no diagnostic
// text of its own — issue text reaches only the refusal body, bounded.
func TestCDSCertifierOneFindingPerViolation(t *testing.T) {
	answer := []byte(withCard(`{"summary":"s","indicator":"info","source":{}}`))
	var findings []ConformanceFinding
	got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(EnforcementStrict),
		func(f ConformanceFinding) { findings = append(findings, f) },
		answer, "2.0", "peer")
	if got.Status != http.StatusBadGateway {
		t.Fatalf("want a refusal, got %d %s", got.Status, got.Message)
	}
	if len(findings) != 2 {
		t.Fatalf("want one finding per violation (2 broken rules), got %d: %+v", len(findings), findings)
	}
	rules := map[string]bool{}
	for _, f := range findings {
		rules[f.Rule] = true
		if len(f.Issues) != 0 {
			t.Fatalf("a finding carries no diagnostic text: %+v", f)
		}
	}
	if !rules["card.source.label"] || !rules["card.source.topic"] {
		t.Fatalf("want findings for both card.source.label and card.source.topic, got %+v", findings)
	}
}

// TestNewBindsFindingEmitterToNativeResponder is the wiring-level proof for
// engine.New's emitter-binding block: without it, a native responder built
// with a real payer occupant would never record a single CDS finding, and
// the forwardCRD half of this task would ship unproven. A raw (unwrapped)
// *nativeResponder passed as Config.Responder is exactly the shape
// findingEmitterBinder's type assertion must reach.
func TestNewBindsFindingEmitterToNativeResponder(t *testing.T) {
	authzPub, _ := genED25519(t)
	_, paySignPriv := genED25519(t)
	payEncPub, payEncPriv := genKeyPair(t)
	sor := newCensusSoR()
	n := NewNativeResponder(&http.Client{}, "http://payer-backend.test", "", nil, nil)
	mustNew(t, Config{
		Role:            "payer",
		HolderID:        "payer",
		Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
		AuthzURL:        "http://stub.test",
		AuthzPub:        authzPub,
		HubTransportPub: authzPub,
		Reg:             shnsdk.NewRegistry(),
		Validator:       syntheticFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Responder:       n,
		Clock:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if n.emitFinding == nil {
		t.Fatal("engine.New must bind its finding emitter to a raw *nativeResponder passed as Config.Responder")
	}
}

// A real receiving gateway applies its registry around the HTTP responder.
// Delivery is proved independently of optional findings about its own backend.
func TestForwardCRD_RelaysAtNoneWithOwnFinding(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing-description=%v", level, missing), func(t *testing.T) {
				p := newCDSPayer(t, referencePayerServices...)
				answer := []byte(externalPayerDescriptionlessAnswer)
				wantState := CheckInvalid
				if !missing {
					answer = bytes.Replace(answer, []byte(`"type":"update",`), []byte(`"type":"update","description":"apply coverage",`), 1)
					wantState = CheckValid
				}
				violations := shnsdk.CheckCDSHooksResponse(answer, "2.0")
				errorsFound := 0
				for _, v := range violations {
					if v.Severity == shnsdk.SeverityError {
						errorsFound++
						if v.Rule != "action.description" {
							t.Fatalf("unrelated mutation: %+v", v)
						}
					}
				}
				if (errorsFound == 1) != missing {
					t.Fatalf("control-minus-description: missing=%v violations=%+v", missing, violations)
				}
				p.respond(http.StatusAccepted, "application/json; charset=utf-8", answer)
				gw, requester := newInboundTestGatewayWithPolicy(t, true, level)
				var mu sync.Mutex
				calls := 0
				gw.cfg.Validator = syntheticEvidenceValidatorFunc(func([]byte) (shnsdk.Result, error) {
					mu.Lock()
					calls++
					mu.Unlock()
					return shnsdk.Result{Valid: true}, nil
				})
				gw.cfg.SoR = nativeReadPanicSoR{}
				gw.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.crd@2.0"}), WithNativeResponseDeclarations(declaredCDSFixtureOutput(t, p.srv.URL)))
				env := shnsdk.Envelope{}
				env.Metadata.CorrelationID, env.Metadata.Sender = "corr-own", requester.ID
				req := conformantCRD("MBR-COVERED", "72148")
				r := newSignedInboundRequest(t, gw, requester.ID)
				ex := ExchangeContext{holder: requester.ID, recipient: "payer", legType: "crd-order-select", operation: "crd-order-select", subjectPCI: "explicit-synthetic-subject", correlationID: "corr-own", contractVersion: "pa.crd@2.0", policy: gw.policy()}
				r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ex))
				rec := httptest.NewRecorder()
				gw.handleNativeInbound(rec, r, "crd-order-select", env, shnsdk.Token{Subject: ex.subjectPCI}, req, "")
				if rec.Code != 200 {
					t.Fatalf("sealed response: %d %s", rec.Code, rec.Body.String())
				}
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || hdr.Status != 202 || hdr.Headers["Content-Type"] != "application/json; charset=utf-8" || !bytes.Equal(body, answer) || p.sentAny() != 1 {
					t.Fatalf("delivery: frame=%+v err=%v body=%q backend=%d", hdr, err, body, p.sentAny())
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := gw.WaitObserverCompletion(ctx); err != nil {
					t.Fatal(err)
				}
				findings, dropped := gw.ConformanceObservationsForTest()
				if dropped != 0 {
					t.Fatalf("optional evidence lost: %d", dropped)
				}
				mu.Lock()
				n := calls
				mu.Unlock()
				if level == EnforcementNone {
					if len(findings) != 0 || n != 0 {
						t.Fatalf("none work: checker=%d findings=%+v", n, findings)
					}
					return
				}
				found := false
				for _, f := range findings {
					if f.Rule == "cds.response" && f.Direction == "response" {
						if f.Gateway != "payer" || f.PayloadSHA256 != sha256hex(answer) || f.State != wantState || f.Action != "not_enforced" || f.Level != "observe" || f.Decision != "" {
							t.Fatalf("backend finding=%+v", f)
						}
						if missing && !slices.ContainsFunc(f.CheckIssues, func(i CheckIssue) bool { return i.Severity == "error" && i.Code == "validator-result" }) {
							t.Fatalf("specific mutation missing: %+v", f)
						}
						found = true
					}
				}
				if !found || n == 0 {
					t.Fatalf("response check not executed: calls=%d findings=%+v", n, findings)
				}
			})
		}
	}
}

// nativeCRDPolicyCase traverses the real response policy and sealed reply path.
// The caller supplies the backend's bytes independently of the request context.
func nativeCRDPolicyCase(t *testing.T, leg string, answer []byte) (int, []byte, *cdsPayer) {
	t.Helper()
	p := newCDSPayer(t, referencePayerServices...)
	p.respond(202, "application/json", answer)
	g, requester := newInboundTestGatewayWithPolicy(t, true, EnforcementStrict)
	g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.crd@2.0"}), WithNativeResponseDeclarations(declaredCDSFixtureOutput(t, p.srv.URL)))
	subject := coveredPCI(t, g)
	ex := ExchangeContext{holder: requester.ID, recipient: "payer", legType: leg, operation: leg, subjectPCI: subject, correlationID: "crd-policy-corpus", contractVersion: "pa.crd@2.0", policy: g.policy()}
	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ex))
	request := conformantCRD("MBR-COVERED", "72148")
	if leg == "crd-order-dispatch" {
		// This response corpus enters through the complete dispatch shape the
		// participant backend expects, so a request-shape failure cannot obscure
		// the answer mutation under test.
		var payload map[string]any
		if err := json.Unmarshal(request, &payload); err != nil {
			t.Fatal(err)
		}
		payload["hook"] = "order-dispatch"
		payload["context"] = map[string]any{"patientId": "MBR-COVERED", "dispatchedOrders": []string{"ServiceRequest/sr1"}, "performer": "Organization/o1"}
		var err error
		request, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		request = bytes.Replace(request, []byte(`"order-select"`), []byte(`"order-sign"`), 1)
	}
	env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: requester.ID, CorrelationID: ex.correlationID}}
	rec := httptest.NewRecorder()
	g.handleNativeInbound(rec, r, leg, env, shnsdk.Token{Subject: subject}, request, "")
	if rec.Code != 200 {
		t.Fatalf("transport status=%d body=%s", rec.Code, rec.Body.String())
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || p.sentAny() != 1 {
		t.Fatalf("frame=%+v err=%v backend=%d body=%s", hdr, err, p.sentAny(), body)
	}
	return hdr.Status, body, p
}
