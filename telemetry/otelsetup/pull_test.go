package otelsetup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
)

// pullSecret is a bearer token planted to prove it never reaches a line.
const pullSecret = "pull-secret-token-7f3a"

// testPKI is a CA and the server and client certificates it signs, written
// as PEM files a test can name.
type testPKI struct {
	caFile, serverCert, serverKey, clientCert, clientKey string
	pool                                                 *x509.CertPool
	client                                               tls.Certificate
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pull test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(serial int64, usage x509.ExtKeyUsage) (certPEM, keyPEM string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "pull test"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
			KeyUsage: x509.KeyUsageDigitalSignature, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kder, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	}
	sc, sk := leaf(2, x509.ExtKeyUsageServerAuth)
	cc, ck := leaf(3, x509.ExtKeyUsageClientAuth)
	p := testPKI{
		caFile:     writeFile(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))),
		serverCert: writeFile(t, "server.pem", sc), serverKey: writeFile(t, "server-key.pem", sk),
		clientCert: writeFile(t, "client.pem", cc), clientKey: writeFile(t, "client-key.pem", ck),
		pool: x509.NewCertPool(),
	}
	p.pool.AddCert(ca)
	if p.client, err = tls.X509KeyPair([]byte(cc), []byte(ck)); err != nil {
		t.Fatal(err)
	}
	return p
}

