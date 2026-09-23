package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A malformed message must fail the syntax rule before any envelope check.
func TestStructuralRules_Table(t *testing.T) {
	for _, tc := range []struct {
		body string
		want CheckState
	}{
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, CheckValid},
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]`, CheckInvalid},
	} {
		in := CheckInput{Direction: "request", Body: []byte(tc.body), Exchange: ExchangeContext{legType: "pas-claim", policy: NewConformancePolicy(EnforcementBasic)}}
		found := false
		for _, r := range StructuralRules() {
			if r.ID == "json.syntax" && r.Applies(in) {
				found = true
				if got := r.Check(context.Background(), in); got.State != tc.want {
					t.Fatalf("syntax = %+v, want %s", got, tc.want)
				}
			}
		}
		if !found {
			t.Fatal("missing applicable json.syntax rule")
		}
	}
}

// Each row removes or changes one shape from a valid envelope. Clinical facts
// and references are deliberately outside this registry's responsibility.
func TestStructuralEnvelopes(t *testing.T) {
	rows := []struct{ name, leg, op, direction, version, good, bad, rule string }{
		{"object", "pas-claim", "", "request", "", `{}`, `[]`, "json.object"},
		{"duplicate", "pas-claim", "", "request", "", `{"a":1}`, `{"a":1,"\u0061":2}`, "json.duplicate_key"},
		{"crd hook", "crd-order-select", "", "request", "", `{"hook":"order-select","hookInstance":"id","context":{}}`, `{"hook":1,"hookInstance":"id","context":{}}`, "crd.request.hook"},
		{"crd instance", "crd-order-dispatch", "", "request", "", `{"hook":"order-dispatch","hookInstance":"id","context":{}}`, `{"hook":"order-dispatch","context":{}}`, "crd.request.hookInstance"},
		{"crd context", "crd-order-select", "", "request", "", `{"hook":"order-select","hookInstance":"id","context":{}}`, `{"hook":"order-select","hookInstance":"id","context":[]}`, "crd.request.context"},
		{"cards", "crd-order-select", "", "response", "", `{"cards":[]}`, `{"cards":null}`, "response.cards"},
		{"card object", "crd-order-dispatch", "", "response", "", `{"cards":[{}]}`, `{"cards":[1]}`, "card.object"},
		{"package type", "dtr-questionnaire-fetch", "questionnaire-package", "request", "", `{"resourceType":"Parameters","parameter":[]}`, `{"resourceType":"Bundle","parameter":[]}`, "dtr.package.request"},
		{"package missing", "dtr-questionnaire-fetch", "questionnaire-package", "request", "", `{"resourceType":"Parameters","parameter":[]}`, `{"resourceType":"Parameters"}`, "dtr.package.request"},
		{"package array", "dtr-questionnaire-fetch", "questionnaire-package", "request", "", `{"resourceType":"Parameters","parameter":[]}`, `{"resourceType":"Parameters","parameter":{}}`, "dtr.package.request"},
		{"package entry", "dtr-questionnaire-fetch", "questionnaire-package", "request", "", `{"resourceType":"Parameters","parameter":[{"name":"x"}]}`, `{"resourceType":"Parameters","parameter":[1]}`, "dtr.package.request"},
		{"package name", "dtr-questionnaire-fetch", "questionnaire-package", "request", "", `{"resourceType":"Parameters","parameter":[{"name":"x"}]}`, `{"resourceType":"Parameters","parameter":[{"name":1}]}`, "dtr.package.request"},
		{"package success", "dtr-questionnaire-fetch", "questionnaire-package", "response", "", `{"resourceType":"Parameters"}`, `{"resourceType":"Parameters","parameter":null}`, "dtr.package.response"},
		{"next bare request", "dtr-questionnaire-fetch", "next-question", "request", "", `{"resourceType":"QuestionnaireResponse"}`, `{"resourceType":"Questionnaire"}`, "dtr.next-question.request"},
		{"next wrapped request", "dtr-questionnaire-fetch", "next-question", "request", "", `{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":{"resourceType":"QuestionnaireResponse"}}]}`, `{"resourceType":"Parameters","parameter":[]}`, "dtr.next-question.request"},
		{"next bare response", "dtr-questionnaire-fetch", "next-question", "response", "", `{"resourceType":"QuestionnaireResponse"}`, `{"resourceType":"Questionnaire"}`, "dtr.next-question.response"},
		{"next resource", "dtr-questionnaire-fetch", "next-question", "response", "", `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"QuestionnaireResponse"}}]}`, `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{}}]}`, "dtr.next-question.response"},
		{"pas request type", "pas-claim", "", "request", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, `{"resourceType":"Parameters","entry":[{"resource":{"resourceType":"Claim"}}]}`, "pas.request.bundle"},
		{"pas request primary", "pas-claim-update", "", "request", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, `{"resourceType":"Bundle","entry":[]}`, "pas.request.bundle"},
		{"pas inquiry request primary", "pas-claim-inquire", "", "request", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, "pas.request.bundle"},
		{"pas response primary", "pas-claim", "", "response", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, `{"resourceType":"Bundle","entry":[]}`, "pas.response.bundle"},
		{"pas response duplicate", "pas-claim-update", "", "response", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"ClaimResponse"}}]}`, "pas.response.bundle"},
		{"inquiry old shape", "pas-claim-inquire", "", "response", "pa.pas@2.0", `{"resourceType":"Bundle"}`, `{"resourceType":"Parameters"}`, "pas.inquiry.response"},
		{"inquiry middle shape", "pas-claim-inquire", "", "response", "pa.pas@2.1", `{"resourceType":"Bundle","entry":[]}`, `{"resourceType":"Bundle","entry":{}}`, "pas.inquiry.response"},
		{"inquiry modern shape", "pas-claim-inquire", "", "response", "pa.pas@2.2", `{"resourceType":"Parameters"}`, `{"resourceType":"Bundle"}`, "pas.inquiry.response"},
		{"inquiry return name", "pas-claim-inquire", "", "response", "pa.pas@2.2", `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"Bundle"}}]}`, `{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":{"resourceType":"Bundle"}}]}`, "pas.inquiry.return"},
		{"inquiry return resource", "pas-claim-inquire", "", "response", "pa.pas@2.2", `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"Bundle"}}]}`, `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"ClaimResponse"}}]}`, "pas.inquiry.response"},
		{"inquiry outcome", "pas-claim-inquire", "", "response", "pa.pas@2.2", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"x","diagnostics":"peer text"}]}`, `{"resourceType":"OperationOutcome","issue":[{"severity":1,"code":"x","diagnostics":"peer text"}]}`, "fhir.operation-outcome"},
		{"package outcome", "dtr-questionnaire-fetch", "questionnaire-package", "response", "", `{"resourceType":"OperationOutcome","issue":[]}`, `{"resourceType":"OperationOutcome","issue":{}}`, "fhir.operation-outcome"},
		{"eligibility request", "coverage-eligibility", "", "request", "", `{"resourceType":"CoverageEligibilityRequest"}`, `{"resourceType":"Task"}`, "eligibility.request"},
		{"eligibility response", "coverage-eligibility", "", "response", "", `{"resourceType":"CoverageEligibilityResponse"}`, `{"resourceType":"Task"}`, "eligibility.response"},
		{"query request", "federated-query", "", "request", "", `{"resourceType":"Task"}`, `{"resourceType":"Bundle"}`, "query.request"},
		{"query response", "federated-query", "", "response", "", `{"resourceType":"Task"}`, `{"resourceType":"Bundle"}`, "query.response"},
		{"patient link", "patient-dtr", "", "request", "", `{"linkId":"q","answer":"a","patientRef":"p"}`, `{"linkId":1,"answer":"a","patientRef":"p"}`, "patient-dtr.request.linkId"},
		{"patient answer", "patient-dtr", "", "request", "", `{"linkId":"q","answer":"a","patientRef":"p"}`, `{"linkId":"q","answer":{},"patientRef":"p"}`, "patient-dtr.request.answer"},
		{"patient ref", "patient-dtr", "", "request", "", `{"linkId":"q","answer":"a","patientRef":"p"}`, `{"linkId":"q","answer":"a"}`, "patient-dtr.request.patientRef"},
		{"patient response", "patient-dtr", "", "response", "", `{"attestedItem":{}}`, `{"attestedItem":[]}`, "patient-dtr.response"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.good)) || !json.Valid([]byte(tc.bad)) {
				t.Fatal("shape mutation must retain valid JSON in both control and rejection")
			}
			in := CheckInput{Exchange: ExchangeContext{legType: tc.leg, operation: tc.op}, Direction: tc.direction, Status: 200, DeclaredVersion: tc.version}
			var found bool
			for _, r := range StructuralRules() {
				if r.ID != tc.rule {
					continue
				}
				found = true
				for _, b := range []struct {
					body  string
					state CheckState
				}{{tc.good, CheckValid}, {tc.bad, CheckInvalid}} {
					in.Body = []byte(b.body)
					if !r.Applies(in) {
						t.Fatalf("rule %s not applicable", r.ID)
					}
					if got := r.Check(context.Background(), in); got.State != b.state || (got.State == CheckInvalid && got.Code != tc.rule) {
						t.Fatalf("%s(%s) = %+v want %s", r.ID, b.body, got, b.state)
					}
				}
			}
			if !found {
				t.Fatalf("missing rule %s", tc.rule)
			}
		})
	}
}

func TestBasicDoesNotDemandPASGraphClosure(t *testing.T) {
	for _, body := range []string{
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","insurer":{"reference":"Organization/missing"}}}]}`,
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse","patient":{"reference":"Patient/a"}}},{"resource":{"resourceType":"Patient","id":"b"}}]}`,
	} {
		in := CheckInput{Direction: "response", Status: 200, Body: []byte(body), Exchange: ExchangeContext{legType: "pas-claim", policy: NewConformancePolicy(EnforcementBasic)}}
		if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
}

