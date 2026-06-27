package engine

// orderTuple is one originated order's {system, code, display, dx}.
type orderTuple struct{ system, code, display, dx string }

// originationProfile holds the per-UC order tuples for one lane. The sandbox lane keeps the
// CPT/lumbar shape (byte-unchanged); the composite lane uses the HCPCS codes br-payer
// adjudicates, each with a clinically-coherent ICD-10-CM dx. br-payer carries NO dx
// in its scenarios and keys adjudication on the HCPCS code, so the dx is
// verdict-independent — chosen here for clinical coherence (NOT the lumbar M51.16, which would
// read as order-shaping).
type originationProfile struct {
	uc02, uc03, uc04, uc05, uc06, uc07, uc07hcpcs, uc08 orderTuple
}

func originationCodes(profile string) originationProfile {
	if profile == "composite" {
		return originationProfile{
			uc02:      orderTuple{systemHCPCSBuild, "E0250", "Hospital bed, fixed height, with side rails and mattress", "Z74.01"}, // covered/no-PA (admin doc → no DTR)
			uc03:      orderTuple{systemHCPCSBuild, "L8000", "Mastectomy bra", "Z90.11"},                                           // covered+PA+no-doc → skip DTR → A1 at submit (single-shot)
			uc04:      orderTuple{systemHCPCSBuild, "G0151", "Physical therapist services in home health, per 15 min", "M62.81"},   // conditional+PA+clinical → DTR → A4 pend → DiagnosticReport amendment → A1
			uc05:      orderTuple{systemHCPCSBuild, "G0151", "Physical therapist services in home health, per 15 min", "M62.81"},   // A4 pend → consent-gated CDex federated query resumes → A1 (needs a PEND code: L8000 approves at submit, leaving no pend to federate into)
			uc06:      orderTuple{systemHCPCSBuild, "G0151", "Physical therapist services in home health, per 15 min", "M62.81"},   // conditional+PA+clinical → DTR + attestation → A4 pend → A1
			uc07:      orderTuple{systemHCPCSBuild, "G0151", "Physical therapist services in home health, per 15 min", "M62.81"},   // A4 pend → patient attestation resumes → A1 (same pend-code requirement as UC-05)
			uc07hcpcs: orderTuple{systemHCPCSBuild, "L8000", "Mastectomy bra", "Z90.11"},
			uc08:      orderTuple{systemHCPCSBuild, "J3490", "Unclassified drugs (experimental, excluded)", "D57.1"}, // not-covered → CRD denial+rationale
		}
	}
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
