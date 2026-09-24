package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func crdResponseInput(body, version string, level ConformanceEnforcement) CheckInput {
	return CheckInput{Body: []byte(body), Status: 200, Direction: "response", DeclaredVersion: version, Exchange: ExchangeContext{legType: "crd-order-select", contractVersion: version, policy: NewConformancePolicy(level)}}
}

func TestCRDEmptyFHIRTargetsApplicability(t *testing.T) {
	for _, version := range []string{"pa.crd@2.0", "pa.crd@2.1", "pa.crd@2.2"} {
		for _, body := range []string{`{"cards":[]}`, withCard(crdCard), `{"cards":[],"systemActions":[{"type":"delete","description":"remove draft","resourceId":"ServiceRequest/synthetic-draft"}]}`, withCard(cardWith(`"selectionBehavior":"any","suggestions":[{"label":"review","actions":[]}]`))} {
			if violations := shnsdk.CheckCDSHooksResponse([]byte(body), strings.TrimPrefix(version, "pa.crd@")); len(violations) != 0 {
				t.Fatalf("valid no-target control has violations: %+v", violations)
			}
			for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
				t.Run(fmt.Sprintf("%s/%s/%s", version, level, body), func(t *testing.T) {
					var calls atomic.Int32
					g := newObservationGateway(t, level, observationValidator(func(context.Context, []byte, string) (shnsdk.Result, error) {
						calls.Add(1)
						panic("no FHIR target")
					}))
					in := crdResponseInput(body, version, level)
					for _, id := range []string{"fhir.profile"} {
						if deepRule(t, g, id).Applies(in) {
							t.Fatalf("%s must be inapplicable", id)
						}
					}
					if err := g.enforceContent(context.Background(), in); err != nil {
						t.Fatal(err)
					}
					observationFlush(t, g)
					findings, drops := g.ConformanceObservationsForTest()
					if calls.Load() != 0 || drops != 0 {
						t.Fatalf("checker=%d drops=%d", calls.Load(), drops)
					}
					if level == EnforcementNone && (len(findings) != 0 || g.certification != nil) {
						t.Fatalf("none optional work=%+v", findings)
					}
					for _, f := range findings {
						if f.Rule == "fhir.profile" {
							t.Fatalf("no-target must not claim evidence: %+v", f)
						}
					}
					health := g.ConformanceStatus()
					want := "unavailable"
					if level == EnforcementNone {
						want = "disabled"
					}
					if health.Availability["profile"] != want {
						t.Fatalf("health=%+v", health)
					}
				})
			}
		}
	}
}

func TestCRDEmptyFHIRTargetsCannotHideChecks(t *testing.T) {
	for _, row := range []struct{ name, body, version, direction string }{
		{"absent", `{"cards":[]}`, "", "response"},
		{"unknown", `{"cards":[]}`, "pa.crd@9.9", "response"},
		{"wrong contract", `{"cards":[]}`, "pa.pas@2.0", "response"},
		{"request", `{"hook":"order-select","hookInstance":"i","context":{}}`, "pa.crd@2.0", "request"},
		{"malformed JSON", `{"cards":`, "pa.crd@2.0", "response"},
		{"duplicate", `{"cards":[],"cards":[]}`, "pa.crd@2.0", "response"},
		{"missing cards", `{}`, "pa.crd@2.0", "response"},
		{"cards type", `{"cards":{}}`, "pa.crd@2.0", "response"},
		{"card type", `{"cards":[1]}`, "pa.crd@2.0", "response"},
		{"suggestions type", `{"cards":[{"suggestions":{}}]}`, "pa.crd@2.0", "response"},
		{"suggestions null", `{"cards":[{"suggestions":null}]}`, "pa.crd@2.0", "response"},
		{"suggestion type", `{"cards":[{"suggestions":[1]}]}`, "pa.crd@2.0", "response"},
		{"actions type", `{"cards":[{"suggestions":[{"actions":{}}]}]}`, "pa.crd@2.0", "response"},
		{"action type", `{"cards":[{"suggestions":[{"actions":[1]}]}]}`, "pa.crd@2.0", "response"},
		{"system actions type", `{"cards":[],"systemActions":{}}`, "pa.crd@2.0", "response"},
		{"system actions null", `{"cards":[],"systemActions":null}`, "pa.crd@2.0", "response"},
		{"system action type", `{"cards":[],"systemActions":[1]}`, "pa.crd@2.0", "response"},
		{"null resource", `{"cards":[],"systemActions":[{"resource":null}]}`, "pa.crd@2.0", "response"},
		{"malformed resource", `{"cards":[],"systemActions":[{"resource":1}]}`, "pa.crd@2.0", "response"},
		{"present resource", `{"cards":[],"systemActions":[{"resource":{"resourceType":"ServiceRequest"}}]}`, "pa.crd@2.0", "response"},
		{"suggested resource", `{"cards":[{"suggestions":[{"actions":[{"resource":{"resourceType":"ServiceRequest"}}]}]}]}`, "pa.crd@2.0", "response"},
	} {
		t.Run(row.name, func(t *testing.T) {
			in := crdResponseInput(row.body, row.version, EnforcementStrict)
			in.Direction = row.direction
			for _, id := range []string{"fhir.profile"} {
				if !deepRule(t, &Gateway{}, id).Applies(in) {
					t.Fatalf("%s incorrectly waived", id)
				}
			}
		})
	}
}

