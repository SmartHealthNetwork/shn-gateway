package engine

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// A handler that sets the context hands the choke point the leg metadata a
// finding needs; a validate call made outside any handler still produces a
// finding, labelled unknown, and never suppresses one.
func TestFindingContextRoundTrips(t *testing.T) {
	fc := findingContext{LegType: "crd-order-select", CorrelationID: "corr-1", Seam: "provider-ingress", Whose: "peer"}
	got := findingContextFrom(withFindingContext(context.Background(), fc))
	if got != fc {
		t.Fatalf("finding context must round-trip: got %+v want %+v", got, fc)
	}
}

func TestFindingContextAbsentIsUnknownNeverSuppressing(t *testing.T) {
	got := findingContextFrom(context.Background())
	if got.LegType != "unknown" {
		t.Fatalf("an absent finding context must read as unknown, got %q", got.LegType)
	}
	if got.Whose != "" || got.CorrelationID != "" || got.Seam != "" {
		t.Fatalf("an absent finding context must invent nothing else: %+v", got)
	}
}

func TestFindingContextEmptyLegTypeIsUnknown(t *testing.T) {
	got := findingContextFrom(withFindingContext(context.Background(), findingContext{Whose: "own"}))
	if got.LegType != "unknown" {
		t.Fatalf("an empty legType must read as unknown, got %q", got.LegType)
	}
}

// The finding is emitted on both carriers with the same JSON, and that JSON is
// metadata only: a validator diagnostic string never appears on either one
// (§5 — diagnostics can contain whole foreign resources, and hosted-tenant
// logs are tailed across the participant boundary into SHN's console).
func TestEmitFindingBothCarriersMetadataOnly(t *testing.T) {
	const diagnostic = "Coverage.status: minimum required = 1, but only found 0 (from FOREIGN-RESOURCE-BODY)"
	var events []ObserverEvent
	g := &Gateway{cfg: Config{
		Clock:    func() time.Time { return time.Unix(0, 0).UTC() },
		Observer: func(e ObserverEvent) { events = append(events, e) },
	}}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	g.emitFinding(ConformanceFinding{
		Kind: "fhir-ingress", Direction: "ingress", LegType: "crd-order-select",
		CorrelationID: "corr-1", Seam: "provider-ingress", Whose: "peer",
		Line: "2.0", Level: "none", Decision: "relayed",
		PayloadSHA256: "abcd", Issues: []string{diagnostic}, // RAW — emitFinding must redact this itself.
	})

	if len(events) != 1 {
		t.Fatalf("want exactly one observer event, got %d", len(events))
	}
	if events[0].Kind != ConformanceObservedEvent {
		t.Fatalf("observer kind = %q, want %q", events[0].Kind, ConformanceObservedEvent)
	}
	if events[0].Direction != "validate" {
		t.Fatalf("observer direction = %q, want validate", events[0].Direction)
	}
	if events[0].CorrelationID != "corr-1" || events[0].LegType != "crd-order-select" {
		t.Fatalf("observer event must carry the leg: %+v", events[0])
	}
	if !strings.Contains(logged.String(), "conformance: ") {
		t.Fatalf("log line must be the conformance: structured line, got %q", logged.String())
	}
	wantRedacted := certificationIssueMetadata([]string{diagnostic})[0]
	for name, carrier := range map[string]string{"log": logged.String(), "observer": events[0].Detail} {
		if strings.Contains(carrier, diagnostic) || strings.Contains(carrier, "FOREIGN-RESOURCE-BODY") {
			t.Fatalf("%s carrier leaked a validator diagnostic: %s", name, carrier)
		}
		if !strings.Contains(carrier, `"decision":"relayed"`) {
			t.Fatalf("%s carrier must state the decision: %s", name, carrier)
		}
		if !strings.Contains(carrier, wantRedacted) {
			t.Fatalf("%s carrier must carry the redacted count=…/bytes=…/sha256=… form, got: %s", name, carrier)
		}
	}
}

// A gateway with no observer configured still writes the log line: the
// observer stream exists only where OBSERVER_ADDR is set, so in a hosted
// tenant the log is the only carrier.
func TestEmitFindingWithoutObserverStillLogs(t *testing.T) {
	g := &Gateway{cfg: Config{Clock: func() time.Time { return time.Unix(0, 0).UTC() }}}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g.emitFinding(ConformanceFinding{Kind: "fhir-egress", Decision: "refused", Level: "strict"})
	if !strings.Contains(logged.String(), `"kind":"fhir-egress"`) {
		t.Fatalf("log carrier must carry the finding without an observer: %q", logged.String())
	}
}

// The strict refusal body is the one place issue text may appear, and it is
// bounded exactly like the CDS certifier's list. The boundary at exactly
// findingIssuesShown (5) is the one an off-by-one guard (< vs <=) misbehaves
// on: it would wrongly append "and 0 more" to a five-item list.
func TestBoundIssues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"zero", nil, []string{}},
		{"under the bound", []string{"a", "b"}, []string{"a", "b"}},
		{"exactly at the bound", []string{"a", "b", "c", "d", "e"}, []string{"a", "b", "c", "d", "e"}},
		{"over the bound", []string{"a", "b", "c", "d", "e", "f", "g"}, []string{"a", "b", "c", "d", "e", "and 2 more"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundIssues(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("boundIssues(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("boundIssues(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
