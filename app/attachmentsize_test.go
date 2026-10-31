//go:build !shn_coreonly

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payerEnv is a payer gateway's minimal environment with extra set.
func payerEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"ROLE": "payer", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
		"PAYER_DAVINCI_BASE_URL": "https://payer.example",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// TestBuildWiresAttachmentCeilingToGateway proves ATTACHMENT_CEILING_BYTES
// reaches the engine.Config build() hands to engine.New, so a lower ceiling the
// boot log states is the one the gateway reads to.
func TestBuildWiresAttachmentCeilingToGateway(t *testing.T) {
	for raw, want := range map[string]int64{"": shnsdk.MaxAttachmentBytes, "1048576": 1 << 20} {
		extra := map[string]string{"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate", "ATTACHMENT_CEILING_BYTES": raw}
		b, _, err := buildProviderForPopulate(t, extra)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if b.attachmentCeiling != want {
			t.Errorf("ATTACHMENT_CEILING_BYTES=%q: the gateway's ceiling is %d, want %d", raw, b.attachmentCeiling, want)
		}
	}
}

// TestLoadConfig_AttachmentCeiling: ATTACHMENT_CEILING_BYTES defaults to the
// network's ceiling, accepts any positive integer up to it, and refuses to
// boot, naming the variable, on anything else: a ceiling above the network's
// would read a body the strict JSON scan cannot.
func TestLoadConfig_AttachmentCeiling(t *testing.T) {
	for _, c := range []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", shnsdk.MaxAttachmentBytes, false},
		{"1048576", 1 << 20, false},
		{"1", 1, false},
		{strconv.Itoa(shnsdk.MaxAttachmentBytes), shnsdk.MaxAttachmentBytes, false},
		{strconv.Itoa(shnsdk.MaxAttachmentBytes + 1), 0, true},
		{"67108864", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"16MiB", 0, true},
		{"1.5", 0, true},
	} {
		env := payerEnv(map[string]string{"ATTACHMENT_CEILING_BYTES": c.raw})
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if c.wantErr {
			if err == nil || !strings.Contains(err.Error(), "ATTACHMENT_CEILING_BYTES") {
				t.Errorf("ATTACHMENT_CEILING_BYTES=%q: want an error naming the variable, got %v", c.raw, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ATTACHMENT_CEILING_BYTES=%q: %v", c.raw, err)
			continue
		}
		if cfg.AttachmentCeiling != c.want {
			t.Errorf("ATTACHMENT_CEILING_BYTES=%q: got %d want %d", c.raw, cfg.AttachmentCeiling, c.want)
		}
	}
}

// TestLoadConfig_PayerAttachmentMaxBytes: unset, a payer declares no limit
// (unknown, never zero). Set, it is a positive integer up to 2^53-1, kept as
// declared even above the network's ceiling (a sender applies the smaller of
// the two), and anything else refuses to boot naming the variable. A provider
// gateway refuses it: it is the payer's statement.
func TestLoadConfig_PayerAttachmentMaxBytes(t *testing.T) {
	for _, c := range []struct {
		name    string
		env     map[string]string
		want    *int64
		wantErr string
	}{
		{name: "unset declares none", env: payerEnv(nil)},
		{name: "declared", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "5242880"}), want: ptr(int64(5 << 20))},
		{name: "above the network's ceiling stays as declared", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "104857600"}), want: ptr(int64(100 << 20))},
		{name: "the largest", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "9007199254740991"}), want: ptr(int64(1<<53 - 1))},
		{name: "past 2^53-1", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "9007199254740992"}), wantErr: "PAYER_ATTACHMENT_MAX_BYTES"},
		{name: "zero", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "0"}), wantErr: "PAYER_ATTACHMENT_MAX_BYTES"},
		{name: "negative", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "-5"}), wantErr: "PAYER_ATTACHMENT_MAX_BYTES"},
		{name: "not a number", env: payerEnv(map[string]string{"PAYER_ATTACHMENT_MAX_BYTES": "5MB"}), wantErr: "PAYER_ATTACHMENT_MAX_BYTES"},
		{name: "a provider gateway refuses", env: map[string]string{
			"ROLE": "provider", "SHN_SECRETS": "/x", "SHN_DISCOVERY_URL": "https://d",
			"PROVIDER_DTR_POPULATE_URL":  "https://populate.test/fhir/Questionnaire/$populate",
			"PAYER_ATTACHMENT_MAX_BYTES": "5242880",
		}, wantErr: "payer gateway setting"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := loadConfig(func(k string) string { return c.env[k] })
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := cfg.PayerAttachmentMaxBytes
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("PayerAttachmentMaxBytes = %v, want %v", deref(got), deref(c.want))
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// deref prints a declared limit, or "absent".
func deref(p *int64) string {
	if p == nil {
		return "absent"
	}
	return strconv.FormatInt(*p, 10)
}

// TestConvergeRegistry_CarriesTheDeclaredAttachmentLimit: a fed holder's
// declared attachmentMaxBytes reaches its registry entry as declared, and a
// holder that declared none reads as none (unknown, never zero).
func TestConvergeRegistry_CarriesTheDeclaredAttachmentLimit(t *testing.T) {
	var enc [32]byte
	enc[0] = 7
	declared := int64(5 << 20)
	holders := []shnsdk.Holder{
		{ID: "payer-a", Role: "payer", EncPub: base64.StdEncoding.EncodeToString(enc[:]), BaseURL: "https://payer-a.example", AttachmentMaxBytes: &declared},
		{ID: "payer-b", Role: "payer", EncPub: base64.StdEncoding.EncodeToString(enc[:]), BaseURL: "https://payer-b.example"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(holders)
	}))
	defer srv.Close()
	reg := shnsdk.NewRegistry()
	if _, err := convergeRegistry(context.Background(), http.DefaultClient, srv.URL, reg); err != nil {
		t.Fatalf("convergeRegistry: %v", err)
	}
	a, _ := reg.Lookup("payer-a")
	b, _ := reg.Lookup("payer-b")
	if a.AttachmentMaxBytes == nil || *a.AttachmentMaxBytes != declared || b.AttachmentMaxBytes != nil {
		t.Fatalf("payer-a %s, payer-b %s; want %d and absent", deref(a.AttachmentMaxBytes), deref(b.AttachmentMaxBytes), declared)
	}
}
