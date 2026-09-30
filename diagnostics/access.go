package diagnostics

import "time"

// AccessLine is the one line a gateway writes for each call it answers on a Da
// Vinci ingress route or for a leg it receives from the network
// (engine.ExchangeRecord, as JSON). It is written to the gateway's log as `gateway: access: <json>` at
// every conformance level, and, with capture configured, published as the
// Detail of a metadata-only Event of kind KindAccess. Metadata only: never a
// body or a patient identifier. The ids it carries are the leg's, the
// caller's own trace id (Trace, the X-Correlation-Id it sent when it differs
// from the leg's id) and the verified test-endpoint call id (CallID); no
// other header value.
//
// The JSON names are a contract with the network's stats service; decode
// tolerantly, since fields and closed-set values may be added.
type AccessLine struct {
	// Time is when the gateway received the call.
	Time time.Time `json:"time"`
	// CorrelationID is the leg's id (X-SHN-Leg-Id, the envelope's
	// correlationId); Trace is the caller's own X-Correlation-Id when it
	// differs; CallID is the pa-test door's verified call id.
	CorrelationID string `json:"correlationId,omitempty"`
	Trace         string `json:"trace,omitempty"`
	CallID        string `json:"callId,omitempty"`
	// Direction is "ingress" (a call from the gateway's own participant) or
	// "inbound" (a leg from the network).
	Direction string `json:"direction"`
	Route     string `json:"route"`
	// Exchange is the leg's transaction type, or "other".
	Exchange     string `json:"exchange"`
	Operation    string `json:"operation,omitempty"`
	ContractLine string `json:"contractLine,omitempty"`
	Sender       string `json:"sender,omitempty"`
	Recipient    string `json:"recipient,omitempty"`
	// RequestCiphertextHash and ResponseCiphertextHash equal the Hub audit
	// records' payloadBundleHash for the same leg.
	RequestCiphertextHash  string `json:"requestCiphertextHash,omitempty"`
	ResponseCiphertextHash string `json:"responseCiphertextHash,omitempty"`
	// Outcome is answered, refused, unreachable, upstream-error or other.
	Outcome string         `json:"outcome"`
	Refusal *AccessRefusal `json:"refusal,omitempty"`
	// Status is the status the requester received.
	Status    int            `json:"status"`
	LatencyMs int64          `json:"latencyMs"`
	Backend   *AccessBackend `json:"backend,omitempty"`
	Findings  AccessFindings `json:"findings"`
}

// AccessRefusal says who refused a call and on which network rule.
type AccessRefusal struct {
	By   string `json:"by"`
	Rule string `json:"rule"`
}

// AccessBackend is the call to the participant's own system the answer came
// from: the operation forwarded to it or, where none was, the last read
// (engine.ExchangeRecord.Backend); Calls counts every call made.
type AccessBackend struct {
	Status     int    `json:"status"`
	LatencyMs  int64  `json:"latencyMs"`
	ErrorClass string `json:"errorClass,omitempty"`
	Calls      int    `json:"calls"`
}

// AccessFindings summarizes the conformance findings a call's checks recorded.
type AccessFindings struct {
	Count   int      `json:"count"`
	Refused bool     `json:"refused"`
	Kinds   []string `json:"kinds,omitempty"`
}
