//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A PAS request Bundle at 2.1 or 2.2 is read per entry (readPASRequestBundle)
// when it is checked against profile-pas-request-bundle, asked or declared. At those lines the
// validator cannot read it against that profile: in the Bundle.entry:Claim
// slice, which allows two candidate profiles, it stops without a verdict
// (HAPI-0389) when the Claim matches its candidate with only information to
// report, and checks nothing else. Read so, such a request could never be
// judged.
//
// Temporary seam, tracked in the SHN platform repository as "Whole-Bundle
// reading returns when the validator finishes the 2.1/2.2 request Bundle
// (HAPI-0389)". The validator stays the certifier, in one call: it reads the
// Bundle without the request Bundle profile (so there is no two-candidate
// slice to stop in) and each Claim against the candidate it is built to,
// declared on the Claim itself, in the Bundle, so the resources a Claim points
// to are held to its target profiles as the whole reading holds them. Only
// the request Bundle profile's own constraints on the Bundle are read here,
// and those are a closed list: every element of the profile's differential at
// PAS 2.1.0 and 2.2.1, each read as the validator reads it
// (pasRequestBundleRules). The platform's conformance suite holds the list to
// the profile's own bytes, holds the reading and each rule to the validator's
// whole reading where it finishes, and fails once the validator finishes the
// requests it stops on today, when this reading is to be removed for the one
// whole call. It changes only what the gateway asks its own validator, never a
// relayed byte.

// pasRequestBundlePerEntryLines are the lines whose validator stops on a
// request Bundle read whole. At 2.0 the Claim slice has one candidate and the
// validator finishes.
var pasRequestBundlePerEntryLines = []string{"2.1", "2.2"}

// reading is the PA workstream's part in validateAt: a request Bundle at
// pasRequestBundlePerEntryLines, checked against that line's request Bundle
// profile or, with no profile asked, declaring it (unversioned or at the
// line's version) in its own meta.profile, is read per entry. Every other
// payload, profile and line is the validator's one call.
func (paCertification) reading(line, profile string) laneReading {
	switch {
	case !slices.Contains(pasRequestBundlePerEntryLines, line):
		return nil
	case profile == "":
		return func(ctx context.Context, v shnsdk.Validator, body []byte) (shnsdk.Result, error) {
			if !declaresLineRequestBundle(body, line) {
				return v.Validate(ctx, body, "")
			}
			return readPASRequestBundle(ctx, v, body, line, "")
		}
	case !pasRequestBundleReadPerEntry(line, profile):
		return nil
	}
	return func(ctx context.Context, v shnsdk.Validator, body []byte) (shnsdk.Result, error) {
		return readPASRequestBundle(ctx, v, body, line, profile)
	}
}

// declaresLineRequestBundle reports whether body is a Bundle whose own
// meta.profile names line's request Bundle profile, unversioned or at line's
// version.
func declaresLineRequestBundle(body []byte, line string) bool {
	var b struct {
		ResourceType string `json:"resourceType"`
		Meta         struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
	}
	if json.Unmarshal(body, &b) != nil || b.ResourceType != "Bundle" {
		return false
	}
	return slices.ContainsFunc(b.Meta.Profile, func(p string) bool { return pasRequestBundleReadPerEntry(line, p) })
}

// pasRequestBundleReadPerEntry reports whether profile is line's request
// Bundle profile (unversioned, or at line's own package version) at a line
// read per entry.
func pasRequestBundleReadPerEntry(line, profile string) bool {
	if !slices.Contains(pasRequestBundlePerEntryLines, line) || canonicalOf(profile) != pasRequestBundleCanonical {
		return false
	}
	want, ok := paCertification{}.profile("PASRequestBundle", line, "pas-claim")
	return ok && (profile == pasRequestBundleCanonical || profile == want)
}

