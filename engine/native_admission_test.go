package engine

import (
	"bytes"
	"context"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

type nativeAdmissionWrapper struct{ LegResponder }

func TestNativeFutureFrameConsumerBoundary(t *testing.T) {
	n := NewNativeResponder(http.DefaultClient, "https://backend.example", "", nil, nil, WithDeclaredContractVersions([]string{"pa.pas@9.9"}))
	for _, row := range []struct {
		name, leg, token string
		responder        LegResponder
		accepted         bool
	}{
		{"native PAS", "pas-claim", "pa.pas@9.9", n, true},
		{"native update", "pas-claim-update", "pa.pas@9.9", n, true},
		{"native inquire", "pas-claim-inquire", "pa.pas@9.9", n, true},
		{"native DTR", "dtr-questionnaire-fetch", "pa.dtr@9.9", n, true},
		{"native CRD select", "crd-order-select", "pa.crd@9.9", n, true},
		{"native CRD dispatch", "crd-order-dispatch", "pa.crd@9.9", n, true},
		{"custom", "pas-claim", "pa.pas@9.9", nativeAdmissionWrapper{n}, false},
		{"nil", "pas-claim", "pa.pas@9.9", nil, false},
		{"local eligibility", "coverage-eligibility", "pa.pas@9.9", n, false},
		{"local federated query", "federated-query", "pa.pas@9.9", n, false},
		{"local patient DTR", "patient-dtr", "pa.dtr@9.9", n, false},
		{"unknown leg", "unknown", "pa.pas@9.9", n, false},
		{"wrong contract", "pas-claim", "pa.dtr@9.9", n, false},
		{"empty segment", "pas-claim", "pa.pas@9..9", n, false},
		{"nonnumeric", "pas-claim", "pa.pas@future", n, false},
		{"comma", "pas-claim", "pa.pas@9.9,2.0", n, false},
		{"oversize", "pas-claim", "pa.pas@" + strings.Repeat("9", 42), n, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := &Gateway{cfg: Config{Responder: row.responder}}
			body, version, status, _ := g.unframeRequest(row.leg, framedRequest(t, row.token, []byte("exact")))
			if (status == 0) != row.accepted {
				t.Fatalf("status=%d accepted=%t", status, row.accepted)
			}
			if row.accepted && (version != row.token || string(body) != "exact") {
				t.Fatal("declaration or body changed")
			}
		})
	}
}

