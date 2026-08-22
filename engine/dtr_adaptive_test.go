package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const testAdaptiveCanonical = "http://example.org/fhir/Questionnaire/Adaptive"

// adaptiveTree is a two-group adaptive questionnaire tree: group 1 delivered, group 3
// enableWhen-gated on 1.1 (the HomeHealthAssessment's shape, trimmed).
func adaptiveTree(t *testing.T, groups ...string) []byte {
	t.Helper()
	items := []map[string]any{}
	for _, g := range groups {
		switch g {
		case "1":
			items = append(items, map[string]any{"linkId": "1", "type": "group", "item": []map[string]any{
				{"linkId": "1.1", "type": "choice", "required": true},
			}})
		case "3":
			items = append(items, map[string]any{"linkId": "3", "type": "group", "item": []map[string]any{
				{"linkId": "3.1", "type": "string", "required": true},
			}})
		}
	}
	raw, err := json.Marshal(map[string]any{
		"resourceType": "Questionnaire", "url": testAdaptiveCanonical, "status": "active",
		"extension": []map[string]any{{"url": sdcQuestionnaireAdaptiveExt, "valueUrl": "http://example.org/$next-question"}},
		"item":      items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rawItems(t *testing.T, tree []byte) []json.RawMessage {
	t.Helper()
	var q struct {
		Item []json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(tree, &q); err != nil {
		t.Fatal(err)
	}
	return q.Item
}

func nextQuestionAnswer(t *testing.T, subject string, items []json.RawMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{{"name": "questionnaire-response", "resource": map[string]any{
			"resourceType": "QuestionnaireResponse", "status": "in-progress",
			"subject":   map[string]string{"reference": subject},
			"contained": []map[string]any{{"resourceType": "Questionnaire", "id": nextQuestionContainedID, "item": items}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestQuestionnaireIsAdaptive(t *testing.T) {
	if !questionnaireIsAdaptive(adaptiveTree(t, "1")) {
		t.Fatal("a questionnaire carrying the SDC questionnaireAdaptive extension must read adaptive")
	}
	if questionnaireIsAdaptive(shnsdk.SandboxLumbarQuestionnaire()) {
		t.Fatal("the sandbox lumbar questionnaire must NOT read adaptive (it would drive $next-question on the sandbox lane)")
	}
	if questionnaireIsAdaptive([]byte(`not json`)) {
		t.Fatal("malformed bytes must not read adaptive")
	}
}

// TestBuildNextQuestionQR pins the SDC adaptive request shape the reference payer reads: the
// delivered tree CONTAINED under the canonical id, derivedFrom the source canonical, with
// its own distinct url; the response in progress, pointing at the contained questionnaire.
func TestBuildNextQuestionQR(t *testing.T) {
	partial := []byte(`{"resourceType":"QuestionnaireResponse","status":"completed","questionnaire":"` + testAdaptiveCanonical + `","item":[{"linkId":"1"}]}`)
	out, err := buildNextQuestionQR(partial, adaptiveTree(t, "1"), testAdaptiveCanonical)
	if err != nil {
		t.Fatal(err)
	}
	var qr struct {
		Status        string `json:"status"`
		Questionnaire string `json:"questionnaire"`
		Contained     []struct {
			ResourceType string   `json:"resourceType"`
			ID           string   `json:"id"`
			URL          string   `json:"url"`
			DerivedFrom  []string `json:"derivedFrom"`
			Item         []struct {
				LinkID string `json:"linkId"`
			} `json:"item"`
		} `json:"contained"`
		Item []any `json:"item"`
	}
	if err := json.Unmarshal(out, &qr); err != nil {
		t.Fatal(err)
	}
	if qr.Status != "in-progress" || qr.Questionnaire != "#"+nextQuestionContainedID {
		t.Fatalf("status=%q questionnaire=%q, want in-progress / #%s", qr.Status, qr.Questionnaire, nextQuestionContainedID)
	}
	if len(qr.Contained) != 1 || qr.Contained[0].ResourceType != "Questionnaire" || qr.Contained[0].ID != nextQuestionContainedID {
		t.Fatalf("contained = %+v, want one Questionnaire id %s", qr.Contained, nextQuestionContainedID)
	}
	c := qr.Contained[0]
	if len(c.DerivedFrom) != 1 || c.DerivedFrom[0] != testAdaptiveCanonical {
		t.Fatalf("contained derivedFrom = %v, want [%s] (the payer resolves the source by it)", c.DerivedFrom, testAdaptiveCanonical)
	}
	if c.URL == testAdaptiveCanonical || c.URL == "" {
		t.Fatalf("contained url = %q, want a url DISTINCT from the source (a self-referential derivedFrom)", c.URL)
	}
	if len(c.Item) != 1 || c.Item[0].LinkID != "1" {
		t.Fatalf("contained items = %+v, want the delivered tree (group 1)", c.Item)
	}
	if len(qr.Item) != 1 {
		t.Fatalf("the partial fill's items must be carried (got %d)", len(qr.Item))
	}
}

// TestParseNextQuestionResponse_Rejections: only a questionnaire-response Parameters whose
// QuestionnaireResponse contains a Questionnaire is an answer — everything else is refused
// (a payer that answered the package op, or nothing, must not read as a delivered group).
func TestParseNextQuestionResponse_Rejections(t *testing.T) {
	good := nextQuestionAnswer(t, "Patient/X", rawItems(t, adaptiveTree(t, "1", "3")))
	qr, items, err := parseNextQuestionResponse(good)
	if err != nil || len(items) != 2 || len(qr) == 0 {
		t.Fatalf("faithful answer: err=%v items=%d", err, len(items))
	}
	for _, tc := range []struct{ name, body, want string }{
		{"package-parameters", `{"resourceType":"Parameters","parameter":[{"name":"packagebundle","resource":{"resourceType":"Bundle","type":"collection"}}]}`, "no questionnaire-response parameter"},
		{"bare-bundle", `{"resourceType":"Bundle","type":"collection","entry":[]}`, "want Parameters"},
		{"not-a-qr", `{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":{"resourceType":"Questionnaire"}}]}`, "not a QuestionnaireResponse"},
		{"no-contained", `{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":{"resourceType":"QuestionnaireResponse","status":"in-progress"}}]}`, "no contained Questionnaire"},
		{"garbage", `garbage`, "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseNextQuestionResponse([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestMergeDeliveredGroups: the delivered set only grows — a new group is appended in the
// payer's order, a re-sent known group keeps the CLIENT's bytes, an empty round reports no
// growth, and an answer that drops a delivered group is refused.
func TestMergeDeliveredGroups(t *testing.T) {
	tree := adaptiveTree(t, "1")
	both := rawItems(t, adaptiveTree(t, "1", "3"))

	merged, grew, err := mergeDeliveredGroups(tree, both)
	if err != nil || !grew {
		t.Fatalf("grow: err=%v grew=%v", err, grew)
	}
	if got := rawItems(t, merged); len(got) != 2 || string(got[0]) != string(rawItems(t, tree)[0]) {
		t.Fatalf("merged items = %d, want 2 with the client's own bytes for group 1 kept", len(got))
	}

	same, grew, err := mergeDeliveredGroups(merged, both)
	if err != nil || grew || string(same) != string(merged) {
		t.Fatalf("empty round: err=%v grew=%v sameBytes=%v, want no growth and the tree untouched", err, grew, string(same) == string(merged))
	}

	if _, _, err := mergeDeliveredGroups(merged, rawItems(t, adaptiveTree(t, "3"))); err == nil || !strings.Contains(err.Error(), `dropped delivered group "1"`) {
		t.Fatalf("dropped group: err=%v, want a refusal naming group 1", err)
	}
}

// TestSandboxResponder_RefusesNextQuestion: the sandbox serves no adaptive questionnaire, so
// a $next-question round is REFUSED (400, named) — never answered with a package the
// originator could mistake for a delivered group. The plain fetch on the same leg still
// serves the package (control).
func TestSandboxResponder_RefusesNextQuestion(t *testing.T) {
	stub := NewStubHolderData()
	clock := func() time.Time { return time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC) }
	responder := NewSandboxResponder(NewSandboxAdjudicator(stub, clock), stub, stub, clock)

	round, err := json.Marshal(dtrLegRequest{Canonical: shnsdk.QuestionnaireCanonicalLumbarMRI, NextQuestion: json.RawMessage(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/MBR-COVERED"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := responder.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", round)
	if err != nil || res.Status != http.StatusBadRequest || !strings.Contains(res.Message, "$next-question") {
		t.Fatalf("next-question round: err=%v status=%d msg=%q, want 400 naming $next-question", err, res.Status, res.Message)
	}

	fetch, err := json.Marshal(dtrLegRequest{Canonical: shnsdk.QuestionnaireCanonicalLumbarMRI})
	if err != nil {
		t.Fatal(err)
	}
	res, err = responder.Handle(context.Background(), "dtr-questionnaire-fetch", "corr", "pci", fetch)
	if err != nil || res.Status != 0 || len(res.ResponseFHIR) == 0 {
		t.Fatalf("plain fetch (control): err=%v status=%d, want the package", err, res.Status)
	}
}

// TestNextQuestionSubjectBindAndFence pins the payer side's two fences on an adaptive round:
// (A) the carried subject resolves to the token subject via the payer's OWN record; (C) the
// answer is about the patient the request carried.
func TestNextQuestionSubjectBindAndFence(t *testing.T) {
	stub := NewStubHolderData()
	g := &Gateway{cfg: Config{SoR: stub}}
	coveredPCI, _, _ := stub.ResolvePatient("MBR-COVERED")
	uc04PCI, _, _ := stub.ResolvePatient("MBR-UC04")

	for _, tc := range []struct {
		name, subject, token string
		wantStatus           int
	}{
		{"match", "Patient/MBR-COVERED", coveredPCI, 0},
		{"other-member", "Patient/MBR-UC04", coveredPCI, http.StatusForbidden},
		{"unknown-member", "Patient/MBR-NOBODY", coveredPCI, http.StatusBadRequest},
		{"no-subject", "", uc04PCI, http.StatusBadRequest},
		{"not-a-patient-ref", "Practitioner/1", uc04PCI, http.StatusBadRequest},
	} {
		t.Run("bind/"+tc.name, func(t *testing.T) {
			if status, _ := g.bindNextQuestionSubject(tc.subject, tc.token); status != tc.wantStatus {
				t.Fatalf("bind(%q) status=%d, want %d", tc.subject, status, tc.wantStatus)
			}
		})
	}

	items := rawItems(t, adaptiveTree(t, "1", "3"))
	if status, _ := fenceNextQuestionSubject("Patient/MBR-COVERED", LegResult{ResponseFHIR: nextQuestionAnswer(t, "Patient/MBR-COVERED", items)}); status != 0 {
		t.Fatalf("fence(same patient) status=%d, want pass", status)
	}
	if status, msg := fenceNextQuestionSubject("Patient/MBR-COVERED", LegResult{ResponseFHIR: nextQuestionAnswer(t, "Patient/MBR-COVERED-FOREIGN", items)}); status != http.StatusForbidden || !strings.Contains(msg, "response patient") {
		t.Fatalf("fence(foreign patient) status=%d msg=%q, want 403", status, msg)
	}
	if status, msg := fenceNextQuestionSubject("Patient/MBR-COVERED", LegResult{ResponseFHIR: []byte(`{"resourceType":"Bundle"}`)}); status != http.StatusBadGateway || !strings.Contains(msg, "questionnaire-response") {
		t.Fatalf("fence(package-shaped) status=%d msg=%q, want 502", status, msg)
	}
	if status, _ := fenceNextQuestionSubject("Patient/MBR-COVERED", LegResult{Status: http.StatusBadRequest, Message: "upstream refused"}); status != 0 {
		t.Fatalf("fence(non-2xx relay) status=%d, want pass-through to respondLegError", status)
	}
}

// TestNextQuestionRequestSubject: the carriage probe reads the adaptive round's subject and
// says false for an ordinary package fetch (so the plain leg is untouched).
func TestNextQuestionRequestSubject(t *testing.T) {
	if _, ok := nextQuestionRequestSubject([]byte(`{"canonical":"x"}`)); ok {
		t.Fatal("a plain package fetch must not read as a next-question round")
	}
	subj, ok := nextQuestionRequestSubject([]byte(`{"canonical":"x","nextQuestion":{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/M"}}}`))
	if !ok || subj != "Patient/M" {
		t.Fatalf("round: ok=%v subject=%q, want Patient/M", ok, subj)
	}
}

// TestUC04AttestationAnswers_SupportingInfo: 3.2 / 3.3 are read from the order's
// supportingInfo through the SoR resolver (ClinicalImpression.summary / Goal.description.text),
// skipping unresolvable or other-typed references; absent ⇒ omitted, never invented.
func TestUC04AttestationAnswers_SupportingInfo(t *testing.T) {
	order := []byte(`{"resourceType":"ServiceRequest","id":"sr-1","status":"active","intent":"order",
		"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"G0151"}]},
		"subject":{"reference":"Patient/MBR-X"},
		"reasonCode":[{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"I63.9","display":"Cerebral infarction"}]}],
		"supportingInfo":[{"reference":"Observation/ignored"},{"reference":"ClinicalImpression/ci-1"},{"reference":"Goal/goal-1"},{"reference":"Goal/missing"}]}`)
	resolve := func(ref string) ([]byte, bool) {
		switch ref {
		case "ClinicalImpression/ci-1":
			return []byte(`{"resourceType":"ClinicalImpression","status":"completed","description":"PT assessment","summary":"Impaired gait"}`), true
		case "Goal/goal-1":
			return []byte(`{"resourceType":"Goal","lifecycleStatus":"active","description":{"text":"Walk independently"}}`), true
		case "Observation/ignored":
			return []byte(`{"resourceType":"Observation","status":"final"}`), true
		}
		return nil, false
	}
	answers, err := uc04AttestationAnswers(order, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if got := answers["3.2"]; got.String == nil || *got.String != "Impaired gait" {
		t.Fatalf("3.2 = %+v, want the ClinicalImpression summary", got)
	}
	if got := answers["3.3"]; got.String == nil || *got.String != "Walk independently" {
		t.Fatalf("3.3 = %+v, want the Goal description text", got)
	}
	// No resolver / nothing resolvable ⇒ 3.2/3.3 absent (the fill's honesty guard then refuses
	// the delivered group — never a fabricated narrative).
	for name, r := range map[string]func(string) ([]byte, bool){"nil": nil, "unresolvable": func(string) ([]byte, bool) { return nil, false }} {
		answers, err := uc04AttestationAnswers(order, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := answers["3.2"]; ok {
			t.Fatalf("%s resolver: 3.2 present, want omitted", name)
		}
		if _, ok := answers["3.3"]; ok {
			t.Fatalf("%s resolver: 3.3 present, want omitted", name)
		}
	}
}
