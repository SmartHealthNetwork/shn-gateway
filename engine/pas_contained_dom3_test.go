package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The prior-authorization request a gateway authors must satisfy FHIR dom-3: a
// resource contained in another resource SHALL be referred to from elsewhere in
// the resource that contains it.
//
// The rows below are driven by the PARTICIPANT'S OWN SEEDED BYTES — the baked
// provider-tenant persona bundle the deployed provider system of record actually
// holds — rather than by a stand-in's idea of a Coverage. That distinction is the
// whole point of the test: every hermetic system-of-record stand-in in this
// package names its payer organization by an external REFERENCE, while every
// Coverage internal/fhirseed seeds CONTAINS it (fhirmap.BuildCoverageForMemberWithContainedPayer
// — the shape to use when the reader returns only the Coverage's own bytes). A
// submission built from the contained shape stranded that organization the moment
// the builder re-pointed payor at the payer Organization entry, and no hermetic
// gate could see it because no hermetic record ever had a contained payor. It
// surfaced as the deployed cloud smoke's egress $validate refusing UC03-bridge-demo:
// "The contained resource 'bridge-demo-payer-org' is not referenced to from elsewhere".

// seededProviderCoverage returns the Coverage the baked provider-tenant seed
// holds for a member — the bytes the deployed system of record answers
// OpenCoverage with, contained payer organization and all.
func seededProviderCoverage(t *testing.T, member string) []byte {
	t.Helper()
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(fhirseed.DemoProviderPersonasBundle(), &bundle); err != nil {
		t.Fatalf("read the baked provider persona bundle: %v", err)
	}
	for _, e := range bundle.Entry {
		var r struct {
			ResourceType string `json:"resourceType"`
			Identifier   []struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
		}
		if json.Unmarshal(e.Resource, &r) != nil || r.ResourceType != "Coverage" {
			continue
		}
		for _, id := range r.Identifier {
			if id.System == "urn:shn:coverage" && id.Value == member {
				return e.Resource
			}
		}
	}
	t.Fatalf("the baked provider persona bundle holds no Coverage for %s", member)
	return nil
}

// containedStrandedIn reports every contained resource an authored request
// carries that nothing in the resource containing it references — FHIR dom-3,
// read off the assembled bytes the way the pinned IG-profile validator does.
func containedStrandedIn(t *testing.T, bundleJSON []byte) []string {
	t.Helper()
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		t.Fatalf("read the authored request: %v", err)
	}
	var stranded []string
	for _, e := range bundle.Entry {
		var r struct {
			ResourceType string            `json:"resourceType"`
			ID           string            `json:"id"`
			Contained    []json.RawMessage `json:"contained"`
		}
		if json.Unmarshal(e.Resource, &r) != nil {
			continue
		}
		for _, c := range r.Contained {
			var head struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(c, &head) != nil || head.ID == "" {
				continue
			}
			// Read the container WITHOUT its contained array, so a record that
			// only refers to itself never counts as its own referrer.
			outer := e.Resource
			var m map[string]json.RawMessage
			if json.Unmarshal(e.Resource, &m) == nil {
				delete(m, "contained")
				if trimmed, err := json.Marshal(m); err == nil {
					outer = trimmed
				}
			}
			if !strings.Contains(string(outer), `"#`+head.ID+`"`) {
				stranded = append(stranded, r.ResourceType+"/"+r.ID+" contains "+head.ID)
			}
		}
	}
	return stranded
}

// payerOrgRidesAsEntry reports whether the authored request carries the payer
// organization as an entry of its own, under the id the member's record named it
// by and carrying the payer identity the exchange routed on. Dropping the
// contained copy is only correct because this is true.
func payerOrgRidesAsEntry(t *testing.T, bundleJSON []byte, orgID string, payer shnsdk.PayerIdentifier) bool {
	t.Helper()
	var bundle struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		t.Fatalf("read the authored request: %v", err)
	}
	for _, e := range bundle.Entry {
		var r struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
			Identifier   []struct {
				System string `json:"system"`
				Value  string `json:"value"`
			} `json:"identifier"`
		}
		if json.Unmarshal(e.Resource, &r) != nil || r.ResourceType != "Organization" || r.ID != orgID {
			continue
		}
		for _, id := range r.Identifier {
			if id.System == payer.System && id.Value == payer.Value {
				return true
			}
		}
	}
	return false
}

