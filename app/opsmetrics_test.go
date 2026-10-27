//go:build !shn_coreonly

package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/pgstore"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// lastSnapshots keeps the latest snapshot of each instrument, as the OTel
// sink does.
type lastSnapshots struct {
	mu   sync.Mutex
	last map[*telemetry.Instrument][]telemetry.Measurement
}

func (s *lastSnapshots) Record(telemetry.Measurement) {}

func (s *lastSnapshots) RecordSnapshot(i *telemetry.Instrument, series []telemetry.Measurement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[*telemetry.Instrument][]telemetry.Measurement{}
	}
	s.last[i] = series
}

// of is the latest snapshot of i as "attr,attr=value" strings.
func (s *lastSnapshots) of(i *telemetry.Instrument) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.last[i] {
		out = append(out, strings.Join(m.Attributes, ",")+"="+strconv.FormatFloat(m.Value, 'f', -1, 64))
	}
	return out
}

// MON-R27: one info point per declared IG line, with the wire protocol
// given and the conformance level, whatever order and repetition the
// declared contract versions have.
func TestInfoPoints_MON_R27(t *testing.T) {
	got := infoPoints("2.0.0", []string{"pa.pas@2.2", "pa.crd@2.1", "pa.dtr@2.2", "malformed"}, "observe")
	var lines []string
	for _, p := range got {
		if p.Value != 1 || p.Attributes[0] != "2.0.0" || p.Attributes[1] != "observe" {
			t.Fatalf("point %+v", p)
		}
		lines = append(lines, p.Attributes[2])
	}
	if !slices.Equal(lines, []string{"2.1", "2.2"}) {
		t.Fatalf("lines %v, want each declared line once, sorted", lines)
	}
}

// MON-R25, R26, R45, MON-R15: each sample reports what is known now and
// leaves out what is not: a certificate kind that aged out, a key store with
// no signing key, a gateway with no registrar.
func TestEmitOps_ReportsWhatIsKnownNow_MON_R15(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	now := t0
	clock := func() time.Time { return now }
	certs := endpointcert.NewWithClock(clock)
	certs.Record(endpointcert.Of(endpointcert.Hub), endpointcert.Server, "hub.example", t0.Add(10*24*time.Hour))
	certs.Record(endpointcert.Lane(endpointcert.Validator, "2.1"), endpointcert.Server, "v.example", t0.Add(48*time.Hour))
	certs.Record(endpointcert.Of(endpointcert.Ingress), endpointcert.Self, "TLS_CERT_FILE", t0.Add(30*24*time.Hour))
	keys := pgstore.IngressKeyTimes{SigningNotAfter: t0.Add(time.Hour), HasSigning: true, NewestCreated: t0.Add(-23 * time.Hour), HasNewest: true}
	feed := newFeedFreshness(clock)
	feed.succeeded(4)
	snap := &lastSnapshots{}
	reg := telemetry.New(snap)
	src := opsSources{
		info:  infoPoints(gatewayInfoWire, []string{"pa.pas@2.1"}, "none"),
		certs: certs, keys: func() pgstore.IngressKeyTimes { return keys }, feed: feed, now: clock,
	}

	now = t0.Add(time.Minute)
	emitOps(reg, src)
	for i, want := range map[*telemetry.Instrument][]string{
		telemetry.GatewayInfo: {gatewayInfoWire + ",none,2.1=1"},
		telemetry.CertificateExpiry: {
			"hub,server,none=863940", "ingress,self,none=2591940", "validator,server,2.1=172740",
		},
		telemetry.IngressKeyRemaining: {"=3540"},
		telemetry.IngressKeyAge:       {"=82860"},
		telemetry.DependencyAge:       {"registrar-feed=60"},
		telemetry.PeersKnown:          {"=4"},
	} {
		if got := snap.of(i); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", i.Name, got, want)
		}
	}

	// A day with no response from the hub and the validator: both age out,
	// the gateway's own certificate does not. The feed's polls fail all day,
	// so its age keeps growing and its peers stay as last read.
	now = t0.Add(24*time.Hour + time.Minute)
	keys = pgstore.IngressKeyTimes{}
	emitOps(reg, src)
	if got := snap.of(telemetry.CertificateExpiry); !slices.Equal(got, []string{"ingress,self,none=2505540"}) {
		t.Errorf("certificate expiry after a day = %v, want only the gateway's own", got)
	}
	for _, i := range []*telemetry.Instrument{telemetry.IngressKeyRemaining, telemetry.IngressKeyAge} {
		if got := snap.of(i); len(got) != 0 {
			t.Errorf("%s = %v with no key held, want absent", i.Name, got)
		}
	}
	if got := snap.of(telemetry.DependencyAge); !slices.Equal(got, []string{"registrar-feed=86460"}) {
		t.Errorf("dependency age after a day of failed polls = %v, want it still growing", got)
	}
	if got := snap.of(telemetry.PeersKnown); !slices.Equal(got, []string{"=4"}) {
		t.Errorf("peers = %v, want the last successful read's", got)
	}
	// A read succeeds again: the age starts over.
	feed.succeeded(5)
	now = now.Add(30 * time.Second)
	emitOps(reg, src)
	if got := snap.of(telemetry.DependencyAge); !slices.Equal(got, []string{"registrar-feed=30"}) {
		t.Errorf("dependency age after recovery = %v, want 30 s", got)
	}

	// No key store and no registrar: neither is reported at all.
	src.keys, src.feed = nil, nil
	emitOps(reg, src)
	for _, i := range []*telemetry.Instrument{telemetry.IngressKeyRemaining, telemetry.IngressKeyAge, telemetry.DependencyAge, telemetry.PeersKnown} {
		if got := snap.of(i); len(got) != 0 {
			t.Errorf("%s = %v without its source, want absent", i.Name, got)
		}
	}
}

