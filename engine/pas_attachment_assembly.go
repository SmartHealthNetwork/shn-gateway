package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

var errPASParticipantSource = errors.New("PAS participant Claim source unavailable")

func authoredPASBuildStatus(err error) int {
	if errors.Is(err, errPASParticipantSource) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// pasFactsForOrder selects exactly one participant-held Claim whose sole item
// names this order. Other Claims from the bounded patient search are unrelated;
// a matching but incomplete or ambiguous Claim is a refusal.
func pasFactsForOrder(claims [][]byte, order []byte) (*shnsdk.PASLineItemFacts, error) {
	var selected *shnsdk.PASLineItemFacts
	for _, claim := range claims {
		facts, err := shnsdk.PASClaimFactsFromSource(claim, order)
		if errors.Is(err, shnsdk.ErrPASSourceMismatch) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errPASParticipantSource, err)
		}
		if selected != nil {
			return nil, fmt.Errorf("%w: more than one Claim names the order", errPASParticipantSource)
		}
		selected = facts
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: no Claim names the order", errPASParticipantSource)
	}
	return selected, nil
}

func (g *Gateway) pasFactsFromSource(ctx context.Context, member string, sourceOrder, order []byte) (*shnsdk.PASLineItemFacts, error) {
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
	if err != nil {
		return nil, fmt.Errorf("%w: patient source read failed: %v", errPASParticipantSource, err)
	}
	if !found || !strings.HasPrefix(ref, "Patient/") {
		return nil, fmt.Errorf("%w: patient record absent", errPASParticipantSource)
	}
	// The caller carries the exact held order bytes from its one SoR acquisition,
	// before the registered patient-name rewrite. PAS independently resolves the
	// current patient and searches fresh Claims; it must not open a different order
	// later and silently attach those facts to what CRD actually submitted.
	if len(sourceOrder) == 0 {
		return nil, fmt.Errorf("%w: held source order absent", errPASParticipantSource)
	}
	var held struct {
		ResourceType string `json:"resourceType"`
		Subject      struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	if json.Unmarshal(sourceOrder, &held) != nil || (held.ResourceType != "ServiceRequest" && held.ResourceType != "DeviceRequest") || held.Subject.Reference != ref {
		return nil, fmt.Errorf("%w: source order patient differs from resolved patient", errPASParticipantSource)
	}
	expectedOrder := sourceOrder
	if ref != "Patient/"+member {
		expectedOrder, err = namePatientByMember(sourceOrder, strings.TrimPrefix(ref, "Patient/"), member)
		if err != nil {
			return nil, fmt.Errorf("%w: source order patient rewrite invalid", errPASParticipantSource)
		}
	}
	var expected, submitted any
	if json.Unmarshal(expectedOrder, &expected) != nil || json.Unmarshal(order, &submitted) != nil || !reflect.DeepEqual(expected, submitted) {
		return nil, fmt.Errorf("%w: submitted order differs from held order", errPASParticipantSource)
	}
	result := runSoRSearch(ctx, g.cfg.SoR, "Claim", strings.TrimPrefix(ref, "Patient/"), false)
	if result.Outcome != SearchOK && result.Outcome != SearchZero {
		return nil, fmt.Errorf("%w: Claim search %s", errPASParticipantSource, result.Outcome)
	}
	claims := make([][]byte, 0, len(result.matches))
	for _, match := range result.matches {
		claims = append(claims, result.pages[match.page][match.start:match.end])
	}
	return pasFactsForOrder(claims, sourceOrder)
}

func (g *Gateway) buildAuthoredPASSubmit(ctx context.Context, line, member string, sourceOrder []byte, in shnsdk.ConformantClaimInputs) ([]byte, error) {
	if def, ok := shnsdk.PASLineDef(line); ok && def.ClaimItemLineDetailRequired {
		facts, err := g.pasFactsFromSource(ctx, member, sourceOrder, in.SR)
		if err != nil {
			return nil, err
		}
		in.ItemFacts = facts
	}
	return buildAuthoredPASSubmit(line, in)
}

// buildAuthoredPASSubmit preserves the holder-authored attachment while the
// SDK owns the PAS envelope and its final resource identities.
func buildAuthoredPASSubmit(line string, in shnsdk.ConformantClaimInputs) ([]byte, error) {
	body, err := shnsdk.BuildConformantClaimBundleAtLine(line, in)
	if err != nil || len(in.QR) == 0 {
		return body, err
	}
	return completeAuthoredPASAttachment(body, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs)
}

func buildAuthoredPASUpdate(line string, in shnsdk.ConformantClaimUpdateInputs) ([]byte, error) {
	body, err := shnsdk.BuildConformantClaimUpdateBundleAtLine(line, in)
	if err != nil {
		return nil, err
	}
	order, err := authoredPASObject(in.SR)
	if err != nil || dtrString(order["resourceType"].raw) != "ServiceRequest" || len(in.QR) == 0 {
		return nil, fmt.Errorf("authored PAS update requires a ServiceRequest and QuestionnaireResponse")
	}
	return completeAuthoredPASAttachment(body, in.QR, in.SR, in.CoverageRef, in.AbsoluteRefs)
}

type authoredPASValue struct {
	raw        json.RawMessage
	start, end int
}

// Decode each value with its original byte span. Duplicate object members are
// ambiguous identities and cannot be used for an attachment replacement.
func authoredPASObject(raw []byte) (map[string]authoredPASValue, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid PAS JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected PAS object")
	}
	fields := map[string]authoredPASValue{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("invalid PAS member")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate PAS member %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		end := int(decoder.InputOffset())
		fields[name] = authoredPASValue{raw: value, start: end - len(value), end: end}
	}
	return fields, nil
}
func authoredPASArray(raw []byte) ([]authoredPASValue, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid PAS array")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("expected PAS array")
	}
	var values []authoredPASValue
	for decoder.More() {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		end := int(decoder.InputOffset())
		values = append(values, authoredPASValue{raw: value, start: end - len(value), end: end})
	}
	return values, nil
}

