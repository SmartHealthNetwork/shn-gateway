package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/internal/observationjson"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// DeepRules is the supported semantic check set. It does not own response
// writers, clinical stores or authority. Applicability never depends on whether
// a message is relayed, authored, or supplied by a reference participant.
func (g *Gateway) DeepRules() []ConformanceRule {
	rules := []ConformanceRule{
		{ID: "cds.response", Check: checkCDSResponse},
		{ID: "fhir.profile", Check: func(ctx context.Context, in CheckInput) CheckResult {
			return validationResult(g.contentValidation(ctx, in).Profile, nil)
		}},
		{ID: "pas.graph", Check: checkPASGraph},
		{ID: "pas.provenance", Check: checkPASProvenance},
		{ID: "qr.attestation", Check: checkQRAttestation},
		{ID: "crd.resources", Check: checkCRDResources},
		{ID: "version.consistency", Check: checkVersionConsistency},
	}
	for i := range rules {
		id := rules[i].ID
		rules[i].Class = CheckDeep
		rules[i].Applies = func(in CheckInput) bool {
			if !structuralApplicable(in) {
				return false
			}
			_, known := paCatalog[in.Exchange.legType]
			if !known {
				return false
			}
			crd := in.Exchange.legType == "crd-order-select" || in.Exchange.legType == "crd-order-dispatch"
			pas := strings.HasPrefix(in.Exchange.legType, "pas-claim")
			switch id {
			case "pas.graph":
				return pas && in.Direction == "response"
			case "pas.provenance":
				return in.Exchange.legType == "pas-claim-update" && in.Direction == "request"
			case "qr.attestation":
				return pas && in.Direction == "request"
			case "crd.resources":
				return crd
			case "cds.response":
				return crd && in.Direction == "response"
			case "version.consistency":
				return in.Direction == "response" && paCatalog[in.Exchange.legType].Contract != ""
			case "fhir.profile":
				return in.Exchange.legType != "patient-dtr" && !emptyInquiryReply(in, true) && !emptyCRDResponse(in)
			}
			return false
		}
	}
	return rules
}

// inquiryReplyBundles recognizes only independently declared operation reply
// forms. The Parameters envelope carries no patient identity of its own.
func inquiryReplyBundles(in CheckInput) ([]any, bool) {
	if in.Exchange.legType != "pas-claim-inquire" || in.Direction != "response" {
		return nil, false
	}
	root, ok := deepDocument(in)
	if !ok || !inquiryShape(in, root) {
		return nil, false
	}
	switch in.DeclaredVersion {
	case "pa.pas@2.0", "pa.pas@2.1":
		if !resourceIs(root, "Bundle") {
			return nil, false
		}
		return []any{root}, true
	case "pa.pas@2.2":
		if !resourceIs(root, "Parameters") {
			return nil, false
		}
		params, _ := parameterShapes(root, false)
		bundles := make([]any, 0, len(params))
		for _, raw := range params {
			p := raw.(map[string]any)
			if p["name"] != "return" {
				return nil, false
			}
			bundles = append(bundles, p["resource"])
		}
		return bundles, true
	default:
		return nil, false
	}
}

// emptyCRDResponse proves absence of FHIR targets in a supported response's
// schema-defined action slots. It does not establish validator availability or
// waive the independent CDS envelope, resource and declaration checks.
func emptyCRDResponse(in CheckInput) bool {
	if in.Direction != "response" || in.Status < 200 || in.Status >= 300 || (in.Exchange.legType != "crd-order-select" && in.Exchange.legType != "crd-order-dispatch") {
		return false
	}
	contract, line, ok := strings.Cut(in.DeclaredVersion, "@")
	if !ok || contract != "pa.crd" {
		return false
	}
	if _, ok := shnsdk.CRDLineDef(line); !ok {
		return false
	}
	root, ok := deepDocument(in)
	if !ok {
		return false
	}
	// Presence of a resource, even null or malformed, requires its own check.
	noResources := func(owner map[string]any, key string) bool {
		raw, present := owner[key]
		if !present {
			return true
		}
		actions, ok := raw.([]any)
		if !ok {
			return false
		}
		for _, raw := range actions {
			action, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			if _, present := action["resource"]; present {
				return false
			}
		}
		return true
	}
	if !noResources(root, "systemActions") {
		return false
	}
	cards, ok := root["cards"].([]any)
	if !ok {
		return false
	}
	for _, raw := range cards {
		card, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		raw, present := card["suggestions"]
		if !present {
			continue
		}
		suggestions, ok := raw.([]any)
		if !ok {
			return false
		}
		for _, raw := range suggestions {
			suggestion, ok := raw.(map[string]any)
			if !ok || !noResources(suggestion, "actions") {
				return false
			}
		}
	}
	return true
}

