package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Native DTR ingress carries the producer's Parameters without enrichment.
// Explicit participant construction is exercised through the SDK builder.

// packageAnswer is the payer's package, in its own layout.
var packageAnswer = []byte("{ \"resourceType\" : \"Parameters\", \"parameter\" : [ { \"name\" : \"PackageBundle\", \"resource\" : { \"resourceType\" : \"Bundle\", \"type\" : \"collection\", \"entry\" : [ { \"resource\" : { \"resourceType\" : \"Questionnaire\", \"url\" : \"http://x/q\", \"text\" : { \"div\" : \"a " + lt + " b\" } } } ] } } ] }")

// dtrFixture reads a vendored DTR input fixture, naming the patient
// prefetchMember and the payer the test router knows.
func dtrFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("Patient/examplepatient"), []byte("Patient/"+prefetchMember))
	return bytes.ReplaceAll(b, []byte(`"system":"urn:oid:2.16.840.1.113883.4.7","value":"10D0202020"`),
		[]byte(`"system":"`+shnsdk.CMSPayerIdentity.System+`","value":"`+shnsdk.CMSPayerIdentity.Value+`"`))
}

// withParameters inserts parameter elements (JSON text) after the first
// element of body's parameter array.
func withParameters(t *testing.T, body []byte, elements ...string) []byte {
	t.Helper()
	doc, err := relay.Doc(relay.NewBody(body, relay.OriginIngressRequest))
	if err != nil {
		t.Fatal(err)
	}
	arr, _ := doc.Member(doc.Root(), "parameter")
	_, end := doc.Span(doc.Elems(arr)[0])
	var b []byte
	b = append(b, body[:end]...)
	for _, e := range elements {
		b = append(b, ",\n    "...)
		b = append(b, e...)
	}
	return append(b, body[end:]...)
}

// ehrCoverageParam is an EHR coverage parameter for patient, naming payer value.
func ehrCoverageParam(patient, payerValue string) string {
	return `{"name":"coverage","resource":{"resourceType":"Coverage","id":"cov-` + payerValue + `","status":"active","beneficiary":{"reference":"Patient/` + patient + `"},"payor":[{"identifier":{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + payerValue + `"}}]}}`
}

// ehrOrderParam is an order parameter about patient.
func ehrOrderParam(id, patient string) string {
	return `{"name":"order","resource":{"resourceType":"ServiceRequest","id":"` + id + `","status":"active","intent":"order","subject":{"reference":"Patient/` + patient + `"},"quantityQuantity":{"value":2.50}}}`
}

// ehrParams is a Parameters in the EHR's layout holding elements.
func ehrParams(elements ...string) []byte {
	return []byte("{\n  \"resourceType\" : \"Parameters\",\n  \"parameter\" : [\n    " + strings.Join(elements, ",\n    ") + "\n  ]\n}\n")
}

// jsonEscapeE is the JSON escape of U+00E9, built at run time so no editor
// turns it into the character.
var jsonEscapeE = string(rune(92)) + "u00e9"

const dtrQuestionnaire = `{"name":"questionnaire","valueCanonical":"http://x/q|2.1.0"}`

// dtrOperationFrames declares framed DTR operations only (a manifest payer
// declares them; other transactions stay unframed to such a test peer).
var dtrOperationFrames = []string{shnsdk.RequestFrameV1Op}

// declareFramedDTR makes the in-process payer declare framed DTR operations
// (or, with capable false, only the base request frame).
func declareFramedDTR(t *testing.T, env *inProcessExchange, capable bool) {
	t.Helper()
	entry, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatal("no payer in the registry")
	}
	entry.RequestFrames = []string{shnsdk.RequestFrameV1}
	if capable {
		entry.RequestFrames = append(entry.RequestFrames, shnsdk.RequestFrameV1Op)
	}
	env.originator.cfg.Reg.Set(env.payerID, entry)
}

// postDTRIngress remains the raw-ingress fixture used by legacy patient
// assembly unit rows. Signed native tests below use signedFixtureIngress.
func postDTRIngress(env *inProcessExchange, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	env.originator.handleDTRIngress(rec, req)
	return rec
}

