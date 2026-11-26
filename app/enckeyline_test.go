//go:build !shn_coreonly

package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// A built gateway writes one boot line about its bundle's encryption key, for
// each verdict of comparing manifest.json's encPub with the key enc.key
// derives: informational when they agree, a warning when the manifest's is
// missing, malformed, of small order or another key. The line names each key
// by its fingerprint, never by its bytes.
func TestBuild_WritesTheBundlesEncryptionKeyVerdict(t *testing.T) {
	other, err := shnsdk.GenerateIdentity("h-other")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		verdict shnsdk.EncKeyVerdict
		// encPub is what manifest.json states; nil leaves the field out, and
		// "" keeps the one WriteBundle wrote.
		encPub  *string
		warning bool
		says    string
	}{
		{shnsdk.EncKeyOK, ptr(""), false, "manifest.json's encPub is the key enc.key derives"},
		{shnsdk.EncKeyManifestMissing, nil, true, "manifest.json states no encPub"},
		{shnsdk.EncKeyManifestMalformed, ptr("not a key"), true, "is not a 32-byte base64 key"},
		{shnsdk.EncKeyManifestLowOrder, ptr(base64.StdEncoding.EncodeToString(make([]byte, 32))), true, "is a small-order X25519 key"},
		{shnsdk.EncKeyManifestDiffers, ptr(base64.StdEncoding.EncodeToString(other.EncPub[:])), true, "is not the key enc.key derives"},
	} {
		t.Run(string(row.verdict), func(t *testing.T) {
			dir := t.TempDir()
			id, err := shnsdk.GenerateIdentity("h-payer")
			if err != nil {
				t.Fatal(err)
			}
			if err := shnsdk.WriteBundle(dir, id, "payer", "https://holder.example"); err != nil {
				t.Fatal(err)
			}
			if row.encPub == nil || *row.encPub != "" {
				restateEncPub(t, dir, row.encPub)
			}
			payerSystem := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(payerSystem.Close)
			_, stdout := buildPayerFrom(t, dir, payerSystem.URL, nil)
			var lines []string
			for _, l := range strings.Split(stdout, "\n") {
				if strings.Contains(l, "encryption key") {
					lines = append(lines, l)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("%d encryption key lines, want one:\n%s", len(lines), stdout)
			}
			line := lines[0]
			if !strings.HasPrefix(line, "gateway: ") || strings.Contains(line, "WARNING") != row.warning ||
				!strings.Contains(line, "encryption key "+string(row.verdict)+":") || !strings.Contains(line, row.says) {
				t.Fatalf("line %q, want verdict %s (warning %v) saying %q", line, row.verdict, row.warning, row.says)
			}
			if fp := shnsdk.KeyFingerprint(id.EncPub[:]); !strings.Contains(line, "("+fp+")") {
				t.Fatalf("line %q does not name the derived key's fingerprint %s", line, fp)
			}
			// A manifest key the line compares is named by its own fingerprint.
			if row.verdict == shnsdk.EncKeyManifestLowOrder || row.verdict == shnsdk.EncKeyManifestDiffers {
				raw, err := base64.StdEncoding.DecodeString(*row.encPub)
				if err != nil {
					t.Fatal(err)
				}
				if fp := shnsdk.KeyFingerprint(raw); !strings.Contains(line, "("+fp+")") {
					t.Fatalf("line %q does not name the manifest key's fingerprint %s", line, fp)
				}
			}
			if strings.Contains(line, base64.StdEncoding.EncodeToString(id.EncPub[:])) || strings.Contains(line, base64.StdEncoding.EncodeToString(other.EncPub[:])) {
				t.Fatalf("line %q prints a key", line)
			}
		})
	}
}

// A verdict this gateway does not know, from a later shn-sdk, is a warning
// that names it.
func TestEncKeyLine_AnUnknownVerdictIsAWarning(t *testing.T) {
	line := encKeyLine(shnsdk.EncKeyReport{Verdict: "manifest-from-the-future", Derived: "0a1b2c3d"})
	if !strings.HasPrefix(line, "WARNING: ") || !strings.Contains(line, `"manifest-from-the-future"`) || !strings.Contains(line, "0a1b2c3d") {
		t.Fatalf("line %q", line)
	}
}

// restateEncPub rewrites the encPub manifest.json in dir states: encPub, or
// none when nil.
func restateEncPub(t *testing.T, dir string, encPub *string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "encPub")
	if encPub != nil {
		m["encPub"] = *encPub
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