// emptyInquiryReply proves there are no returned targets for a particular check.
// Malformed or unrelated messages retain their required checks. Bare Bundles
// still have a profile to validate, even when they have no entries.
func emptyInquiryReply(in CheckInput, wrapperOnly bool) bool {
	bundles, ok := inquiryReplyBundles(in)
	if !ok {
		return false
	}
	if wrapperOnly {
		return len(bundles) == 0
	}
	for _, raw := range bundles {
		bundle := raw.(map[string]any)
		entries, _ := bundleEntries(bundle, false)
		if bundle["type"] != "collection" || len(entries) != 0 {
			return false
		}
	}
	return true
}

func deepUnavailable(code string) CheckResult {
	return CheckResult{State: CheckUnavailable, Code: code, Severity: "error"}
}
func deepDocument(in CheckInput) (map[string]any, bool) { return structuralObject(in) }

// Safe codes are independent of any server's diagnostic prose. Unknown codes
// from custom evidence adapters cannot leak resource values through findings.
func safeValidationCode(code string) string {
	switch code {
	case "invalid", "structure", "required", "value", "invariant", "too-long", "duplicate", "business-rule", "informational", "code-invalid", "not-supported", "not-found", "incomplete", "profile-unsupported", "execution-unavailable", "synthetic", "synthetic-rejection":
		return code
	default:
		return "validator-result"
	}
}
func validationResult(result validationCheckEvidence, err error) CheckResult {
	out := deepUnavailable("validator_unavailable")
	if err != nil {
		return out
	}
	switch result.State {
	case validationValid:
		out = CheckResult{State: CheckValid}
	case validationInvalid:
		out = CheckResult{State: CheckInvalid, Code: safeValidationCode(result.Code), Severity: "error"}
		// Required rules already established applicability. A checker cannot waive
		// the rule by returning not-applicable without that rule's own evidence.
	}
	for _, issue := range result.Issues {
		switch issue.Severity {
		case "fatal", "error", "warning", "information":
		default:
			return deepUnavailable("validator_unavailable")
		}
		if len(out.Issues) == 64 {
			return deepUnavailable("validator_issue_budget")
		}
		out.Issues = append(out.Issues, CheckIssue{Severity: issue.Severity, Code: safeValidationCode(issue.Code)})
	}
	return out
}

type contentEvidence struct {
	done  bool
	value validationEvidence
}
type validationTarget struct {
	body    []byte
	profile string
}

