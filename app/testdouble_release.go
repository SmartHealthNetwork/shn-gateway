//go:build !shn_testdouble && !shn_coreonly

package app

import (
	"errors"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// testDoubleBuild: an untagged build, as every release is, has no validator test
// double (testdouble.go).
const testDoubleBuild = false

// lineFakeValidator: an untagged build links no validator test double. Nothing
// reaches this: fakeValidator is false here, and loadConfig refuses
// SHN_FAKE_VALIDATOR before any lane is built. It refuses as loadConfig does.
func lineFakeValidator(string) (shnsdk.Validator, error) {
	return nil, errors.New(testDoubleRefusal)
}
