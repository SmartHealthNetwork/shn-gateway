// Package fhirclient is a small read-only FHIR R4 HTTP client (Search and Read).
// It depends only on
// the stdlib and samply FHIR models — no substrate-internal imports.
package fhirclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	fhir "github.com/samply/golang-fhir-models/fhir-models/fhir"
)

// maxBodyBytes caps a response body so a misbehaving/adversarial FHIR server cannot
// exhaust memory (8 MiB, matching internal/wire.MaxResponseBytes; kept as a local const
// so this package stays free of substrate-internal imports — see the package doc).
const maxBodyBytes = 8 << 20

// Client reads from a FHIR R4 server base URL (e.g. https://ehr.example/fhir).
// Unauthenticated by default; an injected token source supplies auth.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a Client for baseURL. A nil hc uses http.DefaultClient. Trailing
// slashes on baseURL are trimmed so path joining is unambiguous.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// Search issues GET {base}/{resourceType}?{query} and returns the parsed Bundle.
// Non-2xx or a decode failure is an error (callers decide how to degrade).
func (c *Client) Search(ctx context.Context, resourceType string, query url.Values) (*fhir.Bundle, error) {
	u := c.base + "/" + resourceType
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("fhirclient: new request: %w", err)
	}
	req.Header.Set("Accept", "application/fhir+json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fhirclient: GET %s: %w", resourceType, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("fhirclient: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("fhirclient: GET %s: status %d: %s", resourceType, resp.StatusCode, truncate(body))
	}
	var b fhir.Bundle
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("fhirclient: decode bundle: %w", err)
	}
	return &b, nil
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max])
	}
	return string(b)
}
