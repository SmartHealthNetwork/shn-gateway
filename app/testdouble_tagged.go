//go:build shn_testdouble && !shn_coreonly

package app

import (
	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// testDoubleBuild: this build was tagged shn_testdouble, for a harness that runs the
// gateway binary on the validator test double (testdouble.go).
const testDoubleBuild = true

// lineFakeValidator is the validator test double fixed to line. Only a test build
// links it.
func lineFakeValidator(line string) (shnsdk.Validator, error) {
	return engine.NewLineFakeValidator(line), nil
}
