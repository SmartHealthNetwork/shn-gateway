package engine

import (
	"bytes"
	"context"
	"encoding/json"
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
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
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
	for _, r := range rows {
		for _, leg := range []string{"crd-order-select", "crd-order-dispatch"} {
			t.Run(leg+"/"+r.rule, func(t *testing.T) {
				hook := "order-sign"
				if leg == "crd-order-dispatch" {
					hook = "order-dispatch"
				}
				if v := shnsdk.CheckCDSHooksResponse([]byte(r.answer), "2.2"); !slices.ContainsFunc(v, func(v shnsdk.Violation) bool { return v.Rule == r.rule }) {
					t.Fatalf("the row does not break %s: %v", r.rule, v)
				}
				res, p, _ := relayCase(t, leg, hook, "application/json", []byte(r.answer))
				const prefix = "payer CRD response is not a valid CDS Hooks response: "
				if res.Status != http.StatusBadGateway || !strings.HasPrefix(res.Message, prefix) || !strings.Contains(res.Message, r.rule) {
					t.Fatalf("got %d %q", res.Status, res.Message)
				}
				if res.Response.Ownership() != 0 {
					t.Fatal("the refusal carries the payer's answer")
				}
				if p.sentAny() != 1 {
					t.Fatalf("%d requests reached the payer, want 1", p.sentAny())
				}
			})
		}
	}
	t.Run("the refusal names where each rule is broken", func(t *testing.T) {
		answer := withCard(`{"summary":"s","indicator":"info","source":{}}`)
		res, _, _ := relayCase(t, "crd-order-select", "order-sign", "application/json", []byte(answer))
		want := "payer CRD response is not a valid CDS Hooks response: card.source.label at cards[0].source.label; card.source.topic at cards[0].source.topic"
		if res.Message != want {
			t.Fatalf("message %q\nwant %q", res.Message, want)
		}
	})
	t.Run("a long list is shortened", func(t *testing.T) {
		cards := strings.TrimSuffix(strings.Repeat(`"x",`, 7), ",")
		res, _, _ := relayCase(t, "crd-order-select", "order-sign", "application/json", []byte(`{"cards":[`+cards+`]}`))
		if !strings.HasSuffix(res.Message, "; and 2 more") || strings.Count(res.Message, "card.object") != 5 {
			t.Fatalf("message %q", res.Message)
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
				g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
				var mu sync.Mutex
				var validated [][]byte
				g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
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
				tail.handle(g, rec, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Subject: pci})
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d %s", rec.Code, rec.Body.String())
				}
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(body, answer) {
					t.Fatalf("the requester got %v %d %s", err, hdr.Status, body)
				}
				var embedded []byte
				for _, b := range validated {
					if bytes.Contains(b, []byte(covInfo)) {
						embedded = b
					}
				}
				if !bytes.Contains(answer, embedded) || len(embedded) == 0 || embedded[0] != '{' {
					t.Fatalf("the embedded order was not validated as sent: %s", embedded)
				}
				var got []crdEmbeddedValidation
				for _, e := range events {
					if e.Kind != CRDEmbeddedValidatedEvent {
						continue
					}
					if e.LegType != tail.leg || e.CorrelationID != "corr-1" || len(e.Payload) != 0 {
						t.Errorf("event %+v", e)
					}
					var v crdEmbeddedValidation
					if err := json.Unmarshal([]byte(e.Detail), &v); err != nil {
						t.Fatal(err)
					}
					got = append(got, v)
				}
				want := []crdEmbeddedValidation{{Path: "systemActions[0].resource", ResourceType: "DeviceRequest", Line: answerLineOr(context.Background(), "pa.crd"), Outcome: verdict.outcome}}
				if !slices.Equal(got, want) {
					t.Fatalf("recorded %+v, want %+v", got, want)
				}
			})
		}
	}
	t.Run("a refused answer is not validated", func(t *testing.T) {
		g, requester := newInboundTestGateway(t, true)
		p := newCDSPayer(t, referencePayerServices...)
		p.respond(http.StatusOK, "application/json", []byte(withCard(`{"summary":"s","indicator":"info"}`)))
		g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
		calls := 0
		g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
			calls++
			return shnsdk.Result{Valid: true}, nil
		})
		pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
		env := shnsdk.Envelope{}
		env.Metadata.CorrelationID, env.Metadata.Sender = "corr-1", requester.ID
		req := bytes.Replace(conformantCRD("MBR-COVERED", "72148"), []byte(`"order-select"`), []byte(`"order-sign"`), 1)
		rec := httptest.NewRecorder()
		g.handleCRDNativeInbound(rec, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Subject: pci}, req, "")
		hdr, _, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil || hdr.Status != http.StatusBadGateway {
			t.Fatalf("got %v %d", err, hdr.Status)
		}
		if calls != 2 { // the order and the coverage the request carried; nothing from the answer
			t.Fatalf("validator ran %d times, want 2", calls)
		}
	})
}

