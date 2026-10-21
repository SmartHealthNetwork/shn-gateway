package engine

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// WithEligibilityURL declares the payer's own coverage-eligibility endpoint:
// the absolute URL its system takes the request's POST on, the request as the
// provider's system sent it (a CoverageEligibilityRequest, or a Parameters
// holding one).
// A coverage-eligibility request is then carried to it exactly and its answer
// relayed, instead of being answered from the payer's records. Empty leaves
// the gateway answering from the records.
func WithEligibilityURL(eligibilityURL string) NativeOption {
	return func(n *nativeResponder) { n.eligibilityURL = eligibilityURL }
}

// forwardsEligibility reports whether the payer declared its own eligibility
// endpoint.
func (n *nativeResponder) forwardsEligibility() bool { return n.eligibilityURL != "" }

// forwardEligibility carries the network's coverage-eligibility request
// exactly to the payer's own endpoint and returns its answer: a 2xx answer
// relayed exactly, a non-2xx answer as the payer's error for verbatim relay,
// or an error when the payer's system gave no answer. Nothing is answered in
// the payer's place.
func (n *nativeResponder) forwardEligibility(ctx context.Context, request []byte) (LegResult, error) {
	in := relay.NewBody(request, relay.OriginPeerFrame)
	up, bad, err := n.post(ctx, n.eligibilityURL, "", relay.Exact(in, "application/fhir+json"), "coverage-eligibility", "eligibility")
	if err != nil {
		return LegResult{}, err
	}
	if bad.Status != 0 {
		return bad, nil
	}
	return LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status}, nil
}

// eligibilityForwarder is the occupant that carries coverage-eligibility to
// the payer's own endpoint when the payer declared one.
type eligibilityForwarder interface {
	forwardsEligibility() bool
	forwardEligibility(ctx context.Context, request []byte) (LegResult, error)
}

// EligibilityURLForTest reports the eligibility endpoint the payer declared
// (WithEligibilityURL), so the build that wires it can be proven from another
// package; empty when none was declared.
func (n *nativeResponder) EligibilityURLForTest() string { return n.eligibilityURL }

// WithDTRBaseURL overrides the base used for the DTR forwards
// (/Questionnaire/$questionnaire-package and $next-question), for partners whose DTR
// endpoint is NOT co-located with their PAS/FHIR base. Unset ⇒ DTR posts use
// the FHIR baseURL (co-located default, the prior behavior).
func WithDTRBaseURL(dtrBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if dtrBaseURL != "" {
			n.dtrBaseURL = dtrBaseURL
		}
	}
}

// WithPASBaseURL overrides the base used for the PAS forwards (/Claim/$submit for
// submit and update; a later PAS operation belongs here too), for partners whose PAS
// endpoint is NOT co-located with their DTR/FHIR base. Unset ⇒ PAS posts use
// the FHIR baseURL (co-located default, the prior behavior).
func WithPASBaseURL(pasBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if pasBaseURL != "" {
			n.pasBaseURL = pasBaseURL
		}
	}
}

// WithEndpointEvidenceObserver supplies the endpoint-evidence same-origin-drop note sink
// (see endpointEvidenceObserver's field comment). Unset ⇒ silent drops (still
// dropped — this only affects observability, never the trust decision).
func WithEndpointEvidenceObserver(f func(note string)) NativeOption {
	return func(n *nativeResponder) { n.endpointEvidenceObserver = f }
}

// EndpointEvidenceSetter is the sink app.go's checks-runner→responder
// evidence hook targets: implemented by *nativeResponder (SetEndpointEvidence,
// below). A separate, narrower interface from LegResponder (a stable public
// partner-implementable seam) on purpose — this is app-internal wiring
// plumbing between the checks runner and the ONE responder that can act on
// probe evidence, not part of the payer-content contract every LegResponder
// implements.
type EndpointEvidenceSetter interface {
	SetEndpointEvidence(evidence map[string]string)
}

var _ EndpointEvidenceSetter = (*nativeResponder)(nil)

// SetEndpointEvidence WHOLESALE-replaces the responder's per-line endpoint
// evidence (HRex per-version endpoint selection): app.go's checks-runner
// hook feeds it the davinci-config probe's
// retained "<contract>@<line>" -> URL map (checks.Capability.EndpointURLs)
// once per completed checks cycle. No TTL — this cycle's evidence replaces
// last cycle's outright; a token absent this time is simply gone (never
// sticky), and an empty/nil map clears everything back to the configured
// base.
//
// SAME-ORIGIN TRUST RULE (binding project ruling): an entry is honored IFF its
// scheme+host+port equal those of the base ITS CONTRACT forwards to
// (contractBase: the DTR base for pa.dtr, the PAS base for pa.pas, the
// shared base otherwise — each of which IS the shared base unless a
// per-operation base is configured). Judged per contract base, never
// the shared base alone: a split-base partner must not be fenced out of its
// own published DTR endpoint (TestEndpointEvidenceFenceJudgedPerContractBase).
// A probe-published cross-origin
// endpoint is NEVER a target a PHI-bearing submission follows — a
// misconfigured or compromised davinci-configuration document must not be
// able to redirect traffic off the operator-configured partner.
// Non-same-origin (or unparseable) entries are DROPPED HERE, AT SET TIME —
// never stored, never resolved later — each noted via
// endpointEvidenceObserver (TestEndpointEvidenceSameOriginEnforced is the
// rejection test).
func (n *nativeResponder) SetEndpointEvidence(evidence map[string]string) {
	kept := make(map[string]string, len(evidence))
	for tok, u := range evidence {
		contract, _, _ := strings.Cut(tok, "@")
		baseOrigin, baseOK := originOf(n.contractBase(contract))
		entryOrigin, ok := originOf(u)
		if !baseOK || !ok || entryOrigin != baseOrigin {
			if n.endpointEvidenceObserver != nil {
				n.endpointEvidenceObserver(fmt.Sprintf(
					"engine: endpoint evidence for %q dropped (not same-origin as the configured base %s): %s",
					tok, baseOrigin, redactURLForLog(u)))
			}
			continue
		}
		kept[tok] = u
	}
	n.evMu.Lock()
	n.endpointEvidence = kept
	n.evMu.Unlock()
}

