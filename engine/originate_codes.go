package engine

// orderTuple is one originated order's {system, code, display, dx}.
type orderTuple struct{ system, code, display, dx string }

// originationProfile holds the per-UC order tuples for the sandbox lane (the CPT/lumbar
// shape, byte-unchanged). The provider-data lane reads its orders from the SoR (orderSource
// → OpenOrder), not from these tuples, so it does not key on this map.
type originationProfile struct {
	uc02, uc03, uc04, uc05, uc06, uc07, uc07hcpcs, uc08 orderTuple
}

func originationCodes(profile string) originationProfile {
	// sandbox (default, incl. ""): the existing CPT/lumbar shape — MUST be byte-unchanged.
	return originationProfile{
		uc02:      orderTuple{systemCPTBuild, "72100", "X-ray lumbar spine", "M51.16"},
		uc03:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
		uc04:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
		uc05:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
		uc06:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
		uc07:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
		uc07hcpcs: orderTuple{systemHCPCSBuild, "L8000", "Breast prosthesis, mastectomy bra", "M51.16"}, // existing DEF-4 stub stays for sandbox
		uc08:      orderTuple{systemCPTBuild, "72148", "MRI lumbar spine w/o contrast", "M51.16"},
	}
}
