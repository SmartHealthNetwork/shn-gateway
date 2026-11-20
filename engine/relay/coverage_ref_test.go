package relay

import "slices"

// hitsEach is hits as it was before the covered regions were indexed: every
// effect compared with every region, in coverage order. It is the reference
// coverageMap.hits is held to. What the regions are is signedcontent's
// reading, held to its own reference (coverageEach) in the SDK's tests.

func hitsEach(c []coveredRegion, e effect) []string {
	var out []string
	for _, r := range c {
		touched := e.start < r.end && r.start < e.end
		if e.inside {
			touched = r.start <= e.start && e.end <= r.end
		}
		if touched && !slices.Contains(out, r.carrier) {
			out = append(out, r.carrier)
		}
	}
	return out
}