// EndpointEvidenceForTest returns a copy of the currently-held endpoint
// evidence — a READ-ONLY test-observation seam (the repo's *ForTest
// precedent: TransformPASForTest, SelectChainRouteForTest,
// NormalizePASResponseForTest), used by gateway/app's
// TestProbeEvidenceReachesResponder to prove evidence that arrived through
// the REAL checksRunner.Run → OnResults → SetEndpointEvidence wiring landed
// here — never a second, test-only write path.
func (n *nativeResponder) EndpointEvidenceForTest() map[string]string {
	n.evMu.RLock()
	defer n.evMu.RUnlock()
	out := make(map[string]string, len(n.endpointEvidence))
	for k, v := range n.endpointEvidence {
		out[k] = v
	}
	return out
}

// defaultPortForScheme is the scheme's implicit port ("" for a scheme with
// none) — RFC 3986 §6.2.3: "http://x" and "http://x:80" name the SAME
// origin. Without this, a same-origin comparison done by exact string
// equality (the original shape) drops ALL evidence the moment the operator
// base and the partner-published URL disagree on whether to spell out the
// scheme's own default port — a common, unremarkable config shape, not an
// attack — silently disabling endpoint evidence for that deployment while looking, in the
// logs, exactly like a real cross-origin rejection.
func defaultPortForScheme(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// originOf reduces raw to its normalized "scheme://host:port" origin — an
// explicit port equal to the scheme's default is treated identically to no
// port at all (defaultPortForScheme). ok is false for an unparseable URL or
// one missing a scheme/host (never same-origin to anything).
func originOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(u.Scheme)
	}
	host := u.Hostname()
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return u.Scheme + "://" + host, true
}

// redactURLForLog keeps only scheme+host+port for a log line (the REDACTION
// RULE this codebase applies to every operator-visible string derived from
// partner input — checks.targetOf's exact precedent): a rejected entry's
// path/query is peer-published, untrusted, and never surfaced verbatim.
func redactURLForLog(raw string) string {
	if origin, ok := originOf(raw); ok {
		return origin
	}
	return "(unparseable)"
}

// dtrBase / pasBase are the per-operation bases with their fallback applied:
// the configured DTR/PAS base when set, the shared base otherwise. Every DTR
// and PAS forward reads through these (never the raw fields), so a responder
// built without the constructor defaults still forwards to the shared base.
func (n *nativeResponder) dtrBase() string {
	if n.dtrBaseURL != "" {
		return n.dtrBaseURL
	}
	return n.baseURL
}

func (n *nativeResponder) pasBase() string {
	if n.pasBaseURL != "" {
		return n.pasBaseURL
	}
	return n.baseURL
}

// contractBase is the configured base a contract's operations forward to: the
// DTR base for pa.dtr, the PAS base for pa.pas, the shared base for everything
// else. Each per-operation base defaults to the shared base, so a
// deployment that sets neither resolves every contract to baseURL exactly as
// before. It is the base the same-origin fence judges evidence against AND the
// fallback resolvedURL appends the operation path to.
func (n *nativeResponder) contractBase(contract string) string {
	switch contract {
	case "pa.dtr":
		return n.dtrBase()
	case "pa.pas":
		return n.pasBase()
	}
	return n.baseURL
}

// resolvedURL is the per-line endpoint resolution: the post URL for a leg
// routed at contract@line, preferring same-origin-validated probe evidence
// over the configured base+path. contract == "" (version-neutral leg) or no
// answer line resolved on ctx, or no evidence for that exact token, all fall
// back to base+path UNCHANGED — the fence (TestNativeForwardSelectsLineEndpoint's
// byte-identical case). Read under RLock; SetEndpointEvidence is the sole
// writer (wholesale replace under Lock) — concurrency-clean under -race.
func (n *nativeResponder) resolvedURL(ctx context.Context, contract, base, path string) string {
	def := base + path
	if contract == "" {
		return def
	}
	line := answerLineOr(ctx, contract)
	if line == "" {
		return def
	}
	n.evMu.RLock()
	u, ok := n.endpointEvidence[contract+"@"+line]
	n.evMu.RUnlock()
	if !ok {
		return def
	}
	return u
}

// forwardDTROperation sends a framed DTR operation's own input to the payer's
// system at path and relays the answer exactly. When carrier is set, the
// payer identity of every coverage in the input is mapped (when the mapping
// is configured) and nothing else changes.
func (n *nativeResponder) forwardDTROperation(ctx context.Context, contract string, in relay.Body, path string, carrier payorEdgeCarrier, leg, label string) (LegResult, error) {
	const fhirJSON = "application/fhir+json"
	request := relay.Exact(in, fhirJSON)
	if carrier != 0 {
		mapped, lr, err := n.payorEdgeRequest(in, carrier, fhirJSON, nil)
		if err != nil || lr.Status != 0 {
			return lr, err
		}
		request = mapped
	}
	up, bad, err := n.post(ctx, n.resolvedURL(ctx, contract, n.dtrBase(), path), "", request, leg, label)
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	return LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status}, nil
}
