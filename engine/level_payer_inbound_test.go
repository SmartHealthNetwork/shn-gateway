package engine

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Per-level rows for the payer-side inbound path: a sealed request arrives
// from the Hub at the payer gateway (handleInbound), which forwards it to its
// participant's own system (the native forward) and carries the answer back
// (none and observe refuse nothing a participant's payload is judged by;
// network rules and routing refuse at every level).
//
// Every row drives the real handleInbound: the Hub assertion, the bound
// token, the envelope and the leg dispatch are the production path, so the
// finding context each row asserts is the one handleInbound sets.

const (
	crdSelectPath   = "/cds-services/shn-order-select"
	crdDispatchPath = "/cds-services/order-dispatch-crd"
	pasSubmitPath   = "/Claim/$submit"
	pasInquirePath  = "/Claim/$inquire"
)

// levelPayer is a payer gateway at one conformance level whose content
// occupant is the native forward to a stub of its participant's own system.
type levelPayer struct {
	g         *Gateway
	requester inboundTestRequester
	partner   *stubPartner
	store     *censusSoR
	level     ConformanceEnforcement
	hubPriv   ed25519.PrivateKey
	authzPriv ed25519.PrivateKey
	pci       string
	findings  []ConformanceFinding
	skipped   []ObserverEvent
	seq       int
}

// newLevelPayer builds the payer at level. opts are extra native-forward
// options (the payer identity mapping, the partner's declared lines).
func newLevelPayer(t *testing.T, level ConformanceEnforcement, opts ...NativeOption) *levelPayer {
	t.Helper()
	g, requester := newInboundTestGateway(t, true)
	g.cfg.ConformanceEnforcement = level
	hubPub, hubPriv := genED25519(t)
	authzPub, authzPriv := genED25519(t)
	g.cfg.HubTransportPub, g.cfg.AuthzPub = hubPub, authzPub
	store := g.cfg.Store.(*censusSoR)
	p := newStubPartner(t)
	p.respByPath[crdSelectPath] = realCRDAnswer(t)
	p.respByPath[crdDispatchPath] = realCRDAnswer(t)
	p.respByPath[packagePath] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	p.respByPath[nextPath] = nextQuestionAnswer(t, "Patient/"+dtrFrameMember, rawItems(t, adaptiveTree(t, "1")))
	p.respByPath[pasSubmitPath] = []byte(assemblyRealPending)
	p.respByPath[pasInquirePath] = decidedAnswer(t)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", store, fixedClock,
		append([]NativeOption{WithConformancePolicy(NewConformancePolicy(level))}, opts...)...)
	n.bindFindingEmitter(g.emitFinding)
	g.cfg.Responder = n
	pci, _, ok := store.ResolvePatient(dtrFrameMember)
	if !ok {
		t.Fatalf("%s is not in the test system of record", dtrFrameMember)
	}
	lp := &levelPayer{g: g, requester: requester, partner: p, store: store, level: level, hubPriv: hubPriv, authzPriv: authzPriv, pci: pci}
	g.cfg.Observer = func(e ObserverEvent) {
		switch e.Kind {
		case ConformanceObservedEvent:
			var f ConformanceFinding
			if json.Unmarshal([]byte(e.Detail), &f) == nil {
				lp.findings = append(lp.findings, f)
			}
		case LocalWriteSkippedEvent:
			lp.skipped = append(lp.skipped, e)
		}
	}
	return lp
}

// payerAnswer is what the requester received: the application status and
// body of the response frame, or the raw refusal the payer gateway wrote.
type payerAnswer struct {
	status int
	body   []byte
	framed bool
	corr   string
}

// send delivers body on leg through handleInbound, bound to the covered
// member's pci, naming operation in the request frame when set.
func (p *levelPayer) send(t *testing.T, leg, operation string, body []byte) payerAnswer {
	t.Helper()
	return p.sendAs(t, leg, operation, body, p.pci)
}

