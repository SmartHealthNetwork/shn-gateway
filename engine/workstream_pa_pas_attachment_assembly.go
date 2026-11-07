//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildAuthoredPASSubmit preserves the holder-authored attachment while the
// SDK owns the PAS envelope and its final resource identities. Each extend
// step runs on the SDK's bundle before the attachment is completed, so the
// completion sees every entry the request carries.
func buildAuthoredPASSubmit(line string, in shnsdk.ConformantClaimInputs, extend ...func([]byte) ([]byte, error)) ([]byte, error) {
	body, err := shnsdk.BuildConformantClaimBundleAtLineCarried(line, in)
	if err != nil {
		return body, err
	}
	if err := builtEntriesNamed(body, pasBuildReIDs(in.SR, false)); err != nil {
		return nil, err
	}
	if body, err = extendAuthoredPAS(body, extend); err != nil || len(in.QR) == 0 {
		return body, err
	}
	return completeAuthoredPASAttachment(body, in.QR, in.SR, in.CoverageRef)
}

func buildAuthoredPASUpdate(line string, in shnsdk.ConformantClaimUpdateInputs, extend ...func([]byte) ([]byte, error)) ([]byte, error) {
	body, err := shnsdk.BuildConformantClaimUpdateBundleAtLineCarried(line, in)
	if err != nil {
		return nil, err
	}
	if err := builtEntriesNamed(body, pasBuildReIDs(in.SR, len(in.DiagnosticReport) == 0)); err != nil {
		return nil, err
	}
	order, err := authoredPASObject(in.SR)
	if err != nil || dtrString(order["resourceType"].raw) != "ServiceRequest" || len(in.QR) == 0 {
		return nil, fmt.Errorf("authored PAS update requires a ServiceRequest and QuestionnaireResponse")
	}
	if body, err = extendAuthoredPAS(body, extend); err != nil {
		return nil, err
	}
	return completeAuthoredPASAttachment(body, in.QR, in.SR, in.CoverageRef)
}

// pasBuildReIDs is the set of types of the records the SDK's Carried build
// gives an id of its own, by the build's structure rather than by any id
// value: the order (its own type, read exactly) and the QuestionnaireResponse
// always, and an amendment's Provenance only where the amendment carries no
// report (withProvenance). The Provenance attributing a report is carried under
// its own id and is not re-identified.
func pasBuildReIDs(order []byte, withProvenance bool) map[string]bool {
	ids := map[string]bool{"QuestionnaireResponse": true}
	if m, err := exactMembers(order); err == nil {
		if typ := exactString(m, "resourceType"); typ != "" {
			ids[typ] = true
		}
	}
	if withProvenance {
		ids["Provenance"] = true
	}
	return ids
}

// builtEntriesNamed is the fidelity of the gateway's own PAS build, checked
// once, right after the SDK's Carried build, on every path of a submission or
// an amendment, before any other step; nothing is sent when it refuses
// (errRecordNotCarried, 422). Two of the build's own edits cannot be made:
//   - It addresses each record it places (its entry's fullUrl, and the
//     references the Claim and the Coverage make to it) by the record's
//     resourceType and id. A participant's record that states either only in
//     another spelling (an "ID", say) has no FHIR id, and the request would
//     name it by an id the participant never gave.
//   - It gives some records an id of its own (reIDs, pasBuildReIDs).
//     Where such a record spells its id another way, the build's id would sit
//     beside it, and the body would name the member twice under case folding:
//     one the network's message integrity refuses to seal (relay's duplicate
//     rule), since it can be read two ways. The re-id cannot be made. Once
//     those records are carried as written, a lone member spelled so is
//     carried. A record the build carries under its own id that itself names
//     its id twice so is the participant's own ambiguity, not this edit's.
//
// Every entry SHN builds itself states both exactly, and only once.
func builtEntriesNamed(body []byte, reIDs map[string]bool) error {
	bundle, err := exactMembers(body)
	if err != nil {
		return err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(bundle["entry"], &entries); err != nil {
		return err
	}
	for _, e := range entries {
		entry, err := exactMembers(e)
		if err != nil {
			return err
		}
		r, err := exactMembers(entry["resource"])
		if err != nil || exactString(r, "resourceType") == "" || exactString(r, "id") == "" {
			return fmt.Errorf("%w: it states its resourceType or id only in another spelling", errRecordNotCarried)
		}
		if reIDs[exactString(r, "resourceType")] {
			for name := range r {
				if name != "id" && foldName(name) == foldName("id") {
					return fmt.Errorf("%w: the build's own id for it cannot be set beside the id it spells another way", errRecordNotCarried)
				}
			}
		}
	}
	return nil
}

// extendAuthoredPAS runs the build's extend steps in order.
func extendAuthoredPAS(body []byte, extend []func([]byte) ([]byte, error)) ([]byte, error) {
	for _, step := range extend {
		var err error
		if body, err = step(body); err != nil {
			return nil, err
		}
	}
	return body, nil
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
func completeAuthoredPASAttachment(body, sourceQR, sourceOrder []byte, coverageRef string) ([]byte, error) {
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
	if bytes.Equal(replacement, qrSpan.raw) {
		return body, nil
	}
	out := make([]byte, 0, len(body)-len(qrSpan.raw)+len(replacement))
	out = append(out, body[:qrSpan.start]...)
	out = append(out, replacement...)
	out = append(out, body[qrSpan.end:]...)
	return out, nil
}

// The policy describes exact occurrences in a verified holder-authored attachment.
// It is local to request evidence completion and never authorizes a foreign graph.
type authoredPASReferencePolicy struct {
	owner      string
	resource   string
	references map[string]string
}

// Matches reports whether the policy was made for g (pasgraph.ReferencePolicy):
// a nil policy matches every graph.
func (p *authoredPASReferencePolicy) Matches(g *pasGraph) bool {
	if p == nil {
		return true
	}
	owner := g.ByURL[p.owner]
	if owner == nil {
		return false
	}
	raw, err := json.Marshal(owner.Resource)
	return err == nil && string(raw) == p.resource
}

// Allows reports whether node, at path inside owner, is an occurrence the
// policy names (pasgraph.ReferencePolicy): a nil policy allows none.
func (p *authoredPASReferencePolicy) Allows(owner *pasGraphEntry, path string, node map[string]any) bool {
	if p == nil || owner == nil || owner.FullURL != p.owner {
		return false
	}
	expected, ok := p.references[path]
	if !ok {
		return false
	}
	raw, err := json.Marshal(node)
	return err == nil && string(raw) == expected
}
func authoredPASReferencePolicyFor(body, sourceQR, sourceOrder []byte, coverageRef string) (*authoredPASReferencePolicy, error) {
	verified, err := completeAuthoredPASAttachment(body, sourceQR, sourceOrder, coverageRef)
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
