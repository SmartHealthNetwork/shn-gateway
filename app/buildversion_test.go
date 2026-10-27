//go:build !shn_coreonly

package app

import (
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/buildversion"
)

// /health reports the build's own version unless the deployment sets
// SHN_VERSION, and the boot line names both when they differ.
func TestVersionReport(t *testing.T) {
	for _, row := range []struct {
		name, shn, build string
		src              buildversion.Source
		health, line     string
	}{
		{"stamped, no override", "", "v1.0.0", buildversion.FromStamp,
			"v1.0.0", "gateway: build version v1.0.0 (stamp)"},
		{"unstamped, no override", "", buildversion.Unstamped, buildversion.FromNone,
			buildversion.Unstamped, "gateway: build version v0.0.0-unstamped (unstamped)"},
		{"a bad stamp says so", "", buildversion.Unstamped, buildversion.FromBadStamp,
			buildversion.Unstamped, "gateway: build version v0.0.0-unstamped (unstamped: the stamp is not a version)"},
		{"an override equal to the build", "v1.0.0", "v1.0.0", buildversion.FromStamp,
			"v1.0.0", "gateway: build version v1.0.0 (stamp)"},
		{"an override that differs", "120bc4c14e22", "v1.0.1-0.20261024041500-120bc4c14e22", buildversion.FromStamp,
			"120bc4c14e22", "gateway: build version v1.0.1-0.20261024041500-120bc4c14e22 (stamp); /health reports SHN_VERSION 120bc4c14e22 instead"},
	} {
		health, line := versionReport(row.shn, row.build, row.src)
		if health != row.health || line != row.line {
			t.Errorf("%s:\n got %q, %q\nwant %q, %q", row.name, health, line, row.health, row.line)
		}
	}
}