func (g *Gateway) contentValidation(ctx context.Context, in CheckInput) validationEvidence {
	if in.evidence != nil && in.evidence.done {
		return in.evidence.value
	}
	evidence := unavailableValidatorEvidence()
	if ctx.Err() == nil {
		targets, contract, line, ok := deepValidationTargets(in)
		if ok {
			evidence = *validContentEvidence()
			validator := g.validatorForContractLine(contract, line)
			for _, target := range targets {
				raw, release, ok := observationBytesTarget(in, target.body)
				if !ok {
					evidence = unavailableValidatorEvidence()
					break
				}
				result, err := func() (validationEvidence, error) {
					defer release()
					return delegateValidatorEvidence(ctx, validator, raw, target.profile)
				}()
				if err != nil {
					attempted := result.ExecutionAttempted
					result = unavailableValidatorEvidence()
					result.ExecutionAttempted = attempted
				}
				// Adapters distinguish actual checker attempts from local input gaps.
				// Interface implementation alone does not prove execution.
				g.recordCheckerAvailability(contract, line, result)
				evidence.Profile = mergeValidationEvidence(evidence.Profile, result.Profile)
			}
		}
	}
	if in.evidence != nil {
		in.evidence.done = true
		in.evidence.value = evidence
	}
	return evidence
}
func validContentEvidence() *validationEvidence {
	return &validationEvidence{Profile: validationCheckEvidence{State: validationValid}}
}
func mergeValidationEvidence(a, b validationCheckEvidence) validationCheckEvidence {
	rank := func(s validationState) int {
		switch s {
		case validationValid:
			return 0
		case validationInvalid:
			return 1
		default:
			return 2
		}
	}
	issues := append(a.Issues, b.Issues...)
	if rank(b.State) > rank(a.State) {
		a = b
	}
	a.Issues = issues
	if len(issues) > 64 {
		a.State = validationUnavailable
		a.Code = "validator_issue_budget"
		a.Issues = nil
	}
	return a
}

// Only an independent declaration selects a profile line. Profile maps use
// existing published contract definitions, never request stamps or parser guesses.
func deepValidationTargets(in CheckInput) ([]validationTarget, string, string, bool) {
	root, ok := deepDocument(in)
	if !ok {
		return nil, "", "", false
	}
	contract := paCatalog[in.Exchange.legType].Contract
	line := ""
	if contract != "" {
		declaredContract, declaredLine, found := strings.Cut(in.DeclaredVersion, "@")
		if !found || declaredContract != contract {
			return nil, contract, "", false
		}
		line = declaredLine
		switch contract {
		case "pa.pas":
			_, ok = shnsdk.PASLineDef(line)
		case "pa.dtr":
			_, ok = shnsdk.DTRLineDef(line)
		case "pa.crd":
			_, ok = shnsdk.CRDLineDef(line)
		}
		if !ok {
			return nil, contract, line, false
		}
	}
	targets := []validationTarget{}
	add := func(resource map[string]any, profile string) {
		raw, _ := json.Marshal(resource)
		targets = append(targets, validationTarget{body: raw, profile: profile})
	}
	switch contract {
	case "pa.pas":
		if resourceIs(root, "OperationOutcome") {
			add(root, "")
			break
		}
		if resourceIs(root, "Parameters") && in.Exchange.legType == "pas-claim-inquire" && in.Direction == "response" {
			// The operation-defined wrapper has no IG profile; its shape is checked by
			// the structural registry and each returned Bundle uses the inquiry profile.
			params, ok := root["parameter"].([]any)
			if !ok || len(params) == 0 || len(params) > crdEmbeddedValidationMax {
				return nil, contract, line, false
			}
			for _, v := range params {
				p, _ := v.(map[string]any)
				r, _ := p["resource"].(map[string]any)
				if !resourceIs(r, "Bundle") {
					return nil, contract, line, false
				}
				profile, ok := profileFor("PASResponseBundle", line, in.Exchange.legType)
				if !ok {
					return nil, contract, line, false
				}
				add(r, profile)
			}
			break
		}
		species := "PASRequestBundle"
		if in.Direction == "response" {
			species = "PASResponseBundle"
		}
		profile, ok := profileFor(species, line, in.Exchange.legType)
		if !ok {
			return nil, contract, line, false
		}
		add(root, profile)
	case "pa.crd":
		resources, ok := crdResources(root, in.Direction)
		if !ok {
			return nil, contract, line, false
		}
		// An empty extraction cannot establish the required FHIR checker support.
		// Keep the support gap visible instead of manufacturing a valid result.
		if len(resources) == 0 {
			return nil, contract, line, false
		}
		for _, r := range resources {
			add(r, "")
		}
	case "pa.dtr":
		// The pinned DTR package defines both the collection Bundle and optional
		// operation wrapper at all supported lines (DTRDef and dtr.go).
		if in.Exchange.operation != shnsdk.FrameOperationQuestionnairePackage {
			return nil, contract, line, false
		}
		def, _ := shnsdk.DTRLineDef(line)
		profile := shnsdk.QuestionnairePackageInputProfile
		if in.Direction == "response" {
			switch root["resourceType"] {
			case "Bundle":
				profile = certificationDTR + "DTR-QPackageBundle"
			case "Parameters":
				profile = certificationDTR + "dtr-qpackage-output-parameters"
			default:
				return nil, contract, line, false
			}
		}
		add(root, profile+"|"+def.PackageVersion)
	default:
		add(root, "")
	}
	if len(targets) == 0 || len(targets) > crdEmbeddedValidationMax {
		return nil, contract, line, false
	}
	return targets, contract, line, true
}

