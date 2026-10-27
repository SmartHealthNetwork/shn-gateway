//go:build shn_coreonly

package relay

// zoneOwnership is the ownership rows the engine's zones register: none in a
// core-only build, which compiles no zone, so the table is the rows the
// engine registers whatever it compiles in and the test profile's.
func zoneOwnership() map[Key]Rule { return nil }
