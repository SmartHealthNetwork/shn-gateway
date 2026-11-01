//go:build !shn_coreonly

package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// TestOriginatedPackageRequest_CanonicalAndContextAsStated: the questionnaire
// request the gateway originates names the questionnaire exactly as the payer
// stated it (its |version kept) and carries the payer's coverage-assertion-id
// as context, byte for byte, at every DTR line; the Coverage and the order are
// the given bytes.
func TestOriginatedPackageRequest_CanonicalAndContextAsStated(t *testing.T) {
	const canonical = "http://x/q|1.0.0"
	const assertionID = "ctx"
	cov := `{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/MBR-1"},"payor":[{"reference":"Organization/o1"}],"costToBeneficiary":[{"valueMoney":{"value":10.50}}]}`
	org := `{"resourceType":"Organization","id":"o1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}`
	recs := crdOriginRecords{
		member: "MBR-1",
		coverage: []byte(`{"resourceType":"Bundle","type":"searchset","total":1,"entry":[` +
			`{"fullUrl":"urn:uuid:a","resource":` + cov + `,"search":{"mode":"match"}},` +
			`{"fullUrl":"urn:uuid:b","resource":` + org + `,"search":{"mode":"include"}}]}`),
	}
	order := []byte(`{"resourceType":"ServiceRequest","id":"sr1","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"L8000"}]}}`)
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			body, sealed, err := originatedPackageRequest(line, recs, order, canonical, assertionID, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`{"name":"questionnaire","valueCanonical":"http://x/q|1.0.0"}`,
				`{"name":"context","valueString":"ctx"}`,
				`{"name":"coverage","resource":` + cov + `}`,
				`{"name":"order","resource":` + string(order) + `}`,
			} {
				if !bytes.Contains(body, []byte(want)) {
					t.Errorf("the request does not carry %s:\n%s", want, body)
				}
			}
			// The payer Organization the search included for the Coverage's
			// payor reference travels once, as referenced
			// (TestOriginatedPackageRequest_CarriesThePayorTheCoverageNames).
			if strings.Contains(string(body), `"http://x/q"`) || strings.Count(string(body), org) != 1 || !strings.Contains(string(body), `{"name":"referenced","resource":`+org+`}`) {
				t.Errorf("the request carries an unversioned canonical, or not the included payer Organization once as referenced:\n%s", body)
			}
			if sealed.Ownership() != relay.OwnershipAuthored || !bytes.Equal(relay.BytesForTest(sealed), body) {
				t.Errorf("sealed %v", sealed.Ownership())
			}
		})
	}
	t.Run("no assertion id, no context", func(t *testing.T) {
		body, _, err := originatedPackageRequest("2.2", recs, order, canonical, "", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(`"context"`)) {
			t.Fatalf("a context was invented:\n%s", body)
		}
	})
	// The canonical is carried as stated, not normalized.
	if shnsdk.StripCanonicalVersion(canonical) == canonical {
		t.Fatal("the row's canonical must carry a version")
	}
}