// pullEnv is OTEL_METRICS_EXPORTER=prometheus with kv added.
func pullEnv(kv ...string) map[string]string {
	m := map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// MON-R6, MON-R7: the default bind is localhost:9464 over plain HTTP; a
// bind off this machine is served over TLS with a bearer token, a client CA
// or both.
func TestLoad_PullListener_MON_R6_R7(t *testing.T) {
	pki := newTestPKI(t)
	tlsKV := []string{"METRICS_PULL_TLS_CERT_FILE", pki.serverCert, "METRICS_PULL_TLS_KEY_FILE", pki.serverKey}
	for _, c := range []struct {
		name                 string
		env                  map[string]string
		addr                 string
		loopback, tls, token bool
		clientCA             bool
	}{
		{"default", pullEnv(), "localhost:9464", true, false, false, false},
		{"loopback address and port", pullEnv("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1", "OTEL_EXPORTER_PROMETHEUS_PORT", "19464"), "127.0.0.1:19464", true, false, false, false},
		{"IPv6 loopback", pullEnv("OTEL_EXPORTER_PROMETHEUS_HOST", "::1"), "[::1]:9464", true, false, false, false},
		{"loopback with a token over plain HTTP", pullEnv("METRICS_PULL_TOKEN", pullSecret), "localhost:9464", true, false, true, false},
		{"loopback over TLS", pullEnv(tlsKV...), "localhost:9464", true, true, false, false},
		{"all interfaces, TLS and token", pullEnv(append(tlsKV, "OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", "METRICS_PULL_TOKEN", pullSecret)...), "0.0.0.0:9464", false, true, true, false},
		{"all interfaces, TLS and client CA", pullEnv(append(tlsKV, "OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", "METRICS_PULL_CLIENT_CA_FILE", pki.caFile)...), "0.0.0.0:9464", false, true, false, true},
		{"a host name, TLS, token and client CA", pullEnv(append(tlsKV, "OTEL_EXPORTER_PROMETHEUS_HOST", "metrics.internal", "METRICS_PULL_TOKEN", pullSecret, "METRICS_PULL_CLIENT_CA_FILE", pki.caFile)...), "metrics.internal:9464", false, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Load(env(c.env), nil)
			if len(got.Problems) != 0 || got.Pull == nil {
				t.Fatalf("pull %+v, problems %+v", got.Pull, got.Problems)
			}
			p := got.Pull
			if p.Addr != c.addr || p.Loopback != c.loopback || (p.TLS != nil) != c.tls || (p.Token != "") != c.token || p.ClientCA != c.clientCA {
				t.Fatalf("pull %+v", p)
			}
			if c.clientCA && (p.TLS.ClientAuth != tls.RequireAndVerifyClientCert || p.TLS.ClientCAs == nil) {
				t.Fatalf("client CA set, but TLS does not require a verified client certificate: %+v", p.TLS)
			}
			if got.Mode != ModeEnvironment || got.OTLP != nil {
				t.Fatalf("mode %s, OTLP %+v: pull alone is environment mode without push", got.Mode, got.OTLP)
			}
			tel := Build(got, testIdentity, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(context.Background())
			if tel.Sink() == nil || tel.PullAddr() != c.addr || tel.ExportCheck().Present {
				t.Fatalf("sink %v, pull addr %q, export check %+v", tel.Sink(), tel.PullAddr(), tel.ExportCheck())
			}
			if st := tel.ConfigCheck(); !st.Present || !st.OK {
				t.Fatalf("telemetry-config %+v", st)
			}
			lines := strings.Join(tel.Lines(), "\n")
			if !strings.Contains(lines, "Prometheus pull on: GET /metrics at "+c.addr) || strings.Contains(lines, pullSecret) {
				t.Fatalf("boot lines %q", lines)
			}
		})
	}
}

// MON-R7, MON-R42, MON-R44: each pull setting that is not valid, and every
// unprotected bind off this machine, turns Prometheus pull off (and only it:
// OTLP push, also asked for, stays on), names the setting and the fix, never
// the value, and fails telemetry-config. Nothing is served.
func TestLoad_PullRejections_MON_R7_R42(t *testing.T) {
	pki := newTestPKI(t)
	notPEM := writeFile(t, "not.pem", "not a certificate")
	tlsKV := []string{"METRICS_PULL_TLS_CERT_FILE", pki.serverCert, "METRICS_PULL_TLS_KEY_FILE", pki.serverKey}
	for _, c := range []struct {
		name, setting, value string
		env                  []string
	}{
		{"port not a number", "OTEL_EXPORTER_PROMETHEUS_PORT", "metrics-port", nil},
		{"port zero", "OTEL_EXPORTER_PROMETHEUS_PORT", "0", nil},
		{"port out of range", "OTEL_EXPORTER_PROMETHEUS_PORT", "70000", nil},
		{"host with a port", "OTEL_EXPORTER_PROMETHEUS_HOST", "localhost:9464", nil},
		{"host with brackets", "OTEL_EXPORTER_PROMETHEUS_HOST", "[::1]", nil},
		{"host with a space", "OTEL_EXPORTER_PROMETHEUS_HOST", "metrics host", nil},
		{"token with a space", "METRICS_PULL_TOKEN", "pull secret-token", nil},
		{"token with a quote", "METRICS_PULL_TOKEN", `pull"secret-token`, nil},
		{"TLS certificate without key", "METRICS_PULL_TLS_KEY_FILE", "", []string{"METRICS_PULL_TLS_CERT_FILE", pki.serverCert}},
		{"TLS key without certificate", "METRICS_PULL_TLS_CERT_FILE", "", []string{"METRICS_PULL_TLS_KEY_FILE", pki.serverKey}},
		{"TLS pair not PEM", "METRICS_PULL_TLS_CERT_FILE", notPEM, []string{"METRICS_PULL_TLS_KEY_FILE", notPEM}},
		{"TLS key not the certificate's", "METRICS_PULL_TLS_CERT_FILE", pki.serverCert, []string{"METRICS_PULL_TLS_KEY_FILE", pki.clientKey}},
		{"client CA not PEM", "METRICS_PULL_CLIENT_CA_FILE", notPEM, tlsKV},
		{"client CA missing", "METRICS_PULL_CLIENT_CA_FILE", "/nonexistent/pull-ca-secret.pem", tlsKV},
		{"client CA without TLS", "METRICS_PULL_CLIENT_CA_FILE", pki.caFile, nil},
		{"all interfaces, nothing set", "OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", nil},
		{"IPv6 all interfaces, nothing set", "OTEL_EXPORTER_PROMETHEUS_HOST", "::", nil},
		{"a host name, nothing set", "OTEL_EXPORTER_PROMETHEUS_HOST", "metrics.internal", nil},
		{"all interfaces, a token over plain HTTP", "OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", []string{"METRICS_PULL_TOKEN", pullSecret}},
		{"all interfaces, TLS without token or client CA", "OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", tlsKV},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := pullEnv(c.env...)
			m["OTEL_METRICS_EXPORTER"] = "otlp,prometheus"
			if c.value != "" {
				m[c.setting] = c.value
			}
			got := Load(env(m), nil)
			if got.Pull != nil {
				t.Fatalf("pull %+v is on", got.Pull)
			}
			if got.OTLP == nil {
				t.Fatal("OTLP push turned off by a pull setting")
			}
			var hit *Problem
			for i := range got.Problems {
				if got.Problems[i].Setting == c.setting {
					hit = &got.Problems[i]
				}
			}
			if hit == nil || hit.Piece != "Prometheus pull" || hit.Fix == "" {
				t.Fatalf("problem %+v among %+v", hit, got.Problems)
			}
			var lines []string
			tel := Build(got, testIdentity, Options{Logf: func(f string, a ...any) { lines = append(lines, f) }})
			defer tel.Shutdown(context.Background())
			if tel.PullAddr() != "" {
				t.Fatalf("pull listener %q built", tel.PullAddr())
			}
			tel.ServePull(t.Context()) // returns at once: nothing to serve
			lines = append(lines, tel.Lines()...)
			for _, l := range lines {
				if len(c.value) > 3 && strings.Contains(l, c.value) || strings.Contains(l, pullSecret) {
					t.Fatalf("a line carries a value: %q", l)
				}
			}
			if !strings.Contains(hit.Line(), c.setting) || !strings.Contains(hit.Line(), "Prometheus pull is off") {
				t.Fatalf("boot line %q", hit.Line())
			}
			st := tel.ConfigCheck()
			if !st.Present || st.OK || !st.Off || !containsString(st.Settings, c.setting) {
				t.Fatalf("telemetry-config %+v", st)
			}
			if !tel.ExportCheck().Present {
				t.Fatal("OTLP push is not on")
			}
		})
	}
}

