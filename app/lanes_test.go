package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/internal/lanequalify"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestLanesDiscoveredAtDefaultAddresses(t *testing.T) {
	canonical := shnsdk.NewFakeValidator()
	t.Run("malformed overrides refuse before qualification", func(t *testing.T) {
		for _, base := range []string{":invalid", "ftp://validator/fhir", "http://"} {
			_, manager, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.dtr@2.1"}, canonical, config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL21: base}, engine.DefaultLaneURL, func(context.Context, string, string) error { return errors.New("unexpected qualification") })
			manager.Close()
			if err == nil {
				t.Fatalf("malformed override %q admitted", base)
			}
		}
	})
	t.Run("fake mode keeps distinct deterministic lines", func(t *testing.T) {
		lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "1" }, shnsdk.NativeContractVersions(), canonical, config{ConformanceEnforcement: engine.EnforcementStrict}, func(string) string { t.Fatal("fake mode resolved DNS"); return "" }, func(context.Context, string, string) error { t.Fatal("fake mode qualified"); return nil })
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		if len(m.defaults) != 0 {
			t.Fatal("fake mode created default workers")
		}
		for _, line := range []string{"2.0", "2.1", "2.2"} {
			assertLineFake(t, lanes[line], line)
		}
	})
	for _, declared := range [][]string{shnsdk.SupportedContractVersions(), {"pa.pdex@2.1"}, {"pa.crd@2.2"}} {
		for _, fail := range []bool{false, true} {
			t.Run(strings.Join(declared, ",")+map[bool]string{true: " unavailable", false: " reachable"}[fail], func(t *testing.T) {
				entered, release := make(chan struct{}, 2), make(chan struct{})
				qualifier := func(ctx context.Context, base, line string) error {
					if base != engine.DefaultLaneURL(line) {
						t.Errorf("address=%s", base)
					}
					entered <- struct{}{}
					if strings.Contains(strings.Join(declared, ","), "pa.crd@2.2") {
						if fail {
							return errors.New("wrong-line verdict")
						}
						return nil
					}
					select {
					case <-release:
						if fail {
							return errors.New("negative control passed")
						}
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				lanes, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, declared, canonical, config{ConformanceEnforcement: engine.EnforcementStrict}, engine.DefaultLaneURL, qualifier)

				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				if declared[0] == "pa.crd@2.2" {
					m.workers.Wait()
					if m.defaults["2.2"].Ready() == fail {
						t.Fatal("failed qualification admitted, or success lost")
					}
				}
				if lanes["2.0"] != canonical {
					t.Fatal("configured canonical missing")
				}
				if declared[0] != "pa.crd@2.2" {
					<-entered
					if m.defaults["2.1"].Ready() {
						t.Fatal("warming lane ready")
					}
					if lanes["2.1"] != canonical || !m.fallbacks["2.1"] {
						t.Fatal("PDex fallback lost before qualification")
					}
					close(release)
					m.workers.Wait()
					if m.defaults["2.1"].Ready() == fail {
						t.Fatal("wrong readiness")
					}
					if lanes["2.1"] != canonical {
						t.Fatal("qualification overwrote PDex fallback")
					}
				}
			})
		}
	}
	t.Run("explicit override is never qualified", func(t *testing.T) {
		var calls atomic.Int32
		_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.crd@2.2"}, canonical, config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL21: "http://explicit21/fhir", FHIRValidateURL22: "http://explicit22/fhir"}, engine.DefaultLaneURL, func(context.Context, string, string) error {
			calls.Add(1)
			return errors.New("unexpected qualification")
		})
		if err != nil {
			t.Fatal(err)
		}
		m.Close()
		if calls.Load() != 0 {
			t.Fatal("explicit endpoint probed")
		}
	})
	t.Run("close cancels and joins", func(t *testing.T) {
		entered, exited := make(chan struct{}, 2), make(chan struct{}, 2)
		_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pas@2.0"}, canonical, config{ConformanceEnforcement: engine.EnforcementStrict}, engine.DefaultLaneURL, func(ctx context.Context, base, line string) error {
			entered <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		<-entered
		m.Close()
		if len(exited) != 2 {
			t.Fatal("Close returned with a running worker")
		}
		m.Close()
	})
}