// sendAs is send for a token whose subject is subject.
func (p *levelPayer) sendAs(t *testing.T, leg, operation string, body []byte, subject string) payerAnswer {
	t.Helper()
	p.partner.lastPath, p.partner.lastBody = "", nil
	p.seq++
	spec, ok := paCatalog[leg]
	if !ok {
		t.Fatalf("no catalog entry for %s", leg)
	}
	corr := fmt.Sprintf("corr-%s-%d", leg, p.seq)
	payload := body
	if operation != "" {
		var err error
		payload, err = shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, map[string]string{shnsdk.FrameHeaderOperation: operation}, body)
		if err != nil {
			t.Fatal(err)
		}
	}
	clock := p.g.cfg.Clock()
	env, err := shnsdk.Seal(shnsdk.Metadata{
		Sender: p.requester.ID, Recipient: p.g.cfg.HolderID, TransactionType: leg,
		AuthorityFrame: spec.ReqFrame, Timestamp: clock.Format(time.RFC3339), CorrelationID: corr,
	}, payload, p.g.cfg.Identity.EncPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tok := signTestToken(shnsdk.Token{
		Operation: spec.Op, Subject: subject, Frame: spec.ReqFrame, Holder: p.requester.ID,
		CorrelationID: corr, Expiry: clock.Add(time.Hour), PayloadHash: sha256hexT(env.Ciphertext),
	}, p.authzPriv)
	tokBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.Metadata.AuthzToken = string(tokBytes)
	raw, err := shnsdk.EncodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	as, err := json.Marshal(shnsdk.IssueAssertion("hub", p.g.cfg.HolderID, p.hubPriv, clock, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/substrate/inbound", bytes.NewReader(raw))
	r.Header.Set("X-Hub-Assertion", base64.StdEncoding.EncodeToString(as))
	rec := httptest.NewRecorder()
	p.g.handleInbound(rec, r)
	if rec.Code != http.StatusOK {
		return payerAnswer{status: rec.Code, body: rec.Body.Bytes(), corr: corr}
	}
	hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, p.requester, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode response frame: %v", err)
	}
	return payerAnswer{status: hdr.Status, body: answer, framed: true, corr: corr}
}

// content returns the content-rule findings recorded, as rule/decision.
func (p *levelPayer) content() []ConformanceFinding {
	var out []ConformanceFinding
	for _, f := range p.findings {
		if f.Kind == string(KindContent) {
			out = append(out, f)
		}
	}
	return out
}

func findingsText(fs []ConformanceFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule+"/"+f.Decision+"/"+f.Whose)
	}
	return out
}

// wantRefused asserts the requester received status naming msg and that the
// participant's system saw nothing.
func (p *levelPayer) wantRefused(t *testing.T, got payerAnswer, status int, msg string) {
	t.Helper()
	if got.status != status || !strings.Contains(string(got.body), msg) {
		t.Fatalf("answer = %d %s, want %d naming %q", got.status, got.body, status, msg)
	}
	if p.partner.lastPath != "" {
		t.Fatalf("a refused request reached the participant's system at %s", p.partner.lastPath)
	}
}

// wantFindings asserts the content findings a row records: none at none, one
// relayed finding naming rule (the first) on leg from whose at observe, and a
// refused one at strict.
func (p *levelPayer) wantFindings(t *testing.T, leg, rule, whose string) {
	t.Helper()
	fs := p.content()
	switch p.level {
	case EnforcementNone:
		if len(fs) != 0 {
			t.Fatalf("at none nothing is recorded, got %v", findingsText(fs))
		}
	case EnforcementObserve:
		if len(fs) == 0 || fs[0].Rule != rule || fs[0].Decision != "relayed" || fs[0].LegType != leg || fs[0].Whose != whose || fs[0].Seam != "payer-native" || fs[0].PayloadSHA256 == "" {
			t.Fatalf("at observe want a relayed %s finding on %s from %s, got %+v", rule, leg, whose, fs)
		}
		for _, f := range fs {
			if f.Decision != "relayed" {
				t.Fatalf("at observe nothing is refused, got %v", findingsText(fs))
			}
		}
	case EnforcementStrict:
		if len(fs) == 0 || fs[0].Rule != rule || fs[0].Decision != "refused" || fs[0].LegType != leg {
			t.Fatalf("at strict want a refused %s finding on %s, got %v", rule, leg, findingsText(fs))
		}
	}
}

