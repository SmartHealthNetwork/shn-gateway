package engine

// exchangerecord.go — the exchange record seam.
//
// Every call this gateway answers on a Da Vinci ingress route, on a route a
// compiled-in module originates exchanges from (gatewayModule.exchangeRoutes:
// on-behalf building's /scenario origination routes) or on
// /substrate/inbound produces exactly one ExchangeRecord, handed to
// Config.ExchangeObserved once the answer is written: on success, on every
// refusal (the earliest authentication refusal included) and when the handler
// panics. The record is metadata only — ids, parties, the leg, the outcome and
// its timing — never a message body or a patient identifier. The only header
// value it carries is the caller's own X-Correlation-Id, which the ingress
// accepts only as one bounded token; an id taken from a message (a Claim's own
// correlation) is recorded as its sha256 digest unless it is one bounded token
// too (boundedID).
//
// Every value a record can carry in Direction, Route, Exchange, Outcome,
// RefusedBy, Rule and Backend.ErrorClass comes from the closed lists below, so
// a consumer may use them as metric dimensions: anything the engine cannot
// classify is recorded as "other", never as a caller's string.
//
// The facts are noted where the engine learns them (the leg's round trip, the
// response leg it builds, the call to its participant's own system, a check
// that refuses, the registered edits a transmit applied) on a recorder the wrapper puts on the request context. A
// recorder that is absent (ExchangeObserved unset, or a handler driven without
// the wrapper) makes every note a no-op.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/serviceclient"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// ExchangeRecord is one call this gateway answered, as it saw it.
type ExchangeRecord struct {
	// Start is when this gateway received the call (Config.Clock).
	Start time.Time
	// CorrelationID is the leg's id: the X-SHN-Leg-Id an ingress answer
	// carries, the first leg's correlation id on a module's origination
	// route, or the envelope correlationId of an inbound leg, as boundedID
	// records it. Empty when the call was refused before it was settled (an
	// inbound leg refused before its envelope was verified).
	CorrelationID string
	// Trace is the caller's own X-Correlation-Id when it differs from
	// CorrelationID (ingress only).
	Trace string
	// CallID is the pa-test door's call id, from a verified X-SHN-Test-Trace
	// proof (ingress only; empty elsewhere).
	CallID string
	// Direction is DirectionIngress (a call from this gateway's own
	// participant, a module's origination route's included: the request
	// enters from the participant's own side and this gateway sends the leg)
	// or DirectionInbound (a leg from the network).
	Direction string
	// Route is the route the call arrived on (ExchangeRoutes).
	Route string
	// Exchange is the leg's transaction type (ExchangeKinds), or ExchangeOther
	// when the call was answered before a leg was known. On an inbound leg it
	// is read from the envelope before the leg's token is verified; it is a
	// closed value either way. A call that sends more than one leg (a
	// module's origination) records the first leg it sent.
	Exchange string
	// Operation is the CDS hook or the DTR operation the leg carries, when it
	// names one (ExchangeOperations; empty when it names none). Not a metric
	// dimension.
	Operation string
	// ContractLine is the IG line the leg ran at, when one was settled.
	ContractLine string
	// AnswerLine is, on an ingress call, the contract token of the IG line the
	// answer this gateway wrote back to its own participant is written in: the
	// routed line when the answer was carried as sent, or the line a bridge
	// wrote it at (E-08). Recorded by the DTR ingress; "" elsewhere.
	AnswerLine string
	// Sender and Recipient are the leg's holder ids. This gateway's own is
	// always set (the sender of an ingress call, the recipient of an inbound
	// leg); the other side's is set once the leg is verified, so an inbound
	// leg refused before that has no Sender.
	Sender, Recipient string
	// RequestCiphertextHash and ResponseCiphertextHash are sha256 hex of the
	// request and answer envelopes' ciphertext: the Hub's audit records carry
	// the same value as payloadBundleHash, so they join a record to the Hub's.
	RequestCiphertextHash, ResponseCiphertextHash string
	// Outcome is the call's outcome (ExchangeOutcomes). An ingress call is
	// seen from the requester's side: a non-2xx answer from past this
	// gateway's own leg is ExchangeUpstreamError whichever side produced it;
	// the answering gateway's inbound record says which.
	Outcome string
	// RefusedBy and Rule say who refused and on which network rule
	// (RefusalParties, RefusalRules); empty unless Outcome is ExchangeRefused.
	// A facility's or a PHG's gateway refusing an inbound leg is recorded as
	// ExchangeOther. RefusalLimit is recorded for a coverage-eligibility batch
	// over its bound (maxEligibilityBatch), and from v0.58.0 the gateway records an
	// Authorization Framework denial as RefusalAuthority, whatever its reason.
	RefusedBy, Rule string
	// RefusalStage is where a fidelity refusal (Rule RefusalFidelity) was
	// made (recording.RefusalStages): recording.RefusalStageBuild, SHN
	// refusing to build the message the refusal is about because of what the
	// caller declared or did not state (earlier legs of the call may have been
	// sent); recording.RefusalStageVerify, an edit or message this gateway
	// built failing its own verification. Each site that refuses on fidelity
	// names its stage; empty for every other rule and outcome.
	RefusalStage string
	// RefusalReason is the reason a refusal this gateway made of its
	// participant's call, before the request it refused reached the Hub, gave
	// its participant (on a module's origination route, an earlier leg of the
	// same call may have been sent): the gateway's own fixed refusal
	// sentence, from the closed set refusalReasons, never a value read from
	// the request, its body, a credential or a patient. Where the answer adds
	// a detail read from the request (an identifier, a member name), the
	// record keeps the sentence without it. A reason outside the set is
	// recorded as ExchangeOther. Empty unless Outcome is ExchangeRefused and
	// the refusing site named its reason (authentication and scope at an
	// ingress, message integrity, authority read from the body, routing with
	// no recipient, size). Not a metric dimension; a minor release may reword
	// or add a sentence, so decide by Rule, never by this text.
	RefusalReason string
	// Status is the status the requester received: the HTTP status of an
	// ingress answer; the application status inside an inbound answer's frame
	// (the HTTP status to the Hub when the answer was not framed).
	Status int
	// Latency is Start to the handler returning, the answer written.
	Latency time.Duration
	// Backend is the call to this gateway's participant's own system the
	// answer came from: the operation forwarded to it or, where none was, the
	// last read of its system of record (a read after the operation does not
	// replace it); nil when its system was not called. BackendCalls counts
	// every call made.
	Backend      *BackendCall
	BackendCalls int
	// BackendResends names, for each call to the participant's own system its
	// connector sent a second time, why it did (a BackendResend): the system
	// answered 401 to the credential the call carried, and the call was sent
	// once more, the same request with a refreshed credential, as a direct
	// client would. BackendCalls counts each resend as a call. Nil when there
	// was none.
	BackendResends []BackendResend
	// BackendRedirects is the class of each redirect hop the participant's
	// own system, or its authorization server's token endpoint, answered
	// with, over every call to it, in order (smartauth.RedirectValues; never a
	// URL): same-origin and cross-origin for a hop followed (a cross-origin
	// hop, and every later hop of its chain, is sent without the bearer or
	// configured headers), https-to-http-refused for a downgrade that was not
	// sent, which ends its call (a read fails with BackendHTTP3xx; the
	// forward is refused on message integrity; a token request fails as a
	// token failure), token-redirect-refused for any other redirect of a
	// token request, never followed, which fails it as a token failure, and
	// cross-origin-refused for a redirect to another origin of a request that
	// carries a body, never followed (the forward is refused on routing, as
	// delivered; $populate fails at its transport). Nil when no answer asked
	// for one.
	BackendRedirects []string
	// Stages is the time an inbound leg spent in each stage of its answer
	// (LegStages); nil on an ingress call.
	Stages *LegStages
	// Network is the time an ingress call waited on the network (MON-R22):
	// from handing each sealed request to the transport for the Hub to
	// reading the Hub's answer, or the transport's failure, so the Hub's and
	// the peer's time together. It is wall time with at least one request
	// outstanding, so it never exceeds Latency, and Latency less it is this
	// gateway's own time. Nil on an inbound leg and on an ingress call that
	// handed nothing to the transport for the Hub.
	Network *NetworkWait
	// RequestBytes and ResponseBytes are the sizes of the HTTP bodies of the
	// call itself (MON-R23): on an ingress call, its participant's request and
	// the answer written to it; on an inbound leg, the envelope from the Hub
	// and the answer written back. RequestBytes is nil when the body was not
	// read to its end (refused unread, or cut off at a limit), since its size
	// is then not known; ResponseBytes is nil when the handler panicked.
	RequestBytes, ResponseBytes *int64
	// AnswerError is why an inbound leg's answer (the participant's, or this
	// gateway's own refusal) could not be authorized (AnswerErrorClasses);
	// empty otherwise.
	AnswerError string
	// AuthorizeError is why no authority could be obtained for a request leg
	// this gateway was sending (AnswerErrorClasses); empty otherwise.
	AuthorizeError string
	// InvolvedOmitted counts, by reason (InvolvedOmitReasons), the patients this
	// call's involved lists left out: the exchange has no record under them.
	// Nil when none was.
	InvolvedOmitted map[string]int
	// Findings summarizes the conformance findings the call's checks recorded.
	Findings FindingSummary
	// Edits are the ids of the registered edits (relay.EditIDs: E-01 …) this
	// gateway applied to the bytes it transmitted on the leg, in transmit
	// order, each once: on an ingress call, the request it sealed for the
	// network; on an inbound leg, its forward to its participant's own
	// system. Only registered ids, and only for bytes sent (the receiver
	// answered, with anything, or the connection failed after the request was
	// written): a request refused before it was sent, or whose connection or
	// bearer could not be obtained, names none, nor does one carried exactly.
	// A request this gateway built itself names first the edits made to a
	// participant's record the build carries, before it was built (E-06; see
	// Ownership). It also names the edits to the answer this gateway wrote
	// (E-08): RequestEdits and AnswerEdits are the same ids by body. Metadata only
	// (never a value an edit removed or wrote); not a metric dimension.
	Edits []string
	// Ownership is whose message the request this gateway sent on the leg is
	// (ExchangeOwnerships): on an ingress call, the request it sealed for the
	// network; on an inbound leg, its forward to its participant's own system.
	// relayed, the participant's own byte for byte; edited, the participant's
	// with registered edits; authored, this gateway's own, from a registered
	// builder (Builder), as a registered module ingress client's request is;
	// edited with Builder set, this gateway's own carried to the peer's IG line
	// by the registered transform (E-09 in Edits). Either may carry a
	// participant's record a registered edit changed before the build (E-06):
	// the message is still the builder's, and RequestEdits names the edit.
	// Set once that request is sent, empty when none was. It says nothing of
	// the answer, which AnswerOwnership describes: Edits may also name an edit
	// to the answer (E-08). Content-free; Evolving until the Audit Plane
	// carries it.
	Ownership string
	// Builder is the registered builder (relay.Builders) of this gateway's own
	// message: authored, or edited by the registered transform (E-09); empty
	// otherwise.
	Builder string
	// RequestEdits are the Edits this gateway applied to the request
	// Ownership describes, in transmit order, each once; AnswerEdits are those
	// it applied to the answer it wrote. Edits is the two together. Evolving.
	RequestEdits, AnswerEdits []string
	// AnswerOwnership is whose message the answer this gateway wrote on the
	// call is (ExchangeOwnerships): on an ingress call, the answer it wrote to
	// its participant's system; on an inbound leg, the answer it sealed for
	// the network, or wrote bare to a requester without a message frame.
	// relayed, the participant's (or the peer's) own byte for byte; edited,
	// theirs with registered edits (AnswerEdits); authored, this gateway's
	// own, from a registered builder (AnswerBuilder), as every refusal it
	// writes is, one written before a leg was opened included. Set once that
	// answer is written, empty when none was. Content-free; Evolving until the
	// Audit Plane carries it.
	AnswerOwnership string
	// AnswerBuilder is the registered builder of the answer when it is this
	// gateway's own, as Builder is of the request; empty otherwise.
	AnswerBuilder string
	// LineCarried says where the leg was carried between two IG lines with
	// nothing translated, the network's default (LineCarriedValues, each at
	// most once): LineCarriedRequest when the request went to, or came from,
	// a participant that shares no declared line with this one, a pend's
	// follow-up was carried at another line than the pend is pinned at, or an
	// adaptive questionnaire's round was framed at another line than the
	// address it was delivered at names;
	// LineCarriedAnswer when the answer was stamped with another line than
	// the leg routed. Empty when it was not. Not a metric dimension.
	LineCarried []string
	// EditsNotMade names each registered edit the gateway would make that it
	// could not make on the call, with why (EditNotMadeValues, each
	// at most once): the bytes it would have changed were carried as sent.
	// It also names a statement the requester made for an amendment this
	// gateway built that the amendment does not carry.
	// Empty when every such edit was made or had nothing to change.
	// Content-free; not a metric dimension. Evolving.
	EditsNotMade []string
	// AnswerHeadersNotCarried names each header of a participant's 2xx answer
	// that could not be carried, with why (AnswerHeaderNotCarriedValues, each
	// at most once): never its value. The answer goes through without it,
	// unless it is refused for its own size.
	// Content-free; not a metric dimension. Evolving.
	AnswerHeadersNotCarried []string
	// ReadsFailed is the error class (BackendErrorClasses) of each read of the
	// participant's system of record that failed while the answer was still
	// built from what could be read, one per failed read, in read order: what
	// that read would have given is unknown, and recorded so. Nil when no such
	// read failed, never empty. Backend keeps its meaning (the call the answer
	// came from, or the last read). Content-free: never a URL, an identifier
	// or text. Not a metric dimension. Evolving.
	ReadsFailed []string
	// HubDelivered is the Hub's own word on whether the recipient's gateway
	// received an ingress call's leg, read from the HubDeliveredHeader of the
	// Hub's error answer to the leg: "no", "yes" or "unknown". Empty when the
	// leg drew no error answer from the Hub's route handler (it was answered,
	// it never reached the Hub, or the error came from in front of it): this
	// gateway never infers it. Always empty on an inbound leg.
	HubDelivered string
}

