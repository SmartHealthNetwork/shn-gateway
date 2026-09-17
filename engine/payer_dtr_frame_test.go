package engine

// Payer-side DTR operations: a request frame may name the DTR operation its
// body is the input of (the operation header). The payer gateway answers a
// named operation by sending its payer's system the participant's own input
// exactly (or with only the payer identity mapped), binds every patient the
// input names to the authorized subject, and relays the payer's answer
// exactly. A request without the header is the older questionnaire request,
// still accepted.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const (
	dtrFrameMember = "MBR-COVERED"
	dtrOtherMember = "MBR-UC04"
	packagePath    = "/Questionnaire/$questionnaire-package"
	nextPath       = "/Questionnaire/$next-question"
)

// dtrPackageFixture returns a published $questionnaire-package input with its
// patient renamed to a member the test system of record knows.
func dtrPackageFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b := payorFixture(t, rel)
	for _, from := range []string{"Patient/examplepatient", "Patient/example"} {
		b = bytes.ReplaceAll(b, []byte(from), []byte("Patient/"+dtrFrameMember))
	}
	return b
}

// dtrCoverage is a minimal Coverage for member, paid by the CMS identity.
func dtrCoverage(id, member string) string {
	return `{"resourceType":"Coverage","id":"` + id + `","status":"active","beneficiary":{"reference":"Patient/` + member +
		`"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`
}

// dtrOrder is a minimal ServiceRequest for member.
func dtrOrder(member string) string {
	return `{"resourceType":"ServiceRequest","id":"sr-1","status":"draft","intent":"order","subject":{"reference":"Patient/` + member + `"}}`
}

// dtrParams assembles a $questionnaire-package input from raw parameter
// objects, byte for byte.
func dtrParams(params ...string) []byte {
	return []byte(`{"resourceType":"Parameters","parameter":[` + strings.Join(params, ",") + `]}`)
}

func resourceParam(name, resource string) string {
	return `{"name":"` + name + `","resource":` + resource + `}`
}

const questionnaireParam = `{"name":"questionnaire","valueCanonical":"http://example.org/Questionnaire/q|1.0.0"}`

// nextQuestionQR is an in-progress QuestionnaireResponse about member.
func nextQuestionQR(member string) string {
	return `{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/` + member + `"}}`
}

// dtrPayer is a payer gateway whose content occupant forwards to a stub of
// the payer's own system.
type dtrPayer struct {
	g         *Gateway
	requester inboundTestRequester
	partner   *stubPartner
	coveredPC string
}

func newDTRPayer(t *testing.T, opts ...NativeOption) dtrPayer {
	t.Helper()
	g, requester := newInboundTestGateway(t, true)
	p := newStubPartner(t)
	p.respByPath[packagePath] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	p.respByPath[nextPath] = nextQuestionAnswer(t, "Patient/"+dtrFrameMember, rawItems(t, adaptiveTree(t, "1")))
	g.cfg.Responder = NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil, opts...)
	pci, _, ok := g.cfg.SoR.(*censusSoR).ResolvePatient(dtrFrameMember)
	if !ok {
		t.Fatalf("%s is not in the test system of record", dtrFrameMember)
	}
	return dtrPayer{g: g, requester: requester, partner: p, coveredPC: pci}
}

// dtrAnswer is what the requester received: the application status and body
// of the response frame, or the refusal the payer gateway wrote.
type dtrAnswer struct {
	status int
	body   []byte
}

// send delivers body on the questionnaire leg, naming operation in the
// request frame ("" for a request without the header), for the covered
// member's authorized subject.
func (d dtrPayer) send(t *testing.T, operation string, body []byte) dtrAnswer {
	t.Helper()
	return d.sendFor(t, operation, body, d.coveredPC)
}

func (d dtrPayer) sendFor(t *testing.T, operation string, body []byte, subject string) dtrAnswer {
	t.Helper()
	d.partner.lastPath, d.partner.lastBody = "", nil
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: d.requester.ID, Recipient: "payer", TransactionType: "dtr-questionnaire-fetch",
		AuthorityFrame: "provider-tpo", Timestamp: d.g.cfg.Clock().Format(time.RFC3339),
		CorrelationID: "corr-dtr-op",
	}, body, d.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(withRequestFrameOperation(withAnswerLine(context.Background(), "pa.dtr@2.0"), operation))
	d.g.handleDTRInbound(rec, r, env, shnsdk.Token{Operation: "dtr-questionnaire-fetch", Subject: subject, CorrelationID: "corr-dtr-op"},
		body, "pa.dtr@2.0")
	if rec.Code != http.StatusOK {
		return dtrAnswer{status: rec.Code, body: rec.Body.Bytes()}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, d.requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return dtrAnswer{status: hdr.Status, body: answer}
}