// The embedded validation is a conformance check: at none it does not run (no
// validator call, no event) and the answer is relayed; at observe it runs and
// records its event, and the answer is relayed.
func TestNativeCRD_EmbeddedValidationOnlyWhereChecksRun(t *testing.T) {
	const covInfo = "ext-coverage-information"
	answer := realCRDAnswer(t)
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural} {
		for _, tail := range embeddedValidationTails {
			t.Run(tail.leg+"/"+level.String(), func(t *testing.T) {
				g, requester := newInboundTestGateway(t, true)
				g.cfg.ConformanceEnforcement = level
				p := newCDSPayer(t, referencePayerServices...)
				g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil, WithConformancePolicy(NewConformancePolicy(level)))
				var mu sync.Mutex
				embeddedCalls, calls := 0, 0
				g.cfg.Validator = validatorFunc(func(b []byte) (shnsdk.Result, error) {
					mu.Lock()
					defer mu.Unlock()
					calls++
					if bytes.Contains(b, []byte(covInfo)) {
						embeddedCalls++
					}
					return shnsdk.Result{Valid: true}, nil
				})
				embeddedEvents := 0
				g.cfg.Observer = func(e ObserverEvent) {
					mu.Lock()
					defer mu.Unlock()
					if e.Kind == CRDEmbeddedValidatedEvent {
						embeddedEvents++
					}
				}
				pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
				env := shnsdk.Envelope{}
				env.Metadata.CorrelationID, env.Metadata.Sender = "corr-1", requester.ID
				rec := httptest.NewRecorder()
				tail.handle(g, rec, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Subject: pci})
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || hdr.Status != http.StatusOK || !bytes.Equal(body, answer) {
					t.Fatalf("the answer must be relayed: %v %d %s", err, hdr.Status, body)
				}
				switch level {
				case EnforcementNone:
					if calls != 0 || embeddedEvents != 0 {
						t.Fatalf("at none nothing is validated: %d validator call(s), %d %s event(s)", calls, embeddedEvents, CRDEmbeddedValidatedEvent)
					}
				case EnforcementObserve, EnforcementStructural:
					if embeddedCalls != 1 || embeddedEvents != 1 {
						t.Fatalf("at %s the embedded resource is validated and recorded: %d call(s), %d event(s)", level, embeddedCalls, embeddedEvents)
					}
				}
			})
		}
	}
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
		{EnforcementStructural, http.StatusBadGateway, "action.description at systemActions[0].description"},
		{EnforcementObserve, 0, ""},
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
			if tc.level == EnforcementNone {
				if len(findings) != 0 {
					t.Fatalf("at none no rule runs, so nothing is recorded: %+v", findings)
				}
				return
			}
			if len(findings) == 0 {
				t.Fatal("every violation is recorded at every level that runs the rules")
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
			if tc.level == EnforcementObserve {
				wantDecision = "relayed"
			}
			if f.Decision != wantDecision {
				t.Fatalf("finding decision = %q, want %q", f.Decision, wantDecision)
			}
		})
	}
}

// An unreadable answer refuses only at strict: at
// observe it is recorded and relayed, at none nothing runs.
func TestCDSCertifierUnreadableAnswerPerLevel(t *testing.T) {
	for _, tc := range []struct {
		level        ConformanceEnforcement
		wantStatus   int
		wantFindings int
	}{
		{EnforcementStrict, http.StatusBadGateway, 1},
		{EnforcementStructural, http.StatusBadGateway, 1},
		{EnforcementObserve, 0, 1},
		{EnforcementNone, 0, 0},
	} {
		var findings []ConformanceFinding
		got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(tc.level),
			func(f ConformanceFinding) { findings = append(findings, f) },
			[]byte(`not json at all`), "2.0", "payer")
		if got.Status != tc.wantStatus || len(findings) != tc.wantFindings {
			t.Errorf("%s: status %d with %d findings, want %d with %d", tc.level, got.Status, len(findings), tc.wantStatus, tc.wantFindings)
		}
	}
	// The other value of the pending row refuses it below strict too.
	if got := certifyCDSHooksAnswer(context.Background(), newConformancePolicy(EnforcementNone, true), nil, []byte(`not json at all`), "2.0", "payer"); got.Status != http.StatusBadGateway {
		t.Fatalf("with the row set to refuse, an unreadable answer must refuse at none, got %d", got.Status)
	}
}

// A responder the engine never wired still refuses at strict and never panics
// when it emits.
func TestCDSCertifierUnwiredEmitterIsSafe(t *testing.T) {
	got := certifyCDSHooksAnswer(context.Background(), ConformancePolicy{}, nil,
		[]byte(externalPayerDescriptionlessAnswer), "2.0", "peer")
	if got.Status != http.StatusBadGateway {
		t.Fatalf("an unwired responder is strict by the zero value, got %d", got.Status)
	}
}

