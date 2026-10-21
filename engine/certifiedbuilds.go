package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// certifiedBuild is one builder at one line of its contract.
type certifiedBuild struct {
	Builder  relay.BuilderID
	Contract string
	Line     string
}

// certifiedBuilds (workstreams.go) are the builder and line pairs that
// build-time certification proves, from every compiled-in workstream, each with
// the make validate test that proves it. A pair is certified when a test in
// that line's make validate run list builds the whole message with the
// gateway's own builder at that line and validates it against the builder's
// own profile on that line's lane, with a refusal control. Request shape built
// from fixture inputs does not count. A pair is added only with its test;
// every other pair keeps needing a lane.

// carriedRequest is the builder a line selection names for a participant's own
// request, which this gateway carries and does not build: none.
const carriedRequest relay.BuilderID = ""

// buildsAt reports whether this gateway can build builder's message natively
// at contract's line for native reach (arm 2): a validator lane serves the
// line, or no payload check consumes lanes here and the pair is certified at
// build time, so no runtime check would read a lane. A participant's own
// carried request (carriedRequest) always needs the lane.
func (g *Gateway) buildsAt(builder relay.BuilderID, contract, line string) bool {
	if g.validatorForContractLine(contract, line) != nil {
		return true
	}
	if builder == carriedRequest || payloadChecksConsumeLanes(g.cfg.ConformanceEnforcement) {
		return false
	}
	_, certified := certifiedBuilds[certifiedBuild{builder, contract, line}]
	return certified
}
