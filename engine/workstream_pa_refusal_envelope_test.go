//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
)

// legsByAnswerEnvelope is every compiled-in leg of the network's taxonomy
// (PA and FQS, never a test profile's) whose answer envelope is envelope, each
// with its catalog row.
func legsByAnswerEnvelope(t *testing.T, envelope string) map[string]legSpec {
	t.Helper()
	network := map[string]bool{}
	for _, tax := range []legtaxonomy.Taxonomy{legtaxonomy.PA(), legtaxonomy.FQS()} {
		for _, l := range tax.Legs {
			network[l.TransactionType] = true
		}
	}
	out := map[string]legSpec{}
	for _, w := range workstreams {
		for tx, spec := range w.catalog() {
			if network[tx] && spec.AnswerEnvelope == envelope {
				out[tx] = spec
			}
		}
	}
	return out
}

// TestRefusalEnvelope_LegsSplitAsTheTaxonomySays fences the rows below
// against vacuity: the FHIR legs are the eight a client reaches through a
// FHIR operation, and the JSON legs the CDS Hooks pair and patient-authored
// DTR.
func TestRefusalEnvelope_LegsSplitAsTheTaxonomySays(t *testing.T) {
	keys := func(m map[string]legSpec) []string { return slices.Sorted(maps.Keys(m)) }
	if got, want := keys(legsByAnswerEnvelope(t, legtaxonomy.AnswerEnvelopeFHIR)), []string{
		"cdex-attachment-request", "cdex-attachment-submit", "coverage-eligibility", "dtr-questionnaire-fetch",
		"federated-query", "pas-claim", "pas-claim-inquire", "pas-claim-update",
	}; !slices.Equal(got, want) {
		t.Fatalf("FHIR legs = %v, want %v", got, want)
	}
	if got, want := keys(legsByAnswerEnvelope(t, legtaxonomy.AnswerEnvelopeJSON)), []string{
		"crd-order-dispatch", "crd-order-select", "patient-dtr",
	}; !slices.Equal(got, want) {
		t.Fatalf("JSON legs = %v, want %v", got, want)
	}
}

// operationOutcomeFor is the exact body a FHIR leg's gateway refusal carries:
// the OperationOutcome the requesting gateway's own refusals on a FHIR
// operation route already are (fhirOperationValue), so a client reads one
// shape whichever end refused.
func operationOutcomeFor(t *testing.T, status int, msg string) []byte {
	t.Helper()
	b, err := json.Marshal(fhirOperationValue(status, map[string]string{"error": msg}))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRespondLegError_AGatewayRefusalOnAFHIRLegIsAnOperationOutcome: on every
// FHIR leg, the answering gateway's own refusal (no answer from its system to
// carry) reaches the requester as an OperationOutcome, application/fhir+json,
// with the refusal's status and its message as the diagnostics: framed to a
// frame-capable requester, and bare to one that negotiated none.
func TestRespondLegError_AGatewayRefusalOnAFHIRLegIsAnOperationOutcome(t *testing.T) {
	const msg = "not an address this payer issued, or its base or key changed; fetch the package again"
	for tx, spec := range legsByAnswerEnvelope(t, legtaxonomy.AnswerEnvelopeFHIR) {
		for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusGatewayTimeout} {
			t.Run(fmt.Sprintf("%s/%d/framed", tx, status), func(t *testing.T) {
				g, requester := newInboundTestGateway(t, true)
				rec := httptest.NewRecorder()
				g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), spec.RespFrame, spec.RespOp, tx,
					"corr-1", LegResult{Status: status, Message: msg}, "pci-1", requester.ID, "", "")
				if rec.Code != http.StatusOK {
					t.Fatalf("to-Hub status = %d, want 200 (framed)", rec.Code)
				}
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				if hdr.Status != status || hdr.Headers["Content-Type"] != "application/fhir+json" {
					t.Fatalf("frame %d %q, want %d application/fhir+json", hdr.Status, hdr.Headers["Content-Type"], status)
				}
				if want := operationOutcomeFor(t, status, msg); !bytes.Equal(body, want) {
					t.Fatalf("body %s, want %s", body, want)
				}
			})
			t.Run(fmt.Sprintf("%s/%d/bare", tx, status), func(t *testing.T) {
				g, requester := newInboundTestGateway(t, false)
				rec := httptest.NewRecorder()
				g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), spec.RespFrame, spec.RespOp, tx,
					"corr-1", LegResult{Status: status, Message: msg}, "pci-1", requester.ID, "", "")
				if rec.Code != status || rec.Header().Get("Content-Type") != "application/fhir+json" {
					t.Fatalf("bare %d %q, want %d application/fhir+json", rec.Code, rec.Header().Get("Content-Type"), status)
				}
				if want := append(operationOutcomeFor(t, status, msg), '\n'); !bytes.Equal(rec.Body.Bytes(), want) {
					t.Fatalf("body %s, want %s", rec.Body.Bytes(), want)
				}
			})
		}
	}
}