// LegStages is the time an inbound leg spent in each stage of its answer,
// durations only. The stages do not overlap; what is not in one (dispatch,
// framing, fencing) is the rest of the call's Latency.
type LegStages struct {
	// Unwrap is verifying the Hub's assertion and the leg's authority and
	// opening the request envelope, up to the leg's dispatch (for a leg
	// refused before it, the call's time in no other stage).
	Unwrap time.Duration
	// Reads is every read of the participant's own system (its system of
	// record, its CDS service listing); Forward is every call forwarding the
	// operation to it (BackendCalls counts both).
	Reads, Forward time.Duration
	// Validate is the $validate checks the answer waits for, and queueing an
	// observe check it does not wait for (its own time is the finding's
	// ValidatorMs).
	Validate time.Duration
	// Seal is sealing the answer and authorizing it with the Authorization
	// Framework, the involved patients' tokens and its diagnostic capture
	// included.
	Seal time.Duration
	// Ledger is the pend ledger's work for the answer: its lookups, the claim
	// an update takes (BeginClaimUpdate) and its release when the update is
	// not recorded, an inquiry's effect derived from the payer's answer, and
	// its writes.
	Ledger time.Duration
	// Write is handing the answer to the HTTP server.
	Write time.Duration
}

// NetworkWait is the time an ingress call waited on the Hub and its peer.
type NetworkWait struct {
	Latency time.Duration
	// Calls counts the requests handed to the transport for the Hub.
	Calls int
}

// BackendCall is one call to the participant's own system.
type BackendCall struct {
	// Status is the HTTP status its system answered; 0 when it gave none.
	Status  int
	Latency time.Duration
	// ErrorClass is "" for a usable 2xx answer, else one of BackendErrorClasses.
	ErrorClass string
}

// BackendResend is one call to the participant's own system sent a second
// time. Reason is one of BackendResendReasons. Challenge is what the 401's
// WWW-Authenticate said, as observed (smartauth.ChallengeValues): it is
// recorded, never used to decide whether to resend.
type BackendResend struct {
	Reason    string
	Challenge string
}

// BackendResendCredentialRefreshed: the participant's system answered 401 to
// the credential the call carried, and the call was sent once more with a
// refreshed one.
const BackendResendCredentialRefreshed = "credential-refreshed-after-401"

// BackendResendReasons is the closed list BackendResend.Reason draws from.
var BackendResendReasons = []string{BackendResendCredentialRefreshed}

// FindingSummary counts the conformance findings recorded for one call.
type FindingSummary struct {
	Count   int
	Refused bool
	// Kinds are the distinct CheckKind values recorded, in first-seen order.
	Kinds []string
	// Deferred says the call's findings are not counted here: at observe a
	// payload check runs off the request path, so a count taken when the call
	// ends would be partial. Count and Kinds are then empty, and each finding is
	// captured as its own event, keyed by the leg's correlation id. Refused stays
	// meaningful: a check that refuses is judged inline at every level.
	Deferred bool
}

// ExchangeOther is the value recorded for anything a closed list does not name.
//
// Deprecated: use recording.ExchangeOther, which this is; a later minor release removes it.
const ExchangeOther = vocab.ExchangeOther

// Directions.
//
// Deprecated: use package recording's names, which these are; a later minor release removes them.
const (
	DirectionIngress = vocab.DirectionIngress
	DirectionInbound = vocab.DirectionInbound
)

// RouteSubstrateInbound is the route the network delivers every leg to. A
// workstream's own ingress routes are its adapter's (the PA workstream's in
// workstream_pa.go).
const RouteSubstrateInbound = "substrate-inbound"

// Outcomes.
//
// Deprecated: use package recording's names, which these are; a later minor release removes them.
const (
	ExchangeAnswered      = vocab.ExchangeAnswered
	ExchangeRefused       = vocab.ExchangeRefused
	ExchangeUnreachable   = vocab.ExchangeUnreachable
	ExchangeUpstreamError = vocab.ExchangeUpstreamError
)

// redirectRefused is recording.RedirectRefused: an answer error class
// (AnswerErrorClasses) for an authorize call answered with a redirect, which
// is not followed.
const redirectRefused = vocab.RedirectRefused

// Refusing parties.
//
// Deprecated: use package recording's names, which these are; a later minor release removes them.
const (
	RefusedByProviderGateway        = vocab.RefusedByProviderGateway
	RefusedByHub                    = vocab.RefusedByHub
	RefusedByAuthorizationFramework = vocab.RefusedByAuthorizationFramework
	RefusedByPayerGateway           = vocab.RefusedByPayerGateway
)

// Refusal rules.
//
// Deprecated: use package recording's names, which these are; a later minor release removes them.
const (
	RefusalAuthentication = vocab.RefusalAuthentication
	RefusalAuthority      = vocab.RefusalAuthority
	RefusalConsent        = vocab.RefusalConsent
	RefusalRouting        = vocab.RefusalRouting
	RefusalReplay         = vocab.RefusalReplay
	RefusalIntegrity      = vocab.RefusalIntegrity
	RefusalAudit          = vocab.RefusalAudit
	RefusalFidelity       = vocab.RefusalFidelity
	RefusalConformance    = vocab.RefusalConformance
	RefusalLimit          = vocab.RefusalLimit
)

