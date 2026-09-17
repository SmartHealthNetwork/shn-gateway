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
			body, sealed, err := originatedPackageRequest(line, recs, order, canonical, assertionID)
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
			if strings.Contains(string(body), `"http://x/q"`) || strings.Contains(string(body), org) {
				t.Errorf("the request carries an unversioned canonical or an included record:\n%s", body)
			}
			if sealed.Ownership() != relay.OwnershipAuthored || !bytes.Equal(relay.BytesForTest(sealed), body) {
				t.Errorf("sealed %v", sealed.Ownership())
			}
		})
	}
	t.Run("no assertion id, no context", func(t *testing.T) {
		body, _, err := originatedPackageRequest("2.2", recs, order, canonical, "")
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
