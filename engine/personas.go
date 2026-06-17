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
}
