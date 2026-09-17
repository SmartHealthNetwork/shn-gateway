package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
)

const (
	lfPAS              = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
	lfDTR              = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"
	lfTypeSystem       = "http://terminology.hl7.org/CodeSystem/claim-type"
	lfAdditionalSystem = "http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes"
)

type lfObject = map[string]any

func lfDecode(t *testing.T, b []byte) lfObject {
	t.Helper()
	var m lfObject
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func lfBytes(t *testing.T, m lfObject) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func lfAt(m lfObject, key string, i int) lfObject { return m[key].([]any)[i].(map[string]any) }
func lfConcept(system, code string) lfObject {
	return lfObject{"coding": []any{lfObject{"system": system, "code": code}}}
}
func lfExt(url string) lfObject {
	return lfObject{"url": url, "valueReference": lfObject{"reference": "Coverage/synthetic"}}
}
func lfClaim(t *testing.T) lfObject {
	t.Helper()
	b := lfDecode(t, pasGolden(t, "2.2/conformant/pas-submit-request.json"))
	for _, e := range b["entry"].([]any) {
		r := e.(map[string]any)["resource"].(map[string]any)
		if r["resourceType"] == "Claim" {
			return r
		}
	}
	t.Fatal("fixture has no Claim")
	return nil
}
func lfRelated(t *testing.T) lfObject {
	c := lfClaim(t)
	c["related"] = []any{lfObject{"claim": lfObject{"reference": "Claim/prior"}, "relationship": lfConcept("http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship", "prior")}, lfObject{"claim": lfObject{"reference": "Claim/prior-second"}, "relationship": lfConcept("http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship", "prior")}}
	return c
}
func lfAdditional(t *testing.T) lfObject {
	c := lfClaim(t)
	// Report type copied from the PAS referral authorization example.
	c["supportingInfo"] = []any{lfObject{"sequence": 1, "category": lfConcept(lfAdditionalSystem, "additionalInformation"), "valueReference": lfObject{"reference": "DocumentReference/synthetic"}, "extension": []any{lfObject{"url": lfPAS + "extension-documentInformation", "extension": []any{lfObject{"url": "reportTypeCode", "valueCodeableConcept": lfConcept("https://codesystem.x12.org/005010/755", "PY")}}}}}}
	return c
}
func lfQR(t *testing.T) lfObject {
	t.Helper()
	return lfObject{"resourceType": "QuestionnaireResponse", "meta": lfObject{"profile": []any{lfDTR + "dtr-questionnaireresponse"}}, "status": "completed", "questionnaire": "https://example.org/Questionnaire/synthetic", "subject": lfObject{"reference": "Patient/synthetic"}, "extension": []any{lfExt(lfDTR + "qr-context"), lfExt(lfDTR + "qr-context"), lfExt(lfDTR + "qr-coverage")}, "item": []any{lfObject{"linkId": "synthetic", "answer": []any{lfObject{"valueString": "synthetic", "extension": []any{lfObject{"url": lfDTR + "information-origin", "extension": []any{lfObject{"url": "source", "valueCode": "manual"}}}}}}}}}
}
func lfOrigin(q lfObject) lfObject {
	return lfAt(lfAt(lfAt(q, "item", 0), "answer", 0), "extension", 0)
}
func lfResponse(t *testing.T) lfObject {
	return lfObject{"resourceType": "ClaimResponse", "request": lfObject{"reference": "Claim/synthetic"}, "outcome": "complete"}
}
func lfBundle(r lfObject) lfObject {
	return lfObject{"resourceType": "Bundle", "type": "collection", "identifier": lfObject{"system": "https://example.org/bundles", "value": "synthetic"}, "entry": []any{lfObject{"resource": r}, lfObject{"resource": lfObject{"resourceType": "Patient", "id": "synthetic"}}}}
}
func lfRemoveExt(m lfObject, url string) {
	a := m["extension"].([]any)
	var b []any
	for _, e := range a {
		if e.(map[string]any)["url"] != url {
			b = append(b, e)
		}
	}
	m["extension"] = b
}
func lfCheck(t *testing.T, line string, m lfObject, profile string, paths ...string) {
	t.Helper()
	b := lfBytes(t, m)
	before := append([]byte(nil), b...)
	r, err := NewLineFakeValidator(line).Validate(context.Background(), b, profile)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, p := range paths {
		want = append(want, "line "+line+": "+p)
	}
	if r.Valid != (len(want) == 0) || !reflect.DeepEqual(r.Issues, want) {
		t.Fatalf("valid=%v issues=%v; want valid=%v issues=%v", r.Valid, r.Issues, len(want) == 0, want)
	}
	if !reflect.DeepEqual(b, before) {
		t.Fatal("validator changed caller payload")
	}
	if profile == "" && !reflect.DeepEqual(lineMinimaIssues(line, b), want) {
		t.Fatal("pure helper differs from validator")
	}
}

func TestLineFakeGuardMutations(t *testing.T) {
	type row struct {
		name                   string
		lines                  []string
		base                   func(*testing.T) lfObject
		profile, path, earlier string
		mutate                 func(lfObject)
	}
	rows := []row{
		{"item certification", []string{"2.1", "2.2"}, lfClaim, "", "Claim.item[0].extension:certificationType", "2.0", func(m lfObject) { lfRemoveExt(lfAt(m, "item", 0), lfPAS+"extension-certificationType") }},
		{"item request", []string{"2.1", "2.2"}, lfClaim, "", "Claim.item[0].extension:requestType", "2.0", func(m lfObject) { lfRemoveExt(lfAt(m, "item", 0), lfPAS+"extension-serviceItemRequestType") }},
		{"item location", []string{"2.1", "2.2"}, lfClaim, "", "Claim.item[0].location[x]", "2.0", func(m lfObject) { delete(lfAt(m, "item", 0), "locationCodeableConcept") }},
		{"update relationship", []string{"2.1", "2.2"}, lfRelated, lfPAS + "profile-claim-update", "Claim.related[1].relationship", "2.0", func(m lfObject) { delete(lfAt(m, "related", 1), "relationship") }},
		{"claim type wrong system", []string{"2.2"}, lfClaim, "", "Claim.type", "2.1", func(m lfObject) { m["type"] = lfConcept("https://example.org/local", "professional") }},
		{"claim type pharmacy", []string{"2.2"}, lfClaim, "", "Claim.type", "2.1", func(m lfObject) { m["type"] = lfConcept(lfTypeSystem, "pharmacy") }},
		{"claim type no coding", []string{"2.2"}, lfClaim, "", "Claim.type", "2.1", func(m lfObject) { m["type"] = lfObject{"text": "professional"} }},
		{"claim type missing", []string{"2.2"}, lfClaim, "", "Claim.type", "2.1", func(m lfObject) { delete(m, "type") }},
		{"request ClaimFirst order", []string{"2.2"}, func(t *testing.T) lfObject { return lfBundle(lfClaim(t)) }, lfPAS + "profile-pas-request-bundle", "Bundle.ClaimFirst", "2.1", func(m lfObject) { a := m["entry"].([]any); a[0], a[1] = a[1], a[0] }},
		{"request ClaimFirst empty", []string{"2.2"}, func(t *testing.T) lfObject { return lfBundle(lfClaim(t)) }, lfPAS + "profile-pas-request-bundle", "Bundle.ClaimFirst", "2.1", func(m lfObject) { m["entry"] = []any{} }},
		{"document missing", []string{"2.2"}, lfAdditional, "", "Claim.supportingInfo[0].extension:documentInformation", "2.1", func(m lfObject) { delete(lfAt(m, "supportingInfo", 0), "extension") }},
		{"document duplicate", []string{"2.2"}, lfAdditional, "", "Claim.supportingInfo[0].extension:documentInformation", "2.1", func(m lfObject) {
			i := lfAt(m, "supportingInfo", 0)
			i["extension"] = append(i["extension"].([]any), lfAt(i, "extension", 0))
		}},
		{"response request", []string{"2.1", "2.2"}, lfResponse, "", "ClaimResponse.request", "2.0", func(m lfObject) { delete(m, "request") }},
		{"outcome queued", []string{"2.2"}, lfResponse, "", "ClaimResponse.outcome", "2.1", func(m lfObject) { m["outcome"] = "queued" }},
		{"outcome missing", []string{"2.2"}, lfResponse, "", "ClaimResponse.outcome", "2.1", func(m lfObject) { delete(m, "outcome") }},
		{"outcome unknown", []string{"2.2"}, lfResponse, "", "ClaimResponse.outcome", "2.1", func(m lfObject) { m["outcome"] = "not-an-outcome" }},
		{"response identifier", []string{"2.2"}, func(t *testing.T) lfObject { return lfBundle(lfResponse(t)) }, lfPAS + "profile-pas-response-bundle", "Bundle.identifier", "2.1", func(m lfObject) { delete(m, "identifier") }},
		{"QR context", []string{"2.0", "2.1"}, lfQR, "", "QuestionnaireResponse.extension:qr-context", "", func(m lfObject) { a := m["extension"].([]any); m["extension"] = a[1:] }},
		{"QR items", []string{"2.0", "2.1"}, lfQR, "", "QuestionnaireResponse.item", "", func(m lfObject) { delete(m, "item") }},
		{"QR coverage", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.extension:qr-coverage", "2.1", func(m lfObject) { lfRemoveExt(m, lfDTR+"qr-coverage") }},
		{"origin missing source", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.item[0].answer[0].extension:information-origin.extension:source", "2.1", func(m lfObject) { delete(lfOrigin(m), "extension") }},
		{"origin duplicate source", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.item[0].answer[0].extension:information-origin.extension:source", "2.1", func(m lfObject) {
			o := lfOrigin(m)
			o["extension"] = append(o["extension"].([]any), lfAt(o, "extension", 0))
		}},
		{"origin missing code", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.item[0].answer[0].extension:information-origin.extension:source.valueCode", "2.1", func(m lfObject) { delete(lfAt(lfOrigin(m), "extension", 0), "valueCode") }},
		{"origin wrong primitive", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.item[0].answer[0].extension:information-origin.extension:source.valueCode", "2.1", func(m lfObject) {
			s := lfAt(lfOrigin(m), "extension", 0)
			delete(s, "valueCode")
			s["valueString"] = "manual"
		}},
		{"origin unknown code", []string{"2.2"}, lfQR, "", "QuestionnaireResponse.item[0].answer[0].extension:information-origin.extension:source.valueCode", "2.1", func(m lfObject) { lfAt(lfOrigin(m), "extension", 0)["valueCode"] = "auto" }},
	}
	for _, r := range rows {
		for _, line := range r.lines {
			t.Run(r.name+"/"+line, func(t *testing.T) {
				m := r.base(t)
				lfCheck(t, line, m, r.profile)
				r.mutate(m)
				lfCheck(t, line, m, r.profile, r.path)
				if r.earlier != "" {
					lfCheck(t, r.earlier, m, r.profile)
					if r.earlier == "2.1" {
						lfCheck(t, "2.0", m, r.profile)
					}
				}
			})
		}
	}
}

func TestFakeValidatorRefusesWrongLine(t *testing.T) {
	q := lfQR(t)
	q["extension"] = q["extension"].([]any)[1:]
	lfCheck(t, "2.2", q, "")
	lfCheck(t, "2.1", q, "", "QuestionnaireResponse.extension:qr-context")
	c := lfClaim(t)
	i := lfAt(c, "item", 0)
	lfRemoveExt(i, lfPAS+"extension-certificationType")
	lfRemoveExt(i, lfPAS+"extension-serviceItemRequestType")
	delete(i, "locationCodeableConcept")
	lfCheck(t, "2.0", c, "")
	lfCheck(t, "2.1", c, "", "Claim.item[0].extension:certificationType", "Claim.item[0].extension:requestType", "Claim.item[0].location[x]")
	r := lfResponse(t)
	r["outcome"] = "queued"
	lfCheck(t, "2.1", r, "")
	lfCheck(t, "2.2", r, "", "ClaimResponse.outcome")
	q = lfQR(t)
	delete(q, "item")
	lfCheck(t, "2.2", q, "")
	lfCheck(t, "2.1", q, "", "QuestionnaireResponse.item")
}

func TestFakeValidatorToleratesUnknownExtension(t *testing.T) {
	unknown := lfObject{"url": "https://example.org/StructureDefinition/future", "valueString": "preserved"}
	// Uninterpreted extension content must not acquire float64 range limits.
	lfCheck(t, "2.2", lfObject{"resourceType": "Patient", "extension": []any{lfObject{"url": "https://example.org/StructureDefinition/future", "valueDecimal": json.Number("1e999")}}}, "")
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for _, kind := range []string{"QR", "Claim", "Claim.item"} {
			t.Run(line+"/"+kind, func(t *testing.T) {
				var m, target lfObject
				if kind == "QR" {
					m = lfQR(t)
					target = m
				} else {
					m = lfClaim(t)
					target = m
					if kind == "Claim.item" {
						target = lfAt(m, "item", 0)
					}
				}
				lfCheck(t, line, m, "")
				a, _ := target["extension"].([]any)
				target["extension"] = append(a, unknown)
				lfCheck(t, line, m, "")
			})
		}
	}
	q := lfQR(t)
	q["extension"] = q["extension"].([]any)[1:]
	lfCheck(t, "2.1", q, "", "QuestionnaireResponse.extension:qr-context")
	q["extension"] = append(q["extension"].([]any), unknown)
	lfCheck(t, "2.1", q, "", "QuestionnaireResponse.extension:qr-context")
	lfCheck(t, "2.2", q, "")
}

func TestFakeValidatorAcceptsEachLineOwnShape(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			c := lfClaim(t)
			q := lfQR(t)
			r := lfResponse(t)
			if line == "2.0" {
				i := lfAt(c, "item", 0)
				delete(i, "extension")
				delete(i, "locationCodeableConcept")
				delete(r, "request")
			}
			if line != "2.2" {
				r["outcome"] = "queued"
				lfRemoveExt(q, lfDTR+"qr-coverage")
				lfAt(lfOrigin(q), "extension", 0)["valueCode"] = "auto"
			} else {
				lfRemoveExt(q, lfDTR+"qr-context")
			}
			for _, m := range []lfObject{c, q, r, lfBundle(c), lfBundle(r)} {
				lfCheck(t, line, m, "")
			}
		})
	}
}

