package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestDecisionEOB_GoldensAreTheProjection: the decision EOB goldens
// (testdata/eobgolden: each golden with the reference payer capture it is built
// from, copied into this module) are what this gateway's own projection writes
// for those captures, value for value. The goldens are built by a separate
// projection where they are published; this row keeps the two from drifting,
// and the source repository keeps this copy fresh.
func TestDecisionEOB_GoldensAreTheProjection(t *testing.T) {
	dir := filepath.Join("testdata", "eobgolden")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct {
		Name, CorrID, Member, Request, Answer, EOB string
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest) == 0 {
		t.Fatalf("manifest: %v (%d rows)", err, len(manifest))
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, m := range manifest {
		t.Run(m.Name, func(t *testing.T) {
			request, answer := read(m.Request), read(m.Answer)
			cr := firstClaimResponse(answer)
			if cr == nil {
				t.Fatal("the answer carries no ClaimResponse")
			}
			parsed, err := shnsdk.ParseClaimResponse(cr)
			if err != nil {
				t.Fatal(err)
			}
			// The order the request item codes, as the submit leg reads it.
			var claim struct {
				Item []struct {
					ProductOrService struct {
						Coding []struct {
							System string `json:"system"`
							Code   string `json:"code"`
						} `json:"coding"`
					} `json:"productOrService"`
				} `json:"item"`
			}
			for _, e := range bundleEntries(request) {
				var h struct {
					ResourceType string `json:"resourceType"`
				}
				if decodeMessage(e.Resource, &h) == nil && h.ResourceType == "Claim" {
					_ = decodeMessage(e.Resource, &claim)
					break
				}
			}
			if len(claim.Item) == 0 || len(claim.Item[0].ProductOrService.Coding) == 0 {
				t.Fatal("the request Claim item codes no product")
			}
			c := claim.Item[0].ProductOrService.Coding[0]
			got, err := decisionEOB(m.CorrID, "Patient/"+m.Member, c.System, c.Code, "", parsed,
				decisionEOBSource{request: request, itemIndex: -1, claimResponse: cr, answer: answer})
			if err != nil {
				t.Fatalf("the projection wrote no EOB: %v", err)
			}
			var g, w any
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(read(m.EOB), &w); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("the gateway's projection differs from the golden:\n got: %s\nwant: %s", got, read(m.EOB))
			}
		})
	}
}
