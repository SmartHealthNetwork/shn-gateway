package engine

import (
	"context"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

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
	// Response is the answer sealed back to the requester. Its ownership says
	// whose message it is: relay.Exact (or an edit) for the participant's own
	// answer, relayed; relay.Authored for a message this gateway built. The
	// engine checks it against the leg's ownership table before it is fenced,
	// $validated or sent. Its ContentType is the answer's media type.
	//
	// On a non-2xx Status, a set Response is the participant's application
	// error, relayed; an unset Response means the gateway (or the connector)
	// refuses, and Message is the refusal text.
	Response       relay.Payload
	SideEffectFHIR [][]byte     // payer-local FHIR to persist (EOB); engine egress-$validates each before Commit
	Status         int          // connector-signalled HTTP outcome (409/422); 0 = proceed
	Message        string       // body for a non-zero Status
	Commit         func() error // NON-FHIR durable state (Store writes); fired after buildResponseLeg, before writeLeg; error => 502
	Rollback       func()       // undo a claim acquired in Handle; engine arms defer-rollback-unless-committed
	// ResponseAssembled identifies a local terminal PAS replacement. It requires
	// PAS-profile certification and holder-local assembly Provenance before Commit.
	ResponseAssembled bool
	// ResponseSubjectForeign identifies the payer's patient namespace. The full
	// graph must remain internally subject-consistent; it is not compared with
	// the request's SHN member id. Locally produced EOBs remain member-fenced.
	ResponseSubjectForeign bool
}

// ResponseRelayed reports whether the answer is the participant's own message
// (relayed, possibly with registered edits) rather than one this gateway
// built. Relayed bytes are neither certified nor stamped by this gateway;
// locally produced side-effects still are.
func (r LegResult) ResponseRelayed() bool {
	o := r.Response.Ownership()
	return o == relay.OwnershipRelayed || o == relay.OwnershipEdited
}

// ResponseContentType is the answer's media type.
func (r LegResult) ResponseContentType() string { return r.Response.ContentType() }