// observationBytesTarget accounts for a serialized validation target against
// the bounded asynchronous observer's shared memory budget.
func observationBytesTarget(in CheckInput, raw []byte) ([]byte, func(), bool) {
	if len(raw) == 0 {
		return nil, nil, false
	}
	if in.observation == nil {
		return raw, func() {}, true
	}
	if len(raw) > observationMessageLimit || !in.observation.reserve(len(raw)) {
		return nil, nil, false
	}
	return raw, func() { in.observation.release(len(raw)) }, true
}
func checkPASGraph(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	root, ok := deepDocument(in)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	if resourceIs(root, "OperationOutcome") {
		return CheckResult{State: CheckValid}
	}
	checkBundle := func(raw []byte) bool {
		graph, err := readPASResponseGraph(raw, in.Exchange.legType == "pas-claim-inquire")
		return err == nil && graph.validate() == nil
	}
	if resourceIs(root, "Parameters") {
		entries, _ := root["parameter"].([]any)
		for _, v := range entries {
			p, _ := v.(map[string]any)
			r, _ := p["resource"].(map[string]any)
			raw, release, ok := observationTarget(in, r)
			if !ok {
				return deepUnavailable("observation_body_budget")
			}
			valid := func() bool { defer release(); return checkBundle(raw) }()
			if !valid {
				return checkResult("pas.graph", false)
			}
		}
		return CheckResult{State: CheckValid}
	}
	return checkResult("pas.graph", checkBundle(in.Body))
}

func checkPASProvenance(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	root, ok := deepDocument(in)
	if !ok || !resourceIs(root, "Bundle") {
		return deepUnavailable("content_unreadable")
	}
	entries, ok := bundleEntries(root, true)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	type namedResource struct {
		url      string
		resource map[string]any
	}
	var proofs, reports, responses []namedResource
	seenFullURL := make(map[string]bool, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return deepUnavailable("content_unreadable")
		}
		resource, ok := entry["resource"].(map[string]any)
		if !ok {
			return deepUnavailable("content_unreadable")
		}
		fullURL, ok := entry["fullUrl"].(string)
		if !ok || fullURL == "" {
			return deepUnavailable("content_unreadable")
		}
		if seenFullURL[fullURL] {
			return deepUnavailable("content_ambiguous")
		}
		seenFullURL[fullURL] = true
		item := namedResource{url: fullURL, resource: resource}
		switch resource["resourceType"] {
		case "Provenance":
			proofs = append(proofs, item)
		case "DiagnosticReport":
			reports = append(reports, item)
		case "QuestionnaireResponse":
			responses = append(responses, item)
		}
	}
	if len(proofs) == 0 {
		return checkResult("pas.provenance", false)
	}
	if len(proofs) != 1 || len(reports) > 1 || len(responses) > 1 {
		return deepUnavailable("content_ambiguous")
	}
	supplemental := reports
	if len(supplemental) == 0 {
		supplemental = responses
	}
	if len(supplemental) != 1 {
		return checkResult("pas.provenance", false)
	}
	proof := proofs[0]
	agents, present := proof.resource["agent"]
	if !present {
		return checkResult("pas.provenance", false)
	}
	agentList, ok := agents.([]any)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	actor := false
	for _, raw := range agentList {
		agent, ok := raw.(map[string]any)
		if !ok {
			return deepUnavailable("content_unreadable")
		}
		who, ok := agent["who"].(map[string]any)
		if !ok {
			continue
		}
		if ref, ok := who["reference"].(string); ok && strings.TrimSpace(ref) != "" {
			actor = true
		}
		if id, ok := who["identifier"].(map[string]any); ok {
			system, _ := id["system"].(string)
			value, _ := id["value"].(string)
			if system != "" && strings.TrimSpace(value) != "" {
				actor = true
			}
		}
	}
	if !actor {
		return checkResult("pas.provenance", false)
	}
	targets, present := proof.resource["target"]
	if !present {
		return checkResult("pas.provenance", false)
	}
	targetList, ok := targets.([]any)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	unreadableRelative := false
	for _, raw := range targetList {
		target, ok := raw.(map[string]any)
		if !ok {
			return deepUnavailable("content_unreadable")
		}
		ref, _ := target["reference"].(string)
		if ref == supplemental[0].url {
			return CheckResult{State: CheckValid}
		}
		resourceType, id, relative := strings.Cut(ref, "/")
		if relative && resourceType == supplemental[0].resource["resourceType"] && id != "" && !strings.ContainsAny(id, "/?#") {
			proofID, ok := proof.resource["id"].(string)
			if !ok || proofID == "" {
				unreadableRelative = true
				continue
			}
			if base, ok := pasProvenanceOwnerBase(proof.url, proofID); ok && base+"/"+ref == supplemental[0].url {
				return CheckResult{State: CheckValid}
			}
		}
	}
	if unreadableRelative {
		return deepUnavailable("content_unreadable")
	}
	return checkResult("pas.provenance", false)
}