// Pull settings without the prometheus exporter do nothing, and a warning
// names them.
func TestLoad_PullSettingsWithoutPrometheusWarn(t *testing.T) {
	got := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_PROMETHEUS_HOST": "0.0.0.0", "METRICS_PULL_TOKEN": pullSecret}), nil)
	if got.Pull != nil || len(got.Problems) != 0 {
		t.Fatalf("pull %+v, problems %+v", got.Pull, got.Problems)
	}
	w := strings.Join(got.Warnings, "\n")
	if !strings.Contains(w, "OTEL_EXPORTER_PROMETHEUS_HOST, METRICS_PULL_TOKEN") || strings.Contains(w, pullSecret) {
		t.Fatalf("warnings %q", got.Warnings)
	}
	if got := Load(env(map[string]string{"METRICS_PULL_TOKEN": pullSecret}), nil); !got.Set || got.Mode != ModeOff {
		t.Fatalf("a pull setting alone: set=%t mode=%s", got.Set, got.Mode)
	}
}

// MON-R7, MON-R42: with a token, a scrape without it, with another, or in
// another scheme gets 401 and a Bearer challenge, whatever its path; with
// it, GET /metrics is served and nothing else is.
func TestPullHandler_BadTokenGets401_MON_R7_R42(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "served") })
	h := pullHandler(metrics, pullSecret)
	for _, c := range []struct {
		name, method, path, auth string
		status                   int
	}{
		{"no token", "GET", "/metrics", "", 401},
		{"wrong token", "GET", "/metrics", "Bearer not-the-token", 401},
		{"token with a suffix", "GET", "/metrics", "Bearer " + pullSecret + "x", 401},
		{"token as a prefix", "GET", "/metrics", "Bearer " + pullSecret[:len(pullSecret)-1], 401},
		{"basic scheme", "GET", "/metrics", "Basic " + pullSecret, 401},
		{"no scheme", "GET", "/metrics", pullSecret, 401},
		{"other path without token", "GET", "/debug/pprof/", "", 401},
		{"the token", "GET", "/metrics", "Bearer " + pullSecret, 200},
		{"the token, scheme in lower case", "GET", "/metrics", "bearer " + pullSecret, 200},
		{"the token, HEAD", "HEAD", "/metrics", "Bearer " + pullSecret, 200},
		{"the token, other path", "GET", "/", "Bearer " + pullSecret, 404},
		{"the token, POST", "POST", "/metrics", "Bearer " + pullSecret, 405},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("status %d, want %d", w.Code, c.status)
			}
			if c.status == 401 {
				if w.Header().Get("WWW-Authenticate") != `Bearer realm="metrics"` || strings.Contains(w.Body.String(), "served") {
					t.Fatalf("401 answer %q, challenge %q", w.Body.String(), w.Header().Get("WWW-Authenticate"))
				}
			}
			if strings.Contains(w.Body.String(), pullSecret) {
				t.Fatal("the answer carries the token")
			}
		})
	}
	if !bearerMatches("Bearer "+pullSecret, sha256.Sum256([]byte(pullSecret))) || bearerMatches("Bearer", sha256.Sum256([]byte(""))) {
		t.Fatal("bearerMatches")
	}
}

// servePull runs tel's pull listener on a loopback port the test picks, and
// returns its address.
func servePull(t *testing.T, tel *Telemetry) string {
	t.Helper()
	bound := make(chan string, 1)
	listen = func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			bound <- ln.Addr().String()
		}
		return ln, err
	}
	t.Cleanup(func() { listen = net.Listen })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tel.ServePull(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServePull did not return after its context ended")
		}
	})
	select {
	case addr := <-bound:
		return addr
	case <-time.After(5 * time.Second):
		t.Fatal("the pull listener did not bind")
	}
	return ""
}

