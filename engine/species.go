package engine

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
	// excuse marks the issues of a reading of body at line against profile
	// that are a known consequence of how the validator checks the
	// workstream's profiles, not a defect in the message.
	excuse(errs []fhirIssue, body []byte, line, profile string)
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
