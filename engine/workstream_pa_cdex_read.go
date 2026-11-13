//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer's read of its CDex Task (cdex-task-status, D129): a payer's system
// reads the attachment-request Task it created at a provider, through its own
// gateway, the Hub and the provider's gateway, to learn the Task's status. The
// read is a plain FHIR read or vread. It names no patient, so the leg is
// flagged (subject: from-answer): its request is authorized with no patient,
// and the provider's gateway binds the patient from the answer.
//
// The provider's gateway reads the Task below its own participant's Task
// endpoint (Config.ProviderCDexTaskURL), with a token asked for the read
// scope its participant sets (Config.ProviderCDexReadScope), and only then
// declares the leg (TakenLegs). A provider that sets none answers the leg as
// routing, at every conformance level.
//
// On the provider's answer, in this order:
//  1. an answer that is not a 2xx is carried as sent, under no patient;
//  2. a 2xx answer is delivered only when it is one JSON Task whose one
//     structural payer-url is this gateway's own return path for the reading
//     payer (E-11's value): every other answer gets the network's 404, the
//     same bytes in every case, before any patient lookup (D129, Q2);
//  3. a Task that passes is bound to its patient by D121's rule, and one that
//     cannot be bound is refused 502 (D129, Q1);
//  4. the Task is relayed exactly, under that patient.

const legCDexTaskStatus = "cdex-task-status"

var legCDexTaskRead = inboundLeg{"provider-disclosure", "cdex-task-read-response", legCDexTaskStatus}

// The provider gateway's refusals of a payer's read.
const (
	// refusalCDexReadNotTaken: a provider gateway whose participant sets no
	// read scope reads no Task of its own system for a payer.
	refusalCDexReadNotTaken = "this provider takes no payer's read of its Task: its gateway reads no Task of its own system for payers"
	// refusalCDexReadBody: a read carries no body.
	refusalCDexReadBody = "a Task read carries no body"
	// refusalCDexReadNoPath: the request frame names no read path.
	refusalCDexReadNoPath = "the request names no Task to read: its frame carries no readPath"
	// refusalCDexReadPath opens the refusal of a read path that is not one
	// Task's read or vread.
	refusalCDexReadPath = "the request's readPath is not a Task read or vread (Task/{id} or Task/{id}/_history/{vid}, each a FHIR id, then an optional query): "
	// refusalCDexReadUnbound opens the refusal of a delivered Task the
	// provider's gateway cannot bind to a patient (D121): the network cannot
	// then identify the patient whose record it would disclose.
	refusalCDexReadUnbound = "the provider's gateway cannot bind the Task to a patient, so it is not delivered: "
	// refusalCDexReadTarget: the read's address below the Task endpoint
	// could not be built.
	refusalCDexReadTarget = "the provider's gateway cannot build the Task's read address below its Task endpoint; nothing was sent"
)

// The causes refusalCDexReadUnbound names.
const (
	cdexReadUnboundNoMember = "Task.for names no one member number (an identifier typed v2-0203#MB), and is not a reference Patient/{id} the gateway can read"
	cdexReadUnboundBases    = "Task.for is a reference Patient/{id}, and the provider's Task endpoint is not under its system of record's FHIR base (FHIR_DATA_URL), where the gateway reads that Patient: serve Tasks under the system of record's base, or name the member on the Task's subject (an identifier typed v2-0203#MB)"
	cdexReadUnboundNotHeld  = "the provider's system of record holds no Patient that Task.for names"
	// The provider's system answered from another address than the Task
	// endpoint, so the endpoint's base says nothing of which server's
	// Patient a bare reference names.
	cdexReadUnboundRedirected = "Task.for is a reference Patient/{id}, and the provider's system answered the read from another address after a redirect, so the gateway cannot tell which server that Patient is on: answer the read at the Task endpoint itself, or name the member on the Task's subject (an identifier typed v2-0203#MB)"
	cdexReadUnboundPatient    = "the Patient Task.for names carries no one member identifier (system " + shnsdk.MemberSystem + ")"
)