// Backend error classes.
//
// Deprecated: use package recording's names, which these are; a later minor release removes them.
const (
	BackendTimeout   = vocab.BackendTimeout
	BackendConnect   = vocab.BackendConnect
	BackendTLS       = vocab.BackendTLS
	BackendAuth      = vocab.BackendAuth
	BackendHTTP3xx   = vocab.BackendHTTP3xx
	BackendHTTP4xx   = vocab.BackendHTTP4xx
	BackendHTTP5xx   = vocab.BackendHTTP5xx
	BackendRead      = vocab.BackendRead
	BackendMalformed = vocab.BackendMalformed
	BackendCancelled = vocab.BackendCancelled
)

// The closed lists. Each ends in ExchangeOther. The directions, outcomes,
// refusing parties, rules and error classes are package recording's, each
// deprecated here; the routes, kinds and operations are the compiled-in
// workstreams'.
var (
	// Deprecated: use recording.ExchangeDirections, which this is; a later minor release removes it.
	ExchangeDirections = vocab.ExchangeDirections
	// ExchangeRoutes are the ingress routes the compiled-in workstreams
	// declare, the routes the compiled-in modules originate exchanges from
	// (gatewayModule.exchangeRoutes), and the core's inbound route (set in
	// init).
	ExchangeRoutes []string
	// ExchangeOriginationRoutes are the routes of ExchangeRoutes that the
	// compiled-in modules originate exchanges from: on-behalf building's
	// originating /scenario routes, each recorded with direction ingress
	// (set in init).
	ExchangeOriginationRoutes []string
	// ExchangeKinds is every leg of the compiled-in workstreams' catalogs
	// (set in init).
	ExchangeKinds []string
	// ExchangeOperations are the operations the compiled-in workstreams'
	// legs name: those their ingress records name (the CDS hooks of the CRD
	// services) and the frame operations their taxonomies declare (set in
	// init).
	ExchangeOperations []string
	// Deprecated: use recording.ExchangeOutcomes, which this is; a later minor release removes it.
	ExchangeOutcomes = vocab.ExchangeOutcomes
	// Deprecated: use recording.RefusalParties, which this is; a later minor release removes it.
	RefusalParties = vocab.RefusalParties
	// Deprecated: use recording.RefusalRules, which this is; a later minor release removes it.
	RefusalRules = vocab.RefusalRules
	// Deprecated: use recording.BackendErrorClasses, which this is; a later minor release removes it.
	BackendErrorClasses = vocab.BackendErrorClasses
	// AnswerErrorClasses say why an inbound leg's answer, or a request leg (its
	// AuthorizeError), could not be authorized: the request ended before it
	// (cancelled: the requester was gone), the authorize call ran out of time
	// (timeout), the Authorization Framework denied it (auth), never answered
	// it (unreachable) or answered it with a redirect, which is not followed
	// (redirect-refused), or another failure (other).
	//
	// Deprecated: use recording.AnswerErrorClasses, which this is; a later minor release removes it.
	AnswerErrorClasses = vocab.AnswerErrorClasses
)

// init derives ExchangeRoutes, ExchangeOriginationRoutes, ExchangeKinds and
// ExchangeOperations from the compiled-in workstreams and modules. It runs after every package-level variable is
// set: the workstreams' ingress handlers record exchanges against these
// lists, so the lists cannot be initializers of their own without an
// initialization cycle.
func init() {
	ExchangeOriginationRoutes = moduleExchangeRouteNames()
	ExchangeRoutes = slices.Concat(workstreamIngressRouteNames(), ExchangeOriginationRoutes, []string{RouteSubstrateInbound, ExchangeOther})
	ExchangeKinds = append(workstreamKinds(), ExchangeOther)
	ExchangeOperations = append(workstreamOperations(), ExchangeOther)
}

// closed returns v when list names it, else ExchangeOther.
func closed(list []string, v string) string {
	if slices.Contains(list, v) {
		return v
	}
	return ExchangeOther
}

type exchangeRecorderKey struct{}

// exchangeRecorder collects one call's facts. Every method is nil-safe and
// goroutine-safe (a leg's work may run on more than one goroutine).
type exchangeRecorder struct {
	mu  sync.Mutex
	rec ExchangeRecord
	// tally follows the leg's findings at observe (module3_finding_tally.go); nil at
	// every other level.
	tally *legTally
	// outcome/party/rule are the settled outcome, set by decided;
	// refusalStage is a settled fidelity refusal's stage; reason is a settled
	// refusal's reason (refusedFor).
	outcome, party, rule, refusalStage string
	reason                             refusalReason
	// appStatus is the status inside an inbound answer's frame.
	appStatus int
	// guardRule is the rule of a check that refused after the last decided
	// outcome; it names the refusal when the answer written is non-2xx.
	// guardStage is its stage when the rule is fidelity, and guardReason its
	// reason when the site named one (refusingFor).
	guardRule, guardStage string
	guardReason           refusalReason
	// self is the party this gateway is.
	self string
	// attested is set once a refusal has been attested to the Hub: the outcome
	// it settled is final (refusedAttested).
	attested bool
	// forwarded is set once the operation has been forwarded to the
	// participant's own system (backend); reads after it do not replace it.
	forwarded bool
	// forwardTracked says every forward of the operation to the participant's
	// own system goes through backend: set by the built-in native responder,
	// whose forward is recorded; a connector's own calls are not.
	forwardTracked bool
	// cut is set when any call to the participant's system was cut short
	// because the request ended, the Backend or a read after it.
	cut bool
	// now is the gateway's clock.
	now func() time.Time
	// unwrapped is set once the leg's Unwrap stage has ended.
	unwrapped bool
	// validating counts the synchronous validations in progress, so a
	// nested one is timed once.
	validating int
	// hubOut counts the requests to the Hub outstanding, since hubSince.
	hubOut   int
	hubSince time.Time
}

func exchangeOf(ctx context.Context) *exchangeRecorder {
	x, _ := ctx.Value(exchangeRecorderKey{}).(*exchangeRecorder)
	return x
}

func (x *exchangeRecorder) with(f func()) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	f()
}

// leg notes the leg's transaction type.
func (x *exchangeRecorder) leg(txType string) {
	x.with(func() { x.rec.Exchange = closed(ExchangeKinds, txType) })
}

// sending notes the transaction type and correlation id of a leg this
// gateway sends, each unless the call's is already noted: an ingress handler
// notes its leg itself (and its answer names the leg's id, which the record
// takes), and a call that sends more than one leg (a module's origination)
// records the first leg's. A refusal recorded later may be a later leg's.
func (x *exchangeRecorder) sending(txType, correlationID string) {
	x.with(func() {
		if x.rec.Exchange == ExchangeOther {
			x.rec.Exchange = closed(ExchangeKinds, txType)
		}
		if x.rec.CorrelationID == "" {
			x.rec.CorrelationID = correlationID
		}
	})
}

func (x *exchangeRecorder) operation(op string) {
	x.with(func() {
		if op != "" {
			op = closed(ExchangeOperations, op)
		}
		x.rec.Operation = op
	})
}
func (x *exchangeRecorder) line(l string)    { x.with(func() { x.rec.ContractLine = l }) }
func (x *exchangeRecorder) callID(id string) { x.with(func() { x.rec.CallID = id }) }
func (x *exchangeRecorder) trace(t string)   { x.with(func() { x.rec.Trace = t }) }

func (x *exchangeRecorder) peers(sender, recipient string) {
	x.with(func() { x.rec.Sender, x.rec.Recipient = sender, recipient })
}

func (x *exchangeRecorder) correlation(id string) {
	x.with(func() { x.rec.CorrelationID = id })
}

func (x *exchangeRecorder) requestHash(h string) { x.with(func() { x.rec.RequestCiphertextHash = h }) }
func (x *exchangeRecorder) responseHash(h string) {
	x.with(func() { x.rec.ResponseCiphertextHash = h })
}

// decided settles the outcome. A refusal that names no rule takes the rule
// (and the stage) of the check that refused since the last decided outcome.
// by "" is this gateway.
func (x *exchangeRecorder) decided(outcome, by, rule string, reason ...refusalReason) {
	x.settle(outcome, by, rule, "", reason...)
}

// settle is decided with a fidelity refusal's stage. A refusal that takes the
// rule of the check that refused takes that check's reason too; any other
// clears the reason (refusedFor names one).
func (x *exchangeRecorder) settle(outcome, by, rule, stage string, named ...refusalReason) {
	x.with(func() {
		if x.attested {
			return
		}
		var reason refusalReason
		if len(named) == 1 {
			reason = named[0]
		}
		if outcome == ExchangeRefused {
			if rule == "" {
				rule, stage, reason = x.guardRule, x.guardStage, x.guardReason
			}
			if by == "" {
				by = x.self
			}
		} else {
			by, rule, stage, reason = "", "", "", ""
		}
		x.outcome, x.party, x.rule, x.refusalStage, x.reason = outcome, by, rule, stage, reason
		x.guardRule, x.guardStage, x.guardReason = "", "", ""
	})
}

// answerLine records the line the answer the gateway wrote back is written in.
func (x *exchangeRecorder) answerLine(token string) {
	x.with(func() { x.rec.AnswerLine = token })
}

// ExchangeOwnerships is the closed list ExchangeRecord.Ownership draws from:
// relay.Ownership's names for the three kinds of message a gateway sends.
var ExchangeOwnerships = []string{relay.OwnershipRelayed.String(), relay.OwnershipEdited.String(), relay.OwnershipAuthored.String()}

// sent records the message this gateway sent on the leg: whose it is, its
// builder when it is the gateway's own, and the edits it carries
// (requestEdits).
func (x *exchangeRecorder) sent(p relay.Payload) {
	if own, builder, ok := ownedBy(p); ok {
		edits := requestEdits(p)
		x.with(func() {
			x.rec.Ownership, x.rec.Builder = own, builder
			x.rec.RequestEdits = addEdits(x.rec.RequestEdits, edits)
			x.rec.Edits = addEdits(x.rec.Edits, edits)
		})
	}
}