// requireRefused checks a refusal and that nothing reached the payer's system.
func (d dtrPayer) requireRefused(t *testing.T, got dtrAnswer, status int, msg string) {
	t.Helper()
	if got.status != status || !strings.Contains(string(got.body), msg) {
		t.Fatalf("answer = %d %s, want %d naming %q", got.status, got.body, status, msg)
	}
	if d.partner.lastPath != "" {
		t.Fatalf("a refused request reached the payer's system at %s: %s", d.partner.lastPath, d.partner.lastBody)
	}
}

// TestPayerDTR_DispatchByFrameOperation: the operation header selects the
// payer operation, the body is sent exactly, and an operation this gateway
// does not define is refused before anything is sent.
func TestPayerDTR_DispatchByFrameOperation(t *testing.T) {
	d := newDTRPayer(t)
	pkg := dtrPackageFixture(t, "valid/dtr-package-params-2.0.json")
	nextParams := dtrParams(resourceParam("questionnaire-response", nextQuestionQR(dtrFrameMember)))
	nextBare := []byte(nextQuestionQR(dtrFrameMember))

	for _, tc := range []struct {
		name, operation, path string
		body                  []byte
	}{
		{"questionnaire-package", shnsdk.FrameOperationQuestionnairePackage, packagePath, pkg},
		{"next-question Parameters", shnsdk.FrameOperationNextQuestion, nextPath, nextParams},
		{"next-question bare QuestionnaireResponse", shnsdk.FrameOperationNextQuestion, nextPath, nextBare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := d.send(t, tc.operation, tc.body)
			if got.status != http.StatusOK {
				t.Fatalf("answer = %d %s, want 200", got.status, got.body)
			}
			if d.partner.lastPath != tc.path {
				t.Fatalf("payer's system called at %q, want %q", d.partner.lastPath, tc.path)
			}
			if !bytes.Equal(d.partner.lastBody, tc.body) {
				t.Fatalf("payer's system received\n%s\nwant the carried input exactly\n%s", d.partner.lastBody, tc.body)
			}
			if want := d.partner.respByPath[tc.path]; !bytes.Equal(got.body, want) {
				t.Fatalf("requester received %s, want the payer's answer exactly %s", got.body, want)
			}
		})
	}

	t.Run("unknown operation refused", func(t *testing.T) {
		d.requireRefused(t, d.send(t, "questionnaire-fetch", pkg), http.StatusBadRequest, "unsupported DTR operation")
	})

	t.Run("responder refuses an unknown operation too", func(t *testing.T) {
		n := NewNativeResponder(d.partner.srv.Client(), d.partner.srv.URL, "shn-order-select", nil, nil)
		d.partner.lastPath = ""
		res, err := n.Handle(withRequestFrameOperation(context.Background(), "questionnaire-fetch"), "dtr-questionnaire-fetch", "corr", "pci", pkg)
		if err != nil || res.Status != http.StatusBadRequest || !strings.Contains(res.Message, "unsupported DTR operation") {
			t.Fatalf("Handle = %+v, %v; want a 400 naming the operation", res, err)
		}
		if d.partner.lastPath != "" {
			t.Fatalf("an unknown operation reached the payer's system")
		}
	})

	t.Run("answer keeps the payer's media type", func(t *testing.T) {
		n := NewNativeResponder(d.partner.srv.Client(), d.partner.srv.URL, "shn-order-select", nil, nil)
		for _, op := range []string{shnsdk.FrameOperationQuestionnairePackage, shnsdk.FrameOperationNextQuestion, ""} {
			body := pkg
			switch op {
			case shnsdk.FrameOperationNextQuestion:
				body = nextParams
			case "":
				body = []byte(`{"canonical":"http://example.org/Questionnaire/q","coverage":` + dtrCoverage("cov-1", dtrFrameMember) + `}`)
			}
			res, err := n.Handle(withRequestFrameOperation(context.Background(), op), "dtr-questionnaire-fetch", "corr", "pci", body)
			if err != nil || res.Status != 0 {
				t.Fatalf("operation %q: Handle = %+v, %v", op, res, err)
			}
			if res.Response.Ownership() != relay.OwnershipRelayed || res.ResponseContentType() != "application/json" {
				t.Fatalf("operation %q: answer is %v %q, want relayed with the payer's own media type", op, res.Response.Ownership(), res.ResponseContentType())
			}
		}
	})
}