// wantRequestRow asserts a request content-rule row: strict refuses with
// status and msg before the participant's system is called; below strict the
// request reaches the participant's system at path exactly as sent and its
// answer is relayed exactly.
func (p *levelPayer) wantRequestRow(t *testing.T, leg string, got payerAnswer, body []byte, path, rule string, status int, msg string) {
	t.Helper()
	if p.level == EnforcementStrict {
		p.wantRefused(t, got, status, msg)
	} else {
		if got.status != http.StatusOK || !got.framed {
			t.Fatalf("at %s the request must be forwarded: answer %d %s", p.level, got.status, got.body)
		}
		if p.partner.lastPath != path || !bytes.Equal(p.partner.lastBody, body) {
			t.Fatalf("at %s the participant's system must receive the request as sent at %s, got %s:\n%s", p.level, path, p.partner.lastPath, p.partner.lastBody)
		}
		if want := p.partner.respByPath[path]; !bytes.Equal(got.body, want) {
			t.Fatalf("at %s the answer must be relayed exactly:\n got %s\nwant %s", p.level, got.body, want)
		}
	}
	p.wantFindings(t, leg, rule, "peer")
}

// wantAnswerRow asserts an answer content-rule row: strict refuses with
// status and msg (the request was forwarded); below strict the participant's
// answer is relayed exactly. The finding names the answer as the payer's own.
func (p *levelPayer) wantAnswerRow(t *testing.T, leg string, got payerAnswer, path string, answer []byte, rule string, status int, msg string) {
	t.Helper()
	if p.partner.lastPath != path {
		t.Fatalf("the request must be forwarded to %s, got %q", path, p.partner.lastPath)
	}
	if p.level == EnforcementStrict {
		if got.status != status || !strings.Contains(string(got.body), msg) {
			t.Fatalf("answer = %d %s, want %d naming %q", got.status, got.body, status, msg)
		}
	} else if got.status != http.StatusOK || !got.framed || !bytes.Equal(got.body, answer) {
		t.Fatalf("at %s the participant's answer must be relayed exactly: %d %s", p.level, got.status, got.body)
	}
	p.wantFindings(t, leg, rule, "own")
}

// ---- CRD order-select (crd_native.go) ----

func crdSelectWith(t *testing.T, old, new string) []byte {
	t.Helper()
	body := conformantCRD("MBR-COVERED", "72148")
	out := bytes.Replace(body, []byte(old), []byte(new), 1)
	if bytes.Equal(out, body) {
		t.Fatalf("fixture: %q not replaced", old)
	}
	return out
}

// The order and the prefetch coverage are the request's own content;
// the subject is context.patientId.
func TestLevelPayerCRDSelect_RequestContent(t *testing.T) {
	const sr = `{"fullUrl":"urn:uuid:sr1","resource":{"resourceType":"ServiceRequest","id":"sr1","status":"draft","intent":"order","subject":{"reference":"Patient/MBR-COVERED"},"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148"}]}}}`
	rows := map[string]struct {
		body   []byte
		rule   string
		status int
		msg    string
	}{
		"no order": {crdSelectWith(t, sr, ``), RuleRequestShape, http.StatusBadRequest, "no order (ServiceRequest or DeviceRequest) in draftOrders"},
		"order naming no patient": {crdSelectWith(t, `"subject":{"reference":"Patient/MBR-COVERED"},"code"`, `"code"`),
			RulePatientMixed, http.StatusBadRequest, "parse order subject failed"},
		"no prefetch coverage": {crdSelectWith(t, `"coverage":{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/MBR-COVERED"}}`, `"other":{}`),
			RuleRequestShape, http.StatusBadRequest, "parse coverage beneficiary failed"},
		"order for another patient": {crdSelectWith(t, `"subject":{"reference":"Patient/MBR-COVERED"},"code"`, `"subject":{"reference":"Patient/MBR-UC04"},"code"`),
			RulePatientMixed, http.StatusBadRequest, "inconsistent patient in order-select"},
		"coverage for another patient": {crdSelectWith(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"}`, `"beneficiary":{"reference":"Patient/MBR-UC04"}`),
			RulePatientMixed, http.StatusBadRequest, "inconsistent patient in order-select"},
		// "Patient/" is read and names the empty member: inconsistent with
		// context.patientId, as strict has always refused it.
		"hookInstance of the wrong type": {crdSelectWith(t, `"hookInstance":"hi-1"`, `"hookInstance":7`),
			RuleRequestShape, http.StatusBadRequest, "parse cds request failed"},
		"prefetch of the wrong type": {crdSelectWith(t, `"prefetch":{`, `"prefetch":[],"x":{`),
			RuleRequestShape, http.StatusBadRequest, "parse cds request failed"},
		"order naming the empty member": {crdSelectWith(t, `"subject":{"reference":"Patient/MBR-COVERED"},"code"`, `"subject":{"reference":"Patient/"},"code"`),
			RulePatientMixed, http.StatusBadRequest, "inconsistent patient in order-select"},
		"coverage naming the empty member": {crdSelectWith(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"}`, `"beneficiary":{"reference":"Patient/"}`),
			RulePatientMixed, http.StatusBadRequest, "inconsistent patient in order-select"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "crd-order-select", "", row.body)
				p.wantRequestRow(t, "crd-order-select", got, row.body, crdSelectPath, row.rule, row.status, row.msg)
			})
		}
	}
}

