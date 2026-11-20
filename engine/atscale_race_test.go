//go:build race

package engine

// A race build sets raceBuild (atscale_test.go), so a leaf at scale skips in
// it. The default is false, so no test file of the package is built only
// without -race.
func init() { raceBuild = true }
