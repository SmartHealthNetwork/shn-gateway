// workstream_pa.go — the Layer-3 Prior-Authorization workstream module.
// It NAMES AND RELOCATES what was inlined in the fused engine: the per-leg-type
// authority frames, operations, transaction types, scopes, and leg-physics. The
// Layer-2 primitives (OriginateLeg, handleInbound's FulfillLeg dispatch) read this
// catalog instead of carrying the literals inline, so the origination and inbound
// sides can no longer drift. A future workstream (X12/EDI, payer-to-payer) is a
// sibling module with its own catalog; Layers 1-2 do not change.
package engine

// legSpec is the per-leg-type contract: the authority/transport literals that were
// formerly passed positionally at each roundTrip call site (origination) and held
// in the now-deleted inboundReqOp map (fulfillment). ReqFrame is the originator's authority frame
// ("provider-tpo" for every PA leg today); RespFrame is the responder's. Op/RespOp
// are the request/response authority operations. Scope is the policy-derived
// min-necessary scope label (documentary; authz derives the real scope). Physics is
// the Layer-2 leg classification — recorded now, load-bearing later
// (notification/async are headroom; no PA scenario exercises them).
type legSpec struct {
	ReqFrame  string
	RespFrame string
	Op        string
	RespOp    string
	Scope     string
	Physics   LegPhysics
}

// paCatalog is the PA workstream's leg catalog, keyed by legType (= envelope
// TransactionType). Every entry is the EXACT literal set today's call sites pass
// (verified byte-for-byte by workstream_pa_test.go). The recipient is NOT a catalog
// property: payer legs target Config.CounterpartID, facility/phg legs target a
// registry LookupByRole result, so the recipient stays a call-time argument.
var paCatalog = map[string]legSpec{
	"coverage-eligibility": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "eligibility-inquiry", RespOp: "eligibility-response", Scope: "eligibility-scope",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	"dtr-questionnaire-fetch": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "dtr-questionnaire-fetch", RespOp: "dtr-questionnaire", Scope: "questionnaire-only",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	"federated-query": {
		ReqFrame: "provider-tpo", RespFrame: "facility-disclosure",
		Op: "federated-query-submit", RespOp: "federated-query-response", Scope: "named-docs-only",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	"patient-dtr": {
		ReqFrame: "provider-tpo", RespFrame: "patient-authorship",
		Op: "patient-dtr-request", RespOp: "patient-dtr-response", Scope: "patient-authorship-only",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	"crd-order-select": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "crd-order-select", RespOp: "crd-cards", Scope: "crd-context",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	// crd-order-dispatch: the order-dispatch sibling of crd-order-select.
	// A DISTINCT Op is load-bearing: handleInbound pins spec.Op per TransactionType into
	// VerifyBound; sharing the Op with crd-order-select would let an order-select token
	// be lifted onto an order-dispatch envelope — an AI-11/C2 regression.
	"crd-order-dispatch": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "crd-order-dispatch", RespOp: "crd-dispatch-cards", Scope: "crd-context",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	"pas-claim": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "pas-submit", RespOp: "pas-response", Scope: "pas-bundle",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectMutating, Timing: TimingSync, Locality: LocalitySubstrate},
	},
	// F-B1: the conformant amended re-POST leg — the UPDATE-family analog of pas-claim
	// (which is the conformant SUBMIT leg). It carries the update op-family (Op
	// pas-update-submit; RespOp pas-update-response; Scope pas-update-bundle — NOT pas-bundle),
	// reusing the update resp-op exactly as pas-claim reuses pas-response. Keeping it in the
	// pas-update-bundle scope preserves the pas-bundle ∥ pas-update-bundle min-necessary
	// distinction (FR-34/C12).
	"pas-claim-update": {
		ReqFrame: "provider-tpo", RespFrame: "payer-coverage",
		Op: "pas-update-submit", RespOp: "pas-update-response", Scope: "pas-update-bundle",
		Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectMutating, Timing: TimingSync, Locality: LocalitySubstrate},
	},
}
