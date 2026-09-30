package engine

import (
	"fmt"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// forwardedContracts are the exchange contracts a payer gateway's native
// responder carries to the payer's own system (PAYER_DAVINCI_BASE_URL): every
// contract in the PA leg catalog. The other contracts a gateway declares
// (pa.pdex) are answered by the gateway itself, whatever the payer's system
// speaks.
func forwardedContracts() map[string]bool {
	out := map[string]bool{}
	for _, spec := range paCatalog {
		if spec.Contract != "" {
			out[spec.Contract] = true
		}
	}
	return out
}

// DeriveDeclaredContractVersions is the exchange-contract set a payer gateway
// that forwards to its payer's own system declares when SHN_CONTRACT_VERSIONS
// is unset and the payer's system declares its own versions
// (PAYER_DAVINCI_CONTRACT_VERSIONS, backend). A leg of a contract the payer's
// system does not share a line with is refused at the forward, so declaring
// the build default instead would advertise lines this gateway then refuses
// (FR-G48): the declaration follows the participant's own system.
//
// The result is the backend's lines for every forwarded contract (pa.crd,
// pa.dtr, pa.pas), plus the build default's lines for every other contract
// (pa.pdex@2.1, which the gateway answers itself), in NativeContractVersions
// order, so the same backend set always derives the same declaration. A
// backend token of another contract is not a forwarded line and does not
// change the result.
//
// It refuses — naming what to set instead — a forwarded line this build cannot
// exchange, and a backend set that names no forwarded line at all, which
// would declare a payer no provider can reach for prior authorization.
//
// It is the single rule: the gateway derives at boot with it, and the hosted
// control plane derives a hosted payer's declaration, its rendered
// SHN_CONTRACT_VERSIONS and its lanes with it, so the gateway and its registry
// entry cannot disagree.
func DeriveDeclaredContractVersions(backend []string) ([]string, error) {
	forwarded := forwardedContracts()
	native := shnsdk.NativeContractVersions()
	chosen := map[string]bool{}
	for _, raw := range backend {
		tok := strings.TrimSpace(raw)
		contract, _, ok := strings.Cut(tok, "@")
		if !ok || !forwarded[contract] {
			continue
		}
		if !slices.Contains(native, tok) {
			return nil, fmt.Errorf("PAYER_DAVINCI_CONTRACT_VERSIONS declares %s, a line this gateway cannot exchange (native: %s): set SHN_CONTRACT_VERSIONS to the lines this gateway should declare", tok, strings.Join(native, ", "))
		}
		chosen[tok] = true
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("PAYER_DAVINCI_CONTRACT_VERSIONS (%s) declares no CRD, DTR or PAS line, so no provider could reach this payer for prior authorization: declare the payer system's lines there, or set SHN_CONTRACT_VERSIONS", strings.Join(backend, ", "))
	}
	for _, tok := range shnsdk.SupportedContractVersions() {
		if contract, _, _ := strings.Cut(tok, "@"); !forwarded[contract] {
			chosen[tok] = true
		}
	}
	var out []string
	for _, tok := range native {
		if chosen[tok] {
			out = append(out, tok)
		}
	}
	return out, nil
}

// RequireForwardedContractLine refuses a declared set, explicit or derived,
// with no line for any contract a payer gateway forwards to its payer's own
// system (pa.crd, pa.dtr, pa.pas): a peer selects against the declaration, so
// such a payer is unreachable for prior authorization.
func RequireForwardedContractLine(declared []string) error {
	forwarded := forwardedContracts()
	for _, tok := range declared {
		if contract, _, ok := strings.Cut(strings.TrimSpace(tok), "@"); ok && forwarded[contract] {
			return nil
		}
	}
	return fmt.Errorf("SHN_CONTRACT_VERSIONS (%s) declares no CRD, DTR or PAS line, so no provider could reach this payer for prior authorization: declare the lines its system speaks", strings.Join(declared, ", "))
}
