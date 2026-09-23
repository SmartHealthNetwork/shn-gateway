package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

type nativeReadPanicSoR struct{ SystemOfRecord }

func TestNativeRelaySuppliedContext(t *testing.T) {
	for _, compatibility := range []bool{false, true} {
		for _, row := range []struct{ name, body string }{
			{"unknown local member", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/external"}}}]}`},
			{"mismatched patients", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/a"}}},{"resource":{"resourceType":"Coverage","beneficiary":{"reference":"Patient/b"}}}]}`},
			{"malformed clinical bytes", `not FHIR`},
			{"signed submit overrides amendment body", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","related":[{}],"identifier":[{"system":"urn:shn:correlation","value":"body-correlation"}]}}]}`},
		} {
			t.Run(fmt.Sprintf("%s/compatibility=%v", row.name, compatibility), func(t *testing.T) {
				pair := newInProcessExchange(t)
				g := pair.originator
				g.cfg.AcceptUnknownMembers = compatibility
				g.cfg.ConformanceEnforcement = EnforcementNone
				g.cfg.SoR = nativeReadPanicSoR{}
				g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) { panic("signed path resolved a patient") })
				g.cfg.ingressAuthBypass = false
				key, pub := newTestClientKey(t)
				g.ingressAuth = newTestAuthServer(t, "native-source", pub, "ES384")
				reg := g.ingressAuth.clients["native-source"]
				reg.ContextOperations = []string{"pas-submit"}
				g.ingressAuth.clients["native-source"] = reg
				entry, _ := g.cfg.Reg.Lookup("payer")
				entry.RequestFrames = shnsdk.SupportedRequestFrames()
				g.cfg.Reg.Set("payer", entry)
				body := []byte(row.body)
				r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/custom+json; charset=utf-8")
				now := g.ingressAuth.now()
				c := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "native-source", Subject: "native-source", Audience: jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}, ID: "context-once", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "externally-established-pci", CorrelationID: "native-correlation", ContentType: r.Header.Get("Content-Type"), ContractVersion: "pa.pas@2.0"}
				putContext(t, r, c, body, key)
				r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims(c.Issuer, testIngressBaseURL+r.URL.Path, now)))
				answer := []byte(`opaque unknown decision and incomplete graph`)
				framed, err := shnsdk.EncodeHTTPFrame(201, "application/answer+json", answer)
				if err != nil {
					t.Fatal(err)
				}
				pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody(framed, relay.OriginPeerFrame), "application/json")})
				w := httptest.NewRecorder()
				g.Handler().ServeHTTP(w, r)
				if w.Code != 201 || w.Header().Get("Content-Type") != "application/answer+json" || !bytes.Equal(w.Body.Bytes(), answer) {
					t.Fatalf("reply %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
				}
				var requestToken shnsdk.Token
				if err := json.Unmarshal([]byte(pair.substrate.lastMetadata.AuthzToken), &requestToken); err != nil {
					t.Fatal(err)
				}
				if pair.substrate.lastMetadata.TransactionType != c.Leg || requestToken.Operation != c.Operation || w.Header().Get(CorrelationHeader) != c.CorrelationID {
					t.Fatal("signed context overridden by body metadata")
				}
				header, sent, err := shnsdk.DecodeHTTPFrame(pair.lastRequestPayload())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(sent, body) || header.Headers["Content-Type"] != r.Header.Get("Content-Type") || pair.routeHitCount() != 1 {
					t.Fatalf("request changed header=%+v bytes=%s hits=%d", header, sent, pair.routeHitCount())
				}
			})
		}
	}
}

func TestNativeRelayPASDoesNotConsumeClinicalState(t *testing.T) {
	for _, leg := range []string{"pas-claim", "pas-claim-update", "pas-claim-inquire"} {
		t.Run(leg, func(t *testing.T) {
			request := []byte(`no ServiceRequest or local pend`)
			answer := []byte(`unknown decision; no Organization graph`)
			hits := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, request) {
					t.Errorf("changed request %s", body)
				}
				w.Header().Set("Content-Type", "application/backend+json")
				w.WriteHeader(202)
				w.Write(answer)
			}))
			defer upstream.Close()
			n := NewNativeResponder(upstream.Client(), upstream.URL, "", nil, time.Now)
			result, err := n.Handle(context.Background(), leg, "corr", "external-pci", request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Commit != nil || result.Rollback != nil || len(result.SideEffectFHIR) != 0 {
				t.Fatal("native relay scheduled clinical state")
			}
			raw, err := relay.Transmit(result.Response, relay.Check(answerKey(leg, relay.OutcomeAnswered)))
			if err != nil || hits != 1 || result.ApplicationStatus != 202 || result.Response.ContentType() != "application/backend+json" || !bytes.Equal(raw, answer) {
				t.Fatalf("hits=%d reply=%+v bytes=%s err=%v", hits, result, raw, err)
			}
		})
	}
}

