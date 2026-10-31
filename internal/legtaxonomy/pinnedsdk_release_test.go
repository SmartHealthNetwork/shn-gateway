//go:build !shn_toyworkstream

package legtaxonomy

import (
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestCatalogLegsInPinnedSDK: every leg the gateway's catalog serves has its
// flat and prefixed names in the leg table of the SDK this build resolved.
// Built alone (GOWORK=off), as a release is, that is the SDK go.mod pins, so
// a gateway release cannot pin an SDK whose table lacks a leg it serves.
//
// It pins the release build, which compiles PA and the federated query and
// never the toy test profile, so a build with the toy (the core-only and toy
// profile runs) leaves it out.
func TestCatalogLegsInPinnedSDK(t *testing.T) {
	if missing := missingWireNames(catalogTaxonomies(), shnsdk.PrefixedTransactionTypes()); len(missing) > 0 {
		t.Errorf("the SDK this build resolved lacks %d catalog leg(s); release the SDK and pin it first:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}
