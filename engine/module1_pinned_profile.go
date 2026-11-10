//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"fmt"
)

// pinnedProfile is the one profile a request this gateway built pins in its
// meta.profile. Its egress check asks for it, so the check certifies exactly
// the profile the builder pinned. A request that pins none, or more than one,
// is a build this gateway cannot certify.
func pinnedProfile(body []byte) (string, error) {
	var r struct {
		Meta struct {
			Profile []string `json:"profile"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("read the request's pinned profile: %w", err)
	}
	if len(r.Meta.Profile) != 1 {
		return "", fmt.Errorf("the request pins %d profiles, want one", len(r.Meta.Profile))
	}
	return r.Meta.Profile[0], nil
}
