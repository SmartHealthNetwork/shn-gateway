package engine

// CanaryTwins maps each scenario member to its dedicated canary twin
// (observability Phase 3, settled decision #1): the monitor's scenario canary
// drives ONLY these members, so continuous canary runs never mutate the shared
// demo personas' state (EOB accumulation, auth-number overwrites) and canary
// audit records are attributable by PCI. Twins share the original's birthDate
// and take family "<orig>-Canary" — the member id alone makes the PCI distinct.
// internal/fhirseed mirrors this table (census-pinned by its canary_test.go) and
// seeds the twins into the deployed stack's FHIR tenants.
//
// The table must cover every member scenarioMember can resolve ON A LANE A
// CANARY RUNS AGAINST — one roster is not enough. sceneMember has three arms,
// and which one a deployment lands in is a configuration fact, not a build fact:
// a provider gateway that declares no origination profile is the family-keyed
// demo lane (gateway/app normalizes the unset value once, at the config
// boundary), so its scenario routes resolve the MBR-D-UC0N roster and a canary
// request asks for THOSE members' twins. A member missing here does not fall
// back to the shared persona — scenarioMember fails the request closed with a
// 400, which is the correct refusal but a dead canary. canary_test.go's
// TestCanaryTwins_CoverEveryLaneScenarioMember derives the required set from the
// scenarioMember call sites themselves, so this table cannot fall behind the
// members the handlers actually resolve.
//
// It stays engine-side (unlike the persona census, which lives in
// internal/fhirseed) because originate.go's canary scenario route resolves the
// twin from it on every canary origination — a gateway-internal read no
// substrate package can serve.
var CanaryTwins = map[string]string{
	// Conformance roster — sceneMember's default arm, plus MBR-UC07HCPCS, whose
	// scenario resolves the same member on every arm.
	"MBR-COVERED":        "MBR-CANARY-COVERED",
	"MBR-NOTCOVERED":     "MBR-CANARY-NOTCOVERED",
	"MBR-UC04":           "MBR-CANARY-UC04",
	"MBR-UC05":           "MBR-CANARY-UC05",
	"MBR-UC05-NOCONSENT": "MBR-CANARY-UC05-NOCONSENT",
	"MBR-UC06":           "MBR-CANARY-UC06",
	"MBR-UC07":           "MBR-CANARY-UC07",
	"MBR-UC07HCPCS":      "MBR-CANARY-UC07HCPCS",
	"MBR-UC08":           "MBR-CANARY-UC08",

	// Demo roster — sceneMember's demo arm, which is the lane every deployed
	// provider gateway that declares no origination profile runs, and therefore
	// the lane the deployed canary actually drives.
	"MBR-D-UC01":    "MBR-CANARY-D-UC01",
	"MBR-D-UC01-NC": "MBR-CANARY-D-UC01-NC",
	"MBR-D-UC02":    "MBR-CANARY-D-UC02",
	"MBR-D-UC03":    "MBR-CANARY-D-UC03",
	"MBR-D-UC04":    "MBR-CANARY-D-UC04",
	"MBR-D-UC05":    "MBR-CANARY-D-UC05",
	"MBR-D-UC05-NC": "MBR-CANARY-D-UC05-NC",
	"MBR-D-UC06":    "MBR-CANARY-D-UC06",
	"MBR-D-UC07":    "MBR-CANARY-D-UC07",
	"MBR-D-UC08":    "MBR-CANARY-D-UC08",
}