var authoredPASResourceType = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

func authoredPASResourceIdentity(fields map[string]authoredPASValue) (string, string, error) {
	typ, id := dtrString(fields["resourceType"].raw), dtrString(fields["id"].raw)
	if !authoredPASResourceType.MatchString(typ) || !pasSafeResourceID(id) {
		return "", "", fmt.Errorf("invalid PAS resource identity")
	}
	return typ, id, nil
}

// completeAuthoredPASAttachment is a construction boundary for an already
// materialized holder-owned QR. It never accepts an inbound resource to repair.
// Only the actual QR entry span changes; every other SDK-produced byte stays put.
func completeAuthoredPASAttachment(body, sourceQR, sourceOrder []byte, coverageRef string, absolute bool) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, fmt.Errorf("invalid authored PAS attachment identity") }
	order, err := authoredPASObject(sourceOrder)
	if err != nil {
		return fail()
	}
	orderType, orderID, err := authoredPASResourceIdentity(order)
	if err != nil || (orderType != "ServiceRequest" && orderType != "DeviceRequest") {
		return fail()
	}
	if !strings.HasPrefix(coverageRef, "Coverage/") || !pasSafeResourceID(strings.TrimPrefix(coverageRef, "Coverage/")) {
		return fail()
	}
	qr, err := authoredPASObject(sourceQR)
	if err != nil || dtrString(qr["resourceType"].raw) != "QuestionnaireResponse" {
		return fail()
	}
	if id, exists := qr["id"]; exists && !pasSafeResourceID(dtrString(id.raw)) {
		return fail()
	}
	root, err := authoredPASObject(body)
	if err != nil || dtrString(root["resourceType"].raw) != "Bundle" {
		return fail()
	}
	entries, err := authoredPASArray(root["entry"].raw)
	if err != nil || len(entries) == 0 {
		return fail()
	}
	refs := map[string]string{}
	fullURLs := map[string]bool{}
	required := map[string]string{}
	var qrSpan authoredPASValue
	for _, entry := range entries {
		fields, err := authoredPASObject(entry.raw)
		if err != nil {
			return fail()
		}
		resource, err := authoredPASObject(fields["resource"].raw)
		if err != nil {
			return fail()
		}
		typ, id, err := authoredPASResourceIdentity(resource)
		if err != nil {
			return fail()
		}
		relative := typ + "/" + id
		full := dtrString(fields["fullUrl"].raw)
		u, err := url.Parse(full)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.Path, "/"+relative) || refs[relative] != "" || fullURLs[full] {
			return fail()
		}
		refs[relative] = full
		fullURLs[full] = true
		if typ == "QuestionnaireResponse" || typ == "Coverage" || typ == orderType {
			if required[typ] != "" {
				return fail()
			}
			required[typ] = relative
			if typ == "QuestionnaireResponse" {
				qrSpan = fields["resource"]
				qrSpan.start += root["entry"].start + entry.start
				qrSpan.end += root["entry"].start + entry.start
			}
		}
	}
	for _, typ := range []string{"QuestionnaireResponse", "Coverage", orderType} {
		if required[typ] == "" {
			return fail()
		}
	}
	completed := make(map[string]json.RawMessage, len(qr))
	for key, value := range qr {
		completed[key] = value.raw
	}
	completed["id"] = dtrRaw(strings.TrimPrefix(required["QuestionnaireResponse"], "QuestionnaireResponse/"))
	if ext, exists := qr["extension"]; exists {
		extensions, err := authoredPASArray(ext.raw)
		if err != nil {
			return fail()
		}
		output := make([]json.RawMessage, len(extensions))
		for i, value := range extensions {
			fields, err := authoredPASObject(value.raw)
			if err != nil {
				return fail()
			}
			output[i] = value.raw
			canonical := dtrString(fields["url"].raw)
			if canonical != dtrExtensionBase+"qr-context" && canonical != dtrExtensionBase+"qr-coverage" {
				continue
			}
			reference, err := authoredPASObject(fields["valueReference"].raw)
			if err != nil {
				return fail()
			}
			if _, present := reference["reference"]; !present {
				if logical, err := dtrLogicalContextReference(fields["valueReference"].raw, canonical); err != nil {
					return fail()
				} else if logical {
					continue
				}
			}
			current := dtrString(reference["reference"].raw)
			if current == "" {
				return fail()
			}
			target := ""
			if current == coverageRef {
				target = required["Coverage"]
			}
			if current == orderType+"/"+orderID {
				target = required[orderType]
			}
			if target == "" {
				continue
			}
			refOut := map[string]json.RawMessage{}
			for key, v := range reference {
				refOut[key] = v.raw
			}
			refOut["reference"] = dtrRaw(target)
			extOut := map[string]json.RawMessage{}
			for key, v := range fields {
				extOut[key] = v.raw
			}
			extOut["valueReference"], err = json.Marshal(refOut)
			if err != nil {
				return nil, err
			}
			output[i], err = json.Marshal(extOut)
			if err != nil {
				return nil, err
			}
		}
		completed["extension"], err = json.Marshal(output)
		if err != nil {
			return nil, err
		}
	}
	replacement, err := json.Marshal(completed)
	if err != nil {
		return nil, err
	}
	if absolute {
		replacement, err = authoredPASAbsoluteRefs(replacement, refs, false)
		if err != nil {
			return nil, err
		}
	}
	if bytes.Equal(replacement, qrSpan.raw) {
		return body, nil
	}
	out := make([]byte, 0, len(body)-len(qrSpan.raw)+len(replacement))
	out = append(out, body[:qrSpan.start]...)
	out = append(out, replacement...)
	out = append(out, body[qrSpan.end:]...)
	return out, nil
}

