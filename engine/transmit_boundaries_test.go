package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Each transmit boundary refuses, before anything is sent: a payload this
// gateway authored on a transmit that only relays, the zero Payload, and an
// edited payload whose edit the transmit does not permit. A refusal is a 500
// local fault and a relay.ownership.refused observation. Each boundary also
// has a control row that sends.

type boundaryPayloads struct {
	authored relay.Payload // a real builder the transmit does not list
	zero     relay.Payload
	edited   relay.Payload // no transmit permits an edit yet
}

func newBoundaryPayloads(t *testing.T, id relay.BuilderID, body string) boundaryPayloads {
	t.Helper()
	authored, err := relay.Authored(id, []byte(body), "application/fhir+json")
	if err != nil {
		t.Fatal(err)
	}
	cds := relay.NewBody([]byte(`{"hook":"order-select","hookInstance":"h","fhirServer":"https://ehr.example/fhir"}`), relay.OriginIngressRequest)
	doc, err := relay.Doc(cds)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := relay.Apply(cds, "application/json", relay.EditCDSCallbackStrip, doc.RemoveMember(doc.Root(), "fhirServer"))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Ownership() != relay.OwnershipEdited {
		t.Fatalf("edited payload is %v", edited)
	}
	var zero relay.Payload
	return boundaryPayloads{authored: authored, zero: zero, edited: edited}
}

func (p boundaryPayloads) rows() []struct {
	name    string
	payload relay.Payload
} {
	return []struct {
		name    string
		payload relay.Payload
	}{{"authored", p.authored}, {"zero", p.zero}, {"edited", p.edited}}
}

// refusalCounter counts relay.ownership.refused observations.
type refusalCounter struct {
	mu   sync.Mutex
	seen []ObserverEvent
}

func (c *refusalCounter) observe(e ObserverEvent) {
	if e.Kind != relay.RefusedEvent {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, e)
}

func (c *refusalCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func assertLocalFault(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != errOwnershipFault {
		t.Fatalf("body = %s, want the ownership fault", rec.Body.String())
	}
}

// Requests to the network: roundTripInner, through OriginateLeg.
func TestBoundaryRequestToNetworkRefusesUnpermittedPayloads(t *testing.T) {
	bundle := `{"resourceType":"Bundle","type":"collection"}`
	payloads := newBoundaryPayloads(t, relay.BuilderSDKPASSubmit, bundle)
	for _, row := range payloads.rows() {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			refused := &refusalCounter{}
			env.originator.cfg.Observer = refused.observe
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "pas-claim", "pci-1", "corr-1", "",
				Content{WorkstreamType: workstreamPA, Payload: row.payload, Carried: true})
			if !isOwnershipFault(err) {
				t.Fatalf("err = %v, want an ownership refusal", err)
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a refused request reached the Hub")
			}
			if refused.count() == 0 {
				t.Fatal("the refusal was not observed")
			}
			rec := httptest.NewRecorder()
			if !env.originator.relayOriginationError(rec, err) {
				t.Fatal("the refusal was not answered")
			}
			assertLocalFault(t, rec)
		})
	}
	t.Run("an originated request under a carried row", func(t *testing.T) {
		env := newInProcessExchange(t)
		authored := sealRequest(relay.BuilderSDKPASSubmit, []byte(bundle), "application/fhir+json")
		_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "pas-claim", "pci-1", "corr-1", "",
			Content{WorkstreamType: workstreamPA, Payload: authored, Carried: true})
		if !isOwnershipFault(err) || env.routeHitCount() != 0 {
			t.Fatalf("err = %v, hub calls %d", err, env.routeHitCount())
		}
	})
	t.Run("control: the participant's request, carried exactly", func(t *testing.T) {
		env := newInProcessExchange(t)
		exact := relay.Exact(relay.NewBody([]byte(bundle), relay.OriginIngressRequest), "application/fhir+json")
		env.payerReturns(LegResult{Response: testResponse([]byte(`{"resourceType":"Bundle"}`))})
		_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "pas-claim", "pci-1", "corr-1", "",
			Content{WorkstreamType: workstreamPA, Payload: exact, Carried: true})
		if isOwnershipFault(err) || env.routeHitCount() != 1 {
			t.Fatalf("err = %v, hub calls %d", err, env.routeHitCount())
		}
		if string(env.lastRequestPayload()) != bundle {
			t.Fatalf("request = %s, want the participant's bytes", env.lastRequestPayload())
		}
	})
}