// requestEdits are the registered edits a request this gateway sends
// carries, in the order made: those made to a participant's record the
// gateway's own build carries, before it was built (p.InputEdits: E-06,
// E-14), then
// those made to the request itself (p.Edits).
func requestEdits(p relay.Payload) []relay.EditID {
	return append(p.InputEdits(), p.Edits()...)
}

// Why an edit was not made (ExchangeRecord.EditsNotMade).
const (
	// EditNotMadePayorEdgeNoCoverage: the payer identity mapping (E-03) was not
	// made because the request a payer's gateway forwards to its own system
	// carries no Coverage, so it names no payor identifier to re-key; the request
	// is carried as sent.
	EditNotMadePayorEdgeNoCoverage = "E-03:no-coverage"
	// EditNotMadeNoStructuralPayerURL: a PAS Task in the answer names a
	// payer-url input that is not a string valueUrl, so the return-path
	// re-key (E-10) has no value to replace.
	EditNotMadeNoStructuralPayerURL = "E-10:no-structural-payer-url"
	// EditNotMadeUnreadableAnswer: the answer could not be read as JSON, so
	// whether it carries a payer-url to re-key (E-10) is unknown.
	EditNotMadeUnreadableAnswer = "E-10:unreadable-answer"
	// EditNotMadePayerDoesNotTake: the payer that answered is not known to
	// take the documentation submission, so a return path (E-10) to it would
	// always refuse.
	EditNotMadePayerDoesNotTake = "E-10:payer-does-not-take"
	// EditNotMadeDTRCheckCouldNotRun: the check that a questionnaire package
	// answer is valid at its own line could not run or that line's lane is
	// still qualifying, or, at none, the lane for the provider's own line is
	// still qualifying, so whether to rename its line-specific names is unknown
	// and the answer was carried as sent.
	EditNotMadeDTRCheckCouldNotRun = "E-08:check-could-not-run"
	// EditNotMadeReportTypeNotCarried: the request to an amending route stated
	// a report type and the amendment does not carry it: its line has no place
	// for one, its build was bridged, or the pend ties the answer to no item.
	// The amendment went on without it. Not a registered edit: the amendment is
	// this gateway's own build.
	EditNotMadeReportTypeNotCarried = "amendment:report-type-not-carried"
	// EditNotMadeTaskNoStructuralPayerURL: a payer's attachment-request Task
	// has one payer-url input and it is not a non-empty valueUrl, so the
	// return-path re-key (E-11) has no value to replace.
	EditNotMadeTaskNoStructuralPayerURL = "E-11:no-structural-payer-url"
	// EditNotMadeTaskUnreadablePayerURL: the Task has several payer-url inputs,
	// or an input that cannot be read and could be one, so which address to
	// re-key (E-11) is unknown.
	EditNotMadeTaskUnreadablePayerURL = "E-11:unreadable-payer-url"
	// EditNotMadeTaskUnreadable: the request could not be read as one Task,
	// so whether it carries a payer-url to re-key (E-11) is unknown.
	EditNotMadeTaskUnreadable = "E-11:unreadable-task"
	// EditNotMadeTaskPayerDoesNotTake: the payer that sent the Task is not
	// known to take the documentation submission, so a return path (E-11) to
	// it would always refuse.
	EditNotMadeTaskPayerDoesNotTake = "E-11:payer-does-not-take"
	// EditNotMadeAddressNotVerified: an adaptive address in the shape the
	// re-key (E-12) writes, naming this payer, whose tag does not verify for
	// the provider that sent it: not issued by this payer, or its base, line
	// or key changed since. It is carried as sent (E-13).
	EditNotMadeAddressNotVerified = "E-13:not-verified"
	// EditNotMadeAddressUncleanRemainder: such an address whose remainder
	// breaks the clean remainder rule, carried as sent (E-13).
	EditNotMadeAddressUncleanRemainder = "E-13:unclean-remainder"
	// EditNotMadeAddressUnreadableRequest: the request could not be read as
	// JSON, so whether it carries an address to restore (E-13) is unknown, or
	// an address found in it could not be rewritten.
	EditNotMadeAddressUnreadableRequest = "E-13:unreadable-request"
	// EditNotMadeRekeyPayerGatewayOlder: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyPayerGatewayOlder = "E-12:payer-gateway-older"
	// EditNotMadeRekeyNotTheRoundsAddress: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyNotTheRoundsAddress = "E-12:not-the-rounds-address"
	// EditNotMadeRekeyContainedInPackageResponse: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyContainedInPackageResponse = "E-12:contained-in-package-response"
	// EditNotMadeRekeyRequestCarriedAQuestionnaire: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyRequestCarriedAQuestionnaire = "E-12:request-carried-a-questionnaire"
	// EditNotMadeRekeyNotUnderBase: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyNotUnderBase = "E-12:not-under-base"
	// EditNotMadeRekeyEscapedAddress: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyEscapedAddress = "E-12:escaped-address"
	// EditNotMadeRekeyBaseSpelledDifferently: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyBaseSpelledDifferently = "E-12:base-spelled-differently"
	// EditNotMadeRekeyUnreadableTag: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyUnreadableTag = "E-12:unreadable-tag"
	// EditNotMadeRekeyUncleanRemainder: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyUncleanRemainder = "E-12:unclean-remainder"
	// EditNotMadeRekeyUnreadableAddress: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyUnreadableAddress = "E-12:unreadable-address"
	// EditNotMadeRekeyUnreadableAnswer: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyUnreadableAnswer = "E-12:unreadable-answer"
	// EditNotMadeRekeyAddressNotWritable: the adaptive address re-key (E-12) was not made: see the CDex design §14.5.
	EditNotMadeRekeyAddressNotWritable = "E-12:address-not-writable"
)

// EditNotMadeValues is the closed list ExchangeRecord.EditsNotMade draws from.
var EditNotMadeValues = []string{EditNotMadePayorEdgeNoCoverage, EditNotMadeNoStructuralPayerURL, EditNotMadeUnreadableAnswer, EditNotMadePayerDoesNotTake, EditNotMadeDTRCheckCouldNotRun, EditNotMadeReportTypeNotCarried,
	EditNotMadeTaskNoStructuralPayerURL, EditNotMadeTaskUnreadablePayerURL, EditNotMadeTaskUnreadable, EditNotMadeTaskPayerDoesNotTake,
	EditNotMadeAddressNotVerified, EditNotMadeAddressUncleanRemainder, EditNotMadeAddressUnreadableRequest,
	EditNotMadeRekeyPayerGatewayOlder, EditNotMadeRekeyNotTheRoundsAddress, EditNotMadeRekeyContainedInPackageResponse, EditNotMadeRekeyRequestCarriedAQuestionnaire, EditNotMadeRekeyNotUnderBase, EditNotMadeRekeyEscapedAddress, EditNotMadeRekeyBaseSpelledDifferently, EditNotMadeRekeyUnreadableTag, EditNotMadeRekeyUncleanRemainder, EditNotMadeRekeyUnreadableAddress, EditNotMadeRekeyUnreadableAnswer, EditNotMadeRekeyAddressNotWritable}

// answerHeaderNotCarried records a header of an answer that was left out, and
// why (one of AnswerHeaderNotCarriedValues).
func (x *exchangeRecorder) answerHeaderNotCarried(value string) {
	x.with(func() {
		if !slices.Contains(x.rec.AnswerHeadersNotCarried, value) {
			x.rec.AnswerHeadersNotCarried = append(x.rec.AnswerHeadersNotCarried, value)
		}
	})
}

// editNotMade records that an edit was not made, and why (one of
// EditNotMadeValues).
func (x *exchangeRecorder) editNotMade(why string) {
	x.with(func() {
		if !slices.Contains(x.rec.EditsNotMade, why) {
			x.rec.EditsNotMade = append(x.rec.EditsNotMade, why)
		}
	})
}

// answered records the answer this gateway wrote on the call, once the
// transmit check admitted it: whose it is, its builder when it is the
// gateway's own, and the edits it carries. A call writes one answer, so it
// replaces any answer recorded before. Every writer of an answer calls it
// (TestEveryAnswerTransmitRecordsTheAnswer).
func (x *exchangeRecorder) answered(p relay.Payload) {
	if own, builder, ok := ownedBy(p); ok {
		x.with(func() {
			x.rec.AnswerOwnership, x.rec.AnswerBuilder = own, builder
			x.rec.AnswerEdits = addEdits(nil, p.Edits())
			x.rec.Edits = addEdits(x.rec.Edits, p.Edits())
		})
	}
}

// ownedBy names whose message p is and, when it is the gateway's own, its
// builder: as built (authored), or carried to another IG line by the
// registered transform (edited, E-09). ok is false for a payload with no
// ownership the closed list names.
func ownedBy(p relay.Payload) (own, builder string, ok bool) {
	own = p.Ownership().String()
	if !slices.Contains(ExchangeOwnerships, own) {
		return "", "", false
	}
	if slices.Contains(relay.Builders(), p.Builder()) {
		builder = string(p.Builder())
	}
	return own, builder, true
}

// addEdits appends to list each registered id in ids it does not yet hold,
// in order.
func addEdits(list []string, ids []relay.EditID) []string {
	for _, id := range ids {
		if slices.Contains(relay.EditIDs(), id) && !slices.Contains(list, string(id)) {
			list = append(list, string(id))
		}
	}
	return list
}

// routed notes a payer-routing answer: the 422 a request is answered when its
// coverage names no payer the network registers, and the 412 a CRD request is
// answered when no coverage to route by could be obtained. Any other status is
// left to the answer.
func (x *exchangeRecorder) routed(status int) { x.routedFor(status, "") }

