package engine

import "github.com/SmartHealthNetwork/shn-gateway/engine/relay"

// certifiedBuild is one builder at one line of its contract.
type certifiedBuild struct {
	Builder  relay.BuilderID
	Contract string
	Line     string
}

// certification is how a certified pair is proven: the platform's validation-gate test
// that certifies it, and the shn-sdk release whose builder builds the bytes
// that test certified. The test runs against the tree's sdk, while this
// gateway builds with the shn-sdk its go.mod pins, so a pair holds only while
// that pin is SDK or later (the platform conformance suite's
// TestCertifiedBuildsHoldForThePinnedSDK). SDK is
// set by hand, from a byte comparison of that release's builder with the
// tree's on the test's fixtures; the check reads only the pin against it, so
// a builder change after that release, at a pin that has not moved, needs the
// pair compared again, and a later release holds only when it is cut from a
// tree whose validation gate passed for the pair.
type certification struct {
	Test string
	SDK  string
}

// certifiedBuilds (workstreams.go) are the builder and line pairs that
// build-time certification proves, from every compiled-in workstream, each
// with the validation-gate test that proves it and the shn-sdk release it holds
// from. A pair is certified when a test in that line's validation-gate run list
// builds the whole message with the gateway's own builder at that line and
// validates it against the builder's own profile on that line's lane, with a
// refusal control, and the shn-sdk this gateway pins builds those bytes.
// Request shape built from fixture inputs does not count. A pair is added
// only with its test; every other pair keeps needing a lane.

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
	if builder == carriedRequest || payloadChecksConsumeLanes(g.conformance().Level()) {
		return false
	}
	_, certified := certifiedBuilds[certifiedBuild{builder, contract, line}]
	return certified
}