// Requests to the participant's own system: nativeResponder.post.
func TestBoundaryRequestToOwnSystemRefusesUnpermittedPayloads(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	n := &nativeResponder{client: srv.Client()}
	payloads := newBoundaryPayloads(t, relay.BuilderSDKPASSubmit, `{"resourceType":"Bundle"}`)
	for _, row := range payloads.rows() {
		t.Run(row.name, func(t *testing.T) {
			_, _, err := n.post(context.Background(), srv.URL, "/Claim/$submit", row.payload, "pas-claim", "PAS submit")
			if !isOwnershipFault(err) {
				t.Fatalf("err = %v, want an ownership refusal", err)
			}
			if hits.Load() != 0 {
				t.Fatal("a refused request reached the participant's system")
			}
			g, _ := newInboundTestGateway(t, true)
			refused := &refusalCounter{}
			g.cfg.Observer = refused.observe
			rec := httptest.NewRecorder()
			g.responderFailed(rec, "pas-claim", err)
			if rec.Code != http.StatusInternalServerError || refused.count() != 1 {
				t.Fatalf("status %d, refusals observed %d", rec.Code, refused.count())
			}
		})
	}
	t.Run("control: the network's request, forwarded exactly", func(t *testing.T) {
		exact := relay.Exact(relay.NewBody([]byte(`{"resourceType":"Bundle"}`), relay.OriginPeerFrame), "application/fhir+json")
		if _, _, err := n.post(context.Background(), srv.URL, "/Claim/$submit", exact, "pas-claim", "PAS submit"); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 1 {
			t.Fatalf("participant calls = %d, want 1", hits.Load())
		}
	})
}

// Answers to the network: respondLeg and buildResponseLeg (a leg whose
// answer is only ever relayed), and respondLegError (an upstream error).
func TestBoundaryAnswerToNetworkRefusesUnpermittedPayloads(t *testing.T) {
	payloads := newBoundaryPayloads(t, relay.BuilderSDKDTRPackage, `{"resourceType":"Bundle","type":"collection"}`)
	for _, row := range payloads.rows() {
		t.Run("respondLeg/"+row.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			refused := &refusalCounter{}
			g.cfg.Observer = refused.observe
			rec := httptest.NewRecorder()
			g.respondLeg(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch",
				"corr-1", row.payload, "pci-1", requester.ID, "", "")
			assertLocalFault(t, rec)
			if refused.count() == 0 {
				t.Fatal("the refusal was not observed")
			}
		})
		t.Run("buildResponseLeg/"+row.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			// The PAS answer admits the payer's Bundle or an interim assembly,
			// never a PAS submit Bundle this gateway built.
			p := row.payload
			if row.name == "authored" {
				var err error
				if p, err = relay.Authored(relay.BuilderSDKPASSubmit, []byte(`{"resourceType":"Bundle"}`), "application/fhir+json"); err != nil {
					t.Fatal(err)
				}
			}
			out, status, msg := g.buildResponseLeg(newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "pas-response", "pas-claim", "corr-1",
				p, answerKey("pas-claim", relay.OutcomeAnswered), nil, "pci-1", requester.ID, "")
			if out != nil || status != http.StatusInternalServerError || msg != errOwnershipFault {
				t.Fatalf("built %d bytes, status %d %q", len(out), status, msg)
			}
		})
		t.Run("respondLegError/"+row.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			rec := httptest.NewRecorder()
			if row.name == "zero" {
				// An unset Response on an error is the gateway's own refusal,
				// never the participant's bytes.
				g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
					"corr-1", LegResult{Status: http.StatusUnprocessableEntity, Response: row.payload}, "pci-1", requester.ID, "", "")
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || hdr.Status != http.StatusUnprocessableEntity || hdr.Headers["Content-Type"] != "application/json" ||
					!strings.Contains(string(body), "crd-order-select: recipient answered 422") {
					t.Fatalf("frame %+v %s %v", hdr, body, err)
				}
				return
			}
			errPayload := row.payload
			if row.name == "authored" {
				// The refusal writer's own body is the gateway's refusal; any
				// other message this gateway built is not the participant's error.
				var err error
				if errPayload, err = relay.Authored(relay.BuilderCDexFulfillment, []byte(`{"error":"x"}`), "application/json"); err != nil {
					t.Fatal(err)
				}
			}
			g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
				"corr-1", LegResult{Status: http.StatusUnprocessableEntity, Response: errPayload}, "pci-1", requester.ID, "", "")
			assertLocalFault(t, rec)
		})
	}
	t.Run("control: a relayed package", func(t *testing.T) {
		g, requester := newInboundTestGateway(t, true)
		rec := httptest.NewRecorder()
		g.respondLeg(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch",
			"corr-1", relayedResponse([]byte(`{"resourceType":"Bundle"}`)), "pci-1", requester.ID, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("control: the gateway's own refusal on an error", func(t *testing.T) {
		g, requester := newInboundTestGateway(t, true)
		rec := httptest.NewRecorder()
		g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
			"corr-1", LegResult{Status: http.StatusUnprocessableEntity, Message: "refused"}, "pci-1", requester.ID, "", "")
		hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil || hdr.Status != http.StatusUnprocessableEntity || !strings.Contains(string(body), "refused") {
			t.Fatalf("frame %+v %s %v", hdr, body, err)
		}
	})
}

// A payer whose system answers a questionnaire request with a package this
// gateway built (not relayed) is refused: that answer is only ever relayed.
func TestBoundaryQuestionnaireAnswerMustBeRelayed(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	built, err := relay.Authored(relay.BuilderSDKDTRPackage, []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), "application/fhir+json")
	if err != nil {
		t.Fatal(err)
	}
	g.cfg.Responder = pasResultResponder{result: LegResult{Response: built}}
	refused := &refusalCounter{}
	g.cfg.Observer = refused.observe
	rec := httptest.NewRecorder()
	env := shnsdk.Envelope{Metadata: shnsdk.Metadata{CorrelationID: "corr-1", Sender: requester.ID}}
	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withRequestFrameOperation(r.Context(), shnsdk.FrameOperationQuestionnairePackage))
	g.handleDTRInbound(rec, r, env, shnsdk.Token{Subject: coveredPCI(t, g)}, dtrFramedPackageFor(t, g), "")
	assertLocalFault(t, rec)
	if refused.count() == 0 {
		t.Fatal("the refusal was not observed")
	}
}

