//go:build !shn_toyworkstream && !shn_coreonly

package app

// toyProfileBuild reports whether this test binary compiles in the toy test
// profile: not in a release build.
const toyProfileBuild = false
