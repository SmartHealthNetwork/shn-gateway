package engine

import (
	"maps"
	"slices"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// TestRelayEditRegistryPinned holds the edit registry's content: ids,
// names, transmits, paths and kinds. Any change is a reviewed diff here.
func TestRelayEditRegistryPinned(t *testing.T) {
	type pin struct {
		id        relay.EditID
		name      string
		legs      []string
		role      relay.Role
		direction relay.Direction
		paths     []string
		kind      relayEditKind
	}
	crd := []string{"crd-order-dispatch", "crd-order-select"}
	want := []pin{
		{"E-01", "cds-callback-strip", crd, relay.RoleRequester, relay.DirectionRequest,
			[]string{"$.fhirServer", "$.fhirAuthorization"}, "remove-member"},
		{"E-02", "cds-prefetch-obtain", crd, relay.RoleRequester, relay.DirectionRequest,
			[]string{"$.prefetch", "$.prefetch.<key>"}, "insert-member"},
		{"E-03", "payor-edge-restamp",
			[]string{"crd-order-dispatch", "crd-order-select", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update"},
			relay.RoleRecipient, relay.DirectionRequest,
			[]string{
				"Coverage.payor[i].identifier.system",
				"Coverage.payor[i].identifier.value",
				"Organization.identifier[j].system",
				"Organization.identifier[j].value",
				"Claim.insurer (resolved the same way as Coverage.payor)",
			}, "replace-value"},
		{"E-04", "dtr-coverage-obtain", []string{"dtr-questionnaire-fetch"}, relay.RoleRequester, relay.DirectionRequest,
			[]string{"$.parameter"}, "array-append"},
	}
	if len(relayEdits) != len(want) {
		t.Fatalf("registry has %d edits, want %d", len(relayEdits), len(want))
	}
	for i, w := range want {
		e := relayEdits[i]
		got := pin{e.ID, e.Name, e.Legs, e.Role, e.Direction, e.Paths, e.Kind}
		if got.id != w.id || got.name != w.name || !slices.Equal(got.legs, w.legs) || got.role != w.role ||
			got.direction != w.direction || !slices.Equal(got.paths, w.paths) || got.kind != w.kind {
			t.Errorf("edit %d:\n got %+v\nwant %+v", i, got, w)
		}
		if e.Precondition == "" || e.Authority == "" || e.Disclosure == "" {
			t.Errorf("%s: precondition, authority and disclosure are all required", e.ID)
		}
		if byID, ok := relayEditByID(e.ID); !ok || byID.Name != e.Name {
			t.Errorf("%s: lookup failed", e.ID)
		}
	}
	if _, ok := relayEditByID("E-05"); ok {
		t.Fatal("an unregistered id resolved")
	}
	// The registry and the relay package agree on the closed id set.
	ids := make([]relay.EditID, len(relayEdits))
	for i, e := range relayEdits {
		ids[i] = e.ID
	}
	if !slices.Equal(ids, relay.EditIDs()) {
		t.Fatalf("registry ids %v, relay ids %v", ids, relay.EditIDs())
	}
	// Every declared leg is a catalog leg.
	for _, e := range relayEdits {
		for _, leg := range e.Legs {
			if _, ok := paCatalog[leg]; !ok {
				t.Errorf("%s: unknown leg %q", e.ID, leg)
			}
		}
	}
}

// TestLegOwnershipCoversCatalog: the ownership table names exactly the legs
// the catalog defines, and every catalog leg has a refusal and a requester
// request row, so no leg can transmit outside the table.
func TestLegOwnershipCoversCatalog(t *testing.T) {
	catalog := slices.Sorted(maps.Keys(paCatalog))
	if !slices.Equal(relay.Legs(), catalog) {
		t.Fatalf("table legs %v, catalog legs %v", relay.Legs(), catalog)
	}
	table := relay.LegOwnership()
	for _, leg := range catalog {
		for _, k := range []relay.Key{
			{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeRefused},
			{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: relay.OutcomeRefused},
			{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeUpstreamError},
		} {
			if _, ok := table[k]; !ok {
				t.Errorf("missing %s", k)
			}
		}
		originated := relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: relay.OutcomeOriginated}
		carried := relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
		_, o := table[originated]
		_, c := table[carried]
		if !o && !c {
			t.Errorf("%s: no requester request row", leg)
		}
		answered := relay.Key{Leg: leg, Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}
		if _, ok := table[answered]; !ok {
			t.Errorf("missing %s", answered)
		}
	}
}