func TestNativeRelayRecipientIgnoresClinicalClosures(t *testing.T) {
	for _, authored := range []bool{false, true} {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("authored=%v/panic=%v", authored, panics), func(t *testing.T) {
				g, requester := newInboundTestGateway(t, true)
				g.cfg.ConformanceEnforcement = EnforcementNone
				pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
				body := conformantPASBundleWithQR(t, "MBR-COVERED")
				commits, rollbacks := 0, 0
				answer := []byte(assemblyRealPending)
				response := relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), "application/answer+json")
				if authored {
					var err error
					response, err = relay.Authored(relay.BuilderSDKPASSubmit, answer, "application/answer+json")
					if err != nil {
						t.Fatal(err)
					}
				}
				g.cfg.Responder = pasResultResponder{result: LegResult{ApplicationStatus: 201, Response: response, SideEffectFHIR: [][]byte{[]byte("invalid projected EOB")}, Commit: func() error {
					commits++
					if panics {
						panic("clinical closure executed")
					}
					return errors.New("clinical ledger unavailable")
				}, Rollback: func() { rollbacks++ }}}
				env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim", CorrelationID: "native-corr"}}
				w := httptest.NewRecorder()
				g.handlePASNativeInbound(w, newSignedInboundRequest(t, g, requester.ID), env, shnsdk.Token{Subject: pci}, body, "pa.pas@2.0")
				if commits != 0 || rollbacks != 1 {
					t.Fatalf("commits=%d rollbacks=%d", commits, rollbacks)
				}
				if authored {
					if w.Code != 500 {
						t.Fatalf("unregistered answer builder admitted: %d", w.Code)
					}
					return
				}
				if w.Code != 200 {
					t.Fatalf("transport refusal %d %s", w.Code, w.Body.String())
				}
				header, got, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, w.Body.Bytes()))
				if err != nil || header.Status != 201 || !bytes.Equal(got, answer) {
					t.Fatalf("reply=%+v err=%v", header, err)
				}
			})
		}
	}
}

func TestNativeRelayDeclaredCRDService(t *testing.T) {
	for _, hook := range []string{"order-select", "order-sign"} {
		for _, body := range []string{`not JSON`, `{"hook":"order-dispatch"}`} {
			t.Run(hook+body, func(t *testing.T) {
				backend := newCDSPayer(t, CDSService{ID: "select-service", Hook: "order-select"}, CDSService{ID: "sign-service", Hook: "order-sign"})
				backend.respond(202, "application/answer+json", []byte(`unparsed reply`))
				n := NewNativeResponder(backend.srv.Client(), backend.srv.URL, "", nil, time.Now)
				ctx := context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{legType: "crd-order-select", crdHook: hook, contentType: "application/input+json"})
				result, err := n.Handle(ctx, "crd-order-select", "corr", "pci", []byte(body))
				service := "select-service"
				if hook == "order-sign" {
					service = "sign-service"
				}
				sent := backend.sent(service)
				if err != nil || result.ApplicationStatus != 202 || len(sent) != 1 || !bytes.Equal(sent[0], []byte(body)) {
					t.Fatalf("declared routing reply=%+v err=%v sent=%q", result, err, sent)
				}
			})
		}
	}
}

func TestNativeRelayOriginPolicy(t *testing.T) {
	for _, row := range []struct {
		name            string
		level           ConformanceEnforcement
		request, answer string
		status, hits    int
	}{
		{"none opaque", EnforcementNone, "broken request", "broken answer", 0, 1},
		{"observe opaque", EnforcementObserve, "broken request", "broken answer", 0, 1},
		{"basic request refused", EnforcementBasic, "broken request", "broken answer", 422, 0},
		{"basic response refused", EnforcementBasic, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, "broken answer", 502, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			pair := newInProcessExchange(t)
			g := pair.originator
			g.cfg.ConformanceEnforcement = row.level
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody([]byte(row.answer), relay.OriginPeerFrame), "application/fhir+json")})
			ex := ExchangeContext{holder: "provider", recipient: "payer", legType: "pas-claim", operation: "pas-submit", subjectPCI: "external-pci", correlationID: "corr", contentType: "application/fhir+json", bodySHA256: sha256hex([]byte(row.request)), policy: g.policy()}
			_, err := g.dispatchNative(context.Background(), pair.req, ex, relay.Exact(relay.NewBody([]byte(row.request), relay.OriginIngressRequest), ex.contentType))
			var ce *conformanceError
			if row.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &ce) || ce.status != row.status {
				t.Fatalf("err=%v want conformance %d", err, row.status)
			}
			if pair.routeHitCount() != row.hits {
				t.Fatalf("dispatches=%d want %d", pair.routeHitCount(), row.hits)
			}
		})
	}
}