func TestLineFakeRequiredBindingAllowsTranslations(t *testing.T) {
	for _, code := range []string{"institutional", "professional", "oral"} {
		c := lfClaim(t)
		c["type"] = lfConcept(lfTypeSystem, code)
		c["type"].(map[string]any)["coding"] = append([]any{lfObject{"system": "https://example.org/local", "code": "local"}}, c["type"].(map[string]any)["coding"].([]any)...)
		lfCheck(t, "2.2", c, "")
	}
	for _, code := range []string{"auto-client", "auto-server", "override", "manual"} {
		q := lfQR(t)
		lfAt(lfOrigin(q), "extension", 0)["valueCode"] = code
		lfCheck(t, "2.2", q, "")
	}
	for _, outcome := range []string{"complete", "error", "partial"} {
		r := lfResponse(t)
		r["outcome"] = outcome
		lfCheck(t, "2.2", r, "")
	}
	c := lfAdditional(t)
	cat := lfAt(c, "supportingInfo", 0)["category"].(map[string]any)
	cat["coding"] = append([]any{lfObject{"system": "https://example.org/local", "code": "local"}}, cat["coding"].([]any)...)
	lfCheck(t, "2.2", c, "")
	delete(lfAt(c, "supportingInfo", 0), "extension")
	lfCheck(t, "2.2", c, "", "Claim.supportingInfo[0].extension:documentInformation")
}

