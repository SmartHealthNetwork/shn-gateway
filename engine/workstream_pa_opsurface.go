// workstream_pa_opsurface.go — read-only export of the PA workstream catalog's operation
// bindings. The other trust domains keep independent copies of these bindings
// by design (the Hub is payload-blind and must not import this engine; the
// published SDK is the partner-facing surface and imports neither), so a
// divergence on a shared TransactionType would surface only as a live 400/403.
// This accessor exists so a network-side lockstep conformance check can pin
// those copies against this catalog — the same read-only-accessor precedent as
// ChainSteps. It exposes only the wire-contract bindings (frames, operations and
// the minimum-necessary scope the authorization policy signs for them); physics
// and contract line stay internal.
package engine

// PALegOperation is one leg type's wire-contract operation binding: the
// authority frame and operation of the request leg and of the response leg,
// and the minimum-necessary scope label both legs carry. The authorization
// policy is the authority for Scope (it is the value signed into tokens and
// audit records); the catalog mirrors it.
type PALegOperation struct {
	RequestFrame  string
	RequestOp     string
	ResponseFrame string
	ResponseOp    string
	Scope         string
}

// PALegOperations returns a fresh copy of the PA workstream catalog's
// operation bindings, keyed by envelope TransactionType — the exact frames and
// operations OriginateLeg stamps and handleInbound enforces, and each leg's
// documentary Scope label. Mutating the returned map never touches the catalog.
// The federated query is the Federated Query Services workstream's leg, not
// PA's (FQSLegOperations).
func PALegOperations() map[string]PALegOperation {
	pa, _ := catalogFor(workstreamPA) // PA is always compiled in (workstreams)
	out := make(map[string]PALegOperation, len(pa))
	for legType, spec := range pa {
		out[legType] = PALegOperation{
			RequestFrame:  spec.ReqFrame,
			RequestOp:     spec.Op,
			ResponseFrame: spec.RespFrame,
			ResponseOp:    spec.RespOp,
			Scope:         spec.Scope,
		}
	}
	return out
}
