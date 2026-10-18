// Package fhirclient is a bounded read-only FHIR R4 HTTP client.
package fhirclient

import (
	"context"
	"encoding/json"
	"errors"
	fhir "github.com/samply/golang-fhir-models/fhir-models/fhir"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const maxBodyBytes = 8 << 20

// HTTPError carries status evidence without upstream response content.
type HTTPError struct{ StatusCode int }

func (*HTTPError) Error() string { return "FHIR endpoint rejected request" }

// TransportError distinguishes connection and body-read failures from invalid content.
type TransportError struct{ Cause error }

func (*TransportError) Error() string   { return "FHIR endpoint unavailable" }
func (e *TransportError) Unwrap() error { return e.Cause }

// InvalidResponseError indicates malformed or oversized FHIR content.
type InvalidResponseError struct{}

func (*InvalidResponseError) Error() string { return "invalid FHIR response" }

type Client struct {
	base string
	hc   *http.Client
}

func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{strings.TrimRight(baseURL, "/"), hc}
}

// BaseURL returns the FHIR base URL the client reads, without a trailing slash.
func (c *Client) BaseURL() string { return c.base }

// HTTPClient returns the HTTP client the client reads with.
func (c *Client) HTTPClient() *http.Client { return c.hc }

func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/"+path, nil)
	if err != nil {
		return nil, 0, &InvalidResponseError{}
	}
	req.Header.Set("Accept", "application/fhir+json")
	resp, err := Do(c.hc, req)
	if err != nil {
		return nil, 0, &TransportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, &HTTPError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, 0, &TransportError{err}
	}
	if len(body) > maxBodyBytes {
		return nil, 0, &InvalidResponseError{}
	}
	return body, resp.StatusCode, nil
}

// RetryPause is how long a read waits before its one more try.
const RetryPause = 250 * time.Millisecond

// RetryWithin is how soon a first try must have failed to be tried again.
const RetryWithin = time.Second

// Do sends req, a GET, with hc, and sends it once more after RetryPause when
// the first try failed within RetryWithin with no answer (the connection
// refused, reset or closed) or with 502, 503 or 504, and req's deadline
// leaves more than the pause. A system of record rolling to a new task, or
// the proxy in front of it with no task to reach for a moment, fails at once
// and often answers the next try; a GET changes nothing, so trying it again
// is safe. A slower failure is not tried again: a second wait would hold the
// caller's own answer past the time its requester gives it. Any other answer
// or failure, a 4xx among them, is returned as it came, and a second failure
// is the answer.
func Do(hc *http.Client, req *http.Request) (*http.Response, error) {
	began := time.Now()
	resp, err := hc.Do(req)
	if time.Since(began) > RetryWithin || !again(req.Context(), resp, err) {
		return resp, err
	}
	if resp != nil {
		// Closed unread: a body that stalls would hold the try up, and a
		// connection to a server mid-roll is not worth keeping.
		resp.Body.Close()
	}
	pause := time.NewTimer(RetryPause)
	defer pause.Stop()
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-pause.C:
	}
	return hc.Do(req.Clone(req.Context()))
}

// Windows reports a refused or reset connection by its own codes, which its
// syscall.ECONNREFUSED and ECONNRESET do not match; no other system uses them.
const (
	wsaConnRefused = syscall.Errno(10061)
	wsaConnReset   = syscall.Errno(10054)
)

// again reports whether a GET's first try may be made again.
func again(ctx context.Context, resp *http.Response, err error) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= RetryPause {
		return false
	}
	if err != nil {
		return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, wsaConnRefused) || errors.Is(err, wsaConnReset) ||
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// ValidateResource checks identity and JSON shape; it does not certify a FHIR profile.
func ValidateResource(body []byte, resourceType string) error {
	var head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if json.Unmarshal(body, &head) != nil || head.ResourceType != resourceType || strings.TrimSpace(head.ID) == "" {
		return &InvalidResponseError{}
	}
	var resource any
	switch resourceType {
	case "Patient":
		resource = &fhir.Patient{}
	case "Coverage":
		resource = &fhir.Coverage{}
	case "Condition":
		resource = &fhir.Condition{}
	case "Observation":
		// The model uses json.Number, which also accepts quoted numbers. Check
		// the consumed quantity's wire type before it can become clinical evidence.
		var observation struct {
			ValueQuantity *struct {
				Value json.RawMessage `json:"value"`
			} `json:"valueQuantity"`
		}
		if json.Unmarshal(body, &observation) != nil {
			return &InvalidResponseError{}
		}
		if observation.ValueQuantity != nil {
			value := strings.TrimSpace(string(observation.ValueQuantity.Value))
			if value != "" && value != "null" && value[0] != '-' && (value[0] < '0' || value[0] > '9') {
				return &InvalidResponseError{}
			}
		}
		resource = &fhir.Observation{}
	case "DiagnosticReport":
		resource = &fhir.DiagnosticReport{}
	case "DocumentReference":
		resource = &fhir.DocumentReference{}
	case "Procedure":
		resource = &fhir.Procedure{}
	case "DeviceRequest":
		resource = &fhir.DeviceRequest{}
	case "ServiceRequest":
		resource = &fhir.ServiceRequest{}
	case "Organization":
		resource = &fhir.Organization{}
	}
	if resource != nil && json.Unmarshal(body, resource) != nil {
		return &InvalidResponseError{}
	}
	return nil
}
func (c *Client) Search(ctx context.Context, resourceType string, q url.Values) (*fhir.Bundle, error) {
	path := resourceType
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	body, _, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var head struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
	}
	var b fhir.Bundle
	if json.Unmarshal(body, &head) != nil || head.ResourceType != "Bundle" || head.Type != "searchset" || json.Unmarshal(body, &b) != nil {
		return nil, &InvalidResponseError{}
	}
	entries := b.Entry[:0]
	for _, e := range b.Entry {
		if e.Search != nil && e.Search.Mode != nil && e.Search.Mode.String() != "match" {
			continue
		}
		if err := ValidateResource(e.Resource, resourceType); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	b.Entry = entries
	return &b, nil
}
func (c *Client) Read(ctx context.Context, resourceType, id string) ([]byte, bool, error) {
	body, status, err := c.get(ctx, resourceType+"/"+id)
	if status == 404 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err = ValidateResource(body, resourceType); err != nil {
		return nil, false, err
	}
	return body, true, nil
}
