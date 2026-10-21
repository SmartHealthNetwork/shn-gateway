package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The E-08 rows drive an EHR's $questionnaire-package request through a
// provider gateway that declares one DTR line, to a payer whose answer names
// its parameters as another line does. The payer's answer is a real Parameters
// in the payer's own layout (key order, spacing), so a byte comparison shows
// the edit changed the names and nothing else.

// e08Answer is a payer's answer: one package parameter named pkg whose Bundle
// holds entries (resource types), then one outcome parameter per name in
// outcomes. A package that is not a Bundle is written when entries is nil and
// notBundle is set.
func e08Answer(pkg string, entries []string, notBundle bool, outcomes ...string) []byte {
	var b strings.Builder
	b.WriteString("{\n  \"resourceType\" : \"Parameters\",\n  \"parameter\" : [")
	sep := " "
	if pkg != "" {
		b.WriteString(sep + "{ \"name\" : \"" + pkg + "\", \"resource\" : ")
		if notBundle {
			b.WriteString(`{ "resourceType" : "Questionnaire", "status" : "active" }`)
		} else {
			b.WriteString(`{ "resourceType" : "Bundle", "type" : "collection", "entry" : [`)
			for i, rt := range entries {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(` { "resource" : ` + e08Entry(rt, pkg) + ` }`)
			}
			b.WriteString(" ] }")
		}
		b.WriteString(" }")
		sep = ", "
	}
	for _, o := range outcomes {
		b.WriteString(sep + `{ "name" : "` + o + `", "resource" : { "resourceType" : "OperationOutcome", "issue" : [ { "severity" : "information", "code" : "informational" } ] } }`)
		sep = ", "
	}
	b.WriteString(" ]\n}\n")
	return []byte(b.String())
}

// The package entries in the payer's own layout. A QuestionnaireResponse is
// shaped as its package's line shapes one: at 2.0.1 and 2.1.0 its Coverage is a
// qr-context and its intendedUse coding is CRD's temp code system; at 2.2.0 the
// Coverage is qr-coverage and the coding coverage-information-codes.
const (
	e08Questionnaire = `{ "resourceType" : "Questionnaire", "status" : "active", "text" : { "div" : "caf\u00e9" } }`
	e08Library       = `{ "resourceType" : "Library", "status" : "active", "type" : { "text" : "logic-library" } }`
	e08QR20          = `{ "resourceType" : "QuestionnaireResponse", "status" : "in-progress", "extension" : [ ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse", "valueCodeableConcept" : { "coding" : [ { "system" : "http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp", "code" : "withorder" } ] } }, ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context", "valueReference" : { "reference" : "Coverage/c1" } }, ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context", "valueReference" : { "reference" : "ServiceRequest/sr1" } } ], "quantity" : 1.50 }`
	e08QR22 = `{ "resourceType" : "QuestionnaireResponse", "status" : "in-progress", "extension" : [ ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/intendedUse", "valueCodeableConcept" : { "coding" : [ { "system" : "http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes", "code" : "withorder" } ] } }, ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-coverage", "valueReference" : { "reference" : "Coverage/c1" } }, ` +
		`{ "url" : "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-context", "valueReference" : { "reference" : "ServiceRequest/sr1" } } ] }`
)

// e08Entry is the entry resource of type rt in a package named pkg.
func e08Entry(rt, pkg string) string {
	switch rt {
	case "Questionnaire":
		return e08Questionnaire
	case "Library":
		return e08Library
	case "QuestionnaireResponse":
		if pkg == "packagebundle" {
			return e08QR22
		}
		return e08QR20
	}
	return `{ "resourceType" : "` + rt + `" }`
}

// e08Bridged is answer as E-08 carries it from the from line to the to line:
// the names renamed, and each QuestionnaireResponse entry the registered
// pa.dtr chain's output for its own bytes, nothing else changed.
func e08Bridged(t *testing.T, answer []byte, from, to string) []byte {
	t.Helper()
	out := renamedAs(answer, "PackageBundle", "packagebundle", "operationOutcome", "outcome")
	qr := e08QR20
	if to != "2.2" {
		out = renamedAs(answer, "packagebundle", "PackageBundle")
		qr = e08QR22
	}
	if !bytes.Contains(out, []byte(qr)) {
		return out
	}
	stepped, _, err := TransformDTRForTest(from, to, []byte(qr), ExchangeIdentity{CorrelationID: "c"})
	if err != nil {
		t.Fatalf("fixture: the registered chain refuses the QuestionnaireResponse: %v", err)
	}
	return bytes.ReplaceAll(out, []byte(qr), stepped)
}

