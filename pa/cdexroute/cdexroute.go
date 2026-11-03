// Package cdexroute is the address of a CDex per-participant base on a
// gateway's own Da Vinci ingress:
//   - a documentation return path: the fixed per-payer base on a provider
//     gateway, to which the provider's system sends `$submit-attachment` for
//     that payer (ReturnBase);
//   - a per-provider base: the fixed base on a payer gateway at which the
//     payer's system creates an attachment-request Task for that provider
//     (ProviderBase).
//
// Each address is rebuilt from the gateway's ingress base and a participant id
// alone, so no gateway stores a mapping and nothing a request carries chooses
// it. The gateway's ingress serves it and its discovery publishes it.
package cdexroute

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// Prefix is the path every return path starts with, under the ingress base.
// The payer's participant id, path-escaped, is the one segment after it.
const Prefix = "/cdex/payer/"

// Operation is the CDex operation a return path accepts, posted either to the
// return path itself or to the return path with this segment appended.
const Operation = "$submit-attachment"

// ReturnBase returns the return path for the payer whose participant id is
// payer, under the provider gateway's public ingress base: the value a
// re-keyed payer-url names. The base must be an absolute http or https URL
// with no user information, query or fragment (no "?" or "#" at all: a bare
// one parses as empty, and an address appended after it would not be a
// path); the payer id must be non-empty, not "." or "..", with no space or
// control character.
// The address names no operation: the ingress takes a submission at the base
// and at the base with Operation appended.
func ReturnBase(ingressBase, payer string) (string, error) {
	return under(ingressBase, Prefix, payer, "payer")
}

// ProviderPrefix is the path every per-provider base on a payer gateway's own
// Da Vinci ingress starts with, under the ingress base. The provider's
// participant id, path-escaped, is the one segment after it.
const ProviderPrefix = "/cdex/provider/"

// ProviderBase returns the base on a payer gateway's ingress for the provider
// whose participant id is provider: a payer's system creates an
// attachment-request Task for that provider at the base's Task endpoint
// ("<base>/Task"), and the network routes it to that provider by the address
// alone. The base and the id are held to ReturnBase's rules.
func ProviderBase(ingressBase, provider string) (string, error) {
	return under(ingressBase, ProviderPrefix, provider, "provider")
}

// under is the address of participant's one segment after prefix, under
// ingressBase; role names the participant in an error.
func under(ingressBase, prefix, participant, role string) (string, error) {
	u, err := url.Parse(ingressBase)
	if err != nil {
		return "", errors.New("cdexroute: ingress base is not a URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("cdexroute: ingress base is not an absolute http or https URL")
	}
	if strings.ContainsAny(ingressBase, "?#") {
		return "", errors.New("cdexroute: ingress base carries a query or fragment")
	}
	if u.User != nil {
		return "", errors.New("cdexroute: ingress base carries user information")
	}
	if participant == "" || participant == "." || participant == ".." || strings.IndexFunc(participant, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("cdexroute: " + role + " participant id is empty, a dot segment, or carries a space or control character")
	}
	return strings.TrimRight(ingressBase, "/") + prefix + url.PathEscape(participant), nil
}
