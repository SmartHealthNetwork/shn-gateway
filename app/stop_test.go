//go:build !shn_coreonly

package app

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/auditappend"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry"
	"github.com/SmartHealthNetwork/shn-gateway/telemetry/otelsetup"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	metrics "github.com/SmartHealthNetwork/shn-sdk/metrics"
)

const deploymentPath = "../docs/DEPLOYMENT.md"

// stopSteps are the code's bounds for each row of DEPLOYMENT.md's "Stopping
// the gateway" table, by the row's step.
var stopSteps = map[string]time.Duration{
	"The listener stops taking calls and waits for the calls in flight": listenerShutdownWait,
	"The calls still running are cut, and their handlers return":        cutHandlerWait,
	"The in-memory audit outbox sends what it holds":                    auditappend.DrainLimit,
	// The engine's afterAnswerStopWait, twice, which its own
	// TestStopAfterAnswersWaitsTheDocumentedBound holds at this figure.
	"A payer's requester-side binding records under way finish, or are cut and written as they stand": time.Second,
	// The engine's observeFlushTimeout, which its own
	// TestCloseWaitsTheDocumentedFlushBound holds at this figure.
	"Queued conformance checks finish":                10 * time.Second,
	"The final metrics export":                        otelsetup.ShutdownBudget,
	"The diagnostic publisher delivers what it holds": diagnosticDrain,
	// The publisher's lastHeartbeatWait, which its own
	// TestLastHeartbeatWaitIsTheDocumentedBound holds at this figure.
	"The diagnostic publisher's last heartbeat": time.Second,
}

// stopSection is DEPLOYMENT.md's "Stopping the gateway" section.
func stopSection(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "\n## Stopping the gateway\n")
	if !ok {
		t.Fatal(`DEPLOYMENT.md has no "Stopping the gateway" section`)
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	return section
}

var boundCell = regexp.MustCompile(`^(\d+) s$`)

// TestStopBudgetIsTheDocumentedOne: each step DEPLOYMENT.md lists for a stop
// is bounded in code by the figure it states, the totals it states are their
// sums, and the stop timeout it asks for covers the longest stop.
func TestStopBudgetIsTheDocumentedOne(t *testing.T) {
	section := stopSection(t)
	var all, withoutDiagnostics time.Duration
	seen := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Step ") || strings.HasPrefix(line, "| ---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 3 {
			t.Fatalf("row %q has %d cells, want step, bound and when", line, len(cells))
		}
		step, when := cells[0], cells[2]
		m := boundCell.FindStringSubmatch(cells[1])
		if m == nil {
			t.Fatalf("row %q: bound %q is not whole seconds", line, cells[1])
		}
		secs, _ := strconv.Atoi(m[1])
		documented := time.Duration(secs) * time.Second
		code, ok := stopSteps[step]
		if !ok {
			t.Fatalf("DEPLOYMENT.md lists a stop step the code does not bound: %q", step)
		}
		if documented != code {
			t.Errorf("%q: documented %s, the code waits up to %s", step, documented, code)
		}
		seen[step] = true
		all += documented
		if !strings.Contains(when, "`DIAGNOSTIC_SINK_URL`") {
			withoutDiagnostics += documented
		}
	}
	for step := range stopSteps {
		if !seen[step] {
			t.Errorf("DEPLOYMENT.md does not list the stop step %q", step)
		}
	}
	prose := strings.Join(strings.Fields(section), " ")
	totals := regexp.MustCompile("a stop takes up to (\\d+) s, or (\\d+) s without `DIAGNOSTIC_SINK_URL`").FindStringSubmatch(prose)
	if totals == nil {
		t.Fatal("DEPLOYMENT.md no longer states the stop's totals")
	}
	if got := totals[1] + "/" + totals[2]; got != strconv.Itoa(int(all.Seconds()))+"/"+strconv.Itoa(int(withoutDiagnostics.Seconds())) {
		t.Errorf("documented totals %s s, the steps sum to %s and %s", got, all, withoutDiagnostics)
	}
	timeout := regexp.MustCompile(`a stop timeout of (\d+) s`).FindStringSubmatch(prose)
	if timeout == nil {
		t.Fatal("DEPLOYMENT.md no longer states the stop timeout to give")
	}
	if secs, _ := strconv.Atoi(timeout[1]); time.Duration(secs)*time.Second < all {
		t.Errorf("the stop timeout DEPLOYMENT.md asks for, %s s, is under the longest stop, %s", timeout[1], all)
	}
}

