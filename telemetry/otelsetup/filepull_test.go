package otelsetup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"go.opentelemetry.io/contrib/otelconf"
	"go.yaml.in/yaml/v3"
)

// pullBlock is the path of the first reader's prometheus/development block.
const pullBlock = "meter_provider.readers[0].pull.exporter.prometheus/development"

// pullFile is a configuration file whose one reader is a pull reader with
// the given exporter settings (YAML lines under prometheus/development).
func pullFile(settings ...string) string {
	block := " {}"
	if len(settings) > 0 {
		block = "\n            " + strings.Join(settings, "\n            ")
	}
	return "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - pull:\n        exporter:\n          prometheus/development:" + block + "\n"
}

// kvMap is kv as a map.
func kvMap(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// The tripwire for filepull.go: otelconf's stable package decodes a pull
// reader's prometheus/development exporter to nothing, which is why the
// gateway reads that block itself. When this fails, otelconf types it: read
// the block from otelconf's type and delete the decoder and substitution.
func TestOtelconfStillDropsThePullExporter(t *testing.T) {
	cfg, err := otelconf.ParseYAML([]byte(pullFile("host: 0.0.0.0", "port: 19464", "without_scope_info: true")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeterProvider == nil || len(cfg.MeterProvider.Readers) != 1 || cfg.MeterProvider.Readers[0].Pull == nil {
		t.Fatalf("otelconf no longer reads the pull reader: %+v", cfg.MeterProvider)
	}
	if got := cfg.MeterProvider.Readers[0].Pull.Exporter; !reflect.DeepEqual(got, otelconf.PullMetricExporter{}) {
		t.Fatalf("otelconf now decodes the pull exporter (%+v): read it from otelconf's type and delete filepull.go's decoder and substitution", got)
	}
}

// The gateway's substitution is otelconf's, on the pinned version: the same
// raw value, substituted in the same file, reads the same, or both refuse
// the file. A resource attribute's value carries it through otelconf.
func TestFileSubstitutionMatchesOtelconf(t *testing.T) {
	t.Setenv("SHN_SUB_SET", "value-a")
	t.Setenv("SHN_SUB_EMPTY", "")
	t.Setenv("SHN_SUB_COLON", "a: b")
	t.Setenv("SHN_SUB_HASH", "x # y")
	t.Setenv("SHN_SUB_DOLLAR", "cost$5")
	t.Setenv("SHN_SUB_NUMBER", "19464")
	t.Setenv("SHN_SUB_LONG", "a-value-longer-than-its-reference")
	// SHN_SUB_UNSET is set nowhere.
	for _, raw := range []string{
		"${SHN_SUB_SET}",
		"${env:SHN_SUB_SET}",
		"pre-${SHN_SUB_SET}-post",
		"${SHN_SUB_SET:-fallback}",
		"${SHN_SUB_UNSET:-fallback}",
		"${SHN_SUB_EMPTY:-fallback}",
		"${env:SHN_SUB_UNSET:-fallback}",
		"${SHN_SUB_UNSET:-a$$b}",
		"${SHN_SUB_UNSET}",
		"${SHN_SUB_EMPTY}",
		"${SHN_SUB_NUMBER}",
		"$$",
		"a $$ b",
		"a $ b",
		"$${SHN_SUB_SET}",
		"$$${SHN_SUB_SET}",
		"$$$${SHN_SUB_SET}",
		"$$${SHN_SUB_LONG} tail ${SHN_SUB_SET}",
		"$${SHN_SUB_UNSET:-${SHN_SUB_SET}}",
		"${SHN_SUB_UNSET:-${SHN_SUB_SET}}",
		"${",
		"${SHN_SUB_SET",
		"${1BAD}",
		"${bad name}",
		"${env:}",
		"${env:a:b}",
		"${SHN_SUB_COLON}",
		`"${SHN_SUB_COLON}"`,
		"${SHN_SUB_HASH}",
		"${SHN_SUB_DOLLAR}",
	} {
		t.Run(raw, func(t *testing.T) {
			file := []byte("file_format: \"1.0\"\nresource:\n  attributes:\n    - name: probe\n      value: " + raw + "\n")
			theirs, theirErr := otelconf.ParseYAML(file)
			mine, myErr := substituteEnv(file, os.Getenv)
			var doc struct {
				Resource struct {
					Attributes []struct {
						Value any `yaml:"value"`
					} `yaml:"attributes"`
				} `yaml:"resource"`
			}
			if myErr == nil {
				myErr = yaml.Unmarshal(mine, &doc)
			}
			if (theirErr != nil) != (myErr != nil) {
				t.Fatalf("otelconf's error %v, the gateway's %v", theirErr, myErr)
			}
			if theirErr != nil {
				return
			}
			if len(theirs.Resource.Attributes) != 1 || len(doc.Resource.Attributes) != 1 {
				t.Fatalf("attributes: otelconf %+v, the gateway %+v", theirs.Resource.Attributes, doc.Resource.Attributes)
			}
			if a, b := fmt.Sprint(theirs.Resource.Attributes[0].Value), fmt.Sprint(doc.Resource.Attributes[0].Value); a != b {
				t.Fatalf("otelconf reads %q, the gateway %q", a, b)
			}
		})
	}
}

// The stability fence's allowance for the substitution's reads rests on
// this: the substitution reads exactly the variables the file names in its
// ${…} references, and no other, never enumerating the environment. An
// escaped reference ($${…}) names nothing and is not read.
func TestFileSubstitutionReadsOnlyTheFilesReferences(t *testing.T) {
	file := []byte(`file_format: "1.0"
resource:
  attributes:
    - name: team
      value: ${SHN_REF_TEAM}
    - name: literal
      value: $${SHN_REF_ESCAPED} and $$ alone
meter_provider:
  readers:
    - pull:
        exporter:
          prometheus/development:
            host: ${env:SHN_REF_HOST:-localhost}
            port: ${SHN_REF_PORT}
`)
	var read []string
	getenv := func(name string) string {
		read = append(read, name)
		return map[string]string{"SHN_REF_PORT": "19464", "SHN_REF_TEAM": "edge"}[name]
	}
	ex, err := filePullExporter(file, 0, getenv)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(read)
	if want := []string{"SHN_REF_HOST", "SHN_REF_PORT", "SHN_REF_TEAM"}; !slices.Equal(read, want) {
		t.Fatalf("the substitution read %v, want exactly the file's references %v", read, want)
	}
	if b := mapValue(ex, pullExporterKey); mapValue(b, "host").Value != "localhost" || mapValue(b, "port").Value != "19464" {
		t.Fatalf("the block reads %+v", b)
	}
}

// The spec's Config file criterion for pull (MON-R8): a pull reader in the
// file and the same intent in the environment give the same listener, or
// the same refusal under the file's own setting names, and the same
// warnings. Protection is METRICS_PULL_* in both modes (MON-R7).
func TestFilePull_SameAsEnvironment_MON_R8(t *testing.T) {
	pki := newTestPKI(t)
	tlsKV := []string{"METRICS_PULL_TLS_CERT_FILE", pki.serverCert, "METRICS_PULL_TLS_KEY_FILE", pki.serverKey}
	with := func(a []string, b ...string) []string { return append(append([]string(nil), a...), b...) }
	for _, row := range []struct {
		name, host, port string
		protect          []string
		on               bool
	}{
		{"the defaults", "", "", nil, true},
		{"a loopback address and port", "127.0.0.1", "19464", nil, true},
		{"IPv6 loopback", "::1", "", nil, true},
		{"loopback with a token over plain HTTP", "", "", []string{"METRICS_PULL_TOKEN", pullSecret}, true},
		{"all interfaces, TLS and token", "0.0.0.0", "", with(tlsKV, "METRICS_PULL_TOKEN", pullSecret), true},
		{"a host name, TLS and client CA", "metrics.internal", "", with(tlsKV, "METRICS_PULL_CLIENT_CA_FILE", pki.caFile), true},
		{"all interfaces, nothing set", "0.0.0.0", "", nil, false},
		{"all interfaces, a token over plain HTTP", "0.0.0.0", "", []string{"METRICS_PULL_TOKEN", pullSecret}, false},
		{"all interfaces, TLS without token or client CA", "0.0.0.0", "", tlsKV, false},
		{"a port out of range", "", "70000", nil, false},
		{"a host with a port", "localhost:9464", "", nil, false},
		{"a token with a space", "", "", []string{"METRICS_PULL_TOKEN", "pull secret-token"}, false},
		{"a client CA without TLS", "", "", []string{"METRICS_PULL_CLIENT_CA_FILE", pki.caFile}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			envKV := row.protect
			var settings []string
			if row.host != "" {
				envKV = with(envKV, "OTEL_EXPORTER_PROMETHEUS_HOST", row.host)
				settings = append(settings, fmt.Sprintf("host: %q", row.host))
			}
			if row.port != "" {
				envKV = with(envKV, "OTEL_EXPORTER_PROMETHEUS_PORT", row.port)
				settings = append(settings, "port: "+row.port)
			}
			fromEnv := Load(env(pullEnv(envKV...)), nil)
			fromFile := loadFileYAML(t, pullFile(settings...), kvMap(row.protect...))
			if (fromEnv.Pull != nil) != row.on || (fromFile.Pull != nil) != row.on {
				t.Fatalf("pull on: environment %v, file %v, want %v (problems %+v / %+v)", fromEnv.Pull != nil, fromFile.Pull != nil, row.on, fromEnv.Problems, fromFile.Problems)
			}
			if row.on {
				a, b := fromEnv.Pull, fromFile.Pull
				if a.Addr != b.Addr || a.Loopback != b.Loopback || a.Token != b.Token || a.ClientCA != b.ClientCA || (a.TLS == nil) != (b.TLS == nil) ||
					a.TLS != nil && a.TLS.ClientAuth != b.TLS.ClientAuth {
					t.Fatalf("environment %+v, file %+v", a, b)
				}
			}
			if !reflect.DeepEqual(fromEnv.Warnings, fromFile.Warnings) {
				t.Fatalf("warnings: environment %q, file %q", fromEnv.Warnings, fromFile.Warnings)
			}
			inFile := map[string]string{envPromHost: pullBlock + ".host", envPromPort: pullBlock + ".port"}
			if len(fromEnv.Problems) != len(fromFile.Problems) {
				t.Fatalf("problems: environment %+v, file %+v", fromEnv.Problems, fromFile.Problems)
			}
			for i, p := range fromEnv.Problems {
				if s, ok := inFile[p.Setting]; ok {
					p.Setting = s
				}
				if p != fromFile.Problems[i] {
					t.Fatalf("problem %d: environment %+v, file %+v", i, p, fromFile.Problems[i])
				}
			}
			var lines [2]string
			for i, c := range []Config{fromEnv, fromFile} {
				tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
				defer tel.Shutdown(context.Background())
				for _, l := range tel.Lines() {
					if strings.Contains(l, "Prometheus pull on") {
						lines[i] = l
					}
				}
			}
			if lines[0] != lines[1] {
				t.Fatalf("boot lines: environment %q, file %q", lines[0], lines[1])
			}
		})
	}
}

// MON-R7: a file's pull reader on a host other than this machine, with no
// TLS and no token, serves nothing, exactly as in environment mode: pull is
// off, the file's host setting is named with the fix, and telemetry-config
// fails.
func TestFilePull_UnprotectedNonLoopbackIsOff_MON_R7(t *testing.T) {
	c := loadFileYAML(t, pullFile(`host: "0.0.0.0"`), nil)
	if c.Pull != nil {
		t.Fatalf("pull %+v is on", c.Pull)
	}
	if len(c.Problems) != 1 || c.Problems[0].Setting != pullBlock+".host" || c.Problems[0].Piece != "Prometheus pull" ||
		!strings.Contains(c.Problems[0].Fix, "METRICS_PULL_TLS_CERT_FILE") {
		t.Fatalf("problems %+v", c.Problems)
	}
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	if tel.PullAddr() != "" || tel.PullHandler() != nil {
		t.Fatalf("pull listener %q built", tel.PullAddr())
	}
	if st := tel.ConfigCheck(); st.OK || !st.Off || !containsString(st.Settings, pullBlock+".host") {
		t.Fatalf("telemetry-config %+v", st)
	}
}

// MON-R44: what a file's pull reader asks for that the gateway does not do
// is reported under its own path, its value never quoted, and only what it
// affects is off.
func TestFilePull_ReportedAndContained_MON_R44(t *testing.T) {
	reader := func(rest string) string {
		return "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - pull:\n" + rest
	}
	for _, row := range []struct {
		name, file string
		on         bool
		setting    string // "" for no problem at all
		says       string
		value      string // a value the lines must not quote
	}{
		{"the gateway's own naming, spelled out", pullFile("without_scope_info: true", "without_target_info: false", "translation_strategy: underscore_escaping_with_suffixes", "host: null"), true, "", "", ""},
		{"another translation strategy", pullFile("translation_strategy: no_translation"), true, pullBlock + ".translation_strategy", "names are fixed", "no_translation"},
		{"scope labels", pullFile("without_scope_info: false"), true, pullBlock + ".without_scope_info", "names are fixed", ""},
		{"no target_info", pullFile("without_target_info: true"), true, pullBlock + ".without_target_info", "names are fixed", ""},
		{"a naming flag that is not a boolean", pullFile("without_scope_info: sometimes"), true, pullBlock + ".without_scope_info", "names are fixed", "sometimes"},
		{"resource constant labels", pullFile("with_resource_constant_labels:", "  included: [secret-attribute*]"), true, pullBlock + ".with_resource_constant_labels", "single value", "secret-attribute"},
		{"a key the gateway does not read", pullFile("scrape_hint: secret-hint"), true, pullBlock + ".scrape_hint", "does not read", "secret-hint"},
		{"another pull exporter beside it", reader("        exporter:\n          prometheus/development: {}\n          other/development: {}\n"), true, "meter_provider.readers[0].pull.exporter.other/development", "does not build", ""},
		{"producers", reader("        exporter:\n          prometheus/development: {}\n        producers:\n          - opencensus: {}\n"), true, "meter_provider.readers[0].pull.producers", "ignored", ""},
		{"cardinality limits", reader("        cardinality_limits:\n          default: 10\n        exporter:\n          prometheus/development: {}\n"), true, "meter_provider.readers[0].pull.cardinality_limits", "gateway's limits", ""},
		{"an exponential view", "file_format: \"1.0\"\nmeter_provider:\n  views:\n    - selector:\n        instrument_name: shn.gateway.exchange.duration\n      stream:\n        aggregation:\n          base2_exponential_bucket_histogram: {}\n  readers:\n    - pull:\n        exporter:\n          prometheus/development: {}\n", true, "meter_provider.views[0].stream.aggregation", "explicit buckets", ""},
		{"only another pull exporter", reader("        exporter:\n          other/development: {}\n"), false, "meter_provider.readers[0].pull.exporter", "Prometheus pull", ""},
		{"exporter settings that are not a mapping", reader("        exporter:\n          prometheus/development: [secret-item]\n"), false, pullBlock, "Prometheus pull", "secret-item"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := loadFileYAML(t, row.file, nil)
			if (c.Pull != nil) != row.on || !c.PullRequested {
				t.Fatalf("pull %+v requested %v, want on=%v (problems %+v)", c.Pull, c.PullRequested, row.on, c.Problems)
			}
			if row.setting == "" {
				if len(c.Problems) != 0 {
					t.Fatalf("problems %+v", c.Problems)
				}
				return
			}
			var hit *Problem
			for i := range c.Problems {
				if c.Problems[i].Setting == row.setting {
					hit = &c.Problems[i]
				}
			}
			if hit == nil || !strings.Contains(hit.Piece+hit.Effect, row.says) || hit.Fix == "" {
				t.Fatalf("no problem %s saying %q: %+v", row.setting, row.says, c.Problems)
			}
			tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
			defer tel.Shutdown(context.Background())
			lines := strings.Join(tel.Lines(), "\n")
			if !strings.Contains(lines, row.setting) || row.value != "" && strings.Contains(lines, row.value) {
				t.Fatalf("boot lines %q", lines)
			}
			if (tel.PullHandler() != nil) != row.on {
				t.Fatalf("pull handler built: %v", tel.PullHandler() != nil)
			}
		})
	}
}

// MON-R8: a file's views apply to what the pull listener serves, as to OTLP
// push; a view's exponential histogram is served on the instrument's
// explicit buckets, since the text exposition has none.
func TestFilePull_ServesTheFileViews_MON_R8(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  views:
    - selector:
        instrument_name: shn.gateway.exchanges
      stream:
        name: gw.exchanges
    - selector:
        instrument_name: shn.gateway.exchange.duration
      stream:
        aggregation:
          base2_exponential_bucket_histogram: {}
  readers:
    - pull:
        exporter:
          prometheus/development:
            host: 127.0.0.1
            port: 19464
`, nil)
	if c.Pull == nil || c.Pull.Addr != "127.0.0.1:19464" || c.Mode != ModeFile {
		t.Fatalf("pull %+v mode %s problems %+v", c.Pull, c.Mode, c.Problems)
	}
	tel := Build(c, testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	if !strings.Contains(strings.Join(tel.Lines(), "\n"), "Prometheus pull on: GET /metrics at 127.0.0.1:19464 (plain HTTP, this machine only)") {
		t.Fatalf("boot lines %q", tel.Lines())
	}
	reg := telemetry.New(tel.Sink())
	tel.Bind(reg)
	reg.Add(telemetry.Exchanges, 1, "inbound", kind, "answered")
	reg.RecordDuration(telemetry.ExchangeDuration, 1500*time.Millisecond, "inbound", kind)
	w := httptest.NewRecorder()
	tel.PullHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "\ngw_exchanges_total{") || strings.Contains(body, "shn_gateway_exchanges_total") {
		t.Fatalf("the renamed stream is not what the scrape serves (%d):\n%s", w.Code, body)
	}
	if !strings.Contains(body, "shn_gateway_exchange_duration_seconds_bucket{") {
		t.Fatalf("the duration histogram is not served on explicit buckets:\n%s", body)
	}
}

// MON-R44: a file's pull listener that cannot bind names the file's host and
// port settings, as environment mode names its own.
func TestFilePull_BindFailureNamesTheFile_MON_R44(t *testing.T) {
	tel := Build(loadFileYAML(t, pullFile("port: 19464"), nil), testIdentity, Options{Logf: func(string, ...any) {}})
	defer tel.Shutdown(context.Background())
	var lines []string
	tel.logf = func(f string, a ...any) { lines = append(lines, f) }
	listen = func(string, string) (net.Listener, error) {
		return nil, errors.New("listen tcp localhost:19464: bind: address already in use")
	}
	t.Cleanup(func() { listen = net.Listen })
	tel.ServePull(t.Context())
	if len(lines) != 1 || !strings.Contains(lines[0], pullBlock+".port") || strings.Contains(lines[0], "OTEL_EXPORTER_PROMETHEUS") {
		t.Fatalf("lines %q", lines)
	}
	st := tel.ConfigCheck()
	if st.OK || !containsString(st.Settings, pullBlock+".host") || !containsString(st.Settings, pullBlock+".port") {
		t.Fatalf("telemetry-config %+v", st)
	}
}

// The pull block's reader refuses what it cannot walk to an exporter mapping:
// loadFile reports that as the pull reader off (otelconf parsed the file, so
// it is not expected from a file that got this far).
func TestFilePullExporter_Refusals(t *testing.T) {
	getenv := func(string) string { return "" }
	for name, file := range map[string]string{
		"not YAML":                   "meter_provider: [\n",
		"no meter_provider":          "file_format: \"1.0\"\n",
		"no such reader":             "file_format: \"1.0\"\nmeter_provider:\n  readers: []\n",
		"a reader without pull":      "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - periodic: {}\n",
		"an exporter that is a list": "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - pull:\n        exporter: [prometheus/development]\n",
		"a substitution it refuses":  "file_format: \"1.0\"\nmeter_provider:\n  readers:\n    - pull:\n        exporter:\n          prometheus/development:\n            host: ${bad name}\n",
	} {
		if _, err := filePullExporter([]byte(file), 0, getenv); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// A pull reader written through YAML merge keys (<<), which otelconf reads,
// is read the same: the merged keys count, and a key the mapping writes wins.
func TestFilePull_MergeKeys_MON_R8(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
x-defaults:
  reader: &pull
    pull:
      exporter:
        prometheus/development:
          host: 127.0.0.1
          port: 19000
meter_provider:
  readers:
    - <<: *pull
`, nil)
	if c.Pull == nil || c.Pull.Addr != "127.0.0.1:19000" || len(c.Problems) != 0 {
		t.Fatalf("merged pull reader: pull %+v problems %+v", c.Pull, c.Problems)
	}
	c = loadFileYAML(t, `file_format: "1.0"
x-defaults:
  bind: &bind
    host: 127.0.0.1
    port: 19000
meter_provider:
  readers:
    - pull:
        exporter:
          prometheus/development:
            <<: *bind
            port: 19464
`, nil)
	if c.Pull == nil || c.Pull.Addr != "127.0.0.1:19464" || len(c.Problems) != 0 {
		t.Fatalf("a written key over a merged one: pull %+v problems %+v", c.Pull, c.Problems)
	}
}

// MON-R44: a mapping that merges itself, which YAML allows and otelconf passes
// in a block it does not decode, is read once: the gateway boots instead of
// recursing until the stack runs out.
func TestFilePull_SelfMergeIsReadOnce_MON_R44(t *testing.T) {
	c := loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - pull:
        exporter:
          prometheus/development: &p
            <<: *p
`, nil)
	if c.Pull == nil || c.Pull.Addr != "localhost:9464" {
		t.Fatalf("a block that merges itself: pull %+v problems %+v", c.Pull, c.Problems)
	}
	c = loadFileYAML(t, `file_format: "1.0"
meter_provider:
  readers:
    - &r
      pull:
        exporter:
          <<: *r
`, nil)
	if c.Pull != nil || !c.PullRequested || len(c.Problems) == 0 {
		t.Fatalf("an exporter that merges its own reader: pull %+v problems %+v", c.Pull, c.Problems)
	}
}
