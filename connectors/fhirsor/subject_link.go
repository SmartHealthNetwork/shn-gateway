package fhirsor

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// This is the Patient.identifier system already used by the Audit Plane for a
// network PCI. A participant's source may retain a previously issued mapping;
// this connector never creates one.
const sourcePCISystem = "urn:shn:pci"

// sourcePCI accepts one unambiguous, already-issued identifier in an actual
// Patient returned by this participant's FHIR source. Missing linkage is absence.
func sourcePCI(raw []byte) (string, bool, error) {
	var p struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Identifier   []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ResourceType != "Patient" || p.ID == "" {
		return "", false, invalidResponse()
	}
	var pci string
	for _, identifier := range p.Identifier {
		if identifier.System != sourcePCISystem {
			continue
		}
		if !strings.HasPrefix(identifier.Value, "pci:") || len(identifier.Value) <= len("pci:") || pci != "" {
			return "", false, invalidResponse()
		}
		pci = identifier.Value
	}
	return pci, pci != "", nil
}

func sourcePatientID(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "Patient/") {
		return "", false
	}
	id := strings.TrimPrefix(ref, "Patient/")
	if id == "" || id == "." || id == ".." || len(id) > 64 {
		return "", false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			continue
		}
		return "", false
	}
	return id, true
}

func patientHasIdentifier(raw []byte, system, value string) bool {
	var p struct {
		ResourceType string `json:"resourceType"`
		Identifier   []struct {
			System string `json:"system"`
			Value  string `json:"value"`
		} `json:"identifier"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ResourceType != "Patient" {
		return false
	}
	for _, id := range p.Identifier {
		if id.System == system && id.Value == value {
			return true
		}
	}
	return false
}

// ResolveSubject looks up only references that this holder's authenticated FHIR
// source can actually resolve. An absolute reference to another base, or a
// Patient in another holder's namespace, is unavailable rather than re-keyed by
// coincidentally equal local resource IDs.
func (s *SoR) ResolveSubject(ctx context.Context, ref engine.PatientReference) (string, bool, error) {
	if s.holder == "" || ref.Holder != s.holder || ref.Value == "" {
		return "", false, nil
	}
	var raw []byte
	if ref.System == "fhir-relative" || ref.System == s.fc.BaseURL() {
		id, ok := sourcePatientID(ref.Value)
		if !ok {
			return "", false, nil
		}
		body, found, err := s.fc.Read(ctx, "Patient", id)
		if err != nil {
			return "", false, safeReadError(err)
		}
		if !found {
			return "", false, nil
		}
		var patient struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &patient) != nil || patient.ID != id {
			return "", false, invalidResponse()
		}
		raw = body
	} else {
		// URI systems are source-local only after an exact identifier search and
		// confirmation in the returned Patient. Unsupported foreign REST bases
		// cannot be treated as identifier systems.
		if ref.System == "" || ref.System == "fhir-relative" || strings.HasPrefix(ref.System, "http://") || strings.HasPrefix(ref.System, "https://") {
			return "", false, nil
		}
		bundle, err := s.fc.Search(ctx, "Patient", url.Values{"identifier": {ref.System + "|" + ref.Value}})
		if err != nil {
			return "", false, safeReadError(err)
		}
		if bundle == nil || len(bundle.Entry) == 0 {
			return "", false, nil
		}
		if len(bundle.Entry) != 1 || bundle.Total != nil && int(*bundle.Total) != 1 {
			return "", false, invalidResponse()
		}
		for _, link := range bundle.Link {
			if link.Relation == "next" {
				return "", false, invalidResponse()
			}
		}
		raw = bundle.Entry[0].Resource
		if !patientHasIdentifier(raw, ref.System, ref.Value) {
			return "", false, invalidResponse()
		}
	}
	return sourcePCI(raw)
}

var _ engine.SubjectReferenceResolver = (*SoR)(nil)