func TestCRDEmptyFHIRTargetsRetainCDSRefusal(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, mutated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing-summary=%v", level, mutated), func(t *testing.T) {
				body := withCard(crdCard)
				if mutated {
					body = strings.Replace(body, `"summary":"Prior authorization required",`, "", 1)
				}
				g := newObservationGateway(t, level, nil)
				in := crdResponseInput(body, "pa.crd@2.0", level)
				err := g.enforceContent(context.Background(), in)
				if mutated && level == EnforcementStrict {
					wantStructuralError(t, err, 502, "cds.response")
				} else if err != nil {
					t.Fatal(err)
				}
				observationFlush(t, g)
				findings, drops := g.ConformanceObservationsForTest()
				if drops != 0 {
					t.Fatal(drops)
				}
				if level == EnforcementObserve || level == EnforcementBasic {
					found := false
					for _, f := range findings {
						if f.Rule == "cds.response" {
							found = true
							want := CheckValid
							if mutated {
								want = CheckInvalid
							}
							if f.State != want || f.Action != "not_enforced" || f.PayloadSHA256 != sha256hex(in.Body) {
								t.Fatalf("finding=%+v", f)
							}
						}
					}
					if !found {
						t.Fatal("missing CDS finding")
					}
				}
			})
		}
	}
}