// TestRespondLegError_AGatewayRefusalOnAJSONLegStaysJSON: on the CDS Hooks
// legs and patient-authored DTR, the gateway's own refusal stays
// {"error": msg}, application/json, framed and bare.
func TestRespondLegError_AGatewayRefusalOnAJSONLegStaysJSON(t *testing.T) {
	const msg = "payer offers no CDS service for hook order-select"
	for tx, spec := range legsByAnswerEnvelope(t, legtaxonomy.AnswerEnvelopeJSON) {
		t.Run(tx+"/framed", func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			rec := httptest.NewRecorder()
			g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), spec.RespFrame, spec.RespOp, tx,
				"corr-1", LegResult{Status: http.StatusUnprocessableEntity, Message: msg}, "pci-1", requester.ID, "", "")
			hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if hdr.Status != http.StatusUnprocessableEntity || hdr.Headers["Content-Type"] != "application/json" || string(body) != `{"error":"`+msg+`"}` {
				t.Fatalf("frame %d %q %s, want 422 application/json {\"error\"}", hdr.Status, hdr.Headers["Content-Type"], body)
			}
		})
		t.Run(tx+"/bare", func(t *testing.T) {
			g, requester := newInboundTestGateway(t, false)
			rec := httptest.NewRecorder()
			g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), spec.RespFrame, spec.RespOp, tx,
				"corr-1", LegResult{Status: http.StatusUnprocessableEntity, Message: msg}, "pci-1", requester.ID, "", "")
			if rec.Code != http.StatusUnprocessableEntity || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"error":"`+msg+`"}`+"\n" {
				t.Fatalf("bare %d %q %s, want 422 application/json {\"error\"}", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
			}
		})
	}
}

// TestRespondLegError_AParticipantsOwnErrorIsCarriedAsSent: on every leg
// whose ownership table carries a participant's error, a FHIR leg included,
// the error its own system answered is relayed byte for byte with its own
// media type: only the gateway's own refusal takes the leg's envelope. The
// federated query and patient-authored DTR carry none (their answers are
// built here), and stay so.
func TestRespondLegError_AParticipantsOwnErrorIsCarriedAsSent(t *testing.T) {
	var carriesNone []string
	for _, envelope := range []string{legtaxonomy.AnswerEnvelopeFHIR, legtaxonomy.AnswerEnvelopeJSON} {
		for tx, spec := range legsByAnswerEnvelope(t, envelope) {
			probe := relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginUpstreamResponse), "application/json")
			if _, err := relay.Transmit(probe, relay.Check(answerKey(tx, relay.OutcomeUpstreamError))); err != nil {
				carriesNone = append(carriesNone, tx)
				continue
			}
			for _, row := range []struct{ body, ct string }{
				{`{"error":"the system's own words"}`, "application/json"},
				{`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule","diagnostics":"x"}]}`, "application/fhir+json"},
				{"plain text", "text/plain"},
			} {
				t.Run(tx+"/"+row.ct, func(t *testing.T) {
					g, requester := newInboundTestGateway(t, true)
					rec := httptest.NewRecorder()
					p := relay.Exact(relay.NewBody([]byte(row.body), relay.OriginUpstreamResponse), row.ct)
					g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), spec.RespFrame, spec.RespOp, tx,
						"corr-1", LegResult{Status: http.StatusConflict, Response: p}, "pci-1", requester.ID, "", "")
					hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					if hdr.Status != http.StatusConflict || hdr.Headers["Content-Type"] != row.ct || string(body) != row.body {
						t.Fatalf("frame %d %q %s, want the participant's 409 %q %s", hdr.Status, hdr.Headers["Content-Type"], body, row.ct, row.body)
					}
				})
			}
		}
	}
	if slices.Sort(carriesNone); !slices.Equal(carriesNone, []string{"federated-query", "patient-dtr"}) {
		t.Fatalf("legs that carry no participant error = %v, want [federated-query patient-dtr]", carriesNone)
	}
}

