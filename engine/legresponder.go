package engine

import "context"

// LegResponder is the per-leg payer CONTENT seam (FHIR-in / FHIR-out). The engine
// owns authority (the A/B inbound fences + the C outbound fence), sealing, edge
// $validate, and audit; the connector owns content only — the decision and the
// response FHIR.
//
// leg is the inbound TransactionType. corrID + subjectPCI are engine-owned,
// leg-invariant authority outputs the connector needs for builders/Store keys
// (subjectPCI is the token's PCI; the connector never resolves it). requestFHIR is
// the already-opened, already-authority-fenced request plaintext.
type LegResponder interface {
	Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error)
}

// LegResult is what a LegResponder returns.
type LegResult struct {
	ResponseFHIR   []byte       // sealed back; engine (C)-fences + egress-$validates (FHIR legs)
	SideEffectFHIR [][]byte     // payer-local FHIR to persist (EOB); engine egress-$validates each before Commit
	Status         int          // connector-signalled HTTP outcome (409/422); 0 = proceed
	Message        string       // body for a non-zero Status
	Commit         func() error // NON-FHIR durable state (Store writes); fired after buildResponseLeg, before writeLeg; error => 502
	Rollback       func()       // undo a claim acquired in Handle; engine arms defer-rollback-unless-committed
	// ResponseRelayed reports that ResponseFHIR is a verbatim relay of a foreign holder's bytes
	// (SHN did not produce it). When true the engine SKIPS the response egress-$validate (R-8:
	// foreign Da Vinci profiles are unresolvable in SHN's US-Core-only validator). SHN-produced
	// side-effects (the EOB) are $validated unconditionally. Zero value = strict. Only the
	// native-forward responder sets this.
	ResponseRelayed bool
	// ResponseSubjectForeign reports that ResponseFHIR's patient subject is in a foreign
	// (non-SHN-member) namespace (a real br-payer answers Patient/SubscriberExample), so a
	// member-match against the bound request patient is a category error. When true the engine
	// SKIPS the (C) ClaimResponse member-fence (R-7). The SHN-produced EOB side-effect is
	// member-fenced unconditionally. Zero value = strict. Only the native-forward responder sets
	// this. (Two flags, not one: the conformant-mock north star is SHN-produced-yet-foreign-
	// namespace — $validate must stay ON while the member-fence stands down; a fused bool would
	// skip $validate on an SHN-produced resource, an FR-36 violation.)
	ResponseSubjectForeign bool
}
