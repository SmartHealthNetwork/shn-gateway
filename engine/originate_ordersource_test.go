package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Every lane reads the member's OPEN ORDER out of the participant's own system —
// there is no lane left that builds one. An order this gateway authored was held
// in no participant's system, so the Claim/$inquire that continues a pended
// authorization (which re-reads the order first) could never resolve it.
func TestOrderSource_ReadsTheMembersOpenOrder(t *testing.T) {
	sor := newCensusSoR()
	for _, profile := range []string{"demo", "provider-data", "some-other-lane"} {
		g := &Gateway{cfg: Config{OriginationProfile: profile, SoR: sor}}
		got, status, msg := g.orderSourceContext(context.Background(), "MBR-D-UC04", "", "")
		if status != 0 {
			t.Fatalf("%s: orderSource status=%d msg=%q, want 0", profile, status, msg)
		}
		want, ok := sor.OpenOrder("MBR-D-UC04")
		if !ok {
			t.Fatalf("%s: the fixture system of record holds no open order for the member", profile)
		}
		if string(got) != string(want) {
			t.Fatalf("%s: orderSource did not return the system of record's own bytes\n got: %s\nwant: %s", profile, got, want)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("%s: unmarshal order: %v", profile, err)
		}
		// The two things that make it an order a system HOLDS rather than one
		// this gateway minted: its own identity, and the party it is requested
		// under (which is what a payer matches a later inquiry on alongside the
		// member id).
		if string(m["id"]) == "" || string(m["id"]) == `""` {
			t.Fatalf("%s: the order carries no id — nothing could re-read it later", profile)
		}
		if string(m["performer"]) != `[{"reference":"`+OrderingProviderRef+`"}]` {
			t.Fatalf("%s: order performer = %s, want the participant's own requesting provider", profile, m["performer"])
		}
	}
}

// A member whose system of record holds no open order is REFUSED. Nothing is
// authored to stand in for it.
func TestOrderSource_NoOpenOrderRefused(t *testing.T) {
	for _, profile := range []string{"demo", "provider-data"} {
		g := &Gateway{cfg: Config{OriginationProfile: profile, SoR: newCensusSoR()}}
		_, status, msg := g.orderSourceContext(context.Background(), "MBR-X", "", "")
		if status != 502 {
			t.Fatalf("%s: orderSource(no order) status=%d msg=%q, want 502", profile, status, msg)
		}
	}
}

// noCodingSoR embeds the standard stub and overrides OpenOrder to return a ServiceRequest whose
// code is in a NON-{CPT,HCPCS} system — so ParseOrderProductCoding finds no recognized product
// coding. Proves orderSource fails closed (502) rather than originating an order whose code does
// not trace to a recognized product coding (the provider-data honesty guard).
type noCodingSoR struct {
	*censusSoR
}

func (s *noCodingSoR) OpenOrder(memberID string) ([]byte, bool) {
	// A syntactically valid ServiceRequest whose only coding is SNOMED (not CPT/HCPCS).
	return []byte(`{"resourceType":"ServiceRequest","id":"sr-nocode","status":"active","intent":"order","code":{"coding":[{"system":"http://snomed.info/sct","code":"123456","display":"not a product code"}]},"subject":{"reference":"Patient/MBR-X"}}`), true
}

func TestOrderSource_OrderNoRecognizedCoding(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "provider-data", SoR: &noCodingSoR{newCensusSoR()}}}
	_, status, msg := g.orderSourceContext(context.Background(), "MBR-X", "", "")
	if status != 502 {
		t.Fatalf("orderSource(order w/ no recognized coding) status=%d msg=%q, want 502", status, msg)
	}
	if !strings.Contains(msg, "no recognized product coding") {
		t.Fatalf("orderSource msg=%q, want it to mention 'no recognized product coding'", msg)
	}
}

// The scenario states which product the origination is about; an order the
// system holds for a DIFFERENT product is refused, not originated as something
// else. This is what keeps the seeded order and the scenario's own verdict
// expectations from drifting apart silently.
func TestOrderSource_RefusesAnOrderForAnotherProduct(t *testing.T) {
	g := &Gateway{cfg: Config{OriginationProfile: "demo", SoR: newCensusSoR()}}
	c := DemoOrderCodes()
	// MBR-D-UC04's open order is the UC-04 product; ask about UC-08's.
	_, status, msg := g.orderSourceContext(context.Background(), "MBR-D-UC04", c.UC08.System, c.UC08.Code)
	if status != 502 {
		t.Fatalf("orderSource(mismatched product) status=%d msg=%q, want 502", status, msg)
	}
	if !strings.Contains(msg, c.UC08.Code) || !strings.Contains(msg, c.UC04.Code) {
		t.Fatalf("orderSource msg=%q, want it to name both the product asked about and the one on file", msg)
	}
	// …and the matching product is accepted, so the row above is about the
	// disagreement and not about the check refusing everything.
	if _, status, msg = g.orderSourceContext(context.Background(), "MBR-D-UC04", c.UC04.System, c.UC04.Code); status != 0 {
		t.Fatalf("orderSource(matching product) status=%d msg=%q, want 0", status, msg)
	}
}
