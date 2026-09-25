package engine

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The rules the payer-side inbound path keeps at every level: the subject
// (it resolves to a pci through the payer's own system, the unknown-member setting applying), a
// body read one way only (a repeated member name), a request whose subject
// cannot be read, and routing to the participant's own system (the operation,
// the version route, the payer identity mapping). Each refuses at none,
// observe and strict with the same status and body, records no content
// finding, and sends nothing to the participant's system.

type payerNetworkRow struct {
	leg, operation string
	body           []byte
	opts           []NativeOption
	requireKnown   bool // the payer opts in to REQUIRE_KNOWN_MEMBERS
	status         int
	msg            string
}

// A token for another patient is not a payer-side row: the payer binds the member
// the request names by its own system and files what it records under that
// binding (TestLevelPayer_TokenForAnotherPatientIsAccepted, payer_subject_test.go).
func runPayerNetworkRows(t *testing.T, rows map[string]payerNetworkRow) {
	t.Helper()
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level, row.opts...)
				p.g.cfg.RequireKnownMembers = row.requireKnown
				got := p.sendAs(t, row.leg, row.operation, row.body, p.pci)
				p.wantRefused(t, got, row.status, row.msg)
				if fs := p.content(); len(fs) != 0 {
					t.Fatalf("a network rule records no content finding, got %v", findingsText(fs))
				}
			})
		}
	}
}

func TestLevelPayerCRD_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	sel := conformantCRD("MBR-COVERED", "72148")
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"order-select: duplicate key": {leg: "crd-order-select", body: bytes.Replace(sel, []byte(`"hook":"order-select",`), []byte(`"hook":"order-select","hook":"order-select",`), 1),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-select: case-folded duplicate key": {leg: "crd-order-select", body: bytes.Replace(sel, []byte(`"hook":"order-select",`), []byte(`"hook":"order-select","Hook":"order-select",`), 1),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-select: no context.patientId": {leg: "crd-order-select", body: crdSelectWith(t, `"patientId":"MBR-COVERED",`, ``),
			status: http.StatusBadRequest, msg: "inconsistent patient in order-select"},
		"order-select: subject of the wrong type": {leg: "crd-order-select", body: crdSelectWith(t, `"patientId":"MBR-COVERED"`, `"patientId":7`),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-select: unknown member, known members required": {leg: "crd-order-select", requireKnown: true, body: bytes.ReplaceAll(sel, []byte("MBR-COVERED"), []byte("MBR-NOBODY")),
			status: http.StatusBadRequest, msg: "unknown member"},
		"order-dispatch: no context.patientId": {leg: "crd-order-dispatch", body: crdDispatchWith(t, `"patientId":"MBR-COVERED",`, ``),
			status: http.StatusBadRequest, msg: "missing context.patientId"},
		"order-dispatch: unknown member, known members required": {leg: "crd-order-dispatch", requireKnown: true, body: []byte(strings.ReplaceAll(crdDispatchRequest, "MBR-COVERED", "MBR-NOBODY")),
			status: http.StatusBadRequest, msg: "unknown member"},
		// The subject and the prefetch the orders and coverage are read from
		// must read.
		"order-dispatch: subject of the wrong type": {leg: "crd-order-dispatch", body: crdDispatchWith(t, `"patientId":"MBR-COVERED"`, `"patientId":7`),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-dispatch: prefetch of the wrong type": {leg: "crd-order-dispatch", body: crdDispatchWith(t, `"prefetch":{`, `"prefetch":[],"x":{`),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-dispatch: duplicate key": {leg: "crd-order-dispatch", body: crdDispatchWith(t, `"performer":"Organization/o1"`, `"performer":"Organization/o1","performer":5`),
			status: http.StatusBadRequest, msg: "parse cds request failed"},
		"order-select: version route": {leg: "crd-order-select", body: sel, opts: []NativeOption{WithDeclaredContractVersions([]string{"pa.pas@2.0"})},
			status: http.StatusUnprocessableEntity, msg: "no shared contract line for pa.crd"},
	})
}