// routedFor is routed for a routing answer whose fixed reason the resolver
// that refused it named.
func (x *exchangeRecorder) routedFor(status int, reason refusalReason) {
	if status == http.StatusUnprocessableEntity || status == http.StatusPreconditionFailed {
		x.refusedFor(RefusalRouting, reason)
	}
}

// exchangeOfWriter finds the recorder from a writer the wrapper's writer sits
// under, for the refusal writers that are handed only the writer.
func exchangeOfWriter(w http.ResponseWriter) *exchangeRecorder {
	for w != nil {
		if ew, ok := w.(*exchangeWriter); ok {
			return ew.x
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// ownFailure settles this gateway's own internal failure: other, never a
// refusal, even after a leg of the same call was answered.
func (x *exchangeRecorder) ownFailure() { x.decided(ExchangeOther, "", "") }

// refused settles a refusal by this gateway on rule. A fidelity refusal is
// settled by refusedFidelity, which names its stage.
func (x *exchangeRecorder) refused(rule string) { x.decided(ExchangeRefused, "", rule) }

// refusedFor settles a refusal by this gateway on rule, made before the request
// reached the Hub, with the fixed reason its answer gives (refusalReasons).
func (x *exchangeRecorder) refusedFor(rule string, reason refusalReason) {
	x.decided(ExchangeRefused, "", rule, reason)
}

// refusedAttested settles a refusal this gateway has attested to the Hub: what
// the wire said is final, so nothing that unwinds after it (a responder's
// failure settling its own class) can make the record say otherwise.
func (x *exchangeRecorder) refusedAttested(by, rule string) {
	x.decided(ExchangeRefused, by, rule)
	x.with(func() { x.attested = true })
}

// refusing names the rule of a refusal about to be written, for the outcome
// the refusal's answer settles. A fidelity refusal is named by
// refusingFidelity, which names its stage.
func (x *exchangeRecorder) refusing(rule string, reason ...refusalReason) {
	var r refusalReason
	if len(reason) == 1 {
		r = reason[0]
	}
	x.with(func() { x.guardRule, x.guardStage, x.guardReason = rule, "", r })
}

// refusingFor names the rule and the fixed reason (refusalReasons) of a
// refusal about to be written before the request reached the Hub, as refusing
// names the rule alone.
func (x *exchangeRecorder) refusingFor(rule string, reason refusalReason) { x.refusing(rule, reason) }

// refusedFidelity settles a fidelity refusal by this gateway (the fidelity of
// its own edits and builds) made at stage: recording.RefusalStageBuild or
// recording.RefusalStageVerify. It and refusingFidelity are the only ways a fidelity
// refusal is recorded, so every one names its stage
// (TestFidelityRefusals_EveryOneNamesItsStage).
func (x *exchangeRecorder) refusedFidelity(stage string) {
	x.settle(ExchangeRefused, "", RefusalFidelity, stage)
}

// refusingFidelity names a fidelity refusal about to be written, made at
// stage, as refusing names any other.
func (x *exchangeRecorder) refusingFidelity(stage string) {
	x.with(func() { x.guardRule, x.guardStage, x.guardReason = RefusalFidelity, stage, "" })
}

// frameRefusalRule names the rule behind a request-frame refusal: a line the
// recipient does not serve (422) is routing; a frame it cannot read (400) is
// message integrity.
func frameRefusalRule(status int) string {
	switch status {
	case http.StatusUnprocessableEntity:
		return RefusalRouting
	case http.StatusBadRequest:
		return RefusalIntegrity
	}
	return ExchangeOther
}

// answer notes the status inside an inbound answer's frame.
func (x *exchangeRecorder) answer(status int) { x.with(func() { x.appStatus = status }) }

// checked notes one governed check's decision: the finding it recorded, and
// the rule when it refused.
func (x *exchangeRecorder) checked(kind CheckKind, rule string, recorded, refused bool) {
	x.with(func() {
		if recorded {
			x.rec.Findings.Count++
			if !slices.Contains(x.rec.Findings.Kinds, string(kind)) {
				x.rec.Findings.Kinds = append(x.rec.Findings.Kinds, string(kind))
			}
			x.rec.Findings.Refused = x.rec.Findings.Refused || refused
		}
		// An answer from the participant's own system that this gateway
		// cannot read is that call's failure, relayed or refused. A readable
		// answer a check found fault with is that system's answer, not its
		// failure.
		if recorded || refused {
			if b := x.rec.Backend; b != nil && b.ErrorClass == "" && unreadableAnswer(kind, rule) {
				b.ErrorClass = BackendMalformed
			}
		}
		if refused {
			x.guardRule, x.guardStage = refusalRuleFor(kind, rule)
			x.guardReason = ""
		}
	})
}

// unreadableAnswer reports whether a check's rule found an answer this
// gateway cannot read: a CDS Hooks answer that is not one JSON object or is at
// a line it does not know, a FHIR answer that is not the resource the leg
// answers with, or one repeating a member name.
func unreadableAnswer(kind CheckKind, rule string) bool {
	return (kind == KindCDSEnvelope && unreadableCDSRules[rule]) || rule == RuleAnswerShape || rule == RuleDuplicateKey
}

// answering reports that the call is a leg this gateway answers for the
// network (its participant's own system is the backend).
func (x *exchangeRecorder) answering() bool {
	answering := false
	x.with(func() { answering = x.rec.Direction == DirectionInbound })
	return answering
}

// since is now() less start, read only when recording: a gateway that records
// nothing reads no clock for it.
func (x *exchangeRecorder) since(now func() time.Time, start time.Time) time.Duration {
	if x == nil {
		return 0
	}
	return now().Sub(start)
}

// systemAnswered reports whether the participant's own system has answered
// the operation forwarded to it with a status (any status: it may have acted).
// A read made on the way to the forward (the CDS service listing) is not that
// answer: the system has been asked nothing to act on yet. It is what decides
// whether a responder's failure to authorize its answer is attested delivered,
// so a gateway with no recorder gets the plain 502, which claims less, not
// more.
func (x *exchangeRecorder) systemAnswered() bool {
	answered := false
	x.with(func() { answered = x.forwarded && x.rec.Backend != nil && x.rec.Backend.Status != 0 })
	return answered
}

// backend notes the call that asks the participant's own system for the
// answer (the operation forwarded to it): it is the exchange's Backend.
func (x *exchangeRecorder) backend(status int, latency time.Duration, class string) {
	x.with(func() {
		x.rec.BackendCalls++
		x.rec.Backend = &BackendCall{Status: status, Latency: latency, ErrorClass: class}
		if x.rec.Stages != nil {
			x.rec.Stages.Forward += latency
		}
		x.forwarded = true
		x.cut = x.cut || class == BackendCancelled
	})
}

// resent notes each call to the participant's own system its connector sent a
// second time after a 401 (smartauth.CredentialRefreshObservation): one more
// call each, and why.
func (x *exchangeRecorder) resent(challenges []string) {
	if len(challenges) == 0 {
		return
	}
	x.with(func() {
		for _, c := range challenges {
			x.rec.BackendCalls++
			x.rec.BackendResends = append(x.rec.BackendResends, BackendResend{Reason: BackendResendCredentialRefreshed, Challenge: c})
		}
	})
}

// redirected notes the redirect hops a call to the participant's own system
// was answered with (smartauth.RedirectObservation), each by its class.
func (x *exchangeRecorder) redirected(hops []string) {
	if len(hops) == 0 {
		return
	}
	x.with(func() { x.rec.BackendRedirects = append(x.rec.BackendRedirects, hops...) })
}

// trackForward notes that the operation's forward to the participant's own
// system, if any, is recorded (backend): only the built-in native responder,
// whose one forward is, says so.
func (x *exchangeRecorder) trackForward() {
	x.with(func() { x.forwardTracked = true })
}

// forwardedToSystem reports whether the operation was forwarded to the
// participant's own system, and whether that is known: only when the forward
// is tracked (trackForward). A connector that calls its system itself, or an
// exchange with no recorder, cannot tell.
func (x *exchangeRecorder) forwardedToSystem() (forwarded, known bool) {
	if x == nil {
		return false, false
	}
	x.with(func() { forwarded, known = x.forwarded, x.forwarded || x.forwardTracked })
	return forwarded, known
}

// read notes one read of the participant's own system: of its system of
// record (status 0), or of its CDS service listing. It is the exchange's
// Backend until the operation is forwarded; a read after that (a check of the
// answer) is counted, and the forwarded call stays the one the answer came
// from.
func (x *exchangeRecorder) read(status int, latency time.Duration, class string) {
	x.with(func() {
		x.rec.BackendCalls++
		x.cut = x.cut || class == BackendCancelled
		if x.rec.Stages != nil {
			x.rec.Stages.Reads += latency
		}
		if !x.forwarded {
			x.rec.Backend = &BackendCall{Status: status, Latency: latency, ErrorClass: class}
		}
	})
}

// stage times one stage of an inbound leg's answer: it returns the call that
// ends it, adding the time between to the stage add names. A nil recorder,
// or an ingress call's, times nothing and reads no clock.
func (x *exchangeRecorder) stage(add func(*LegStages, time.Duration)) func() {
	if x == nil {
		return func() {}
	}
	var timed bool
	x.with(func() { timed = x.rec.Stages != nil })
	if !timed {
		return func() {}
	}
	start := x.clock()
	return func() {
		d := x.clock().Sub(start)
		x.with(func() { add(x.rec.Stages, d) })
	}
}

// network times one request to the Hub, from handing it to the transport to
// reading the answer: it returns the call that ends it. Requests outstanding
// together are timed once, so the total never exceeds the call's Latency. A
// nil recorder, or an inbound leg's, times nothing and reads no clock.
func (x *exchangeRecorder) network() func() {
	if x == nil {
		return func() {}
	}
	var timed bool
	x.with(func() {
		if timed = x.rec.Direction == DirectionIngress; !timed {
			return
		}
		if x.rec.Network == nil {
			x.rec.Network = &NetworkWait{}
		}
		x.rec.Network.Calls++
		if x.hubOut == 0 {
			x.hubSince = x.clock()
		}
		x.hubOut++
	})
	if !timed {
		return func() {}
	}
	return func() {
		x.with(func() {
			if x.hubOut--; x.hubOut == 0 {
				x.rec.Network.Latency += x.clock().Sub(x.hubSince)
			}
		})
	}
}

// addLedger adds a pend ledger read or write to its stage.
func addLedger(s *LegStages, d time.Duration) { s.Ledger += d }

// answerErrorClass is the class of a failed authorize call (AnswerErrorClasses),
// for an inbound leg's answer or a request leg: the request ending first is
// cancelled whatever the call returned, since the requester had gone.
func answerErrorClass(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return BackendCancelled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return BackendTimeout
	case errors.Is(err, errAuthorizationDenied):
		return BackendAuth
	case serviceclient.IsRedirectRefused(err):
		// The Framework answered a redirect, not followed: the call fails
		// as when the Framework cannot be reached, told apart by its class.
		return redirectRefused
	case errors.Is(err, errAuthzUnreachable):
		return ExchangeUnreachable
	}
	return ExchangeOther
}

// validation times a synchronous validation: one nested inside another is
// not timed again.
func (x *exchangeRecorder) validation() func() {
	if x == nil {
		return func() {}
	}
	outer := false
	x.with(func() {
		outer = x.validating == 0
		x.validating++
	})
	stop := func() {}
	if outer {
		stop = x.stage(func(s *LegStages, d time.Duration) { s.Validate += d })
	}
	return func() {
		stop()
		x.with(func() { x.validating-- })
	}
}

// unwrapDone ends the leg's Unwrap stage: from the call's arrival to its
// dispatch.
func (x *exchangeRecorder) unwrapDone() {
	if x == nil {
		return
	}
	now := x.clock()
	x.with(func() {
		if x.rec.Stages != nil && !x.unwrapped {
			x.rec.Stages.Unwrap = now.Sub(x.rec.Start)
			x.unwrapped = true
		}
	})
}

// answerError records why an inbound leg's answer could not be authorized
// (AnswerErrorClasses).
func (x *exchangeRecorder) answerError(class string) {
	x.with(func() { x.rec.AnswerError = closed(AnswerErrorClasses, class) })
}

// authorizeError notes why no authority could be obtained for a request leg
// (AnswerErrorClasses). A leg that fails ends its call's legs, so this is about
// the call's last.
func (x *exchangeRecorder) authorizeError(class string) {
	x.with(func() { x.rec.AuthorizeError = closed(AnswerErrorClasses, class) })
}

// involvedOmitted notes a patient left out of an involved list.
func (x *exchangeRecorder) involvedOmitted(reason string) {
	x.with(func() {
		if x.rec.InvolvedOmitted == nil {
			x.rec.InvolvedOmitted = map[string]int{}
		}
		x.rec.InvolvedOmitted[closed(InvolvedOmitReasons, reason)]++
	})
}

// clock is the time the recorder reads latencies by: the gateway's own.
func (x *exchangeRecorder) clock() time.Time {
	if x.now == nil {
		return time.Now()
	}
	return x.now()
}

// bodyReadClass is the class of an answer whose body could not be read: cut
// short because the request it served ended, out of time, or the
// participant's system failing to send it.
func bodyReadClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return BackendCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return BackendTimeout
	}
	return BackendRead
}

