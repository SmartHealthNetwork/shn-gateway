// Package fhirseed is the partner/Kit FHIR seed LOADER: a small HTTP client that drives a HAPI
// FHIR server through the seed sequence a partner (or the desktop Kit's shnkitd) needs before
// running scenarios — wait for readiness, create tenant partitions, install the operated-CQL
// prepop Library fixtures, warm the $populate engine, load provider-data persona bundles, and
// write/poll the seed-complete marker. It is deliberately thin: the heavy sandbox persona
// builders (full PersonaSet seeding, CQL Library construction) stay in the private canonical
// seeder; this package only carries the wire-level steps that are safe to publish.
package fhirseed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Client drives a HAPI FHIR server through the seed-loader flow.
type Client struct {
	// Base is the FHIR base URL WITHOUT a tenant, e.g. http://localhost:8080/fhir. Tenant-taking
	// methods address {Base}/{tenant}; never pass a tenant-qualified Base.
	Base string
	// HTTP is the client used for all requests. nil uses http.DefaultClient.
	HTTP *http.Client
	// Logf receives progress/log lines. nil is silent.
	Logf func(format string, args ...any)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// WaitReady polls {Base}/DEFAULT/metadata with GET until it returns HTTP 200, or timeout elapses.
// A cold HAPI boot (including US Core IG download) can take several minutes. Progress is logged
// every 10 seconds.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	const (
		interval = 5 * time.Second
		logEvery = 10 * time.Second
	)
	url := c.Base + "/DEFAULT/metadata"
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var lastLog time.Time
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}
		resp, err := c.httpClient().Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for HAPI at %s", timeout, url)
		}

		if time.Since(lastLog) >= logEvery {
			if err != nil {
				c.logf("fhirseed: waiting for HAPI (%v)…", err)
			} else {
				c.logf("fhirseed: waiting for HAPI (status %d)…", resp.StatusCode)
			}
			lastLog = time.Now()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s waiting for HAPI at %s", timeout, url)
		case <-time.After(interval):
		}
	}
}

// CreatePartitions POSTs $partition-management-create-partition for each named partition, using
// stable deterministic integer ids starting at 101.
//
// Operation path:
// POST {Base}/DEFAULT/$partition-management-create-partition
// Payload: {"resourceType":"Parameters","parameter":[{"name":"id","valueInteger":<n>},{"name":"name","valueCode":<name>}]}
func (c *Client) CreatePartitions(ctx context.Context, names []string) error {
	for i, name := range names {
		body := fmt.Sprintf(
			`{"resourceType":"Parameters","parameter":[{"name":"id","valueInteger":%d},{"name":"name","valueCode":%q}]}`,
			i+101, name,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.Base+"/DEFAULT/$partition-management-create-partition",
			strings.NewReader(body),
		)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		respBody := string(rb)
		// Idempotent: a partition that already exists is success on re-run. Match the PHRASE,
		// not a brittle code — stock HAPI says "already exists"; HAPI on persistent Postgres
		// returns HAPI-1309 "Partition name … is already defined" (hfj_partition rows survive a
		// restart).
		if resp.StatusCode/100 != 2 && !strings.Contains(respBody, "already exists") && !strings.Contains(respBody, "already defined") {
			return fmt.Errorf("create partition %q: %d: %s", name, resp.StatusCode, respBody)
		}
	}
	return nil
}

// InstallCRLibraries PUTs the embedded prepop CQL Libraries (CRPrepopLibraries) into DEFAULT
// (HAPI-1318: a Library cannot live in a tenant partition). HAPI CR compiles each text/cql → ELM
// on first $populate reference. Idempotent (PUT by id).
func (c *Client) InstallCRLibraries(ctx context.Context) error {
	dbase := c.Base + "/DEFAULT"
	for id, body := range CRPrepopLibraries() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, dbase+"/Library/"+id, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/fhir+json")
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("PUT Library/%s: %d: %s", id, resp.StatusCode, rb)
		}
		c.logf("fhirseed: installed Library/%s into DEFAULT", id)
	}
	return nil
}

