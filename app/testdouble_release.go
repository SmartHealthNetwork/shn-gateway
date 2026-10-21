//go:build !shn_testdouble

package app

// testDoubleBuild: an untagged build, as every release is, has no validator test
// double outside a test binary (testdouble.go).
const testDoubleBuild = false
