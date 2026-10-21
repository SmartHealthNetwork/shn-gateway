package engine

import (
	"encoding/json"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PA workstream's certification adapter (certificationAdapter,
// species.go): the PAS and DTR message families certification recognises,
// the IG profile each is certified against per line and leg, and the lines a
// payload points to.

const certificationPAS = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
const certificationDTR = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"

// paCertification is the PA workstream's certification adapter.
type paCertification struct{}

func (paWorkstream) certification() certificationAdapter { return paCertification{} }

// species identifies only supported resource envelopes. A PAS response may
// also carry its original Claim; the response resource determines species.
func (paCertification) species(payload []byte) string {
	var r struct {
		ResourceType string
		Entry        []struct{ Resource struct{ ResourceType string } }
	}
	if json.Unmarshal(payload, &r) != nil {
		return ""
	}
	switch r.ResourceType {
	case "Claim", "ClaimResponse", "QuestionnaireResponse":
		return r.ResourceType
	case "Bundle":
		request := false
		for _, e := range r.Entry {
			if e.Resource.ResourceType == "ClaimResponse" {
				return "PASResponseBundle"
			}
			request = request || e.Resource.ResourceType == "Claim"
		}
		if request {
			return "PASRequestBundle"
		}
	}
	return ""
}

// certifies reports whether species is one the PA workstream certifies.
func (c paCertification) certifies(species string) bool {
	_, ok := c.profile(species, "2.0", "")
	return ok
}

func (paCertification) profile(species, line, legType string) (string, bool) {
	if legType != "" && legType != "pas-claim" && legType != "pas-claim-update" && legType != "pas-claim-inquire" {
		return "", false
	}
	if species == "QuestionnaireResponse" {
		d, ok := shnsdk.DTRLineDef(line)
		if !ok {
			return "", false
		}
		return certificationDTR + "dtr-questionnaireresponse|" + d.PackageVersion, true
	}
	d, ok := shnsdk.PASLineDef(line)
	if !ok {
		return "", false
	}
	// An inquiry is its own message family: the request Claim, its request Bundle and
	// the answer's response Bundle each have an inquiry profile of their own, and
	// certifying an inquiry against the submit profiles would report it invalid for a
	// profile it was never meant to meet. The 2.2.1 answer's Parameters wrapper is not
	// certified: species does not recognise it, because the IG governs that
	// wrapper by its OperationDefinition and declares no profile for it.
	inquiry := legType == "pas-claim-inquire"
	name := ""
	switch species {
	case "Claim":
		switch {
		case inquiry:
			name = "profile-claim-inquiry"
		case legType == "pas-claim-update":
			name = "profile-claim-update"
		default:
			name = "profile-claim"
		}
	case "ClaimResponse":
		name = "profile-claimresponse"
		if inquiry {
			name = "profile-claiminquiryresponse"
		}
	case "PASRequestBundle":
		name = "profile-pas-request-bundle"
		if inquiry {
			name = "profile-pas-inquiry-request-bundle"
		}
	case "PASResponseBundle":
		name = "profile-pas-response-bundle"
		if inquiry {
			name = "profile-pas-inquiry-response-bundle"
		}
	default:
		return "", false
	}
	return certificationPAS + name + "|" + d.PackageVersion, true
}

// lineClaims orders the Da Vinci lines 2.2, 2.1 and 2.0 for certifying
// payload, whatever its species: first the lines a versioned meta.profile claims
// (a PAS or DTR profile at that line's package version), then the lines a known
// structural marker points to, then the rest, each group in 2.2, 2.1, 2.0 order.
// It walks Bundle entries and Parameters parameter resources, so a DTR
// $questionnaire-package answer is read through its PackageBundle. claimed is
// the lines the payload itself points to, by a versioned meta.profile or a
// structural marker. Claims and markers order attempts only; every supported
// line is retained, including when extensions belong to an unknown namespace.
func (paCertification) lineClaims(payload []byte) ([]string, map[string]bool) {
	order := []string{"2.2", "2.1", "2.0"}
	claimed := map[string]bool{}
	var r map[string]any
	if json.Unmarshal(payload, &r) != nil {
		return order, claimed
	}
	claims := map[string]bool{}
	markers := map[string]bool{}
	var walk func(map[string]any)
	walk = func(m map[string]any) {
		meta, _ := m["meta"].(map[string]any)
		profiles, _ := meta["profile"].([]any)
		for _, p := range profiles {
			p, _ := p.(string)
			canonical, version, ok := strings.Cut(p, "|")
			if !ok {
				continue
			}
			for _, line := range order {
				d, _ := shnsdk.PASLineDef(line)
				q, _ := shnsdk.DTRLineDef(line)
				if strings.HasPrefix(canonical, certificationPAS) && version == d.PackageVersion || strings.HasPrefix(canonical, certificationDTR) && version == q.PackageVersion {
					claims[line] = true
				}
			}
		}
		resources := []map[string]any{m}
		if m["resourceType"] == "Claim" {
			items, _ := m["item"].([]any)
			for _, item := range items {
				r, _ := item.(map[string]any)
				resources = append(resources, r)
			}
		}
		for _, r := range resources {
			extensions, _ := r["extension"].([]any)
			for _, extension := range extensions {
				e, _ := extension.(map[string]any)
				switch e["url"] {
				case certificationPAS + "extension-certificationType", certificationPAS + "extension-serviceItemRequestType", certificationDTR + "qr-context":
					markers["2.1"] = true
				case certificationDTR + "qr-coverage":
					markers["2.2"] = true
				}
			}
		}
		entries, _ := m["entry"].([]any)
		if m["resourceType"] == "Parameters" {
			entries, _ = m["parameter"].([]any)
		}
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			resource, _ := entry["resource"].(map[string]any)
			walk(resource)
		}
	}
	walk(r)
	for line := range claims {
		claimed[line] = true
	}
	for line := range markers {
		claimed[line] = true
	}
	out := make([]string, 0, 3)
	for _, line := range order {
		if claims[line] {
			out = append(out, line)
		}
	}
	for _, line := range order {
		if markers[line] && !claims[line] {
			out = append(out, line)
		}
	}
	for _, line := range order {
		if !claims[line] && !markers[line] {
			out = append(out, line)
		}
	}
	return out, claimed
}