// TestAuthoredPASSubmit_SeededContainedPayerOrgIsNeverStranded is the hermetic
// twin of the deployed UC03-bridge-demo failure, on the real emission path: the
// member's own seeded Coverage, read for its payer organization through
// memberPayerOrganization (the ONE reader), then authored into a submission with
// exactly the flags the demo lane sets — and the result must satisfy dom-3.
//
// MBR-COVERED rides alongside the bridging persona deliberately. Its seeded
// Coverage contains a payer organization too; its id merely happens to be the one
// the builder's drop was hardcoded to (cms-payer), which is why the whole
// conformant roster passed the deployed smoke while the one persona with its own
// payer identity did not. A future drop that narrows back to a fixed id fails the
// bridging row; a future re-point that strands anything fails both.
func TestAuthoredPASSubmit_SeededContainedPayerOrgIsNeverStranded(t *testing.T) {
	provider, ok := newCensusSoR().ResolveByReference(OrderingProviderRef)
	if !ok {
		t.Fatal("the participant's own requesting-provider record is required to author a submission")
	}
	for _, member := range []string{"MBR-BRIDGE-DEMO", "MBR-COVERED"} {
		t.Run(member, func(t *testing.T) {
			coverage := seededProviderCoverage(t, member)
			// The payer organization, read the way every originated leg reads it:
			// out of the record that contains it, never minted.
			insurer, status, msg := (&Gateway{}).memberPayerOrganization(context.Background(), coverage)
			if status != 0 {
				t.Fatalf("memberPayerOrganization: %d %s", status, msg)
			}
			payer, parsed := shnsdk.ParsePayerIdentifier(coverage, nil)
			if !parsed {
				t.Fatalf("the seeded Coverage for %s names no payer identity", member)
			}
			order := []byte(`{"resourceType":"ServiceRequest","id":"sr-dom3","status":"active","intent":"order",` +
				`"subject":{"reference":"Patient/` + member + `"},"performer":[{"reference":"` + OrderingProviderRef + `"}],` +
				`"code":{"coding":[{"system":"http://www.ama-assn.org/go/cpt","code":"72148","display":"MRI lumbar spine w/o contrast"}]}}`)

			// The demo lane's own flags (originate.go: relaysReferencePayerBytes) —
			// the payer organization rides as a resolvable bundle entry, which is
			// the re-point that strands a contained copy.
			bundleJSON, err := buildAuthoredPASSubmit("2.2", shnsdk.ConformantClaimInputs{ItemFacts: syntheticPASItemFacts(),
				SR: order, Provider: provider, Coverage: coverage, Insurer: insurer,
				PatientRef: "Patient/" + member, CoverageRef: "Coverage/" + member,
				MemberID: member, MemberIDSystem: shnsdk.MemberSystem,
				Corr: "dom3-0001", Created: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
				ContainedInsurer: true, AbsoluteRefs: true, PayerOrgEntry: true,
				Payer: payer,
			})
			if err != nil {
				t.Fatalf("author the submission: %v", err)
			}
			if stranded := containedStrandedIn(t, bundleJSON); len(stranded) != 0 {
				t.Errorf("the authored submission strands %v — a contained resource referenced from nowhere is FHIR dom-3, which the egress validator refuses: %s",
					stranded, bundleJSON)
			}
			// The payer organization is not merely gone: the SAME record rides the
			// request as its own entry, carrying the identity the member's coverage
			// named, so nothing the participant's record asserted was dropped.
			var org struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(insurer, &org) != nil || org.ID == "" {
				t.Fatal("the payer organization read from the member's coverage has no id")
			}
			if !payerOrgRidesAsEntry(t, bundleJSON, org.ID, payer) {
				t.Errorf("the payer organization %q must ride the request as an entry carrying %s|%s: %s",
					org.ID, payer.System, payer.Value, bundleJSON)
			}
		})
	}
}
