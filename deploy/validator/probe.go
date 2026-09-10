// probe.go carries the bounded HTTP request path shared with the Kit's
// supervised validator children (see rows.go for the twin contract).
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

const maxOutcomeBytes = 1 << 20

// outcomeError is a verdict failure that also carries the answer that
// produced it (already bounded by maxOutcomeBytes, folded onto one line).
// Error() stays the bounded failure class (the image's marker allows only
// those strings, and the image never renders the excerpt); a caller that
// wants the answer uses errors.As.
type outcomeError struct {
	reason  string
	excerpt string
}

func (e *outcomeError) Error() string { return e.reason }

func excerptOf(raw []byte) string {
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b == '\n' || b == '\r' || b == '\t' {
			b = ' '
		}
		out = append(out, b)
	}
	return string(out)
}

// Loaded IG CapabilityStatements can exceed 1.9 MB. Metadata is drained without
// retaining its body and has a separate bound from validation OperationOutcomes.
const maxMetadataBytes = 4 << 20

func httpClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("response incomplete")
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("response oversized")
	}
	return raw, nil
}
func metadata(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("request invalid")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("metadata unavailable")
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil {
		return errors.New("response incomplete")
	}
	if n > maxMetadataBytes {
		return errors.New("response oversized")
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("metadata unavailable")
	}
	return nil
}
func validate(ctx context.Context, client *http.Client, base string, row warmup) error {
	body, err := fixtureBody(row)
	if err != nil {
		return errors.New("fixture unavailable")
	}
	return submitValidation(ctx, client, base, row, body)
}

func submitValidation(ctx context.Context, client *http.Client, base string, row warmup, body []byte) error {
	endpoint := base + "/" + row.resourceType + "/$validate?profile=" + url.QueryEscape(row.profile)
	if row.profile == "" {
		endpoint = base + "/" + row.resourceType + "/$validate"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("request invalid")
	}
	// A non-idempotent POST has no retry headers. Do not follow redirects or use
	// another request after uncertainty about server-side completion.
	req.Header.Set("Content-Type", "application/fhir+json")
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return errors.New("request failed")
	}
	defer resp.Body.Close()
	raw, err := readBounded(resp.Body, maxOutcomeBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return errors.New("redirect refused")
	}
	if err := assertVerdict(row, resp.StatusCode, raw); err != nil {
		return &outcomeError{reason: err.Error(), excerpt: excerptOf(raw)}
	}
	return nil
}
