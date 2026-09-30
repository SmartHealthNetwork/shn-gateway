package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestBuiltPayerReportsInboundExchanges drives the real wiring: a payer gateway
// built with METRICS_SERVICE reports a leg it refused on /substrate/inbound as
// an Exchange on its own stdout, and one built without it reports nothing and
// answers the same.
func TestBuiltPayerReportsInboundExchanges(t *testing.T) {
	pub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("payer-holder")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://payer.example"); err != nil {
		t.Fatal(err)
	}
	statuses := map[bool]int{}
	for _, on := range []bool{true, false} {
		env := map[string]string{
			"ROLE":                   "payer",
			"SHN_SECRETS":            dir,
			"SHN_DISCOVERY_URL":      disc.URL,
			"SHN_FAKE_VALIDATOR":     "1",
			"AUDIT_URL":              "http://127.0.0.1:1",
			"FHIR_DATA_URL":          "http://127.0.0.1:1/fhir",
			"PAYER_DAVINCI_BASE_URL": "http://127.0.0.1:1/fhir",
		}
		if on {
			env["METRICS_SERVICE"] = "shn-cloud-acme"
		}
		var out bytes.Buffer
		b, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
		t.Cleanup(func() {
			if b.gateway != nil {
				_ = b.gateway.Close()
			}
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		srv := httptest.NewServer(b.handler)
		res, err := http.Post(srv.URL+"/substrate/inbound", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		srv.Close()
		statuses[on] = res.StatusCode
		var exchange []map[string]any
		// stdout also carries the gateway's plain startup lines; EMF lines are JSON.
		var emf bytes.Buffer
		for _, ln := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(ln, "{") {
				emf.WriteString(ln + "\n")
			}
		}
		for _, m := range decodeEMFLines(t, &emf) {
			if m["Exchange"] != nil {
				exchange = append(exchange, m)
			}
		}
		if !on {
			if len(exchange) != 0 {
				t.Fatalf("reported without METRICS_SERVICE: %v", exchange)
			}
			continue
		}
		if len(exchange) != 1 {
			t.Fatalf("Exchange lines=%d want 1; stdout:\n%s", len(exchange), out.String())
		}
		m := exchange[0]
		if m["Service"] != "shn-cloud-acme" || m["direction"] != "inbound" || m["exchange"] != "other" || m["outcome"] != "refused" {
			t.Fatalf("Exchange=%v", m)
		}
	}
	if statuses[true] != statuses[false] || statuses[true] < 400 {
		t.Fatalf("metrics changed the answer or the leg was not refused: %v", statuses)
	}
}
