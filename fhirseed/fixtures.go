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

// conformantPersonas is the baked conformant-lane Patient roster (5 members:
// MBR-COVERED / MBR-NOTCOVERED / MBR-UC06 / MBR-UC07HCPCS / MBR-UC08), generated
// from the private canonical seed source and drift-guarded there (synthetic
// personas only, FR-39). Partners load it so the conformant-lane ingress
// member-fence resolves these members against their SoR.
//
//go:embed fixtures/conformant-personas.json
var conformantPersonas []byte

// ConformantSeedBundle returns a copy of the baked conformant Patient roster as
// a FHIR transaction Bundle (idempotent PUT entries). Patient-only: the ingress
// subject-bind reads only Patient by member identifier. Returns plain []byte
// (no error) like SandboxProviderPersonasBundle — it copies embedded bytes and
// cannot fail.
func ConformantSeedBundle() []byte {
	return append([]byte(nil), conformantPersonas...)
}

// providerData is the baked provider-data (plain-EHR) persona set: the 12
// self-contained clinical personas concatenated + deduped into one transaction
// Bundle, generated from the private canonical seed source and drift-guarded there
// (synthetic personas only, FR-39). Partners POST it to seed their own SoR.
//
//go:embed fixtures/provider-personas.json
var providerData []byte

// ProviderDataSeedBundle returns a copy of the baked provider-data seed Bundle as
// a FHIR transaction Bundle (idempotent PUT entries). Like ConformantSeedBundle it
// copies embedded bytes and cannot fail — the published module never recomputes
// against its shn-sdk pin, so the downloadable seed/provider-personas.json and this
// output are the same frozen artifact.
func ProviderDataSeedBundle() []byte {
	return append([]byte(nil), providerData...)
}
