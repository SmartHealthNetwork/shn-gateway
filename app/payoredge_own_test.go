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
	"slices"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestBuildWiresPublishedPayerIdentitiesToNativeResponder proves build()'s
// NativeOption list gives the payer-edge identity mapping the identities THIS
// holder publishes on the converged /holders feed, keyed by this gateway's own
// holder id. Deleting WithPayorEdgePublishedIdentities leaves this red: a payer
// that publishes a second identity would still be routable to on it at the
// network and refused on it at its own edge.
func TestBuildWiresPublishedPayerIdentitiesToNativeResponder(t *testing.T) {
	payer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer payer.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(keyBody))
	}))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()

	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test-payer-published-ids")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	configured := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00300"}
	published := shnsdk.PayerIdentifier{System: "http://example.org/fhir/ehr-assigned-payer-id", Value: "204"}
	env := map[string]string{
		"ROLE":                         "payer",
		"SHN_SECRETS":                  dir,
		"SHN_DISCOVERY_URL":            disc.URL,
		"SHN_FAKE_VALIDATOR":           "1",
		"FHIR_DATA_URL":                "https://sor.example/fhir",
		"PAYER_DAVINCI_BASE_URL":       payer.URL,
		"PAYER_DAVINCI_CRD_SERVICE_ID": "svc",
		"PAYER_DAVINCI_PAYOR_OWN":      configured.System + "|" + configured.Value,
		"PAYER_DAVINCI_PAYOR_BACKEND":  "urn:example:payer-backend|BACKEND-7",
	}
	b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
	})
	reader, ok := b.nativeResponder.(interface {
		OwnPayerIdentitiesForTest() []shnsdk.PayerIdentifier
	})
	if !ok {
		t.Fatalf("nativeResponder %T does not expose OwnPayerIdentitiesForTest", b.nativeResponder)
	}
	if got := reader.OwnPayerIdentitiesForTest(); !slices.Equal(got, []shnsdk.PayerIdentifier{configured}) {
		t.Fatalf("before the feed carries this holder: own = %+v, want only the configured identity", got)
	}
	// The registrar poller converges the feed onto this same registry value; this
	// holder's own entry arrives with both of its published identities.
	b.reg.Set(id.HolderID, shnsdk.RegistryEntry{
		ID: id.HolderID, Role: "payer",
		PayerIDs: []shnsdk.PayerIdentifier{configured, published},
	})
	got := reader.OwnPayerIdentitiesForTest()
	if !slices.Equal(got, []shnsdk.PayerIdentifier{configured, published}) {
		t.Fatalf("own = %+v, want the configured identity and the one this holder publishes", got)
	}
}
