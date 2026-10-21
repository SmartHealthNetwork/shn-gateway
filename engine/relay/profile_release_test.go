//go:build !shn_toyworkstream

package relay

// profileOwnership is the ownership rows the compiled-in test profiles add:
// none in a release build.
func profileOwnership() map[Key]Rule { return nil }
