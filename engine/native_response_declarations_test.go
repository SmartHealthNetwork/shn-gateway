package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func responseBinding(op, url, token string) string {
	return fmt.Sprintf(`{"operation":%q,"endpoint":%q,"contractVersion":%q}`, op, url, token)
}
func TestNativeResponseDeclarationsParser(t *testing.T) {
	row := responseBinding("pas-submit", "https://payer.example/Claim/$submit?x=1", "pa.pas@2.2")
	for _, raw := range []string{"", " \t\n", "[]", "[" + row + "]", strings.Repeat(" ", 32768), "[" + responseBinding("pas-inquire", "http://payer.example/"+strings.Repeat("x", 2048-len("http://payer.example/")), "pa.pas@"+strings.Repeat("1", 41)) + "]"} {
		if _, err := ParseNativeResponseDeclarations(raw); err != nil {
			t.Errorf("valid len=%d: %v", len(raw), err)
		}
	}
	bad := []string{"null", "{}", "true", "[null]", "[1]", "[{}]", "[" + row + ",]", "[" + row + "]{}", "[" + row + "," + row + "]", strings.Repeat(" ", 32769), "[" + strings.Replace(row, `"operation":`, `"extra":true,"operation":`, 1) + "]", "[" + strings.Replace(row, `"operation":`, `"operation":"pas-submit","operation":`, 1) + "]", "[" + strings.Replace(row, `"operation":"pas-submit"`, `"operation":null`, 1) + "]", "[" + strings.Replace(row, `"operation":"pas-submit"`, `"operation":4`, 1) + "]", "[" + strings.Replace(row, `"operation":"pas-submit"`, `"operation":"pas-claim"`, 1) + "]", "[" + row + string([]byte{255}) + "]"}
	for _, url := range []string{"/relative", "ftp://payer.example/x", "http:///x", "https://user:password@payer.example/x", "https://payer.example/x#fragment", "https://payer.example/x#", "https://payer.example/" + strings.Repeat("x", 2048)} {
		bad = append(bad, "["+responseBinding("pas-submit", url, "pa.pas@2.2")+"]")
	}
	for _, token := range []string{"pa.dtr@2.2", "pa.pas@", "pa.pas@2.x", "pa.pas@" + strings.Repeat("1", 42), " pa.pas@2.2"} {
		bad = append(bad, "["+responseBinding("pas-submit", "https://payer.example/x", token)+"]")
	}
	rows := []string{}
	for i := 0; i < 33; i++ {
		rows = append(rows, responseBinding("pas-submit", fmt.Sprintf("https://payer.example/%d", i), "pa.pas@9.9"))
	}
	if _, err := ParseNativeResponseDeclarations("[" + strings.Join(rows[:32], ",") + "]"); err != nil {
		t.Fatal(err)
	}
	bad = append(bad, "["+strings.Join(rows, ",")+"]")

	assertRejected := func(t *testing.T, raw string) {
		t.Helper()
		value, err := ParseNativeResponseDeclarations(raw)
		if err == nil {
			t.Fatal("invalid configuration accepted")
		} else if len(err.Error()) > 160 || strings.Contains(err.Error(), "payer.example") || strings.Contains(err.Error(), "password") {
			t.Errorf("unsafe error %v", err)
		}
		if len(value.bindings) != 0 {
			t.Error("partial application")
		}
	}
	for i, raw := range bad {
		t.Run(fmt.Sprintf("invalid/%d", i), func(t *testing.T) { assertRejected(t, raw) })
	}
	for _, key := range []string{"operation", "endpoint", "contractVersion"} {
		for _, mutation := range []struct{ name, value string }{
			{"null", "null"}, {"boolean", "false"}, {"number", "4"}, {"array", "[]"}, {"object", "{}"}, {"missing", ""},
		} {
			t.Run(key+"/"+mutation.name, func(t *testing.T) {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(row), &fields); err != nil {
					t.Fatal(err)
				}
				if mutation.name == "missing" {
					delete(fields, key)
				} else {
					fields[key] = json.RawMessage(mutation.value)
				}
				raw, err := json.Marshal([]map[string]json.RawMessage{fields})
				if err != nil {
					t.Fatal(err)
				}
				assertRejected(t, string(raw))
			})
		}
		t.Run(key+"/duplicate", func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(row), &fields); err != nil {
				t.Fatal(err)
			}
			assertRejected(t, "["+strings.TrimSuffix(row, "}")+`,"`+key+`":`+string(fields[key])+"}]")
		})
	}
}