func TestLineFakeConditionalContexts(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		c := lfAdditional(t)
		info := lfAt(c, "supportingInfo", 0)
		delete(info, "extension")
		info["category"] = lfConcept("https://example.org/local", "additionalInformation")
		lfCheck(t, line, c, "")
		delete(c, "supportingInfo")
		lfCheck(t, line, c, "")
		c = lfRelated(t)
		delete(lfAt(c, "related", 1), "relationship")
		lfCheck(t, line, c, lfPAS+"profile-claim")
		q := lfQR(t)
		delete(lfAt(lfAt(q, "item", 0), "answer", 0), "extension")
		lfCheck(t, line, q, "")
		// Fields outside the recognized resource contexts are deliberately uninterpreted.
		lfCheck(t, line, lfObject{"resourceType": "Patient", "item": []any{lfObject{}}, "extension": []any{lfObject{"url": lfDTR + "information-origin"}}}, "")
	}
	// A response profile wins even when a Claim is present after a Patient.
	b := lfBundle(lfResponse(t))
	b["entry"] = append(b["entry"].([]any), lfObject{"resource": lfClaim(t)})
	lfCheck(t, "2.2", b, lfPAS+"profile-pas-response-bundle")
	lfCheck(t, "2.2", lfObject{"resourceType": "Bundle", "entry": []any{}}, "")
	c := lfRelated(t)
	delete(lfAt(c, "related", 1), "relationship")
	lfCheck(t, "2.1", c, "", "Claim.related[1].relationship")
	lfCheck(t, "2.1", lfBundle(c), "", "Claim.related[1].relationship")
	b = lfBundle(lfClaim(t))
	a := b["entry"].([]any)
	a[0], a[1] = a[1], a[0]
	lfCheck(t, "2.2", b, "", "Bundle.ClaimFirst")
	b = lfBundle(lfResponse(t))
	delete(b, "identifier")
	lfCheck(t, "2.2", b, "", "Bundle.identifier")
}

