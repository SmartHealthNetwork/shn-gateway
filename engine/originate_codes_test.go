package engine

import "testing"

func TestOriginationCodes_CompositeVsSandbox(t *testing.T) {
	sb := originationCodes("sandbox")
	cp := originationCodes("composite")
	// default ("" and "sandbox") are the same
	if originationCodes("") != sb {
		t.Fatal(`originationCodes("") must equal sandbox`)
	}
	// UC-02 covered/no-PA: sandbox CPT 72100 vs composite HCPCS E0250
	if sb.uc02.code != "72100" || sb.uc02.system != systemCPTBuild {
		t.Fatalf("sandbox uc02 = %+v, want CPT 72100", sb.uc02)
	}
	if cp.uc02.code != "E0250" || cp.uc02.system != systemHCPCSBuild {
		t.Fatalf("composite uc02 = %+v, want HCPCS E0250", cp.uc02)
	}
	// composite approve-at-submit = L8000 (uc03 single-shot, uc07hcpcs); pend→resume→A1 = G0151
	// (uc04 DiagnosticReport amendment, uc05 consent-gated federated query, uc06/uc07 attestation
	// — the only CRD-PA-required + pend code br-payer has, spec §3). UC-05/07 need a code that
	// PENDS so their consent-query / attestation distinctive can resume the pend; L8000 approves
	// at submit (no pend), which would leave handleUC05/scenarioToPend with "expected pended
	// response". deny (uc08) = J3490 (CRD not-covered → PAS A2 "Not Certified").
	if cp.uc03.code != "L8000" || cp.uc07hcpcs.code != "L8000" {
		t.Fatalf("composite approve-at-submit codes wrong: uc03=%s uc07hcpcs=%s", cp.uc03.code, cp.uc07hcpcs.code)
	}
	if cp.uc04.code != "G0151" || cp.uc05.code != "G0151" || cp.uc06.code != "G0151" || cp.uc07.code != "G0151" {
		t.Fatalf("composite pend codes wrong: uc04=%s uc05=%s uc06=%s uc07=%s", cp.uc04.code, cp.uc05.code, cp.uc06.code, cp.uc07.code)
	}
	if cp.uc08.code != "J3490" {
		t.Fatalf("composite deny code wrong: uc08=%s", cp.uc08.code)
	}
	// every composite code carries a COHERENT dx (NOT reused lumbar M51.16) — spec §3
	for name, tup := range map[string]orderTuple{"uc02": cp.uc02, "uc03": cp.uc03, "uc04": cp.uc04, "uc05": cp.uc05, "uc06": cp.uc06, "uc07": cp.uc07, "uc07hcpcs": cp.uc07hcpcs, "uc08": cp.uc08} {
		if tup.dx == "M51.16" {
			t.Fatalf("composite %s must carry a coherent dx, not lumbar M51.16", name)
		}
		if tup.system != systemHCPCSBuild {
			t.Fatalf("composite %s must be HCPCS-system, got %s", name, tup.system)
		}
	}
	// sandbox approve/pend/deny tuples are byte-unchanged CPT/lumbar (uc07hcpcs stays L8000)
	if sb.uc03.code != "72148" || sb.uc03.dx != "M51.16" || sb.uc07hcpcs.code != "L8000" {
		t.Fatalf("sandbox tuples drifted: uc03=%+v uc07hcpcs=%+v", sb.uc03, sb.uc07hcpcs)
	}
}
