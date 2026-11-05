//go:build !shn_coreonly

package app

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// noCgroup is a read of a container with no cgroup files.
func noCgroup(string) ([]byte, error) { return nil, errors.New("no such file") }

// metadataAnswering is a task metadata endpoint giving status and body for
// /task, and what a request for anything else gets.
func metadataAnswering(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			t.Errorf("metadata read at %s, want /task", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The budget for large bodies is a tenth of the task's memory, never below the
// ceiling. Memory that cannot be read is the fallback, never a guess.
func TestAttachmentInflightDefault(t *testing.T) {
	const ceiling = shnsdk.MaxAttachmentBytes
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		}
	}
	for name, row := range map[string]struct {
		files map[string]string
		want  int64
		from  string
	}{
		"512 MiB, version 2":                    {map[string]string{cgroupMemoryMax: "536870912\n"}, 53687091, inflightFromCgroup},
		"2 GiB, version 2":                      {map[string]string{cgroupMemoryMax: "2147483648\n"}, 214748364, inflightFromCgroup},
		"512 MiB, version 1":                    {map[string]string{cgroupMemoryLimit: "536870912\n"}, 53687091, inflightFromCgroup},
		"a tenth under the ceiling":             {map[string]string{cgroupMemoryMax: "104857600\n"}, ceiling, inflightFromCgroup},
		"version 2 unlimited":                   {map[string]string{cgroupMemoryMax: "max\n"}, attachmentInflightFallback, inflightFromFallback},
		"version 2 unlimited, version 1 stated": {map[string]string{cgroupMemoryMax: "max\n", cgroupMemoryLimit: "536870912\n"}, 53687091, inflightFromCgroup},
		"version 1 unlimited":                   {map[string]string{cgroupMemoryLimit: "9223372036854771712\n"}, attachmentInflightFallback, inflightFromFallback},
		"not a number":                          {map[string]string{cgroupMemoryMax: "lots\n"}, attachmentInflightFallback, inflightFromFallback},
		"zero":                                  {map[string]string{cgroupMemoryMax: "0\n"}, attachmentInflightFallback, inflightFromFallback},
		"no cgroup":                             {nil, attachmentInflightFallback, inflightFromFallback},
	} {
		t.Run(name, func(t *testing.T) {
			got, from := attachmentInflightDefault(t.Context(), ceiling, files(row.files), "", http.DefaultClient)
			if got != row.want || from != row.from {
				t.Fatalf("budget %d from %q, want %d from %q", got, from, row.want, row.from)
			}
		})
	}
}