// ---- CRD order-dispatch (crd_dispatch_native.go) ----

const crdDispatchRequest = `{"hook":"order-dispatch","hookInstance":"h-1","context":{"patientId":"MBR-COVERED","dispatchedOrders":["DeviceRequest/dr1"],"performer":"Organization/o1"},` +
	`"prefetch":{"deviceHistory":{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"DeviceRequest/dr1","resource":{"resourceType":"DeviceRequest","id":"dr1","status":"active","intent":"order",` +
	`"codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431"}]},"subject":{"reference":"Patient/MBR-COVERED"}}}]},` +
	`"coverage":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"payor":[{"reference":"Organization/o2"}]}}]}}}`

func crdDispatchWith(t *testing.T, old, new string) []byte {
	t.Helper()
	out := strings.Replace(crdDispatchRequest, old, new, 1)
	if out == crdDispatchRequest {
		t.Fatalf("fixture: %q not replaced", old)
	}
	return []byte(out)
}

// Everything but context.patientId is the request's own
// content. A value of the wrong type outside context.patientId and prefetch
// (which the dispatched orders and the coverage are read from) is the
// request's own shape, refused at strict with the body strict has always
// given.
func TestLevelPayerCRDDispatch_RequestContent(t *testing.T) {
	rows := map[string]struct {
		body   []byte
		rule   string
		status int
		msg    string
	}{
		"no dispatchedOrders": {crdDispatchWith(t, `"dispatchedOrders":["DeviceRequest/dr1"],`, ``), RuleRequestShape, http.StatusBadRequest, "no dispatchedOrders"},
		"no performer":        {crdDispatchWith(t, `,"performer":"Organization/o1"`, ``), RuleRequestShape, http.StatusBadRequest, "missing performer"},
		"order not in prefetch": {crdDispatchWith(t, `"dispatchedOrders":["DeviceRequest/dr1"]`, `"dispatchedOrders":["DeviceRequest/dr1","DeviceRequest/dr9"]`),
			RuleRequestShape, http.StatusBadRequest, "dispatched order not resolvable from prefetch"},
		"order naming no patient": {crdDispatchWith(t, `,"subject":{"reference":"Patient/MBR-COVERED"}}}]}`, `}}]}`),
			RulePatientMixed, http.StatusForbidden, "dispatched order missing patient subject"},
		"order for another patient": {crdDispatchWith(t, `"subject":{"reference":"Patient/MBR-COVERED"}`, `"subject":{"reference":"Patient/MBR-UC04"}`),
			RulePatientMixed, http.StatusForbidden, "inconsistent patient in order-dispatch"},
		"coverage for another patient": {crdDispatchWith(t, `"beneficiary":{"reference":"Patient/MBR-COVERED"}`, `"beneficiary":{"reference":"Patient/MBR-UC04"}`),
			RulePatientMixed, http.StatusForbidden, "inconsistent patient in order-dispatch"},
		"performer of the wrong type": {crdDispatchWith(t, `"performer":"Organization/o1"`, `"performer":5`),
			RuleRequestShape, http.StatusBadRequest, "parse cds request failed"},
		"dispatchedOrders of the wrong type": {crdDispatchWith(t, `"dispatchedOrders":["DeviceRequest/dr1"]`, `"dispatchedOrders":"DeviceRequest/dr1"`),
			RuleRequestShape, http.StatusBadRequest, "parse cds request failed"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "crd-order-dispatch", "", row.body)
				p.wantRequestRow(t, "crd-order-dispatch", got, row.body, crdDispatchPath, row.rule, row.status, row.msg)
			})
		}
	}
}

