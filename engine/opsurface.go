// opsurface.go — read-only export of the PA workstream catalog's operation
// bindings. The other trust domains keep independent copies of these bindings
// by design (the Hub is payload-blind and must not import this engine; the
// published SDK is the partner-facing surface and imports neither), so a
// divergence on a shared TransactionType would surface only as a live 400/403.
// This accessor exists so a network-side lockstep conformance check can pin
// those copies against this catalog — the same read-only-accessor precedent as
// ChainSteps. It exposes only the wire-contract bindings (frames + operations);
// scope, physics and contract line stay internal.
package engine

// PALegOperation is one leg type's wire-contract operation binding: the
// authority frame and operation of the request leg and of the response leg.
type PALegOperation struct {
	RequestFrame  string
	RequestOp     string
	ResponseFrame string
	ResponseOp    string
}

// PALegOperations returns a fresh copy of the PA workstream catalog's
// operation bindings, keyed by envelope TransactionType — the exact bindings
// OriginateLeg stamps and handleInbound enforces. Mutating the returned map
// never touches the catalog.
func PALegOperations() map[string]PALegOperation {
	out := make(map[string]PALegOperation, len(paCatalog))
	for legType, spec := range paCatalog {
		out[legType] = PALegOperation{
			RequestFrame:  spec.ReqFrame,
			RequestOp:     spec.Op,
			ResponseFrame: spec.RespFrame,
			ResponseOp:    spec.RespOp,
		}
	}
	return out
}