// instrument is the catalogue instrument named name.
func instrument(t *testing.T, name string) *telemetry.Instrument {
	t.Helper()
	for _, inst := range telemetry.Instruments() {
		if inst.Name == name {
			return inst
		}
	}
	t.Fatalf("no instrument %s", name)
	return nil
}

// recordOne records one exchange on the registry.
func recordOne(t *testing.T, tel *Telemetry) {
	reg := telemetry.New(tel.Sink())
	tel.Bind(reg)
	inst := instrument(t, "shn.gateway.exchanges")
	reg.Add(inst, 1, make([]string, len(inst.Attributes))...)
}

// MON-R6, MON-R7, MON-R42: a real listener off loopback's protection. Over
// TLS with a token and a client CA, a scrape needs both: no client
// certificate fails the handshake, a wrong token gets 401, and both get the
// metrics. The listener closes with its context.
func TestServePull_TLSTokenAndClientCertificate_MON_R7_R42(t *testing.T) {
	pki := newTestPKI(t)
	c := Load(env(pullEnv("OTEL_EXPORTER_PROMETHEUS_HOST", "0.0.0.0", "METRICS_PULL_TLS_CERT_FILE", pki.serverCert, "METRICS_PULL_TLS_KEY_FILE", pki.serverKey,
		"METRICS_PULL_TOKEN", pullSecret, "METRICS_PULL_CLIENT_CA_FILE", pki.caFile)), nil)
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	recordOne(t, tel)
	url := "https://" + servePull(t, tel) + "/metrics"
	scrape := func(withCert bool, token string) (int, string, error) {
		cfg := &tls.Config{RootCAs: pki.pool, MinVersion: tls.VersionTLS12}
		if withCert {
			cfg.Certificates = []tls.Certificate{pki.client}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		defer client.CloseIdleConnections()
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	if _, _, err := scrape(false, pullSecret); err == nil {
		t.Fatal("a scrape without a client certificate was answered")
	}
	if code, _, err := scrape(true, "not-the-token"); err != nil || code != 401 {
		t.Fatalf("wrong token: %d %v", code, err)
	}
	code, body, err := scrape(true, pullSecret)
	if err != nil || code != 200 || !strings.Contains(body, "shn_gateway_") || !strings.Contains(body, "target_info") {
		t.Fatalf("scrape: %d %v\n%s", code, err, body)
	}
	plain := &http.Client{Timeout: 5 * time.Second}
	if resp, err := plain.Get(strings.Replace(url, "https://", "http://", 1)); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "shn_gateway_") {
			t.Fatal("served over plain HTTP")
		}
	}
}

// MON-R6: a loopback bind with nothing set serves GET /metrics over plain
// HTTP, with the catalogue's names in Prometheus form.
func TestServePull_LoopbackPlain_MON_R6(t *testing.T) {
	tel := Build(Load(env(pullEnv()), nil), testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	recordOne(t, tel)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + servePull(t, tel) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "# TYPE shn_gateway_exchanges_total counter") {
		t.Fatalf("%d\n%s", resp.StatusCode, b)
	}
}

