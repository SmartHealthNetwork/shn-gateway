package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func definitionPackage(t *testing.T, line string) string {
	t.Helper()
	b, err := shnsdk.BuildQuestionnairePackageAtLine(line, []byte(`{"resourceType":"Questionnaire","id":"q","url":"https://fixture.test/Questionnaire/q","status":"active"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDTRDefinitionOnlyApplicability(t *testing.T) {
	for _, line := range []string{"2.0", "2.1"} {
		bundle := definitionPackage(t, line)
		wrapped := `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":` + bundle + `}]}`
		var dependencies map[string]any
		if err := json.Unmarshal([]byte(bundle), &dependencies); err != nil {
			t.Fatal(err)
		}
		entries := dependencies["entry"].([]any)
		for _, kind := range []string{"Library", "ValueSet"} {
			entries = append(entries, map[string]any{"fullUrl": "https://fixture.test/" + kind + "/dependency", "resource": map[string]any{"resourceType": kind, "id": "dependency"}})
		}
		dependencies["entry"] = entries
		withDependencies, _ := json.Marshal(dependencies)
		for _, body := range []string{bundle, wrapped, string(withDependencies)} {
			in := CheckInput{Exchange: ExchangeContext{legType: "dtr-questionnaire-fetch", operation: "questionnaire-package", contractVersion: "pa.dtr@" + line, policy: NewConformancePolicy(EnforcementStrict)}, Direction: "response", Status: 200, DeclaredVersion: "pa.dtr@" + line, Body: []byte(body)}
			g := &Gateway{cfg: Config{Validator: syntheticFakeValidator()}}
			if deepRule(t, g, "patient.consistency").Applies(in) {
				t.Fatalf("definition-only %s still requires patient", line)
			}
			if err := g.enforceContent(context.Background(), in); err != nil {
				t.Fatalf("definition-only %s: %v", line, err)
			}
			g.cfg.Validator = &shnsdk.FakeValidator{}
			if err := g.enforceContent(context.Background(), in); err == nil {
				t.Fatal("identity applicability disabled required profile/terminology checks")
			}
		}
	}
	base := definitionPackage(t, "2.1")
	mutations := []struct{ name, body, version, leg, op, direction string }{
		{"modern requires QR", base, "pa.dtr@2.2", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"unknown line", base, "pa.dtr@9", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"missing declaration", base, "", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"wrong operation", base, "pa.dtr@2.1", "dtr-questionnaire-fetch", "next-question", "response"},
		{"wrong direction", base, "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "request"},
		{"wrong leg", base, "pa.pas@2.1", "pas-claim", "", "response"},
		{"empty bundle", `{"resourceType":"Bundle","type":"collection","entry":[]}`, "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"unknown resource", strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"Unknown"`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"clinical missing patient", strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"QuestionnaireResponse"`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"coverage missing patient", strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"Coverage"`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"foreign assertion", strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"Questionnaire","subject":{"reference":"Patient/foreign"}`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"unresolved assertion", strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"Questionnaire","extension":[{"url":"urn:subject","valueReference":{"reference":"Patient/unresolved"}}]`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"wrong wrapper name", `{"resourceType":"Parameters","parameter":[{"name":"unknown","resource":` + base + `}]}`, "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
		{"missing fullUrl", strings.Replace(base, `"fullUrl":`, `"missing":`, 1), "pa.dtr@2.1", "dtr-questionnaire-fetch", "questionnaire-package", "response"},
	}
	for _, row := range mutations {
		t.Run(row.name, func(t *testing.T) {
			in := CheckInput{Exchange: ExchangeContext{legType: row.leg, operation: row.op, policy: NewConformancePolicy(EnforcementStrict)}, Direction: row.direction, Status: 200, DeclaredVersion: row.version, Body: []byte(row.body)}
			g := &Gateway{}
			if !deepRule(t, g, "patient.consistency").Applies(in) {
				t.Fatal("unsupported content obtained patient applicability exception")
			}
		})
	}
}

func TestDTRPackageBareResponseStructure(t *testing.T) {
	good := definitionPackage(t, "2.0")
	for _, body := range []string{good, `{"resourceType":"Bundle","type":"collection","entry":[]}`} {
		in := structuralInput("dtr-questionnaire-fetch", "questionnaire-package", "response", body)
		if err := (&Gateway{}).enforceRules(context.Background(), in, StructuralRules()); err != nil {
			t.Fatalf("supported bare response: %v", err)
		}
	}
	for _, body := range []string{
		`{"resourceType":"Bundle","entry":[]}`,
		`{"resourceType":"Bundle","type":"searchset","entry":[]}`,
		`{"resourceType":"Bundle","type":"collection","entry":{}}`,
		`{"resourceType":"Bundle","type":"collection","entry":[null]}`,
		`{"resourceType":"Bundle","type":"collection","entry":[{"resource":[]}]}`,
		`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{}}]}`,
	} {
		in := structuralInput("dtr-questionnaire-fetch", "questionnaire-package", "response", body)
		wantStructuralError(t, (&Gateway{}).enforceRules(context.Background(), in, StructuralRules()), 502, "dtr.package.response")
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementObserve} {
			in.Exchange.policy = NewConformancePolicy(level)
			if err := (&Gateway{}).enforceRules(context.Background(), in, StructuralRules()); err != nil {
				t.Fatal(err)
			}
		}
	}
	in := structuralInput("dtr-questionnaire-fetch", "questionnaire-package", "request", good)
	wantStructuralError(t, (&Gateway{}).enforceRules(context.Background(), in, StructuralRules()), 422, "dtr.package.request")
	// Basic accepts an incomplete clinical resource; deeper rules own its semantics.
	in = structuralInput("dtr-questionnaire-fetch", "questionnaire-package", "response", `{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"QuestionnaireResponse"}}]}`)
	if err := (&Gateway{}).enforceContent(context.Background(), in); err != nil {
		t.Fatalf("basic inspected clinical semantics: %v", err)
	}
}

func TestDTRDefinitionOnlyIdentifierReference(t *testing.T) {
	pci, _, _ := newCensusSoR().ResolvePatient("MBR-COVERED")
	for _, line := range []string{"2.0", "2.1"} {
		// Artifact identifiers are ordinary definition metadata, not Patient References.
		base := strings.Replace(definitionPackage(t, line), `"resourceType":"Questionnaire"`, `"resourceType":"Questionnaire","identifier":[{"system":"urn:fixture:questionnaires","value":"q"}]`, 1)
		for _, wrapped := range []bool{false, true} {
			envelope := func(b string) string {
				if wrapped {
					return `{"resourceType":"Parameters","parameter":[{"name":"PackageBundle","resource":` + b + `}]}`
				}
				return b
			}
			in := CheckInput{Exchange: ExchangeContext{holder: "requester", recipient: "payer", subjectPCI: pci, contractVersion: "pa.dtr@" + line, legType: "dtr-questionnaire-fetch", operation: "questionnaire-package", policy: NewConformancePolicy(EnforcementStrict)}, Direction: "response", Status: 200, DeclaredVersion: "pa.dtr@" + line, Body: []byte(envelope(base))}
			g := &Gateway{cfg: Config{Validator: syntheticFakeValidator()}}
			if deepRule(t, g, "patient.consistency").Applies(in) {
				t.Fatal("ordinary artifact identifier lost definition-only applicability")
			}
			if err := g.enforceContent(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			for _, member := range []string{"MBR-NOTCOVERED", "unknown"} {
				for _, assertion := range []string{
					`"author":{"identifier":{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}}`,
					`"extension":[{"url":"urn:fixture:patient","valueReference":{"type":"Patient","identifier":{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}}}]`,
					`"extension":[{"url":"urn:fixture:patient","valueReference":{"identifier":{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}}}]`,
				} {
					body := strings.Replace(base, `"resourceType":"Questionnaire"`, `"resourceType":"Questionnaire",`+assertion, 1)
					in.Body = []byte(envelope(body))
					if !deepRule(t, g, "patient.consistency").Applies(in) {
						t.Errorf("%s wrapped=%t %s: identifier-only assertion exempted", line, wrapped, member)
					}
					for _, resolver := range []SubjectReferenceResolver{nil, censusSubjectResolver("requester", "payer")} {
						g.cfg.SubjectReferenceResolver = resolver
						if got := g.checkSubjectConsistency(context.Background(), in); got.State != CheckUnavailable {
							t.Errorf("resolver configured=%t: %+v", resolver != nil, got)
						}
						if err := g.enforceContent(context.Background(), in); err == nil {
							t.Errorf("%s wrapped=%t %s: other successful checkers certified unsupported identity", line, wrapped, member)
						}
					}
				}
			}
		}
	}
}