// TestOriginatedPackageRequest_CarriesThePayorTheCoverageNames: a
// questionnaire request the gateway originates carries, as a referenced
// parameter, the payer Organization each carried Coverage names by a
// reference outside itself, once, byte for byte and sealed as an embed of the
// participant's own record, at every DTR line: the one the same search result
// included, else the one the system supplied for the routing Coverage's same
// reference. A contained payer, a payer named by identifier alone, a payor
// that is not an Organization, and a reference no record answers or several
// different Organizations answer get none: nothing is minted or guessed.
func TestOriginatedPackageRequest_CarriesThePayorTheCoverageNames(t *testing.T) {
	org := `{"resourceType":"Organization","id":"o1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}],"name":"Payer \u00e9 One"}`
	otherOrg := `{"resourceType":"Organization","id":"o1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00002"}]}`
	coverage := func(id, payor, extra string) string {
		return `{"resourceType":"Coverage","id":"` + id + `","status":"active","beneficiary":{"reference":"Patient/MBR-1"},` + extra + `"payor":[` + payor + `]}`
	}
	match := func(cov string) string { return `{"resource":` + cov + `,"search":{"mode":"match"}}` }
	include := func(res string) string { return `{"resource":` + res + `,"search":{"mode":"include"}}` }
	searchset := func(entries ...string) crdOriginRecords {
		return crdOriginRecords{member: "MBR-1", coverage: []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)}
	}
	byRef := coverage("c1", `{"reference":"Organization/o1"}`, "")
	byRefToo := coverage("c2", `{"reference":"Organization/o1"}`, "")
	contained := coverage("c1", `{"reference":"#o1"}`, `"contained":[`+org+`],`)
	byIdentifier := coverage("c1", `{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}`, "")
	selfPay := coverage("c1", `{"reference":"Patient/MBR-1"}`, "")
	patient := `{"resourceType":"Patient","id":"MBR-1"}`
	order := []byte(`{"resourceType":"ServiceRequest","id":"sr1","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"L8000"}]}}`)
	referenced := `{"name":"referenced","resource":` + org + `}`
	// Two Coverages naming one payer carry it once (DTR 2.2 takes one
	// Coverage).
	for _, line := range []string{"2.0", "2.1"} {
		body, _, err := originatedPackageRequest(line, searchset(match(byRef), match(byRefToo), include(org)), order, "http://x/q", "", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n := bytes.Count(body, []byte(referenced)); n != 1 {
			t.Fatalf("%s: two Coverages naming one payer: %d referenced, want 1:\n%s", line, n, body)
		}
	}
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		for name, row := range map[string]struct {
			recs              crdOriginRecords
			routing, payerOrg string
			want              int
		}{
			"by reference, included in the search":             {searchset(match(byRef), include(org)), "", "", 1},
			"by reference, the routing Coverage's payer read":  {searchset(match(byRef)), byRef, org, 1},
			"by reference, no record answers":                  {searchset(match(byRef)), "", "", 0},
			"by reference, the routing Coverage names another": {searchset(match(byRef)), coverage("c9", `{"reference":"Organization/o9"}`, ""), org, 0},
			"by reference, two different Organizations answer": {searchset(match(byRef), include(org), include(otherOrg)), "", "", 0},
			"a contained payer":                                {searchset(match(contained)), contained, org, 0},
			"a payer named by identifier alone":                {searchset(match(byIdentifier)), byIdentifier, "", 0},
			"a payor that is not an Organization":              {searchset(match(selfPay), include(patient)), selfPay, patient, 0},
		} {
			t.Run(line+"/"+name, func(t *testing.T) {
				body, sealed, err := originatedPackageRequest(line, row.recs, order, "http://x/q", "", []byte(row.routing), []byte(row.payerOrg), nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := bytes.Count(body, []byte(`"name":"referenced"`)); got != row.want {
					t.Fatalf("%d referenced parameters, want %d:\n%s", got, row.want, body)
				}
				if row.want == 1 && !bytes.Contains(body, []byte(referenced)) {
					t.Fatalf("the referenced parameter is not the payer Organization byte for byte:\n%s", body)
				}
				if sealed.Ownership() != relay.OwnershipAuthored || !bytes.Equal(relay.BytesForTest(sealed), body) {
					t.Fatalf("sealed %v", sealed.Ownership())
				}
			})
		}
	}
}

// TestOriginatedPackageRequest_LeftOutToldOnlyWhenBuilt: what the build
// leaves out is told only once the request is built and sealed. A request the
// build refuses (DTR 2.2 takes one Coverage) tells nothing, though one of its
// Coverages names a reference no record answers; the same Coverages at 2.1
// are built and tell it.
func TestOriginatedPackageRequest_LeftOutToldOnlyWhenBuilt(t *testing.T) {
	cov := func(id, payor string) string {
		return `{"resourceType":"Coverage","id":"` + id + `","status":"active","beneficiary":{"reference":"Patient/MBR-1"},"payor":[{"reference":"` + payor + `"}]}`
	}
	recs := crdOriginRecords{member: "MBR-1", coverage: []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` +
		`{"resource":` + cov("c1", "Organization/o1") + `,"search":{"mode":"match"}},` +
		`{"resource":` + cov("c2", "Organization/o2") + `,"search":{"mode":"match"}}]}`)}
	order := []byte(`{"resourceType":"ServiceRequest","id":"sr1","status":"active","intent":"order","subject":{"reference":"Patient/MBR-1"},"code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"L8000"}]}}`)
	for line, built := range map[string]bool{"2.2": false, "2.1": true} {
		var told []string
		_, _, err := originatedPackageRequest(line, recs, order, "http://x/q", "", nil, nil, func(reason, typ string) {
			told = append(told, reason+" "+typ)
		})
		if (err == nil) != built {
			t.Fatalf("%s: built %v (err %v), want %v", line, err == nil, err, built)
		}
		want := ""
		if built {
			want = "none-answers Organization|none-answers Organization"
		}
		if got := strings.Join(told, "|"); got != want {
			t.Fatalf("%s: told %q, want %q", line, got, want)
		}
	}
}
