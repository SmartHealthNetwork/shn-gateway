package diagnostics

import "time"

// AccessLine is the one line a gateway writes for each call it answers on a Da
// Vinci ingress route or on one of module 1's origination routes, or for a leg
// it receives from the network
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
	// Direction is "ingress" (a call from the gateway's own participant, on
	// a Da Vinci ingress route or a module 1 origination route) or "inbound"
	// (a leg from the network).
	Direction string `json:"direction"`
	Route     string `json:"route"`
	// Exchange is the leg's transaction type, or "other".
	Exchange     string `json:"exchange"`
	Operation    string `json:"operation,omitempty"`
	ContractLine string `json:"contractLine,omitempty"`
	// AnswerLine is, on an ingress call, the contract token of the IG line
	// the answer this gateway wrote back to its own participant is written in
	// ("pa.pas@2.2"): the routed line when the counterpart's answer was carried
	// as sent, or the line a version bridge wrote it at. A reader of
	// retained evidence judges a line-dependent answer at it. Absent on an
	// inbound leg, and from a gateway that does not record it; a reader then
	// keeps its own reading of the line (the leg's seal).
	AnswerLine string `json:"answerLine,omitempty"`
	// LineCarried says where the leg was carried between two IG lines with
	// nothing translated, the network's default from shn-gateway v0.62.0
	// (engine.LineCarriedValues): "request" when the request went to, or came
	// from, a participant that shares no declared line with this one;
	// "answer" when the answer was stamped with another line than the leg
	// routed. Absent when it was not.
	LineCarried []string `json:"lineCarried,omitempty"`
	Sender      string   `json:"sender,omitempty"`
	Recipient   string   `json:"recipient,omitempty"`
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
	// Network is the time an ingress call waited on the Hub and its peer
	// (engine.ExchangeRecord.Network); absent on an inbound leg and on an
	// ingress call that handed nothing to the transport for the Hub.
	// Evolving.
	Network *AccessNetwork `json:"network,omitempty"`
	// AnswerError is why an inbound leg's answer (the participant's, or the
	// gateway's own refusal) could not be authorized (engine.AnswerErrorClasses):
	// cancelled, timeout, auth, unreachable, redirect-refused or other;
	// absent otherwise.
	AnswerError string `json:"answerError,omitempty"`
	// AuthorizeError is why no authority could be obtained for a request leg
	// this gateway was sending.
	AuthorizeError string `json:"authorizeError,omitempty"`
	// InvolvedOmitted counts, by reason, the patients this call's involved
	// lists left out: the exchange has no record under them
	// (engine.InvolvedOmitReasons). Absent when none was.
	InvolvedOmitted map[string]int `json:"involvedOmitted,omitempty"`
	Findings        AccessFindings `json:"findings"`
	// RelayEdits are the ids of the registered edits (E-01 …) the gateway
	// applied to the bytes it sent on the leg, in transmit order, each once,
	// named only once the bytes were sent (engine.ExchangeRecord.Edits). A
	// request the gateway built names first an edit made to a record its
	// build carries, before the build (E-06); the request is still the
	// builder's own. Ids only: never a value an edit
	// removed or wrote. Absent when the leg carried its message exactly.
	RelayEdits []string `json:"relayEdits,omitempty"`
	// Ownership is whose message the request the gateway sent on the leg was
	// (engine.ExchangeRecord.Ownership): relayed, edited or authored; it says
	// nothing of the answer, though RelayEdits may name an answer's edit
	// (E-08). Builder is the registered builder of a request the gateway built:
	// authored, or edited when the registered transform (E-09) carried it to
	// the peer's IG line. Names only. Both are absent when no request was sent,
	// and Builder unless the request was the gateway's own. Evolving.
	Ownership string `json:"ownership,omitempty"`
	Builder   string `json:"builder,omitempty"`
	// RequestEdits are the RelayEdits applied to the request Ownership
	// describes, and AnswerEdits those applied to the answer the gateway
	// wrote (engine.ExchangeRecord.RequestEdits, AnswerEdits): RelayEdits is
	// the two together. AnswerOwnership and AnswerBuilder are Ownership and
	// Builder for that answer (engine.ExchangeRecord.AnswerOwnership,
	// AnswerBuilder): authored by gateway-refusal for every refusal the
	// gateway writes. Each is absent when empty. Evolving.
	RequestEdits    []string `json:"requestEdits,omitempty"`
	AnswerOwnership string   `json:"answerOwnership,omitempty"`
	AnswerBuilder   string   `json:"answerBuilder,omitempty"`
	AnswerEdits     []string `json:"answerEdits,omitempty"`
	// EditsNotMade names each registered edit the gateway would make that
	// it could not make on the call, with why
	// (engine.ExchangeRecord.EditsNotMade, engine.EditNotMadeValues): the
	// bytes it would have changed were carried as sent. It also names a
	// statement the requester made for an amendment this gateway built that
	// the amendment does not carry. Absent when empty. Evolving.
	EditsNotMade []string `json:"editsNotMade,omitempty"`
	// AnswerHeadersNotCarried names each header of a participant's 2xx answer
	// that could not be carried, with why
	// (engine.ExchangeRecord.AnswerHeadersNotCarried), never its value.
	AnswerHeadersNotCarried []string `json:"answerHeadersNotCarried,omitempty"`
	// ReadsFailed is the error class of each read of the participant's system
	// of record that failed while the answer was still built from what could
	// be read, one per failed read, in read order
	// (engine.ExchangeRecord.ReadsFailed, engine.BackendErrorClasses). Absent
	// when none failed. Never a URL, an identifier or text. Evolving.
	ReadsFailed []string `json:"readsFailed,omitempty"`
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

