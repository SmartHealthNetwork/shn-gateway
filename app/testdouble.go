package app

import "testing"

// The validator test double (SHN_FAKE_VALIDATOR=1, engine.NewLineFakeValidator on
// every lane) checks a few PAS/DTR cardinalities and bindings and accepts
// everything else, so a gateway running it appears to certify bodies it never
// checked. It exists only in a test binary or a build tagged shn_testdouble. Every
// other build (the published image, go install, a plain go build) refuses to boot
// with SHN_FAKE_VALIDATOR set to any value (loadConfig), before any conformance
// level or validator lane is read, so the refusal holds at every level and with no
// runtime validator at all; and fakeValidator reads it as unset, so no path that
// skips loadConfig reaches the double either. The refusal is of the
// operator's configuration, for the fidelity of SHN's own edits: a gateway never
// claims a certification it did not perform. It never judges a payload.
//
// A var so this package's rejection rows can stand in for a released build.
var testDoubleAvailable = func() bool { return testDoubleBuild || testing.Testing() }

// fakeValidator reports whether this gateway runs the validator test double. It is
// the only reader of SHN_FAKE_VALIDATOR's value.
func fakeValidator(getenv func(string) string) bool {
	return testDoubleAvailable() && getenv("SHN_FAKE_VALIDATOR") == "1"
}

// testDoubleRefusal is a released build's boot refusal of SHN_FAKE_VALIDATOR. It
// names the supported paths: a real validator, or the level that checks no
// payload and so needs none (it claims no validation, so it fakes none). The
// refusal holds at none too: the variable is refused, not the level.
const testDoubleRefusal = "gateway: SHN_FAKE_VALIDATOR is set, but a released gateway has no validator test double: " +
	"refusing to boot rather than appear to validate what it never checked (FR-36). Unset it, and run the IG-loaded " +
	"$validate beside the gateway and point FHIR_VALIDATE_URL at it (deploy/bundle/compose.yml ships that pairing; " +
	"deploy/eval runs a first end-to-end test with a real validator), " + noneRemedy
