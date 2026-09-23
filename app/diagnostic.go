package app

import (
	"context"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"
)

type diagnosticSource struct {
	queue     *diagnostics.Queue
	publisher diagnostics.PublisherConfig
	traceKey  []byte
	budget    *diagnostics.CaptureBudget
}

// Optional capture is local to explicitly configured test deployments. Invalid
// diagnostic settings disable capture; they never disable the participant.
func newDiagnosticSource(getenv func(string) string, role string, out io.Writer, clock func() time.Time) *diagnosticSource {
	endpoint, source, keyFile := getenv("DIAGNOSTIC_SINK_URL"), getenv("DIAGNOSTIC_SOURCE"), getenv("DIAGNOSTIC_KEY_FILE")
	if endpoint == "" && source == "" && keyFile == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || source == "" || len(source) > 1024 {
		fmt.Fprintln(out, "gateway: diagnostic capture unavailable: invalid optional configuration")
		return nil
	}
	key, err := readDiagnosticKey(keyFile)
	if err != nil {
		fmt.Fprintln(out, "gateway: diagnostic capture unavailable: key unavailable")
		return nil
	}
	health := *u
	health.Path = path.Join(path.Dir(u.Path), "health")
	health.RawPath = ""
	d := &diagnosticSource{queue: diagnostics.NewQueue(diagnostics.Limits{MaxEvents: 512, MaxBytes: 64 << 20}), budget: diagnostics.NewCaptureBudget(16<<20, 8), publisher: diagnostics.PublisherConfig{Source: source, URL: u.String(), HealthURL: health.String(), Key: key, Clock: clock, Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}}
	if role == "provider" && getenv("DIAGNOSTIC_TRACE_KEY_FILE") != "" {
		d.traceKey, err = readDiagnosticKey(getenv("DIAGNOSTIC_TRACE_KEY_FILE"))
		if err != nil {
			fmt.Fprintln(out, "gateway: diagnostic trace attribution unavailable: key unavailable")
		}
	}
	return d
}
func readDiagnosticKey(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 4096 {
		return nil, fmt.Errorf("invalid key file")
	}
	key, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(key) == 0 || len(key) > 4096 {
		return nil, fmt.Errorf("invalid key file")
	}
	return key, nil
}
func (d *diagnosticSource) emit(e diagnostics.Event) bool { return d.queue.TryEmit(e) }
func (d *diagnosticSource) run(ctx context.Context) {
	_ = diagnostics.RunPublisher(ctx, d.queue, d.publisher)
}
func (d *diagnosticSource) transport(base http.RoundTripper) http.RoundTripper {
	return diagnostics.ObserveTransport(base, d.emit, d.publisher.Clock, 8<<20, d.budget)
}
func (d *diagnosticSource) smart(sc *smartauth.Config) {
	if d == nil {
		return
	}
	sc.Transport = d.transport(sc.Transport)
	sc.HTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: d.transport(nil)}
}
func (d *diagnosticSource) client(base *http.Client) *http.Client {
	if d == nil {
		return base
	}
	c := *base
	c.Transport = d.transport(c.Transport)
	return &c
}

// start owns exactly one publisher per runtime. Shutdown closes admission and
// pending ownership immediately; an uncooperative active transport may retain
// this one worker and its charged body until it returns, never a replacement.
func (d *diagnosticSource) start(parent context.Context) func() {
	if d == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); d.run(ctx) }()
	return func() {
		d.queue.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
}
