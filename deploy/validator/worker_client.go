package main

import (
	"context"
	"errors"
	"net/http"
)

// Only the image supervisor's finite worker uses this client. Its transport is
// private and has no proxy: the fixed loopback URL is also the dial destination.
func runImageWarmup(ctx context.Context, base, path string, st markerState) error {
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	return runWarmupWithClient(ctx, base, path, st, imageWorkerClient(transport))
}

func imageWorkerClient(transport http.RoundTripper) *http.Client {
	client := httpClient()
	client.Transport = workerAuthorityTransport{transport}
	return client
}

type workerAuthorityTransport struct{ transport http.RoundTripper }

func (t workerAuthorityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "http" || req.URL.Host != "127.0.0.1:18080" || req.URL.User != nil || req.URL.Opaque != "" {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("worker destination refused")
	}
	// HAPI caches metadata globally. Preserve the historical worker authority so
	// that its existing metadata request cannot seed a private backend URL.
	// Clone headers and URL too: neither a caller nor a shared transport is mutated.
	owned := req.Clone(req.Context())
	owned.Host = "localhost:8080"
	return t.transport.RoundTrip(owned)
}