// renamedAs is answer with each "name" : "<from>" written as "name" : "<to>",
// the only bytes E-08 may change.
func renamedAs(answer []byte, pairs ...string) []byte {
	out := answer
	for i := 0; i+1 < len(pairs); i += 2 {
		out = bytes.ReplaceAll(out, []byte(`"name" : "`+pairs[i]+`"`), []byte(`"name" : "`+pairs[i+1]+`"`))
	}
	return out
}

type e08Result struct {
	env      *inProcessExchange
	rec      *httptest.ResponseRecorder
	findings []ConformanceFinding
	records  []ExchangeRecord
}

// e08Row posts an EHR's package request to a provider gateway declaring
// declared (its DTR lines among them) at level, each line laned by the line
// fake, the payer answering answer.
func e08Row(t *testing.T, level ConformanceEnforcement, declared []string, answer []byte) e08Result {
	t.Helper()
	return e08RowFull(t, level, declared, answer, nil, nil, nil)
}

// e08RowObserved is e08Row with every observer event also given to observe.
func e08RowObserved(t *testing.T, level ConformanceEnforcement, declared []string, answer []byte, observe func(ObserverEvent)) e08Result {
	t.Helper()
	return e08RowFull(t, level, declared, answer, nil, observe, nil)
}

// e08RowWith is e08Row with the line lanes adjusted by lanes.
func e08RowWith(t *testing.T, level ConformanceEnforcement, declared []string, answer []byte, lanes func(map[string]shnsdk.Validator)) e08Result {
	t.Helper()
	return e08RowFull(t, level, declared, answer, lanes, nil, nil)
}

// e08RowFull is e08Row with the lanes adjusted, every observer event given to
// observe, and, when payerLines is set, the payer's registry entry declaring them.
func e08RowFull(t *testing.T, level ConformanceEnforcement, declared []string, answer []byte, lanes func(map[string]shnsdk.Validator), observe func(ObserverEvent), payerLines []string) e08Result {
	t.Helper()
	return e08RowTuned(t, level, declared, answer, lanes, observe, payerLines, nil)
}

// e08RowTuned is e08RowFull with the provider's gateway adjusted by tune before
// the request.
func e08RowTuned(t *testing.T, level ConformanceEnforcement, declared []string, answer []byte, lanes func(map[string]shnsdk.Validator), observe func(ObserverEvent), payerLines []string, tune func(*Gateway)) e08Result {
	t.Helper()
	env := newInProcessExchange(t)
	// E-08 is module 2's: these rows run with it on unless tune turns it off.
	env.originator.cfg.TranslateIGLines = true
	if tune != nil {
		tune(env.originator)
	}
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.DeclaredContractVersions = declared
	byLine := map[string]shnsdk.Validator{
		"2.0": NewLineFakeValidator("2.0"), "2.1": NewLineFakeValidator("2.1"), "2.2": NewLineFakeValidator("2.2"),
	}
	if lanes != nil {
		lanes(byLine)
	}
	env.originator.cfg.ValidatorsByContractLine = contractLanes(byLine)
	r := e08Result{env: env}
	env.originator.cfg.Observer = func(e ObserverEvent) {
		if observe != nil {
			observe(e)
		}
		var f ConformanceFinding
		if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil {
			r.findings = append(r.findings, f)
		}
	}
	env.originator.cfg.ExchangeObserved = func(x ExchangeRecord) { r.records = append(r.records, x) }
	declareFramedDTR(t, env, true)
	if payerLines != nil {
		entry, _ := env.originator.cfg.Reg.Lookup(env.payerID)
		entry.ContractVersions = payerLines
		env.originator.cfg.Reg.Set(env.payerID, entry)
	}
	env.payerReturns(LegResult{Response: testResponse(answer)})
	// Through the ingress route, so the exchange record names the edits made.
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
	req.Header.Set("Content-Type", "application/fhir+json")
	r.rec = httptest.NewRecorder()
	env.originator.ingressRoute(RouteDTR)(r.rec, req)
	env.originator.drainObserveChecks() // findings an observe-level check queued
	return r
}

