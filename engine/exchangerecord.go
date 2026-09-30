package engine

// exchangerecord.go — the exchange record seam.
//
// Every call this gateway answers on a Da Vinci ingress route or on
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
// that refuses) on a recorder the wrapper puts on the request context. A
// recorder that is absent (ExchangeObserved unset, or a handler driven without
// the wrapper) makes every note a no-op.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ExchangeRecord is one call this gateway answered, as it saw it.
type ExchangeRecord struct {
	// Start is when this gateway received the call (Config.Clock).
	Start time.Time
	// CorrelationID is the leg's id: the X-SHN-Leg-Id an ingress answer
	// carries, or the envelope correlationId of an inbound leg, as boundedID
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
	// participant) or DirectionInbound (a leg from the network).
	Direction string
	// Route is the route the call arrived on (ExchangeRoutes).
	Route string
	// Exchange is the leg's transaction type (ExchangeKinds), or ExchangeOther
	// when the call was answered before a leg was known. On an inbound leg it
	// is read from the envelope before the leg's token is verified; it is a
	// closed value either way.
	Exchange string
	// Operation is the CDS hook or the DTR operation the leg carries, when it
	// names one (ExchangeOperations; empty when it names none). Not a metric
	// dimension.
	Operation string
	// ContractLine is the IG line the leg ran at, when one was settled.
	ContractLine string
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
	// ExchangeOther. RefusalLimit is reserved, and from v0.58.0 the gateway records an
	// Authorization Framework denial as RefusalAuthority, whatever its reason.
	RefusedBy, Rule string
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
	// Findings summarizes the conformance findings the call's checks recorded.
	Findings FindingSummary
}

// BackendCall is one call to the participant's own system.
type BackendCall struct {
	// Status is the HTTP status its system answered; 0 when it gave none.
	Status  int
	Latency time.Duration
	// ErrorClass is "" for a usable 2xx answer, else one of BackendErrorClasses.
	ErrorClass string
}

// FindingSummary counts the conformance findings recorded for one call.
type FindingSummary struct {
	Count   int
	Refused bool
	// Kinds are the distinct CheckKind values recorded, in first-seen order.
	Kinds []string
}

// ExchangeOther is the value recorded for anything a closed list does not name.
const ExchangeOther = "other"

// Directions.
const (
	DirectionIngress = "ingress"
	DirectionInbound = "inbound"
)

// Routes.
const (
	RouteCRD              = "crd"
	RouteDTR              = "dtr"
	RoutePAS              = "pas"
	RoutePASInquire       = "pas-inquire"
	RouteSubstrateInbound = "substrate-inbound"
)

// Outcomes.
const (
	ExchangeAnswered      = "answered"
	ExchangeRefused       = "refused"
	ExchangeUnreachable   = "unreachable"
	ExchangeUpstreamError = "upstream-error"
)

// Refusing parties.
const (
	RefusedByProviderGateway        = "provider-gateway"
	RefusedByHub                    = "hub"
	RefusedByAuthorizationFramework = "authorization-framework"
	RefusedByPayerGateway           = "payer-gateway"
)

// Refusal rules: the network's own primitives, and conformance for a check a
// participant opted into.
const (
	RefusalAuthentication = "authentication"
	RefusalAuthority      = "authority"
	RefusalConsent        = "consent"
	RefusalRouting        = "routing"
	RefusalReplay         = "replay"
	RefusalIntegrity      = "integrity"
	RefusalAudit          = "audit"
	RefusalFidelity       = "fidelity"
	RefusalConformance    = "conformance"
	RefusalLimit          = "limit"
)

// Backend error classes.
const (
	BackendTimeout   = "timeout"
	BackendConnect   = "connect"
	BackendTLS       = "tls"
	BackendAuth      = "auth"
	BackendHTTP3xx   = "http-3xx"
	BackendHTTP4xx   = "http-4xx"
	BackendHTTP5xx   = "http-5xx"
	BackendRead      = "read"
	BackendMalformed = "malformed"
	// BackendCancelled is a call cut short because the request it served
	// ended: the requester stopped waiting or went away. A system slower
	// than the responder's own deadline (WithBackendDeadline) is its
	// timeout instead. The exchange is not recorded as an upstream error.
	BackendCancelled = "cancelled"
)