// TestRefuseInbound_TheIssuesEchoOnAFHIRLegIsAnOperationOutcome: a refusal
// with a detail object (a validation verdict echoing the validator's issues)
// on a FHIR leg is an OperationOutcome whose first issue is the refusal and
// whose next issues are the echoed texts, in order; on a JSON leg the detail
// object is written as it is.
func TestRefuseInbound_TheIssuesEchoOnAFHIRLegIsAnOperationOutcome(t *testing.T) {
	issues := []string{"Patient.birthDate: invalid date", "and 3 more"}
	detail := map[string]any{"error": refusalIngressValidation, "issues": issues}
	for _, frameCapable := range []bool{true, false} {
		g, requester := newInboundTestGateway(t, frameCapable)
		rec := httptest.NewRecorder()
		env := shnsdk.Envelope{Metadata: shnsdk.Metadata{TransactionType: "coverage-eligibility", CorrelationID: "corr-1", Sender: requester.ID}}
		g.refuseInbound(rec, newSignedInboundRequest(t, g, requester.ID), legEligibility, env, shnsdk.Token{Subject: "pci-1"}, "",
			http.StatusUnprocessableEntity, refusalIngressValidation, detail)
		status, ct, body := rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()
		if frameCapable {
			hdr, b, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, body))
			if err != nil {
				t.Fatal(err)
			}
			status, ct, body = hdr.Status, hdr.Headers["Content-Type"], b
		}
		var got struct {
			ResourceType string
			Issue        []struct{ Severity, Code, Diagnostics string }
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("framed=%v: %v (%s)", frameCapable, err, body)
		}
		type issue = struct{ Severity, Code, Diagnostics string }
		want := []issue{
			{"error", "not-supported", refusalIngressValidation},
			{"error", "invalid", issues[0]},
			{"error", "invalid", issues[1]},
		}
		if status != http.StatusUnprocessableEntity || ct != "application/fhir+json" || got.ResourceType != "OperationOutcome" || !slices.Equal(got.Issue, want) {
			t.Fatalf("framed=%v: %d %q %+v, want 422 application/fhir+json OperationOutcome %+v", frameCapable, status, ct, got, want)
		}
	}
}