func structuralInput(leg, operation, direction, body string) CheckInput {
	return CheckInput{Direction: direction, Status: 200, Body: []byte(body), Exchange: ExchangeContext{legType: leg, operation: operation, policy: NewConformancePolicy(EnforcementBasic)}}
}
func wantStructuralError(t *testing.T, err error, status int, rule string) {
	t.Helper()
	var e *conformanceError
	if !errors.As(err, &e) || e.status != status || e.Rule != rule {
		t.Fatalf("error=%+v want status=%d rule=%s", err, status, rule)
	}
}

// The participant policy controls execution, not just the disposition of a
// validator result. A failing rule must never be called at none/observe.
func TestStructuralEnforcementExecution(t *testing.T) {
	g := &Gateway{cfg: Config{HolderID: "gateway-a"}}
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
		in := structuralInput("pas-claim", "", "request", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`)
		in.Exchange.policy = NewConformancePolicy(level)
		calls := 0
		var doc *structuralDocument
		rules := StructuralRules()
		for i := range rules {
			check := rules[i].Check
			rules[i].Check = func(ctx context.Context, in CheckInput) CheckResult {
				calls++
				if in.decoded != nil {
					if doc != nil && doc != in.decoded {
						t.Fatal("message decoded more than once")
					}
					doc = in.decoded
				}
				return check(ctx, in)
			}
		}
		if err := g.enforceRules(context.Background(), in, rules); err != nil {
			t.Fatal(err)
		}
		if (level == EnforcementNone || level == EnforcementObserve) && (calls != 0 || doc != nil) {
			t.Fatal("disabled checker executed")
		}
		if level >= EnforcementBasic && (calls == 0 || doc == nil) {
			t.Fatal("required check never executed")
		}
		in.Body = []byte(`{"a":1,"a":2}`)
		err := g.enforceContent(context.Background(), in)
		if level >= EnforcementBasic {
			wantStructuralError(t, err, 422, "json.duplicate_key")
		} else if err != nil {
			t.Fatal(err)
		}
	}
	// Required but absent code cannot silently report success.
	in := structuralInput("pas-claim", "", "request", `{}`)
	wantStructuralError(t, g.enforceRules(context.Background(), in, []ConformanceRule{{ID: "required.rule", Class: CheckStructural, Applies: structuralApplicable}}), 503, "required.rule")
	in.Exchange.operation = "unknown"
	in.Exchange.legType = "dtr-questionnaire-fetch"
	wantStructuralError(t, g.enforceContent(context.Background(), in), 503, "content.contract")
	in.Exchange.legType = "unknown"
	wantStructuralError(t, g.enforceContent(context.Background(), in), 503, "content.contract")
}

func TestStructuralActualResponseStatus(t *testing.T) {
	g := &Gateway{}
	for leg := range paCatalog {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve, EnforcementBasic, EnforcementStrict} {
			in := structuralInput(leg, "questionnaire-package", "response", "")
			in.Exchange.policy = NewConformancePolicy(level)
			for _, status := range []int{100, 302, 400, 422, 500} {
				in.Status = status
				if err := g.enforceContent(context.Background(), in); err != nil {
					t.Fatalf("%s/%d: %v", leg, status, err)
				}
			}
			in.Status = 204
			err := g.enforceContent(context.Background(), in)
			if level >= EnforcementBasic {
				wantStructuralError(t, err, 502, "content.required")
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestStructuralJSONBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body, rule string
		status           int
	}{
		{"trailing", `{} {}`, "json.syntax", 422},
		{"utf8", string([]byte{'{', '"', 'x', '"', ':', '"', 255, '"', '}'}), "json.syntax", 422},
		{"nested duplicate", `{"x":{"a":1,"a":2}}`, "json.duplicate_key", 422},
		{"array duplicate", `{"x":[{"a":1,"a":2}]}`, "json.duplicate_key", 422},
		{"bytes", `{"x":"` + strings.Repeat("x", structuralMaxBytes) + `"}`, "json.bounds", 503},
		{"depth", strings.Repeat("[", structuralMaxDepth+2) + strings.Repeat("]", structuralMaxDepth+2), "json.bounds", 503},
		{"tokens", `{"x":[` + strings.Repeat("0,", structuralMaxTokens) + `0]}`, "json.bounds", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := structuralInput("pas-claim", "", "request", tc.body)
			wantStructuralError(t, (&Gateway{}).enforceContent(context.Background(), in), tc.status, tc.rule)
		})
	}
	in := structuralInput("crd-order-select", "", "response", `{"cards":[],"x":1,"X":2}`)
	if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
		t.Fatalf("case-distinct keys are not duplicates: %v", err)
	}
	// Direct rule checks are also usable by a later observer without enforcing.
	in.Body = []byte(`{"cards":[],"cards":[{}]}`)
	in.Exchange.policy = NewConformancePolicy(EnforcementObserve)
	for _, r := range StructuralRules() {
		if r.ID == "json.duplicate_key" && r.Check(context.Background(), in).State != CheckInvalid {
			t.Fatal("observer cannot see duplicate")
		}
	}
}

func TestStructuralInquiryUnionAndAlternatives(t *testing.T) {
	bodies := []string{
		`{"resourceType":"Bundle"}`,
		`{"resourceType":"Bundle","entry":[]}`,
		`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"ClaimResponse"}}]}`,
		`{"resourceType":"Parameters"}`,
		`{"resourceType":"Parameters","parameter":[]}`,
		`{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"Bundle"}},{"name":"return","resource":{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"ClaimResponse"}}]}}]}`,
		`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer diagnostics"}]}`,
	}
	for _, body := range bodies {
		in := structuralInput("pas-claim-inquire", "", "response", body)
		// Deliberately conflicting request stamp must never constrain the answer.
		in.Exchange.contractVersion = "pa.pas@2.0"
		before := bytes.Clone(in.Body)
		if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
			t.Fatalf("union rejected %s: %v", body, err)
		}
		if !bytes.Equal(before, in.Body) {
			t.Fatal("checker changed body")
		}
	}
	for _, body := range []string{`{"resourceType":"Parameters"}`, `{"resourceType":"Parameters","parameter":[]}`, `{"resourceType":"OperationOutcome","issue":[]}`} {
		in := structuralInput("dtr-questionnaire-fetch", "questionnaire-package", "response", body)
		if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
}