// malformed notes that the last call's 2xx answer was not one this gateway
// could read.
func (x *exchangeRecorder) malformed() {
	x.with(func() {
		if b := x.rec.Backend; b != nil && b.ErrorClass == "" {
			b.ErrorClass = BackendMalformed
		}
	})
}

// repeatsAMember reports a body that repeats a member name. It can be read two
// ways, so it refuses at every level on message integrity: the call's record
// names that rule, and when the body was the participant's own system's
// answer, that call's failure (malformed).
func repeatsAMember(ctx context.Context, body []byte) bool {
	if !errors.Is(scanMessage(body), relay.ErrDuplicateKey) {
		return false
	}
	x := exchangeOf(ctx)
	x.refusing(RefusalIntegrity)
	x.malformed()
	return true
}

// readsFailed records, in read order, the class (BackendErrorClasses) of each
// failed read of the participant's system of record the answer was built
// without (ExchangeRecord.ReadsFailed). Nothing is recorded for none.
func (x *exchangeRecorder) readsFailed(classes []string) {
	if len(classes) == 0 {
		return
	}
	x.with(func() {
		for _, c := range classes {
			x.rec.ReadsFailed = append(x.rec.ReadsFailed, closed(BackendErrorClasses, c))
		}
	})
}

// sorFailure settles an answering-side refusal caused by err, a failed read
// of the participant's own system of record (the read itself is noted where
// it ran, recordingSoR): that system not answering usably, an upstream
// failure. A read the request's end cut short was noted as such
// (readEndedClass), and the exchange settles "other" (emitExchange), whatever
// the connector returned: a connector's own cancellation is its system not
// answering. An answer that repeats a member name (relay.ErrDuplicateKey) can
// be read two ways, so it is recorded as repeatsAMember records it on every
// leg: refused on message integrity, that call's failure malformed.
func (x *exchangeRecorder) sorFailure(err error) {
	if readRepeatsAMember(err) {
		x.malformed()
		// Settled now, and named for the refusal the leg writes after it too
		// (a caller can settle before its answer is written).
		x.decided(ExchangeRefused, "", RefusalIntegrity)
		x.refusing(RefusalIntegrity)
		return
	}
	x.decided(ExchangeUpstreamError, "", "")
}

// sorReadClass is the class (BackendErrorClasses) a failed read of the
// participant's system of record is recorded with: the read's own class
// (sorClass), or how the request it served ended (readEndedClass).
func sorReadClass(ctx context.Context, err error) string {
	return closed(BackendErrorClasses, readEndedClass(ctx, sorClass(err)))
}

// readRepeatsAMember reports a failed read of the participant's own system of
// record whose answer repeats a member name (relay.ErrDuplicateKey).
func readRepeatsAMember(err error) bool { return errors.Is(err, relay.ErrDuplicateKey) }

// sorClass is the class of a read of the participant's own system of record:
// the same categories SoRFailureResponse answers with.
func sorClass(err error) string {
	var readErr *SoRReadError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, smartauth.ErrCleartextRedirect) && !smartauth.IsTokenAcquisitionError(err):
		// The system redirected the read from https to http, and the hop
		// was not sent. A token endpoint's downgrade is a token failure.
		return BackendHTTP3xx
	case errors.Is(err, context.DeadlineExceeded):
		return BackendTimeout
	case errors.Is(err, context.Canceled):
		// The connector cancelled the read itself: its system did not
		// answer. A read the request's end cut short is named by
		// readEndedClass, which reads the request's context.
		return ExchangeOther
	case errors.As(err, &readErr) && readErr != nil && readErr.Kind == SoRAuthenticationFailed:
		return BackendAuth
	case errors.As(err, &readErr) && readErr != nil && readErr.Kind == SoRUnavailable:
		return ExchangeOther
	}
	return BackendMalformed
}

// refusalRuleFor names the network rule behind a refusing check, and its
// stage when the rule is fidelity: a bridged message failing its $validate is
// an edit this gateway built failing its own verification.
func refusalRuleFor(kind CheckKind, rule string) (string, string) {
	switch {
	case kind == KindFHIRBridged:
		return RefusalFidelity, vocab.RefusalStageVerify
	case rule == RuleSubjectPCI, rule == RuleSubjectToken:
		// No site routes these through the guard. The federated-query bind
		// records its refusal as consent itself (handleFederatedQueryInbound).
		return RefusalAuthority, ""
	case rule == RuleDuplicateKey:
		return RefusalIntegrity, ""
	}
	return RefusalConformance, ""
}

// callEndedClass is a failed call's class, with cancelled kept only when the
// request the call served has ended, as for a read (readEndedClass): a call
// cancelled while that request is live was not cut short by it, and one whose
// request ran out of time is a timeout. ctx is the call's own context: the
// request's for a forwarded operation; for the CDS service listing, its own
// bounded one, detached from the request.
func callEndedClass(ctx context.Context, class string) string {
	if class != BackendCancelled {
		return class
	}
	switch {
	case ctx.Err() == nil:
		return ExchangeOther
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return BackendTimeout
	}
	return class
}

// backendClass classifies a call to the participant's own system that got no
// usable answer: err is the client's error (nil when it answered status).
func backendClass(status int, err error) string {
	if err == nil {
		switch {
		case status == http.StatusUnauthorized, status == http.StatusForbidden:
			return BackendAuth // the participant's system rejected this gateway's credentials or access
		}
		switch status / 100 {
		case 2:
			return ""
		case 3:
			return BackendHTTP3xx
		case 4:
			return BackendHTTP4xx
		case 5:
			return BackendHTTP5xx
		}
		return ExchangeOther
	}
	var ne net.Error
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	var hostname x509.HostnameError
	var unknownAuth x509.UnknownAuthorityError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.Canceled):
		return BackendCancelled
	case smartauth.IsTokenAcquisitionError(err):
		return BackendAuth
	case errors.Is(err, smartauth.ErrCDSClientIssuerUnknown):
		// The CDS client JWT could not be minted: this gateway's own
		// credential, unsent, as a bearer that could not be obtained is.
		return BackendAuth
	case errors.As(err, &cert), errors.As(err, &hostname), errors.As(err, &unknownAuth), errors.As(err, &record):
		return BackendTLS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return BackendTimeout
	case errors.As(err, &op) && op.Op == "dial":
		return BackendConnect
	}
	return ExchangeOther
}

