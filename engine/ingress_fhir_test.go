package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

type unreadableIngressBody struct{}

func (unreadableIngressBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (unreadableIngressBody) Close() error             { return nil }

func TestFHIRIngressLocalErrors(t *testing.T) {
	for _, protocol := range []string{"pas", "dtr"} {
		for _, tc := range []struct {
			name   string
			bypass bool
			body   io.ReadCloser
			status int
		}{
			{"authentication", false, io.NopCloser(strings.NewReader("{}")), 401},
			{"body read", true, unreadableIngressBody{}, 400},
			{"malformed", true, io.NopCloser(strings.NewReader("{")), 400},
			{"missing fields", true, io.NopCloser(strings.NewReader(`{"resourceType":"Bundle","entry":[]}`)), 400},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				g := &Gateway{cfg: Config{ingressAuthBypass: tc.bypass}}
				r := httptest.NewRequest(http.MethodPost, "/", tc.body)
				w := httptest.NewRecorder()
				if protocol == "pas" {
					g.handlePASIngress(w, r)
				} else {
					g.handleDTRIngress(w, r)
				}
				assertFHIRIngressError(t, w, tc.status)
			})
		}
	}
}

func assertFHIRIngressError(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/fhir+json" {
		t.Errorf("Content-Type=%q", got)
	}
	var oo struct {
		ResourceType string                                         `json:"resourceType"`
		Issue        []struct{ Severity, Code, Diagnostics string } `json:"issue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &oo); err != nil {
		t.Fatal(err)
	}
	if oo.ResourceType != "OperationOutcome" || len(oo.Issue) != 1 || oo.Issue[0].Severity != "error" || oo.Issue[0].Code == "" || oo.Issue[0].Diagnostics == "" {
		t.Errorf("invalid OperationOutcome: %s", w.Body.String())
	}
	// FHIR R4 IssueType codes (https://hl7.org/fhir/R4/codesystem-issue-type.html),
	// pinned independently of the response encoder.
	wantCode := map[int]string{400: "invalid", 401: "login", 403: "forbidden", 413: "processing", 422: "not-supported", 500: "processing", 502: "processing", 503: "transient", 504: "timeout"}[status]
	if len(oo.Issue) == 1 && oo.Issue[0].Code != wantCode {
		t.Errorf("issue code=%q want=%q", oo.Issue[0].Code, wantCode)
	}
}

func TestFHIRIngressKeyStoreOutage(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	kid, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	bearer := mintAssertionKID(t, priv, kid, jwt.MapClaims{"client_id": "br-provider", "aud": testIngressBaseURL, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	for _, path := range []string{"/Claim/$submit", "/Questionnaire/$questionnaire-package"} {
		t.Run(path, func(t *testing.T) {
			eph, err := newEphemeralKeyStore()
			if err != nil {
				t.Fatal(err)
			}
			down := &unavailableKeyStore{IngressKeyStore: eph}
			g := gatewayWithAuthStores(t, "br-provider", pub, down, nil)
			req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+bearer)
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, req)
			assertFHIRIngressError(t, w, 503)
			if w.Header().Get("Cache-Control") != "no-store" || down.calls != 1 || len(g.ExchangeSnapshot()) != 0 {
				t.Fatal("outage must consult store once, refuse before exchange, and prevent caching")
			}
		})
	}
}

// Encoder coverage, not handler branch coverage. DTR's defensive marshal failure
// is unreachable with parsed RawMessages, and its envelope adaptation cannot
// fail: egressAdapt passes dtr-questionnaire-fetch bytes through unchanged.
// Keep their defensive encodings covered without inventing a runtime fault seam.
// Reachable failures are exercised through the handlers in the other tests.
func TestFHIRIngressFailureEncoding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		write  func(http.ResponseWriter)
	}{
		{"store unavailable", 503, func(w http.ResponseWriter) { writeStoreUnavailable(w, "ingress key store unavailable") }},
		{"route refusal", 422, func(w http.ResponseWriter) {
			if !(&Gateway{}).relayOriginationError(w, &RouteRefusalError{Contract: "pa.dtr", LegType: "dtr-questionnaire-fetch", Recipient: "payer"}) {
				t.Fatal("route refusal not handled")
			}
		}},
		{"adaptation or unframed relay failure", 502, func(w http.ResponseWriter) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "exchange failed"})
		}},
		{"build failure", 500, func(w http.ResponseWriter) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr fetch failed"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.write(&fhirOperationWriter{w})
			assertFHIRIngressError(t, w, tc.status)
			if tc.status == 503 && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("store failure must not be cached")
			}
		})
	}
}

func TestFHIRIngressPreservesFramedUpstreamFailure(t *testing.T) {
	for _, contentType := range []string{"application/fhir+json", "application/json", ""} {
		t.Run(contentType, func(t *testing.T) {
			const body = "{\"resourceType\":\"OperationOutcome\",\"issue\":[{\"severity\":\"error\",\"code\":\"business-rule\",\"diagnostics\":\"payer refusal\"}]}\n"
			w := httptest.NewRecorder()
			if !(&Gateway{}).relayOriginationError(&fhirOperationWriter{w}, &RelayError{Status: 409, Body: []byte(body), ContentType: contentType, leg: "pas-claim"}) {
				t.Fatal("framed failure not handled")
			}
			if w.Code != 409 || w.Body.String() != body || w.Header().Get("Content-Type") != contentType {
				t.Fatalf("upstream answer changed: %d %s %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestNonFHIRIngressKeepsJSONErrors(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		handler      func(http.ResponseWriter, *http.Request)
	}{
		{http.MethodPost, "/cds-services/order-select", (&Gateway{}).handleCRDIngress},
		{http.MethodGet, "/cds-services", (&Gateway{}).handleCDSDiscovery},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			// The shared dispatcher selects the protocol's local-error format from
			// the mounted route; "/" is not a CDS Hooks invocation path.
			tc.handler(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
			if w.Code != 401 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"error":`) {
				t.Fatalf("non-FHIR envelope changed: %d %s %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestFHIRIngressExchangeFailures(t *testing.T) {
	const coverage = `{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`
	for _, protocol := range []string{"pas", "dtr"} {
		body := `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Patient","id":"MBR-COVERED"}},{"resource":{"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}}},{"resource":{"resourceType":"ServiceRequest","subject":{"reference":"Patient/MBR-COVERED"}}},{"resource":` + coverage + `}]}`
		if protocol == "dtr" {
			body = `{"resourceType":"Parameters","parameter":[{"name":"questionnaire","valueCanonical":"urn:test:questionnaire"},{"name":"coverage","resource":` + coverage + `}]}`
		}
		failures := []string{"framed upstream", "unframed transport", "unsupported contract", "payer absent", "context absent"}

		for _, failure := range failures {
			t.Run(protocol+"/"+failure, func(t *testing.T) {
				env := newTransportExchange(t)
				if protocol == "dtr" {
					declareFramedDTR(t, env, true)
				}
				requestBody := body
				const upstream = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule","diagnostics":"payer refusal"}]}`
				env.payerReturns(LegResult{Status: 409, Response: testResponse([]byte(upstream))})
				switch failure {
				case "unframed transport":
					env.substrate.mutateResp = func([]byte) []byte { return []byte(`{`) }
				case "unsupported contract":
					declareRecipientVersions(t, env, []string{"pa.crd@2.0"})
				case "payer absent":
					requestBody = strings.ReplaceAll(body, `,"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]`, "")
				case "context absent":
					requestBody = `{"resourceType":"Bundle","entry":[]}`
					if protocol == "dtr" {
						requestBody = `{"resourceType":"Parameters","parameter":[]}`
					}

				}
				w := httptest.NewRecorder()
				path, leg, operation, version := "/Claim/$submit", "pas-claim", "pas-submit", "pa.pas@2.0"
				if protocol == "dtr" {
					path, leg, operation, version = "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", "questionnaire-package", "pa.dtr@2.0"
				}
				r := signedFixtureIngress(t, env.originator, path, leg, operation, "", "pci-covered", version, "fhir-failure", []byte(requestBody))
				if failure == "payer absent" || failure == "context absent" {
					r.Header.Del(exchangecontext.Header)
				}
				if protocol == "pas" {
					env.originator.handlePASIngress(w, r)
				} else {
					env.originator.handleDTRIngress(w, r)
				}
				switch failure {
				case "framed upstream":
					if w.Code != 409 || w.Body.String() != upstream || w.Header().Get("Content-Type") != "application/fhir+json" {
						t.Fatalf("upstream reply changed: %d %s %s", w.Code, w.Header(), w.Body.String())
					}
				case "unframed transport":
					assertFHIRIngressError(t, w, 502)
				case "unsupported contract":
					assertFHIRIngressError(t, w, 422)
				case "payer absent":
					assertFHIRIngressError(t, w, 422)
					if !strings.Contains(w.Body.String(), "no payer identifier") {
						t.Fatalf("missing payer was not a routing refusal: %s", w.Body.String())
					}
				case "context absent":
					assertFHIRIngressError(t, w, 400)
					if !strings.Contains(w.Body.String(), "context_missing") {
						t.Fatalf("missing context not identified: %s", w.Body.String())
					}
				}
				wantHits := 1
				if failure == "unsupported contract" || failure == "payer absent" || failure == "context absent" {
					wantHits = 0
				}
				if env.routeHitCount() != wantHits {
					t.Fatalf("Hub calls=%d want%d", env.routeHitCount(), wantHits)
				}
			})
		}
	}
}

// TestFHIRIngressHubLegTimeoutIsTimeoutOutcome: a $submit whose Hub leg produces
// no answer within the gateway's leg deadline answers 504 as an OperationOutcome
// with issue code "timeout" and the reason text carrying the budget. The Hub is
// held open until the deadline; nothing else in the exchange is changed.
func TestFHIRIngressHubLegTimeoutIsTimeoutOutcome(t *testing.T) {
	env := newTransportExchange(t)
	var routeCalls atomic.Int32
	base := env.originator.cfg.Client.Transport
	held := diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/route") {
			routeCalls.Add(1)
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base.RoundTrip(r)
	})
	env.originator.cfg.Client = &http.Client{Transport: held, Timeout: 50 * time.Millisecond}

	w := httptest.NewRecorder()
	r := signedFixtureIngress(t, env.originator, "/Claim/$submit", "pas-claim", "pas-submit", "", "pci-covered", "pa.pas@2.0", "timeout-proof", []byte(pasIngressBundle("00001", "")))
	env.originator.handlePASIngress(w, r)

	assertFHIRIngressError(t, w, http.StatusGatewayTimeout)
	if routeCalls.Load() != 1 {
		t.Fatalf("Hub calls=%d", routeCalls.Load())
	}
	const want = `"diagnostics":"no answer on the hub leg within 50ms (hub leg timeout)"`
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("body=%s want to contain %s", w.Body.String(), want)
	}
}