// Recorded native responses catch accidental narrowing by local workflow readers.
func TestStructuralNativeFixtures(t *testing.T) {
	for _, tc := range []struct{ file, leg, op string }{
		{"../relayfidelity/valid/pas-inquiry-response-2.0.json", "pas-claim-inquire", ""},
		{"../relayfidelity/valid/pas-inquiry-response-2.2.json", "pas-claim-inquire", ""},
		{"crd-response.json", "crd-order-select", ""},
		{"questionnaire-package.json", "dtr-questionnaire-fetch", "questionnaire-package"},
	} {
		body, err := os.ReadFile(filepath.Join("testdata", "br-payer", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		in := structuralInput(tc.leg, tc.op, "response", string(body))
		if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
	}
}

// Registry-plus-exchange composition only: the two real Gateway components use
// OriginateLegMessage and respondLeg; Hub routing and authority issuance use
// existing test transports. Checks are explicit boundaries here, not mounted
// native ingress/receiver wiring or proof of real Hub/OPA behavior.
func TestStructuralPairComposition(t *testing.T) {
	const claim = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`
	const bare = `{"resourceType":"QuestionnaireResponse"}`
	const wrapped = `{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":{"resourceType":"QuestionnaireResponse"}}]}`
	const wrappedResponse = `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"QuestionnaireResponse"}}]}`
	const bundle = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}},{"resource":{"resourceType":"ClaimResponse"}}]}`
	const outcome = `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid"}]}`
	rows := []struct{ name, leg, op, version, req, resp, badReq, badResp, reqRule, respRule string }{
		{"package bare", "dtr-questionnaire-fetch", "questionnaire-package", "pa.dtr@2.0", `{"resourceType":"Parameters","parameter":[]}`, definitionPackage(t, "2.0"), `{"resourceType":"Bundle","type":"collection","entry":[]}`, `{"resourceType":"Bundle","type":"collection","entry":[{"resource":[]}]}`, "dtr.package.request", "dtr.package.response"},
		{"next bare/bare", "dtr-questionnaire-fetch", "next-question", "", bare, bare, `{"resourceType":"Questionnaire"}`, `{"resourceType":"Questionnaire"}`, "dtr.next-question.request", "dtr.next-question.response"},
		{"next wrapped/wrapped", "dtr-questionnaire-fetch", "next-question", "", wrapped, wrappedResponse, `{"resourceType":"Parameters","parameter":[]}`, `{"resourceType":"Parameters","parameter":[]}`, "dtr.next-question.request", "dtr.next-question.response"},
		{"inquire old multiple responses", "pas-claim-inquire", "", "pa.pas@2.0", claim, bundle, "", `{"resourceType":"Bundle","entry":{}}`, "", "pas.inquiry.response"},
		{"inquire middle empty", "pas-claim-inquire", "", "pa.pas@2.1", claim, `{"resourceType":"Bundle","entry":[]}`, "", `{"resourceType":"Bundle","entry":{}}`, "", "pas.inquiry.response"},
		{"inquire modern omitted", "pas-claim-inquire", "", "pa.pas@2.2", claim, `{"resourceType":"Parameters"}`, "", `{"resourceType":"Parameters","parameter":null}`, "", "pas.inquiry.response"},
		{"inquire modern empty", "pas-claim-inquire", "", "pa.pas@2.2", claim, `{"resourceType":"Parameters","parameter":[]}`, "", `{"resourceType":"Parameters","parameter":{}}`, "", "pas.inquiry.response"},
		{"inquire modern multiple bundles", "pas-claim-inquire", "", "pa.pas@2.2", claim, `{"resourceType":"Parameters","parameter":[{"name":"return","resource":` + bundle + `},{"name":"return","resource":` + bundle + `}]}`, "", `{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":` + bundle + `},{"name":"return","resource":` + bundle + `}]}`, "", "pas.inquiry.return"},
	}
	for _, version := range []string{"pa.pas@2.0", "pa.pas@2.1", "pa.pas@2.2", ""} {
		rows = append(rows, struct{ name, leg, op, version, req, resp, badReq, badResp, reqRule, respRule string }{"outcome " + version, "pas-claim-inquire", "", version, claim, outcome, "", `{"resourceType":"OperationOutcome","issue":[{"severity":1,"code":"invalid"}]}`, "", "fhir.operation-outcome"})
	}
	for _, tc := range rows {
		for _, boundary := range []string{"valid", "request-originator", "request-recipient", "response-recipient", "response-originator"} {
			if strings.HasPrefix(boundary, "request") && tc.badReq == "" {
				continue
			}
			t.Run(tc.name+"/"+boundary, func(t *testing.T) {
				request, answer := tc.req, tc.resp
				if strings.HasPrefix(boundary, "request") {
					request = tc.badReq
				}
				if strings.HasPrefix(boundary, "response") {
					answer = tc.badResp
				}
				// Only the selected boundary enforces in mutation rows. Its peer's off
				// setting cannot lower that independently selected policy.
				source, dest := EnforcementBasic, EnforcementBasic
				if boundary == "request-recipient" || boundary == "response-recipient" {
					source = EnforcementNone
				}
				if boundary == "request-originator" || boundary == "response-originator" {
					dest = EnforcementNone
				}
				got, err, hits, backend := structuralPair(t, tc.leg, tc.op, tc.version, request, answer, 200, source, dest)
				switch boundary {
				case "valid":
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, []byte(answer)) || hits != 1 || backend != 1 {
						t.Fatalf("reply=%q hits=%d backend=%d", got, hits, backend)
					}
				case "request-originator":
					wantStructuralError(t, err, 422, tc.reqRule)
					if hits != 0 || backend != 0 {
						t.Fatal("refused request reached network")
					}
				case "request-recipient":
					wantStructuralError(t, err, 422, tc.reqRule)
					if hits != 1 || backend != 0 {
						t.Fatal("refused request reached backend")
					}
				default:
					wantStructuralError(t, err, 502, tc.respRule)
					if hits != 1 || backend != 1 {
						t.Fatal("response check ran outside answer boundary")
					}
				}
			})
		}
	}
	// The recorded responseBundle deviation stays byte-identical at off/observe.
	deviation := `{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":` + bundle + `}]}`
	for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
		got, err, hits, backend := structuralPair(t, "pas-claim-inquire", "", "pa.pas@2.2", claim, deviation, 202, level, level)
		if err != nil || string(got) != deviation || hits != 1 || backend != 1 {
			t.Fatalf("deviation changed: %q %v", got, err)
		}
	}
	for _, status := range []int{302, 422, 500} {
		got, err, hits, backend := structuralPair(t, "pas-claim-inquire", "", "pa.pas@2.0", string(conformantPASBundleWithQR(t, "MBR-COVERED")), "non JSON peer answer", status, EnforcementStrict, EnforcementStrict)
		if err != nil || string(got) != "non JSON peer answer" || hits != 1 || backend != 1 {
			t.Fatalf("application error changed: %q %v", got, err)
		}
	}
}