// sentOperation returns the request frame's operation header and the body
// the payer's side received.
func sentOperation(t *testing.T, env *inProcessExchange) (string, []byte) {
	t.Helper()
	b := env.lastRequestPayload()
	if !shnsdk.IsFramed(b) {
		t.Fatalf("the questionnaire request was not framed: %.80q", b)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(b)
	if err != nil {
		t.Fatalf("decode request frame: %v", err)
	}
	return hdr.Headers[shnsdk.FrameHeaderOperation], body
}

func TestDTRIngress_ParametersRelayedExactly(t *testing.T) {
	reference := dtrFixture(t, "dtr-package-params-2.0.json")
	for _, row := range []struct {
		name string
		body []byte
	}{
		{"the reference provider's input: repeated orders and canonicals, a |version, context, meta, markup and decimals", reference},
		{"the 2.2 example: id, meta, language, two orders, a |version and context", dtrFixture(t, "dtr-package-params-2.2.json")},
		{"referenced records and changedsince", withParameters(t, reference,
			`{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/example"},"valueQuantity":{"value":98.60}}}]}}`,
			`{"name":"changedsince","valueDateTime":"2026-01-01T00:00:00.000+00:00"}`)},
		{"an unknown parameter and an unknown member", withParameters(t, reference,
			`{"name":"x-unknown","valueString":"kept `+jsonEscapeE+`","extension":[{"url":"http://example.org/x","valueInteger":1}]}`)},
		{"a canonical beside the order", ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)},
		{"an order alone", ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"))},
		{"context and a coverage only", ehrParams(ehrCoverageParam(prefetchMember, "00001"), `{"name":"context","valueString":"ctx-1"}`)},
		{"the same parameter repeated", ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire, dtrQuestionnaire, `{"name":"context","valueString":"a"}`, `{"name":"context","valueString":"a"}`)},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				env := newTransportExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.SoR = s.sor()
				env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
				req := signedFixtureIngress(t, env.originator, "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, "", "pci-example", "", "dtr-exact", row.body)
				rec := httptest.NewRecorder()
				env.originator.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				op, sent := sentOperation(t, env)
				if op != shnsdk.FrameOperationQuestionnairePackage {
					t.Fatalf("operation header %q", op)
				}
				if !bytes.Equal(sent, row.body) {
					t.Fatalf("the payer's side received other bytes:\n got %s\nwant %s", sent, row.body)
				}
				if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 || env.routeHitCount() != 1 {
					t.Fatalf("native request consulted source or changed dispatch count: searched=%v read=%v hits=%d", searched, read, env.routeHitCount())
				}
				if !bytes.Equal(rec.Body.Bytes(), packageAnswer) {
					t.Fatalf("the EHR received %s", rec.Body.Bytes())
				}
				if leg, outcome := lastOutcome(t, env.originator); leg != "dtr-questionnaire-fetch" || outcome != "ok" {
					t.Fatalf("recorded %s %s", leg, outcome)
				}
			})
		}
	}
}

func TestDTRIngress_SuppliedSubjectPolicy(t *testing.T) {
	const answer = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule","diagnostics":"payer inquiry refusal"}]}`
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		for _, mutation := range []string{"", "second coverage", "second order"} {
			mutated := mutation != ""
			t.Run(fmt.Sprintf("%s/%s", level, mutation), func(t *testing.T) {
				env := newTransportExchangeWithPolicy(t, level)
				var calls atomic.Int32
				env.originator.cfg.Validator = observationValidator(func(context.Context, []byte, string) (shnsdk.ValidationEvidence, error) {
					calls.Add(1)
					return *syntheticEvidence(), nil
				})
				sor := newPrefetchSoR()
				subject, _, ok, err := sor.ResolvePatientContext(context.Background(), prefetchMember)
				if err != nil || !ok {
					t.Fatal("seeded subject missing")
				}
				env.originator.cfg.SubjectReferenceResolver = subjectResolverFunc(func(ctx context.Context, ref PatientReference) (string, bool, error) {
					if ref.Holder != "provider" || ref.System != "fhir-relative" {
						return "", false, nil
					}
					switch ref.Value {
					case "Patient/" + prefetchMember:
						return subject, true, nil
					case "Patient/other":
						return "pci-other", true, nil
					}
					return "", false, nil
				})
				valid := []string{ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire}
				switch mutation {
				case "second coverage":
					valid = append(valid, strings.ReplaceAll(ehrCoverageParam(prefetchMember, "00001"), "Patient/"+prefetchMember, "Patient/other"))
				case "second order":
					valid = append(valid, strings.ReplaceAll(ehrOrderParam("sr2", prefetchMember), "Patient/"+prefetchMember, "Patient/other"))
				}
				body := ehrParams(valid...)
				env.payerReturns(LegResult{Status: 409, Response: testResponse([]byte(answer))})
				req := signedFixtureIngress(t, env.originator, "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, "", subject, "pa.dtr@2.0", "dtr-subject-policy", body)
				rec := httptest.NewRecorder()
				env.originator.handleDTRIngress(rec, req)
				if mutated && level == EnforcementStrict {
					if rec.Code != 422 || env.routeHitCount() != 0 || !strings.Contains(rec.Body.String(), `"valueString":"patient.consistency"`) || !strings.Contains(rec.Body.String(), `"valueString":"conformance_invalid"`) {
						t.Fatalf("refusal status=%d Hub=%d body=%s", rec.Code, env.routeHitCount(), rec.Body)
					}
				} else {
					if rec.Code != 409 || rec.Body.String() != answer || rec.Header().Get("Content-Type") != "application/fhir+json" || env.routeHitCount() != 1 {
						t.Fatalf("delivery status=%d Hub=%d body=%s", rec.Code, env.routeHitCount(), rec.Body)
					}
					hdr, sent, err := shnsdk.DecodeHTTPFrame(env.lastRequestPayload())
					if err != nil || !bytes.Equal(sent, body) || hdr.Headers["Content-Type"] != "application/fhir+json" {
						t.Fatalf("request changed: %s header=%+v err=%v", sent, hdr, err)
					}
				}
				observationFlush(t, env.originator)
				findings, drops := env.originator.ConformanceObservationsForTest()
				if drops != 0 {
					t.Fatal(drops)
				}
				if level == EnforcementNone && (calls.Load() != 0 || len(findings) != 0 || env.originator.certification != nil) {
					t.Fatalf("none calls=%d findings=%+v", calls.Load(), findings)
				}
				if level == EnforcementObserve || level == EnforcementBasic {
					found := false
					for _, f := range findings {
						if f.Rule != "patient.consistency" || f.Direction != "request" {
							continue
						}
						found = true
						want := CheckValid
						if mutated {
							want = CheckInvalid
						}
						if f.State != want || f.Action != "not_enforced" || f.PayloadSHA256 != sha256hex(body) {
							t.Fatalf("finding=%+v", f)
						}
					}
					if !found {
						t.Fatal("missing patient consistency finding")
					}
				}
			})
		}
	}
}

