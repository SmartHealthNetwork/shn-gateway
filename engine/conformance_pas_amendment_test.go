package engine

import (
	"context"
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The pinned PAS request Bundle permits one Claim at 2.0 and up to two at
// 2.1/2.2. The second is the included prior Claim in the SDK's amendment.
// An absent declaration uses the supported structural union, not certification.
func TestPASAmendmentStructuralLineCardinality(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			body, err := shnsdk.BuildConformantClaimUpdateBundleAtLine(line, attachmentUpdateInputs(t, line, true, true))
			if err != nil {
				t.Fatal(err)
			}
			var bundle map[string]any
			if err := json.Unmarshal(body, &bundle); err != nil {
				t.Fatal(err)
			}
			entries := bundle["entry"].([]any)
			claims := 0
			for _, raw := range entries {
				if raw.(map[string]any)["resource"].(map[string]any)["resourceType"] == "Claim" {
					claims++
				}
			}
			wantClaims := 2
			if line == "2.0" {
				wantClaims = 1
			}
			if claims != wantClaims {
				t.Fatalf("actual SDK amendment Claims=%d want%d", claims, wantClaims)
			}
			for _, version := range []string{"pa.pas@" + line, ""} {
				t.Run("declaration-"+version, func(t *testing.T) {
					in := CheckInput{Body: body, Direction: "request", DeclaredVersion: version, Exchange: ExchangeContext{legType: "pas-claim-update"}}
					want := CheckValid
					if version == "pa.pas@2.0" && line != "2.0" {
						want = CheckInvalid
					}
					for _, rule := range StructuralRules() {
						if rule.ID == "pas.request.bundle" {
							got := rule.Check(context.Background(), in)
							if got.State != want {
								t.Fatalf("actual SDK %s amendment declaration=%q Claims=%d structural=%+v want%s", line, version, claims, got, want)
							}
							return
						}
					}
					t.Fatal("missing PAS request rule")
				})
			}
		})
	}
}

func TestPASRequestStructuralCardinalityGuards(t *testing.T) {
	const one = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`
	const two = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}}]}`
	const three = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}},{"resource":{"resourceType":"Claim"}}]}`
	const later = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Patient"}},{"resource":{"resourceType":"Claim"}}]}`
	for _, version := range []string{"pa.pas@2.0", "pa.pas@2.1", "pa.pas@2.2", "", "pa.pas@9.9"} {
		for _, leg := range []string{"pas-claim", "pas-claim-update", "pas-claim-inquire"} {
			t.Run(version+"/"+leg, func(t *testing.T) {
				for _, row := range []struct {
					name, body string
					valid      bool
				}{
					{"single", one, true},
					{"two", two, leg != "pas-claim-inquire" && (version == "" || version == "pa.pas@2.1" || version == "pa.pas@2.2")},
					{"third Claim", three, false},
					{"later primary", later, leg == "pas-claim-inquire" || version != "pa.pas@2.2"},
					{"empty", `{"resourceType":"Bundle","entry":[]}`, false},
					{"nonprimary", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, false},
					{"malformed entries", `{"resourceType":"Bundle","entry":{}}`, false},
					{"malformed resource", `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}},{"resource":1}]}`, false},
					{"non Bundle", `{"resourceType":"Parameters","entry":[{"resource":{"resourceType":"Claim"}}]}`, false},
				} {
					t.Run(row.name, func(t *testing.T) {
						in := CheckInput{Body: []byte(row.body), DeclaredVersion: version, Direction: "request", Exchange: ExchangeContext{legType: leg}}
						want := CheckInvalid
						if row.valid {
							want = CheckValid
						}
						if version == "pa.pas@9.9" {
							want = CheckUnavailable
						}
						for _, r := range StructuralRules() {
							if r.ID == "pas.request.bundle" {
								if got := r.Check(context.Background(), in); got.State != want {
									t.Fatalf("%+v want%s", got, want)
								}
								return
							}
						}
						t.Fatal("missing rule")
					})
				}
			})
		}
	}
}