// TestPayerDTR_LegacyEnvelopeStillAccepted: a questionnaire request without
// the operation header is still answered, for both a package and an
// adaptive round.
func TestPayerDTR_LegacyEnvelopeStillAccepted(t *testing.T) {
	d := newDTRPayer(t)
	t.Run("package", func(t *testing.T) {
		body := []byte(`{"canonical":"http://example.org/Questionnaire/q","coverage":` + dtrCoverage("cov-1", dtrFrameMember) + `}`)
		got := d.send(t, "", body)
		if got.status != http.StatusOK || d.partner.lastPath != packagePath {
			t.Fatalf("answer = %d %s at %q, want 200 from the package operation", got.status, got.body, d.partner.lastPath)
		}
		if !bytes.Contains(d.partner.lastBody, []byte(`"name":"coverage"`)) || !bytes.Contains(d.partner.lastBody, []byte(`"valueCanonical":"http://example.org/Questionnaire/q"`)) {
			t.Fatalf("payer's system received %s, want the package input built from the request", d.partner.lastBody)
		}
	})
	t.Run("adaptive round", func(t *testing.T) {
		body := []byte(`{"canonical":"http://example.org/Questionnaire/q","nextQuestion":` + nextQuestionQR(dtrFrameMember) + `}`)
		got := d.send(t, "", body)
		if got.status != http.StatusOK || d.partner.lastPath != nextPath {
			t.Fatalf("answer = %d %s at %q, want 200 from the next-question operation", got.status, got.body, d.partner.lastPath)
		}
	})
	t.Run("canonical only", func(t *testing.T) {
		got := d.send(t, "", []byte(`{"canonical":"http://example.org/Questionnaire/q"}`))
		if got.status != http.StatusOK || d.partner.lastPath != packagePath {
			t.Fatalf("answer = %d %s at %q, want 200", got.status, got.body, d.partner.lastPath)
		}
	})
}

