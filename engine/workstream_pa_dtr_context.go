package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const (
	dtrQRCanonical   = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse"
	baseQRProfile    = "http://hl7.org/fhir/StructureDefinition/QuestionnaireResponse|4.0.1"
	dtrExtensionBase = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"
)

var dtrLocalReference = regexp.MustCompile(`^(Patient|Coverage|ServiceRequest|DeviceRequest)/[A-Za-z0-9.-]{1,64}$`)

func dtrQRProfile(line string) (string, error) {
	def, ok := shnsdk.DTRLineDef(line)
	if !ok {
		return "", fmt.Errorf("unsupported DTR line")
	}
	return dtrQRCanonical + "|" + def.PackageVersion, nil
}
func (g *Gateway) validateDTRQuestionnaireResponse(ctx context.Context, raw []byte, line string) (int, string) {
	profile, err := dtrQRProfile(line)
	if err != nil {
		return http.StatusInternalServerError, "unsupported DTR validation line"
	}
	return g.validateFHIRForContract(ctx, raw, "egress", "pa.dtr", line, profile)
}

// completeDTRContext preserves the local population artifact and creates an
// explicitly profiled outgoing QR from the holder's established administrative facts.
func (g *Gateway) completeDTRContext(ctx context.Context, raw []byte, line string, qc shnsdk.QRContext) ([]byte, int, string) {
	out, err := composeDTRContextAtLine(raw, line, qc)
	if err != nil {
		return nil, http.StatusBadGateway, "DTR administrative context conflicts with the exchange"
	}
	status, msg := g.validateDTRQuestionnaireResponse(ctx, out, line)
	return out, status, msg
}

func dtrObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("expected object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("expected one object")
	}
	return obj, nil
}
func dtrString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
func dtrRaw(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

// composeDTRContextAtLine adds administrative context only. Existing clinical
// content remains raw, including number lexemes, software attribution and diagnostics.
// A conflicting supplied value is refused; the composer never translates it.
func composeDTRContextAtLine(raw []byte, line string, qc shnsdk.QRContext) ([]byte, error) {
	profile, err := dtrQRProfile(line)
	if err != nil {
		return nil, err
	}
	def, _ := shnsdk.DTRLineDef(line)
	for _, ref := range []string{qc.PatientRef, qc.CoverageRef, qc.OrderRef} {
		_, referenceID, _ := strings.Cut(ref, "/")
		if !dtrLocalReference.MatchString(ref) || !pasSafeResourceID(referenceID) {
			return nil, fmt.Errorf("invalid administrative reference")
		}
	}
	if !strings.HasPrefix(qc.PatientRef, "Patient/") || !strings.HasPrefix(qc.CoverageRef, "Coverage/") || (!strings.HasPrefix(qc.OrderRef, "ServiceRequest/") && !strings.HasPrefix(qc.OrderRef, "DeviceRequest/")) {
		return nil, fmt.Errorf("incorrect administrative reference type")
	}
	obj, err := dtrObject(raw)
	if err != nil {
		return nil, err
	}
	if dtrString(obj["resourceType"]) != "QuestionnaireResponse" {
		return nil, fmt.Errorf("expected QuestionnaireResponse")
	}
	subject, err := dtrObject(obj["subject"])
	if err != nil || dtrString(subject["reference"]) != qc.PatientRef {
		return nil, fmt.Errorf("subject does not match")
	}
	var extensions []json.RawMessage
	if old, ok := obj["extension"]; ok {
		if bytes.Equal(bytes.TrimSpace(old), []byte("null")) || json.Unmarshal(old, &extensions) != nil {
			return nil, fmt.Errorf("invalid extension array")
		}
	}
	coverageURL := dtrExtensionBase + "qr-context"
	if def.SingleCoverageConstraint {
		coverageURL = dtrExtensionBase + "qr-coverage"
	}
	var coverage, order, intended bool
	for _, rawExt := range extensions {
		ext, err := dtrObject(rawExt)
		if err != nil {
			return nil, fmt.Errorf("invalid extension")
		}
		url := dtrString(ext["url"])
		if url == "" {
			return nil, fmt.Errorf("invalid extension URL")
		}
		if url != dtrExtensionBase+"qr-context" && url != dtrExtensionBase+"qr-coverage" && url != dtrExtensionBase+"intendedUse" {
			continue
		}
		valueKey := "valueReference"
		if url == dtrExtensionBase+"intendedUse" {
			valueKey = "valueCodeableConcept"
		}
		for key := range ext {
			if (strings.HasPrefix(key, "value") && key != valueKey) || key == "extension" {
				return nil, fmt.Errorf("ambiguous administrative extension")
			}
		}
		value, err := dtrObject(ext[valueKey])
		if err != nil {
			return nil, fmt.Errorf("invalid administrative value")
		}
		if url == dtrExtensionBase+"intendedUse" {
			if intended {
				return nil, fmt.Errorf("duplicate intended use")
			}
			var codings []map[string]json.RawMessage
			if json.Unmarshal(value["coding"], &codings) != nil {
				return nil, fmt.Errorf("invalid intended use")
			}
			match := false
			for _, coding := range codings {
				system, code := dtrString(coding["system"]), dtrString(coding["code"])
				if (system == "http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp" || system == "http://hl7.org/fhir/us/davinci-crd/CodeSystem/coverage-information-codes") && system != def.IntendedUseCodeSystem {
					return nil, fmt.Errorf("mixed intended use lines")
				}
				if system == def.IntendedUseCodeSystem {
					if code != "withpa" {
						return nil, fmt.Errorf("conflicting intended use")
					}
					match = true
				}
			}
			if !match {
				return nil, fmt.Errorf("intended use does not match selected line")
			}
			intended = true
			continue
		}
		if _, present := value["reference"]; !present {
			if logical, err := dtrLogicalContextReference(ext[valueKey], url); err != nil {
				return nil, err
			} else if logical {
				continue
			}
		}
		for _, key := range []string{"reference", "type"} {
			if rawValue, present := value[key]; present && dtrString(rawValue) == "" {
				return nil, fmt.Errorf("invalid reference primitive")
			}
		}
		ref := dtrString(value["reference"])
		kind, referenceID, _ := strings.Cut(ref, "/")
		if typ := dtrString(value["type"]); typ != "" && kind != "" && typ != kind && typ != "http://hl7.org/fhir/StructureDefinition/"+kind {
			return nil, fmt.Errorf("reference type disagrees with identity")
		}
		if url == dtrExtensionBase+"qr-coverage" || kind == "Coverage" || dtrReferenceType(dtrString(value["type"])) == "Coverage" {
			if url != coverageURL || ref != qc.CoverageRef || coverage {
				return nil, fmt.Errorf("conflicting coverage context")
			}
			coverage = true
		} else if kind == "ServiceRequest" || kind == "DeviceRequest" || dtrReferenceType(dtrString(value["type"])) == "ServiceRequest" || dtrReferenceType(dtrString(value["type"])) == "DeviceRequest" {
			if !dtrLocalReference.MatchString(ref) || !pasSafeResourceID(referenceID) {
				return nil, fmt.Errorf("invalid order context identity")
			}
			// Only the established active identity asserts this exchange's order.
			// A different optional order remains unchanged in the sourced context.
			if ref == qc.OrderRef {
				if order {
					return nil, fmt.Errorf("conflicting order context")
				}
				order = true
			}
		} else if ref == "" && value["identifier"] == nil {
			return nil, fmt.Errorf("context has no reference identity")
		}
	}
	if !coverage {
		extensions = append(extensions, dtrRaw(map[string]any{"url": coverageURL, "valueReference": map[string]string{"reference": qc.CoverageRef}}))
	}
	if !order {
		extensions = append(extensions, dtrRaw(map[string]any{"url": dtrExtensionBase + "qr-context", "valueReference": map[string]string{"reference": qc.OrderRef}}))
	}
	if !intended {
		extensions = append(extensions, dtrRaw(map[string]any{"url": dtrExtensionBase + "intendedUse", "valueCodeableConcept": map[string]any{"coding": []map[string]string{{"system": def.IntendedUseCodeSystem, "code": "withpa", "display": "Information needed for a prior authorization"}}}}))
	}
	meta := map[string]json.RawMessage{}
	if old, ok := obj["meta"]; ok {
		meta, err = dtrObject(old)
		if err != nil {
			return nil, fmt.Errorf("invalid meta")
		}
	}
	var profiles []string
	if old, ok := meta["profile"]; ok {
		if bytes.Equal(bytes.TrimSpace(old), []byte("null")) || json.Unmarshal(old, &profiles) != nil {
			return nil, fmt.Errorf("invalid profile declarations")
		}
	}
	found := false
	for _, p := range profiles {
		canonical, version, versioned := strings.Cut(p, "|")
		if canonical == dtrQRCanonical && versioned && version != def.PackageVersion {
			return nil, fmt.Errorf("conflicting DTR profile line")
		}
		if p == profile {
			found = true
		}
	}
	if !found {
		profiles = append(profiles, profile)
	}
	meta["profile"] = dtrRaw(profiles)
	obj["meta"] = dtrRaw(meta)
	obj["extension"] = dtrRaw(extensions)
	return json.Marshal(obj)
}

func dtrReferenceType(value string) string {
	return strings.TrimPrefix(value, "http://hl7.org/fhir/StructureDefinition/")
}

// A logical optional context retains its identifier without claiming either of
// the exchange's known literal administrative identities. No target is acquired.
func dtrLogicalContextReference(raw []byte, canonical string) (bool, error) {
	fields, err := authoredPASObject(raw)
	if err != nil {
		return false, err
	}
	if _, present := fields["reference"]; present {
		return false, fmt.Errorf("logical context has a literal reference member")
	}
	if canonical != dtrExtensionBase+"qr-context" {
		return false, fmt.Errorf("logical coverage cannot identify active coverage")
	}
	if typ, present := fields["type"]; present {
		kind := dtrReferenceType(dtrString(typ.raw))
		if !authoredPASResourceType.MatchString(kind) || kind == "Coverage" || kind == "ServiceRequest" || kind == "DeviceRequest" {
			return false, fmt.Errorf("invalid logical context type")
		}
	}
	identifier, err := authoredPASObject(fields["identifier"].raw)
	if err != nil || dtrString(identifier["value"].raw) == "" {
		return false, fmt.Errorf("context has no logical identity")
	}
	if system, present := identifier["system"]; present && dtrString(system.raw) == "" {
		return false, fmt.Errorf("invalid logical identifier system")
	}
	return true, nil
}