// TestRefuseInbound_ADetailMemberAnOperationOutcomeCannotHoldIsRefused: on a
// FHIR leg, a detail object with a member other than its error and its echoed
// issues, or issues that are not a list of texts, is this gateway's own fault
// (the ownership fault), never written with the member dropped. On a JSON leg
// the same object is written as it is.
func TestRefuseInbound_ADetailMemberAnOperationOutcomeCannotHoldIsRefused(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"another member":       {"error": "x", "offered": []string{"order-select"}},
		"issues not texts":     {"error": "x", "issues": []int{1}},
		"issues not a list":    {"error": "x", "issues": "one"},
		"another member alone": {"error": "x", "hint": "y"},
		"another message":      {"error": "y", "issues": []string{"z"}},
		"a message not a text": {"error": 1},
	} {
		t.Run(name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, false)
			rec := httptest.NewRecorder()
			env := shnsdk.Envelope{Metadata: shnsdk.Metadata{TransactionType: "coverage-eligibility", CorrelationID: "corr-1", Sender: requester.ID}}
			g.refuseInbound(rec, newSignedInboundRequest(t, g, requester.ID), legEligibility, env, shnsdk.Token{Subject: "pci-1"}, "",
				http.StatusUnprocessableEntity, "x", detail)
			assertLocalFault(t, rec)
		})
	}
	t.Run("a JSON leg keeps its detail", func(t *testing.T) {
		g, requester := newInboundTestGateway(t, false)
		rec := httptest.NewRecorder()
		leg := inboundLeg{"payer-coverage", "crd-cards", "crd-order-select"}
		env := shnsdk.Envelope{Metadata: shnsdk.Metadata{TransactionType: leg.tx, CorrelationID: "corr-1", Sender: requester.ID}}
		g.refuseInbound(rec, newSignedInboundRequest(t, g, requester.ID), leg, env, shnsdk.Token{Subject: "pci-1"}, "",
			http.StatusUnprocessableEntity, "x", map[string]any{"error": "x", "offered": []string{"order-select"}})
		if rec.Code != http.StatusUnprocessableEntity || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"error":"x","offered":["order-select"]}` {
			t.Fatalf("%d %q %s, want the detail as it is", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	})
}

// gatewayRefusal reads this gateway's own refusal on leg tx from an answer's
// body and media type, holding it to the leg's answer envelope: an
// OperationOutcome (application/fhir+json) on a FHIR leg, {"error"}
// (application/json) otherwise. It returns the refusal's message and, on a
// FHIR leg, the issues it echoes.
func gatewayRefusal(t *testing.T, tx, contentType string, body []byte) (string, []string) {
	t.Helper()
	if !answersFHIR(tx) {
		var r struct {
			Error string `json:"error"`
		}
		if contentType != "application/json" || json.Unmarshal(body, &r) != nil {
			t.Fatalf("%s: refusal %q %s, want application/json {\"error\"}", tx, contentType, body)
		}
		return r.Error, nil
	}
	var o struct {
		ResourceType string
		Issue        []struct{ Severity, Code, Diagnostics string }
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if contentType != "application/fhir+json" || dec.Decode(&o) != nil || o.ResourceType != "OperationOutcome" || len(o.Issue) == 0 {
		t.Fatalf("%s: refusal %q %s, want an application/fhir+json OperationOutcome", tx, contentType, body)
	}
	var echoed []string
	for i, is := range o.Issue {
		if is.Severity != "error" || is.Code == "" || is.Diagnostics == "" || (i > 0 && is.Code != "invalid") {
			t.Fatalf("%s: issue %d %+v is not this gateway's refusal shape", tx, i, is)
		}
		if i > 0 {
			echoed = append(echoed, is.Diagnostics)
		}
	}
	return o.Issue[0].Diagnostics, echoed
}

// ingressRouteLegs is each ingress route by the legs it originates. A route a
// participant's own system calls is the leg's client surface, so the leg's
// answer envelope must be the route's.
var ingressRouteLegs = map[string][]string{
	RouteCRD:              {"crd-order-select", "crd-order-dispatch"},
	RouteDTR:              {"dtr-questionnaire-fetch"},
	RoutePAS:              {"pas-claim", "pas-claim-update"},
	RoutePASInquire:       {"pas-claim-inquire"},
	RouteEligibility:      {"coverage-eligibility"},
	RouteCDexSubmit:       {"cdex-attachment-submit"},
	RouteCDexReturnSubmit: {"cdex-attachment-submit"},
	RouteCDexRequestBase:  {"cdex-attachment-request"},
	RouteCDexRequest:      {"cdex-attachment-request"},
	RouteFederatedQuery:   {"federated-query"},
}

// TestAnswerEnvelope_IsTheIngressRoutesEnvelope fences the taxonomy's answer
// envelopes to the routes a participant's own system calls: a leg answers
// FHIR exactly when the ingress route that originates it is a FHIR operation
// route (its handler writes this gateway's own refusals as an
// OperationOutcome), every ingress route names its legs here, and every leg
// with an envelope has an ingress route but patient-authored DTR, which only
// module 1 originates.
func TestAnswerEnvelope_IsTheIngressRoutesEnvelope(t *testing.T) {
	covered := map[string]bool{}
	routes := workstreamIngressRouteNames()
	if len(routes) < 10 {
		t.Fatalf("%d ingress routes, want >= 10: the fence would be vacuous", len(routes))
	}
	for _, route := range routes {
		legs, ok := ingressRouteLegs[route]
		if !ok {
			t.Errorf("ingress route %q names no legs here: add it with the legs it originates", route)
			continue
		}
		h, _ := workstreamIngressRoute(route)
		w := httptest.NewRecorder()
		h.handler(&Gateway{})(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
		writesFHIR := w.Header().Get("Content-Type") == "application/fhir+json"
		if w.Code < 400 || (!writesFHIR && w.Header().Get("Content-Type") != "application/json") {
			t.Fatalf("%s: answered %d %q, want its own refusal", route, w.Code, w.Header().Get("Content-Type"))
		}
		for _, leg := range legs {
			covered[leg] = true
			if answersFHIR(leg) != writesFHIR {
				t.Errorf("%s: leg %s answers FHIR %v, its ingress route writes FHIR %v", route, leg, answersFHIR(leg), writesFHIR)
			}
		}
	}
	for _, envelope := range []string{legtaxonomy.AnswerEnvelopeFHIR, legtaxonomy.AnswerEnvelopeJSON} {
		for leg := range legsByAnswerEnvelope(t, envelope) {
			if !covered[leg] && leg != "patient-dtr" {
				t.Errorf("leg %s has no ingress route here", leg)
			}
		}
	}
}