func TestNativeResponseDeclarationsSnapshot(t *testing.T) {
	rows := []struct{ op, leg, path, contract string }{{"pas-submit", "pas-claim", "/Claim/$submit", "pa.pas"}, {"pas-update-submit", "pas-claim-update", "/Claim/$submit", "pa.pas"}, {"pas-inquire", "pas-claim-inquire", "/Claim/$inquire", "pa.pas"}, {"questionnaire-package", "dtr-questionnaire-fetch", "/Questionnaire/$questionnaire-package", "pa.dtr"}, {"next-question", "dtr-questionnaire-fetch", "/Questionnaire/$next-question", "pa.dtr"}, {"crd-order-select", "crd-order-select", "/cds-services/select", "pa.crd"}, {"crd-order-dispatch", "crd-order-dispatch", "/cds-services/dispatch", "pa.crd"}}
	for _, r := range rows {
		t.Run(r.op, func(t *testing.T) {
			base := "https://payer.example"
			d, err := ParseNativeResponseDeclarations("[" + responseBinding(r.op, base+r.path, r.contract+"@2.2") + "]")
			if err != nil {
				t.Fatal(err)
			}
			n := NewNativeResponder(nil, base, "", nil, nil, WithDeclaredContractVersions([]string{r.contract + "@2.0"}), WithNativeResponseDeclarations(d))
			ctx := context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{contractVersion: r.contract + "@2.0"})
			got := n.endpointForDispatch(ctx, r.leg, base, r.path)
			if !got.admitted || got.version != r.contract+"@2.2" || got.versionSource != "configured-endpoint" {
				t.Fatalf("snapshot=%+v", got)
			}
			missing := n.endpointForDispatch(ctx, r.leg, base, r.path+"?different=1")
			if missing.version != "" || missing.versionSource != "" || !missing.admitted {
				t.Fatalf("unmatched=%+v", missing)
			}
			denied := n.endpointForDispatch(context.WithValue(ctx, nativeExchangeKey{}, ExchangeContext{contractVersion: r.contract + "@2.2"}), r.leg, base, r.path)
			if denied.admitted {
				t.Fatal("output admitted input")
			}
			for _, other := range rows {
				if other.op != r.op && other.contract == r.contract {
					control := n.endpointForDispatch(ctx, other.leg, base, other.path)
					if control.version != "" || control.versionSource != "" {
						t.Fatalf("other %s=%+v", other.op, control)
					}
				}
			}
		})
	}
}

func TestNativeResponseDeclarationsHeldDispatch(t *testing.T) {
	for _, row := range []struct {
		name               string
		initial, refreshed map[string]string
		want, next         string
		output             map[string]string
	}{
		{"bound refresh", map[string]string{"pa.pas@9.9": "/initial"}, map[string]string{"pa.pas@9.9": "/refreshed"}, "pa.pas@2.2", "pa.pas@9.8", map[string]string{"/initial": "pa.pas@2.2", "/refreshed": "pa.pas@9.8"}},
		{"unbound refresh", map[string]string{"pa.pas@9.9": "/initial"}, map[string]string{"pa.pas@9.9": "/refreshed"}, "pa.pas@2.2", "", map[string]string{"/initial": "pa.pas@2.2"}},
		{"unbound before dispatch", map[string]string{"pa.pas@9.9": "/refreshed"}, map[string]string{"pa.pas@9.9": "/initial"}, "", "pa.pas@2.2", map[string]string{"/initial": "pa.pas@2.2"}},
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
				rows := []string{}
				for path, token := range row.output {
					rows = append(rows, responseBinding("pas-submit", server.URL+path, token))
				}
				declarations, err := ParseNativeResponseDeclarations("[" + strings.Join(rows, ",") + "]")
				if err != nil {
					t.Fatal(err)
				}
				n := NewNativeResponder(server.Client(), server.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.pas@9.9"}), WithNativeResponseDeclarations(declarations))
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
					if got.err != nil || err != nil || got.result.ApplicationStatus != status || string(raw) != "exact reply" || got.result.Response.ContentType() != "application/participant-answer" || got.result.ResponseContractVersion != want || (got.result.ResponseVersionSource != "configured-endpoint") != (want == "") {
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

func TestNativeResponseDeclarationsActualRoutes(t *testing.T) {
	for _, r := range []struct{ op, leg, path, frame, hook, contract string }{
		{"pas-submit", "pas-claim", "/Claim/$submit", "", "", "pa.pas"},
		{"pas-update-submit", "pas-claim-update", "/Claim/$submit", "", "", "pa.pas"},
		{"pas-inquire", "pas-claim-inquire", "/Claim/$inquire", "", "", "pa.pas"},
		{"questionnaire-package", "dtr-questionnaire-fetch", "/Questionnaire/$questionnaire-package", "questionnaire-package", "", "pa.dtr"},
		{"next-question", "dtr-questionnaire-fetch", "/Questionnaire/$next-question", "next-question", "", "pa.dtr"},
		{"crd-order-select", "crd-order-select", "/cds-services/select", "", "order-select", "pa.crd"},
		{"crd-order-dispatch", "crd-order-dispatch", "/cds-services/dispatch", "", "order-dispatch", "pa.crd"},
	} {
		for _, status := range []int{202, 409} {
			t.Run(r.op+fmt.Sprint(status), func(t *testing.T) {
				request := []byte("opaque request")
				if r.hook != "" {
					request = cdsRequest(r.hook)
				}
				hits := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method == "GET" {
						io.WriteString(w, `{"services":[{"id":"select","hook":"order-select"},{"id":"dispatch","hook":"order-dispatch"}]}`)
						return
					}
					hits++
					body, _ := io.ReadAll(req.Body)
					if req.URL.RequestURI() != r.path || string(body) != string(request) {
						t.Errorf("path=%s body=%s", req.URL.RequestURI(), body)
					}
					w.Header().Set("Content-Type", "application/participant-answer")
					w.WriteHeader(status)
					io.WriteString(w, " exact response\n")
				}))
				defer srv.Close()
				d, err := ParseNativeResponseDeclarations("[" + responseBinding(r.op, srv.URL+r.path, r.contract+"@2.2") + "]")
				if err != nil {
					t.Fatal(err)
				}
				n := NewNativeResponder(srv.Client(), srv.URL, "", nil, nil, WithDeclaredContractVersions([]string{r.contract + "@2.0"}), WithNativeResponseDeclarations(d))
				ctx := withAnswerLine(context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{contractVersion: r.contract + "@2.0"}), r.contract+"@2.0")
				if r.frame != "" {
					ctx = withRequestFrameOperation(ctx, r.frame)
				}
				result, err := n.Handle(ctx, r.leg, "corr", "pci", request)
				if err != nil || result.ResponseContractVersion != r.contract+"@2.2" || result.ResponseVersionSource != "configured-endpoint" || result.ApplicationStatus != status || hits != 1 {
					t.Fatalf("result=%+v hits=%d err=%v", result, hits, err)
				}
				outcome := relay.OutcomeAnswered
				if status == 409 {
					outcome = relay.OutcomeUpstreamError
				}
				raw, err := relay.Transmit(result.Response, relay.Check(answerKey(r.leg, outcome)))
				if err != nil || string(raw) != " exact response\n" || result.Response.ContentType() != "application/participant-answer" {
					t.Fatalf("response=%s err=%v", raw, err)
				}
			})
		}
	}
}