var (
	declares22   = []string{"pa.crd@2.0", "pa.dtr@2.2", "pa.pas@2.0"}
	declares20   = []string{"pa.crd@2.0", "pa.dtr@2.0", "pa.pas@2.0"}
	declares21   = []string{"pa.crd@2.0", "pa.dtr@2.1", "pa.pas@2.0"}
	declaresBoth = []string{"pa.crd@2.0", "pa.dtr@2.0", "pa.dtr@2.2", "pa.pas@2.0"}
)

// recordedEdits is the edits the provider's exchange record names.
func (r e08Result) recordedEdits() []string {
	var edits []string
	for _, x := range r.records {
		edits = append(edits, x.Edits...)
	}
	return edits
}

func (r e08Result) wantAnswered(t *testing.T, want []byte, edited bool) {
	t.Helper()
	if r.rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
	}
	if !bytes.Equal(r.rec.Body.Bytes(), want) {
		t.Fatalf("the EHR received\n%s\nwant\n%s", r.rec.Body.Bytes(), want)
	}
	if leg, outcome := lastOutcome(t, r.env.originator); leg != dtrLeg || outcome != "ok" {
		t.Fatalf("recorded %s %s", leg, outcome)
	}
	if got := slices.Contains(r.recordedEdits(), string(relay.EditDTRAnswerLineNames)); got != edited {
		t.Fatalf("E-08 recorded=%v, want %v (edits %v)", got, edited, r.recordedEdits())
	}
	// E-08 is an edit to the answer: it is the answer's, never the request's,
	// and the answer the EHR received is the payer's, edited only by it.
	wantOwn := relay.OwnershipRelayed.String()
	if edited {
		wantOwn = relay.OwnershipEdited.String()
	}
	var answers []string
	for _, x := range r.records {
		if slices.Contains(x.RequestEdits, string(relay.EditDTRAnswerLineNames)) {
			t.Fatalf("E-08 recorded as an edit to the request: %v", x.RequestEdits)
		}
		if x.Direction == DirectionIngress {
			answers = append(answers, x.AnswerOwnership)
			if got := slices.Contains(x.AnswerEdits, string(relay.EditDTRAnswerLineNames)); got != edited || x.AnswerBuilder != "" {
				t.Fatalf("answer edits %v builder %q, want E-08 %v and no builder", x.AnswerEdits, x.AnswerBuilder, edited)
			}
		}
	}
	if !slices.Equal(answers, []string{wantOwn}) {
		t.Fatalf("ingress answer ownership %v, want [%s]", answers, wantOwn)
	}
}

// Both directions, at every level: the names the requester's line gives the
// package and the outcome, and nothing else changed.
func TestDTRAnswerBridge_RenamesToTheRequestersLine(t *testing.T) {
	q, qr := "Questionnaire", "QuestionnaireResponse"
	for _, row := range []struct {
		name     string
		declared []string
		answer   []byte
		want     []byte
	}{
		{"2.0 package and outcome to a 2.2 provider", declares22,
			e08Answer("PackageBundle", []string{q, qr}, false, "operationOutcome"), nil},
		{"2.0 package only to a 2.2 provider", declares22,
			e08Answer("PackageBundle", []string{q, qr, "Library"}, false), nil},
		{"2.1 outcome only to a 2.2 provider", declares22,
			e08Answer("", nil, false, "operationOutcome"), nil},
		{"2.2 package and outcome to a 2.0 provider", declares20,
			e08Answer("packagebundle", []string{q, qr}, false, "outcome"), nil},
		{"2.2 package only to a 2.1 provider", declares21,
			e08Answer("packagebundle", []string{q, qr}, false), nil},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				from, to := "2.0", "2.2"
				switch row.declared[1] {
				case "pa.dtr@2.0":
					from, to = "2.2", "2.0"
				case "pa.dtr@2.1":
					from, to = "2.2", "2.1"
				}
				want := e08Bridged(t, row.answer, from, to)
				if bytes.Equal(want, row.answer) {
					t.Fatal("fixture: nothing to rename")
				}
				e08Row(t, level, row.declared, row.answer).wantAnswered(t, want, true)
			})
		}
	}
}

