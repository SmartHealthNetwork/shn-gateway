package engine

// testRequestingProvider is the participant record a prior-authorization request
// in this package names as its requesting provider.
//
// It is not decoration on the request. Da Vinci PAS says a payer SHALL match an
// inquiry on the member id PLUS the ordering or rendering provider identifier,
// so a request that identifies no provider is one no conformant inquiry can find
// again — and a Claim.provider carrying only display text satisfies the element
// at every line, so nothing structural would have said so.
//
// It is the record the census stand-in serves for OrderingProviderRef, and its
// id matches that reference: the provider a request names is the one its ORDER
// named, so a fixture whose two halves disagreed would build a request whose own
// order points at a party the Bundle does not carry.
func testRequestingProvider() []byte {
	return []byte(`{"resourceType":"Organization","id":"org-ordering-provider",` +
		`"identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"` + censusOrderingProviderNPI + `"}],` +
		`"name":"Test Provider Group"}`)
}
