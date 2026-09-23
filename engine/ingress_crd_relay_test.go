package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestCRDIngress_RequestBytesExceptDeclaredEdits drives an EHR's signed
// order-sign request through the provider ingress and the network, and
// compares what the payer's side received with what the EHR sent, byte by
// byte: only the E-01 callback fields are removed. Explicit source assembly
// separately proves that absent prefetch can be obtained from the provider SoR.
func TestCRDIngress_RequestBytesExceptDeclaredEdits(t *testing.T) {
	sentByEHR := signedEHRRequest(t)
	s := newPrefetchSoR()
	medication := searchPage(`{"resourceType":"MedicationRequest","id":"m1","status":"active","intent":"order","subject":{"reference":"Patient/example"},"medicationCodeableConcept":{"text":"a ` + lt + ` b"},"dosageInstruction":[{"doseAndRate":[{"doseQuantity":{"value":0.50}}]}]}`)
	s.answer(t, "MedicationRequest", medication)
	env := newTransportExchange(t)
	env.originator.cfg.SoR = s.sor()
	rec := ingressAt(t, env, "shn-order-sign", sentByEHR)
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
	// Native carriage leaves supplied prefetch alone; missing values are not
	// an implicit request to assemble from the source system.
	wantPrefetch := membersOf(t, sentByEHR, "prefetch")
	if gotPrefetch := membersOf(t, received, "prefetch"); !slices.Equal(gotPrefetch, wantPrefetch) {
		t.Fatalf("prefetch %v, want %v", names(gotPrefetch), names(wantPrefetch))
	}
	// Explicit source assembly remains accountable when invoked. It obtains
	// absent values from the provider SoR, rather than minting them in relay.
	_, assembled := mustPrepare(t, prefetchGateway(s), sentByEHR)
	wantAssembled := append(slices.Clone(wantPrefetch),
		member{"medicationHistory", sorAssembly(t, "MedicationRequest", medication)},
		member{"questionnaireResponses", "null"})
	if got := membersOf(t, assembled, "prefetch"); !slices.Equal(got, wantAssembled) {
		t.Fatalf("source assembly prefetch %v, want %v", names(got), names(wantAssembled))
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
		env := newTransportExchange(t)
		rec := ingressAt(t, env, "shn-order-sign", body)
		if rec.Code != http.StatusOK || !bytes.Equal(sentRequest(t, env), body) {
			t.Fatalf("answer %d; received equals sent: %v", rec.Code, bytes.Equal(sentRequest(t, env), body))
		}
	})
}

// ingressAt runs the whole provider ingress for body posted to
// /cds-services/<service>, through the in-process network, and returns the
// EHR's answer.
func ingressAt(t *testing.T, env *inProcessExchange, service string, body []byte) *httptest.ResponseRecorder {
	return ingressAtVersion(t, env, service, "pa.crd@2.0", body)
}