// writeCA writes a self-signed CA certificate expiring at notAfter, as PEM,
// to path.
func writeCA(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(notAfter.Unix()), NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

// MON-R25: a custom bundle named by SSL_CERT_FILE or SSL_CERT_DIR is one
// trust-root reading, its soonest certificate; the system's own bundle is
// never read, even when a setting names it beside a custom one; with no
// custom bundle there is no reading.
func TestRecordTrustRoots_MON_R25(t *testing.T) {
	soon, later, latest := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2033, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle.pem")
	writeCA(t, bundle, latest)
	writeCA(t, bundle, later)
	caDir := filepath.Join(dir, "ehr-ca")
	if err := os.Mkdir(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCA(t, filepath.Join(caDir, "ehr.pem"), soon)
	if err := os.WriteFile(filepath.Join(caDir, "notes.txt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The system's own bundle file and directory, here holding certificates
	// that would be the soonest, so a read of either shows.
	systemDir := filepath.Join(dir, "system-certs")
	if err := os.Mkdir(systemDir, 0o700); err != nil {
		t.Fatal(err)
	}
	systemFile := filepath.Join(dir, "system-bundle.pem")
	writeCA(t, systemFile, soon.Add(-24*time.Hour))
	writeCA(t, filepath.Join(systemDir, "root.pem"), soon.Add(-48*time.Hour))
	files, dirs := systemTrustFiles, systemTrustDirs
	systemTrustFiles, systemTrustDirs = []string{systemFile}, []string{systemDir}
	t.Cleanup(func() { systemTrustFiles, systemTrustDirs = files, dirs })
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"file only", map[string]string{"SSL_CERT_FILE": bundle}, []string{"trust-root/trust-root " + later.String()}},
		{"file and dir", map[string]string{"SSL_CERT_FILE": bundle, "SSL_CERT_DIR": systemDir + string(os.PathListSeparator) + caDir}, []string{"trust-root/trust-root " + soon.String()}},
		{"system paths only", map[string]string{"SSL_CERT_FILE": systemFile, "SSL_CERT_DIR": systemDir}, nil},
		{"unset", nil, nil},
		{"unreadable", map[string]string{"SSL_CERT_FILE": filepath.Join(dir, "missing.pem"), "SSL_CERT_DIR": filepath.Join(dir, "missing")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := endpointcert.New()
			recordTrustRoots(rec, func(k string) string { return tc.env[k] })
			var got []string
			for _, r := range rec.Readings() {
				got = append(got, r.Tag.Kind().String()+"/"+r.Role.String()+" "+r.NotAfter.String())
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("readings %v, want %v", got, tc.want)
			}
		})
	}
}

// The wiring: with a metrics sink on, build hands the engine its token hook
// and gives Run the sampler, whose first sample is written at once; with
// none, neither exists.
func TestBuild_WiresTheOperationalMetrics(t *testing.T) {
	off, _ := buildMonitored(t, monitoringGateway(t, nil))
	if off.opsStats != nil || off.gateway.TokenMetricWiredForTest() {
		t.Fatal("operational metrics wired with no metrics sink")
	}
	env := monitoringGateway(t, map[string]string{"METRICS_SERVICE": "ops-gw"})
	var out syncBuffer
	b, err := build(context.Background(), func(k string) string { return env[k] }, &out, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() {
		b.telemetry.Shutdown(context.Background())
		_ = b.gateway.Close()
	})
	if b.opsStats == nil || !b.gateway.TokenMetricWiredForTest() {
		t.Fatalf("with METRICS_SERVICE: sampler %v, token hook %v", b.opsStats != nil, b.gateway.TokenMetricWiredForTest())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the first sample is written before the loop waits
	b.opsStats(ctx)
	if !strings.Contains(out.String(), `"GatewayInfo":1`) {
		t.Fatalf("the sampler's first sample wrote no GatewayInfo line:\n%s", out.String())
	}
}

// A setting a dependency reads and the gateway makes a no-op warns at boot
// from its noEffectSettings entry when set, and says nothing when unset.
func TestNoEffectSettingLines(t *testing.T) {
	want := "gateway: WARNING: OTEL_GO_X_DEPRECATED_RUNTIME_METRICS has no effect: the OpenTelemetry Go runtime instrumentation reads it, and the deprecated process.runtime.go.* and runtime.uptime metrics it would add are dropped from every export"
	got := noEffectSettingLines(func(k string) string { return map[string]string{"OTEL_GO_X_DEPRECATED_RUNTIME_METRICS": "true"}[k] })
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q, want %q", got, want)
	}
	if got := noEffectSettingLines(func(string) string { return "" }); len(got) != 0 {
		t.Fatalf("unset: lines %q, want none", got)
	}
}