// WarmUpPopulate compiles each prepop library's ELM so the first real $populate is not the cold
// compile. Measured: GET DEFAULT/Library/<id>/$evaluate (no subject) compiles + caches the
// canonical-keyed ELM that $populate reuses (cold first $populate 5.37s → 4.38s after warm-up). A
// non-2xx is a hard failure (the engine is known-broken before scenarios run).
func (c *Client) WarmUpPopulate(ctx context.Context) error {
	dbase := c.Base + "/DEFAULT"
	for id := range CRPrepopLibraries() {
		u := dbase + "/Library/" + id + "/$evaluate"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("$evaluate Library/%s (warm-up): %d: %s", id, resp.StatusCode, rb)
		}
		c.logf("fhirseed: warmed ELM for Library/%s", id)
	}
	return nil
}

// LoadProviderDataBundles loads every shnsdk.ProviderDataPersonas() transaction Bundle into the
// given tenant by POSTing the published bytes as a FHIR transaction — the SAME bytes a partner
// would POST to their own SoR. Each bundle is freshened (FreshenObservations) before the POST.
func (c *Client) LoadProviderDataBundles(ctx context.Context, tenant string) error {
	for _, persona := range shnsdk.ProviderDataPersonas() {
		raw, err := shnsdk.ProviderDataBundle(persona)
		if err != nil {
			return fmt.Errorf("ProviderDataBundle(%q): %w", persona, err)
		}
		freshened, err := FreshenObservations(raw)
		if err != nil {
			return fmt.Errorf("freshen %q: %w", persona, err)
		}
		if err := c.PostTransaction(ctx, tenant, freshened); err != nil {
			return fmt.Errorf("post %q bundle: %w", persona, err)
		}
		c.logf("fhirseed: loaded provider-data persona %q", persona)
	}
	return nil
}

// PostTransaction POSTs a FHIR transaction Bundle to {Base}/{tenant} and fails on a non-2xx.
func (c *Client) PostTransaction(ctx context.Context, tenant string, bundle []byte) error {
	url := c.Base + "/" + tenant
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bundle))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, rb)
	}
	return nil
}

// WriteSeedMarker PUTs a Basic/seed-complete resource into the tenant partition. Its presence is
// the "all seeding done" signal a readiness probe polls for (WaitForSeedMarker) — distinct from
// "seeding started", which a per-partition data check would falsely report. Basic is
// profile-free, so no $validate.
func (c *Client) WriteSeedMarker(ctx context.Context, tenant string) error {
	const body = `{"resourceType":"Basic","id":"seed-complete","code":{"coding":[{"system":"urn:shn:seed","code":"complete"}]}}`
	url := c.Base + "/" + tenant + "/Basic/seed-complete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("write seed-complete marker: %d: %s", resp.StatusCode, rb)
	}
	return nil
}

// WaitForSeedMarker blocks until the tenant's Basic/seed-complete marker is present — the signal
// that seeding finished. Polls every 5s up to timeout.
func (c *Client) WaitForSeedMarker(ctx context.Context, tenant string, timeout time.Duration) error {
	marker := c.Base + "/" + tenant + "/Basic/seed-complete"
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, marker, nil)
		if err != nil {
			return fmt.Errorf("build seed-marker request: %w", err)
		}
		resp, err := c.httpClient().Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("seed-complete marker not present after %s at %s", timeout, marker)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// FreshenObservations rewrites effectiveDateTime to now (UTC RFC3339) on every Observation entry
// of a transaction Bundle, leaving all other entries byte-identical (re-marshalled). This exists
// because prepop CQL enforces a 3-month ObservationLookBack: a fixture's static timestamp would
// age out and silently empty the QuestionnaireResponse, so freshening to now keeps the loaded
// data honestly recent.
func FreshenObservations(bundleJSON []byte) ([]byte, error) {
	var bundle map[string]any
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return nil, fmt.Errorf("unmarshal bundle: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	entries, _ := bundle["entry"].([]any)
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		res, ok := entry["resource"].(map[string]any)
		if !ok {
			continue
		}
		if res["resourceType"] == "Observation" {
			res["effectiveDateTime"] = now
		}
	}
	out, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("marshal bundle: %w", err)
	}
	return out, nil
}