// AccessNetwork is the time an ingress call waited on the network, in
// milliseconds: from handing each sealed request to the transport for the Hub
// to reading the Hub's answer, timed once while requests overlap, so it is at
// most LatencyMs; the rest is the gateway's own time. Calls counts the
// requests handed to the transport.
type AccessNetwork struct {
	LatencyMs int64 `json:"latencyMs"`
	Calls     int   `json:"calls"`
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

// AccessRefusal says who refused a call and on which network rule, and, for
// a fidelity refusal, its stage (engine.ExchangeRecord.RefusalStage: build or
// verify); absent for every other rule. Reason is the gateway's own fixed
// refusal sentence for a refusal it made before the request reached the Hub
// (engine.ExchangeRecord.RefusalReason), absent otherwise: never a value from
// the request. Its wording may change in a minor release; decide by Rule.
type AccessRefusal struct {
	By     string `json:"by"`
	Rule   string `json:"rule"`
	Stage  string `json:"stage,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// AccessBackend is the call to the participant's own system the answer came
// from: the operation forwarded to it or, where none was, the last read
// (engine.ExchangeRecord.Backend); Calls counts every call made.
type AccessBackend struct {
	Status     int    `json:"status"`
	LatencyMs  int64  `json:"latencyMs"`
	ErrorClass string `json:"errorClass,omitempty"`
	Calls      int    `json:"calls"`
	// Resends names each call to the participant's own system sent a second
	// time, and why (engine.ExchangeRecord.BackendResends); Calls counts each.
	Resends []AccessResend `json:"resends,omitempty"`
	// Redirects is the class of each redirect hop the participant's own system,
	// or its authorization server's token endpoint, answered with, in order
	// (engine.ExchangeRecord.BackendRedirects), never a URL: "same-origin" or
	// "cross-origin" for a hop followed (a cross-origin one, and every later
	// hop of its chain, without the bearer or configured headers),
	// "https-to-http-refused" for a downgrade that was not sent,
	// "token-redirect-refused" for any other redirect of a token request,
	// which is never followed, and "cross-origin-refused" for a redirect to
	// another origin of a request that carries a body, never followed.
	Redirects []string `json:"redirects,omitempty"`
}

// AccessResend is one call to the participant's own system sent a second time.
// Reason is engine.BackendResendReasons' value (today only
// "credential-refreshed-after-401": its system answered 401 to the credential
// the call carried, and the same request was sent once more with a refreshed
// one). Challenge is what the 401's WWW-Authenticate said, as observed and never
// used to decide: "invalid_token", "other" or "none".
type AccessResend struct {
	Reason    string `json:"reason"`
	Challenge string `json:"challenge"`
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
