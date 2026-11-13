package engine

import (
	"context"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// certificationAdapter is a workstream's part in certifying the payloads its
// legs carry (module3_certify.go) and in reading a validator's answer
// (module3_classify.go): the message families, or species, it recognises,
// the profile each is certified against, and the lines a payload points to.
// The core certifies through the compiled-in workstreams' adapters
// (workstreams.go) and names no workstream's profiles itself.
type certificationAdapter interface {
	// species is the message family payload is, or "" when the workstream
	// certifies no such payload.
	species(payload []byte) string
	// certifies reports whether species is one of the workstream's.
	certifies(species string) bool
	// profile is the versioned canonical a payload of species is certified
	// against at line on leg ("" for any of the workstream's legs); ok is
	// false where the workstream certifies none.
	profile(species, line, leg string) (string, bool)
	// lineClaims orders the lines to certify payload at, most likely first,
	// and names what the payload itself points to, each claim a set of lines
	// any one of which, judged, meets it.
	lineClaims(payload []byte) (order []string, claims []claimSet)
	// candidateGroups is the lines a payer's answer of contract can be judged
	// at, by the structure that names its line: each group is the lines whose
	// profiles constrain a part payload names, and the answer is valid only when
	// it is valid at a line of every group. nil when nothing in payload names its
	// line: every line, as one group.
	candidateGroups(contract string, payload []byte) [][]string
	// undeclaredAnswerProfile is the profile, without a version, a payer's
	// answer of contract that declares none is judged against at line on leg
	// at structural and strict (owner decision D47): its line's own profile
	// for what the answer is. ok is false where the workstream names none.
	undeclaredAnswerProfile(contract string, payload []byte, line, leg string) (string, bool)
	// excuse marks the issues of a reading of body at line against profile
	// that are a known consequence of how the validator checks the
	// workstream's profiles, not a defect in the message.
	excuse(errs []fhirIssue, body []byte, line, profile string)
	// reading is the workstream's own way to $validate a payload at line
	// against profile where its lane cannot read that profile in one call,
	// or nil where the one call is the reading (validateAt).
	reading(line, profile string) laneReading
}

// laneReading $validates body on v and answers as v's one call would have:
// one result whose issues name where they are in body.
type laneReading func(ctx context.Context, v shnsdk.Validator, body []byte) (shnsdk.Result, error)

// validateAt is every governed or certifying $validate of body at line
// against profile on v: the compiled-in workstream's own reading where it has
// one (certificationAdapter.reading), else v's one call. The governed check
// (judgeLines) and module 3's certification (collectCertification) both read
// through it, so one payload is read the same way by each.
func validateAt(ctx context.Context, v shnsdk.Validator, body []byte, line, profile string) (shnsdk.Result, error) {
	for _, a := range certificationAdapters() {
		if read := a.reading(line, profile); read != nil {
			return read(ctx, v, body)
		}
	}
	return v.Validate(ctx, body, profile)
}

// claimSet is the lines one claim in a payload names, in 2.2, 2.1, 2.0
// order: one line for a versioned profile, or the lines whose packages define
// a structural marker where it sits. A claim is met when any one of its lines
// is judged.
type claimSet []string

// candidateOrder is the lines to certify a payload of species at, most likely
// first, or none when no compiled-in workstream certifies species.
func candidateOrder(species string, payload []byte) []string {
	a := certificationAdapterFor(species)
	if a == nil {
		return nil
	}
	order, _ := a.lineClaims(payload)
	return order
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

// ValidateAtForTest is validateAt, for the platform's real-validator lane
// tests (its conformance suite), which certify a payload by the reading the
// gateway itself gives it. Test-only, like ClassifyFHIRForTest.
func ValidateAtForTest(ctx context.Context, v shnsdk.Validator, body []byte, line, profile string) (shnsdk.Result, error) {
	return validateAt(ctx, v, body, line, profile)
}