// No edit: the answer reaches the EHR exactly as the payer sent it.
func TestDTRAnswerBridge_CarriesExactly(t *testing.T) {
	q, qr := "Questionnaire", "QuestionnaireResponse"
	for _, row := range []struct {
		name     string
		declared []string
		answer   []byte
	}{
		{"same line 2.2", declares22, e08Answer("packagebundle", []string{q, qr}, false, "outcome")},
		{"same line 2.0", declares20, e08Answer("PackageBundle", []string{q}, false, "operationOutcome")},
		{"2.0.1 names to a 2.1 provider: one name family", declares21, e08Answer("PackageBundle", []string{q}, false, "operationOutcome")},
		{"a provider declaring two DTR lines", declaresBoth, e08Answer("PackageBundle", []string{q, qr}, false, "operationOutcome")},
		{"names of both families", declares22, e08Answer("PackageBundle", []string{q, qr}, false, "outcome", "packagebundle")},
		{"2.2 outcome only to a 2.0 provider: no line-specific name", declares20, e08Answer("", nil, false, "outcome")},
		{"return is not bridged", declares22, renamedAs(e08Answer("PackageBundle", []string{q, qr}, false), "PackageBundle", "return")},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				e08Row(t, level, row.declared, row.answer).wantAnswered(t, row.answer, false)
			})
		}
	}
}

// A renamed answer that is not valid at the requester's line is refused at
// every level, as the fidelity of SHN's own IG-line edit: the bridge never
// fills. The refusal says the payer answered; the finding carries the hash of
// the payer's answer and of the renamed one, and names E-08.
func TestDTRAnswerBridge_RefusesWhatItCannotCarry(t *testing.T) {
	q, qr := "Questionnaire", "QuestionnaireResponse"
	for _, row := range []struct {
		name   string
		answer []byte
		why    string
	}{
		{"a 2.0 package without its QuestionnaireResponse", e08Answer("PackageBundle", []string{q}, false), "entry"},
		{"several 2.0 outcomes into 2.2's one", e08Answer("PackageBundle", []string{q, qr}, false, "operationOutcome", "operationOutcome"), "outcome"},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				r := e08Row(t, level, declares22, row.answer)
				if r.rec.Code == http.StatusOK {
					t.Fatalf("answered %s", r.rec.Body.String())
				}
				body := r.rec.Body.String()
				for _, want := range []string{"the payer answered", "DTR 2.2 line", "E-08", row.why} {
					if !strings.Contains(body, want) {
						t.Fatalf("refusal %q lacks %q", body, want)
					}
				}
				if leg, outcome := lastOutcome(t, r.env.originator); leg != dtrLeg || outcome != "error" {
					t.Fatalf("recorded %s %s", leg, outcome)
				}
				renamed := e08Bridged(t, row.answer, "2.0", "2.2")
				var bridged []ConformanceFinding
				for _, f := range r.findings {
					if f.Kind == string(KindFHIRBridged) {
						bridged = append(bridged, f)
					}
				}
				if len(bridged) != 1 {
					t.Fatalf("want one bridged finding, got %+v", r.findings)
				}
				f := bridged[0]
				if f.Decision != Refuse.String() || f.Edit != "E-08" || f.Line != "2.2" ||
					f.SourcePayloadSHA256 != sha256hex(row.answer) || f.PayloadSHA256 != sha256hex(renamed) {
					t.Fatalf("finding %+v; want refused, E-08, line 2.2, source %s, renamed %s",
						f, sha256hex(row.answer), sha256hex(renamed))
				}
			})
		}
	}
}

// An answer that is not valid at its own line is not renamed: it is carried
// as sent, and the payer-answer check reports it by level as it reports any
// payer's answer: no finding at none, a finding at observe, a refusal at
// strict, and at structural a refusal of this structural defect (a package
// that is not a Bundle).
func TestDTRAnswerBridge_SourceInvalidCarriesByLevel(t *testing.T) {
	answer := e08Answer("PackageBundle", nil, true)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08Row(t, level, declares22, answer)
			var fhir []ConformanceFinding
			for _, f := range r.findings {
				if f.Kind == string(KindFHIRBridged) {
					t.Fatalf("an answer without source evidence was bridged: %+v", f)
				}
				if f.LegType == dtrLeg && f.Profile == dtrOutputParametersProfile {
					fhir = append(fhir, f)
				}
			}
			switch level {
			case EnforcementNone:
				r.wantAnswered(t, answer, false)
				if len(fhir) != 0 {
					t.Fatalf("a finding at none: %+v", fhir)
				}
			case EnforcementObserve:
				r.wantAnswered(t, answer, false)
				if len(fhir) != 1 || fhir[0].Decision != Record.String() || fhir[0].Edit != "" {
					t.Fatalf("want one recorded finding, got %+v", fhir)
				}
			case EnforcementStructural, EnforcementStrict:
				if r.rec.Code == http.StatusOK {
					t.Fatalf("strict carried a non-conformant answer: %s", r.rec.Body.String())
				}
				if len(fhir) != 1 || fhir[0].Decision != Refuse.String() {
					t.Fatalf("want one refused finding, got %+v", fhir)
				}
			}
		})
	}
}