func structuralPair(t *testing.T, leg, op, version, request, answer string, status int, sourceLevel, destLevel ConformanceEnforcement) ([]byte, error, int, int) {
	t.Helper()
	e := newInProcessExchange(t)
	e.originator.cfg.ConformanceEnforcement = sourceLevel
	recipient, _ := newInboundTestGateway(t, true)
	recipient.cfg.ConformanceEnforcement = destLevel
	// Reuse recipient crypto with the existing sender/recipient registry entries.
	recipient.cfg.Identity.EncPub = e.substrate.recipientEncPub
	recipient.cfg.Identity.EncPriv = e.substrate.recipientEncPriv
	recipient.cfg.Reg = e.originator.cfg.Reg
	recipient.cfg.AuthzPub = e.originator.cfg.AuthzPub
	recipient.cfg.Client = &http.Client{Transport: &inboundAuthzStub{authzPriv: e.substrate.authzPriv, clock: e.substrate.clock}}
	entry, _ := e.originator.cfg.Reg.Lookup("payer")
	entry.RequestFrames = shnsdk.SupportedRequestFrames()
	e.originator.cfg.Reg.Set("payer", entry)
	var boundaryErr error
	hits, backend := 0, 0
	source := structuralInput(leg, op, "request", request)
	source.Exchange.policy = NewConformancePolicy(sourceLevel)
	source.Exchange.holder, source.Exchange.recipient = "provider", "payer"
	source.Exchange.subjectPCI, _, _ = e.originator.cfg.SoR.ResolvePatient("MBR-COVERED")
	source.Exchange.contractVersion = version
	source.Exchange.correlationID = "corr-1"
	source.Exchange.contentType = "application/fhir+json"
	source.Exchange.bodySHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(request)))
	if version != "" {
		source.Exchange.versionSource = "producer"
	}
	e.ctx = context.WithValue(e.ctx, nativeExchangeKey{}, source.Exchange)
	if err := e.originator.enforceRules(e.ctx, source, StructuralRules()); err != nil {
		return nil, err, hits, backend
	}
	e.originator.cfg.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/route") {
			return e.substrate.RoundTrip(r)
		}
		hits++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		env, err := shnsdk.DecodeEnvelope(raw)
		if err != nil {
			return nil, err
		}
		wire, err := shnsdk.Open(env, recipient.cfg.Identity.EncPub, recipient.cfg.Identity.EncPriv)
		if err != nil {
			return nil, err
		}
		if shnsdk.IsFramed(wire) {
			header, body, err := shnsdk.DecodeHTTPFrame(wire)
			if err != nil {
				return nil, err
			}
			wire = body
			if op != "" && header.Headers[shnsdk.FrameHeaderOperation] != op {
				t.Fatal("operation changed on wire")
			}
		}
		if !bytes.Equal(wire, []byte(request)) {
			t.Fatal("transmitted request bytes changed")
		}
		dest := source
		dest.Body = wire
		dest.Exchange.policy = NewConformancePolicy(destLevel)
		if err := recipient.enforceRules(e.ctx, dest, StructuralRules()); err != nil {
			boundaryErr = err
			return nil, err
		}
		backend++
		dest.Direction = "response"
		dest.Status = status
		dest.Body = []byte(answer)
		dest.DeclaredVersion = version
		if err := recipient.enforceRules(e.ctx, dest, StructuralRules()); err != nil {
			boundaryErr = err
			return nil, err
		}
		result := LegResult{ApplicationStatus: status, Response: relay.Exact(relay.NewBody([]byte(answer), relay.OriginUpstreamResponse), "application/fhir+json"), ResponseContractVersion: version, ResponseVersionSource: "producer"}
		spec := paCatalog[leg]
		rec := httptest.NewRecorder()
		if status/100 == 2 {
			recipient.respondLeg(rec, r, spec.RespFrame, spec.RespOp, leg, env.Metadata.CorrelationID, result, source.Exchange.subjectPCI, "provider", "", "")
		} else {
			recipient.respondLegError(rec, r, spec.RespFrame, spec.RespOp, leg, env.Metadata.CorrelationID, result, source.Exchange.subjectPCI, "provider", "", "")
		}
		if rec.Code != 200 {
			t.Fatalf("recipient seal failed: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Result(), nil
	})}
	reply, err := e.originator.OriginateLegMessage(e.ctx, e.req, "payer", leg, source.Exchange.subjectPCI, "corr-1", "", Content{WorkstreamType: workstreamPA, ProfileID: version, Operation: op, Payload: testRequest([]byte(request))})
	if boundaryErr != nil {
		return nil, boundaryErr, hits, backend
	}
	if err != nil {
		return nil, err, hits, backend
	}
	got, err := reply.bytes(leg)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Status != status || !bytes.Equal(got, []byte(answer)) {
		t.Fatalf("actual ApplicationReply changed: status=%d body=%q", reply.Status, got)
	}
	if reply.DeclaredVersion != version {
		t.Fatalf("answer declaration changed: %s", reply.DeclaredVersion)
	}
	source.Direction = "response"
	source.Status = reply.Status
	source.Body = got
	source.DeclaredVersion = reply.DeclaredVersion
	err = e.originator.enforceRules(e.ctx, source, StructuralRules())
	return got, err, hits, backend
}

