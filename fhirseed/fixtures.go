package fhirseed

import _ "embed"

// sandboxProviderPersonas is the baked provider-tenant sandbox persona set,
// generated from the private canonical seed source and drift-guarded there
// (synthetic personas only, FR-39). The Kit loads it so the gateway ingress
// can resolve conformant-lane members (e.g. MBR-COVERED) against the local
// provider SoR.
//
//go:embed fixtures/sandbox-provider-personas.json
var sandboxProviderPersonas []byte

// SandboxProviderPersonasBundle returns a copy of the baked provider-tenant
// persona transaction Bundle (PUT entries — idempotent to re-POST).
func SandboxProviderPersonasBundle() []byte {
	return append([]byte(nil), sandboxProviderPersonas...)
}