// pasRequestBundleShape is what the rules read of a request Bundle: each
// member as it is written, absent or null alike reading as not there.
type pasRequestBundleShape struct {
	ResourceType string               `json:"resourceType"`
	Identifier   json.RawMessage      `json:"identifier"`
	Type         json.RawMessage      `json:"type"`
	Timestamp    json.RawMessage      `json:"timestamp"`
	Entry        []pasRequestEntryRaw `json:"entry"`
}

type pasRequestEntryRaw struct {
	FullURL  json.RawMessage `json:"fullUrl"`
	Resource json.RawMessage `json:"resource"`
	Search   json.RawMessage `json:"search"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

// pasElementPresent reports whether a member is there: written, and not null.
func pasElementPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// pasRequestBundleRule is one element of profile-pas-request-bundle's
// differential and how this reading reads it: check, in Go, for a constraint
// on the Bundle itself, or nil for the one element the lane reads (each
// Claim entry's resource, at its candidate profile). lines is where the
// profile states it.
type pasRequestBundleRule struct {
	element string
	lines   []string
	check   func(b pasRequestBundleShape, profile string) []shnsdk.Issue
}

// pasRequestBundleRules is the closed list: the differential of
// profile-pas-request-bundle at PAS 2.1.0 and 2.2.1, element by element (the
// Bundle root's only constraint is 2.2.1's ClaimFirst). The issues each check
// writes are those the validator writes for the same constraint where it reads
// the Bundle whole, by message id, place and wording (a cardinality by
// Validation_VAL_Profile_Minimum or _Maximum on the element's parent, the
// Claim slice's minimum by Validation_VAL_Profile_Minimum_SLICE, the pattern
// by _DT_Fixed_Wrong, the invariant by the profile's versioned canonical and
// its key), so the classifier reads them as it reads the validator's own.
var pasRequestBundleRules = []pasRequestBundleRule{
	{"Bundle", []string{"2.2"}, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		// ClaimFirst: entry.first().resource is Claim. The validator fails
		// it on a Bundle with no entry too.
		if len(b.Entry) > 0 && resourceTypeOf(b.Entry[0].Resource) == "Claim" {
			return nil
		}
		return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: profile + "#ClaimFirst",
			Expression:  []string{"Bundle"},
			Diagnostics: "Constraint failed: ClaimFirst: 'A Prior Authorization Bundle must have the PAS Request as the first resource' (defined in " + profile + ")"}}
	}},
	{"Bundle.identifier", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return minimumOne(pasElementPresent(b.Identifier), "Bundle", "Bundle.identifier", profile)
	}},
	{"Bundle.type", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		// patternCode collection, which the validator reports as a fixed
		// value. A type that is not a string is base FHIR's to report.
		var t string
		if !pasElementPresent(b.Type) || json.Unmarshal(b.Type, &t) != nil || t == "collection" {
			return nil
		}
		return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: "_DT_Fixed_Wrong", Expression: []string{"Bundle.type"},
			Diagnostics: fmt.Sprintf("Value is %q but is fixed to 'collection' in the profile %s#Bundle.type", t, profile)}}
	}},
	{"Bundle.timestamp", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return minimumOne(pasElementPresent(b.Timestamp), "Bundle", "Bundle.timestamp", profile)
	}},
	{"Bundle.entry", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return minimumOne(len(b.Entry) > 0, "Bundle", "Bundle.entry", profile)
	}},
	{"Bundle.entry.fullUrl", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return eachEntry(b, func(e pasRequestEntryRaw, at string) []shnsdk.Issue {
			return minimumOne(pasElementPresent(e.FullURL), at, "Bundle.entry.fullUrl", profile)
		})
	}},
	{"Bundle.entry.resource", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return eachEntry(b, func(e pasRequestEntryRaw, at string) []shnsdk.Issue {
			return minimumOne(pasElementPresent(e.Resource), at, "Bundle.entry.resource", profile)
		})
	}},
	{"Bundle.entry.search", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return eachEntry(b, func(e pasRequestEntryRaw, at string) []shnsdk.Issue {
			return maximumZero(pasElementPresent(e.Search), at, "Bundle.entry.search", profile)
		})
	}},
	{"Bundle.entry.request", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return eachEntry(b, func(e pasRequestEntryRaw, at string) []shnsdk.Issue {
			return maximumZero(pasElementPresent(e.Request), at, "Bundle.entry.request", profile)
		})
	}},
	{"Bundle.entry.response", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		return eachEntry(b, func(e pasRequestEntryRaw, at string) []shnsdk.Issue {
			return maximumZero(pasElementPresent(e.Response), at, "Bundle.entry.response", profile)
		})
	}},
	{"Bundle.entry:Claim", nil, func(b pasRequestBundleShape, profile string) []shnsdk.Issue {
		// min 1, max 2, sliced by the entry resource's type.
		claims := 0
		for _, e := range b.Entry {
			if resourceTypeOf(e.Resource) == "Claim" {
				claims++
			}
		}
		switch {
		case claims < 1:
			return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Minimum_SLICE", Expression: []string{"Bundle"},
				Diagnostics: "Slice 'Bundle.entry:Claim': a matching slice is required, but not found (from " + profile + ")"}}
		case claims > 2:
			return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Maximum", Expression: []string{"Bundle"},
				Diagnostics: fmt.Sprintf("Bundle.entry:Claim: max allowed = 2, but found %d (from %s)", claims, profile)}}
		}
		return nil
	}},
	// Read by the lane: each Claim entry's resource at the candidate it is
	// built to, declared on it (pasClaimCandidates, pasRequestBundleRead).
	{"Bundle.entry:Claim.resource", nil, nil},
}

func minimumOne(there bool, at, element, profile string) []shnsdk.Issue {
	if there {
		return nil
	}
	return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Minimum", Expression: []string{at},
		Diagnostics: element + ": minimum required = 1, but only found 0 (from " + profile + ")"}}
}

func maximumZero(there bool, at, element, profile string) []shnsdk.Issue {
	if !there {
		return nil
	}
	return []shnsdk.Issue{{Severity: "error", Code: "processing", MessageID: "Validation_VAL_Profile_Maximum", Expression: []string{at},
		Diagnostics: element + ": max allowed = 0, but found 1 (from " + profile + ")"}}
}

func eachEntry(b pasRequestBundleShape, check func(e pasRequestEntryRaw, at string) []shnsdk.Issue) []shnsdk.Issue {
	var out []shnsdk.Issue
	for i, e := range b.Entry {
		out = append(out, check(e, fmt.Sprintf("Bundle.entry[%d]", i))...)
	}
	return out
}

// pasClaimCandidates is the candidate profile, or profiles, a Claim entry is
// read against at line: each candidate its meta.profile names unversioned or
// at line's version, every one of which the validator holds a declared
// profile to; else the one the slice can match by Claim.related, which
// profile-claim-update requires and profile-claim forbids (as
// rescuePASClaimSliceMatch reads it). A candidate named at another version is
// no candidate at line: the lane, which does not hold it, says so, as the
// whole reading would.
func pasClaimCandidates(raw json.RawMessage, line string) []string {
	var claim struct {
		Meta struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
		Related []json.RawMessage `json:"related"`
	}
	_ = json.Unmarshal(raw, &claim)
	var named []string
	for _, c := range []string{pasClaimCanonical, pasClaimUpdateCanonical} {
		leg := "pas-claim"
		if c == pasClaimUpdateCanonical {
			leg = "pas-claim-update"
		}
		at, _ := paCertification{}.profile("Claim", line, leg)
		if slices.Contains(claim.Meta.Profile, c) || slices.Contains(claim.Meta.Profile, at) {
			named = append(named, c)
		}
	}
	switch {
	case len(named) > 0:
		return named
	case len(claim.Related) > 0:
		return []string{pasClaimUpdateCanonical}
	}
	return []string{pasClaimCanonical}
}

// readPASRequestBundle reads body at line against profile per entry, in one
// call: the lane reads pasRequestBundleRead's Bundle with no profile asked,
// and every Go rule of pasRequestBundleRules stated at line adds the issue
// the validator writes for its constraint. An issue the lane writes of the
// Bundle's own meta.profile is renamed to the index that profile has in body.
// A body that is no Bundle is the validator's one call. A call that fails
// fails the reading.
func readPASRequestBundle(ctx context.Context, v shnsdk.Validator, body []byte, line, profile string) (shnsdk.Result, error) {
	var shape pasRequestBundleShape
	if json.Unmarshal(body, &shape) != nil || shape.ResourceType != "Bundle" {
		return v.Validate(ctx, body, profile)
	}
	read, kept, err := pasRequestBundleRead(body, line)
	if err != nil {
		return v.Validate(ctx, body, profile)
	}
	res, err := v.Validate(ctx, read, "")
	if err != nil {
		return shnsdk.Result{}, err
	}
	out := shnsdk.Result{Valid: res.Valid, Issues: res.Issues}
	if !res.Valid && len(res.Details) == 0 {
		// An invalid answer naming no issue in Details (an unclassified one,
		// shnsdk.Result) stays unclassified: each issue text it gives, or one
		// saying it gave none, is an error with no message id, which the
		// classifier reads as the most serious kind.
		texts := res.Issues
		if len(texts) == 0 {
			texts = []string{"the validator answered invalid and named no issue"}
		}
		for _, s := range texts {
			out.Details = append(out.Details, shnsdk.Issue{Severity: "error", Diagnostics: s})
		}
	}
	for _, d := range res.Details {
		if d.Expression != nil {
			placed := make([]string, len(d.Expression))
			for i, x := range d.Expression {
				placed[i] = inBundleMeta(x, kept)
			}
			d.Expression = placed
		}
		out.Details = append(out.Details, d)
	}
	// The rules name the profile at line's version, as the validator does.
	profile, _ = paCertification{}.profile("PASRequestBundle", line, "pas-claim")
	for _, r := range pasRequestBundleRules {
		if r.check == nil || r.lines != nil && !slices.Contains(r.lines, line) {
			continue
		}
		for _, iss := range r.check(shape, profile) {
			out.Valid = false
			out.Issues = append(out.Issues, iss.Diagnostics)
			out.Details = append(out.Details, iss)
		}
	}
	return out, nil
}

// pasRequestBundleRead is body as the lane reads it per entry at line:
//   - the request Bundle profile, unversioned or at line's version, taken out
//     of the Bundle's meta.profile (one at another line's version stays, and
//     the lane, which does not hold it, says so); kept is the index in body of
//     each profile left there, in order;
//   - each Claim entry declaring, after whatever it declares, line's version
//     of each candidate it is read against (pasClaimCandidates) that it does
//     not already declare unversioned or at that version.
//
// Nothing else changes: every other declaration stays where it was, so the
// lane holds each to what it declares, and a Claim's own profile indices do
// not move.
func pasRequestBundleRead(body []byte, line string) ([]byte, []int, error) {
	var bundle map[string]json.RawMessage
	var err error
	if err = json.Unmarshal(body, &bundle); err != nil {
		return nil, nil, err
	}
	version, _ := paCertification{}.profile("PASRequestBundle", line, "pas-claim")
	var kept []int
	if kept, err = withoutProfiles(bundle, pasRequestBundleCanonical, version); err != nil {
		return nil, nil, err
	}
	if raw, ok := bundle["entry"]; ok && pasElementPresent(raw) {
		var entries []map[string]json.RawMessage
		if err = json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			rawRes, ok := e["resource"]
			if !ok || resourceTypeOf(rawRes) != "Claim" {
				continue
			}
			var res map[string]json.RawMessage
			if err = json.Unmarshal(rawRes, &res); err != nil {
				return nil, nil, err
			}
			var at []string
			for _, c := range pasClaimCandidates(rawRes, line) {
				leg := "pas-claim"
				if c == pasClaimUpdateCanonical {
					leg = "pas-claim-update"
				}
				p, ok := paCertification{}.profile("Claim", line, leg)
				if !ok {
					return nil, nil, fmt.Errorf("engine: no %s profile at %s", c, line)
				}
				at = append(at, p)
			}
			if err = declaring(res, at...); err != nil {
				return nil, nil, err
			}
			if e["resource"], err = json.Marshal(res); err != nil {
				return nil, nil, err
			}
		}
		if bundle["entry"], err = json.Marshal(entries); err != nil {
			return nil, nil, err
		}
	}
	out, err := json.Marshal(bundle)
	return out, kept, err
}

// withoutProfiles takes each of profiles, exactly as written, out of
// resource's meta.profile, dropping meta.profile, and then meta, when nothing
// is left; kept is the index each remaining profile had, in order (nil when
// resource declares none).
func withoutProfiles(resource map[string]json.RawMessage, profiles ...string) ([]int, error) {
	raw, ok := resource["meta"]
	if !ok || !pasElementPresent(raw) {
		return nil, nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	rawProfiles, ok := meta["profile"]
	if !ok || !pasElementPresent(rawProfiles) {
		return nil, nil
	}
	var declared []string
	if err := json.Unmarshal(rawProfiles, &declared); err != nil {
		return nil, err
	}
	var left []string
	var kept []int
	for i, p := range declared {
		if !slices.Contains(profiles, p) {
			left = append(left, p)
			kept = append(kept, i)
		}
	}
	if len(left) == 0 {
		delete(meta, "profile")
	} else {
		meta["profile"], _ = json.Marshal(left)
	}
	if len(meta) == 0 {
		delete(resource, "meta")
		return kept, nil
	}
	m, err := json.Marshal(meta)
	resource["meta"] = m
	return kept, err
}

// declaring adds each of profiles resource does not already declare, as
// written or unversioned, after what it declares.
func declaring(resource map[string]json.RawMessage, profiles ...string) error {
	meta := map[string]json.RawMessage{}
	if raw, ok := resource["meta"]; ok && pasElementPresent(raw) {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return err
		}
	}
	var declared []string
	if raw, ok := meta["profile"]; ok && pasElementPresent(raw) {
		if err := json.Unmarshal(raw, &declared); err != nil {
			return err
		}
	}
	for _, p := range profiles {
		if !slices.Contains(declared, p) && !slices.Contains(declared, canonicalOf(p)) {
			declared = append(declared, p)
		}
	}
	var err error
	if meta["profile"], err = json.Marshal(declared); err != nil {
		return err
	}
	resource["meta"], err = json.Marshal(meta)
	return err
}

// bundleMetaProfile is an expression of the Bundle's own meta.profile entry.
var bundleMetaProfile = regexp.MustCompile(`^Bundle\.meta\.profile\[(\d+)\]`)

// inBundleMeta renames an expression of the Bundle's own meta.profile[k], in
// the Bundle the lane read, to the index that profile has in body (kept[k]).
// Any other expression is left as it is.
func inBundleMeta(expr string, kept []int) string {
	m := bundleMetaProfile.FindStringSubmatchIndex(expr)
	if m == nil {
		return expr
	}
	k, err := strconv.Atoi(expr[m[2]:m[3]])
	if err != nil || k >= len(kept) {
		return expr
	}
	return fmt.Sprintf("Bundle.meta.profile[%d]", kept[k]) + expr[m[1]:]
}

// PASRequestBundleRuleElementsForTest is every differential element the
// per-entry reading reads at line (pasRequestBundleRules: a Go rule's or the
// lane's), for the platform's conformance suite, which holds the list to the
// profile's own differential; nil at a line not read per entry.
func PASRequestBundleRuleElementsForTest(line string) []string {
	if !slices.Contains(pasRequestBundlePerEntryLines, line) {
		return nil
	}
	var out []string
	for _, r := range pasRequestBundleRules {
		if r.lines == nil || slices.Contains(r.lines, line) {
			out = append(out, r.element)
		}
	}
	return out
}
