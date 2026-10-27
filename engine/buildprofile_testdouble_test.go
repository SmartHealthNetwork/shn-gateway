//go:build shn_testdouble

package engine

// A test build (-tags shn_testdouble) adds its tag to the build profile tags
// this test binary was built with, beside a build profile's.
func init() { thisBuildTags = append(thisBuildTags, "shn_testdouble") }
