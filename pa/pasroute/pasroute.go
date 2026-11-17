// Package pasroute is the address of a PAS per-payer base on a provider
// gateway's own Da Vinci ingress: the fixed base at which the provider's
// system sends Claim/$submit and Claim/$inquire for that payer, and reads its
// capabilities, as it would at the payer's own base in a direct integration.
// The network routes a request at the base to that payer by the address
// alone.
//
// The address is rebuilt from the gateway's ingress base and the payer's
// participant id alone, so no gateway stores a mapping and nothing a request
// carries chooses it. The gateway's ingress serves it and lists it at
// PayersPath.
package pasroute

import (
	"fmt"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
)

// Prefix is the path every per-payer base starts with, under the ingress
// base. The payer's participant id, path-escaped, is the one segment after
// it.
const Prefix = "/pas/payer/"

// PayersPath is the provider gateway's listing of the payers' bases.
const PayersPath = "/pas/payers"

// Base returns the base on a provider gateway's ingress for the payer whose
// participant id is payer. The base and the id are held to the rules of a
// CDex per-provider base (cdexroute.ProviderBase): the ingress base is an
// absolute, canonical http or https URL with no user information, query or
// fragment, and the id is non-empty, not "." or "..", with no space or
// control character. Its errors wrap cdexroute's, so errors.Is reads the
// same refusals.
func Base(ingressBase, payer string) (string, error) {
	base, err := cdexroute.BaseUnder(ingressBase, Prefix, payer, "payer")
	if err != nil {
		return "", fmt.Errorf("pasroute: %w", err)
	}
	return base, nil
}
