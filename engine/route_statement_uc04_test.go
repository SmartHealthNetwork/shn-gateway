package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The UC-04 in-flow amendment: /scenario/uc04 submits, the payer pends, and the
// same request then sends the amendment carrying the operative report. Its
// certificationType is the one the request to the route states — never a
// constant — so a request stating none is refused at 2.1/2.2 before the
// amendment leaves, and a stated one is carried on the amending Claim.

func uc04RouteFixture(t *testing.T, line string) (*Gateway, *pendResumeSubstrate) {
	t.Helper()
	opts := pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"}
	switch line {
	case "2.1":
		opts.declared = []string{shnsdk.ContractPACRD21, shnsdk.ContractPADTR21, shnsdk.ContractPAPAS21}
	case "2.2":
		opts.declared = []string{shnsdk.ContractPACRD22, shnsdk.ContractPADTR22, shnsdk.ContractPAPAS22}
	}
	gw, stub := newPendResumeFixture(t, opts)
	if line != "2.0" {
		gw.cfg.Validator = NewLineFakeValidator("2.0")
		gw.cfg.ValidatorsByLine = map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0"), line: NewLineFakeValidator(line)}
	}
	return gw, stub
}

func runUC04(gw *Gateway, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gw.handleUC04(rec, withOriginationClaimInput(httptest.NewRequest(http.MethodPost, "/scenario/uc04", strings.NewReader(body))))
	return rec
}

func TestUC04InFlowAmendment_CertificationTypeStatedOrRefused(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		t.Run(line+"/absent", func(t *testing.T) {
			gw, stub := uc04RouteFixture(t, line)
			rec := runUC04(gw, editBody(t, testRouteBody(gw, "/scenario/uc04", ""), func(m map[string]json.RawMessage) { delete(m, "certificationType") }))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "certificationType") {
				t.Fatalf("%d %s, want 422 naming certificationType", rec.Code, rec.Body.String())
			}
			// Refused before any leg: not the coverage check, not the
			// questionnaire fetch, not the submission the amendment would
			// follow.
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
			}
		})
		t.Run(line+"/stated", func(t *testing.T) {
			const stated = `{"system":"https://codesystem.x12.org/005010/1322","code":"I","display":"as stated for uc04"}`
			gw, stub := uc04RouteFixture(t, line)
			rec := runUC04(gw, editBody(t, testRouteBody(gw, "/scenario/uc04", ""), func(m map[string]json.RawMessage) { m["certificationType"] = json.RawMessage(stated) }))
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if claim := sentUpdateClaim(t, stub); !strings.Contains(string(claim["item"]), `"display":"as stated for uc04"`) {
				t.Fatalf("the stated certificationType is not carried on the amendment: %s", claim["item"])
			}
		})
	}
}

func TestUC04Route_NamesItsMemberAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(map[string]json.RawMessage)
		status int
		want   string
	}{
		{"another member", func(m map[string]json.RawMessage) { m["member"] = json.RawMessage(`"MBR-UC06"`) }, 422,
			"this route originates for MBR-UC04; the request names MBR-UC06"},
		{"facts stated for another order", func(m map[string]json.RawMessage) {
			m["order"] = json.RawMessage(`{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"J3490"}`)
		}, 422, "fidelity of SHN's own build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, stub := uc04RouteFixture(t, "2.0")
			rec := runUC04(gw, editBody(t, testRouteBody(gw, "/scenario/uc04", ""), tc.edit))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
			if len(stub.legTypes) != 0 {
				t.Fatalf("legs sent before the refusal: %v", stub.legTypes)
			}
		})
	}
}