// With no limit in its cgroup, as on ECS Fargate, the budget comes from the
// task's metadata, and a cgroup that states one goes first. An endpoint that is
// absent, fails, hangs or states nothing usable is the fallback: never a guess,
// and never a failed boot.
func TestAttachmentInflightDefault_TaskMetadata(t *testing.T) {
	const ceiling = shnsdk.MaxAttachmentBytes
	for name, row := range map[string]struct {
		url  func(t *testing.T) string
		want int64
		from string
	}{
		"512 MiB":                   {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"CPU":0.25,"Memory":512}}`) }, 53687091, inflightFromMetadata},
		"4 GiB":                     {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"CPU":2,"Memory":4096}}`) }, 429496729, inflightFromMetadata},
		"a tenth under the ceiling": {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"Memory":100}}`) }, ceiling, inflightFromMetadata},
		"no endpoint":               {func(*testing.T) string { return "" }, attachmentInflightFallback, inflightFromFallback},
		"an error":                  {func(t *testing.T) string { return metadataAnswering(t, 500, `{"Limits":{"Memory":512}}`) }, attachmentInflightFallback, inflightFromFallback},
		"no limits, an EC2 task":    {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Family":"x"}`) }, attachmentInflightFallback, inflightFromFallback},
		"a limit of zero":           {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"Memory":0}}`) }, attachmentInflightFallback, inflightFromFallback},
		"a negative limit":          {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"Memory":-1}}`) }, attachmentInflightFallback, inflightFromFallback},
		"an absurd limit":           {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"Memory":9007199254740992}}`) }, attachmentInflightFallback, inflightFromFallback},
		"not a number":              {func(t *testing.T) string { return metadataAnswering(t, 200, `{"Limits":{"Memory":"lots"}}`) }, attachmentInflightFallback, inflightFromFallback},
		"garbage":                   {func(t *testing.T) string { return metadataAnswering(t, 200, `<html>`) }, attachmentInflightFallback, inflightFromFallback},
		"nothing listening":         {func(t *testing.T) string { return downEndpoint(t) }, attachmentInflightFallback, inflightFromFallback},
		"not a URL":                 {func(*testing.T) string { return "http://[::1" }, attachmentInflightFallback, inflightFromFallback},
	} {
		t.Run(name, func(t *testing.T) {
			got, from := attachmentInflightDefault(t.Context(), ceiling, noCgroup, row.url(t), http.DefaultClient)
			if got != row.want || from != row.from {
				t.Fatalf("budget %d from %q, want %d from %q", got, from, row.want, row.from)
			}
		})
	}
	t.Run("a cgroup that states a limit goes first", func(t *testing.T) {
		url := metadataAnswering(t, 200, `{"Limits":{"Memory":4096}}`)
		got, from := attachmentInflightDefault(t.Context(), ceiling, func(string) ([]byte, error) { return []byte("536870912"), nil }, url, http.DefaultClient)
		if got != 53687091 || from != inflightFromCgroup {
			t.Fatalf("budget %d from %q, want the cgroup's", got, from)
		}
	})
	t.Run("an endpoint that does not answer in time", func(t *testing.T) {
		previous := taskMetadataTimeout
		taskMetadataTimeout = 50 * time.Millisecond
		t.Cleanup(func() { taskMetadataTimeout = previous })
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		got, from := attachmentInflightDefault(t.Context(), ceiling, noCgroup, srv.URL, http.DefaultClient)
		if got != attachmentInflightFallback || from != inflightFromFallback {
			t.Fatalf("budget %d from %q, want the fallback", got, from)
		}
	})
}

// A build with no setting reads the task's memory, reaches the gateway with it,
// and states its source in the boot log. A setting is never overridden, and the
// endpoint is not read.
func TestBuild_TheBudgetsSourceIsInTheBootLog(t *testing.T) {
	populate := "https://populate.test/fhir/Questionnaire/$populate"
	build := func(t *testing.T, env map[string]string) (built, string) {
		t.Helper()
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		b, _, err := buildProviderForPopulate(t, env)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return b, output.String()
	}
	t.Run("metadata", func(t *testing.T) {
		b, logged := build(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": populate,
			taskMetadataEnv: metadataAnswering(t, 200, `{"Limits":{"Memory":4096}}`)})
		if strings.Contains(logged, inflightFromCgroup) {
			t.Skip("this host's cgroup states a limit, which goes first")
		}
		if b.attachmentInflight != 429496729 || !strings.Contains(logged, "attachment budget 429496729 bytes, "+inflightFromMetadata) {
			t.Fatalf("budget %d, boot log %q", b.attachmentInflight, logged)
		}
	})
	t.Run("a setting", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a setting read the task's metadata")
		}))
		t.Cleanup(srv.Close)
		b, logged := build(t, map[string]string{"PROVIDER_DTR_POPULATE_URL": populate, "ATTACHMENT_INFLIGHT_BYTES": "100000000", taskMetadataEnv: srv.URL})
		if b.attachmentInflight != 100000000 || !strings.Contains(logged, "attachment budget 100000000 bytes, "+inflightFromSetting) {
			t.Fatalf("budget %d, boot log %q", b.attachmentInflight, logged)
		}
	})
}

// ATTACHMENT_INFLIGHT_BYTES reaches the engine.Config build() hands to
// engine.New, so the budget the boot log states is the one the gateway holds.
func TestBuildWiresAttachmentInflightToGateway(t *testing.T) {
	b, _, err := buildProviderForPopulate(t, map[string]string{
		"PROVIDER_DTR_POPULATE_URL": "https://populate.test/fhir/Questionnaire/$populate", "ATTACHMENT_INFLIGHT_BYTES": "100000000"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if b.attachmentInflight != 100000000 {
		t.Fatalf("the gateway's budget is %d, want 100000000", b.attachmentInflight)
	}
}

// ATTACHMENT_INFLIGHT_BYTES overrides the default, and a value a full
// submission would not fit refuses to boot.
func TestAttachmentInflightBytes_BootRules(t *testing.T) {
	boot := func(env map[string]string) (config, error) {
		return loadConfig(func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return payerEnv(nil)[k]
		})
	}
	for name, row := range map[string]struct {
		env    map[string]string
		want   int64
		refuse bool
	}{
		"at the ceiling":        {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "16777216"}, want: 16777216},
		"over it":               {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "100000000"}, want: 100000000},
		"one under the ceiling": {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "16777215"}, refuse: true},
		"a lower ceiling":       {env: map[string]string{"ATTACHMENT_CEILING_BYTES": "1048576", "ATTACHMENT_INFLIGHT_BYTES": "1048576"}, want: 1048576},
		"zero":                  {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "0"}, refuse: true},
		"negative":              {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "-1"}, refuse: true},
		"not an integer":        {env: map[string]string{"ATTACHMENT_INFLIGHT_BYTES": "64MiB"}, refuse: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := boot(row.env)
			if row.refuse {
				if err == nil {
					t.Fatalf("booted with %d, want a refusal", cfg.AttachmentInflightBytes)
				}
				return
			}
			if err != nil || cfg.AttachmentInflightBytes != row.want {
				t.Fatalf("budget %d, %v, want %d", cfg.AttachmentInflightBytes, err, row.want)
			}
		})
	}
}