func TestNativeRelayAbsentOnlyFallback(t *testing.T) {
	for _, row := range []struct {
		name, assertion           string
		status, resolutions, hits int
	}{
		{"absent", "", 200, 1, 1}, {"invalid presented", "not-a-jwt", 401, 0, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			pair := newTransportExchange(t)
			g := pair.originator
			key, pub := newTestClientKey(t)
			g.cfg.ingressAuthBypass = false
			g.ingressAuth = newTestAuthServer(t, "native-source", pub, "ES384")
			g.cfg.SoR = nativeReadPanicSoR{}
			calls := 0
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				calls++
				if ref.Holder != "provider" || ref.System != "fhir-relative" || ref.Value != "Patient/known" {
					t.Fatalf("unexpected linkage %+v", ref)
				}
				return "pci:fixture-known", true, nil
			})
			body := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/known"},"identifier":[{"system":"urn:shn:correlation","value":"source-correlation"}]}},{"resource":{"resourceType":"Coverage","payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}]}`)
			r := httptest.NewRequest("POST", "/Claim/$submit", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/fhir+json")
			now := g.ingressAuth.now()
			r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims("native-source", testIngressBaseURL+r.URL.Path, now)))
			if row.assertion != "" {
				r.Header.Set(exchangecontext.Header, row.assertion)
			}
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody([]byte("raw reply"), relay.OriginPeerFrame), "application/fhir+json")})
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, r)
			if row.name == "absent" && w.Header().Get(CorrelationHeader) != "source-correlation" {
				t.Errorf("source correlation lost: %s", w.Header().Get(CorrelationHeader))
			}
			if w.Code != row.status || calls != row.resolutions || pair.routeHitCount() != row.hits {
				t.Fatalf("status=%d body=%s resolutions=%d dispatches=%d", w.Code, w.Body.String(), calls, pair.routeHitCount())
			}
		})
	}
}

func TestNativeRelayReceiverPolicy(t *testing.T) {
	for _, row := range []struct {
		name         string
		level        ConformanceEnforcement
		body, answer string
		status, hits int
	}{
		{"none opaque", EnforcementNone, "opaque", "opaque reply", 202, 1},
		{"observe opaque", EnforcementObserve, "opaque", "opaque reply", 202, 1},
		{"basic request", EnforcementBasic, "opaque", "opaque reply", 422, 0},
		{"basic response", EnforcementBasic, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, "opaque reply", 502, 1},
		{"strict unavailable linkage", EnforcementStrict, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/external"}}}]}`, "opaque reply", 503, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			g.cfg.ConformanceEnforcement = row.level
			g.cfg.SoR = nativeReadPanicSoR{}
			count := 0
			g.cfg.Responder = nativeCountResponder{count: &count, answer: []byte(row.answer)}
			ex := ExchangeContext{holder: requester.ID, recipient: "payer", legType: "pas-claim", operation: "pas-submit", subjectPCI: "pci:subject", correlationID: "policy", contractVersion: "pa.pas@2.0", policy: g.policy()}
			r := newSignedInboundRequest(t, g, requester.ID)
			r = r.WithContext(context.WithValue(r.Context(), nativeExchangeKey{}, ex))
			env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: requester.ID, Recipient: "payer", TransactionType: "pas-claim", CorrelationID: ex.correlationID}}
			w := httptest.NewRecorder()
			g.handlePASNativeInbound(w, r, env, shnsdk.Token{Subject: ex.subjectPCI}, []byte(row.body), ex.contractVersion)
			if w.Code != 200 {
				t.Fatalf("transport %d %s", w.Code, w.Body.String())
			}
			hdr, _, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, w.Body.Bytes()))
			if err != nil || hdr.Status != row.status || count != row.hits {
				t.Fatalf("status=%d hits=%d err=%v", hdr.Status, count, err)
			}
		})
	}
}

