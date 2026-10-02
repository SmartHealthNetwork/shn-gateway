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
	// Stages is the time an inbound leg's answer spent in each stage
	// (engine.LegStages); absent on an ingress call.
	Stages *AccessStages `json:"stages,omitempty"`
	// AnswerError is why an inbound leg's answer (the participant's, or the
	// gateway's own refusal) could not be authorized (engine.AnswerErrorClasses):
	// cancelled, timeout, auth, unreachable or other; absent otherwise.
	AnswerError string         `json:"answerError,omitempty"`
	Findings    AccessFindings `json:"findings"`
	// RelayEdits are the ids of the registered edits (E-01 …) the gateway
	// applied to the bytes it sent on the leg, in transmit order, each once,
	// named only once the bytes were sent (engine.ExchangeRecord.Edits). Ids only: never a value an edit
	// removed or wrote. Absent when the leg carried its message exactly.
	RelayEdits []string `json:"relayEdits,omitempty"`
	// HubDelivered is the Hub's own word, on its error answer to an ingress
	// call's leg, on whether the recipient's gateway received the request
	// (engine.ExchangeRecord.HubDelivered): "no", "yes" or "unknown". Absent
	// when the leg drew no error answer from the Hub, and on an inbound leg.
	HubDelivered string `json:"hubDelivered,omitempty"`
}

// AccessStages is an inbound leg's time in each stage of its answer, in
// milliseconds: durations only. The stages do not overlap, and their sum is at
// most LatencyMs; the rest is the leg's own handling between them.
type AccessStages struct {
	UnwrapMs   int64 `json:"unwrapMs"`
	ReadsMs    int64 `json:"readsMs"`
	ForwardMs  int64 `json:"forwardMs"`
	ValidateMs int64 `json:"validateMs"`
	SealMs     int64 `json:"sealMs"`
	LedgerMs   int64 `json:"ledgerMs"`
	WriteMs    int64 `json:"writeMs"`
}

// RelayEditsDetail is the Detail of a captured transmit that applied
// registered edits: a provider gateway's "leg.sealed" (the request it sealed
// for the network) and a payer gateway's "native.request" (its forward to its
// own system). It names the edit ids the request was built with, captured
// before the send, so it may name edits the access line does not (a request
// then never sent); a transmit that carried its message exactly has no
// Detail.
type RelayEditsDetail struct {
	RelayEdits []string `json:"relayEdits,omitempty"`
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
// At observe the count is not carried: Deferred is true, Count is always 0 and
// Kinds empty, and each finding is its own KindConformanceFinding event, keyed by
// the leg's correlation id, because a check at observe does not hold the call and
// a count taken when the call ends would be partial. Refused is still carried: a
// check that refuses is judged before the answer at every level.
type AccessFindings struct {
	Count    int      `json:"count"`
	Refused  bool     `json:"refused"`
	Kinds    []string `json:"kinds,omitempty"`
	Deferred bool     `json:"deferred,omitempty"`
}
