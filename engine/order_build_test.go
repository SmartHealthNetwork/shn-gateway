package engine

import (
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestBuildServiceRequestCoded_SystemConstsAnchoredToParser pins the gateway-local
// system constants (order_build.go) to the EXACT canonical values shnsdk.ParseServiceRequestProductCoding
// accepts — the FHIR validator does not check system URI values, so without this a drifted
// systemHCPCSBuild/systemCPTBuild would silently build a wrong-system order (deferral D-PCB-1).
func TestBuildServiceRequestCoded_SystemConstsAnchoredToParser(t *testing.T) {
	cases := []struct{ name, system, code string }{
		{"cpt", systemCPTBuild, "72148"},
		{"hcpcs", systemHCPCSBuild, "L8000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sr, err := BuildServiceRequestCoded(c.system, c.code, "display", "M51.16", "Patient/MBR-X")
			if err != nil {
				t.Fatalf("BuildServiceRequestCoded: %v", err)
			}
			gotSystem, gotCode, _, err := shnsdk.ParseServiceRequestProductCoding(sr)
			if err != nil {
				t.Fatalf("ParseServiceRequestProductCoding rejected the built SR — %s const drifted from the canonical value: %v", c.name, err)
			}
			if gotSystem != c.system {
				t.Fatalf("%s: round-trip system %q != input %q", c.name, gotSystem, c.system)
			}
			if gotCode != c.code {
				t.Fatalf("%s: round-trip code %q != input %q", c.name, gotCode, c.code)
			}
		})
	}
}