// The closed lists. Each ends in ExchangeOther.
var (
	ExchangeDirections = []string{DirectionIngress, DirectionInbound, ExchangeOther}
	ExchangeRoutes     = []string{RouteCRD, RouteDTR, RoutePAS, RoutePASInquire, RouteSubstrateInbound, ExchangeOther}
	// ExchangeKinds is every leg of the PA catalog (paCatalog), pinned equal
	// by TestExchangeKindsAreThePACatalog.
	ExchangeKinds = []string{
		"coverage-eligibility", "dtr-questionnaire-fetch", "federated-query", "patient-dtr",
		"crd-order-select", "crd-order-dispatch", "pas-claim", "pas-claim-update", "pas-claim-inquire",
		ExchangeOther,
	}
	// ExchangeOperations are the CDS hooks the CRD legs carry and the DTR
	// operations a request frame names.
	ExchangeOperations  = []string{"order-select", "order-sign", "order-dispatch", shnsdk.FrameOperationQuestionnairePackage, shnsdk.FrameOperationNextQuestion, ExchangeOther}
	ExchangeOutcomes    = []string{ExchangeAnswered, ExchangeRefused, ExchangeUnreachable, ExchangeUpstreamError, ExchangeOther}
	RefusalParties      = []string{RefusedByProviderGateway, RefusedByHub, RefusedByAuthorizationFramework, RefusedByPayerGateway, ExchangeOther}
	RefusalRules        = []string{RefusalAuthentication, RefusalAuthority, RefusalConsent, RefusalRouting, RefusalReplay, RefusalIntegrity, RefusalAudit, RefusalFidelity, RefusalConformance, RefusalLimit, ExchangeOther}
	BackendErrorClasses = []string{BackendTimeout, BackendConnect, BackendTLS, BackendAuth, BackendHTTP3xx, BackendHTTP4xx, BackendHTTP5xx, BackendRead, BackendMalformed, BackendCancelled, ExchangeOther}
)

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
	// outcome/party/rule are the settled outcome, set by decided.
	outcome, party, rule string
	// appStatus is the status inside an inbound answer's frame.
	appStatus int
	// guardRule is the rule of a check that refused after the last decided
	// outcome; it names the refusal when the answer written is non-2xx.
	guardRule string
	// self is the party this gateway is.
	self string
	// forwarded is set once the operation has been forwarded to the
	// participant's own system (backend); reads after it do not replace it.
	forwarded bool
	// cut is set when any call to the participant's system was cut short
	// because the request ended, the Backend or a read after it.
	cut bool
	// now is the gateway's clock.
	now func() time.Time
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

// decided settles the outcome. A refusal that names no rule takes the rule of
// the check that refused since the last decided outcome. by "" is this
// gateway.
func (x *exchangeRecorder) decided(outcome, by, rule string) {
	x.with(func() {
		if outcome == ExchangeRefused {
			if rule == "" {
				rule = x.guardRule
			}
			if by == "" {
				by = x.self
			}
		} else {
			by, rule = "", ""
		}
		x.outcome, x.party, x.rule, x.guardRule = outcome, by, rule, ""
	})
}