func ingressAtVersion(t *testing.T, env *inProcessExchange, service, version string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	svc, _, known := env.originator.advertisedCDSServiceByID(service)
	if !known {
		svc = cdsIngressServices[1] // valid signed operation; unknown path is rejected independently
	}
	req := signedFixtureIngress(t, env.originator, "/cds-services/"+service, svc.Leg, svc.Leg, svc.Hook, "pci-covered", version, "crd-ingress", body)
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

// The shared transport stub seals successful payloads verbatim. Frame here so
// the peer's application media type is carried independently of body bytes.
func framedCRDReply(t *testing.T, status int, media string, body []byte) LegResult {
	t.Helper()
	frame, err := shnsdk.EncodeHTTPFrame(status, media, body)
	if err != nil {
		t.Fatal(err)
	}
	return LegResult{Response: relay.ForTest(frame, media)}
}

func framedCRDReplyDeclared(t *testing.T, status int, media, version string, body []byte) LegResult {
	t.Helper()
	frame, err := shnsdk.EncodeHTTPFrameHeaders(status, map[string]string{
		"Content-Type": media, shnsdk.FrameHeaderContractVersion: version,
	}, body)
	if err != nil {
		t.Fatal(err)
	}
	return LegResult{Response: relay.ForTest(frame, media)}
}

func TestCRDIngress_RelaysExactly(t *testing.T) {
	notCovered := bytes.Replace(realCRDAnswer(t), []byte(`"valueCode": "covered"`), []byte(`"valueCode": "not-covered"`), 1)
	if bytes.Equal(notCovered, realCRDAnswer(t)) {
		t.Fatal("fixture: the recorded answer's covered value was not found")
	}
	for _, row := range []struct {
		name   string
		answer []byte
	}{
		{"the reference payer's recorded answer", realCRDAnswer(t)},
		{"a denial", notCovered},
		{"an answer in the payer's own layout", unusualAnswer()},
		{"no card and no system action", []byte(`{"cards":[]}`)},
		{"a legacy card suggestion", []byte(`{"cards":[` + cardWith(`"selectionBehavior":"any","suggestions":[{"label":"Save","actions":[{"type":"update","description":"d","resource":{"resourceType":"DeviceRequest","extension":[{"url":"http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information","extension":[{"url":"covered","valueCode":"covered"},{"url":"pa-needed","valueCode":"no-auth"}]}]}}]}]`) + `]}`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := newTransportExchange(t)
			env.payerReturns(framedCRDReply(t, 200, "application/json", row.answer))
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), row.answer) {
				t.Fatalf("EHR got %d %q\nwant %q", rec.Code, rec.Body.Bytes(), row.answer)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
			if leg, outcome := lastOutcome(t, env.originator); leg != "crd-order-select" || outcome != "ok" {
				t.Fatalf("recorded %s %s, want delivered crd-order-select", leg, outcome)
			}
		})
	}
	for _, r := range certifierRows() {
		t.Run("none relays: "+r.rule, func(t *testing.T) {
			env := newTransportExchange(t)
			env.payerReturns(framedCRDReply(t, 200, "application/json", []byte(r.answer)))
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte(r.answer)) || rec.Header().Get("Content-Type") != "application/json" || env.routeHitCount() != 1 {
				t.Fatalf("the peer's %s answer changed: %d %s", r.rule, rec.Code, rec.Body.String())
			}
			observationFlush(t, env.originator)
			if findings, drops := env.originator.ConformanceObservationsForTest(); len(findings) != 0 || drops != 0 || env.originator.certification != nil {
				t.Fatalf("none ran optional checks: findings=%+v drops=%d", findings, drops)
			}
		})
	}
	t.Run("the payer's error is relayed", func(t *testing.T) {
		env := newTransportExchange(t)
		body := []byte(`{"error":"payer offers no CDS service for hook order-select","offered":["order-sign"]}`)
		env.payerReturns(framedCRDReply(t, http.StatusUnprocessableEntity, "application/json", body))
		rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
		if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), body) || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
		}
	})
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			env := newTransportExchange(t)
			body := []byte(`{"cards":[]}`)
			if status == http.StatusNoContent {
				body = nil
			}
			env.payerReturns(framedCRDReply(t, status, "application/json", body))
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != status || !bytes.Equal(rec.Body.Bytes(), body) || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("peer reply changed: got %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes())
			}
		})
	}
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
	env := newTransportExchange(t)
	answer := realCRDAnswer(t)
	env.payerReturns(LegResult{Response: testResponse(answer)})
	body := dispatchRequest("MBR-COVERED")
	rec := ingressAt(t, env, "shn-order-dispatch", body)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), answer) {
		t.Fatalf("EHR got %d %s", rec.Code, rec.Body.String())
	}
	if leg, outcome := lastOutcome(t, env.originator); leg != "crd-order-dispatch" || outcome != "ok" {
		t.Fatalf("recorded %s %s", leg, outcome)
	}
	want := bytes.Replace(body, []byte(`"fhirServer":"https://provider.example/fhir",
  `), nil, 1)
	if got := sentRequest(t, env); !bytes.Equal(got, want) {
		t.Fatalf("the payer's side received\n%s\nwant\n%s", got, want)
	}
	t.Run("a dispatch prefetch about another patient is carried at none", func(t *testing.T) {
		env := newTransportExchange(t)
		foreign := bytes.Replace(body, []byte(`"subject":{"reference":"Patient/MBR-COVERED"}`), []byte(`"subject":{"reference":"Patient/MBR-NOTCOVERED"}`), 1)
		rec := ingressAt(t, env, "shn-order-dispatch", foreign)
		if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
			t.Fatalf("got %d %s (routed %d)", rec.Code, rec.Body.String(), env.routeHitCount())
		}
		want := bytes.Replace(foreign, []byte(`"fhirServer":"https://provider.example/fhir",
  `), nil, 1)
		if got := sentRequest(t, env); !bytes.Equal(got, want) {
			t.Fatalf("wrong-patient peer bytes changed: got %s want %s", got, want)
		}
	})
}

