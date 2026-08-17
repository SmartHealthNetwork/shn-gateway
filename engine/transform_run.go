// transform_run.go — RunTransformChain: the exported,
// contract-parameterized sibling of TransformPASForTest/TransformDTRForTest
// (transform_pas.go/transform_dtr.go). Same chainFor+applyChain shape, but
// deliberately NOT *ForTest: this is a published seam, not a test-only
// cross-module escape hatch.
package engine

import "fmt"

// RunTransformChain runs the real compat chain contract@from->to over
// payload. Exported NON-test surface (V-1): the SHN Kit's engine exhibit
// calls it via the gateway's loopback demo endpoint so the exhibit provably
// runs "the same modules your live legs route through" — same binary, same
// manifest. Not a wire exchange; never seals, never routes.
//
// This calls applyChain directly (exactly like TransformPASForTest/
// TransformDTRForTest), NOT the leg-only egressAdapt path — so a chain run
// through this func (and the /demo/transform endpoint that wraps it) emits
// NOTHING on the observer SSE stream. The exhibit's own request/response is
// its own record; it is not itself an observed exchange.
func RunTransformChain(contract, from, to string, payload []byte, x ExchangeIdentity) ([]byte, []LossReport, error) {
	steps := chainFor(contract, from, to)
	if steps == nil {
		return nil, nil, fmt.Errorf("engine: RunTransformChain: no %s chain %s->%s", contract, from, to)
	}
	return applyChain(steps, from, payload, x)
}

// ChainSteps reports the compatibility chain that RunTransformChain would
// walk for contract from->to, in the same wire shape observer events carry
// (ChainStep), including the direction-aware hop rendering. nil when no
// chain exists. Read-only: no step function runs.
func ChainSteps(contract, from, to string) []ChainStep {
	steps := chainFor(contract, from, to)
	if steps == nil {
		return nil
	}
	return chainStepsFrom(from, steps)
}
