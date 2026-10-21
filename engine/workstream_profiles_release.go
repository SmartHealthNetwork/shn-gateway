//go:build !shn_toyworkstream

package engine

// profileWorkstreams are the test profiles' workstreams this build compiles
// in: none, in every build without the shn_toyworkstream tag
// (workstream_toy_test_profile.go).
func profileWorkstreams() []workstream { return nil }