func pasProvenanceOwnerBase(fullURL, id string) (string, bool) {
	u, err := url.Parse(fullURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/Provenance/"+id) {
		return "", false
	}
	return strings.TrimSuffix(fullURL, "/Provenance/"+id), true
}

func checkQRAttestation(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	root, ok := deepDocument(in)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	good := true
	walkContentResources(root, func(r map[string]any) {
		if !resourceIs(r, "QuestionnaireResponse") {
			return
		}
		walkFenceItems(r, func(item map[string]any) bool {
			switch fenceItemOriginRole(item) {
			case "clinician":
				good = good && fenceClinicianAttestationDefect(item) == ""
			case "patient":
				good = good && fencePatientAttestationDefect(item) == ""
			}
			return good
		})
	})
	return checkResult("qr.attestation", good)
}
func walkContentResources(v any, fn func(map[string]any)) {
	switch v := v.(type) {
	case map[string]any:
		if _, ok := v["resourceType"].(string); ok {
			fn(v)
		}
		for _, k := range pasSortedKeys(v) {
			walkContentResources(v[k], fn)
		}
	case []any:
		for _, e := range v {
			walkContentResources(e, fn)
		}
	}
}
func crdResources(root map[string]any, direction string) ([]map[string]any, bool) {
	var resources []map[string]any
	good := true
	add := func(v any) {
		if v == nil {
			return
		}
		r, ok := v.(map[string]any)
		if !ok {
			good = false
			return
		}
		if _, ok := r["resourceType"].(string); !ok {
			good = false
			return
		}
		resources = append(resources, r)
	}
	if direction == "request" {
		c, _ := root["context"].(map[string]any)
		for _, k := range []string{"draftOrders", "orders"} {
			if v, present := c[k]; present {
				add(v)
			}
		}
		if v, present := root["prefetch"]; present {
			p, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			for _, k := range pasSortedKeys(p) {
				add(p[k])
			}
		}
	} else {
		actions := func(raw any) {
			arr, ok := raw.([]any)
			if !ok && raw != nil {
				good = false
			}
			for _, v := range arr {
				a, ok := v.(map[string]any)
				if !ok {
					good = false
					continue
				}
				if r, present := a["resource"]; present {
					add(r)
				}
			}
		}
		actions(root["systemActions"])
		cards, _ := root["cards"].([]any)
		for _, v := range cards {
			c, _ := v.(map[string]any)
			ss, _ := c["suggestions"].([]any)
			for _, v := range ss {
				s, _ := v.(map[string]any)
				actions(s["actions"])
			}
		}
	}
	return resources, good
}
func checkCRDResources(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	root, ok := deepDocument(in)
	if !ok {
		return deepUnavailable("content_unreadable")
	}
	resources, ok := crdResources(root, in.Direction)
	if !ok {
		return checkResult("crd.resources", false)
	}
	if len(resources) > crdEmbeddedValidationMax {
		return deepUnavailable("resource_budget")
	}
	good := true
	for _, r := range resources {
		walkContentResources(r, func(r map[string]any) {
			rt, _ := r["resourceType"].(string)
			_, known := patientBindingPaths[rt]
			good = good && known && rt != "Binary"
		})
	}
	return checkResult("crd.resources", good)
}
func checkCDSResponse(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	if _, ok := deepDocument(in); !ok {
		return deepUnavailable("content_unreadable")
	}
	contract, line, ok := strings.Cut(in.DeclaredVersion, "@")
	if !ok || contract != "pa.crd" {
		return deepUnavailable("version_unavailable")
	}
	if _, ok := shnsdk.CRDLineDef(line); !ok {
		return deepUnavailable("version_unavailable")
	}
	out := CheckResult{State: CheckValid}
	for _, v := range shnsdk.CheckCDSHooksResponse(in.Body, line) {
		out.Issues = append(out.Issues, CheckIssue{string(v.Severity), v.Rule})
		if v.Severity == shnsdk.SeverityError {
			out.State = CheckInvalid
			out.Code = "cds.response"
			out.Severity = "error"
		}
	}
	return out
}
func checkVersionConsistency(ctx context.Context, in CheckInput) CheckResult {
	if ctx.Err() != nil {
		return deepUnavailable("checker_canceled")
	}
	if in.DeclaredVersion == "" || in.Exchange.contractVersion == "" {
		return deepUnavailable("version_unavailable")
	}
	return checkResult("version.consistency", in.DeclaredVersion == in.Exchange.contractVersion)
}