// ---- DTR (payer.go) ----

func dtrPackage(params ...string) []byte {
	return dtrParams(append(params, questionnaireParam)...)
}

// The package input's own shape and consistency. The subject is
// the first coverage's beneficiary.
func TestLevelPayerDTRPackage_RequestContent(t *testing.T) {
	own := resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))
	rows := map[string]struct {
		body   []byte
		rule   string
		status int
		msg    string
	}{
		"order subject not a Patient": {dtrPackage(own, resourceParam("order", `{"resourceType":"ServiceRequest","status":"draft","intent":"order","subject":{"reference":"Group/g1"}}`)),
			RuleRequestShape, http.StatusBadRequest, "not a Patient reference"},
		"coverage without a beneficiary beside the subject's": {dtrPackage(own, resourceParam("coverage", `{"resourceType":"Coverage","id":"cov-2","status":"active"}`)),
			RuleRequestShape, http.StatusBadRequest, "not a Patient reference"},
		// Without payer identity mapping the request is already routed here.
		"coverage parameter not a Coverage": {dtrPackage(own, resourceParam("coverage", `{"resourceType":"Patient","id":"`+dtrFrameMember+`"}`)),
			RuleRequestShape, http.StatusBadRequest, "questionnaire-package coverage parameter is not a Coverage"},
		"Patient with no id": {dtrPackage(own, resourceParam("referenced", `{"resourceType":"Patient","name":[{"family":"Other"}]}`)),
			RuleRequestShape, http.StatusBadRequest, "Patient resource with no id"},
		"no coverage": {dtrPackage(resourceParam("order", dtrOrder(dtrFrameMember))),
			RuleRequestShape, http.StatusBadRequest, "has no coverage"},
		"order for another patient": {dtrPackage(own, resourceParam("order", dtrOrder(dtrOtherMember))),
			RulePatientMixed, http.StatusForbidden, "more than one patient"},
		"Patient resource for another patient": {dtrPackage(own, resourceParam("referenced", `{"resourceType":"Patient","id":"`+dtrOtherMember+`"}`)),
			RulePatientMixed, http.StatusForbidden, "more than one patient"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				got := p.send(t, "dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, row.body)
				p.wantRequestRow(t, "dtr-questionnaire-fetch", got, row.body, packagePath, row.rule, row.status, row.msg)
			})
		}
	}
}

// The payer's DTR answer is its own content.
func TestLevelPayerDTR_AnswerContent(t *testing.T) {
	notQR := []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
	otherPatient := nextQuestionAnswer(t, "Patient/"+dtrOtherMember, rawItems(t, adaptiveTree(t, "1")))
	subjectQuestionnaire := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","id":"q","status":"active","subject":{"reference":"Patient/X"}}}]}`)
	rows := map[string]struct {
		operation, path string
		body, answer    []byte
		rule            string
		status          int
		msg             string
		framed          bool
	}{
		"next-question answer not a questionnaire-response": {shnsdk.FrameOperationNextQuestion, nextPath, []byte(nextQuestionQR(dtrFrameMember)), notQR,
			RuleAnswerShape, http.StatusBadGateway, "next-question response is not a questionnaire-response", false},
		"next-question answer about another patient": {shnsdk.FrameOperationNextQuestion, nextPath, []byte(nextQuestionQR(dtrFrameMember)), otherPatient,
			RulePatientAnswer, http.StatusForbidden, "response patient does not match request patient", true},
		"package Questionnaire carrying a subject": {shnsdk.FrameOperationQuestionnairePackage, packagePath, dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))), subjectQuestionnaire,
			RuleAnswerShape, http.StatusForbidden, "questionnaire response unexpectedly carries a subject", true},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.partner.respByPath[row.path] = row.answer
				got := p.send(t, "dtr-questionnaire-fetch", row.operation, row.body)
				p.wantAnswerRow(t, "dtr-questionnaire-fetch", got, row.path, row.answer, row.rule, row.status, row.msg)
				if level == EnforcementStrict && got.framed != row.framed {
					t.Fatalf("the strict refusal's wire shape changed: framed=%v, want %v", got.framed, row.framed)
				}
			})
		}
	}
}
