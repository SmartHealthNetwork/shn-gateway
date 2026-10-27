//go:build !shn_coreonly

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

// paLineMarkers are the structural markers whose presence in a payload claims
// lines. A marker claims the lines whose packages define it for where it sits
// (paLinesDefining, workstream_pa_extension_lines.json), so the lines are read from the
// packages the validator lanes load, never from the marker's name. A marker
// every line defines there claims every line, which constrains nothing.
var paLineMarkers = map[string]bool{
	certificationPAS + "extension-certificationType":      true,
	certificationPAS + "extension-serviceItemRequestType": true,
	certificationDTR + "qr-context":                       true,
	certificationDTR + "qr-coverage":                      true,
}

// candidateGroups: a DTR $questionnaire-package answer names its line by its
// parameters. The output Parameters profile slices parameter.name open at
// every line, so a line of the other name family leaves those parameters
// unprofiled and does not judge them: PackageBundle and operationOutcome are
// judged at 2.0 and 2.1, packagebundle at 2.2 (outcome is every line's and
// names none), and an answer naming both families must be valid at a line of
// each. An answer naming neither is
// judged at every line, where the profile itself judges the absence. Every
// other answer is judged at every line.
func (paCertification) candidateGroups(contract string, payload []byte) [][]string {
	if contract != "pa.dtr" {
		return nil
	}
	var groups [][]string
	for _, f := range dtrAnswerNameFamilies(payload) {
		groups = append(groups, dtrFamilyLines(f))
	}
	return groups
}

// lineClaims orders the Da Vinci lines 2.2, 2.1 and 2.0 for certifying
// payload, whatever its species: first the lines a versioned meta.profile claims
// (a PAS or DTR profile at that line's package version), then the lines of a
// structural marker's claim narrower than every line, then the rest, each
// group in 2.2, 2.1, 2.0 order. It walks Bundle entries and Parameters
// parameter resources, so a DTR $questionnaire-package answer is read through
// its PackageBundle. claims is what the payload itself points to, each claim
// a set of lines: one line for a versioned meta.profile, and for a marker the
// lines whose packages define it where it sits (a resource's root, or a
// Claim's item). Claims and markers order attempts only; every supported line
// is retained, including when extensions belong to an unknown namespace.
func (paCertification) lineClaims(payload []byte) ([]string, []claimSet) {
	order := []string{"2.2", "2.1", "2.0"}
	var r map[string]any
	if json.Unmarshal(payload, &r) != nil {
		return order, nil
	}
	profiled := map[string]bool{}
	narrow := map[string]bool{}
	var claims []claimSet
	seen := map[string]bool{}
	claim := func(set claimSet) {
		key := strings.Join(set, ",")
		if len(set) == 0 || seen[key] {
			return
		}
		seen[key] = true
		claims = append(claims, set)
		if len(set) < len(order) {
			for _, line := range set {
				narrow[line] = true
			}
		}
	}
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
					profiled[line] = true
					claim(claimSet{line})
				}
			}
		}
		resourceType, _ := m["resourceType"].(string)
		type sited struct {
			r        map[string]any
			position string
		}
		resources := []sited{{m, resourceType}}
		if resourceType == "Claim" {
			items, _ := m["item"].([]any)
			for _, item := range items {
				r, _ := item.(map[string]any)
				resources = append(resources, sited{r, "Claim.item"})
			}
		}
		for _, s := range resources {
			extensions, _ := s.r["extension"].([]any)
			for _, extension := range extensions {
				e, _ := extension.(map[string]any)
				url, _ := e["url"].(string)
				if paLineMarkers[url] {
					claim(claimSet(paLinesDefining(url, s.position, order)))
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
	out := make([]string, 0, len(order))
	for _, group := range []func(string) bool{
		func(l string) bool { return profiled[l] },
		func(l string) bool { return narrow[l] && !profiled[l] },
		func(l string) bool { return !narrow[l] && !profiled[l] },
	} {
		for _, line := range order {
			if group(line) {
				out = append(out, line)
			}
		}
	}
	return out, claims
}
