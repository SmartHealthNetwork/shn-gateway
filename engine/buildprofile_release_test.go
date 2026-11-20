//go:build !shn_toyworkstream

package engine

import "github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"

// thisBuildTags are the build profile tags this test binary was built with:
// none in a release build.
var thisBuildTags []string

// profileTaxonomies are the test profiles' taxonomies this build compiles in,
// beside the network's: none in a release build.
func profileTaxonomies() []legtaxonomy.Taxonomy { return nil }