// Retain the existing patient-compartment anchor semantics, including nested
// references in that protected subtree. Raw values keep their numeric lexemes.
func authoredPASAbsoluteRefs(raw []byte, refs map[string]string, protect bool) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("missing PAS value")
	}
	switch trimmed[0] {
	case '{':
		fields, err := authoredPASObject(raw)
		if err != nil {
			return nil, err
		}
		anchor := ""
		switch dtrString(fields["resourceType"].raw) {
		case "Coverage":
			anchor = "beneficiary"
		case "ServiceRequest", "DeviceRequest":
			anchor = "subject"
		}
		changed := false
		values := map[string]json.RawMessage{}
		for key, field := range fields {
			value := field.raw
			if key == "reference" {
				if !protect {
					if full := refs[dtrString(value)]; full != "" {
						value = dtrRaw(full)
					}
				}
			} else {
				value, err = authoredPASAbsoluteRefs(value, refs, protect || (anchor != "" && key == anchor))
				if err != nil {
					return nil, err
				}
			}
			changed = changed || !bytes.Equal(value, field.raw)
			values[key] = value
		}
		if changed {
			return json.Marshal(values)
		}
	case '[':
		items, err := authoredPASArray(raw)
		if err != nil {
			return nil, err
		}
		values := make([]json.RawMessage, len(items))
		changed := false
		for i, item := range items {
			values[i], err = authoredPASAbsoluteRefs(item.raw, refs, protect)
			if err != nil {
				return nil, err
			}
			changed = changed || !bytes.Equal(values[i], item.raw)
		}
		if changed {
			return json.Marshal(values)
		}
	}
	return raw, nil
}

