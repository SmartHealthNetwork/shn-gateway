package engine

import (
	"encoding/json"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

const certificationPAS = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/"
const certificationDTR = "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/"

// detectSpecies identifies only supported resource envelopes. A PAS response
// may also carry its original Claim; the response resource determines species.
func detectSpecies(payload []byte) string {
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

func profileFor(species, line, legType string) (string, bool) {
	if legType != "" && legType != "pas-claim" && legType != "pas-claim-update" {
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
	name := ""
	switch species {
	case "Claim":
		name = "profile-claim"
		if legType == "pas-claim-update" {
			name = "profile-claim-update"
		}
	case "ClaimResponse":
		name = "profile-claimresponse"
	case "PASRequestBundle":
		name = "profile-pas-request-bundle"
	case "PASResponseBundle":
		name = "profile-pas-response-bundle"
	default:
		return "", false
	}
	return certificationPAS + name + "|" + d.PackageVersion, true
}

// Claims and known structural markers order attempts only; every supported
// line is retained, including when extensions belong to an unknown namespace.
func candidateOrder(species string, payload []byte) []string {
	if _, ok := profileFor(species, "2.0", ""); !ok {
		return nil
	}
	order := []string{"2.2", "2.1", "2.0"}
	var r map[string]any
	if json.Unmarshal(payload, &r) != nil {
		return order
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
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			resource, _ := entry["resource"].(map[string]any)
			walk(resource)
		}
	}
	walk(r)
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
	return out
}

func certificationSource(certified []string, target string) string {
	rank := map[string]int{"2.0": 0, "2.1": 1, "2.2": 2}
	targetRank, known := rank[target]
	best, bestDistance := "", 4
	for _, line := range certified {
		r, ok := rank[line]
		if !ok {
			continue
		}
		distance := 0
		if known {
			distance = r - targetRank
			if distance < 0 {
				distance = -distance
			}
		}
		if distance < bestDistance || distance == bestDistance && line > best {
			best, bestDistance = line, distance
		}
	}
	return best
}