type nativeCountResponder struct {
	count  *int
	answer []byte
}

func (n nativeCountResponder) Handle(context.Context, string, string, string, []byte) (LegResult, error) {
	*n.count++
	return LegResult{ApplicationStatus: 202, Response: relay.Exact(relay.NewBody(n.answer, relay.OriginUpstreamResponse), "application/fhir+json")}, nil
}

func TestNativeRelayBodyLimit(t *testing.T) {
	for _, row := range []struct {
		name             string
		signed, overflow bool
	}{
		{"signed prefix plus extra byte", true, true}, {"unsigned overflow", false, true}, {"exact limit", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			pair := newTransportExchange(t)
			g := pair.originator
			g.cfg.SoR = nativeReadPanicSoR{}
			key, pub := newTestClientKey(t)
			g.cfg.ingressAuthBypass = false
			g.ingressAuth = newTestAuthServer(t, "native-source", pub, "ES384")
			reg := g.ingressAuth.clients["native-source"]
			reg.ContextOperations = []string{"pas-submit"}
			g.ingressAuth.clients["native-source"] = reg
			replay := &stubReplayStore{}
			g.ingressAuth.replay = replay
			resolutions := 0
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) {
				resolutions++
				return "pci:fixture-known", true, nil
			})
			start := []byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/known"}}},{"resource":{"resourceType":"Coverage","payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}]}`)
			prefix := append(start, bytes.Repeat([]byte(" "), shnsdk.MaxRequestBytes-len(start))...)
			received := append([]byte(nil), prefix...)
			if row.overflow {
				received = append(received, 'x')
			}
			r := httptest.NewRequest("POST", "/Claim/$submit", bytes.NewReader(received))
			r.Header.Set("Content-Type", "application/fhir+json")
			now := g.ingressAuth.now()
			r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims("native-source", testIngressBaseURL+r.URL.Path, now)))
			if row.signed {
				c := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "native-source", Subject: "native-source", Audience: jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}, ID: "size-context", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "pci:fixture-known", CorrelationID: "size-correlation", ContentType: r.Header.Get("Content-Type")}
				putContext(t, r, c, prefix, key)
			}
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody([]byte("raw reply"), relay.OriginPeerFrame), "application/fhir+json")})
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, r)
			if row.overflow {
				if w.Code != http.StatusRequestEntityTooLarge || resolutions != 0 || replay.calls != 0 || pair.routeHitCount() != 0 || w.Body.Len() > 1024 {
					t.Fatalf("status=%d bytes=%d resolver=%d replay=%d dispatch=%d", w.Code, w.Body.Len(), resolutions, replay.calls, pair.routeHitCount())
				}
				assertFHIRIngressError(t, w, http.StatusRequestEntityTooLarge)
			} else {
				if w.Code != 200 || resolutions != 0 || replay.calls != 1 || pair.routeHitCount() != 1 {
					t.Fatalf("exact limit status=%d resolver=%d replay=%d dispatch=%d body=%s", w.Code, resolutions, replay.calls, pair.routeHitCount(), w.Body.String())
				}
				_, sent, err := shnsdk.DecodeHTTPFrame(pair.lastRequestPayload())
				if err != nil || !bytes.Equal(sent, prefix) {
					t.Fatalf("exact limit bytes changed: %v", err)
				}
			}
		})
	}
}

func TestNativeRelayUnsignedAmendmentMetadata(t *testing.T) {
	prior := `{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/prior"},"identifier":[{"system":"urn:shn:correlation","value":"prior-correlation"}]}}`
	amendment := `{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/operative"},"identifier":[{"system":"urn:shn:correlation","value":"amend-correlation"}],"related":[{"claim":{"reference":"Claim/prior"}}]}}`
	coverage := `{"resource":{"resourceType":"Coverage","payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}`
	for _, row := range []struct{ name, entries string }{{"prior then operative", prior + "," + amendment}, {"operative then prior", amendment + "," + prior}} {
		t.Run(row.name, func(t *testing.T) {
			pair := newTransportExchange(t)
			g := pair.originator
			g.cfg.SoR = nativeReadPanicSoR{}
			resolved := ""
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
				resolved = ref.Value
				return "pci:source-established", true, nil
			})
			var routed shnsdk.Envelope
			g.cfg.Client = &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/route" {
					raw, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(raw))
					var err error
					routed, err = shnsdk.DecodeEnvelope(raw)
					if err != nil {
						t.Fatal(err)
					}
				}
				return pair.substrate.RoundTrip(r)
			})}
			body := []byte(`{"resourceType":"Bundle","entry":[` + row.entries + "," + coverage + `]}`)
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/Claim/$submit", bytes.NewReader(body)))
			var tok shnsdk.Token
			_ = json.Unmarshal([]byte(routed.Metadata.AuthzToken), &tok)
			_, sent, err := shnsdk.DecodeHTTPFrame(pair.lastRequestPayload())
			if w.Code != 200 || routed.Metadata.TransactionType != "pas-claim-update" || tok.Operation != "pas-update-submit" || routed.Metadata.CorrelationID != "amend-correlation" || w.Header().Get(CorrelationHeader) != "amend-correlation" || resolved != "Patient/operative" || err != nil || !bytes.Equal(sent, body) {
				t.Fatalf("status=%d leg=%s op=%s corr=%s resolved=%s body changed=%v err=%v", w.Code, routed.Metadata.TransactionType, tok.Operation, routed.Metadata.CorrelationID, resolved, !bytes.Equal(sent, body), err)
			}
		})
	}
}

