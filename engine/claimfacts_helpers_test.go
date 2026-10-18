package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The test participant's own statement of its request facts — the codes of the
// PAS IG's own example (Claim-MedicalServicesAuthorizationExample, PAS 2.0.1,
// 2.1.0 and 2.2.1): a professional claim, process priority "normal", X12 1365
// "1" Medical Care, CMS place of service "11" Office and X12 1525 "IN"; an
// amendment states X12 1322 "I" Initial. A test drives an origination route
// with them in its request body, as a requester would.
const (
	testClaimTypeCoding     = `{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}`
	testPriorityCoding      = `{"system":"http://terminology.hl7.org/CodeSystem/processpriority","code":"normal"}`
	testCategoryCoding      = `{"system":"https://codesystem.x12.org/005010/1365","code":"1","display":"Medical Care"}`
	testPlaceCoding         = `{"system":"https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set","code":"11"}`
	testRequestTypeCoding   = `{"system":"https://codesystem.x12.org/005010/1525","code":"IN","display":"Initial Medical Services Reservation"}`
	testCertificationCoding = `{"system":"https://codesystem.x12.org/005010/1322","code":"I","display":"Initial"}`
)

func testClaimFacts() shnsdk.PASClaimFacts {
	return shnsdk.PASClaimFacts{
		ClaimType:       json.RawMessage(testClaimTypeCoding),
		Priority:        json.RawMessage(testPriorityCoding),
		ServiceCategory: json.RawMessage(testCategoryCoding),
		PlaceOfService:  json.RawMessage(testPlaceCoding),
		RequestType:     json.RawMessage(testRequestTypeCoding),
	}
}

// testPriorClaim is a recorded submission as the update builder takes it: a
// submission whose one item (sequence 1) is recorded as its order line with
// no more detail, so an amendment's item 1 differs from it and is marked
// infoChanged "changed". A row about the derivation itself records a real
// submission (recordPASSubmission).
func testPriorClaim() shnsdk.PASPriorClaim {
	return shnsdk.PASPriorClaim{
		ClaimType: json.RawMessage(`{"coding":[` + testClaimTypeCoding + `]}`),
		Priority:  json.RawMessage(`{"coding":[` + testPriorityCoding + `]}`),
		Created:   "2026-06-01T12:00:00Z",
		Items:     []json.RawMessage{json.RawMessage(`{"sequence":1}`)},
	}
}

func testCertificationType() json.RawMessage { return json.RawMessage(testCertificationCoding) }

// testClaimInputJSON is the origination request body members stating the
// test participant's claim facts and amendment certificationType.
const testClaimInputJSON = `"claimFacts":{"claimType":` + testClaimTypeCoding + `,"priority":` + testPriorityCoding +
	`,"serviceCategory":` + testCategoryCoding + `,"placeOfService":` + testPlaceCoding + `,"requestType":` + testRequestTypeCoding +
	`},"certificationType":` + testCertificationCoding

// testClaimBody is an origination request body stating the test claim input,
// beside the route's own members (extra, a JSON member list or "").
func testClaimBody(extra string) string {
	if extra == "" {
		return "{" + testClaimInputJSON + "}"
	}
	return "{" + testClaimInputJSON + "," + extra + "}"
}

// testRouteBody is testClaimBody naming what a driver names: the member the
// route at path resolves on g's lane (and ?personaSet), and the order that
// member holds, read from g's system of record the way a driver reads it from
// the persona's descriptor. A dispatch route names the member extra states. A
// member with no open order (UC-01) states no order.
func testRouteBody(g *Gateway, path, extra string) string {
	u, err := url.Parse(path)
	if err != nil {
		panic(err)
	}
	seg := strings.TrimPrefix(u.Path, "/scenario/")
	seg = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(seg, "/start"), "/complete"), "/cancel")
	var stated struct {
		Branch string `json:"branch"`
		Member string `json:"member"`
	}
	_ = json.Unmarshal([]byte("{"+extra+"}"), &stated)
	member := stated.Member
	if seg != "dispatch" {
		scenario := seg
		switch {
		case seg == "uc01" && stated.Branch == "notcovered":
			scenario = "uc01-notcovered"
		case seg == "uc05" && stated.Branch == "noconsent":
			scenario = "uc05-noconsent"
		case seg == "uc03" && strings.HasPrefix(stated.Branch, "bridge-"):
			scenario = "uc03-" + stated.Branch
		}
		m, ok := scenarioMembers[scenario]
		if !ok {
			panic("testRouteBody: unknown scenario " + scenario)
		}
		member = g.sceneMember(m.conformance, m.providerData, m.demo)
		if u.Query().Get("personaSet") == "canary" {
			if twin, ok := CanaryTwins[member]; ok {
				member = twin
			}
		}
	}
	return testStatedBody(g, member, extra, stated.Member != "")
}

// testStatedBody is testClaimBody naming member and the order member holds in
// g's system of record. extraNamesMember: extra carries its own "member".
func testStatedBody(g *Gateway, member, extra string, extraNamesMember bool) string {
	members := []string{`"member":` + strconv.Quote(member)}
	if g.cfg.SoR != nil {
		var order []byte
		var found bool
		if cs, ok := g.cfg.SoR.(ContextSystemOfRecord); ok {
			order, found, _ = cs.OpenOrderContext(context.Background(), member)
		} else {
			order, found = g.cfg.SoR.OpenOrder(member)
		}
		if found {
			if system, code, _, err := shnsdk.ParseOrderProductCoding(order); err == nil {
				members = append(members, `"order":{"system":`+strconv.Quote(system)+`,"code":`+strconv.Quote(code)+`}`)
			}
		}
	}
	if extra != "" && !extraNamesMember {
		members = append(members, extra)
	} else if extra != "" {
		// extra names the member itself (dispatch): keep its other members.
		var rest map[string]json.RawMessage
		_ = json.Unmarshal([]byte("{"+extra+"}"), &rest)
		delete(rest, "member")
		for k, v := range rest {
			members = append(members, strconv.Quote(k)+":"+string(v))
		}
	}
	return testClaimBody(strings.Join(members, ","))
}

// testOrderBody is testClaimBody stating its facts for order (the order a PAS
// tail driven directly builds its claim about).
func testOrderBody(order []byte, extra string) string {
	system, code, _, err := shnsdk.ParseOrderProductCoding(order)
	if err != nil {
		panic(err)
	}
	stated := `"order":{"system":` + strconv.Quote(system) + `,"code":` + strconv.Quote(code) + `}`
	if extra != "" {
		stated += "," + extra
	}
	return testClaimBody(stated)
}
