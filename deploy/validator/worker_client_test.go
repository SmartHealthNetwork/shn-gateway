package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type workerTransportFunc func(*http.Request) (*http.Response, error)

func (f workerTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The transport connects to a local capture server while recording the actual
// fixed address selected by the worker. No global client or transport is changed.
func captureWorkerTransport(t *testing.T, server *httptest.Server) *http.Transport {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:18080" {
			return nil, fmt.Errorf("unexpected dial %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func TestImageWorkerAuthorityAcrossFiniteCorpus(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			lane := newFakeLane(t)
			type capture struct {
				host, method, path, query string
				body                      []byte
				header                    http.Header
			}
			var mu sync.Mutex
			var requests []capture
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, capture{r.Host, r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header.Clone()})
				mu.Unlock()
				r.Body = io.NopCloser(bytes.NewReader(body))
				lane.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			st := initialState(sameJVM())
			st.Line = line
			path := markerPath(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runWarmupWithClient(ctx, privateBase, path, st, imageWorkerClient(captureWorkerTransport(t, server))); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			rows := readinessRows(line)
			if len(requests) != len(rows)+1 {
				t.Fatalf("requests=%d expected metadata plus %d finite rows", len(requests), len(rows))
			}
			for i, r := range requests {
				if r.host != "localhost:8080" {
					t.Errorf("request %d authority=%q, want localhost:8080", i, r.host)
				}
				if r.header.Get("Cache-Control") != "" || r.header.Get("X-Forwarded-Host") != "" {
					t.Errorf("request %d adds cache/forwarding policy", i)
				}
				if i == 0 {
					if r.method != "GET" || r.path != "/fhir/metadata" || r.query != "" || len(r.body) != 0 {
						t.Fatalf("metadata request=%+v", r)
					}
					continue
				}
				row := rows[i-1]
				body, err := fixtureBody(row)
				if err != nil {
					t.Fatal(err)
				}
				query := ""
				if row.profile != "" {
					query = "profile=" + url.QueryEscape(row.profile)
				}
				if r.method != "POST" || r.path != "/fhir/"+row.resourceType+"/$validate" || r.query != query || !bytes.Equal(body, r.body) || r.header.Get("Content-Type") != "application/fhir+json" {
					t.Fatalf("row %s request changed", row.identity)
				}
			}
			if got := readTestState(t, path); got.State != "ready" || !reflect.DeepEqual(got.Warm, readinessIdentities(line)) {
				t.Fatalf("state=%+v", got)
			}
		})
	}
}

func TestImageWorkerClonesRequestAndPreservesBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:18080/fhir/Claim/$validate?profile=http://example.test/a|2.2&x=%2f+%20", strings.NewReader("exact\x00body\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set("X-Authored", "literal")
	beforeURL := *req.URL
	beforeHeader := req.Header.Clone()
	beforeHost := req.Host
	client := imageWorkerClient(workerTransportFunc(func(got *http.Request) (*http.Response, error) {
		if got == req {
			t.Error("caller-owned request passed to transport")
		}
		if got.Host != "localhost:8080" {
			t.Errorf("Host=%q", got.Host)
		}
		if *got.URL != beforeURL || !reflect.DeepEqual(got.Header, beforeHeader) || got.Method != req.Method || got.ContentLength != req.ContentLength || got.Context() != ctx {
			t.Error("request fields or context changed")
		}
		body, _ := io.ReadAll(got.Body)
		if string(body) != "exact\x00body\n" {
			t.Errorf("body=%q", body)
		}
		got.Header.Set("X-Authored", "transport-local")
		got.URL.RawQuery = "transport-local"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.Host != beforeHost || *req.URL != beforeURL || !reflect.DeepEqual(req.Header, beforeHeader) {
		t.Fatal("caller request mutated")
	}
}

func TestImageWorkerRefusesUnexpectedOriginBeforeDispatch(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:18080/fhir/metadata", "http://127.0.0.1:8080/fhir/metadata", "https://127.0.0.1:18080/fhir/metadata", "http://foreign.example:18080/fhir/metadata", "http://user@127.0.0.1:18080/fhir/metadata"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			client := imageWorkerClient(workerTransportFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
			}))
			req, _ := http.NewRequest("GET", endpoint, nil)
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || calls != 0 {
				t.Fatalf("err=%v dispatched=%d", err, calls)
			}
		})
	}
}

