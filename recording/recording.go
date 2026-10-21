// Package recording is the network's exchange record vocabulary: the closed
// lists an exchange record's direction, outcome, refusing party, refusal
// rule, backend error class and answer error class are drawn from, each
// ending in ExchangeOther. It names no workstream, and it imports no engine
// package, so a reader of recorded exchanges can use the gateway's own words
// without importing the engine.
//
// The vocabulary moved here from package engine, which keeps its names
// (engine.RefusalRules and the rest) as deprecated aliases of these.
package recording

// ExchangeOther is the value recorded for anything a closed list does not name.
const ExchangeOther = "other"

// Directions.
const (
	DirectionIngress = "ingress"
	DirectionInbound = "inbound"
)

// Outcomes.
const (
	ExchangeAnswered      = "answered"
	ExchangeRefused       = "refused"
	ExchangeUnreachable   = "unreachable"
	ExchangeUpstreamError = "upstream-error"
)

// Refusing parties.
const (
	RefusedByProviderGateway        = "provider-gateway"
	RefusedByHub                    = "hub"
	RefusedByAuthorizationFramework = "authorization-framework"
	RefusedByPayerGateway           = "payer-gateway"
)

// Refusal rules: the network's own primitives, and conformance for a check a
// participant opted into.
const (
	RefusalAuthentication = "authentication"
	RefusalAuthority      = "authority"
	RefusalConsent        = "consent"
	RefusalRouting        = "routing"
	RefusalReplay         = "replay"
	RefusalIntegrity      = "integrity"
	RefusalAudit          = "audit"
	RefusalFidelity       = "fidelity"
	RefusalConformance    = "conformance"
	RefusalLimit          = "limit"
)

// Backend error classes.
const (
	BackendTimeout   = "timeout"
	BackendConnect   = "connect"
	BackendTLS       = "tls"
	BackendAuth      = "auth"
	BackendHTTP3xx   = "http-3xx"
	BackendHTTP4xx   = "http-4xx"
	BackendHTTP5xx   = "http-5xx"
	BackendRead      = "read"
	BackendMalformed = "malformed"
	// BackendCancelled is a call cut short because the request it served
	// ended: the requester stopped waiting or went away. A system slower
	// than the responder's own deadline (the engine's WithBackendDeadline) is its
	// timeout instead. The exchange is not recorded as an upstream error.
	BackendCancelled = "cancelled"
)

// The closed lists. Each ends in ExchangeOther.
var (
	ExchangeDirections  = []string{DirectionIngress, DirectionInbound, ExchangeOther}
	ExchangeOutcomes    = []string{ExchangeAnswered, ExchangeRefused, ExchangeUnreachable, ExchangeUpstreamError, ExchangeOther}
	RefusalParties      = []string{RefusedByProviderGateway, RefusedByHub, RefusedByAuthorizationFramework, RefusedByPayerGateway, ExchangeOther}
	RefusalRules        = []string{RefusalAuthentication, RefusalAuthority, RefusalConsent, RefusalRouting, RefusalReplay, RefusalIntegrity, RefusalAudit, RefusalFidelity, RefusalConformance, RefusalLimit, ExchangeOther}
	BackendErrorClasses = []string{BackendTimeout, BackendConnect, BackendTLS, BackendAuth, BackendHTTP3xx, BackendHTTP4xx, BackendHTTP5xx, BackendRead, BackendMalformed, BackendCancelled, ExchangeOther}
	// AnswerErrorClasses say why an inbound leg's answer could not be
	// authorized: the request ended before it (cancelled: the requester was
	// gone), the authorize call ran out of time (timeout), the Authorization
	// Framework denied it (auth) or never answered it (unreachable), or
	// another failure (other).
	AnswerErrorClasses = []string{BackendCancelled, BackendTimeout, BackendAuth, ExchangeUnreachable, ExchangeOther}
)
