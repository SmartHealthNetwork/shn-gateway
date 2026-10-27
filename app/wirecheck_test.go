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
	"sync/atomic"
	"testing"
	"time"
)

// acceptingWire is the wire part of a descriptor that accepts what this gateway
// sends: a network on 1.1.0 that accepts 2.0.0 beside it. Every test
// discovery splices it in, since the start check refuses any other.
const acceptingWire = `"wireProtocolVersion":"1.1.0","wireProtocolVersionsAccepted":["1.1.0","2.0.0"],`

// The start check reads the descriptor once, before any key fetch, and
// starts only when its accepted list holds 2.0.0. The list is the stated one,
// an empty or null one included, or [wireProtocolVersion] when the key is
// absent (PARTICIPANT_PROTOCOL.md §1a). A descriptor it cannot read refuses
// too: unknown is not accepted.
func TestStartCheckAcceptsOnlyANetworkThatAcceptsWhatItSends(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var keyFetches atomic.Int32
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyFetches.Add(1)
		fmt.Fprintf(w, `{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	}))
	defer keys.Close()
	rest := fmt.Sprintf(`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	const refused = "does not accept wire protocol 2.0.0"
	const unknown = "cannot confirm the network accepts wire protocol 2.0.0"
	for _, row := range []struct {
		name   string
		status int
		body   string
		want   string // "" starts
	}{
		{"accepted beside 1.1.0", 200, `{` + acceptingWire + rest, ""},
		{"a network on 2.0.0", 200, `{"wireProtocolVersion":"2.0.0","wireProtocolVersionsAccepted":["2.0.0"],` + rest, ""},
		{"absent, the network on 2.0.0", 200, `{"wireProtocolVersion":"2.0.0",` + rest, ""},
		{"only 1.1.0", 200, `{"wireProtocolVersion":"1.1.0","wireProtocolVersionsAccepted":["1.1.0"],` + rest, refused + ` (it accepts ["1.1.0"])`},
		{"an empty list is a stated one", 200, `{"wireProtocolVersion":"2.0.0","wireProtocolVersionsAccepted":[],` + rest, refused + " (it accepts [])"},
		{"a null list is a stated one", 200, `{"wireProtocolVersion":"2.0.0","wireProtocolVersionsAccepted":null,` + rest, refused},
		{"absent, the network on 1.1.0", 200, `{"wireProtocolVersion":"1.1.0",` + rest, refused + ` (it accepts ["1.1.0"])`},
		{"absent, and no version", 200, `{` + rest, unknown},
		{"absent, and an empty version", 200, `{"wireProtocolVersion":"",` + rest, unknown},
		{"an empty object", 200, `{}`, unknown},
		{"null", 200, `null`, unknown},
		{"another key's case is not the key", 200, `{"wireProtocolVersion":"1.1.0","wireProtocolVersionsAccepted":["1.1.0"],"WIREPROTOCOLVERSIONSACCEPTED":["2.0.0"],` + rest, refused + ` (it accepts ["1.1.0"])`},
		{"a version that is not a string", 200, `{"wireProtocolVersion":2,` + rest, unknown},
		{"a release candidate is not 2.0.0", 200, `{"wireProtocolVersion":"1.1.0","wireProtocolVersionsAccepted":["1.1.0","2.0.0-rc"],` + rest, refused},
		{"a later version is not 2.0.0", 200, `{"wireProtocolVersion":"3.0.0","wireProtocolVersionsAccepted":["3.0.0"],` + rest, refused},
		{"a list that is a string", 200, `{"wireProtocolVersion":"1.1.0","wireProtocolVersionsAccepted":"2.0.0",` + rest, unknown},
		{"not a descriptor", 200, `["2.0.0"]`, unknown},
		{"not JSON", 200, `<html>`, unknown},
		{"an error answer", 503, `{` + acceptingWire + rest, unknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			keyFetches.Store(0)
			disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(row.status)
				fmt.Fprint(w, row.body)
			}))
			defer disc.Close()
			_, _, err := resolveDiscoveryWaiting(context.Background(), oneDiscoveryClient(&http.Client{Timeout: 5 * time.Second}), config{DiscoveryURL: disc.URL})
			if row.want == "" {
				if err != nil || keyFetches.Load() != 2 {
					t.Fatalf("refused (%v) or fetched %d keys, want a start with both", err, keyFetches.Load())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("err %v, want a refusal naming %q", err, row.want)
			}
			if keyFetches.Load() != 0 {
				t.Fatalf("fetched %d keys before refusing: the check decides first", keyFetches.Load())
			}
			if row.want == refused || strings.HasPrefix(row.want, refused) {
				for _, remedy := range []string{disc.URL, "Point it at a network whose discovery accepts 2.0.0", "run a gateway before v1.0.0"} {
					if !strings.Contains(err.Error(), remedy) {
						t.Errorf("the refusal does not name %q: %v", remedy, err)
					}
				}
			}
		})
	}
	// An unreachable discovery refuses as unknown.
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	if _, _, err := resolveDiscoveryWaiting(context.Background(), oneDiscoveryClient(&http.Client{Timeout: 5 * time.Second}), config{DiscoveryURL: url}); err == nil || !strings.Contains(err.Error(), unknown) {
		t.Fatalf("unreachable: err %v, want a refusal naming %q", err, unknown)
	}
}