func TestGenericWorkerKeepsItsAuthority(t *testing.T) {
	lane := newFakeLane(t)
	var hosts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		lane.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runWarmup(ctx, server.URL+"/fhir", markerPath(t), initialState(sameJVM())); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 43 {
		t.Fatalf("requests=%d", len(hosts))
	}
	for _, host := range hosts {
		if host != server.Listener.Addr().String() {
			t.Fatalf("generic Host=%q", host)
		}
	}
}

func TestImageWorkerTerminalFailuresDoNotReplay(t *testing.T) {
	for _, mode := range []string{"redirect", "uncertain", "incomplete", "oversized", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			var posts int
			var mu sync.Mutex
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "localhost:8080" {
					t.Errorf("Host=%q", r.Host)
				}
				if r.URL.Path == "/fhir/metadata" {
					w.Write([]byte(`{"resourceType":"CapabilityStatement"}`))
					return
				}
				io.Copy(io.Discard, r.Body)
				mu.Lock()
				posts++
				mu.Unlock()
				switch mode {
				case "redirect":
					w.Header().Set("Location", "http://127.0.0.1:18080/redirect-target")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "uncertain":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				case "incomplete":
					w.Header().Set("Content-Length", "100")
					w.Write([]byte("{}"))
				case "oversized":
					w.Write([]byte(strings.Repeat(" ", maxOutcomeBytes+1)))
				case "cancel":
					cancel()
					select {
					case <-r.Context().Done():
					case <-release:
					}
				case "deadline":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}))
			defer server.Close()
			defer close(release)
			path := markerPath(t)
			err := runWarmupWithClient(ctx, privateBase, path, initialState(sameJVM()), imageWorkerClient(captureWorkerTransport(t, server)))
			if err == nil {
				t.Fatal("failed worker admitted")
			}
			mu.Lock()
			count := posts
			mu.Unlock()
			if count != 1 {
				t.Fatalf("POST count=%d", count)
			}
			st := readTestState(t, path)
			want := map[string]string{"redirect": "redirect refused", "uncertain": "request failed", "incomplete": "response incomplete", "oversized": "response oversized", "cancel": "request failed", "deadline": "request failed"}[mode]
			if st.State != "failed" || st.Failure != want || st.Row != "init-pas-request-bundle" || len(st.Warm) != 0 {
				t.Fatalf("terminal state=%+v", st)
			}
		})
	}
}

func TestImageWorkerMetadataResponseBoundAndRedirect(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			tr := workerTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", maxMetadataBytes+1)))}
				if mode == "redirect" {
					resp.StatusCode = 307
					resp.Header.Set("Location", "http://127.0.0.1:18080/elsewhere")
					resp.Body = io.NopCloser(strings.NewReader(""))
				}
				return resp, nil
			})
			if err := metadata(context.Background(), imageWorkerClient(tr), privateBase+"/metadata"); err == nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

type workerTrackedBody struct{ closed bool }

func (*workerTrackedBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *workerTrackedBody) Close() error           { b.closed = true; return nil }

func TestImageWorkerDestinationRefusalClosesBody(t *testing.T) {
	for _, mode := range []string{"foreign", "missing-url", "opaque"} {
		t.Run(mode, func(t *testing.T) {
			body := &workerTrackedBody{}
			req, _ := http.NewRequest("POST", "http://foreign.example/fhir/Patient", body)
			if mode == "missing-url" {
				req.URL = nil
			}
			if mode == "opaque" {
				req.URL = &url.URL{Scheme: "http", Host: "127.0.0.1:18080", Opaque: "//foreign.example/fhir/Patient"}
			}
			client := imageWorkerClient(workerTransportFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("refused destination dispatched")
				return nil, nil
			}))
			if _, err := client.Transport.RoundTrip(req); err == nil {
				t.Fatal("unexpected origin accepted")
			}
			if !body.closed {
				t.Fatal("refused request body was not closed")
			}
		})
	}
}