func TestLineFakeNestedAnswers(t *testing.T) {
	for _, nest := range []string{"item", "answer"} {
		t.Run(nest, func(t *testing.T) {
			q := lfQR(t)
			inner := lfAt(q, "item", 0)
			if nest == "item" {
				q["item"] = []any{lfObject{"linkId": "group", "item": []any{inner}}}
			} else {
				q["item"] = []any{lfObject{"linkId": "group", "answer": []any{lfObject{"valueString": "group", "item": []any{inner}}}}}
			}
			lfCheck(t, "2.2", q, "")
			origin := lfAt(lfAt(inner, "answer", 0), "extension", 0)
			lfAt(origin, "extension", 0)["valueCode"] = "auto"
			path := "QuestionnaireResponse.item[0].item[0]"
			if nest == "answer" {
				path = "QuestionnaireResponse.item[0].answer[0].item[0]"
			}
			lfCheck(t, "2.2", q, "", path+".answer[0].extension:information-origin.extension:source.valueCode")
			lfCheck(t, "2.1", q, "")
		})
	}
}

func TestLineFakeCallSnapshotsIsolated(t *testing.T) {
	v := NewLineFakeValidator("2.2")
	q := lfQR(t)
	delete(q, "extension")
	b := lfBytes(t, q)
	r, err := v.Validate(context.Background(), b, lfDTR+"dtr-questionnaireresponse|2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Issues) != 1 {
		t.Fatalf("missing refusal issue: %+v", r)
	}
	r.Issues[0] = "changed result"
	for i := range b {
		b[i] = ' '
	}
	calls := v.Calls()
	want := []LineFakeCall{{ResourceType: "QuestionnaireResponse", Profile: lfDTR + "dtr-questionnaireresponse|2.2.0", Valid: false, Issues: []string{"line 2.2: QuestionnaireResponse.extension:qr-coverage"}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%+v", calls)
	}
	calls[0].Issues[0] = "changed snapshot"
	calls[0].Profile = "changed profile"
	calls = append(calls, LineFakeCall{})
	if !reflect.DeepEqual(v.Calls(), want) {
		t.Fatal("snapshot aliases recorder")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.Validate(context.Background(), []byte(`{"resourceType":"Patient"}`), "")
			v.Calls()
		}()
	}
	wg.Wait()
	if len(v.Calls()) != 13 {
		t.Fatal("lost concurrent calls")
	}
	for _, call := range v.Calls()[1:] {
		if call.ResourceType != "Patient" || call.Profile != "" || !call.Valid || len(call.Issues) != 0 {
			t.Fatalf("success call = %+v", call)
		}
	}
	v.Line = "2.0"
	r, err = v.Validate(context.Background(), lfBytes(t, q), "")
	if err != nil || r.Valid {
		t.Fatal("public metadata changed constructor-fixed behavior")
	}
}