func TestNativeRelayLocalBindingIsNotAdmission(t *testing.T) {
	for _, row := range []struct {
		name, body string
		level      ConformanceEnforcement
	}{
		{"none", "opaque request", EnforcementNone},
		{"observe", "opaque request", EnforcementObserve},
	} {
		t.Run(row.name, func(t *testing.T) {
			pair := newInProcessExchange(t)
			g := pair.originator
			g.cfg.ConformanceEnforcement = row.level
			g.cfg.SoR = nativeReadPanicSoR{}
			g.cfg.SubjectReferenceResolver = subjectResolverFunc(func(context.Context, PatientReference) (string, bool, error) { panic("signed path resolved a patient") })
			g.cfg.ingressAuthBypass = false
			key, pub := newTestClientKey(t)
			g.ingressAuth = newTestAuthServer(t, "native-source", pub, "ES384")
			reg := g.ingressAuth.clients["native-source"]
			reg.ContextOperations = []string{"pas-submit"}
			g.ingressAuth.clients["native-source"] = reg
			entry, _ := g.cfg.Reg.Lookup("payer")
			entry.RequestFrames = shnsdk.SupportedRequestFrames()
			g.cfg.Reg.Set("payer", entry)
			body := []byte(row.body)
			r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/custom+json; charset=utf-8")
			now := g.ingressAuth.now()
			c := exchangecontext.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "native-source", Subject: "native-source", Audience: jwt.ClaimStrings{testIngressBaseURL + r.URL.Path}, ID: "context-once", IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}, Holder: "provider", Recipient: "payer", Leg: "pas-claim", Operation: "pas-submit", SubjectPCI: "externally-established-pci", CorrelationID: "native-correlation", ContentType: r.Header.Get("Content-Type"), ContractVersion: "pa.pas@2.0"}
			putContext(t, r, c, body, key)
			r.Header.Set("Authorization", "Bearer "+signJWT(t, jwt.SigningMethodES384, key, directClaims(c.Issuer, testIngressBaseURL+r.URL.Path, now)))
			answer := bytes.ReplaceAll(homeOxygenApprovedClaimResponse(), []byte("Patient/MBR-OX"), []byte("Patient/foreign"))
			framed, err := shnsdk.EncodeHTTPFrame(201, "application/answer+json", answer)
			if err != nil {
				t.Fatal(err)
			}
			pair.payerReturns(LegResult{Response: relay.Exact(relay.NewBody(framed, relay.OriginPeerFrame), "application/json")})
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, r)
			if w.Code != 201 || w.Header().Get("Content-Type") != "application/answer+json" || !bytes.Equal(w.Body.Bytes(), answer) {
				t.Fatalf("reply %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			var requestToken shnsdk.Token
			if err := json.Unmarshal([]byte(pair.substrate.lastMetadata.AuthzToken), &requestToken); err != nil {
				t.Fatal(err)
			}
			if pair.substrate.lastMetadata.TransactionType != c.Leg || requestToken.Operation != c.Operation || w.Header().Get(CorrelationHeader) != c.CorrelationID {
				t.Fatal("signed context overridden by body metadata")
			}
			header, sent, err := shnsdk.DecodeHTTPFrame(pair.lastRequestPayload())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(sent, body) || header.Headers["Content-Type"] != r.Header.Get("Content-Type") || pair.routeHitCount() != 1 {
				t.Fatalf("request changed header=%+v bytes=%s hits=%d", header, sent, pair.routeHitCount())
			}
		})
	}
}