func TestLevelPayerDTR_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	own := resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))
	pkg := shnsdk.FrameOperationQuestionnairePackage
	next := shnsdk.FrameOperationNextQuestion
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"no operation": {leg: "dtr-questionnaire-fetch", body: dtrPackage(own),
			status: http.StatusBadRequest, msg: refusalDTRUnframed},
		"unsupported operation": {leg: "dtr-questionnaire-fetch", operation: "questionnaire-fetch", body: dtrPackage(own),
			status: http.StatusBadRequest, msg: "unsupported DTR operation"},
		"not Parameters": {leg: "dtr-questionnaire-fetch", operation: pkg, body: []byte(`{"resourceType":"Bundle"}`),
			status: http.StatusBadRequest, msg: "parse questionnaire-package parameters failed"},
		"duplicate key": {leg: "dtr-questionnaire-fetch", operation: pkg, body: dtrPackage(resourceParam("coverage", strings.Replace(dtrCoverage("cov-1", dtrFrameMember), `"status":"active"`, `"status":"active","status":"active"`, 1))),
			status: http.StatusBadRequest, msg: "parse questionnaire-package parameters failed"},
		"coverage parameter not a Coverage, with payer identity mapping": {leg: "dtr-questionnaire-fetch", operation: pkg, body: dtrPackage(own, resourceParam("coverage", `{"resourceType":"Patient","id":"`+dtrFrameMember+`"}`)),
			opts: withIdentityMapping(), status: http.StatusBadRequest, msg: "questionnaire-package coverage parameter is not a Coverage"},
		"coverage parameter that cannot be read": {leg: "dtr-questionnaire-fetch", operation: pkg, body: dtrPackage(own, `{"name":"coverage","resource":5}`),
			status: http.StatusBadRequest, msg: "questionnaire-package coverage parameter is not a Coverage"},
		"no patient named at all": {leg: "dtr-questionnaire-fetch", operation: pkg, body: dtrPackage(resourceParam("coverage", `{"resourceType":"Coverage","id":"cov-2","status":"active"}`)),
			status: http.StatusBadRequest, msg: "not a Patient reference"},
		"unknown member, known members required": {leg: "dtr-questionnaire-fetch", requireKnown: true, operation: pkg, body: dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", "MBR-NOBODY"))),
			status: http.StatusBadRequest, msg: "unknown member"},
		"next-question input unreadable": {leg: "dtr-questionnaire-fetch", operation: next, body: dtrPackage(own),
			status: http.StatusBadRequest, msg: "parse next-question input failed"},
		"next-question naming no patient": {leg: "dtr-questionnaire-fetch", operation: next, body: []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress"}`),
			status: http.StatusBadRequest, msg: "next-question request carries no patient subject"},
	})
}

func TestLevelPayerPAS_NetworkRulesRefuseAtEveryLevel(t *testing.T) {
	base := pasIngressBundle("00001", "")
	// An amendment whose Claim.related is not a list: the subject binds, the
	// prior authorization the amendment names cannot be read. (A value of the
	// wrong type elsewhere in its entries is its own shape:
	// TestLevelPayerPASUpdate_WrongTypedContent.)
	updateUnreadable := updateBundleSetting(t, "Claim", "related", 5)
	cms := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00001"}
	backend := shnsdk.PayerIdentifier{System: "urn:example:payer-backend", Value: "BACKEND-7"}
	runPayerNetworkRows(t, map[string]payerNetworkRow{
		"submit: unreadable": {leg: "pas-claim", body: []byte(`{`),
			status: http.StatusBadRequest, msg: "parse claim bundle failed"},
		"submit: duplicate key": {leg: "pas-claim", body: []byte(strings.Replace(base, `"type":"collection"`, `"type":"collection","type":"collection"`, 1)),
			status: http.StatusBadRequest, msg: "parse claim bundle failed"},
		"submit: not a Bundle": {leg: "pas-claim", body: []byte(`{"resourceType":"Parameters"}`),
			status: http.StatusBadRequest, msg: "PAS request is not a Bundle"},
		"submit: no Claim.patient": {leg: "pas-claim", body: []byte(strings.Replace(base, `"resourceType":"Claim","patient":{"reference":"Patient/MBR-COVERED"}`, `"resourceType":"Claim"`, 1)),
			status: http.StatusBadRequest, msg: "PAS bundle missing Claim.patient"},
		"submit: unknown member, known members required": {leg: "pas-claim", requireKnown: true, body: []byte(strings.ReplaceAll(base, "MBR-COVERED", "MBR-NOBODY")),
			status: http.StatusBadRequest, msg: "unknown member"},
		"update Claim.related unreadable": {leg: "pas-claim-update", body: updateUnreadable,
			status: http.StatusBadRequest, msg: "parse update Claim entry failed"},
		"submit: foreign payer": {leg: "pas-claim", body: []byte(pasIngressBundle("99999", "")), opts: []NativeOption{WithPayorEdgeIdentity(cms, backend)},
			status: http.StatusBadRequest, msg: "does not match this gateway's own payer identit"},
		"submit: version route": {leg: "pas-claim", body: []byte(base), opts: []NativeOption{WithDeclaredContractVersions([]string{"pa.crd@2.0"})},
			status: http.StatusUnprocessableEntity, msg: "no shared contract line for pa.pas"},
		"inquiry Claim naming no patient": {leg: "pas-claim-inquire", body: bytes.Replace(inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), []byte(`"patient":{"reference":"Patient/MBR-COVERED"},`), nil, 1),
			status: http.StatusBadRequest, msg: "PAS inquiry Claim missing patient"},
		"inquiry unknown member, known members required": {leg: "pas-claim-inquire", requireKnown: true, body: inquiryBundle("MBR-NOBODY", "", "TRN-1", "72148"),
			status: http.StatusBadRequest, msg: "unknown member"},
	})
}