// validateRequest, the request a check sends this gateway's validator. A
// Parameters is wrapped; against an explicit profile the wrapped copy asserts
// the profile as its own meta.profile and no ?profile= is sent, because HAPI
// judges the Parameters a ?profile= names: the wrapper. Every other resource
// is sent as it is, with its profile.
func TestValidateRequest(t *testing.T) {
	const prof = "http://example.org/StructureDefinition/p"
	params := []byte(`{"resourceType":"Parameters","meta":{"source":"s","profile":["http://other"]},"parameter":[{"name":"x","valueDecimal":1.50}]}`)
	t.Run("a Parameters against an explicit profile", func(t *testing.T) {
		req, profile := validateRequest(params, prof)
		if profile != "" {
			t.Fatalf("a ?profile= is sent: %q", profile)
		}
		var w struct {
			ResourceType string `json:"resourceType"`
			Parameter    []struct {
				Name     string          `json:"name"`
				Resource json.RawMessage `json:"resource"`
			} `json:"parameter"`
		}
		if err := json.Unmarshal(req, &w); err != nil || w.ResourceType != "Parameters" || len(w.Parameter) != 1 || w.Parameter[0].Name != "resource" {
			t.Fatalf("not the $validate wrapper: %s", req)
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(w.Parameter[0].Resource, &inner); err != nil {
			t.Fatal(err)
		}
		if string(inner["meta"]) != `{"profile":["`+prof+`"],"source":"s"}` {
			t.Fatalf("the copy's meta %s: want the profile in place of any declared, and the rest kept", inner["meta"])
		}
		if string(inner["parameter"]) != `[{"name":"x","valueDecimal":1.50}]` {
			t.Fatalf("the copy's own content changed: %s", inner["parameter"])
		}
	})
	for _, row := range []struct{ name, body string }{
		{"meta null", `{"resourceType":"Parameters","meta":null,"parameter":[]}`},
		{"no meta", `{"resourceType":"Parameters","parameter":[]}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			req, profile := validateRequest([]byte(row.body), prof)
			if profile != "" || !bytes.Contains(req, []byte(`"meta":{"profile":["`+prof+`"]}`)) {
				t.Fatalf("got %q %s", profile, req)
			}
		})
	}
	t.Run("a Parameters against its own meta.profile", func(t *testing.T) {
		req, profile := validateRequest(params, "")
		if profile != "" || !bytes.Equal(req, wrapValidateResource(params)) {
			t.Fatalf("got %q %s", profile, req)
		}
	})
	t.Run("another resource with an explicit profile is sent as it is", func(t *testing.T) {
		body := []byte(`{"resourceType":"Bundle","type":"collection"}`)
		req, profile := validateRequest(body, prof)
		if profile != prof || !bytes.Equal(req, body) {
			t.Fatalf("got %q %s", profile, req)
		}
	})
}

// The line fake judges a wrapped Parameters as HAPI does: with no ?profile= it
// unwraps and checks the resource against its meta.profile; with a ?profile=
// it judges the wrapper itself, which has no package (2.0.1 requires one).
func TestLineFakeMirrorsHAPIOnAWrappedParameters(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false)
	renamed22 := renamedAs(answer, "PackageBundle", "packagebundle")
	v := NewLineFakeValidator("2.0")
	check := func(body []byte, profile string) bool {
		req, p := validateRequest(body, profile)
		res, err := v.Validate(context.Background(), req, p)
		if err != nil {
			t.Fatal(err)
		}
		return res.Valid
	}
	if !check(answer, dtrOutputParametersProfile) {
		t.Fatal("a 2.0.1 answer asserted through the copy's meta.profile is not valid at 2.0")
	}
	if check(renamed22, dtrOutputParametersProfile) {
		t.Fatal("negative control: a 2.2-named answer is valid at 2.0")
	}
	res, err := v.Validate(context.Background(), wrapValidateResource(answer), dtrOutputParametersProfile)
	if err != nil || res.Valid {
		t.Fatalf("a wrapped answer sent with ?profile= is judged as its wrapper by HAPI, so not valid here either: %+v %v", res, err)
	}
}

// e08RecordingValidator keeps every request a check sent, and answers as inner.
type e08RecordingValidator struct {
	inner    shnsdk.Validator
	bodies   [][]byte
	profiles []string
}

func (r *e08RecordingValidator) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	r.bodies, r.profiles = append(r.bodies, append([]byte(nil), body...)), append(r.profiles, profile)
	return r.inner.Validate(ctx, body, profile)
}

// The checks E-08 makes go to the validator as validation copies (the profile
// asserted in the wrapped copy's meta.profile), while the EHR receives the
// payer's bytes with only the names changed, and a refusal's finding hashes
// the payer's bytes and the renamed bytes, never a copy.
func TestDTRAnswerBridge_ValidationCopiesNeverLeave(t *testing.T) {
	q, qr := "Questionnaire", "QuestionnaireResponse"
	for _, row := range []struct {
		name    string
		answer  []byte
		refused bool
	}{
		{"bridged", e08Answer("PackageBundle", []string{q, qr}, false), false},
		{"refused", e08Answer("PackageBundle", []string{q}, false), true},
	} {
		t.Run(row.name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.TranslateIGLines = true
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			env.originator.cfg.DeclaredContractVersions = declares22
			rec := map[string]*e08RecordingValidator{}
			byLine := map[string]shnsdk.Validator{}
			for _, l := range []string{"2.0", "2.1", "2.2"} {
				rec[l] = &e08RecordingValidator{inner: NewLineFakeValidator(l)}
				byLine[l] = rec[l]
			}
			env.originator.cfg.ValidatorsByContractLine = contractLanes(byLine)
			var findings []ConformanceFinding
			env.originator.cfg.Observer = func(e ObserverEvent) {
				var f ConformanceFinding
				if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindFHIRBridged) {
					findings = append(findings, f)
				}
			}
			declareFramedDTR(t, env, true)
			env.payerReturns(LegResult{Response: testResponse(row.answer)})
			req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
			req.Header.Set("Content-Type", "application/fhir+json")
			w := httptest.NewRecorder()
			env.originator.ingressRoute(RouteDTR)(w, req)
			renamed := e08Bridged(t, row.answer, "2.0", "2.2")
			// Each lane E-08 asked saw a wrapped copy asserting the output profile, and no ?profile=.
			asked := 0
			for _, l := range []string{"2.0", "2.2"} {
				for i, b := range rec[l].bodies {
					if !bytes.Contains(b, []byte(dtrOutputParametersProfile)) {
						continue
					}
					asked++
					if rec[l].profiles[i] != "" || !bytes.HasPrefix(b, []byte(`{"resourceType":"Parameters","parameter":[{"name":"resource","resource":`)) {
						t.Fatalf("lane %s was asked %q with %.120s", l, rec[l].profiles[i], b)
					}
				}
			}
			if asked != 2 {
				t.Fatalf("E-08 asked %d lanes, want the source line and the target line", asked)
			}
			if row.refused {
				if w.Code == http.StatusOK || len(findings) != 1 ||
					findings[0].SourcePayloadSHA256 != sha256hex(row.answer) || findings[0].PayloadSHA256 != sha256hex(renamed) {
					t.Fatalf("answer %d, findings %+v", w.Code, findings)
				}
				return
			}
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), renamed) {
				t.Fatalf("the EHR received %d %s", w.Code, w.Body.Bytes())
			}
		})
	}
}

// e08RawEntries is the raw bytes of each entry resource of the package
// parameter at index i.
func e08RawEntries(t *testing.T, body []byte, i int) []json.RawMessage {
	t.Helper()
	var p struct {
		Parameter []struct {
			Resource struct {
				Entry []struct {
					Resource json.RawMessage `json:"resource"`
				} `json:"entry"`
			} `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(body, &p); err != nil || i >= len(p.Parameter) {
		t.Fatalf("not a package answer: %v", err)
	}
	var out []json.RawMessage
	for _, e := range p.Parameter[i].Resource.Entry {
		out = append(out, e.Resource)
	}
	return out
}

// The splice: every entry but the QuestionnaireResponse reaches the EHR byte
// for byte as the payer sent it; the QuestionnaireResponse carries the
// registered moves (qr-coverage, the intendedUse code system); and the record
// names E-08 and each pa.dtr step the chain ran.
func TestDTRAnswerBridge_SplicesOnlyTheQuestionnaireResponse(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse", "Library"}, false, "operationOutcome")
	var transformed []ObserverEvent
	r := e08RowObserved(t, EnforcementObserve, declares22, answer, func(e ObserverEvent) {
		if e.Kind == legTransformedKind {
			transformed = append(transformed, e)
		}
	})
	r.wantAnswered(t, e08Bridged(t, answer, "2.0", "2.2"), true)
	src, got := e08RawEntries(t, answer, 0), e08RawEntries(t, r.rec.Body.Bytes(), 0)
	if len(src) != 3 || len(got) != 3 {
		t.Fatalf("entries %d -> %d", len(src), len(got))
	}
	for _, i := range []int{0, 2} {
		if !bytes.Equal(src[i], got[i]) {
			t.Fatalf("entry %d changed:\n got %s\nwant %s", i, got[i], src[i])
		}
	}
	qr := string(got[1])
	for _, want := range []string{"/qr-coverage", "CodeSystem/coverage-information-codes", "ServiceRequest/sr1"} {
		if !strings.Contains(qr, want) {
			t.Fatalf("the QuestionnaireResponse lacks %s: %s", want, qr)
		}
	}
	var parsed struct {
		Extension []struct {
			URL            string `json:"url"`
			ValueReference struct {
				Reference string `json:"reference"`
			} `json:"valueReference"`
		} `json:"extension"`
	}
	if err := json.Unmarshal(got[1], &parsed); err != nil {
		t.Fatal(err)
	}
	for _, e := range parsed.Extension {
		if strings.HasSuffix(e.URL, "/qr-context") && strings.HasPrefix(e.ValueReference.Reference, "Coverage/") {
			t.Fatalf("the QuestionnaireResponse keeps 2.0.1's Coverage qr-context: %s", qr)
		}
	}
	if strings.Contains(qr, "CodeSystem/temp") {
		t.Fatalf("the QuestionnaireResponse keeps 2.0.1's intendedUse code system: %s", qr)
	}
	// Everything outside the package's entries is the payer's, but for the names.
	outside := func(b []byte, entries []json.RawMessage) []byte {
		for _, e := range entries {
			b = bytes.Replace(b, e, nil, 1)
		}
		return b
	}
	if !bytes.Equal(outside(r.rec.Body.Bytes(), got), outside(renamedAs(answer, "PackageBundle", "packagebundle", "operationOutcome", "outcome"), src)) {
		t.Fatal("bytes outside the package's entries changed beyond the names")
	}
	if len(transformed) != 1 || transformed[0].Detail != "pa.dtr 2.0->2.1, pa.dtr 2.1->2.2" || transformed[0].Payload == nil {
		t.Fatalf("leg.transformed %+v, want the two registered steps with their Provenance", transformed)
	}
}

// A registered pa.dtr step that refuses the QuestionnaireResponse refuses the
// answer at every level, as that step refuses today: a 2.0.1 response whose
// contexts name no Coverage has no honest source for 2.2's qr-coverage.
func TestDTRAnswerBridge_AStepsRefusalRefusesAtEveryLevel(t *testing.T) {
	answer := bytes.Replace(e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false),
		[]byte(`"reference" : "Coverage/c1"`), []byte(`"reference" : "Encounter/e1"`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08Row(t, level, declares22, answer)
			body := r.rec.Body.String()
			if r.rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "the payer answered") || !strings.Contains(body, "pa.dtr 2.1") {
				t.Fatalf("answer %d %s", r.rec.Code, body)
			}
			if leg, outcome := lastOutcome(t, r.env.originator); leg != dtrLeg || outcome != "error" {
				t.Fatalf("recorded %s %s", leg, outcome)
			}
		})
	}
}

// e08LineContent stands in for a validator lane that judges a Questionnaire's
// content by line: at 2.2 it refuses one marked as written for 2.0.1 only.
type e08LineContent struct{ inner shnsdk.Validator }

func (v e08LineContent) Validate(ctx context.Context, body []byte, profile string) (shnsdk.Result, error) {
	if bytes.Contains(body, []byte("written-for-2.0.1-only")) {
		return shnsdk.Result{Valid: false, Issues: []string{"Questionnaire: content of another line"}}, nil
	}
	return v.inner.Validate(ctx, body, profile)
}

// The rest of the package is carried exactly, so a Questionnaire whose content
// differs by line fails the target line and the answer is refused at every
// level: no step rewrites it, and the bridge never fills.
func TestDTRAnswerBridge_AQuestionnaireOfAnotherLineIsRefused(t *testing.T) {
	answer := bytes.Replace(e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false),
		[]byte(`"status" : "active"`), []byte(`"status" : "active", "title" : "written-for-2.0.1-only"`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08RowWith(t, level, declares22, answer, func(lanes map[string]shnsdk.Validator) {
				lanes["2.2"] = e08LineContent{inner: lanes["2.2"]}
			})
			if r.rec.Code == http.StatusOK || !strings.Contains(r.rec.Body.String(), "content of another line") {
				t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
			}
		})
	}
}

// A target check that could not run (no lane for the provider's line) refuses
// at every level whose payload checks consume lanes, and says so: the answer
// was never judged, so the refusal does not call it invalid. At none, where no
// payload check consumes a lane and a declared line may have none, the answer is
// never renamed: it is carried exactly, with no E-08 edit, though the source
// line's lane would have let it cross. Both halves are the level-scoped edge of
// payloadChecksConsumeLanes: a lane missing where checks consume lanes is
// refused, not carried.
func TestDTRAnswerBridge_AnUnjudgedTargetRefusesAndSaysSo(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			r := e08RowWith(t, level, declares22, answer, func(lanes map[string]shnsdk.Validator) {
				delete(lanes, "2.2")
			})
			if level == EnforcementNone {
				r.wantAnswered(t, answer, false)
				return
			}
			body := r.rec.Body.String()
			if r.rec.Code == http.StatusOK || !strings.Contains(body, "the payer answered") ||
				!strings.Contains(body, "could not run") || strings.Contains(body, "not valid at that line") {
				t.Fatalf("answer %d %s", r.rec.Code, body)
			}
		})
	}
}

