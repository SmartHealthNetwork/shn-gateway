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
)

// TestCRDIngress_RequestBytesExceptDeclaredEdits drives an EHR's signed
// order-sign request through the provider ingress and the network, and
// compares what the payer's side received with what the EHR sent, byte by
// byte: only fhirServer and fhirAuthorization are gone and only the two
// absent prefetch keys are new; every other member keeps its bytes and its
// place.
func TestCRDIngress_RequestBytesExceptDeclaredEdits(t *testing.T) {
	sentByEHR := signedEHRRequest(t)
	s := newPrefetchSoR()
	medication := searchPage(`{"resourceType":"MedicationRequest","id":"m1","status":"active","intent":"order","subject":{"reference":"Patient/example"},"medicationCodeableConcept":{"text":"a ` + lt + ` b"},"dosageInstruction":[{"doseAndRate":[{"doseQuantity":{"value":0.50}}]}]}`)
	s.answer(t, "MedicationRequest", medication)
	env, rec := ingressRow(t, s, sentByEHR)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	received := sentRequest(t, env)

	// The whole request: the EHR's members, less the callback, in order,
	// each value byte-identical.
	want := slices.DeleteFunc(membersOf(t, sentByEHR), func(m member) bool {
		return m.name == "fhirServer" || m.name == "fhirAuthorization"
	})
	got := membersOf(t, received)
	if len(got) != len(want) {
		t.Fatalf("members %v, want %v", names(got), names(want))
	}
	for i := range want {
		if got[i].name != want[i].name {
			t.Fatalf("member %d is %q, want %q", i, got[i].name, want[i].name)
		}
		if want[i].name == "prefetch" {
			continue
		}
		if got[i].value != want[i].value {
			t.Errorf("%s changed:\n got %s\nwant %s", want[i].name, got[i].value, want[i].value)
		}
	}
	// The prefetch: the EHR's values exactly, then the obtained ones.
	wantPrefetch := append(membersOf(t, sentByEHR, "prefetch"),
		member{"medicationHistory", sorAssembly(t, "MedicationRequest", medication)},
		member{"questionnaireResponses", "null"})
	if gotPrefetch := membersOf(t, received, "prefetch"); !slices.Equal(gotPrefetch, wantPrefetch) {
		t.Fatalf("prefetch %v, want %v", names(gotPrefetch), names(wantPrefetch))
	}

	// Outside the edited places the bytes are the EHR's: everything up to
	// the removed fhirServer member, and everything from context to the end
	// of the EHR's last prefetch value.
	head := sentByEHR[:bytes.Index(sentByEHR, []byte(`"fhirServer"`))]
	if !bytes.HasPrefix(received, head) {
		t.Fatalf("the request does not start with the EHR's bytes %q", head)
	}
	from := bytes.Index(sentByEHR, []byte(`"context" :`))
	last := membersOf(t, sentByEHR, "prefetch")
	lastValue := []byte(last[len(last)-1].value)
	to := bytes.LastIndex(sentByEHR, lastValue) + len(lastValue)
	if from < 0 || to <= from || !bytes.Contains(received, sentByEHR[from:to]) {
		t.Fatal("the EHR's context and prefetch are not carried as one unchanged run of bytes")
	}

	t.Run("a request needing no edit is relayed exactly", func(t *testing.T) {
		body := ehrRequest(supported + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`)
		body = bytes.Replace(body, []byte("  \"fhirServer\" : \"https://ehr.example/fhir\",\n"), nil, 1)
		body = bytes.Replace(body, []byte("  \"fhirAuthorization\" : { \"access_token\" : \"ehr-secret-token\", \"token_type\" : \"Bearer\", \"expires_in\" : 300 },\n"), nil, 1)
		p, sent := mustPrepare(t, prefetchGateway(newPrefetchSoR()), body)
		if p.request.Ownership() != relay.OwnershipRelayed || !bytes.Equal(sent, body) {
			t.Fatalf("ownership %v, bytes equal %v", p.request.Ownership(), bytes.Equal(sent, body))
		}
		env, rec := ingressRow(t, newPrefetchSoR(), body)
		if rec.Code != http.StatusOK || !bytes.Equal(sentRequest(t, env), body) {
			t.Fatalf("answer %d; received equals sent: %v", rec.Code, bytes.Equal(sentRequest(t, env), body))
		}
	})
}

// ingressAt runs the whole provider ingress for body posted to
// /cds-services/<service>, through the in-process network, and returns the
// EHR's answer.
func ingressAt(t *testing.T, env *inProcessExchange, service string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/cds-services/"+service, bytes.NewReader(body))
	req.SetPathValue("id", service)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, req)
	return rec
}

// lastOutcome is the outcome the provider recorded on its last exchange leg.
func lastOutcome(t *testing.T, g *Gateway) (string, string) {
	t.Helper()
	exs := g.ExchangeSnapshot()
	if len(exs) == 0 || len(exs[len(exs)-1].Legs) == 0 {
		t.Fatal("no exchange leg recorded")
	}
	legs := exs[len(exs)-1].Legs
	return legs[len(legs)-1].Type, legs[len(legs)-1].Outcome
}

func TestCRDIngress_RelaysExactly(t *testing.T) {
	notCovered := bytes.Replace(realCRDAnswer(t), []byte(`"valueCode": "covered"`), []byte(`"valueCode": "not-covered"`), 1)
	if bytes.Equal(notCovered, realCRDAnswer(t)) {
		t.Fatal("fixture: the recorded answer's covered value was not found")
	}
	for _, row := range []struct {
		name    string
		answer  []byte
		outcome string
	}{
		{"the reference payer's recorded answer", realCRDAnswer(t), "pa-required"},
		{"a denial", notCovered, "denied"},
		{"an answer in the payer's own layout", unusualAnswer(), "approved"},
		{"no card and no system action", []byte(`{"cards":[]}`), "answered"},
		{"a legacy card suggestion", []byte(`{"cards":[` + cardWith(`"selectionBehavior":"any","suggestions":[{"label":"Save","actions":[{"type":"update","description":"d","resource":{"resourceType":"DeviceRequest","extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]`) + `]}`), "approved"},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.payerReturns(LegResult{Response: testResponse(row.answer)})
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), row.answer) {
				t.Fatalf("EHR got %d %q\nwant %q", rec.Code, rec.Body.Bytes(), row.answer)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
			if leg, outcome := lastOutcome(t, env.originator); leg != "crd-order-select" || outcome != row.outcome {
				t.Fatalf("recorded %s %s, want %s", leg, outcome, row.outcome)
			}
		})
	}
	for _, r := range certifierRows() {
		t.Run("refused: "+r.rule, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.payerReturns(LegResult{Response: testResponse([]byte(r.answer))})
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusBadGateway || !strings.HasPrefix(body.Error, "payer CRD response is not a valid CDS Hooks response: ") || !strings.Contains(body.Error, r.rule) {
				t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte(r.answer)) {
				t.Fatal("the refused answer reached the EHR")
			}
			if _, outcome := lastOutcome(t, env.originator); outcome != "error" {
				t.Fatalf("recorded %s", outcome)
			}
		})
	}
	t.Run("the payer's error is relayed", func(t *testing.T) {
		env := newInProcessExchange(t)
		body := []byte(`{"error":"payer offers no CDS service for hook order-select","offered":["order-sign"]}`)
		env.payerReturns(LegResult{Status: http.StatusUnprocessableEntity, Response: relay.ForTest(body, "application/json")})
		rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
		if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
		}
	})
}

// dispatchRequest is an EHR's order-dispatch request for MBR-COVERED: the
// dispatched order and the coverage are prefetch values.
func dispatchRequest(member string) []byte {
	ref := "Patient/" + member
	return []byte(`{"hook":"order-dispatch","hookInstance":"hi-d1","fhirServer":"https://provider.example/fhir",
  "context":{"patientId":"` + member + `","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/dme1"},
  "prefetch":{"patient":{"resourceType":"Patient","id":"` + member + `"},
    "deviceHistory":{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","status":"active","intent":"order","subject":{"reference":"` + ref + `"},"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431"}]}}}]},
    "coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"` + ref + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]},
    "serviceHistory":null,"medicationHistory":null,"questionnaireResponses":null}}`)
}

func TestCRDIngress_OrderDispatchRoute(t *testing.T) {
	env := newInProcessExchange(t)
	answer := realCRDAnswer(t)
	env.payerReturns(LegResult{Response: testResponse(answer)})
	body := dispatchRequest("MBR-COVERED")
	rec := ingressAt(t, env, "shn-order-dispatch", body)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
		t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
	}
	if leg, outcome := lastOutcome(t, env.originator); leg != "crd-order-dispatch" || outcome != "pa-required" {
		t.Fatalf("recorded %s %s", leg, outcome)
	}
	want := bytes.Replace(body, []byte(`"fhirServer":"https://provider.example/fhir",
  `), nil, 1)
	if got := sentRequest(t, env); !bytes.Equal(got, want) {
		t.Fatalf("the payer's side received\n%s\nwant\n%s", got, want)
	}
	t.Run("a dispatch prefetch about another patient is refused", func(t *testing.T) {
		env := newInProcessExchange(t)
		foreign := bytes.Replace(body, []byte(`"subject":{"reference":"Patient/MBR-COVERED"}`), []byte(`"subject":{"reference":"Patient/MBR-NOTCOVERED"}`), 1)
		rec := ingressAt(t, env, "shn-order-dispatch", foreign)
		if rec.Code != http.StatusForbidden || env.routeHitCount() != 0 {
			t.Fatalf("got %d %s (routed %d)", rec.Code, rec.Body.String(), env.routeHitCount())
		}
	})
}

func TestCRDIngress_ServiceIDHookMismatch400(t *testing.T) {
	for _, row := range []struct {
		service, hook string
	}{
		{"shn-order-sign", "order-select"},
		{"shn-order-select", "order-sign"},
		{"shn-order-select", "order-dispatch"},
		{"shn-order-dispatch", "order-sign"},
		{"shn-order-sign", ""},
	} {
		t.Run(row.service+"/"+row.hook, func(t *testing.T) {
			env := newInProcessExchange(t)
			body := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"hook":"order-select"`), []byte(`"hook":"`+row.hook+`"`), 1)
			rec := ingressAt(t, env, row.service, body)
			want := "CDS service " + row.service + " is for hook "
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a mismatched request crossed the network")
			}
		})
	}
	t.Run("each service accepts its own hook", func(t *testing.T) {
		for _, svc := range []string{"shn-order-sign", "shn-order-select"} {
			env := newInProcessExchange(t)
			hook := strings.TrimPrefix(svc, "shn-")
			body := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"hook":"order-select"`), []byte(`"hook":"`+hook+`"`), 1)
			if rec := ingressAt(t, env, svc, body); rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", svc, rec.Code, rec.Body.String())
			}
			if leg, _ := lastOutcome(t, env.originator); leg != "crd-order-select" {
				t.Fatalf("%s carried on %s", svc, leg)
			}
		}
	})
}

func TestCRDIngress_UnknownService404(t *testing.T) {
	for _, service := range []string{"order-select-crd", "no-such-service", "SHN-ORDER-SIGN", ""} {
		t.Run(service, func(t *testing.T) {
			env := newInProcessExchange(t)
			rec := ingressAt(t, env, service, conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown CDS service") {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a request to an unknown service crossed the network")
			}
		})
	}
	t.Run("the service is checked after the caller is authenticated", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.ingressAuthBypass = false
		env.originator.ingressAuth = nil
		rec := ingressAt(t, env, "no-such-service", conformantCRDRequest("MBR-COVERED"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCRDIngress_RelaysAtNoneWithFinding is the observable-behaviour proof
// that crdAnswerOutcome's use of g.policy() — not a hardcoded strict
// policy — is what the wire actually does: at ConformanceEnforcement=none, a
// payer answer that breaks a non-structural CDS Hooks rule (an action with no
// description) still reaches the EHR byte-identical, and the violation is
// recorded on the observer stream as a cds-envelope finding, whose="peer"
// (this is a peer's answer, received over the network) and decision="relayed".
func TestCRDIngress_RelaysBelowStrict(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementObserve, EnforcementNone} {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var events []ObserverEvent
			env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			env.payerReturns(LegResult{Response: testResponse([]byte(externalPayerDescriptionlessAnswer))})

			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte(externalPayerDescriptionlessAnswer)) {
				t.Fatalf("at %s the payer's answer must relay exactly: got %d %q, want 200 %q", level, rec.Code, rec.Body.Bytes(), externalPayerDescriptionlessAnswer)
			}
			var found bool
			for _, e := range events {
				if e.Kind != ConformanceObservedEvent {
					continue
				}
				if strings.Contains(e.Detail, `"rule":"action.description"`) {
					found = true
					if !strings.Contains(e.Detail, `"whose":"peer"`) {
						t.Fatalf("the provider ingress's finding must say whose=peer (a peer's answer), got: %s", e.Detail)
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

// An unreadable payer answer is relayed exactly below strict and refused at
// strict.
func TestCRDIngress_UnreadableAnswerPerLevel(t *testing.T) {
	const unreadable = `not json at all`
	for _, tc := range []struct {
		level ConformanceEnforcement
		want  int
	}{
		{EnforcementStrict, http.StatusBadGateway},
		{EnforcementObserve, http.StatusOK},
		{EnforcementNone, http.StatusOK},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = tc.level
			env.payerReturns(LegResult{Response: testResponse([]byte(unreadable))})
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != tc.want {
				t.Fatalf("at %s got %d %q, want %d", tc.level, rec.Code, rec.Body.Bytes(), tc.want)
			}
			if tc.want == http.StatusOK && rec.Body.String() != unreadable {
				t.Fatalf("at %s the answer must relay exactly, got %q", tc.level, rec.Body.Bytes())
			}
		})
	}
}
