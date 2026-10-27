//go:build shn_coreonly

package engine

// The core-only build profile (make core-only) compiles the engine without
// its zones' files, which carry //go:build !shn_coreonly: the PA and FQS
// adapters and module 1 (workstream_pa*.go, workstream_fqs*.go,
// module1_*.go). That it compiles proves no core file names a zone. A
// core-only build must compile in at least one workstream, the toy profile's
// (shn_toyworkstream): with none, the leg ownership the engine registers at
// init has no legs, and init panics.

// zoneWorkstreams are none: the core-only profile leaves the zones out.
func zoneWorkstreams() []workstream { return nil }

// zoneModules are none, for the same reason.
func zoneModules() []gatewayModule { return nil }
