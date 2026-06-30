package engine

import "testing"

// TestOriginationCodes_Sandbox verifies the sandbox lane's per-UC tuples are the CPT/lumbar
// shape and that "" and "sandbox" resolve identically (the default). The provider-data lane
// reads its orders from the SoR (orderSource → OpenOrder), not from this map, so there is no
// per-UC tuple to assert for it here.
func TestOriginationCodes_Sandbox(t *testing.T) {
	sb := originationCodes("sandbox")
	// default ("" and "sandbox") are the same
	if originationCodes("") != sb {
		t.Fatal(`originationCodes("") must equal sandbox`)
	}
	// UC-02 covered/no-PA: sandbox CPT 72100.
	if sb.uc02.code != "72100" || sb.uc02.system != systemCPTBuild {
		t.Fatalf("sandbox uc02 = %+v, want CPT 72100", sb.uc02)
	}
	// sandbox approve/pend/deny tuples are the CPT/lumbar shape (uc07hcpcs stays the L8000 stub).
	if sb.uc03.code != "72148" || sb.uc03.dx != "M51.16" || sb.uc07hcpcs.code != "L8000" {
		t.Fatalf("sandbox tuples drifted: uc03=%+v uc07hcpcs=%+v", sb.uc03, sb.uc07hcpcs)
	}
}