// TestPayerDTR_PackageSubjectBound: every patient a questionnaire request
// names must be the authorized subject, on the framed operations and on the
// older request alike. Each rejection row changes one thing in a request the
// control answers.
func TestPayerDTR_PackageSubjectBound(t *testing.T) {
	d := newDTRPayer(t)
	pkg := func(params ...string) []byte {
		return dtrParams(append(params, questionnaireParam)...)
	}
	own := resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))
	ownOrder := resourceParam("order", dtrOrder(dtrFrameMember))
	package_ := shnsdk.FrameOperationQuestionnairePackage
	next0 := shnsdk.FrameOperationNextQuestion

	t.Run("framed control", func(t *testing.T) {
		if got := d.send(t, package_, pkg(own, ownOrder)); got.status != http.StatusOK {
			t.Fatalf("answer = %d %s, want 200", got.status, got.body)
		}
	})

	framed := []struct {
		name    string
		body    []byte
		status  int
		message string
	}{
		{"second coverage for another patient", pkg(own, resourceParam("coverage", dtrCoverage("cov-2", dtrOtherMember))), http.StatusForbidden, "more than one patient"},
		{"order for another patient", pkg(own, resourceParam("order", dtrOrder(dtrOtherMember))), http.StatusForbidden, "more than one patient"},
		{"order names another patient as patient", pkg(own, resourceParam("order", `{"resourceType":"DeviceRequest","status":"draft","intent":"order","subject":{"reference":"Patient/`+dtrFrameMember+`"},"patient":{"reference":"Patient/`+dtrOtherMember+`"}}`)), http.StatusForbidden, "more than one patient"},
		{"Patient resource for another patient", pkg(own, resourceParam("referenced", `{"resourceType":"Patient","id":"`+dtrOtherMember+`"}`)), http.StatusForbidden, "more than one patient"},
		{"referenced resource about another patient", pkg(own, resourceParam("referenced", `{"resourceType":"Observation","status":"final","subject":{"reference":"https://ehr.example/fhir/Patient/`+dtrOtherMember+`"}}`)), http.StatusForbidden, "more than one patient"},
		{"coverage for a patient other than the authorized one", pkg(resourceParam("coverage", dtrCoverage("cov-2", dtrOtherMember))), http.StatusForbidden, "token subject does not match request patient"},
		{"unknown member", pkg(resourceParam("coverage", dtrCoverage("cov-2", "MBR-NOBODY"))), http.StatusBadRequest, "unknown member"},
		{"no coverage", pkg(ownOrder), http.StatusBadRequest, "has no coverage"},
		{"coverage without a beneficiary", pkg(resourceParam("coverage", `{"resourceType":"Coverage","status":"active"}`)), http.StatusBadRequest, "not a Patient reference"},
		{"coverage parameter that is not a Coverage", pkg(resourceParam("coverage", `{"resourceType":"Patient","id":"`+dtrFrameMember+`"}`)), http.StatusBadRequest, "not a Coverage"},
		{"order subject that is not a Patient", pkg(own, resourceParam("order", `{"resourceType":"ServiceRequest","status":"draft","intent":"order","subject":{"reference":"Group/g1"}}`)), http.StatusBadRequest, "not a Patient reference"},
		{"Patient resource with no id", pkg(own, resourceParam("referenced", `{"resourceType":"Patient","name":[{"family":"Other"}],"birthDate":"1970-01-01"}`)), http.StatusBadRequest, "Patient resource with no id"},
		{"not Parameters", []byte(`{"resourceType":"Bundle"}`), http.StatusBadRequest, "parse questionnaire-package parameters failed"},
		{"duplicate beneficiary member", pkg(resourceParam("coverage", strings.Replace(dtrCoverage("cov-1", dtrFrameMember), `"beneficiary":`, `"beneficiary":{"reference":"Patient/`+dtrOtherMember+`"},"beneficiary":`, 1))), http.StatusBadRequest, "parse questionnaire-package parameters failed"},
	}
	for _, tc := range framed {
		t.Run("framed/"+tc.name, func(t *testing.T) {
			d.requireRefused(t, d.send(t, package_, tc.body), tc.status, tc.message)
		})
	}

	t.Run("framed next-question", func(t *testing.T) {
		next := shnsdk.FrameOperationNextQuestion
		d.requireRefused(t, d.send(t, next, []byte(nextQuestionQR(dtrOtherMember))), http.StatusForbidden, "token subject does not match request patient")
		two := dtrParams(resourceParam("questionnaire-response", nextQuestionQR(dtrFrameMember)), resourceParam("questionnaire-response", nextQuestionQR(dtrOtherMember)))
		d.requireRefused(t, d.send(t, next, two), http.StatusBadRequest, "parse next-question input failed")
		d.requireRefused(t, d.send(t, next, pkg(own)), http.StatusBadRequest, "parse next-question input failed")
		d.requireRefused(t, d.send(t, next, []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Group/g1"}}`)), http.StatusBadRequest, "carries no patient subject")
	})

	t.Run("framed next-question with an absolute patient reference", func(t *testing.T) {
		qr := `{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"https://ehr.example/fhir/Patient/` + dtrFrameMember + `"}}`
		d.partner.respByPath[nextPath] = nextQuestionAnswer(t, "https://ehr.example/fhir/Patient/"+dtrFrameMember, rawItems(t, adaptiveTree(t, "1")))
		defer func() {
			d.partner.respByPath[nextPath] = nextQuestionAnswer(t, "Patient/"+dtrFrameMember, rawItems(t, adaptiveTree(t, "1")))
		}()
		if got := d.send(t, next0, []byte(qr)); got.status != http.StatusOK {
			t.Fatalf("answer = %d %s, want 200", got.status, got.body)
		}
		other := strings.Replace(qr, dtrFrameMember, dtrOtherMember, 1)
		d.requireRefused(t, d.send(t, next0, []byte(other)), http.StatusForbidden, "token subject does not match request patient")
	})

	t.Run("legacy control", func(t *testing.T) {
		body := []byte(`{"canonical":"q","coverage":` + dtrCoverage("cov-1", dtrFrameMember) + `,"order":` + dtrOrder(dtrFrameMember) + `}`)
		if got := d.send(t, "", body); got.status != http.StatusOK {
			t.Fatalf("answer = %d %s, want 200", got.status, got.body)
		}
	})
	legacy := []struct {
		name    string
		body    string
		status  int
		message string
	}{
		{"coverage for another patient", `{"canonical":"q","coverage":` + dtrCoverage("cov-2", dtrOtherMember) + `}`, http.StatusForbidden, "token subject does not match request patient"},
		{"order for another patient", `{"canonical":"q","coverage":` + dtrCoverage("cov-1", dtrFrameMember) + `,"order":` + dtrOrder(dtrOtherMember) + `}`, http.StatusForbidden, "more than one patient"},
		{"order alone for another patient", `{"order":` + dtrOrder(dtrOtherMember) + `}`, http.StatusForbidden, "token subject does not match request patient"},
		{"coverage that is an id-less Patient", `{"canonical":"q","coverage":{"resourceType":"Patient","name":[{"family":"Other"}]}}`, http.StatusBadRequest, "Patient resource with no id"},
		{"duplicate coverage member", `{"canonical":"q","coverage":` + dtrCoverage("cov-1", dtrFrameMember) + `,"coverage":` + dtrCoverage("cov-2", dtrOtherMember) + `}`, http.StatusBadRequest, "parse questionnaire fetch failed"},
	}
	for _, tc := range legacy {
		t.Run("legacy/"+tc.name, func(t *testing.T) {
			d.requireRefused(t, d.send(t, "", []byte(tc.body)), tc.status, tc.message)
		})
	}
}

// TestPayerDTR_FramedParametersPostedExactly: the payer's system receives
// the published package input exactly as the requester sent it, or, with the
// payer identity mapping on, with only the payer identifier tokens of every
// coverage changed.
func TestPayerDTR_FramedParametersPostedExactly(t *testing.T) {
	backend := shnsdk.PayerIdentifier{System: "urn:example:payer-backend", Value: "BACKEND-7"}
	for _, tc := range []struct {
		fixture string
		own     shnsdk.PayerIdentifier
	}{
		{"valid/dtr-package-params-2.0.json", shnsdk.CMSPayerIdentity},
		{"valid/dtr-package-params-2.2.json", shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.4.7", Value: "10D0202020"}},
	} {
		body := dtrPackageFixture(t, tc.fixture)
		t.Run(tc.fixture+"/exact", func(t *testing.T) {
			d := newDTRPayer(t)
			if got := d.send(t, shnsdk.FrameOperationQuestionnairePackage, body); got.status != http.StatusOK {
				t.Fatalf("answer = %d %s", got.status, got.body)
			}
			if !bytes.Equal(d.partner.lastBody, body) {
				t.Fatalf("payer's system received\n%s\nwant exactly\n%s", d.partner.lastBody, body)
			}
		})
		t.Run(tc.fixture+"/payer identity mapped", func(t *testing.T) {
			d := newDTRPayer(t, WithPayorEdgeIdentity(tc.own, backend))
			if got := d.send(t, shnsdk.FrameOperationQuestionnairePackage, body); got.status != http.StatusOK {
				t.Fatalf("answer = %d %s", got.status, got.body)
			}
			sent := d.partner.lastBody
			if bytes.Equal(sent, body) {
				t.Fatal("the mapping made no edit")
			}
			if !bytes.Contains(sent, []byte(`"`+backend.Value+`"`)) {
				t.Fatalf("payer's system did not receive the mapped identity: %s", sent)
			}
			restored := bytes.ReplaceAll(sent, []byte(`"`+backend.System+`"`), []byte(`"`+tc.own.System+`"`))
			restored = bytes.ReplaceAll(restored, []byte(`"`+backend.Value+`"`), []byte(`"`+tc.own.Value+`"`))
			if !bytes.Equal(restored, body) {
				t.Fatalf("bytes other than the payer identifier changed:\n%s\nwant\n%s", sent, body)
			}
		})
	}

	t.Run("every coverage is mapped", func(t *testing.T) {
		d := newDTRPayer(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, backend))
		body := dtrParams(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember)), resourceParam("coverage", dtrCoverage("cov-2", dtrFrameMember)), questionnaireParam)
		if got := d.send(t, shnsdk.FrameOperationQuestionnairePackage, body); got.status != http.StatusOK {
			t.Fatalf("answer = %d %s", got.status, got.body)
		}
		var sent struct {
			Parameter []struct {
				Name     string          `json:"name"`
				Resource json.RawMessage `json:"resource"`
			} `json:"parameter"`
		}
		if err := json.Unmarshal(d.partner.lastBody, &sent); err != nil {
			t.Fatal(err)
		}
		mapped := 0
		for _, p := range sent.Parameter {
			if p.Name != "coverage" {
				continue
			}
			if got, ok := shnsdk.ParsePayerIdentifier(p.Resource, nil); !ok || got != backend {
				t.Fatalf("coverage %s was sent with payer %v, want %v", p.Resource, got, backend)
			}
			mapped++
		}
		if mapped != 2 {
			t.Fatalf("%d coverages mapped, want 2", mapped)
		}
	})

	t.Run("a coverage naming another payer is refused before sending", func(t *testing.T) {
		d := newDTRPayer(t, WithPayorEdgeIdentity(shnsdk.CMSPayerIdentity, backend))
		foreign := strings.Replace(dtrCoverage("cov-2", dtrFrameMember), `"00001"`, `"99999"`, 1)
		body := dtrParams(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember)), resourceParam("coverage", foreign), questionnaireParam)
		got := d.send(t, shnsdk.FrameOperationQuestionnairePackage, body)
		d.requireRefused(t, got, http.StatusUnprocessableEntity, "name more than one payer")
		only := dtrParams(resourceParam("coverage", foreign), questionnaireParam)
		d.requireRefused(t, d.send(t, shnsdk.FrameOperationQuestionnairePackage, only), http.StatusBadRequest, "does not match this gateway's own payer identity")
	})
}

