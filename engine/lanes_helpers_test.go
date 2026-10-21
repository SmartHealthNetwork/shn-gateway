package engine

import shnsdk "github.com/SmartHealthNetwork/shn-sdk"

// contractLanes names per-line explicit lanes by contract line, as
// ContractLineLanes does, for a test that sets its lanes by line.
func contractLanes(byLine map[string]shnsdk.Validator) map[string]shnsdk.Validator {
	return ContractLineLanes(byLine, nil, nil, nil)
}

// dropLaneLine removes every contract's lane at line.
func dropLaneLine(lanes map[string]shnsdk.Validator, line string) {
	for key := range lanes {
		if shnsdk.LineOf(key) == line {
			delete(lanes, key)
		}
	}
}
