//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/checks"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient/redirecttest"
)

// The boot fetches of discovery and of the two trust-anchor keys follow no
// redirect: each fails the boot as a fetch that got no usable answer does,
// its error naming redirect-refused with the status and the Location's
// origin, never its path or query; the other origin and path receive
// nothing.
func TestBootFetchesFollowNoRedirect(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	defer keys.Close()
	for _, fetch := range []string{"discovery", "authorization key", "hub transport key"} {
		for _, c := range redirecttest.Cases() {
			t.Run(fetch+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				discURL := f.URL
				if fetch != "discovery" {
					authzKey, hubKey := keys.URL, keys.URL
					if fetch == "authorization key" {
						authzKey = f.URL
					} else {
						hubKey = f.URL
					}
					disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, authzKey, hubKey)
					}))
					defer disc.Close()
					discURL = disc.URL
				}
				client := f.Client
				client.Timeout = 5 * time.Second
				_, _, err := resolveDiscovery(context.Background(), client, config{DiscoveryURL: discURL})
				f.NotFollowed(t)
				if err == nil {
					t.Fatal("the boot went on past a redirect")
				}
				msg := err.Error()
				if !serviceclient.IsRedirectRefused(err) || !strings.Contains(msg, serviceclient.RedirectRefused) || !strings.Contains(msg, fmt.Sprintf("answered %d", c.Code)) {
					t.Fatalf("boot error %q, want redirect-refused naming %d", msg, c.Code)
				}
				redirecttest.NoSecret(t, "the boot error", msg)
			})
		}
	}
}

// The /internal/checks probe of a network service (and of a sidecar)
// follows no redirect, as its calls do: a redirect fails the check as a
// failing status, its hint naming the status and redirect-refused, never
// unreachable; the other origin and path receive nothing, and the Location's
// path and query appear nowhere in the result.
func TestNetworkServiceChecksFollowNoRedirect(t *testing.T) {
	for _, id := range []string{"HUB_URL", "AUTHZ_URL", "CONSENT_URL", "REGISTRAR_URL", "AUDIT_ENDPOINT", "SHN_DISCOVERY_URL", "AUTHZ_PUBKEY_URL", "HUB_TRANSPORT_KEY_URL", "FHIR_VALIDATE_URL_2_1", "FHIR_CERTIFY_URL_2_2"} {
		for _, c := range redirecttest.Cases() {
			t.Run(id+"/"+c.Name, func(t *testing.T) {
				f := redirecttest.Serve(t, c)
				targets := tagCheckTargets(endpointcert.New(), []checks.Target{{ID: id, Kind: checks.KindReachable, URL: f.URL + "/"}}, f.Client)
				results, err := checks.NewRunner(targets, f.Client, time.Now).Run(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				f.NotFollowed(t)
				if len(results) != 1 {
					t.Fatalf("%d results", len(results))
				}
				r := results[0]
				want := fmt.Sprintf("HTTP %d, %s", c.Code, serviceclient.RedirectRefused)
				if r.OK || r.Failure == nil || r.Failure.Code != checks.FailHTTPStatus || r.Failure.Hint != want {
					t.Fatalf("result %+v (failure %+v), want failed %s %q", r, r.Failure, checks.FailHTTPStatus, want)
				}
				redirecttest.NoSecret(t, "the result", fmt.Sprintf("%+v %+v", r, *r.Failure))
			})
		}
	}
}

// A participant's system keeps its own redirect rule in /internal/checks: a
// probe its system answers with a redirect from https to http is refused by
// the participant policy and stays unreachable, with that policy's own hint
// and no redirect-refused class; the plain origin receives nothing.
func TestParticipantChecksKeepTheirRedirectClass(t *testing.T) {
	var cleartext redirecttest.Case
	for _, c := range redirecttest.Cases() {
		if c.Kind == redirecttest.HTTPSToHTTP {
			cleartext = c
		}
	}
	for _, id := range []string{"PAYER_DAVINCI_BASE_URL", "PAYER_DAVINCI_DTR_BASE_URL", "PROVIDER_DTR_POPULATE_URL", "PROVIDER_CDEX_TASK_URL"} {
		t.Run(id, func(t *testing.T) {
			f := redirecttest.Serve(t, cleartext)
			targets := tagCheckTargets(endpointcert.New(), []checks.Target{{ID: id, Kind: checks.KindReachable, URL: f.URL + "/"}}, f.Client)
			results, err := checks.NewRunner(targets, f.Client, time.Now).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			f.NotFollowed(t)
			r := results[0]
			if r.OK || r.Failure == nil || r.Failure.Code != checks.FailUnreachable || r.Failure.Hint != "smartauth: a redirect from https to http is not followed" {
				t.Fatalf("result %+v (failure %+v), want unreachable with the participant policy's hint", r, r.Failure)
			}
			if strings.Contains(fmt.Sprintf("%+v %+v", r, *r.Failure), serviceclient.RedirectRefused) {
				t.Fatalf("a participant's check carries the network-service class: %+v", *r.Failure)
			}
		})
	}
}
