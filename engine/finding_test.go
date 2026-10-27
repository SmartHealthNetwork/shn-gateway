package engine

import (
	"context"
	"testing"
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
