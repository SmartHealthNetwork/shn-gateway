//go:build !shn_coreonly

package app

import (
	"fmt"

	"github.com/SmartHealthNetwork/shn-gateway/internal/buildversion"
)

// buildVersion is the version /health reports and the boot line naming the
// build's own. SHN_VERSION, when the deployment sets it, overrides what
// /health reports; the boot line then names both if they differ, so an
// override never hides what runs.
func buildVersion(shnVersion string) (healthVersion, line string) {
	v, src := buildversion.Version()
	return versionReport(shnVersion, v, src)
}

func versionReport(shnVersion, build string, src buildversion.Source) (healthVersion, line string) {
	line = fmt.Sprintf("gateway: build version %s (%s)", build, src)
	if shnVersion == "" {
		return build, line
	}
	if shnVersion != build {
		line += fmt.Sprintf("; /health reports SHN_VERSION %s instead", shnVersion)
	}
	return shnVersion, line
}
