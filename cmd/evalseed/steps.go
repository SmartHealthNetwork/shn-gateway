// gateway/cmd/evalseed/steps.go
package main

import (
	"context"
	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
)

type step struct {
	name string
	run  func(ctx context.Context, c *fhirseed.Client) error
}

// provider-data seed sequence: CR prepopulation libraries + provider-data
// persona bundles a partner needs, and nothing else.
func seedSteps() []step {
	return []step{
		{"WaitReady", func(ctx context.Context, c *fhirseed.Client) error { return c.WaitReady(ctx, 20*time.Minute) }},
		{"CreatePartitions(provider)", func(ctx context.Context, c *fhirseed.Client) error {
			return c.CreatePartitions(ctx, []string{"provider"})
		}},
		{"InstallCRLibraries", func(ctx context.Context, c *fhirseed.Client) error { return c.InstallCRLibraries(ctx) }},
		{"WarmUpPopulate", func(ctx context.Context, c *fhirseed.Client) error { return c.WarmUpPopulate(ctx) }},
		{"LoadProviderDataBundles(provider)", func(ctx context.Context, c *fhirseed.Client) error { return c.LoadProviderDataBundles(ctx, "provider") }},
		{"WriteSeedMarker(provider)", func(ctx context.Context, c *fhirseed.Client) error { return c.WriteSeedMarker(ctx, "provider") }},
	}
}

func seedStepNames() []string {
	s := seedSteps()
	names := make([]string, len(s))
	for i := range s {
		names[i] = s[i].name
	}
	return names
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