// The supplied QR is checked only under the participant's chosen content policy.
// Source-disclosure and local signing requirements are separate mandatory guards.
func TestFHIRIngressSuppliedQRAttestationPolicy(t *testing.T) {
	item, err := shnsdk.BuildManualAttestedItem("functional-status-oswestry", "42", shnsdk.Attestation{NPI: "1999999999", Text: "I attest these are my clinical findings.", When: "2026-06-04"})
	if err != nil {
		t.Fatal(err)
	}
	const upstream = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule","diagnostics":"payer refusal"}]}`
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, mutated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/attestation-removed=%v", level, mutated), func(t *testing.T) {
				env := newTransportExchangeWithPolicy(t, level)
				var checkerCalls atomic.Int32
				env.originator.cfg.Validator = observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
					checkerCalls.Add(1)
					return *syntheticEvidence(), nil
				})
				subject, _, ok := env.originator.cfg.SoR.ResolvePatient("MBR-COVERED")
				if !ok {
					t.Fatal("seeded subject missing")
				}
				qrItem := item
				if mutated {
					qrItem = stripItemExtension(t, item)
				}
				qr := `{"resource":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/MBR-COVERED"},"item":[` + string(qrItem) + `]}},`
				body := strings.Replace(pasIngressBundle("00001", ""), `"entry":[`, `"entry":[`+qr, 1)
				env.payerReturns(LegResult{Status: 409, Response: testResponse([]byte(upstream))})
				r := signedFixtureIngress(t, env.originator, "/Claim/$submit", "pas-claim", "pas-submit", "", subject, "pa.pas@2.0", "qr-policy", []byte(body))
				w := httptest.NewRecorder()
				env.originator.handlePASIngress(w, r)
				if mutated && level == EnforcementStrict {
					if w.Code != 422 || env.routeHitCount() != 0 || !strings.Contains(w.Body.String(), `"valueString":"qr.attestation"`) || !strings.Contains(w.Body.String(), `"valueString":"conformance_invalid"`) {
						t.Fatalf("refusal status=%d Hub=%d body=%s", w.Code, env.routeHitCount(), w.Body.String())
					}
				} else if w.Code != 409 || w.Body.String() != upstream || w.Header().Get("Content-Type") != "application/fhir+json" || env.routeHitCount() != 1 {
					t.Fatalf("delivery status=%d Hub=%d body=%s", w.Code, env.routeHitCount(), w.Body.String())
				}
				observationFlush(t, env.originator)
				findings, drops := env.originator.ConformanceObservationsForTest()
				if drops != 0 {
					t.Fatal(drops)
				}
				if level == EnforcementNone && (len(findings) != 0 || checkerCalls.Load() != 0 || env.originator.certification != nil) {
					t.Fatalf("none checker=%d findings=%+v", checkerCalls.Load(), findings)
				}
				if level == EnforcementObserve || level == EnforcementBasic {
					found := false
					for _, f := range findings {
						if f.Rule == "qr.attestation" && f.Direction == "request" {
							found = true
							want := CheckValid
							if mutated {
								want = CheckInvalid
							}
							if f.State != want || f.PayloadSHA256 != sha256hex([]byte(body)) {
								t.Fatalf("finding=%+v", f)
							}
						}
					}
					if !found {
						t.Fatal("missing attestation finding")
					}
				}
			})
		}
	}
}
