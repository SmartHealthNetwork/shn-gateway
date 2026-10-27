// Package buildversion is the gateway's own build version: what `gateway
// -version` prints and /health reports when the deployment sets none, and what
// its start record will state from v1.1.
//
// An image build stamps it with
//
//	-ldflags "-X github.com/SmartHealthNetwork/shn-gateway/internal/buildversion.stamp=v1.0.0"
//
// (gateway/Dockerfile's GATEWAY_VERSION). A release stamps its tag; a main
// build stamps a pseudo-version above the latest release. A build without a
// stamp falls back to the module version Go records (`go install ...@v1.0.0`,
// or a build from a clone whose tag Go reads from version control), and
// otherwise reads as Unstamped, which no check counts as a release.
package buildversion

import (
	"regexp"
	"runtime/debug"
)

// Unstamped is the version of a build that states none.
const Unstamped = "v0.0.0-unstamped"

// stamp is set at build time by -X; a fence test holds every build site to
// this package's import path and this name, since -X on a missing symbol is
// silently ignored. A -trimpath build records no -ldflags, so the stamp is read
// back by running the binary (`gateway -version`).
var stamp string

// pattern is the gateway-start record's bound on gateway.version (the v1.0.0
// signable-content design §3.5).
var pattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]{1,64})?$`)

// Source names where Version came from.
type Source string

const (
	FromStamp  Source = "stamp"
	FromModule Source = "module"
	// FromNone is a build with neither: Version is Unstamped.
	FromNone Source = "unstamped"
	// FromBadStamp is a build stamped with something that is not a version:
	// Version is Unstamped, and the boot line says why.
	FromBadStamp Source = "unstamped: the stamp is not a version"
)

// Version is this build's version and where it came from.
func Version() (string, Source) {
	return resolve(stamp, moduleVersion())
}

// Valid reports whether v is a version: the gateway-start record's bound on
// gateway.version (v1.1).
func Valid(v string) bool { return pattern.MatchString(v) }

// resolve takes the stamp, else the module version, each only when it is a
// version; an invalid stamp is never guessed at.
func resolve(stamp, module string) (string, Source) {
	if stamp != "" {
		if Valid(stamp) {
			return stamp, FromStamp
		}
		return Unstamped, FromBadStamp
	}
	if Valid(module) {
		return module, FromModule
	}
	return Unstamped, FromNone
}

func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return bi.Main.Version
}
