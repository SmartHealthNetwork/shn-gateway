package engine

// PersonaRef maps a console/UI state key to the demo persona member ID it
// resolves to. It is the single enumeration of the demo personas consumed by
// both internal/devstack (to seed the in-process Stack's PCI fields) and
// internal/holdersim (to serve GET /admin/state). The persona DATA lives in
// stubPersonas; this is only the ordered key↔member mapping, so the PCI is
// always derived via (SystemOfRecord).ResolvePatient — never a duplicated literal.
type PersonaRef struct {
	Key      string // the JSON state key the UI reads (e.g. "coveredPci")
	MemberID string // the persona member ID (e.g. "MBR-COVERED")
}

// PersonaRefs is the canonical ordered persona enumeration. Treat it as
// read-only — it is shared by multiple consumers (devstack, holdersim) and must
// not be mutated.
var PersonaRefs = []PersonaRef{
	{"coveredPci", "MBR-COVERED"},
	{"notCoveredPci", "MBR-NOTCOVERED"},
	{"uc04Pci", "MBR-UC04"},
	{"uc05Pci", "MBR-UC05"},
	{"uc06Pci", "MBR-UC06"},
	{"uc07Pci", "MBR-UC07"},
	{"uc08Pci", "MBR-UC08"},
	{"uc07HcpcsPci", "MBR-UC07HCPCS"},
}

// CanaryTwins maps each sandbox scenario member to its dedicated canary twin
// (observability Phase 3, settled decision #1): the monitor's scenario canary
// drives ONLY these members, so continuous canary runs never mutate the shared
// demo personas' state (EOB accumulation, auth-number overwrites) and canary
// audit records are attributable by PCI. Twins share the original's birthDate
// and take family "<orig>-Canary" — the member id alone makes the PCI distinct.
// internal/fhirseed mirrors this table (census-pinned by its canary_test.go).
var CanaryTwins = map[string]string{
	"MBR-COVERED":        "MBR-CANARY-COVERED",
	"MBR-NOTCOVERED":     "MBR-CANARY-NOTCOVERED",
	"MBR-UC04":           "MBR-CANARY-UC04",
	"MBR-UC05":           "MBR-CANARY-UC05",
	"MBR-UC05-NOCONSENT": "MBR-CANARY-UC05-NOCONSENT",
	"MBR-UC06":           "MBR-CANARY-UC06",
	"MBR-UC07":           "MBR-CANARY-UC07",
	"MBR-UC07HCPCS":      "MBR-CANARY-UC07HCPCS",
	"MBR-UC08":           "MBR-CANARY-UC08",
}
