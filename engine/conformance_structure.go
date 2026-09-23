package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// These are checker budgets, not transport limits. Off/observe enforcement
// does not decode. A bounded-out check cannot certify a message as valid.
const structuralMaxBytes = 4 << 20
const structuralMaxDepth = 128
const structuralMaxTokens = 250000

var errStructuralBound = errors.New("structural decoding budget exhausted")

type structuralDocument struct {
	value                      any
	syntax, duplicate, bounded bool
}

func decodeStructural(body []byte) *structuralDocument {
	out := &structuralDocument{}
	if len(body) > structuralMaxBytes {
		out.bounded = true
		return out
	}
	if !utf8.Valid(body) {
		out.syntax = true
		return out
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	tokens := 0
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		tokens++
		if depth > structuralMaxDepth || tokens > structuralMaxTokens {
			return nil, errStructuralBound
		}
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return tok, nil
		}
		switch delim {
		case '{':
			obj := map[string]any{}
			for d.More() {
				tokens++
				if tokens > structuralMaxTokens {
					return nil, errStructuralBound
				}
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				name, ok := key.(string)
				if !ok {
					return nil, errors.New("object member")
				}
				if _, exists := obj[name]; exists {
					out.duplicate = true
				}
				v, err := value(depth + 1)
				if err != nil {
					return nil, err
				}
				obj[name] = v
			}
			end, err := d.Token()
			if err != nil {
				return nil, err
			}
			if end != json.Delim('}') {
				return nil, errors.New("object end")
			}
			return obj, nil
		case '[':
			arr := []any{}
			for d.More() {
				v, err := value(depth + 1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			end, err := d.Token()
			if err != nil {
				return nil, err
			}
			if end != json.Delim(']') {
				return nil, errors.New("array end")
			}
			return arr, nil
		default:
			return nil, errors.New("unexpected delimiter")
		}
	}
	v, err := value(0)
	if errors.Is(err, errStructuralBound) {
		out.bounded = true
		return out
	}
	if err != nil {
		out.syntax = true
		return out
	}
	if _, err = d.Token(); err != io.EOF {
		out.syntax = true
		return out
	}
	out.value = v
	return out
}

func (in CheckInput) document() *structuralDocument {
	if in.decoded != nil {
		return in.decoded
	}
	return decodeStructural(in.Body)
}
func structuralApplicable(in CheckInput) bool {
	return in.Direction == "request" || (in.Direction == "response" && in.Status >= 200 && in.Status < 300)
}
func checkResult(id string, valid bool) CheckResult {
	if valid {
		return CheckResult{State: CheckValid}
	}
	return CheckResult{State: CheckInvalid, Code: id, Severity: "error"}
}
func structuralObject(in CheckInput) (map[string]any, bool) {
	d := in.document()
	if d.syntax || d.bounded || d.duplicate {
		return nil, false
	}
	obj, ok := d.value.(map[string]any)
	return obj, ok
}
func shapeRule(id string, applies func(CheckInput) bool, check func(CheckInput, map[string]any) bool) ConformanceRule {
	return ConformanceRule{ID: id, Class: CheckStructural, Applies: func(in CheckInput) bool { return structuralApplicable(in) && applies(in) }, Check: func(_ context.Context, in CheckInput) CheckResult {
		obj, ok := structuralObject(in)
		if !ok {
			return CheckResult{State: CheckNotApplicable}
		}
		return checkResult(id, check(in, obj))
	}}
}

// PAS schema checks require a supported declaration or the documented absent-
// declaration compatibility envelope. An unknown line supplies no schema proof.
func pasShapeRule(id string, applies func(CheckInput) bool, check func(CheckInput, map[string]any) bool) ConformanceRule {
	rule := shapeRule(id, applies, check)
	knownCheck := rule.Check
	rule.Check = func(ctx context.Context, in CheckInput) CheckResult {
		if _, ok := structuralObject(in); !ok {
			return CheckResult{State: CheckNotApplicable}
		}
		switch in.DeclaredVersion {
		case "", "pa.pas@2.0", "pa.pas@2.1", "pa.pas@2.2":
			return knownCheck(ctx, in)
		default:
			return CheckResult{State: CheckUnavailable, Code: id, Severity: "error"}
		}
	}
	return rule
}

func legDirection(legs []string, direction string) func(CheckInput) bool {
	return func(in CheckInput) bool {
		if in.Direction != direction {
			return false
		}
		for _, leg := range legs {
			if in.Exchange.legType == leg {
				_, ok := paCatalog[leg]
				return ok
			}
		}
		return false
	}
}
func dtrDirection(operation, direction string) func(CheckInput) bool {
	return func(in CheckInput) bool {
		return legDirection([]string{"dtr-questionnaire-fetch"}, direction)(in) && in.Exchange.operation == operation
	}
}
func shapeString(v any) bool                          { _, ok := v.(string); return ok }
func shapeObject(v any) bool                          { _, ok := v.(map[string]any); return ok }
func resourceIs(obj map[string]any, name string) bool { return obj["resourceType"] == name }

// StructuralRules is the closed envelope table. It intentionally does not
// resolve references, compare patients, inspect clinical codes or certify IGs.
// Returned slices can be extended for execution without modifying this table.
func StructuralRules() []ConformanceRule {
	rules := []ConformanceRule{
		{ID: "content.contract", Class: CheckStructural, Applies: structuralApplicable, Check: func(_ context.Context, in CheckInput) CheckResult {
			_, known := paCatalog[in.Exchange.legType]
			if !known || (in.Exchange.legType == "dtr-questionnaire-fetch" && in.Exchange.operation != shnsdk.FrameOperationQuestionnairePackage && in.Exchange.operation != shnsdk.FrameOperationNextQuestion) {
				return CheckResult{State: CheckUnavailable, Code: "content.contract", Severity: "error"}
			}
			return CheckResult{State: CheckValid}
		}},
		{ID: "content.required", Class: CheckStructural, Applies: structuralApplicable, Check: func(_ context.Context, in CheckInput) CheckResult {
			return checkResult("content.required", len(bytes.TrimSpace(in.Body)) > 0)
		}},
	}
	for _, id := range []string{"json.bounds", "json.syntax", "json.duplicate_key", "json.object"} {
		rules = append(rules, ConformanceRule{ID: id, Class: CheckStructural, Applies: structuralApplicable, Check: func(_ context.Context, in CheckInput) CheckResult {
			d := in.document()
			switch id {
			case "json.bounds":
				if d.bounded {
					return CheckResult{State: CheckUnavailable, Code: id, Severity: "error"}
				}
				return CheckResult{State: CheckValid}
			case "json.syntax":
				if d.bounded {
					return CheckResult{State: CheckNotApplicable}
				}
				return checkResult(id, !d.syntax)
			case "json.duplicate_key":
				if d.syntax || d.bounded {
					return CheckResult{State: CheckNotApplicable}
				}
				return checkResult(id, !d.duplicate)
			default:
				if d.syntax || d.bounded || d.duplicate {
					return CheckResult{State: CheckNotApplicable}
				}
				return checkResult(id, shapeObject(d.value))
			}
		}})
	}
	crd := []string{"crd-order-select", "crd-order-dispatch"}
	for _, field := range []string{"hookInstance", "hook", "context"} {
		rules = append(rules, shapeRule("crd.request."+field, legDirection(crd, "request"), func(_ CheckInput, o map[string]any) bool {
			if field == "context" {
				return shapeObject(o[field])
			}
			return shapeString(o[field])
		}))
	}
	rules = append(rules,
		shapeRule(shnsdk.CDSResponseCardsRule, legDirection(crd, "response"), func(_ CheckInput, o map[string]any) bool { _, ok := o["cards"].([]any); return ok }),
		shapeRule(shnsdk.CDSCardObjectRule, legDirection(crd, "response"), func(_ CheckInput, o map[string]any) bool {
			a, ok := o["cards"].([]any)
			if !ok {
				return true
			}
			for _, v := range a {
				if !shapeObject(v) {
					return false
				}
			}
			return true
		}),
	)
	for _, direction := range []string{"request", "response"} {
		rules = append(rules,
			shapeRule("dtr.package."+direction, dtrDirection(shnsdk.FrameOperationQuestionnairePackage, direction), func(_ CheckInput, o map[string]any) bool {
				if direction == "response" && resourceIs(o, "OperationOutcome") {
					return true
				}
				if direction == "response" && resourceIs(o, "Bundle") {
					entries, ok := bundleEntries(o, true)
					if !ok || o["type"] != "collection" {
						return false
					}
					for _, raw := range entries {
						entry, ok := raw.(map[string]any)
						if !ok {
							return false
						}
						resource, ok := entry["resource"].(map[string]any)
						if !ok || !shapeString(resource["resourceType"]) {
							return false
						}
					}
					return true
				}
				_, ok := parameterShapes(o, direction == "request")
				return ok
			}),
			shapeRule("dtr.next-question."+direction, dtrDirection(shnsdk.FrameOperationNextQuestion, direction), func(_ CheckInput, o map[string]any) bool { return nextQuestionShape(o, direction) }),
			shapeRule("query."+direction, legDirection([]string{"federated-query"}, direction), func(_ CheckInput, o map[string]any) bool { return resourceIs(o, "Task") }),
			shapeRule("eligibility."+direction, legDirection([]string{"coverage-eligibility"}, direction), func(_ CheckInput, o map[string]any) bool {
				if direction == "request" {
					return resourceIs(o, "CoverageEligibilityRequest")
				}
				return resourceIs(o, "CoverageEligibilityResponse")
			}),
		)
	}
	rules = append(rules,
		shapeRule("fhir.operation-outcome", func(in CheckInput) bool {
			return in.Direction == "response" && (in.Exchange.legType == "pas-claim-inquire" || dtrDirection(shnsdk.FrameOperationQuestionnairePackage, "response")(in))
		}, func(_ CheckInput, o map[string]any) bool {
			if !resourceIs(o, "OperationOutcome") {
				return true
			}
			return outcomeShape(o)
		}),
		pasShapeRule("pas.request.bundle", legDirection([]string{"pas-claim", "pas-claim-update", "pas-claim-inquire"}, "request"), pasRequestShape),
		pasShapeRule("pas.response.bundle", legDirection([]string{"pas-claim", "pas-claim-update"}, "response"), func(_ CheckInput, o map[string]any) bool { return primaryBundle(o, "ClaimResponse") }),
		pasShapeRule("pas.inquiry.response", legDirection([]string{"pas-claim-inquire"}, "response"), inquiryShape),
		pasShapeRule("pas.inquiry.return", legDirection([]string{"pas-claim-inquire"}, "response"), func(in CheckInput, o map[string]any) bool {
			if !resourceIs(o, "Parameters") {
				return true
			}
			a, ok := parameterShapes(o, false)
			if !ok {
				return true
			}
			for _, v := range a {
				// The recorded legacy wrapper is readable without a declared line.
				// That compatibility does not certify it against a declared IG.
				name := v.(map[string]any)["name"]
				if name != "return" && !(in.DeclaredVersion == "" && name == "responseBundle") {
					return false
				}
			}
			return true
		}),
	)
	for _, field := range []string{"linkId", "answer", "patientRef"} {
		rules = append(rules, shapeRule("patient-dtr.request."+field, legDirection([]string{"patient-dtr"}, "request"), func(_ CheckInput, o map[string]any) bool { return shapeString(o[field]) }))
	}
	return append(rules, shapeRule("patient-dtr.response", legDirection([]string{"patient-dtr"}, "response"), func(_ CheckInput, o map[string]any) bool { return shapeObject(o["attestedItem"]) }))
}

func parameterShapes(o map[string]any, required bool) ([]any, bool) {
	if !resourceIs(o, "Parameters") {
		return nil, false
	}
	v, present := o["parameter"]
	if !present {
		return nil, !required
	}
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, v := range a {
		p, ok := v.(map[string]any)
		if !ok || !shapeString(p["name"]) {
			return nil, false
		}
	}
	return a, true
}
func nextQuestionShape(o map[string]any, direction string) bool {
	if resourceIs(o, "QuestionnaireResponse") {
		return true
	}
	a, ok := parameterShapes(o, true)
	if !ok {
		return false
	}
	name := "questionnaire-response"
	if direction == "response" {
		name = "return"
	}
	count := 0
	for _, v := range a {
		p := v.(map[string]any)
		if r, present := p["resource"]; present && !shapeObject(r) {
			return false
		}
		if p["name"] == name {
			r, ok := p["resource"].(map[string]any)
			if !ok || !resourceIs(r, "QuestionnaireResponse") {
				return false
			}
			count++
		}
	}
	return count == 1
}
func bundleEntries(o map[string]any, required bool) ([]any, bool) {
	if !resourceIs(o, "Bundle") {
		return nil, false
	}
	v, present := o["entry"]
	if !present {
		return nil, !required
	}
	a, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, v := range a {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if raw, present := e["resource"]; present {
			r, ok := raw.(map[string]any)
			if !ok || !shapeString(r["resourceType"]) {
				return nil, false
			}
		}
	}
	return a, true
}

// Request Bundle Claim slices are discriminated by resource type, not profile.
// Newer submit/update envelopes can include the prior Claim. An undeclared
// message uses the supported structural union; this supplies no certification.
func pasRequestShape(in CheckInput, o map[string]any) bool {
	if in.Exchange.legType == "pas-claim-inquire" {
		return primaryBundle(o, "Claim")
	}
	entries, ok := bundleEntries(o, true)
	if !ok {
		return false
	}
	max := 1
	switch in.DeclaredVersion {
	case "", "pa.pas@2.1", "pa.pas@2.2":
		max = 2
	}
	if in.DeclaredVersion == "pa.pas@2.2" {
		if len(entries) == 0 {
			return false
		}
		first, _ := entries[0].(map[string]any)["resource"].(map[string]any)
		if !resourceIs(first, "Claim") {
			return false
		}
	}
	count := 0
	for _, entry := range entries {
		resource, _ := entry.(map[string]any)["resource"].(map[string]any)
		if resourceIs(resource, "Claim") {
			count++
		}
	}
	return count >= 1 && count <= max
}

func primaryBundle(o map[string]any, primary string) bool {
	a, ok := bundleEntries(o, true)
	if !ok {
		return false
	}
	count := 0
	for _, v := range a {
		r, _ := v.(map[string]any)["resource"].(map[string]any)
		if resourceIs(r, primary) {
			count++
		}
	}
	return count == 1
}
func inquiryShape(in CheckInput, o map[string]any) bool {
	if resourceIs(o, "OperationOutcome") {
		return true
	}
	// Only the answer's authoritative declaration selects a line. No request
	// stamp, meta.profile inference or reader compatibility narrows this union.
	switch in.DeclaredVersion {
	case "pa.pas@2.0", "pa.pas@2.1":
		_, ok := bundleEntries(o, false)
		return ok
	case "pa.pas@2.2":
	case "":
		if resourceIs(o, "Bundle") {
			_, ok := bundleEntries(o, false)
			return ok
		}
	}
	a, ok := parameterShapes(o, false)
	if !ok {
		return false
	}
	for _, v := range a {
		r, ok := v.(map[string]any)["resource"].(map[string]any)
		if !ok {
			return false
		}
		if _, ok = bundleEntries(r, false); !ok {
			return false
		}
	}
	return true
}
func outcomeShape(o map[string]any) bool {
	raw, present := o["issue"]
	if !present {
		return true
	}
	a, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, v := range a {
		issue, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for _, field := range []string{"severity", "code", "diagnostics"} {
			if v, present := issue[field]; present && !shapeString(v) {
				return false
			}
		}
		if v, present := issue["details"]; present && !shapeObject(v) {
			return false
		}
		for _, field := range []string{"location", "expression"} {
			if v, present := issue[field]; present {
				a, ok := v.([]any)
				if !ok {
					return false
				}
				for _, v := range a {
					if !shapeString(v) {
						return false
					}
				}
			}
		}
	}
	return true
}

// conformanceError is a gateway refusal, never a peer's application answer.
// Only closed rule IDs and gateway configuration appear in its safe fields.
type conformanceError struct {
	status    int
	Category  string `json:"category"`
	Rule      string `json:"rule"`
	Gateway   string `json:"gateway"`
	Level     string `json:"level"`
	Direction string `json:"direction"`
}

func (e *conformanceError) Error() string { return e.Category + ": " + e.Rule }

func (g *Gateway) enforceContent(ctx context.Context, in CheckInput) error {
	if in.finding.LegType == "" {
		in.finding = findingContextFrom(ctx)
	}
	g.observeContent(in)
	return g.enforceRules(ctx, in, append(StructuralRules(), g.DeepRules()...))
}

// contentFinding snapshots authenticated exchange metadata before an optional
// worker can outlive the request. Ownership comes from the caller's actual
// boundary and payload provenance, never from parsed clinical content.
func contentFinding(ctx context.Context, ex ExchangeContext, whose, fallbackSeam string) findingContext {
	f := findingContextFrom(ctx)
	if ex.legType != "" {
		f.LegType = ex.legType
	}
	if ex.correlationID != "" {
		f.CorrelationID = ex.correlationID
	}
	if f.Seam == "" {
		f.Seam = fallbackSeam
	}
	f.Whose = whose
	return f
}
func (g *Gateway) enforceRules(ctx context.Context, in CheckInput, rules []ConformanceRule) error {
	// Application errors, including redirects, retain their exact bytes/status.
	if in.Direction == "response" && (in.Status < 200 || in.Status >= 300) {
		return nil
	}
	in.evidence = &contentEvidence{}
	for _, r := range rules {
		if in.Exchange.policy.Action(r.Class) != CheckEnforce {
			continue
		}
		if r.Applies != nil && !r.Applies(in) {
			continue
		}
		result := CheckResult{State: CheckUnavailable}
		if r.Check != nil && r.Applies != nil {
			if r.Class == CheckStructural && in.decoded == nil && r.ID != "content.contract" && r.ID != "content.required" {
				in.decoded = decodeStructural(in.Body)
			}
			result = r.Check(ctx, in)
		}
		if result.State == CheckValid || (result.State == CheckNotApplicable && r.Class == CheckStructural) || (result.State == CheckInvalid && result.Severity == "warning") {
			continue
		}
		status, category := http.StatusServiceUnavailable, "conformance_unavailable"
		if result.State == CheckInvalid {
			status, category = http.StatusUnprocessableEntity, "conformance_invalid"
			if in.Direction == "response" {
				status = http.StatusBadGateway
			}
		}
		g.emitFinding(ruleFinding(in, r, result, "refused"))
		return &conformanceError{status: status, Category: category, Rule: r.ID, Gateway: g.cfg.HolderID, Level: in.Exchange.policy.Level().String(), Direction: in.Direction}
	}
	return nil
}