// validateGovernedEvidence preserves explicit unavailable outcomes at existing
// resource-validation callers. New message handlers consume the same baseline
// profile verdict through DeepRules.
func (g *Gateway) validateGovernedEvidence(ctx context.Context, fc findingContext, v shnsdk.Validator, body []byte, dir, line, profile string) govResult {
	ev, err := delegateValidatorEvidence(ctx, v, body, profile)
	result := validationResult(ev.Profile, err)
	if result.State == CheckValid && len(result.Issues) == 0 {
		return govResult{}
	}
	action, decision := "allowed", ""
	if result.State != CheckValid {
		action, decision = "refused", "refused"
	}
	g.emitFinding(ConformanceFinding{Kind: string(kindForDirection(dir, false)), Direction: dir, LegType: fc.LegType, CorrelationID: fc.CorrelationID, Seam: fc.Seam, Whose: fc.Whose, Line: line, Profile: profile, Level: g.policy().Level().String(), Rule: "fhir.profile", Action: action, Decision: decision, State: result.State, CheckIssues: result.Issues, PayloadSHA256: sha256hex(body)})
	switch result.State {
	case CheckInvalid:
		return govResult{Status: 422, Msg: dir + " validation failed", Issues: []string{"fhir.profile"}}
	case CheckValid:
		return govResult{}
	default:
		return govResult{Status: 503, Msg: "conformance_unavailable: fhir.profile"}
	}
}

// observationTarget serializes one resource at a time. The original job and this
// target overlap in the shared budget until its actual validator/checker returns.
// Native enforcement retains its existing encoder behavior.
func observationTarget(in CheckInput, resource map[string]any) ([]byte, func(), bool) {
	if in.observation == nil {
		raw, err := json.Marshal(resource)
		return raw, func() {}, err == nil
	}
	n, ok := observationjson.Size(resource, observationMessageLimit)
	if !ok || !in.observation.reserve(n) {
		return nil, nil, false
	}
	raw := observationjson.Append(make([]byte, 0, n), resource)
	return raw, func() { in.observation.release(n) }, true
}