// TestInboundFrameOperation: the operation header is read from a request
// frame, and is defined only for the questionnaire leg.
func TestInboundFrameOperation(t *testing.T) {
	frame := func(headers map[string]string, body string) []byte {
		t.Helper()
		out, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	withOp := map[string]string{shnsdk.FrameHeaderContractVersion: "pa.dtr@2.0", shnsdk.FrameHeaderOperation: shnsdk.FrameOperationNextQuestion}
	for _, tc := range []struct {
		name, leg string
		payload   []byte
		wantOp    string
		status    int
	}{
		{"framed operation", "dtr-questionnaire-fetch", frame(withOp, `{}`), shnsdk.FrameOperationNextQuestion, 0},
		{"unknown operation is passed to the leg", "dtr-questionnaire-fetch", frame(map[string]string{shnsdk.FrameHeaderOperation: "other"}, `{}`), "other", 0},
		{"frame without the header", "dtr-questionnaire-fetch", frame(map[string]string{shnsdk.FrameHeaderContractVersion: "pa.dtr@2.0"}, `{}`), "", 0},
		{"bare request", "dtr-questionnaire-fetch", []byte(`{"canonical":"q"}`), "", 0},
		{"operation on another leg", "pas-claim", frame(map[string]string{shnsdk.FrameHeaderContractVersion: "pa.pas@2.0", shnsdk.FrameHeaderOperation: shnsdk.FrameOperationQuestionnairePackage}, `{}`), "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, status, msg := inboundFrameOperation(tc.leg, tc.payload)
			if op != tc.wantOp || status != tc.status {
				t.Fatalf("got (%q, %d, %q), want (%q, %d)", op, status, msg, tc.wantOp, tc.status)
			}
			if status != 0 && !strings.Contains(msg, "operation header is not defined") {
				t.Fatalf("refusal %q does not name the header", msg)
			}
		})
	}
	if got := RequestFrameOperation(withRequestFrameOperation(context.Background(), "x")); got != "x" {
		t.Fatalf("RequestFrameOperation = %q", got)
	}
	if got := RequestFrameOperation(context.Background()); got != "" {
		t.Fatalf("RequestFrameOperation without a frame = %q", got)
	}
}

// TestPayerDTR_AnswerWithoutMediaType: an answer the payer's system sends
// with no Content-Type is relayed as FHIR JSON.
func TestPayerDTR_AnswerWithoutMediaType(t *testing.T) {
	answer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil // suppress sniffing: no media type at all
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", nil, nil)
	res, err := n.Handle(withRequestFrameOperation(context.Background(), shnsdk.FrameOperationQuestionnairePackage),
		"dtr-questionnaire-fetch", "corr", "pci", dtrPackageFixture(t, "valid/dtr-package-params-2.0.json"))
	if err != nil || res.Status != 0 {
		t.Fatalf("Handle = %+v, %v", res, err)
	}
	if res.ResponseContentType() != "application/fhir+json" || !bytes.Equal(responseBytes(res), answer) {
		t.Fatalf("answer %q %s, want the payer's bytes as application/fhir+json", res.ResponseContentType(), responseBytes(res))
	}
}