func TestNativeFutureAdmissionRequiresFrozenConfiguration(t *testing.T) {
	for _, row := range []struct {
		name        string
		tokens      []string
		declared    string
		mutate      bool
		wantStatus  int
		wantVersion string
	}{
		{"configured", []string{"pa.pas@9.9"}, "pa.pas@9.9", false, 0, ""},
		{"copied options", []string{"pa.pas@9.9"}, "pa.pas@9.9", true, 0, ""},
		{"missing", nil, "pa.pas@9.9", false, 422, ""},
		{"mismatch", []string{"pa.pas@2.0"}, "pa.pas@9.9", false, 422, ""},
		{"known silent", nil, "pa.pas@2.0", false, 0, ""},
		{"known silent 2.2 after list removal", nil, "pa.pas@2.2", false, 0, ""},
		{"absent declared", []string{"pa.pas@9.9"}, "", false, 0, ""},
		{"absent silent", nil, "", false, 0, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, []byte("opaque")) {
					t.Error("changed bytes")
				}
				w.WriteHeader(202)
				w.Write([]byte("answer"))
			}))
			defer server.Close()
			n := NewNativeResponder(server.Client(), server.URL, "", nil, nil, WithDeclaredContractVersions(row.tokens))
			if row.name == "missing" {
				n.SetEndpointEvidence(map[string]string{"pa.pas@9.9": server.URL + "/future"})
			}
			if row.mutate {
				row.tokens[0] = "pa.pas@2.0"
			}
			// A legacy answer line is not an actual request declaration.
			ctx := withAnswerLine(context.Background(), "pa.pas@2.0")
			if row.declared != "" {
				ctx = context.WithValue(ctx, nativeExchangeKey{}, ExchangeContext{contractVersion: row.declared})
			}
			result, err := n.Handle(ctx, "pas-claim", "corr", "pci", []byte("opaque"))
			wantCalls := 1
			if row.wantStatus != 0 {
				wantCalls = 0
			}
			if err != nil || result.Status != row.wantStatus || result.ResponseContractVersion != row.wantVersion || calls != wantCalls {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestNativeEndpointEvidenceOperationAndConfiguration(t *testing.T) {
	for _, row := range []struct {
		name, leg, path, operation string
		configured, published      []string
		declared, want             string
	}{
		{name: "supported disjoint", leg: "pas-claim", path: "/Claim/$submit", configured: []string{"pa.pas@2.0"}, published: []string{"pa.pas@2.2"}, declared: "pa.pas@2.0"},
		{name: "future disjoint", leg: "pas-claim", path: "/Claim/$submit", configured: []string{"pa.pas@9.9"}, published: []string{"pa.pas@2.0"}, declared: "pa.pas@9.9"},
		{name: "agreement", leg: "pas-claim", path: "/Claim/$submit", configured: []string{"pa.pas@2.0"}, published: []string{"pa.pas@2.0"}, declared: "pa.pas@2.0"},
		{name: "overlap stays ambiguous", leg: "pas-claim", path: "/Claim/$submit", configured: []string{"pa.pas@2.0"}, published: []string{"pa.pas@2.0", "pa.pas@2.2"}, declared: "pa.pas@2.0"},
		{name: "no configuration", leg: "pas-claim", path: "/Claim/$submit", published: []string{"pa.pas@2.2"}, declared: "pa.pas@2.0"},
		{name: "other contract configuration", leg: "pas-claim", path: "/Claim/$submit", configured: []string{"pa.dtr@2.0"}, published: []string{"pa.pas@2.2"}},
		{name: "inquiry URL equality no evidence", leg: "pas-claim-inquire", path: "/Claim/$inquire", published: []string{"pa.pas@2.0"}, declared: "pa.pas@2.0"},
		{name: "next URL equality no evidence", leg: "dtr-questionnaire-fetch", path: "/Questionnaire/$next-question", operation: shnsdk.FrameOperationNextQuestion, published: []string{"pa.dtr@2.0"}, declared: "pa.dtr@2.0"},
	} {
		for _, status := range []int{202, 409} {
			t.Run(row.name+http.StatusText(status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte("answer")) }))
				defer server.Close()
				n := NewNativeResponder(server.Client(), server.URL, "", nil, nil, WithDeclaredContractVersions(row.configured))
				evidence := map[string]string{}
				for _, tok := range row.published {
					evidence[tok] = server.URL + row.path
				}
				n.SetEndpointEvidence(evidence)
				ctx := withAnswerLine(context.Background(), row.declared)
				if row.operation != "" {
					ctx = withRequestFrameOperation(ctx, row.operation)
				}
				endpoint := n.endpointForDispatch(ctx, row.leg, server.URL, row.path)
				if endpoint.url != server.URL+row.path || endpoint.version != row.want || (endpoint.versionSource != "") != (row.want != "") {
					t.Fatalf("endpoint=%+v want=%s", endpoint, row.want)
				}
				// The other-contract row characterizes snapshot metadata, not admissible input.
				if row.name == "other contract configuration" {
					return
				}
				ctx = context.WithValue(ctx, nativeExchangeKey{}, ExchangeContext{contractVersion: row.declared})
				result, err := n.Handle(ctx, row.leg, "corr", "pci", []byte("exact"))
				if err != nil || (result.Status != 0 && result.Status != status) || result.ApplicationStatus != status || result.ResponseContractVersion != row.want {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			})
		}
	}
}

