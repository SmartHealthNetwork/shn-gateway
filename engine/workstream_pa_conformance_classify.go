package engine

import (
	"encoding/json"
	"regexp"
	"strings"
)

// excuse is the PA workstream's part in reading a validator's answer
// (certificationAdapter, allExcused): the slice-match consequences HAPI
// reports on a PAS request Bundle's Claim entries.
func (paCertification) excuse(errs []fhirIssue, body []byte, line, profile string) {
	rescuePASClaimSliceMatch(errs, body, line, profile)
}

// The PAS request Bundle's Claim entry allows two candidate profiles, and
// PAS 2.1 and 2.2 validate the Claim against both. When a deeper rule fails
// the Claim's own profile (licensed X12 terminology the validator cannot load,
// on every real request), the Claim matches neither, and HAPI adds the two
// no-match summaries and the other candidate's own requirements: a submit's
// Claim is reported against profile-claim-update (Claim.related minimum 1), an
// update's against profile-claim (Claim.related maximum 0). None of that is a
// defect in the message; the IG's own example bundles fail the same way.
const (
	pasRequestBundleCanonical = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
	pasClaimCanonical         = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim"
	pasClaimUpdateCanonical   = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claim-update"
)

// validatingAgainst is HAPI's attribution of an issue to the profile it was
// checking: "(validating against <canonical>[|<version>] [<Name>])". An issue
// can carry more than one.
var validatingAgainst = regexp.MustCompile(`\(validating against (\S+) \[[^\]]*\]\)`)

// canonicalReference is any http(s) canonical a summary names, with its
// "|version" if it carries one.
var canonicalReference = regexp.MustCompile(`https?://[^\s,()\[\]]+`)

// rescuePASClaimSliceMatch excuses the slice-match consequences on a PAS 2.1
// or 2.2 request Bundle's Claim entries. It applies to one Claim entry
// only when at least one no-match summary sits on that entry (the cascade is
// present), and every error issue on that entry is one of:
//   - a no-match summary naming exactly the two candidate profiles;
//   - the sibling candidate's own cardinality requirement
//     (Validation_VAL_Profile_Minimum or _Maximum) attributed only to that
//     sibling, the candidate the Claim is not;
//   - an issue the reading already excused, whatever it is attributed to: the
//     validator checks a Claim that declares its own profile against it
//     directly, and those issues carry no attribution.
//
// Attribution matters only for the sibling's requirement. Any other issue on
// the entry that is not excused, attributed or not, leaves the whole entry to
// the ordinary reading, where the sibling's requirement counts.
// Scope is closed: nothing outside this Bundle profile, these lines and this
// slice is touched.
func rescuePASClaimSliceMatch(errs []fhirIssue, body []byte, line, profile string) {
	if line != "2.1" && line != "2.2" {
		return
	}
	var bundle struct {
		ResourceType string `json:"resourceType"`
		Meta         struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &bundle) != nil || bundle.ResourceType != "Bundle" ||
		!declaresProfile(append([]string{profile}, bundle.Meta.Profile...), pasRequestBundleCanonical) {
		return
	}
	for n, e := range bundle.Entry {
		declared, ok := declaredPASClaimProfile(e.Resource)
		if !ok {
			continue
		}
		sibling := pasClaimUpdateCanonical
		if declared == pasClaimUpdateCanonical {
			sibling = pasClaimCanonical
		}
		var onEntry []int
		rescuable, summarized := true, false
		for i, iss := range errs {
			if iss.entry != n {
				continue
			}
			onEntry = append(onEntry, i)
			summarized = summarized || isNoMatchSummary(iss.MessageID)
			if !sliceMatchConsequence(iss, sibling) {
				rescuable = false
			}
		}
		if !rescuable || !summarized {
			continue
		}
		for _, i := range onEntry {
			errs[i].excused = true
		}
	}
}

// sliceMatchConsequence is one issue's test for rescuePASClaimSliceMatch. A
// fatal issue is never a consequence.
func sliceMatchConsequence(iss fhirIssue, sibling string) bool {
	if iss.Severity != "error" {
		return false
	}
	if isNoMatchSummary(iss.MessageID) {
		named := map[string]bool{}
		for _, m := range canonicalReference.FindAllString(iss.Diagnostics, -1) {
			named[canonicalOf(m)] = true
		}
		return len(named) == 2 && named[pasClaimCanonical] && named[pasClaimUpdateCanonical]
	}
	if iss.excused {
		return true
	}
	if iss.MessageID != siblingMinimum && iss.MessageID != siblingMaximum {
		return false
	}
	// Every "(validating against" must be one the attribution pattern reads:
	// an attribution in any other shape could name the Claim's own profile.
	matches := validatingAgainst.FindAllStringSubmatch(iss.Diagnostics, -1)
	if strings.Count(iss.Diagnostics, "(validating against ") != len(matches) {
		return false
	}
	attributed := map[string]bool{}
	for _, m := range matches {
		attributed[canonicalOf(m[1])] = true
	}
	return len(attributed) == 1 && attributed[sibling]
}

// The sibling candidate's own cardinality requirements HAPI reports on a PAS
// Claim that matches neither candidate: Claim.related minimum 1 (a submit
// checked against profile-claim-update) or maximum 0 (an update checked
// against profile-claim).
const (
	siblingMinimum = "Validation_VAL_Profile_Minimum"
	siblingMaximum = "Validation_VAL_Profile_Maximum"
)

// declaredPASClaimProfile is the candidate profile a Claim entry is built to:
// its meta.profile when that names exactly one candidate, else Claim.related
// (present on an update, absent on a submit). ok is false when the resource is
// not a Claim, when meta.profile names both candidates, or when it names one
// that contradicts Claim.related: the entry is then read without the rescue.
func declaredPASClaimProfile(raw json.RawMessage) (string, bool) {
	var claim struct {
		ResourceType string `json:"resourceType"`
		Meta         struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Related []json.RawMessage `json:"related"`
	}
	if json.Unmarshal(raw, &claim) != nil || claim.ResourceType != "Claim" {
		return "", false
	}
	byRelated := pasClaimCanonical
	if len(claim.Related) > 0 {
		byRelated = pasClaimUpdateCanonical
	}
	names := map[string]bool{}
	for _, p := range claim.Meta.Profile {
		if c := canonicalOf(p); c == pasClaimCanonical || c == pasClaimUpdateCanonical {
			names[c] = true
		}
	}
	switch len(names) {
	case 0:
		return byRelated, true
	case 1:
		if !names[byRelated] {
			return "", false
		}
		return byRelated, true
	}
	return "", false
}