func TestStructuralEmptySuccessDoesNotDecode(t *testing.T) {
	in := structuralInput("pas-claim", "", "response", "")
	in.Status = 204
	rules := StructuralRules()
	for i := range rules {
		if rules[i].ID == "content.required" {
			check := rules[i].Check
			rules[i].Check = func(ctx context.Context, in CheckInput) CheckResult {
				if in.decoded != nil {
					t.Error("empty success decoded before required-body refusal")
				}
				return check(ctx, in)
			}
		}
	}
	wantStructuralError(t, (&Gateway{}).enforceRules(context.Background(), in, rules), 502, "content.required")
}

func TestStructuralNestedShapeRejections(t *testing.T) {
	rows := []struct {
		name, leg, op, rule string
		good                string
		bad                 []string
	}{
		{"next containers", "dtr-questionnaire-fetch", "next-question", "dtr.next-question.response", `{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"QuestionnaireResponse"}}]}`, []string{
			`{"resourceType":"Parameters","parameter":{}}`,
			`{"resourceType":"Parameters","parameter":[1]}`,
			`{"resourceType":"Parameters","parameter":[{"name":1,"resource":{"resourceType":"QuestionnaireResponse"}}]}`,
			`{"resourceType":"Parameters","parameter":[{"name":"return","resource":[]}]}`,
			`{"resourceType":"Parameters","parameter":[{"name":"return"}]}`,
			`{"resourceType":"Parameters","parameter":[{"name":"return","resource":{"resourceType":"QuestionnaireResponse"}},{"name":"return","resource":{"resourceType":"QuestionnaireResponse"}}]}`,
		}},
		{"bundle containers", "pas-claim", "", "pas.response.bundle", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, []string{
			`{"resourceType":"Bundle"}`,
			`{"resourceType":"Bundle","entry":null}`,
			`{"resourceType":"Bundle","entry":[1]}`,
			`{"resourceType":"Bundle","entry":[{"resource":[]}]}`,
			`{"resourceType":"Bundle","entry":[{"resource":{}}]}`,
		}},
		{"outcome fields", "pas-claim-inquire", "", "fhir.operation-outcome", `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer text","details":{},"location":["x"],"expression":["y"]}]}`, []string{
			`{"resourceType":"OperationOutcome","issue":[1]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":1,"diagnostics":"peer text","details":{},"location":["x"],"expression":["y"]}]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":1,"details":{},"location":["x"],"expression":["y"]}]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer text","details":[],"location":["x"],"expression":["y"]}]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer text","details":{},"location":{},"expression":["y"]}]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer text","details":{},"location":[1],"expression":["y"]}]}`,
			`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"peer text","details":{},"location":["x"],"expression":[1]}]}`,
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			in := structuralInput(row.leg, row.op, "response", row.good)
			g := &Gateway{}
			if err := g.enforceContent(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			for _, bad := range row.bad {
				in.Body = []byte(bad)
				wantStructuralError(t, g.enforceContent(context.Background(), in), 502, row.rule)
			}
		})
	}
}

func TestStructuralUndeclaredInquiryDeviation(t *testing.T) {
	body := `{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":{"resourceType":"Bundle"}}]}`
	in := structuralInput("pas-claim-inquire", "", "response", body)
	in.Exchange.contractVersion = "pa.pas@2.2"
	g := &Gateway{}
	if err := g.enforceContent(context.Background(), in); err != nil {
		t.Fatalf("request stamp imposed answer parameter name: %v", err)
	}
	in.DeclaredVersion = "pa.pas@2.2"
	wantStructuralError(t, g.enforceContent(context.Background(), in), 502, "pas.inquiry.return")
	in.DeclaredVersion = ""
	for _, bad := range []struct{ body, rule string }{
		{`{"resourceType":"Parameters","parameter":[{"name":"arbitrary","resource":{"resourceType":"Bundle"}}]}`, "pas.inquiry.return"},
		{`{"resourceType":"Parameters","parameter":[{"name":1,"resource":{"resourceType":"Bundle"}}]}`, "pas.inquiry.response"},
		{`{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":[]}]}`, "pas.inquiry.response"},
		{`{"resourceType":"Parameters","parameter":[{"name":"responseBundle","resource":{"resourceType":"ClaimResponse"}}]}`, "pas.inquiry.response"},
	} {
		in.Body = []byte(bad.body)
		wantStructuralError(t, g.enforceContent(context.Background(), in), 502, bad.rule)
	}
	got, err, hits, backend := structuralPair(t, "pas-claim-inquire", "", "", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, body, 200, EnforcementBasic, EnforcementBasic)
	if err != nil || string(got) != body || hits != 1 || backend != 1 {
		t.Fatalf("undeclared component exchange = %q %v", got, err)
	}
}
