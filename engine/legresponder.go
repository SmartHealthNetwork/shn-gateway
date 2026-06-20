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
}