// hubRules are the Hub's own refusals of a leg (the platform's Hub service), by the
// reason it answers, with the network rule each is. Only these name a rule:
// any other answer from the Hub is "other".
var hubRules = map[string]string{
	"missing X-Holder-Assertion header":          RefusalAuthentication,
	"invalid X-Holder-Assertion encoding":        RefusalAuthentication,
	"invalid X-Holder-Assertion JSON":            RefusalAuthentication,
	"unknown sender":                             RefusalAuthentication,
	"assertion holder id mismatch":               RefusalAuthentication,
	"assertion verification failed":              RefusalAuthentication,
	"invalid authz token":                        RefusalAuthority,
	"authz token verification failed":            RefusalAuthority,
	"authz token's patient does not fit the leg": RefusalAuthority,
	"response leg authorization failed":          RefusalAuthority,
	"replay detected":                            RefusalReplay,
	// Routing, and transient: the network cannot carry this body right now.
	"large envelope budget exhausted":        RefusalRouting,
	"stale or future timestamp":              RefusalReplay,
	"unknown recipient":                      RefusalRouting,
	"read body failed":                       RefusalIntegrity,
	"unsupported content coding":             RefusalIntegrity,
	"decode envelope failed":                 RefusalIntegrity,
	"missing authority frame":                RefusalIntegrity,
	"missing correlation id":                 RefusalIntegrity,
	"payload hash mismatch":                  RefusalIntegrity,
	"decode recipient response failed":       RefusalIntegrity,
	"response leg recipient mismatch":        RefusalIntegrity,
	"response leg sender mismatch":           RefusalIntegrity,
	"response leg transaction-type mismatch": RefusalIntegrity,
	"audit append failed":                    RefusalAudit,
	"request body too large":                 RefusalRouting,
	"response body too large":                RefusalRouting,
	"response audit append failed":           RefusalAudit,
}

// The Hub's reasons for an answer refused after the recipient answered
// (the platform's Hub service): the Hub's own refusal of an answer over the network's
// limit (in hubRules), and a recipient gateway's attested refusal of its
// system's answer, which the Hub relays with the refusal's rule
// (hubRefusalError.rule; hubRefusalOutcome reads it by its marker).
const (
	hubReasonAnswerTooLarge         = "response body too large"
	hubReasonRecipientRefusedAnswer = "forward to recipient failed: the recipient's gateway refused its system's answer"
)

// hubRefusalOutcome reads a non-2xx answer to the leg's POST to the Hub: the
// Hub's own refusal on a network rule, the recipient's gateway refusing or
// failing the forward, or no answer from the Hub at all. An answer the Hub's
// route handler did not write (no HubDeliveredHeader: a load balancer in front
// of it, or a connection that failed here) is never the Hub's refusal.
func hubRefusalOutcome(e *hubRefusalError) (outcome, by, rule string) {
	if !e.fromHub {
		if e.delivered == "" {
			return ExchangeUnreachable, "", ""
		}
		// It may have been forwarded before the connection failed.
		return ExchangeOther, "", ""
	}
	// A leg the recipient does not declare it takes, refused before
	// forwarding: where the leg can be delivered.
	if e.legNotTaken() {
		return ExchangeRefused, RefusedByHub, RefusalRouting
	}
	reason := e.reason
	if strings.HasPrefix(reason, "forward to recipient failed") {
		switch {
		case strings.Contains(reason, "the recipient's gateway refused its system's answer"):
			// Its attested refusal after its system answered, on the rule
			// the Hub relayed; unknown ("other") when it relayed none.
			if e.rule == "" {
				return ExchangeRefused, recipientParty(e.leg), ExchangeOther
			}
			return ExchangeRefused, recipientParty(e.leg), e.rule
		case strings.Contains(reason, "the recipient refused it"):
			return ExchangeRefused, recipientParty(e.leg), ExchangeOther
		case strings.Contains(reason, "has no room for this envelope"):
			// Its attested refusal before acting (NotActedAtCapacity): the
			// network cannot carry this body right now.
			return ExchangeRefused, recipientParty(e.leg), RefusalRouting
		case strings.Contains(reason, "the recipient answered"):
			return ExchangeUpstreamError, "", ""
		case strings.Contains(reason, "could not be read"):
			// The recipient answered; the answer was lost on the way back.
			return ExchangeOther, "", ""
		}
		return ExchangeUnreachable, "", ""
	}
	switch {
	case strings.HasPrefix(reason, "involved patients refused"):
		return ExchangeRefused, RefusedByHub, RefusalAuthority
	case reason == "unknown transaction type":
		// Refused before forwarding, the leg is not one the network routes; on
		// the answer, the recipient answered with the wrong leg.
		if e.delivered == "" {
			return ExchangeRefused, RefusedByHub, RefusalRouting
		}
		return ExchangeRefused, RefusedByHub, RefusalIntegrity
	}
	if rule, ok := hubRules[reason]; ok {
		return ExchangeRefused, RefusedByHub, rule
	}
	return ExchangeOther, "", ""
}

// legOutcome notes a round trip's result on the recorder: the counterpart's
// answer (a relayed non-2xx is an upstream error), or why there was none.
func (x *exchangeRecorder) legOutcome(err error) {
	if x == nil {
		return
	}
	var re *RelayError
	var hr *hubRefusalError
	var lost *answerLostError
	var authzRefused *authzRefusal
	var noAuthority *requestNoAuthority
	// The Hub's delivery value, as its route handler stated it, for this leg
	// only: a later leg's answer replaces an earlier one's.
	delivered := ""
	if errors.As(err, &hr) && hr.fromHub {
		delivered = hr.delivered
		if delivered == "" {
			delivered = "no"
		}
	}
	x.with(func() { x.rec.HubDelivered = delivered })
	switch {
	case err == nil:
		x.decided(ExchangeAnswered, "", "")
	case errors.As(err, &re):
		x.decided(ExchangeUpstreamError, "", "")
	case errors.Is(err, errAuthorizationDenied):
		x.decided(ExchangeRefused, RefusedByAuthorizationFramework, RefusalAuthority)
	case errors.As(err, &authzRefused):
		x.decided(ExchangeRefused, RefusedByAuthorizationFramework, authzRefused.rule)
	case errors.As(err, &noAuthority):
		x.refused(RefusalAuthority)
	case errors.Is(err, errHubUnreachable), errors.Is(err, errHubTimeout), errors.Is(err, errAuthzUnreachable):
		x.decided(ExchangeUnreachable, "", "")
	case errors.As(err, &hr):
		x.decided(hubRefusalOutcome(hr))
	case errors.As(err, &lost) && lost.tooLarge:
		// This gateway refused the answer for its size: its own refusal,
		// whichever role sent the leg.
		x.decided(ExchangeRefused, x.self, RefusalRouting)
	case errors.As(err, &lost) && lost.cause == "the Hub's answer could not be read":
		// A read that failed on the way back: transport, not integrity.
		x.decided(ExchangeOther, "", "")
	case errors.As(err, &lost) && lost.rule != "":
		x.decided(ExchangeRefused, "", lost.rule)
	case errors.As(err, &lost):
		x.decided(ExchangeRefused, "", RefusalIntegrity)
	default:
		x.decided(ExchangeOther, "", "")
	}
}

// answerOutcome maps the outcome an inbound answer was checked against to the
// record's.
func answerOutcome(o relay.Outcome) string {
	switch o {
	case relay.OutcomeAnswered:
		return ExchangeAnswered
	case relay.OutcomeUpstreamError:
		return ExchangeUpstreamError
	case relay.OutcomeRefused:
		return ExchangeRefused
	}
	return ExchangeOther
}

// boundedID is how a record names a leg's id: as it is when it is one bounded
// token (correlationShape, the rule the ingress and the header to the
// participant's system apply), else as "sha256:" and its digest, so an id a
// message chose (a Claim's own correlation) never carries the message's text
// into a log line, and still joins every record of the leg.
func boundedID(id string) string {
	if id == "" || correlationShape.MatchString(id) {
		return id
	}
	return "sha256:" + sha256hex([]byte(id))
}

// exchangeWriter records the status written. Writes pass straight through: a
// body written without WriteHeader is a 200, which is what an unset status
// reads as once the handler has returned.
type exchangeWriter struct {
	http.ResponseWriter
	x      *exchangeRecorder
	status int
	// written is the answer's body bytes written.
	written atomic.Int64
}

