// opsurface.go — the operation surface of every compiled-in workstream: each
// leg type's wire-contract operation binding, which the network-side lockstep
// conformance check pins the Hub's, the SDK's and the accounts service's
// copies against. A workstream's catalog fills it; PA's own accessor is in
// workstream_pa_opsurface.go.
package engine

// LegOperation is one leg type's wire-contract operation binding, whichever
// workstream's catalog holds the leg: the authority frame and operation of the
// request leg and of the response leg, and the minimum-necessary scope label
// both legs carry.
type LegOperation struct {
	RequestFrame  string
	RequestOp     string
	ResponseFrame string
	ResponseOp    string
	Scope         string
}

// LegOperations returns a fresh copy of every compiled-in workstream
// catalog's operation bindings, keyed by envelope TransactionType: PA's and
// FQS's in a release build (PALegOperations, FQSLegOperations), and a test
// profile's beside them in a build that compiles one in. Transaction types are
// unique across the compiled-in catalogs, so no workstream's row overwrites
// another's.
func LegOperations() map[string]LegOperation {
	out := map[string]LegOperation{}
	for _, c := range workstreamCatalogs() {
		for legType, spec := range c {
			out[legType] = LegOperation{
				RequestFrame:  spec.ReqFrame,
				RequestOp:     spec.Op,
				ResponseFrame: spec.RespFrame,
				ResponseOp:    spec.RespOp,
				Scope:         spec.Scope,
			}
		}
	}
	return out
}