// This handler-level proof invokes the registered policy around the real HTTP
// responder and seals its answer. Mounted signed-pair proofs are separate.
func TestCRDEmptyFHIRTargetsNativePolicy(t *testing.T) {
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, mutated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing-summary=%v", level, mutated), func(t *testing.T) {
				answer := withCard(crdCard)
				if mutated {
					answer = strings.Replace(answer, `"summary":"Prior authorization required",`, "", 1)
				}
				p := newCDSPayer(t, referencePayerServices...)
				p.respond(202, "application/json; charset=utf-8", []byte(answer))
				g, requester := newInboundTestGatewayWithPolicy(t, true, level)
				g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.crd@2.0"}), WithNativeResponseDeclarations(declaredCDSFixtureOutput(t, p.srv.URL)))
				subject := coveredPCI(t, g)
				ex := ExchangeContext{holder: requester.ID, recipient: "payer", legType: "crd-order-select", operation: "crd-order-select", subjectPCI: subject, correlationID: "crd-empty", contractVersion: "pa.crd@2.0", policy: g.policy()}
				req := newSignedInboundRequest(t, g, requester.ID)
				req = req.WithContext(context.WithValue(req.Context(), nativeExchangeKey{}, ex))
				rec := httptest.NewRecorder()
				env := shnsdk.Envelope{Metadata: shnsdk.Metadata{Sender: requester.ID, CorrelationID: ex.correlationID}}
				g.handleNativeInbound(rec, req, ex.legType, env, shnsdk.Token{Subject: subject}, conformantCRD("MBR-COVERED", "72148"), "")
				if rec.Code != 200 {
					t.Fatalf("sealed response=%d %s", rec.Code, rec.Body.String())
				}
				hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
				if err != nil || p.sentAny() != 1 {
					t.Fatalf("decode=%v backend=%d body=%s", err, p.sentAny(), body)
				}
				if level == EnforcementStrict && mutated {
					if hdr.Status != 502 {
						t.Fatalf("status=%d body=%s", hdr.Status, body)
					}
					var got map[string]any
					if json.Unmarshal(body, &got) != nil || len(got) != 5 || got["category"] != "conformance_invalid" || got["rule"] != "cds.response" || got["gateway"] != "payer" || got["direction"] != "response" || got["level"] != "strict" {
						t.Fatalf("safe refusal=%s", body)
					}
				} else if hdr.Status != 202 || hdr.Headers["Content-Type"] != "application/json; charset=utf-8" || string(body) != answer {
					t.Fatalf("delivery frame=%+v body=%s", hdr, body)
				}
				observationFlush(t, g)
				findings, drops := g.ConformanceObservationsForTest()
				if drops != 0 {
					t.Fatal(drops)
				}
				if level == EnforcementNone && len(findings) != 0 {
					t.Fatalf("none findings=%+v", findings)
				}
				for _, f := range findings {
					if f.Direction == "response" && (f.Rule == "fhir.profile") {
						t.Fatalf("inapplicable response evidence=%+v", f)
					}
				}
			})
		}
	}
}

func TestCRDEnvelopeBeforeFHIRChecks(t *testing.T) {
	for _, version := range []string{"pa.crd@2.0", "pa.crd@2.1", "pa.crd@2.2", "pa.crd@9.9", ""} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
			for _, malformed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/malformed=%v", version, level, malformed), func(t *testing.T) {
					body := withAction(`{"type":"update","description":"draft order","resource":{"resourceType":"ServiceRequest","id":"owned-order"}}`)
					if malformed {
						body = `{"cards":[],"systemActions":{}}`
					}
					var calls atomic.Int32
					g := newObservationGateway(t, level, observationValidator(func(context.Context, []byte, string) (shnsdk.Result, error) {
						calls.Add(1)
						return shnsdk.Result{}, errors.New("validator unavailable")
					}))
					in := crdResponseInput(body, version, level)
					err := g.enforceContent(context.Background(), in)
					known := version != "" && version != "pa.crd@9.9"
					if level == EnforcementStrict {
						switch {
						case !known:
							wantStructuralError(t, err, 503, "cds.response")
						case malformed:
							wantStructuralError(t, err, 502, "cds.response")
						default:
							wantStructuralError(t, err, 503, "fhir.profile")
						}
					} else if err != nil {
						t.Fatal(err)
					}
					observationFlush(t, g)
					findings, drops := g.ConformanceObservationsForTest()
					if drops != 0 {
						t.Fatal(drops)
					}
					if level == EnforcementNone {
						if calls.Load() != 0 || len(findings) != 0 || g.certification != nil {
							t.Fatalf("none work: checker=%d findings=%+v", calls.Load(), findings)
						}
					} else if level == EnforcementStrict && (!known || malformed) {
						if calls.Load() != 0 {
							t.Fatalf("prior CDS refusal invoked checker %d times", calls.Load())
						}
					} else if known && !malformed && calls.Load() != 1 {
						t.Fatalf("embedded resource checker calls=%d, want1", calls.Load())
					}
					if level == EnforcementObserve || level == EnforcementBasic {
						found := false
						for _, f := range findings {
							if f.Rule != "cds.response" {
								continue
							}
							found = true
							want := CheckValid
							if !known {
								want = CheckUnavailable
							} else if malformed {
								want = CheckInvalid
							}
							if f.State != want || f.Action != "not_enforced" || f.PayloadSHA256 != sha256hex(in.Body) {
								t.Fatalf("finding=%+v", f)
							}
						}
						if !found {
							t.Fatal("missing CDS observation")
						}
					}
				})
			}
		}
	}
}