const drainChildEnv = "SHN_TEST_AUDIT_DRAIN_CHILD"

// TestAuditDrainKilledMidwayLeavesTheQueuedCount: a gateway killed while its
// in-memory audit outbox drains has already said how many records it held,
// in its log and, with metrics on, as AuditQueueDepth, and has counted none
// as dropped: what it lost reads as unknown, never as none.
func TestAuditDrainKilledMidwayLeavesTheQueuedCount(t *testing.T) {
	for _, emf := range []bool{true, false} {
		t.Run("metrics="+strconv.FormatBool(emf), func(t *testing.T) {
			release := make(chan struct{})
			plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
			defer plane.Close()
			defer close(release)

			cmd := exec.Command(os.Args[0], "-test.run=^TestAuditDrainChildProcess$")
			cmd.Env = append(os.Environ(), drainChildEnv+"="+plane.URL, drainChildEnv+"_EMF="+strconv.FormatBool(emf))
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string)
			go func() {
				defer close(lines)
				sc := bufio.NewScanner(out)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}()
			var got []string
			deadline := time.After(20 * time.Second)
		wait:
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatalf("the child ended before its drain began: %q", got)
					}
					got = append(got, line)
					if strings.Contains(line, "gateway: audit: stopping with ") {
						break wait
					}
				case <-deadline:
					_ = cmd.Process.Kill()
					for line := range lines {
						got = append(got, line)
					}
					_ = cmd.Wait()
					t.Fatalf("no drain line from the child: %q", got)
				}
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			for line := range lines {
				got = append(got, line)
			}
			_ = cmd.Wait()

			text := strings.Join(got, "\n")
			if !strings.Contains(text, "gateway: audit: stopping with 3 records in the in-memory outbox") {
				t.Errorf("no line names the 3 records queued: %q", got)
			}
			if strings.Contains(text, "gateway: audit: stopped:") || strings.Contains(text, "AuditAppendDropped") {
				t.Errorf("a drain killed midway left a result: %q", got)
			}
			depth := false
			for _, line := range got {
				var emfLine map[string]any
				if json.Unmarshal([]byte(line), &emfLine) == nil && emfLine["AuditQueueDepth"] == float64(3) {
					depth = true
				}
			}
			if depth != emf {
				t.Errorf("AuditQueueDepth of 3 emitted: %v, want %v (metrics on: %v): %q", depth, emf, emf, got)
			}
		})
	}
}

// TestAuditDrainChildProcess is the gateway process the test above kills: an
// in-memory outbox holding three records, wired as the app wires it, closing
// against an Audit Plane that never answers.
func TestAuditDrainChildProcess(t *testing.T) {
	endpoint := os.Getenv(drainChildEnv)
	if endpoint == "" {
		t.Skip("run by TestAuditDrainKilledMidwayLeavesTheQueuedCount")
	}
	var sinks []telemetry.Sink
	if os.Getenv(drainChildEnv+"_EMF") == "true" {
		sinks = append(sinks, telemetry.NewEMF(metrics.New(os.Stdout, "SHN/Gateway", map[string]string{"Env": "test"}, nil), "gw-test", "provider"))
	}
	m := auditMetrics{reg: telemetry.New(sinks...)}
	a, err := auditappend.New(auditappend.Config{
		Holder: "provider-a", SignPriv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)),
		Endpoint: endpoint, Store: auditappend.NewMemStore(), Metrics: m,
		Logf: log.New(os.Stdout, "", log.LstdFlags).Printf,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := a.Append(shnsdk.AuditRecord{
			Timestamp: "2026-06-04T00:00:00Z", Sender: "provider-a", Recipient: "payer-b",
			TransactionType: shnsdk.AuditTypeModuleDetails, AuthorityFrame: "provider-tpo", Scope: "pas-bundle",
			Outcome:           "authored",
			PayloadBundleHash: "2e7c9a1f4b6d8e0a3c5f7b9d1e3a5c7f9b2d4e6a8c0f1b3d5e7a9c2e4b6d8f0a",
			Version:           2, CorrelationID: "7d2e4f6a8b0c1d3e5f7a9b2c4d6e8f0a",
			Module: &shnsdk.Module{Body: "request", Leg: "pa.pas-claim", Ownership: "authored", Builder: "sdk-pas-submit", Edits: []string{}, Loss: []shnsdk.ModuleLoss{}},
		}); got != auditappend.Queued {
			t.Fatalf("Append = %s, want queued", got)
		}
	}
	a.Close()
}