func TestDeclaredDefaultPassesTheExactFiniteCorpus(t *testing.T) {
	var rows []struct {
		Path, Profile, SHA256 string
		Outcome               json.RawMessage
	}
	raw, err := os.ReadFile("testdata/qualification-2.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != lanequalify.RowCount("2.1") {
		t.Fatal("admission transcript differs from finite corpus")
	}
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fhir/metadata" {
			fmt.Fprint(w, `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`)
			return
		}
		index := int(posts.Add(1)) - 1
		if index >= len(rows) {
			http.Error(w, "unexpected extra row", 400)
			return
		}
		row := rows[index]
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if r.Method != http.MethodPost || r.URL.Path != row.Path || r.URL.Query().Get("profile") != row.Profile || hex.EncodeToString(sum[:]) != row.SHA256 {
			http.Error(w, "unexpected corpus row", 400)
			return
		}
		_, _ = w.Write(row.Outcome)
	}))
	defer server.Close()
	_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.dtr@2.1"}, shnsdk.NewFakeValidator(), config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL22: "http://configured.invalid/fhir"}, func(string) string { return server.URL + "/fhir" }, qualifyDefaultLane)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.workers.Wait()
	if !m.defaults["2.1"].Ready() || int(posts.Load()) != len(rows) {
		t.Fatalf("ready=%v rows=%d", m.defaults["2.1"].Ready(), posts.Load())
	}
}

func TestDefaultLaneMetadataIsOnlyAStartingGate(t *testing.T) {
	for _, row := range []struct{ name, metadata, outcome string }{
		{"HTML metadata", `<html>healthy</html>`, ``},
		{"wrong FHIR service", `{"resourceType":"CapabilityStatement","fhirVersion":"5.0.0"}`, ``},
		{"metadata only", `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`, `<html>not found</html>`},
		{"invalid outcome", `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`, `{`},
		{"wrong line", `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`, `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-found","diagnostics":"Unknown profile"}]}`},
		{"negative control accidentally passes", `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`, `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Validation successful"}]}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/metadata") {
					fmt.Fprint(w, row.metadata)
				} else {
					fmt.Fprint(w, row.outcome)
				}
			}))
			defer server.Close()
			if err := qualifyDefaultLane(context.Background(), server.URL+"/fhir", "2.1"); err == nil {
				t.Fatal("incomplete qualification admitted lane")
			}
		})
	}
	t.Run("shutdown interrupts an active request", func(t *testing.T) {
		entered, exited := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- qualifyDefaultLane(ctx, server.URL, "2.2") }()
		<-entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-exited
	})
}

func TestQualificationEventsDescribeTerminalState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var out bytes.Buffer
			old := log.Writer()
			log.SetOutput(&out)
			defer log.SetOutput(old)
			_, m, err := discoverValidatorLanes(context.Background(), func(string) string { return "" }, []string{"pa.pas@2.1"}, shnsdk.NewFakeValidator(), config{ConformanceEnforcement: engine.EnforcementStrict, FHIRValidateURL22: "http://explicit.test/fhir"}, engine.DefaultLaneURL, func(context.Context, string, string) error {
				if fail {
					return errors.New("private outcome payload must not be logged")
				}
				return nil
			})
			if m != nil {
				m.workers.Wait()
				m.Close()
			}
			if err != nil {
				t.Fatalf("error=%v", err)
			}
			records := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(records) != 2 {
				t.Fatalf("qualification events=%q", out.String())
			}
			for i, record := range records {
				_, raw, ok := strings.Cut(record, "gateway: validator_qualification ")
				var event struct {
					Version  int     `json:"version"`
					Line     string  `json:"line"`
					Base     string  `json:"base"`
					State    string  `json:"state"`
					At       string  `json:"at"`
					Duration float64 `json:"duration_ms"`
				}
				if !ok || json.Unmarshal([]byte(raw), &event) != nil {
					t.Fatalf("invalid event %q", record)
				}
				want := "started"
				if i == 1 {
					want = "ready"
					if fail {
						want = "failed"
					}
				}
				if event.Version != 1 || event.Line != "2.1" || event.Base != engine.DefaultLaneURL("2.1") || event.State != want || event.Duration < 0 {
					t.Fatalf("event=%+v", event)
				}
				if _, err := time.Parse(time.RFC3339Nano, event.At); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(out.String(), "private outcome") {
				t.Fatal("failure payload leaked")
			}
		})
	}
}