// A payer answer that repeats a member name is read one way only: it refuses
// at every level with the status and body strict has always given, and
// nothing is written from it.
func TestLevelPayer_AnswerDuplicateKeyRefusesAtEveryLevel(t *testing.T) {
	dupPAS := []byte(strings.Replace(assemblyRealPending, `"type":"collection"`, `"type":"collection","type":"collection"`, 1))
	dupInquiry := bytes.Replace(decidedAnswer(t), []byte(`"type": "collection"`), []byte(`"type": "collection", "Type": "collection"`), 1)
	if bytes.Equal(dupInquiry, decidedAnswer(t)) {
		t.Fatal("fixture: the inquiry answer gained no repeated member")
	}
	dupPackage := []byte(`{"resourceType":"Bundle","type":"collection","type":"collection","entry":[]}`)
	dupNext := bytes.Replace(nextQuestionAnswer(t, "Patient/"+dtrFrameMember, rawItems(t, adaptiveTree(t, "1"))), []byte(`"resourceType":"Parameters"`), []byte(`"resourceType":"Parameters","resourceType":"Parameters"`), 1)
	rows := map[string]struct {
		leg, operation, path string
		body, answer         []byte
		status               int
		msg                  string
	}{
		"PAS answer": {"pas-claim", "", pasSubmitPath, originatorBuiltConformantBundle(t, "MBR-COVERED"), dupPAS,
			http.StatusBadGateway, "invalid native PAS response Bundle"},
		"inquiry answer": {"pas-claim-inquire", "", pasInquirePath, inquiryBundle("MBR-COVERED", "", "TRN-1", "72148"), dupInquiry,
			http.StatusBadGateway, "invalid prior-authorization inquiry answer"},
		"package answer": {"dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, packagePath, dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember))), dupPackage,
			http.StatusForbidden, "response repeats a member name"},
		"next-question answer": {"dtr-questionnaire-fetch", shnsdk.FrameOperationNextQuestion, nextPath, []byte(nextQuestionQR(dtrFrameMember)), dupNext,
			http.StatusBadGateway, "next-question response is not a questionnaire-response"},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				p := newLevelPayer(t, level)
				p.seedInquiryPend(t, "corr-submit-1")
				p.partner.respByPath[row.path] = row.answer
				got := p.send(t, row.leg, row.operation, row.body)
				if p.partner.lastPath != row.path {
					t.Fatalf("the request must be forwarded to %s, got %q", row.path, p.partner.lastPath)
				}
				if got.status != row.status || !strings.Contains(string(got.body), row.msg) {
					t.Fatalf("answer = %d %s, want %d naming %q", got.status, got.body, row.status, row.msg)
				}
				if fs := p.content(); len(fs) != 0 || len(p.skipped) != 0 {
					t.Fatalf("a network rule records and skips nothing: %v %+v", findingsText(fs), p.skipped)
				}
				if _, found := p.pendOf(t, got.corr); found {
					t.Fatal("nothing may be written from a refused answer")
				}
				if rec, _ := p.pendOf(t, "corr-submit-1"); rec.State != PendStatePended || p.eobCount() != 0 {
					t.Fatalf("nothing may be decided from a refused answer: %+v, %d EOB(s)", rec, p.eobCount())
				}
			})
		}
	}
}

// A token whose subject is another patient's reaches the payer's own system at
// every level, and its answer is relayed, never refused at the payer's bind.
func TestLevelPayer_TokenForAnotherPatientIsAccepted(t *testing.T) {
	sel := conformantCRD("MBR-COVERED", "72148")
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			p := newLevelPayer(t, level)
			other, _, ok := p.store.ResolvePatient(dtrOtherMember)
			if !ok {
				t.Fatalf("%s is not in the test system of record", dtrOtherMember)
			}
			got := p.sendAs(t, "crd-order-select", "", sel, other)
			if got.status != http.StatusOK {
				t.Fatalf("answer = %d %s, want the payer's own answer relayed", got.status, got.body)
			}
		})
	}
}
