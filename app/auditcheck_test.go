//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestAuditCheckTarget(t *testing.T) {
	if got := auditCheckTarget(""); got.ID != "AUDIT_ENDPOINT" || got.Kind != checks.KindUndiscovered || got.URL != "" {
		t.Fatalf("no endpoint: %+v, want an undiscovered AUDIT_ENDPOINT", got)
	}
	if got := auditCheckTarget("https://audit.example"); got.ID != "AUDIT_ENDPOINT" || got.Kind != checks.KindReachable || got.URL != "https://audit.example" {
		t.Fatalf("an endpoint: %+v, want a reachable AUDIT_ENDPOINT", got)
	}
}

// buildWithDiscovery builds a payer gateway whose discovery descriptor carries
// endpoints.audit = audit ("" for none).
func buildWithDiscovery(t *testing.T, audit string) built {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	t.Cleanup(keys.Close)
	endpoints := `{}`
	if audit != "" {
		endpoints = fmt.Sprintf(`{"audit":%q}`, audit)
	}
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":%s,"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, endpoints, keys.URL, keys.URL)
	}))
	t.Cleanup(disc.Close)
	payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(payer.Close)
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-payer")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ROLE":               "payer",
		"SHN_SECRETS":        dir,
		"SHN_DISCOVERY_URL":  disc.URL,
		"SHN_FAKE_VALIDATOR": "1",
		"FHIR_DATA_URL":      "https://127.0.0.1:1/fhir",
		// A payer gateway forwards to its participant's own Da Vinci endpoint.
		"PAYER_DAVINCI_BASE_URL":       payer.URL,
		"PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
	}
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	t.Cleanup(func() {
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return b
}

func auditResult(t *testing.T, b built) checks.Result {
	t.Helper()
	results, err := b.checksRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.ID == "AUDIT_ENDPOINT" {
			return r
		}
	}
	t.Fatalf("no AUDIT_ENDPOINT among %d check results", len(results))
	return checks.Result{}
}

// TestBuiltGatewayReportsAnUnknownAuditEndpoint: discovery naming no audit
// endpoint is loud. /internal/checks fails AUDIT_ENDPOINT endpoint-unknown,
// and the outbox still exists, in process, sending nothing.
func TestBuiltGatewayReportsAnUnknownAuditEndpoint(t *testing.T) {
	b := buildWithDiscovery(t, "")
	if b.auditAppender == nil || b.auditAppender.EndpointKnown() {
		t.Fatal("want an outbox with no endpoint known")
	}
	r := auditResult(t, b)
	if r.OK || r.Failure == nil || r.Failure.Code != checks.FailEndpointUnknown {
		t.Fatalf("AUDIT_ENDPOINT = %+v, want failing %s", r, checks.FailEndpointUnknown)
	}
}

// TestBuiltGatewayChecksTheAuditEndpointDiscoveryNames: with an endpoint
// named, the outbox knows it and the check probes it.
func TestBuiltGatewayChecksTheAuditEndpointDiscoveryNames(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // only POST /append is served
	}))
	defer plane.Close()
	b := buildWithDiscovery(t, plane.URL)
	if b.auditAppender == nil || !b.auditAppender.EndpointKnown() {
		t.Fatal("want an outbox with the discovered endpoint")
	}
	r := auditResult(t, b)
	if !r.OK || r.Target != plane.URL {
		t.Fatalf("AUDIT_ENDPOINT = %+v, want a passing probe of %s", r, plane.URL)
	}
}