// Answers to the participant's own system: writePayload (the ingress
// writers and relayed application errors).
func TestBoundaryAnswerToOwnSystemRefusesUnpermittedPayloads(t *testing.T) {
	payloads := newBoundaryPayloads(t, relay.BuilderSDKPASSubmit, `{"resourceType":"Bundle"}`)
	key := relay.Key{Leg: "pas-claim", Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}
	for _, row := range payloads.rows() {
		t.Run(row.name, func(t *testing.T) {
			g, _ := newInboundTestGateway(t, true)
			refused := &refusalCounter{}
			g.cfg.Observer = refused.observe
			rec := httptest.NewRecorder()
			g.writePayload(rec, http.StatusOK, "application/fhir+json", row.payload, key)
			assertLocalFault(t, rec)
			if refused.count() != 1 {
				t.Fatalf("refusals observed = %d", refused.count())
			}
		})
	}
	t.Run("an upstream error on an unknown leg", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&Gateway{}).relayOriginationError(rec, &RelayError{Status: 409, Body: []byte(`{}`), ContentType: "application/json"})
		assertLocalFault(t, rec)
	})
	t.Run("control: the recipient's answer, exactly", func(t *testing.T) {
		g, _ := newInboundTestGateway(t, true)
		rec := httptest.NewRecorder()
		exact := relay.Exact(relay.NewBody([]byte(`{"resourceType":"Bundle"}`), relay.OriginPeerFrame), "application/fhir+json")
		g.writePayload(rec, http.StatusOK, "application/fhir+json", exact, key)
		if rec.Code != http.StatusOK || rec.Body.String() != `{"resourceType":"Bundle"}` {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
}

// Refusals: writeJSON checks its own answer against the writer's scope.
func TestBoundaryRefusalWriterChecksItsScope(t *testing.T) {
	t.Run("a refusal on a transmit the table does not list", func(t *testing.T) {
		g, _ := newInboundTestGateway(t, true)
		refused := &refusalCounter{}
		g.cfg.Observer = refused.observe
		w, scope := g.withScope(httptest.NewRecorder(), relay.RoleRecipient)
		scope.leg = "no-such-leg"
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "refused"})
		rec := scope.ResponseWriter.(*httptest.ResponseRecorder)
		if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 || refused.count() != 1 {
			t.Fatalf("status %d body %q refusals %d", rec.Code, rec.Body.String(), refused.count())
		}
	})
	t.Run("scopes name the leg once it is known", func(t *testing.T) {
		g, _ := newInboundTestGateway(t, true)
		w, scope := g.withScope(httptest.NewRecorder(), relay.RoleRecipient)
		if k, _ := refusalKey(w); k.Leg != relay.LegUnestablished || k.Role != relay.RoleRecipient || k.Outcome != relay.OutcomeRefused {
			t.Fatalf("before the leg: %v", k)
		}
		scope.leg = "pas-claim"
		wrapped := &fhirOperationWriter{w}
		if k, _ := refusalKey(wrapped); k.Leg != "pas-claim" || k.Role != relay.RoleRecipient {
			t.Fatalf("after the leg, through a wrapper: %v", k)
		}
		if again, s := g.withScope(wrapped, relay.RoleRequester); again != wrapped || s != scope {
			t.Fatal("an already scoped writer must keep its scope")
		}
		if k, g := refusalKey(httptest.NewRecorder()); k.Role != relay.RoleRequester || k.Leg != relay.LegUnestablished || g != nil {
			t.Fatalf("an unscoped writer: %v", k)
		}
	})
	t.Run("control: a refusal and a local answer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeJSON(rec, http.StatusForbidden, map[string]string{"error": "refused"})
		if rec.Code != http.StatusForbidden || rec.Body.String() != "{\"error\":\"refused\"}\n" {
			t.Fatalf("%d %q", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		writeJSON(rec, http.StatusOK, map[string]string{"ok": "yes"})
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":\"yes\"}\n" {
			t.Fatalf("%d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("an unsealable request is refused at the boundary", func(t *testing.T) {
		p := sealRequest(relay.BuilderSDKPASSubmit, []byte(`{"a":1,"a":2}`), "application/fhir+json")
		if p.Ownership() != 0 {
			t.Fatalf("a duplicate-member request was sealed: %v", p)
		}
		_, err := relay.Transmit(p, relay.Check(requestKey("pas-claim", false)))
		if !errors.Is(err, relay.ErrUnsetOwnership) {
			t.Fatalf("err = %v", err)
		}
	})
}
