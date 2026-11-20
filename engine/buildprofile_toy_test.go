//go:build shn_toyworkstream

package engine

import "github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"

// thisBuildTags are the build profile tags this test binary was built with:
// the toy test profile's.
var thisBuildTags = []string{"shn_toyworkstream"}

// profileTaxonomies are the test profiles' taxonomies this build compiles in,
// beside the network's: the toy's.
func profileTaxonomies() []legtaxonomy.Taxonomy { return []legtaxonomy.Taxonomy{legtaxonomy.Toy()} }