func TestCRDIngress_DeclaredServiceCarriesContradictoryBodyAtNone(t *testing.T) {
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
			env := newTransportExchange(t)
			body := bytes.Replace(conformantCRDRequest("MBR-COVERED"), []byte(`"hook":"order-select"`), []byte(`"hook":"`+row.hook+`"`), 1)
			rec := ingressAt(t, env, row.service, body)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			want := bytes.Replace(body, []byte("\n      \"fhirServer\":\"https://provider.example/fhir\","), nil, 1)
			want = bytes.Replace(want, []byte("\n      \"fhirAuthorization\":{\"token_type\":\"Bearer\",\"access_token\":\"tok\"},"), nil, 1)
			if got := sentRequest(t, env); !bytes.Equal(got, want) {
				t.Fatalf("declared route changed supplied body: got %s want %s", got, want)
			}
		})
	}
	t.Run("each service accepts its own hook", func(t *testing.T) {
		for _, svc := range []string{"shn-order-sign", "shn-order-select"} {
			env := newTransportExchange(t)
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

func TestCRDIngress_UnknownServiceRejectedAfterAuthentication(t *testing.T) {
	for _, service := range []string{"order-select-crd", "no-such-service", "SHN-ORDER-SIGN", ""} {
		t.Run(service, func(t *testing.T) {
			env := newTransportExchange(t)
			rec := ingressAt(t, env, service, conformantCRDRequest("MBR-COVERED"))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "context_invalid") {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if env.routeHitCount() != 0 {
				t.Fatal("a request to an unknown service crossed the network")
			}
		})
	}
	t.Run("the service is checked after the caller is authenticated", func(t *testing.T) {
		env := newTransportExchange(t)
		env.originator.cfg.ingressAuthBypass = false
		env.originator.ingressAuth = nil
		rec := httptest.NewRecorder()
		env.originator.handleCRDIngress(rec, crdIngressPost(conformantCRDRequest("MBR-COVERED")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestCRDIngress_NoneRelaysWithoutOptionalChecks_ObserveFinds(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		t.Run(level.String(), func(t *testing.T) {
			env := newTransportExchangeWithPolicy(t, level)
			answer := []byte(externalPayerDescriptionlessAnswer)
			env.payerReturns(framedCRDReplyDeclared(t, 202, "application/json", "pa.crd@2.0", answer))
			rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
			if rec.Code != 202 || !bytes.Equal(rec.Body.Bytes(), answer) || rec.Header().Get("Content-Type") != "application/json" || env.routeHitCount() != 1 {
				t.Fatalf("peer reply changed: %d %s %q", rec.Code, rec.Header(), rec.Body.Bytes())
			}
			observationFlush(t, env.originator)
			findings, drops := env.originator.ConformanceObservationsForTest()
			if drops != 0 {
				t.Fatalf("observation drops=%d", drops)
			}
			if level == EnforcementNone {
				if len(findings) != 0 || env.originator.certification != nil {
					t.Fatalf("none ran optional checks: %+v", findings)
				}
				return
			}
			found := false
			for _, f := range findings {
				if f.Direction == "response" && f.Rule == "cds.response" && f.State == CheckInvalid && f.PayloadSHA256 == sha256hex(answer) {
					found = true
				}
			}
			if !found {
				t.Fatalf("observe did not record the peer's invalid reply: %+v", findings)
			}
		})
	}
}