// routed notes a payer-routing answer: the 422 a request is answered when its
// coverage names no payer the network registers. Any other status is left to
// the answer.
func (x *exchangeRecorder) routed(status int) {
	if status == http.StatusUnprocessableEntity {
		x.refused(RefusalRouting)
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

// refused settles a refusal by this gateway on rule.
func (x *exchangeRecorder) refused(rule string) { x.decided(ExchangeRefused, "", rule) }

// refusing names the rule of a refusal about to be written, for the outcome
// the refusal's answer settles.
func (x *exchangeRecorder) refusing(rule string) { x.with(func() { x.guardRule = rule }) }

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
			x.guardRule = refusalRuleFor(kind, rule)
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

// backend notes the call that asks the participant's own system for the
// answer (the operation forwarded to it): it is the exchange's Backend.
func (x *exchangeRecorder) backend(status int, latency time.Duration, class string) {
	x.with(func() {
		x.rec.BackendCalls++
		x.rec.Backend = &BackendCall{Status: status, Latency: latency, ErrorClass: class}
		x.forwarded = true
		x.cut = x.cut || class == BackendCancelled
	})
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
		if !x.forwarded {
			x.rec.Backend = &BackendCall{Status: status, Latency: latency, ErrorClass: class}
		}
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

// sorFailure settles an answering-side refusal caused by a failed read of
// the participant's own system of record (the read itself is noted where it
// ran, recordingSoR): that system not answering usably, an upstream failure.
// A read the request's end cut short was noted as such (readEndedClass), and
// the exchange settles "other" (emitExchange), whatever the connector
// returned: a connector's own cancellation is its system not answering.
func (x *exchangeRecorder) sorFailure() {
	x.decided(ExchangeUpstreamError, "", "")
}

// sorClass is the class of a read of the participant's own system of record:
// the same categories SoRFailureResponse answers with.
func sorClass(err error) string {
	var readErr *SoRReadError
	switch {
	case err == nil:
		return ""
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

// refusalRuleFor names the network rule behind a refusing check.
func refusalRuleFor(kind CheckKind, rule string) string {
	switch {
	case kind == KindFHIRBridged:
		return RefusalFidelity
	case rule == RuleSubjectPCI, rule == RuleSubjectToken:
		return RefusalAuthority
	case rule == RuleDuplicateKey:
		return RefusalIntegrity
	}
	return RefusalConformance
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
	case errors.As(err, &cert), errors.As(err, &hostname), errors.As(err, &unknownAuth), errors.As(err, &record):
		return BackendTLS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return BackendTimeout
	case errors.As(err, &op) && op.Op == "dial":
		return BackendConnect
	}
	return ExchangeOther
}

// hubRules are the Hub's own refusals of a leg (internal/hubsvc), by the
// reason it answers, with the network rule each is. Only these name a rule:
// any other answer from the Hub is "other".
var hubRules = map[string]string{
	"missing X-Holder-Assertion header":      RefusalAuthentication,
	"invalid X-Holder-Assertion encoding":    RefusalAuthentication,
	"invalid X-Holder-Assertion JSON":        RefusalAuthentication,
	"unknown sender":                         RefusalAuthentication,
	"assertion holder id mismatch":           RefusalAuthentication,
	"assertion verification failed":          RefusalAuthentication,
	"invalid authz token":                    RefusalAuthority,
	"authz token verification failed":        RefusalAuthority,
	"response leg authorization failed":      RefusalAuthority,
	"replay detected":                        RefusalReplay,
	"stale or future timestamp":              RefusalReplay,
	"unknown recipient":                      RefusalRouting,
	"read body failed":                       RefusalIntegrity,
	"decode envelope failed":                 RefusalIntegrity,
	"missing authority frame":                RefusalIntegrity,
	"missing correlation id":                 RefusalIntegrity,
	"payload hash mismatch":                  RefusalIntegrity,
	"decode recipient response failed":       RefusalIntegrity,
	"response leg recipient mismatch":        RefusalIntegrity,
	"response leg sender mismatch":           RefusalIntegrity,
	"response leg transaction-type mismatch": RefusalIntegrity,
	"audit append failed":                    RefusalAudit,
	"response audit append failed":           RefusalAudit,
}

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
	reason := e.reason
	if strings.HasPrefix(reason, "forward to recipient failed") {
		switch {
		case strings.Contains(reason, "the recipient refused it"):
			return ExchangeRefused, RefusedByPayerGateway, ExchangeOther
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
	switch {
	case err == nil:
		x.decided(ExchangeAnswered, "", "")
	case errors.As(err, &re):
		x.decided(ExchangeUpstreamError, "", "")
	case errors.Is(err, errAuthorizationDenied):
		x.decided(ExchangeRefused, RefusedByAuthorizationFramework, RefusalAuthority)
	case errors.Is(err, errHubUnreachable), errors.Is(err, errHubTimeout):
		x.decided(ExchangeUnreachable, "", "")
	case errors.As(err, &hr):
		x.decided(hubRefusalOutcome(hr))
	case errors.As(err, &lost) && lost.cause == "the Hub's answer could not be read":
		// A read that failed on the way back: transport, not integrity.
		x.decided(ExchangeOther, "", "")
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
}

func (w *exchangeWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 { // a 1xx is not the answer
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *exchangeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ingressRoutes are the Da Vinci ingress handlers, with the observer tag each
// route has always carried.
var ingressRoutes = map[string]struct {
	tag     string
	handler func(*Gateway) http.HandlerFunc
}{
	RouteCRD:        {"crd-ingress", func(g *Gateway) http.HandlerFunc { return g.handleCRDIngress }},
	RouteDTR:        {"dtr-ingress", func(g *Gateway) http.HandlerFunc { return g.handleDTRIngress }},
	RoutePAS:        {"pas-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASIngress }},
	RoutePASInquire: {"pas-inquire-ingress", func(g *Gateway) http.HandlerFunc { return g.handlePASInquireIngress }},
}

// ingressRoute is the handler a Da Vinci ingress route is mounted with: the
// exchange record outermost, so it sees every answer the others write.
func (g *Gateway) ingressRoute(route string) http.HandlerFunc {
	r := ingressRoutes[route]
	return g.recordExchange(route, DirectionIngress, g.observeIngress(r.tag, g.withIngressCorrelation(r.handler(g))))
}

// inboundRoute is the handler /substrate/inbound is mounted with.
func (g *Gateway) inboundRoute() http.HandlerFunc {
	return g.recordExchange(RouteSubstrateInbound, DirectionInbound, g.observeInbound(g.handleInbound))
}

// recordExchange wraps a route so each call produces one ExchangeRecord.
func (g *Gateway) recordExchange(route, direction string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.ExchangeObserved == nil {
			h(w, r)
			return
		}
		x := &exchangeRecorder{self: g.exchangeParty(direction), now: g.exchangeClock}
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
		}
		ew := &exchangeWriter{ResponseWriter: w, x: x}
		panicked := true
		defer func() {
			g.emitExchange(x, ew, panicked)
		}()
		h(ew, r.WithContext(context.WithValue(r.Context(), exchangeRecorderKey{}, x)))
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

// exchangeParty is the refusing party this gateway is on direction.
func (g *Gateway) exchangeParty(direction string) string {
	switch {
	case direction == DirectionIngress:
		return RefusedByProviderGateway
	case g.cfg.Role == "payer":
		return RefusedByPayerGateway
	}
	return ExchangeOther
}

// emitExchange settles the record and hands it to Config.ExchangeObserved. A
// panicking consumer never reaches the request.
func (g *Gateway) emitExchange(x *exchangeRecorder, w *exchangeWriter, panicked bool) {
	defer func() { _ = recover() }()
	x.mu.Lock()
	rec := x.rec
	rec.Findings.Kinds = slices.Clone(rec.Findings.Kinds)
	if rec.Backend != nil {
		b := *rec.Backend
		rec.Backend = &b
	}
	outcome, by, rule, appStatus, guardRule := x.outcome, x.party, x.rule, x.appStatus, x.guardRule
	backendFailed := rec.Backend != nil && rec.Backend.ErrorClass != "" && rec.Backend.ErrorClass != BackendCancelled
	backendCancelled := x.cut
	x.mu.Unlock()

	rec.Latency = g.exchangeClock().Sub(rec.Start)
	rec.Status = w.status
	if rec.Status == 0 && !panicked {
		rec.Status = http.StatusOK // a handler that wrote nothing answered 200
	}
	if rec.Direction == DirectionIngress {
		rec.CorrelationID = w.Header().Get(LegIDHeader)
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
		outcome, by, rule = ExchangeOther, "", ""
	case guardRule != "" && !ok:
		// A check refused after the last settled outcome, and the answer
		// says so.
		outcome, by, rule = ExchangeRefused, x.self, guardRule
	case outcome == ExchangeAnswered && !ok:
		// The leg was answered, and this gateway answered its caller with an
		// error anyway: its own refusal, on a rule nothing named.
		outcome, by, rule = ExchangeRefused, x.self, ExchangeOther
	case outcome != "":
	case ok:
		outcome = ExchangeAnswered
	case rec.Status/100 == 4:
		outcome, by, rule = ExchangeRefused, x.self, ExchangeOther
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
	}
	if rec.Backend != nil && rec.Backend.ErrorClass != "" {
		rec.Backend.ErrorClass = closed(BackendErrorClasses, rec.Backend.ErrorClass)
	}
	g.cfg.ExchangeObserved(rec)
}