// Each nested DTR form that the old raw-ingress guard traversed has its own
// signed control. Patient consistency binds subject references; incidental
// Patient records and an order with no subject are not a proven mismatch under
// this rule. Profile validity remains the separate FHIR checker obligation.
func TestDTRIngress_NestedSuppliedSubjectVariants(t *testing.T) {
	for _, row := range []struct{ name, control, mutation, rule string }{
		{"referenced Bundle", `{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/example"}}}]}}`, `{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}}]}}`, "patient.consistency"},
		{"referenced Patient without binding reference", `{"name":"referenced","resource":{"resourceType":"Patient","id":"example"}}`, `{"name":"referenced","resource":{"resourceType":"Patient","id":"other"}}`, ""},
		{"nested part", `{"name":"x","part":[{"name":"y","resource":{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/example"}}}]}`, `{"name":"x","part":[{"name":"y","resource":{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/other"}}}]}`, "patient.consistency"},
		{"contained Patient without binding reference", `{"name":"referenced","resource":{"resourceType":"Condition","id":"c2","subject":{"reference":"Patient/example"},"contained":[{"resourceType":"Patient","id":"p","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"example"}]}]}}`, `{"name":"referenced","resource":{"resourceType":"Condition","id":"c2","subject":{"reference":"Patient/example"},"contained":[{"resourceType":"Patient","id":"p","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"other"}]}]}}`, ""},
		{"order without subject is not a mismatched binding", ehrOrderParam("sr3", prefetchMember), `{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr3","status":"active","intent":"order"}}`, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, specimen := range []struct{ name, extra, rule string }{{"control", row.control, ""}, {"mutation", row.mutation, row.rule}} {
				body := ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, specimen.extra)
				for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementStrict} {
					t.Run(specimen.name+"/"+level.String(), func(t *testing.T) {
						env := newTransportExchangeWithPolicy(t, level)
						env.originator.cfg.Validator = syntheticFakeValidator()
						env.originator.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
							if ref.Holder != "provider" || (ref.System != "fhir-relative" && ref.System != shnsdk.MemberSystem) {
								return "", false, nil
							}
							switch ref.Value {
							case "Patient/example", "example":
								return "pci-example", true, nil
							case "Patient/other", "other":
								return "pci-other", true, nil
							}
							return "", false, nil
						})
						env.payerReturns(LegResult{Status: 409, Response: testResponse([]byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"business-rule","diagnostics":"payer refusal"}]}`))})
						req := signedFixtureIngress(t, env.originator, "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, "", "pci-example", "pa.dtr@2.0", "nested-dtr-"+row.name+specimen.name+level.String(), body)
						rec := httptest.NewRecorder()
						env.originator.handleDTRIngress(rec, req)
						if level == EnforcementStrict && specimen.rule != "" {
							if rec.Code != http.StatusUnprocessableEntity || env.routeHitCount() != 0 || !strings.Contains(rec.Body.String(), `"valueString":"`+specimen.rule+`"`) || !strings.Contains(rec.Body.String(), `"valueString":"conformance_invalid"`) {
								t.Fatalf("strict status=%d Hub=%d body=%s", rec.Code, env.routeHitCount(), rec.Body)
							}
							return
						}
						_, sent := sentOperation(t, env)
						if rec.Code != http.StatusConflict || env.routeHitCount() != 1 || !bytes.Equal(sent, body) {
							t.Fatalf("carriage status=%d Hub=%d body=%s", rec.Code, env.routeHitCount(), rec.Body)
						}
					})
				}
			}
		})
	}
}

// PCV-08/10: absent Coverage or Patient data is not a request for the native
// gateway to read its optional source. The authenticated participant supplies
// route and subject independently; the backend receives its original bytes.
func TestDTRIngress_NativeParametersWithoutEnrichment(t *testing.T) {
	for _, row := range []struct {
		name string
		body []byte
	}{
		{"complete", ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)},
		{"coverage absent", ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)},
		{"patient absent", ehrParams(dtrQuestionnaire)},
		{"empty", []byte(`{"resourceType":"Parameters","parameter":[]}`)},
		{"two payers", ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrCoverageParam(prefetchMember, "00078"), dtrQuestionnaire)},
		{"wrong subject", ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr-other", "other"), dtrQuestionnaire)},
		{"opaque member", ehrParams(`{"name":"x","valueString":"opaque"}`, dtrQuestionnaire)},
	} {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "must not search"}}
				env := newTransportExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				env.originator.cfg.SoR = s.sor()
				env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
				req := signedFixtureIngress(t, env.originator, "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, "", "pci-example", "", "dtr-native-"+row.name, row.body)
				rec := httptest.NewRecorder()
				env.originator.handleDTRIngress(rec, req)
				if rec.Code != http.StatusOK || env.routeHitCount() != 1 || !bytes.Equal(rec.Body.Bytes(), packageAnswer) {
					t.Fatalf("native DTR status=%d Hub=%d body=%s", rec.Code, env.routeHitCount(), rec.Body)
				}
				op, sent := sentOperation(t, env)
				if op != shnsdk.FrameOperationQuestionnairePackage || !bytes.Equal(sent, row.body) {
					t.Fatalf("operation=%q sent=%s want=%s", op, sent, row.body)
				}
				if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 || s.idReads != 0 {
					t.Fatalf("native DTR read source: searched=%v read=%v idReads=%d", searched, read, s.idReads)
				}
			})
		}
	}
}

// An explicit provider-authored DTR request has different obligations. The
// builder copies source-owned resources exactly and refuses absent Coverage or
// a cross-patient order before a body exists to submit to native ingress.
func TestDTRIngress_ExplicitParticipantConstruction(t *testing.T) {
	coverage := []byte(sorCoverage("cov-1", "00001"))
	order := []byte(sorRequest("sr1", "Patient/"+prefetchMember))
	in := shnsdk.QuestionnairePackageInputs{Coverages: [][]byte{coverage}, Orders: [][]byte{order}, Questionnaires: []string{"http://x/q|2.1.0"}}
	built, err := shnsdk.BuildQuestionnairePackageParameters("2.0", in)
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range built.Copied {
		var source []byte
		switch span.Parameter {
		case "coverage":
			source = coverage
		case "order":
			source = order
		default:
			t.Fatalf("unexpected copied parameter %s", span.Parameter)
		}
		if got := built.Body[span.At : span.At+span.End-span.Start]; !bytes.Equal(got, source[span.Start:span.End]) {
			t.Fatalf("%s changed: got=%s want=%s", span.Parameter, got, source[span.Start:span.End])
		}
	}
	for _, row := range []struct {
		name string
		in   shnsdk.QuestionnairePackageInputs
		want string
	}{
		{"missing coverage", shnsdk.QuestionnairePackageInputs{Orders: in.Orders, Questionnaires: in.Questionnaires}, "requires at least one coverage"},
		{"wrong-subject order", shnsdk.QuestionnairePackageInputs{Coverages: in.Coverages, Orders: [][]byte{[]byte(sorRequest("sr-other", "Patient/other"))}, Questionnaires: in.Questionnaires}, "another patient"},
		{"coverage without beneficiary", shnsdk.QuestionnairePackageInputs{Coverages: [][]byte{[]byte(`{"resourceType":"Coverage","id":"c1"}`)}, Orders: in.Orders, Questionnaires: in.Questionnaires}, "beneficiary"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := shnsdk.BuildQuestionnairePackageParameters("2.0", row.in); err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("builder error=%v; want %q", err, row.want)
			}
		})
	}
}

func TestProviderDTR_RefusedWithoutPeerCapability(t *testing.T) {
	env := newTransportExchange(t)
	declareFramedDTR(t, env, false)
	body := ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	req := signedFixtureIngress(t, env.originator, "/Questionnaire/$questionnaire-package", "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, "", "pci-example", "", "dtr-no-capability", body)
	rec := httptest.NewRecorder()
	env.originator.handleDTRIngress(rec, req)
	refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, shnsdk.ErrFramedDTRUnsupported.Error())
}
