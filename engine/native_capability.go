package engine

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// NativeReceiveOnlyVersions extracts the backend's independently configured
// receive lines, including SDK-known lines absent from this gateway's builder
// declaration. A payer's configured Da Vinci base
// serves the catalog's CRD, DTR and PAS operation paths; the contract tokens
// assert which representations those endpoints accept. They do not authorize
// gateway builders, transformation or conformance certification.
func NativeReceiveOnlyVersions(role, backendBase string, tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	if role != "payer" {
		return nil, fmt.Errorf("native receive contracts require a payer gateway")
	}
	u, err := url.Parse(backendBase)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("native receive contracts require a configured HTTP(S) payer backend base URL")
	}
	var out []string
	for _, token := range tokens {
		if len(token) < 3 || len(token) > 48 || !nativeContractTokenPattern.MatchString(token) {
			return nil, fmt.Errorf("native receive contract token %q is malformed", token)
		}
		contract, _, _ := strings.Cut(token, "@")
		if contract != "pa.crd" && contract != "pa.dtr" && contract != "pa.pas" {
			return nil, fmt.Errorf("native receive contract %q has no configured gateway operation route", token)
		}
		if !slices.Contains(out, token) {
			out = append(out, token)
		}
	}
	return out, nil
}

// PublishedContractVersions adds only independently executable receive lines
// to the SDK build declaration carried through the existing registrar/feed.
func PublishedContractVersions(build, receive []string) []string {
	out := slices.Clone(build)
	for _, token := range receive {
		if !slices.Contains(out, token) {
			out = append(out, token)
		}
	}
	return out
}

// NativePublishedContractVersions keeps the gateway's builder declaration
// separate from its receive declaration. An explicit backend token list is
// exhaustive for the native CRD/DTR/PAS operation routes, including known SDK
// lines; a gateway cannot publish a buildable line its backend would refuse.
func NativePublishedContractVersions(role, backendBase string, backendTokens, build []string) ([]string, error) {
	receive, err := NativeReceiveOnlyVersions(role, backendBase, backendTokens)
	if err != nil {
		return nil, err
	}
	if len(backendTokens) == 0 {
		return slices.Clone(build), nil
	}
	backend := map[string]bool{}
	for _, token := range backendTokens {
		backend[token] = true
	}
	var published []string
	for _, token := range build {
		contract, _, _ := strings.Cut(token, "@")
		if (contract == "pa.crd" || contract == "pa.dtr" || contract == "pa.pas") && !backend[token] {
			continue
		}
		published = append(published, token)
	}
	return PublishedContractVersions(published, receive), nil
}

// canCarryNative asks only whether an independently registered endpoint serves
// this operation and representation. It never asks a validator about payloads.
func (g *Gateway) canCarryNative(recipient, legType, declaredVersion string) bool {
	contract, err := legContract(legType)
	if err != nil {
		return false
	}
	peer, ok := g.cfg.Reg.Lookup(recipient)
	if !ok {
		return false
	}
	endpoint, err := url.Parse(peer.BaseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil {
		return false
	}
	if declaredVersion != "" && (contract == "" || !strings.HasPrefix(declaredVersion, contract+"@")) {
		return false
	}
	if len(peer.ContractVersions) == 0 {
		// Pre-version endpoints implement the catalog routes. Their software may
		// still refuse the actual request; never dispatch a compatibility probe.
		return declaredVersion == "" || nativeContractToken(declaredVersion)
	}
	if contract == "" {
		return true
	}
	lines := contractLineSet(peer.ContractVersions, contract)
	if declaredVersion == "" {
		return len(lines) > 0
	}
	return lines[strings.TrimPrefix(declaredVersion, contract+"@")]
}

// Only this concrete adapter defers framed representation admission to its
// actual configured backend. Wrappers and local-action consumers stay build-bound.
func (g *Gateway) nativeFrameConsumer(leg string) bool {
	n, ok := g.cfg.Responder.(*nativeResponder)
	if !ok || n == nil {
		return false
	}
	switch leg {
	case "crd-order-select", "crd-order-dispatch", "dtr-questionnaire-fetch", "pas-claim", "pas-claim-update", "pas-claim-inquire":
		return true
	}
	return false
}

var nativeContractTokenPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*@[0-9]+(\.[0-9]+)*$`)

func validNativeContractToken(token, contract string) bool {
	return len(token) >= 3 && len(token) <= 48 && strings.HasPrefix(token, contract+"@") && nativeContractTokenPattern.MatchString(token)
}

// Configuration is copied at construction. Evidence refresh cannot expand this
// set, and a legacy answer/build line is never an actual request declaration.
func (n *nativeResponder) admitsNativeRequest(ctx context.Context, contract string) bool {
	declared := ""
	if ex, ok := ctx.Value(nativeExchangeKey{}).(ExchangeContext); ok {
		declared = ex.contractVersion
	}
	if declared != "" && !validNativeContractToken(declared, contract) {
		return false
	}
	if len(n.declaredContractVersions) == 0 {
		return declared == "" || nativeContractToken(declared)
	}
	lines := contractLineSet(n.declaredContractVersions, contract)
	return len(lines) > 0 && (declared == "" || lines[shnsdk.LineOf(declared)])
}

// nativeEndpoint is one immutable dispatch snapshot. No reference to mutable
// evidence survives the lock, and no I/O occurs while holding it.
type nativeEndpoint struct {
	url, version, versionSource string
	admitted                    bool
}

// Existing HRex evidence describes PAS submission and DTR package only. It
// cannot redirect or attest a sibling operation, even at an identical URL.
func operationEndpointEvidence(leg, path string) bool {
	switch leg {
	case "pas-claim", "pas-claim-update":
		return path == "/Claim/$submit"
	case "dtr-questionnaire-fetch":
		return path == "/Questionnaire/$questionnaire-package"
	}
	return false
}

// endpointForDispatch selects admission, operation URL and independent response
// declaration once. CRD retains its independently selected CDS catalog service.
func (n *nativeResponder) endpointForDispatch(ctx context.Context, leg, base, path string) nativeEndpoint {
	endpoint := nativeEndpoint{url: base + path}
	contract, err := legContract(leg)
	if err != nil || contract == "" {
		return endpoint
	}
	n.evMu.RLock()
	defer n.evMu.RUnlock()
	endpoint.admitted = n.admitsNativeRequest(ctx, contract)
	relevant := operationEndpointEvidence(leg, path)
	if relevant {
		if published, ok := n.endpointEvidence[contract+"@"+answerLineOr(ctx, contract)]; ok {
			endpoint.url = published
		}
	}
	if bindings, managed := n.responseDeclarations.bindings[nativeResponseOperation(leg, path)]; managed {
		if version, matched := bindings[endpoint.url]; matched {
			endpoint.version, endpoint.versionSource = version, "configured-endpoint"
		}
	}
	return endpoint
}

// SupportedRequestFrames names the request codecs implemented by this gateway
// source. Registration must additionally prove this is the artifact serving it.
func SupportedRequestFrames() []string {
	return append(shnsdk.SupportedRequestFrames(), shnsdk.RequestFrameV1CRD)
}