// The policy describes exact occurrences in a verified holder-authored attachment.
// It is local to request evidence completion and never authorizes a foreign graph.
type authoredPASReferencePolicy struct {
	owner      string
	resource   string
	references map[string]string
}

func (p *authoredPASReferencePolicy) matches(g *pasGraph) bool {
	if p == nil {
		return true
	}
	owner := g.byURL[p.owner]
	if owner == nil {
		return false
	}
	raw, err := json.Marshal(owner.resource)
	return err == nil && string(raw) == p.resource
}

func (p *authoredPASReferencePolicy) allows(owner *pasGraphEntry, path string, node map[string]any) bool {
	if p == nil || owner == nil || owner.fullURL != p.owner {
		return false
	}
	expected, ok := p.references[path]
	if !ok {
		return false
	}
	raw, err := json.Marshal(node)
	return err == nil && string(raw) == expected
}
func pasReferencePath(parent, key string) string {
	return parent + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
func authoredPASReferencePolicyFor(body, sourceQR, sourceOrder []byte, coverageRef string, absolute bool) (*authoredPASReferencePolicy, error) {
	verified, err := completeAuthoredPASAttachment(body, sourceQR, sourceOrder, coverageRef, absolute)
	if err != nil || !bytes.Equal(verified, body) {
		return nil, fmt.Errorf("authored attachment does not match source")
	}
	source, err := authoredPASObject(sourceQR)
	if err != nil {
		return nil, err
	}
	order, err := authoredPASObject(sourceOrder)
	if err != nil {
		return nil, err
	}
	typ, id, err := authoredPASResourceIdentity(order)
	if err != nil {
		return nil, err
	}
	active := typ + "/" + id
	policy := &authoredPASReferencePolicy{references: map[string]string{}}
	var bundle map[string]any
	if decodePASObject(body, &bundle) != nil {
		return nil, fmt.Errorf("invalid authored bundle")
	}
	var actual map[string]any
	for _, entry := range bundle["entry"].([]any) {
		e := entry.(map[string]any)
		r := e["resource"].(map[string]any)
		if r["resourceType"] == "QuestionnaireResponse" {
			policy.owner = e["fullUrl"].(string)
			actual = r
		}
	}
	encodedResource, err := json.Marshal(actual)
	if err != nil {
		return nil, err
	}
	policy.resource = string(encodedResource)
	ext, present := source["extension"]
	if !present {
		return policy, nil
	}
	extensions, err := authoredPASArray(ext.raw)
	if err != nil {
		return nil, err
	}
	activeCount, coverageCount := 0, 0
	for i, e := range extensions {
		fields, err := authoredPASObject(e.raw)
		if err != nil {
			return nil, err
		}
		canonical := dtrString(fields["url"].raw)
		if canonical != dtrExtensionBase+"qr-context" && canonical != dtrExtensionBase+"qr-coverage" {
			continue
		}
		reference, err := authoredPASObject(fields["valueReference"].raw)
		if err != nil {
			return nil, err
		}
		ref := dtrString(reference["reference"].raw)
		if ref == active {
			activeCount++
			continue
		}
		if ref == coverageRef {
			coverageCount++
			continue
		}
		kind, optionalID, _ := strings.Cut(ref, "/")
		if canonical != dtrExtensionBase+"qr-context" || (kind != "ServiceRequest" && kind != "DeviceRequest") {
			continue
		}
		if !dtrLocalReference.MatchString(ref) || !pasSafeResourceID(optionalID) {
			return nil, fmt.Errorf("invalid optional order identity")
		}
		if supplied, present := reference["type"]; present && dtrReferenceType(dtrString(supplied.raw)) != kind {
			return nil, fmt.Errorf("optional order type mismatch")
		}
		actualExt := actual["extension"].([]any)[i].(map[string]any)
		node, ok := actualExt["valueReference"].(map[string]any)
		if !ok || node["reference"] != ref {
			return nil, fmt.Errorf("optional context changed during assembly")
		}
		encoded, err := json.Marshal(node)
		if err != nil {
			return nil, err
		}
		policy.references["/extension/"+strconv.Itoa(i)+"/valueReference"] = string(encoded)
	}
	if len(policy.references) > 0 && (activeCount != 1 || coverageCount != 1) {
		return nil, fmt.Errorf("optional context lacks unique active identities")
	}
	return policy, nil
}
