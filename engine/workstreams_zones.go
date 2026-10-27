//go:build !shn_coreonly

package engine

// zoneWorkstreams are the workstream adapters of the engine's zones: Prior
// Authorization (workstream_pa*.go) and Federated Query Services
// (workstream_fqs*.go), in every build but the core-only profile
// (workstreams_coreonly.go). Each is an adapter variable that holds its
// catalog; Go's initialization follows the references in this body, so every
// catalog is initialized before the registry that calls it.
func zoneWorkstreams() []workstream { return []workstream{paAdapter, fqsAdapter} }

// zoneModules are the modules of the engine's zones: on-behalf building
// (module1_*.go).
func zoneModules() []gatewayModule { return []gatewayModule{moduleOne{}} }
