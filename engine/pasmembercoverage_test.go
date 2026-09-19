package engine

import (
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// testMemberCoverage is the participant record a prior-authorization request in
// this package names as the coverage it is made under.
//
// It is not decoration on the request either. Claim.insurance.coverage is min=1
// mustSupport at every PAS line and the insurer locates the policy from that
// Coverage's details; a payer that stores what it is given matches a later
// inquiry against the coverage it stored. So the request carries the member's
// OWN record — its own id and its own member identifier — because a Coverage the
// builder minted is one no inquiry built from the participant's own system could
// name again, and one minted id shared by every member is one member's coverage
// overwriting another's.
func testMemberCoverage(member string) []byte {
	return []byte(`{"resourceType":"Coverage","id":"cov-` + strings.ToLower(member) + `","status":"active",` +
		`"identifier":[{"type":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/v2-0203","code":"MB"}]},` +
		`"system":"urn:shn:coverage","value":"` + member + `"}],` +
		`"beneficiary":{"reference":"Patient/` + member + `"},` +
		`"relationship":{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/subscriber-relationship","code":"self"}]},` +
		`"payor":[{"reference":"Organization/org-cms-payer"}]}`)
}

// testMemberCoverageRef is the reference the participant's own records name that
// Coverage by — what a caller passes as CoverageRef for its QR-context and
// native-lane roles, and the entry the built request's Claim resolves to.
func testMemberCoverageRef(member string) string {
	return "Coverage/cov-" + strings.ToLower(member)
}

// testPayerOrganization is the participant record a prior-authorization request
// in this package names as the payer it is made under.
//
// It is the third element of the same rule the provider and the coverage follow.
// The payer scopes an inquiry's search by the insurer and resolves an
// Organization to one it holds only through an NPI, which a payer organization
// does not carry -- so whatever a submission names is what a later inquiry has
// to name, and a request naming a payer organization the builder minted is one
// the participant's own inquiry can never match.
func testPayerOrganization(payer shnsdk.PayerIdentifier) []byte {
	return []byte(`{"resourceType":"Organization","id":"org-cms-payer",` +
		`"identifier":[{"system":"` + payer.System + `","value":"` + payer.Value + `"}],` +
		`"name":"Centers for Medicare and Medicaid Services"}`)
}