func TestLineFakeInvalidJSONAndUnknownLine(t *testing.T) {
	for _, raw := range []string{"", `{"resourceType":`, `{"resourceType":"Patient"} {}`, `{"resourceType":"Patient"} junk`, `[]`, `null`, `true`} {
		t.Run(raw, func(t *testing.T) {
			v := NewLineFakeValidator("2.2")
			r, err := v.Validate(context.Background(), []byte(raw), "test-profile")
			if err == nil || r.Valid {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			if len(v.Calls()) != 1 || v.Calls()[0].Valid {
				t.Fatal("error call not recorded")
			}
			if len(lineMinimaIssues("2.2", []byte(raw))) == 0 {
				t.Fatal("pure helper accepted invalid document")
			}
		})
	}
	for _, line := range []string{"", "9.9", "2.2.1"} {
		v := NewLineFakeValidator(line)
		r, err := v.Validate(context.Background(), []byte(`{"resourceType":"Patient"}`), "")
		if err == nil || r.Valid {
			t.Fatalf("line=%q result=%+v err=%v", line, r, err)
		}
		if len(lineMinimaIssues(line, []byte(`{"resourceType":"Patient"}`))) == 0 {
			t.Fatal("pure helper accepted unsupported line")
		}
	}
}

func TestLineFakeMixedBundleProfilePrecedence(t *testing.T) {
	for _, firstResponse := range []bool{true, false} {
		t.Run(map[bool]string{true: "response first", false: "Patient first"}[firstResponse], func(t *testing.T) {
			b := lfBundle(lfResponse(t))
			entries := b["entry"].([]any)
			if !firstResponse {
				entries[0], entries[1] = entries[1], entries[0]
			}
			b["entry"] = append(entries, lfObject{"resource": lfClaim(t)})
			lfCheck(t, "2.2", b, "")
			lfCheck(t, "2.2", b, lfPAS+"profile-pas-response-bundle")
			lfCheck(t, "2.2", b, lfPAS+"profile-pas-request-bundle", "Bundle.ClaimFirst")
			delete(b, "identifier")
			lfCheck(t, "2.2", b, "", "Bundle.identifier")
			// Explicit request semantics do not inherit the response identifier guard.
			lfCheck(t, "2.2", b, lfPAS+"profile-pas-request-bundle|2.2.1", "Bundle.ClaimFirst")
		})
	}
}