// The DTR ingress records, on its access line, the line the answer it wrote
// back is written in: the provider's own line when E-08 carried it across, the
// routed line when it carried the answer as sent, and none when it refused. The
// record is the ingress call's own (Direction ingress, the questionnaire leg,
// the payer as recipient), the one evidence reads the answer's line from.
func TestDTRAnswerBridge_RecordsTheAnswersLine(t *testing.T) {
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false)
	for _, row := range []struct {
		name          string
		declared      []string
		payer         []string
		answer        []byte
		routed, wrote string
		edited        bool
	}{
		// The payer reached at 2.0, its answer carried across to the 2.2 provider.
		{"bridged to 2.2", declares22, declares20, answer, "2.0", "pa.dtr@2.2", true},
		{"carried as sent", declares20, declares20, answer, "2.0", "pa.dtr@2.0", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := e08RowFull(t, EnforcementObserve, row.declared, row.answer, nil, nil, row.payer)
			if r.rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
			}
			if got := slices.Contains(r.recordedEdits(), string(relay.EditDTRAnswerLineNames)); got != row.edited {
				t.Fatalf("E-08 recorded=%v, want %v", got, row.edited)
			}
			var got []ExchangeRecord
			for _, x := range r.records {
				if x.AnswerLine != "" {
					got = append(got, x)
				}
			}
			if len(got) != 1 {
				t.Fatalf("records with an answer line: %+v", got)
			}
			x := got[0]
			if x.AnswerLine != row.wrote || x.ContractLine != row.routed || x.Direction != DirectionIngress || x.Exchange != dtrLeg || x.Recipient != r.env.payerID || x.Sender == x.Recipient {
				t.Fatalf("record %+v, want routed line %s and answer line %s on the ingress record of the questionnaire leg to %s", x, row.routed, row.wrote, r.env.payerID)
			}
		})
	}
	// A refused answer was never written: no record names a line for it.
	t.Run("refused", func(t *testing.T) {
		r := e08RowFull(t, EnforcementObserve, declares22, e08Answer("PackageBundle", []string{"Questionnaire"}, false), nil, nil, declares20)
		if r.rec.Code == http.StatusOK {
			t.Fatalf("answer %d %s", r.rec.Code, r.rec.Body.String())
		}
		for _, x := range r.records {
			if x.AnswerLine != "" {
				t.Fatalf("a refused answer's record names answer line %s: %+v", x.AnswerLine, x)
			}
		}
	})
}