// A refresh can finish while HTTP is held. Both answers and refusals retain the
// admission and actual URL captured for that dispatch without claiming output.
func TestNativeConfiguredEndpointSnapshot(t *testing.T) {
	for _, row := range []struct {
		name               string
		initial, refreshed map[string]string
		want, next         string
	}{
		{"unique to new endpoint", map[string]string{"pa.pas@9.9": "/initial"}, map[string]string{"pa.pas@9.9": "/refreshed"}, "", ""},
		{"disjoint to agreeing", map[string]string{"pa.pas@2.0": "/Claim/$submit"}, map[string]string{"pa.pas@9.9": "/refreshed"}, "", ""},
		{"agreeing to disjoint", map[string]string{"pa.pas@9.9": "/initial"}, map[string]string{"pa.pas@2.0": "/Claim/$submit"}, "", ""},
		{"unique to ambiguous", map[string]string{"pa.pas@9.9": "/initial"}, map[string]string{"pa.pas@9.9": "/refreshed", "pa.pas@2.0": "/refreshed"}, "", ""},
	} {
		for _, status := range []int{202, 409} {
			t.Run(row.name+http.StatusText(status), func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				paths := make(chan string, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					if string(b) != "opaque future request" {
						t.Errorf("body=%s", b)
					}
					paths <- r.URL.Path
					select {
					case <-started:
					default:
						close(started)
						<-release
					}
					w.Header().Set("Content-Type", "application/participant-answer")
					w.WriteHeader(status)
					w.Write([]byte("exact reply"))
				}))
				defer server.Close()
				defer unblock()
				n := NewNativeResponder(server.Client(), server.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.pas@9.9"}))
				evidence := func(m map[string]string) map[string]string {
					out := map[string]string{}
					for k, v := range m {
						out[k] = server.URL + v
					}
					return out
				}
				n.SetEndpointEvidence(evidence(row.initial))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ctx = withAnswerLine(context.WithValue(ctx, nativeExchangeKey{}, ExchangeContext{contractVersion: "pa.pas@9.9"}), "pa.pas@9.9")
				type outcome struct {
					result LegResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					r, e := n.Handle(ctx, "pas-claim", "corr", "pci", []byte("opaque future request"))
					done <- outcome{r, e}
				}()
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("backend not reached")
				}
				refreshed := make(chan struct{})
				go func() { n.SetEndpointEvidence(evidence(row.refreshed)); close(refreshed) }()
				select {
				case <-refreshed:
				case <-ctx.Done():
					t.Fatal("refresh blocked by backend IO")
				}
				unblock()
				var got outcome
				select {
				case got = <-done:
				case <-ctx.Done():
					t.Fatal("dispatch did not complete")
				}
				verify := func(got outcome, want string) {
					t.Helper()
					key := relay.OutcomeAnswered
					if status/100 != 2 {
						key = relay.OutcomeUpstreamError
					}
					raw, err := relay.Transmit(got.result.Response, relay.Check(answerKey("pas-claim", key)))
					if got.err != nil || err != nil || got.result.ApplicationStatus != status || string(raw) != "exact reply" || got.result.Response.ContentType() != "application/participant-answer" || got.result.ResponseContractVersion != want || (got.result.ResponseVersionSource != "") != (want != "") {
						t.Fatalf("result=%+v raw=%s err=%v/%v", got.result, raw, got.err, err)
					}
				}
				verify(got, row.want)
				expected := "/Claim/$submit"
				if p := row.initial["pa.pas@9.9"]; p != "" {
					expected = p
				}
				if actual := <-paths; actual != expected {
					t.Fatalf("initial path=%s want=%s", actual, expected)
				}
				result, err := n.Handle(ctx, "pas-claim", "next", "pci", []byte("opaque future request"))
				verify(outcome{result, err}, row.next)
				expected = "/Claim/$submit"
				if p := row.refreshed["pa.pas@9.9"]; p != "" {
					expected = p
				}
				if actual := <-paths; actual != expected {
					t.Fatalf("refreshed path=%s want=%s", actual, expected)
				}
			})
		}
	}
}
