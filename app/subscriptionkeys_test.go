//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
)

// predecessorDir writes a predecessor directory for a new identity of
// holder as shn rotate keeps one, the manifest and the signing key and no
// encryption key, and returns it with the identity.
func predecessorDir(t *testing.T, holder string) (string, shnsdk.Identity) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "predecessor")
	id, err := shnsdk.GenerateIdentity(holder)
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "enc.key")); err != nil {
		t.Fatal(err)
	}
	return dir, id
}

var subscriptionBinding = subscription.Binding{Subscriber: "h-test", Notifier: "payer-1", Topic: "pas"}

// The gateway does not start when IDENTITY_PREDECESSOR_DIR cannot be used:
// a directory that is not a key directory, one holding another holder's keys,
// or one holding the current signing key. Each is refused before anything is
// fetched (the discovery URL here answers nothing).
func TestBuild_RefusesAnUnusablePredecessor(t *testing.T) {
	for name, row := range map[string]struct {
		dir  func(t *testing.T, env map[string]string) string
		want string
	}{
		"a directory that does not exist": {func(t *testing.T, _ map[string]string) string {
			return filepath.Join(t.TempDir(), "missing")
		}, `is not a predecessor directory: read manifest.json`},
		"a bad key": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-test")
			if err := os.WriteFile(filepath.Join(dir, "sign.key"), []byte("bm90IGEga2V5"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `is not a predecessor directory: sign.key: not an ed25519 private key`},
		"a key that is not base64": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-test")
			if err := os.WriteFile(filepath.Join(dir, "sign.key"), []byte("!!"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `sign.key: not an ed25519 private key`},
		// The seed of one key with the public half of another: a key whose
		// key id would name a key nothing signs with.
		"a key whose public half is not its seed's": {func(t *testing.T, _ map[string]string) string {
			dir, id := predecessorDir(t, "h-test")
			other, err := shnsdk.GenerateIdentity("h-test")
			if err != nil {
				t.Fatal(err)
			}
			mixed := append(append([]byte(nil), id.SignPriv.Seed()...), other.SignPub...)
			if err := os.WriteFile(filepath.Join(dir, "sign.key"), []byte(base64.StdEncoding.EncodeToString(mixed)), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `sign.key: its public half is not its seed's`},
		"no signing key": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-test")
			if err := os.Remove(filepath.Join(dir, "sign.key")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `is not a predecessor directory: read sign.key`},
		"a manifest that is not JSON": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-test")
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `is not a predecessor directory: parse manifest.json`},
		"a manifest naming no holder": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-test")
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"role":"payer"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}, `manifest.json names no holder id`},
		"another holder's key": {func(t *testing.T, _ map[string]string) string {
			dir, _ := predecessorDir(t, "h-other")
			return dir
		}, `holds holder "h-other"'s key, and this gateway is holder "h-test"`},
		"the current keys": {func(t *testing.T, env map[string]string) string {
			return env["SHN_SECRETS"]
		}, `holds this gateway's current signing key, not the one a rotation replaced`},
	} {
		t.Run(name, func(t *testing.T) {
			env := buildEnvWithBundle(t, "payer", "payer")
			env["IDENTITY_PREDECESSOR_DIR"] = row.dir(t, env)
			_, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
			if err == nil || !strings.HasPrefix(err.Error(), "gateway: IDENTITY_PREDECESSOR_DIR (") || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("build: %v; want the IDENTITY_PREDECESSOR_DIR refusal naming %q", err, row.want)
			}
		})
	}
}

// A predecessor directory holds no encryption key, and none is needed.
func TestLoadPredecessor_ReadsNoEncryptionKey(t *testing.T) {
	dir, id := predecessorDir(t, "h-test")
	if _, err := os.Stat(filepath.Join(dir, "enc.key")); !os.IsNotExist(err) {
		t.Fatalf("the fixture holds an encryption key: %v", err)
	}
	holder, key, err := loadPredecessor(dir)
	if err != nil || holder != "h-test" || !key.Equal(id.SignPriv) {
		t.Fatalf("loadPredecessor = %q, %v", holder, err)
	}
}