// MON-R44, MON-R42: a bind that fails (the port taken) turns pull off with a
// line naming the settings and no address, and fails telemetry-config; the
// gateway carries on.
func TestServePull_BindFailureTurnsPullOff_MON_R44(t *testing.T) {
	tel := Build(Load(env(pullEnv("OTEL_EXPORTER_PROMETHEUS_PORT", "19464")), nil), testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	var lines []string
	tel.logf = func(f string, a ...any) { lines = append(lines, f) }
	listen = func(string, string) (net.Listener, error) {
		return nil, errors.New("listen tcp localhost:19464: bind: address already in use")
	}
	t.Cleanup(func() { listen = net.Listen })
	if st := tel.ConfigCheck(); !st.OK {
		t.Fatalf("before the bind: %+v", st)
	}
	tel.ServePull(t.Context())
	if len(lines) != 1 || !strings.Contains(lines[0], "Prometheus pull is off") || strings.Contains(lines[0], "19464") {
		t.Fatalf("lines %q", lines)
	}
	st := tel.ConfigCheck()
	if st.OK || !st.Off || !containsString(st.Settings, "OTEL_EXPORTER_PROMETHEUS_PORT") || !containsString(st.Settings, "OTEL_EXPORTER_PROMETHEUS_HOST") {
		t.Fatalf("telemetry-config %+v", st)
	}
}

// MON-R44: a Prometheus exporter the SDK refuses turns pull off, and only
// pull: OTLP push stays on; telemetry-config fails and the gateway boots.
func TestBuild_PrometheusExporterRefused_MON_R44(t *testing.T) {
	pullExporterRefusal = errors.New("refused")
	t.Cleanup(func() { pullExporterRefusal = nil })
	tel := Build(Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp,prometheus"}), nil), testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	if tel.PullAddr() != "" || tel.PullHandler() != nil {
		t.Fatal("a refused exporter still serves")
	}
	if _, both := tel.Sink().(sinks); both || tel.Sink() == nil || !tel.ExportCheck().Present {
		t.Fatalf("OTLP push is not on alone: sink %T", tel.Sink())
	}
	if st := tel.ConfigCheck(); st.OK || !st.Off || !containsString(st.Settings, "OTEL_METRICS_EXPORTER") {
		t.Fatalf("telemetry-config %+v", st)
	}
}

// fakeAddrListener is a listener reporting addr as its address.
type fakeAddrListener struct {
	net.Listener
	addr net.Addr
}

func (l fakeAddrListener) Addr() net.Addr { return l.addr }

// MON-R7, MON-R42: localhost is served without protection only where it
// bound a loopback address. One that resolved elsewhere serves nothing,
// records nothing more, names the setting and fails telemetry-config.
func TestServePull_LocalhostBoundOffLoopbackServesNothing_MON_R7_R42(t *testing.T) {
	tel := Build(Load(env(pullEnv()), nil), testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	var lines []string
	tel.logf = func(f string, a ...any) { lines = append(lines, f) }
	var real net.Listener
	listen = func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		real = ln
		return fakeAddrListener{ln, &net.TCPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 9464}}, err
	}
	t.Cleanup(func() { listen = net.Listen })
	tel.ServePull(t.Context()) // returns at once: nothing served
	if _, err := real.Accept(); err == nil {
		t.Fatal("the listener is still open")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "OTEL_EXPORTER_PROMETHEUS_HOST") || strings.Contains(lines[0], "192.0.2.7") {
		t.Fatalf("lines %q", lines)
	}
	st := tel.ConfigCheck()
	if st.OK || !st.Off || !containsString(st.Settings, "OTEL_EXPORTER_PROMETHEUS_HOST") {
		t.Fatalf("telemetry-config %+v", st)
	}
	recordOne(t, tel)
	w := httptest.NewRecorder()
	tel.PullHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "shn_gateway_exchanges_total") {
		t.Fatal("recorded after the listener turned off")
	}
}

// The pull listener's scrape is cumulative and on SHN's explicit buckets
// whatever the OTLP push settings ask, and push and pull both receive every
// measurement.
func TestPull_IndependentOfPushSettings(t *testing.T) {
	c := Load(env(map[string]string{"OTEL_METRICS_EXPORTER": "otlp,prometheus",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE":        "delta",
		"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": "base2_exponential_bucket_histogram"}), nil)
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	if _, both := tel.Sink().(sinks); !both {
		t.Fatalf("sink %T, want push and pull", tel.Sink())
	}
	reg := telemetry.New(tel.Sink())
	hist := instrument(t, "shn.gateway.exchange.duration")
	for range 2 {
		reg.Record(hist, 0.2, make([]string, len(hist.Attributes))...)
	}
	// Two scrapes: a delta scrape would read 0 the second time.
	for range 2 {
		w := httptest.NewRecorder()
		tel.PullHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		body := w.Body.String()
		if !strings.Contains(body, `shn_gateway_exchange_duration_seconds_bucket{shn_direction="other",shn_exchange="other",le="0.25"} 2`) ||
			!strings.Contains(body, `shn_gateway_exchange_duration_seconds_count{shn_direction="other",shn_exchange="other"} 2`) {
			t.Fatalf("scrape is not cumulative on explicit buckets:\n%s", body)
		}
	}
}

// panicSink panics on every measurement.
type panicSink struct{}

func (panicSink) Record(telemetry.Measurement) { panic("sink failure") }

// MON-R12: with push and pull both on, a sink that panics affects neither the
// caller nor the other sink.
func TestSinks_PanicIsolated_MON_R12(t *testing.T) {
	process := captureProcessLog(t)
	rec := &recorderSink{}
	inst := instrument(t, "shn.gateway.exchanges")
	sinks{panicSink{}, rec}.Record(telemetry.Measurement{Instrument: inst, Value: 1, Attributes: make([]string, len(inst.Attributes))})
	if len(rec.got) != 1 || !strings.Contains(process.String(), "a metrics sink panicked recording shn.gateway.exchanges") {
		t.Fatalf("recorded %d after a panicking sink; log %q", len(rec.got), process.String())
	}
}