// action.resourceId is the one SHOULD-level (warning) CDS Hooks rule: a
// delete action naming no resource never refuses, at either level — but it is
// still recorded. Deleting the advisory emit(...) call would leave this
// green-at-zero-findings instead of red.
func TestCDSCertifierAdvisoryRecordsAtBothLevels(t *testing.T) {
	answer := []byte(`{"cards":[],"systemActions":[{"type":"delete","description":"remove the draft"}]}`)
	if got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(EnforcementNone),
		func(f ConformanceFinding) { t.Fatalf("at none no rule runs, got finding %+v", f) }, answer, "2.0", "peer"); got.Status != 0 {
		t.Fatalf("at none nothing refuses, got %d", got.Status)
	}
	for _, level := range []ConformanceEnforcement{EnforcementStrict, EnforcementStructural, EnforcementObserve} {
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
		Validator:       shnsdk.NewFakeValidator(),
		SoR:             sor,
		Store:           sor,
		Responder:       n,
		Clock:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if n.emitFinding == nil {
		t.Fatal("engine.New must bind its finding emitter to a raw *nativeResponder passed as Config.Responder")
	}
}

// TestForwardCRD_RelaysAtNoneWithOwnFinding drives forwardCRD through a REAL
// engine.New-constructed payer gateway (not a bare Handle call): the
// responder's WithConformancePolicy option and the emitter engine.New binds
// to it must both actually take effect, and the finding forwardCRD emits
// must say "own" — the certified bytes are this gateway's OWN backend
// answering, never a peer's — not the "peer" value that is only correct at
// the provider-ingress certifier (crdAnswerOutcome). At structural the same
// answer refuses, since its missing action description is a required member.
func TestForwardCRD_RelaysBelowStrict(t *testing.T) {
	// At structural the missing required member (action.description) refuses:
	// that row checks the refusal and the refused finding, whose=own.
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementNone, EnforcementStructural} {
		t.Run(level.String(), func(t *testing.T) {
			p := newCDSPayer(t, referencePayerServices...)
			p.respond(http.StatusOK, "application/json", []byte(externalPayerDescriptionlessAnswer))

			authzPub, _ := genED25519(t)
			_, paySignPriv := genED25519(t)
			payEncPub, payEncPriv := genKeyPair(t)
			sor := newCensusSoR()
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil,
				WithConformancePolicy(NewConformancePolicy(level)))
			gw := mustNew(t, Config{
				Role:            "payer",
				HolderID:        "payer",
				Identity:        shnsdk.Identity{HolderID: "payer", SignPriv: paySignPriv, EncPub: payEncPub, EncPriv: payEncPriv},
				AuthzURL:        "http://stub.test",
				AuthzPub:        authzPub,
				HubTransportPub: authzPub,
				Reg:             shnsdk.NewRegistry(),
				Validator:       shnsdk.NewFakeValidator(),
				SoR:             sor,
				Store:           sor,
				Responder:       n,
				Clock:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
			})
			var events []ObserverEvent
			gw.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }

			res, err := n.Handle(context.Background(), "crd-order-select", "corr-own", "pci", cdsRequest("order-sign"))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if level == EnforcementStructural {
				if res.Status != http.StatusBadGateway || !strings.Contains(res.Message, "action.description") {
					t.Fatalf("at structural a missing action description refuses, got %d %s", res.Status, res.Message)
				}
				var refused bool
				for _, e := range events {
					if e.Kind == ConformanceObservedEvent && strings.Contains(e.Detail, `"rule":"action.description"`) &&
						strings.Contains(e.Detail, `"whose":"own"`) && strings.Contains(e.Detail, `"decision":"refused"`) && strings.Contains(e.Detail, `"level":"structural"`) {
						refused = true
					}
				}
				if !refused {
					t.Fatalf("at structural want a refused action.description finding, whose=own, got %+v", events)
				}
				return
			}
			if res.Status != 0 {
				t.Fatalf("at %s the payer's own backend's malformed answer must still relay, got %d %s", level, res.Status, res.Message)
			}
			if got := relay.BytesForTest(res.Response); !bytes.Equal(got, []byte(externalPayerDescriptionlessAnswer)) {
				t.Fatalf("answer changed:\n got %s\nwant %s", got, externalPayerDescriptionlessAnswer)
			}
			var found bool
			for _, e := range events {
				if e.Kind != ConformanceObservedEvent {
					continue
				}
				if strings.Contains(e.Detail, `"rule":"action.description"`) {
					found = true
					if !strings.Contains(e.Detail, `"whose":"own"`) {
						t.Fatalf("forwardCRD's finding must say whose=own (this gateway's own backend), got: %s", e.Detail)
					}
					if !strings.Contains(e.Detail, `"decision":"relayed"`) || !strings.Contains(e.Detail, `"level":"observe"`) {
						t.Fatalf("finding must record relayed/observe, got: %s", e.Detail)
					}
				}
			}
			if found != (level == EnforcementObserve) {
				t.Fatalf("at %s cds-envelope finding observed = %v; observe records it, none runs no rule", level, found)
			}
		})
	}
}

// A CDS Hooks answer that repeats a member name refuses at every level: a body
// read two ways cannot be carried faithfully.
func TestCDSCertifierRepeatedMemberRefusesAtEveryLevel(t *testing.T) {
	answer := []byte(`{"cards":[],"cards":[]}`)
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStructural, EnforcementStrict} {
		if got := certifyCDSHooksAnswer(context.Background(), NewConformancePolicy(level), nil, answer, "2.0", "peer"); got.Status != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", level, got.Status)
		}
	}
}