func TestNativeResponseDeclarationsCannotAdmitInput(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(202) }))
	defer srv.Close()
	raw := "[" + responseBinding("pas-submit", srv.URL+"/Claim/$submit", "pa.pas@2.2") + "]"
	d, err := ParseNativeResponseDeclarations(raw)
	if err != nil {
		t.Fatal(err)
	}
	n := NewNativeResponder(srv.Client(), srv.URL, "", nil, nil, WithDeclaredContractVersions([]string{"pa.pas@2.0"}), WithNativeResponseDeclarations(d))
	// Reassigning caller values cannot mutate the adapter's captured parsed value.
	d = NativeResponseDeclarations{}
	raw = "[]"
	ctx := context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{contractVersion: "pa.pas@2.2"})
	got, err := n.Handle(ctx, "pas-claim", "corr", "pci", []byte("unaccepted request"))
	if err != nil || got.Status != 422 || hits != 0 {
		t.Fatalf("result=%+v hits=%d err=%v", got, hits, err)
	}
	ctx = context.WithValue(ctx, nativeExchangeKey{}, ExchangeContext{contractVersion: "pa.pas@2.0"})
	got, err = n.Handle(ctx, "pas-claim", "corr2", "pci", []byte("accepted request"))
	if err != nil || got.ResponseContractVersion != "pa.pas@2.2" || hits != 1 {
		t.Fatalf("mutated configuration: %+v %v", got, err)
	}
	_ = d
	_ = raw
}

func TestNativeResponseDeclarationsSameDTRURLIsolation(t *testing.T) {
	for _, op := range []string{"questionnaire-package", "next-question"} {
		t.Run(op, func(t *testing.T) {
			base := "https://payer.example"
			selected := base + "/Questionnaire/$next-question"
			d, err := ParseNativeResponseDeclarations("[" + responseBinding(op, selected, "pa.dtr@2.2") + "]")
			if err != nil {
				t.Fatal(err)
			}
			n := NewNativeResponder(nil, base, "", nil, nil, WithDeclaredContractVersions([]string{"pa.dtr@2.0"}), WithNativeResponseDeclarations(d))
			// Existing package endpoint evidence selects the same URL the adaptive route uses.
			n.SetEndpointEvidence(map[string]string{"pa.dtr@2.0": selected})
			ctx := withAnswerLine(context.WithValue(context.Background(), nativeExchangeKey{}, ExchangeContext{contractVersion: "pa.dtr@2.0"}), "pa.dtr@2.0")
			for _, path := range []string{"/Questionnaire/$questionnaire-package", "/Questionnaire/$next-question"} {
				got := n.endpointForDispatch(ctx, "dtr-questionnaire-fetch", base, path)
				want, source := "", ""
				if nativeResponseOperation("dtr-questionnaire-fetch", path) == op {
					want, source = "pa.dtr@2.2", "configured-endpoint"
				}
				if got.url != selected || got.version != want || got.versionSource != source {
					t.Fatalf("path=%s snapshot=%+v", path, got)
				}
			}
		})
	}
}