// cdexReadNotFound is the network's answer to a provider's 2xx that the
// reading payer may not be delivered (D129, Q2): an OperationOutcome this
// gateway writes, the same bytes whatever is wrong with the answer, so it
// says nothing of the Task or of why. FHIR R4 allows a 404 for access denied
// (security.html, Access Denied Response Handling).
var cdexReadNotFound = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"not-found","diagnostics":"Task not found"}]}`)

// cdexReadNotFoundMessage is cdexReadNotFound's text.
const cdexReadNotFoundMessage = "Task not found"

// cdexReadState is how a provider's gateway reads a Task below its own
// participant's Task endpoint: the client it reads with, authorized for the
// read scope, and the endpoint as a URL a read is made below.
type cdexReadState struct {
	client  *http.Client
	taskURL *url.URL
}

// newCDexReadState is a provider gateway's read state, nil when its
// participant sets no read scope. The Task endpoint must be one a read is made
// below (cdexroute.TaskReadURL); New refuses a gateway whose endpoint is not.
func newCDexReadState(cfg Config) (*cdexReadState, error) {
	if strings.TrimSpace(cfg.ProviderCDexReadScope) == "" || cfg.Role != "provider" {
		return nil, nil
	}
	u, err := cdexroute.TaskReadURL(strings.TrimSpace(cfg.ProviderCDexTaskURL))
	if err != nil {
		return nil, fmt.Errorf("gateway: ProviderCDexReadScope (PROVIDER_CDEX_READ_SCOPE) requires ProviderCDexTaskURL (PROVIDER_CDEX_TASK_URL) to be a Task endpoint a read is made below: %w", err)
	}
	return &cdexReadState{client: cfg.ProviderCDexReadClient, taskURL: u}, nil
}

// handleCDexTaskReadInbound answers a payer's read of its Task the Hub
// delivered to this provider's gateway (the order above). With no read scope
// it refuses the leg as routing, before reading anything.
func (g *Gateway) handleCDexTaskReadInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	ctx := r.Context()
	refuse := func(rule string, status int, msg string) {
		exchangeOf(ctx).refusing(rule)
		g.refuseInbound(w, r, legCDexTaskRead, env, tok, answerTok, status, msg, nil)
	}
	st := g.paState()
	if st == nil || st.cdexTask == nil || st.cdexRead == nil {
		refuse(RefusalRouting, http.StatusUnprocessableEntity, refusalCDexReadNotTaken)
		return
	}
	read := st.cdexRead
	if len(body) != 0 {
		refuse(RefusalIntegrity, http.StatusBadRequest, refusalCDexReadBody)
		return
	}
	raw, ok := requestFrameReadPath(ctx)
	if !ok {
		refuse(RefusalRouting, http.StatusBadRequest, refusalCDexReadNoPath)
		return
	}
	path, err := shnsdk.ParseCDexTaskReadPath(raw)
	if err != nil {
		refuse(RefusalRouting, http.StatusBadRequest, refusalCDexReadPath+err.Error())
		return
	}
	payer := env.Metadata.Sender
	ids := g.attestedPayerIDs(payer)
	tail := path.ID
	if path.VersionID != "" {
		tail += "/_history/" + path.VersionID
	}
	target, err := cdexroute.ReadTarget(read.taskURL, tail, path.RawQuery, path.HasQuery)
	if err != nil {
		// The parser admits FHIR ids alone, so this is this gateway's own
		// failure: nothing is sent rather than a read of another address.
		exchangeOf(ctx).ownFailure()
		g.refuseInbound(w, r, legCDexTaskRead, env, tok, answerTok, http.StatusInternalServerError, refusalCDexReadTarget, nil)
		return
	}
	// The read is the leg's one call to the provider's system, tracked and
	// named as the attachment request's forward is.
	ctx = withNativeForward(withResponderCorrelation(ctx, env.Metadata.CorrelationID))
	exchangeOf(ctx).trackForward()
	ctx = withForwardHeaders(ctx, http.Header{RequesterHeader: {requesterHeaderValue(payer, ids)}})
	up, bad, err := st.cdexTask.readCDexTask(ctx, read.client, target, body)
	r = r.WithContext(ctx)
	if err != nil {
		g.responderFailed(w, r, legCDexTaskRead, env, tok, answerTok, err)
		return
	}
	if bad.Status != 0 {
		// The provider's own answer (its 404, its 410, its 401 after the one
		// resend) is carried as sent, under no patient: it discloses none.
		g.respondLegError(w, r, legCDexTaskRead.frame, legCDexTaskRead.op, legCDexTaskStatus,
			env.Metadata.CorrelationID, bad, "", payer, "", answerTok)
		return
	}
	task, ok := g.cdexReadDelivers(up.body, payer)
	if !ok {
		g.refuseCDexReadNotFound(w, r, env, tok, answerTok)
		return
	}
	subjectPCI, status, msg := g.bindCDexReadSubject(r.Context(), task, read.taskURL, up.redirected, up.raw)
	if status != 0 {
		g.refuseInbound(w, r, legCDexTaskRead, env, tok, answerTok, status, msg, nil)
		return
	}
	g.noteCDexRequester(task, ids, env.Metadata.CorrelationID, legCDexTaskStatus)
	result := LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers, ResponseSubjectForeign: true}
	g.respondLeg(w, r, legCDexTaskRead.frame, legCDexTaskRead.op, legCDexTaskStatus, env.Metadata.CorrelationID,
		result.Response, result.stated(), subjectPCI, payer, "", answerTok)
}

// refuseCDexReadNotFound writes the network's 404 (cdexReadNotFound) for a
// provider's answer the reading payer may not be delivered, recorded as the
// leg's authority and as this gateway's own refusal, under no patient.
func (g *Gateway) refuseCDexReadNotFound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, answerTok string) {
	exchangeOf(r.Context()).refusing(RefusalAuthority)
	p, err := relay.Authored(relay.BuilderGatewayRefusal, cdexReadNotFound, "application/fhir+json")
	if err != nil {
		writeOwnershipFault(w)
		return
	}
	g.respondLegError(w, r, legCDexTaskRead.frame, legCDexTaskRead.op, legCDexTaskStatus, env.Metadata.CorrelationID,
		LegResult{Status: http.StatusNotFound, Message: cdexReadNotFoundMessage, Response: p}, "", env.Metadata.Sender, "", answerTok)
}

// cdexReadDelivers reports whether a provider's 2xx answer may be delivered
// to payer (D129, Q2), and the Task it reads as when it may: exactly one JSON
// Task, carrying exactly one structural payer-url input whose valueUrl is this
// gateway's own return path for payer (cdexroute.ReturnBase, the value E-11
// writes), compared byte for byte. Nothing else of the answer is read first.
func (g *Gateway) cdexReadDelivers(answer relay.Body, payer string) (cdextask.Task, bool) {
	want, err := cdexroute.ReturnBase(g.cfg.IngressBaseURL, payer)
	if err != nil {
		// No return path for this payer: no Task is its own.
		return cdextask.Task{}, false
	}
	task, err := cdextask.Read(answer)
	if err != nil || task.PayerURL.State != cdextask.Present || task.PayerURL.Value != want {
		return cdextask.Task{}, false
	}
	return task, true
}

// bindCDexReadSubject binds a delivered Task to its patient by D121's rule,
// returning the patient's identifier, or the status and text the read is
// refused with:
//   - a member number Task.for names (cdexTaskMember), bound as the
//     attachment request binds it;
//   - otherwise a Task.for that is only a reference Patient/{id}, read from
//     this gateway's system of record when the Task endpoint is served under
//     that system's FHIR base (sameReadBase) and answered the read itself
//     (redirected false: a read answered after a redirect came from another
//     address, whose base the endpoint's says nothing of), and bound by the member
//     identifier the system names the Patient by (shnsdk.MemberSystem, the
//     identifier the system of record resolves a member by), with the
//     Patient's own bytes;
//   - anything else is refused 502 (authority): the network cannot identify
//     the patient whose record it would disclose. A read of the system of
//     record that fails is that system's failure (subjectBindingFailure).
func (g *Gateway) bindCDexReadSubject(ctx context.Context, task cdextask.Task, taskURL *url.URL, redirected bool, answer []byte) (string, int, string) {
	unbound := func(cause string) (string, int, string) {
		exchangeOf(ctx).refusing(RefusalAuthority)
		return "", http.StatusBadGateway, refusalCDexReadUnbound + cause
	}
	bind := func(member string, payload []byte) (string, int, string) {
		pci, status, msg, recorded := g.bindInboundSubjectRecorded(ctx, member, payload, http.StatusBadGateway)
		switch {
		case status == 0:
			return pci, 0, ""
		case status == http.StatusBadRequest:
			// A member the system of record does not hold, for a participant
			// that requires known members (recorded as conformance), or one
			// it reports without an identifier (no rule recorded: the patient
			// cannot be identified, the leg's authority): the patient cannot
			// be bound. It is not the payer's request at fault, so it is not
			// a 400.
			if !recorded {
				exchangeOf(ctx).refusing(RefusalAuthority)
			}
			return "", http.StatusBadGateway, refusalCDexReadUnbound + msg
		}
		return "", status, msg
	}
	if member, ok := cdexTaskMember(task); ok {
		return bind(member, answer)
	}
	id, ok := barePatientReference(task.Patient)
	if !ok {
		return unbound(cdexReadUnboundNoMember)
	}
	if redirected {
		return unbound(cdexReadUnboundRedirected)
	}
	if !sameReadBase(cdexroute.TaskReadBase(taskURL), sorFHIRBase(g.cfg.SoR)) {
		return unbound(cdexReadUnboundBases)
	}
	patient, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, "Patient/"+id)
	if err != nil {
		return subjectBindingRefusal(ctx, err)
	}
	if !found {
		return unbound(cdexReadUnboundNotHeld)
	}
	member, ok := sorPatientMember(patient)
	if !ok {
		return unbound(cdexReadUnboundPatient)
	}
	return bind(member, patient)
}

// subjectBindingRefusal is subjectBindingFailure as bindCDexReadSubject
// returns it.
func subjectBindingRefusal(ctx context.Context, err error) (string, int, string) {
	status, msg := subjectBindingFailure(ctx, err)
	return "", status, msg
}

// barePatientReference is the id of a Task.for that is only a relative
// reference Patient/{id} into the provider's own server: no identifier on
// the reference, no contained resource, and an id that is a FHIR id other
// than a dot segment. Any other Task.for (absolute, contained, of another
// type, with an identifier, or a version) names none.
func barePatientReference(p cdextask.Patient) (string, bool) {
	if p.State != cdextask.Present || p.Contained || p.Identifier.State != cdextask.Absent {
		return "", false
	}
	id, ok := strings.CutPrefix(p.Reference, "Patient/")
	if !ok || id == "." || id == ".." || !idShape.MatchString(id) {
		return "", false
	}
	return id, true
}

// sorPatientMember is the member the system of record names a Patient it
// answered by: the value of its one identifier of system shnsdk.MemberSystem,
// the identifier the system resolves a member by. ok is false for a body that
// is not a Patient, or one naming no such identifier or two that differ.
func sorPatientMember(patient []byte) (string, bool) {
	var p map[string]any
	if json.Unmarshal(patient, &p) != nil || p["resourceType"] != "Patient" {
		return "", false
	}
	return containedPatientMember(p)
}

// sameReadBase reports whether a Task endpoint's FHIR base (its path less its
// last segment, Task) and the system of record's FHIR base are one base. It is
// not sameFHIRBase, the CRD fhirServer comparison, on purpose, in two ways:
// sameFHIRBase trims every ending slash, where this ignores one only (a base
// ending "//" names another path); and sameFHIRBase admits "." and ".."
// segments, which a server may resolve to another path, where a base holding
// one is never the same as any here, so no Patient is read from a system whose
// base only resolves to the endpoint's. The bases are compared by structure: absolute http or https URLs with no user
// information, query or fragment, their schemes and hosts compared without
// regard to case, a scheme's default port (80 for http, 443 for https)
// dropped, one ending slash of the path ignored, and the rest of the path
// compared exactly, as escaped. A path with a "." or ".." segment, spelled
// or escaped, is never the same as any; nor is an empty, unparsable or
// non-ASCII base.
func sameReadBase(a, b string) bool {
	na, okA := normalReadBase(a)
	nb, okB := normalReadBase(b)
	return okA && okB && na == nb
}

func normalReadBase(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "https" && scheme != "http") || u.Hostname() == "" {
		return "", false
	}
	for _, c := range raw {
		if c > 0x7f {
			return "", false
		}
	}
	path := u.EscapedPath()
	for _, seg := range strings.Split(path, "/") {
		if v, err := url.PathUnescape(seg); err == nil && (v == "." || v == "..") {
			return "", false
		}
	}
	path = strings.TrimSuffix(path, "/")
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + path, true
}

// readCDexTask reads a Task at target, below the provider's own Task
// endpoint, with client (authorized for the read scope), carrying the
// network's request, which is empty, relayed exactly. Whatever comes back is
// the provider's answer: a non-2xx as bad, for relay as sent; a 2xx as up,
// for the delivery rule.
func (n *nativeResponder) readCDexTask(ctx context.Context, client *http.Client, target *url.URL, body []byte) (upstreamReply, LegResult, error) {
	p := relay.Exact(relay.NewBody(body, relay.OriginPeerFrame), "")
	return n.getVia(endpointcert.WithTag(ctx, endpointcert.Of(endpointcert.CDexTask)), client, target, p, legCDexTaskStatus, "CDex Task read")
}