func (w *exchangeWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 { // a 1xx is not the answer
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *exchangeWriter) Write(b []byte) (int, error) {
	defer w.x.stage(func(s *LegStages, d time.Duration) { s.Write += d })()
	n, err := w.ResponseWriter.Write(b)
	w.written.Add(int64(n))
	return n, err
}

// countingBody counts the request body's bytes as the handler reads them, and
// whether it read them to the end: only then is the body's size known.
type countingBody struct {
	io.ReadCloser
	n   atomic.Int64
	eof atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	if err == io.EOF {
		b.eof.Store(true)
	}
	return n, err
}

// size is the body's size, nil when it is not known.
func (b *countingBody) size() *int64 {
	if !b.eof.Load() {
		return nil
	}
	n := b.n.Load()
	return &n
}

func (w *exchangeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ingressRoute is the handler a Da Vinci ingress route is mounted with: the
// exchange record outermost, so it sees every answer the others write.
func (g *Gateway) ingressRoute(route string) http.HandlerFunc {
	r, ok := workstreamIngressRoute(route)
	if !ok {
		panic("engine: no compiled-in workstream declares ingress route " + route)
	}
	return g.recordExchange(route, DirectionIngress, g.observeIngress(r.tag, g.withIngressCorrelation(withIngressCaller(r.handler(g)))))
}

// inboundRoute is the handler /substrate/inbound is mounted with.
func (g *Gateway) inboundRoute() http.HandlerFunc {
	return g.recordExchange(RouteSubstrateInbound, DirectionInbound, g.observeInbound(g.handleInbound))
}

// recordExchange wraps a route so each call produces one ExchangeRecord.
func (g *Gateway) recordExchange(route, direction string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m := g.cfg.ActiveRequestMetric; m != nil {
			dir := closed(ExchangeDirections, direction)
			m(dir, 1)
			defer m(dir, -1)
		}
		if g.cfg.ExchangeObserved == nil {
			h(w, r)
			return
		}
		x := &exchangeRecorder{self: g.exchangeParty(direction), now: g.exchangeClock}
		if g.conformance().Level() == EnforcementObserve {
			x.tally = &legTally{}
		}
		x.rec.Start = g.exchangeClock()
		x.rec.Direction = closed(ExchangeDirections, direction)
		x.rec.Route = closed(ExchangeRoutes, route)
		x.rec.Exchange = ExchangeOther
		// This gateway's own side of the leg is known before anything is
		// read: the sender of a call it originates, the recipient of a leg
		// it receives. The other side is set once the leg is verified.
		if direction == DirectionIngress {
			x.rec.Sender = g.cfg.HolderID
		} else {
			x.rec.Recipient = g.cfg.HolderID
			x.rec.Stages = &LegStages{}
		}
		ew := &exchangeWriter{ResponseWriter: w, x: x}
		rr := r.WithContext(context.WithValue(r.Context(), exchangeRecorderKey{}, x))
		body := &countingBody{ReadCloser: r.Body}
		if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
			// No body: its size, 0, is known without a read.
			body.ReadCloser = http.NoBody
			body.eof.Store(true)
		}
		rr.Body = body
		panicked := true
		defer func() {
			g.emitExchange(x, ew, body, panicked)
		}()
		h(ew, rr)
		panicked = false
	}
}

// exchangeClock is Config.Clock, or the wall clock for a Gateway built without
// the constructor (tests).
func (g *Gateway) exchangeClock() time.Time {
	if g.cfg.Clock == nil {
		return time.Now()
	}
	return g.cfg.Clock()
}

// exchangeParty is the refusing party this gateway is on direction: the
// gateway whose ingress it is (a provider's, or a payer's for the payer's
// attachment request), and on a leg the network delivers to it the gateway of
// its own registered role (a payer's, or a provider's for the payer's
// attachment request). Decided by the role, never by a name.
func (g *Gateway) exchangeParty(direction string) string {
	switch {
	case direction == DirectionIngress && g.cfg.Role == "payer":
		return RefusedByPayerGateway
	case direction == DirectionIngress:
		return RefusedByProviderGateway
	}
	return gatewayParty(g.cfg.Role)
}

// gatewayParty is the refusing party a gateway of role is. A role with no
// party of its own in the record's closed list is "other": unknown, never a
// guessed party.
func gatewayParty(role string) string {
	switch role {
	case "payer":
		return RefusedByPayerGateway
	case "provider":
		return RefusedByProviderGateway
	}
	return ExchangeOther
}

// recipientParty is the gateway party that answers leg, by its row's
// recipient roles (legSpec.RecipientRoles): the party of the one role the
// row names, and "other" for a leg this gateway does not know or a row that
// names several roles of different parties.
func recipientParty(leg string) string {
	spec, _ := legSpecFor(leg)
	return rolesParty(spec.RecipientRoles)
}

// rolesParty is the one gateway party roles share, or "other" when they are
// none or of different parties.
func rolesParty(roles []string) string {
	if len(roles) == 0 {
		return ExchangeOther
	}
	party := gatewayParty(roles[0])
	for _, r := range roles[1:] {
		if gatewayParty(r) != party {
			return ExchangeOther
		}
	}
	return party
}

// emitExchange settles the record and hands it to Config.ExchangeObserved. A
// panicking consumer never reaches the request.
func (g *Gateway) emitExchange(x *exchangeRecorder, w *exchangeWriter, body *countingBody, panicked bool) {
	defer func() { _ = recover() }()
	x.mu.Lock()
	rec := x.rec
	rec.Findings.Kinds = slices.Clone(rec.Findings.Kinds)
	rec.Edits = slices.Clone(rec.Edits)
	rec.RequestEdits = slices.Clone(rec.RequestEdits)
	rec.AnswerEdits = slices.Clone(rec.AnswerEdits)
	// At observe the count and kinds are deferred (checks run off the request
	// path), but a check that refuses is always judged inline, so Refused is known.
	if g.conformance().Level() == EnforcementObserve {
		rec.Findings = FindingSummary{Deferred: true, Refused: rec.Findings.Refused}
	}
	tally := x.tally
	if rec.Backend != nil {
		b := *rec.Backend
		rec.Backend = &b
	}
	unwrapped := x.unwrapped
	if rec.Stages != nil {
		s := *rec.Stages
		rec.Stages = &s
	}
	if rec.Network != nil {
		n := *rec.Network
		rec.Network = &n
	}
	hubOut, hubSince := x.hubOut, x.hubSince
	outcome, by, rule, appStatus, guardRule := x.outcome, x.party, x.rule, x.appStatus, x.guardRule
	stage, guardStage := x.refusalStage, x.guardStage
	reason, guardReason := x.reason, x.guardReason
	backendFailed := rec.Backend != nil && rec.Backend.ErrorClass != "" && rec.Backend.ErrorClass != BackendCancelled
	backendCancelled := x.cut
	x.mu.Unlock()

	rec.Latency = g.exchangeClock().Sub(rec.Start)
	if hubOut > 0 {
		// A request to the Hub still outstanding when the call ended (its
		// handler returned or panicked) waited until then.
		rec.Network.Latency += max(0, rec.Start.Add(rec.Latency).Sub(hubSince))
	}
	if rec.Stages != nil && !unwrapped {
		// Refused before its dispatch with an answer it did not seal: the
		// call's time in no other stage was the unwrap.
		st := rec.Stages
		st.Unwrap = max(0, rec.Latency-(st.Reads+st.Forward+st.Validate+st.Seal+st.Ledger+st.Write))
	}
	rec.Status = w.status
	rec.RequestBytes = body.size()
	if !panicked {
		n := w.written.Load()
		rec.ResponseBytes = &n
	}
	if rec.Status == 0 && !panicked {
		rec.Status = http.StatusOK // a handler that wrote nothing answered 200
	}
	if rec.Direction == DirectionIngress {
		// The leg id the answer names, when it names one; an origination
		// route's answer names none, and its record keeps its first leg's.
		if id := w.Header().Get(LegIDHeader); id != "" {
			rec.CorrelationID = id
		}
	} else if appStatus != 0 && rec.Status == http.StatusOK {
		rec.Status = appStatus
	}
	rec.CorrelationID = boundedID(rec.CorrelationID)
	if rec.Trace == rec.CorrelationID {
		rec.Trace = ""
	}
	ok := rec.Status/100 == 2
	switch {
	case panicked:
		outcome, by, rule, stage, reason = ExchangeOther, "", "", "", ""
	case guardRule != "" && !ok:
		// A check refused after the last settled outcome, and the answer
		// says so.
		outcome, by, rule, stage, reason = ExchangeRefused, x.self, guardRule, guardStage, guardReason
	case outcome == ExchangeAnswered && !ok:
		// The leg was answered, and this gateway answered its caller with an
		// error anyway: its own refusal, on a rule nothing named.
		outcome, by, rule, stage, reason = ExchangeRefused, x.self, ExchangeOther, "", ""
	case outcome != "":
	case ok:
		outcome = ExchangeAnswered
	case rec.Status/100 == 4:
		outcome, by, rule, reason = ExchangeRefused, x.self, ExchangeOther, ""
	default:
		outcome = ExchangeOther
	}
	if outcome == ExchangeRefused && (by == "" || by == x.self) && (rule == "" || rule == ExchangeOther) && backendFailed {
		// This gateway answered with its own refusal, naming no rule, because
		// its participant's system did not answer usably (its service listing
		// could not be read, or its error answer could not be sent to a
		// requester without a message frame): the failure is upstream.
		outcome, by, rule = ExchangeUpstreamError, "", ""
	}
	if backendCancelled && outcome != ExchangeAnswered {
		// A call to the participant's system was cut short because the
		// request ended: whatever refused it, it was not that system.
		outcome, by, rule = ExchangeOther, "", ""
	}
	rec.Outcome = closed(ExchangeOutcomes, outcome)
	if rec.Outcome == ExchangeRefused {
		rec.RefusedBy, rec.Rule = closed(RefusalParties, by), closed(RefusalRules, rule)
		if rec.Rule == RefusalFidelity {
			// Only a fidelity refusal carries a stage; one whose site named
			// none would be "other".
			rec.RefusalStage = closed(vocab.RefusalStages, stage)
		}
		if reason != "" && rec.Direction == DirectionIngress && by == x.self {
			// Only this gateway's own refusal of its participant's call,
			// made before the request reached the Hub, carries its reason; a
			// value outside the registered sentences is recorded as other.
			rec.RefusalReason = closedReason(reason)
		}
	}
	if rec.Backend != nil && rec.Backend.ErrorClass != "" {
		rec.Backend.ErrorClass = closed(BackendErrorClasses, rec.Backend.ErrorClass)
	}
	if tally != nil {
		// Deferred, so the leg's result is captured even when an exchange
		// observer panics.
		defer tally.end(g, rec)
	}
	g.cfg.ExchangeObserved(rec)
}