// A predecessor of the same holder with another key is accepted: a value
// sealed under it opens through the overlap, and with the setting unset it is
// retired. The current key seals either way.
func TestSubscriptionKeys_ThePredecessorOpensWithinTheOverlap(t *testing.T) {
	env := buildEnvWithBundle(t, "payer", "payer")
	bundle, err := shnsdk.LoadBundle(env["SHN_SECRETS"])
	if err != nil {
		t.Fatal(err)
	}
	dir, prev := predecessorDir(t, "h-test")
	old, err := subscription.DeriveKeys("h-test", prev.SignPriv, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed := old.SealEndpoint(subscriptionBinding, "https://ehr.example/notify")

	with, err := subscriptionKeys(config{IdentityPredecessorDir: dir}, bundle)
	if err != nil {
		t.Fatalf("with the predecessor: %v", err)
	}
	if ep, err := with.OpenEndpoint(subscriptionBinding, sealed); err != nil || ep != "https://ehr.example/notify" {
		t.Fatalf("within the overlap: %q, %v", ep, err)
	}
	without, err := subscriptionKeys(config{}, bundle)
	if err != nil {
		t.Fatalf("without the predecessor: %v", err)
	}
	if _, err := without.OpenEndpoint(subscriptionBinding, sealed); !errors.Is(err, subscription.ErrKeyRetired) {
		t.Fatalf("past the overlap: %v, want ErrKeyRetired", err)
	}
	if with.KeyID() != without.KeyID() || with.KeyID() == old.KeyID() {
		t.Fatal("the predecessor's key seals: KeyID is not the current identity's")
	}
}

// The setting reaches the keys a full build derives.
func TestBuild_TheSettingReachesTheSubscriptionKeys(t *testing.T) {
	dir := t.TempDir()
	id, err := shnsdk.GenerateIdentity("h-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
		t.Fatal(err)
	}
	pred, prev := predecessorDir(t, "h-test")
	old, err := subscription.DeriveKeys("h-test", prev.SignPriv, nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed := old.SealEndpoint(subscriptionBinding, "https://ehr.example/notify")

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	keyBody := fmt.Sprintf(`{"pubkey":%q}`, base64.StdEncoding.EncodeToString(pub))
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(keyBody)) }))
	defer keys.Close()
	disc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{`+acceptingWire+`"endpoints":{},"authzPublicKeyURL":%q,"hubTransportKeyURL":%q}`, keys.URL, keys.URL)
	}))
	defer disc.Close()
	payerSystem := httptest.NewServer(http.NotFoundHandler())
	defer payerSystem.Close()
	for _, set := range []bool{true, false} {
		env := map[string]string{
			"ROLE": "payer", "SHN_SECRETS": dir, "SHN_DISCOVERY_URL": disc.URL, "SHN_FAKE_VALIDATOR": "1",
			"FHIR_DATA_URL": "https://fhir.test", "PAYER_DAVINCI_BASE_URL": payerSystem.URL,
		}
		if set {
			env["IDENTITY_PREDECESSOR_DIR"] = pred
		}
		b, err := build(context.Background(), func(k string) string { return env[k] }, io.Discard, nil)
		if b.gateway != nil {
			_ = b.gateway.Close()
		}
		if err != nil {
			t.Fatalf("build (set=%v): %v", set, err)
		}
		_, err = b.subscriptionKeys.OpenEndpoint(subscriptionBinding, sealed)
		if set && err != nil {
			t.Fatalf("with IDENTITY_PREDECESSOR_DIR set, the predecessor's value: %v", err)
		}
		if !set && !errors.Is(err, subscription.ErrKeyRetired) {
			t.Fatalf("unset, the predecessor's value: %v, want ErrKeyRetired", err)
		}
		if b.subscriptionKeys.KeyID() == "" || b.subscriptionKeys.KeyID() == old.KeyID() {
			t.Fatalf("build derived no keys, or the predecessor's (set=%v)", set)
		}
	}
}
