//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer's CDex attachment request (CDex 2.1.0 Task Attachment Request,
// D127): a payer's system asks a provider for documentation for a prior
// authorization, through its own gateway, the Hub and the provider's gateway,
// to the provider's own Task endpoint. The network carries the Task as sent,
// apart from re-keying its payer-url to the provider gateway's return path for
// the payer that sent it (E-11), so the provider's documentation comes back
// through the network (cdex-attachment-submit), and naming the payer that sent
// it outside the body (RequesterHeader).
//
// A provider's gateway carries the Task only to a Task endpoint its own
// participant sets (Config.ProviderCDexTaskURL, PROVIDER_CDEX_TASK_URL), and
// only then declares the leg (TakenLegs). A provider that sets none answers
// the leg with the default the design rules for it: a routing refusal, made
// at every conformance level.

const legCDexRequest = "cdex-attachment-request"

var legCDexAttachmentRequest = inboundLeg{"provider-disclosure", "cdex-attachment-request-response", legCDexRequest}

// refusalCDexRequestNotTaken: a provider gateway whose participant sets no
// Task endpoint for its own system has nowhere to carry a payer's
// attachment-request Task.
const refusalCDexRequestNotTaken = "this provider takes no attachment-request Task: its gateway has no Task endpoint of its own system to carry it to"

// RequesterHeader names, on the Task forward to the provider's own system,
// the payer the network authenticated as the Task's sender: its participant
// id, then `; identifiers="<system>|<value> …"`, the payer identities the
// network's registry attests for it, space-separated, when it has any. The
// provider's gateway sets it on the forward alone and never reads it from a
// caller, so a provider's release decision need not rest on the Task's own
// requester.
const RequesterHeader = "X-SHN-Requester"

// CDexRequesterDiffersEvent is the observer event a provider's gateway raises
// when a Task's requester identifier is not one the network attests for the
// payer that sent it. The Task is carried as sent; the event carries no
// identifier.
const CDexRequesterDiffersEvent = "cdex.requester-differs"

// refusalCDexTaskRekey opens the fidelity refusal of a Task whose return-path
// re-key (E-11) cannot be made.
const refusalCDexTaskRekey = "the gateway cannot re-key the Task's documentation return address to the network (fidelity of SHN's own edit): "

// handleCDexRequestInbound answers a payer's attachment-request Task the Hub
// delivered to this provider's gateway. With no Task endpoint of its own
// system it refuses the leg as routing, before reading the Task. Otherwise it
// decides on the Task as the payer's ingress does (workstream_pa_cdex_task.go)
// and binds it to the patient its member number names, by its own system, as
// every inbound leg does; then it forwards the Task, re-keyed and naming its
// requester, and relays the provider's answer as its system wrote it.
func (g *Gateway) handleCDexRequestInbound(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	ctx := r.Context()
	st := g.paState()
	if st == nil || st.cdexTask == nil {
		exchangeOf(ctx).refusing(RefusalRouting)
		g.refuseInbound(w, r, legCDexAttachmentRequest, env, tok, answerTok, http.StatusUnprocessableEntity, refusalCDexRequestNotTaken, nil)
		return
	}
	refuse := func(rule string, status int, msg string) {
		exchangeOf(ctx).refusing(rule)
		g.refuseInbound(w, r, legCDexAttachmentRequest, env, tok, answerTok, status, msg, nil)
	}
	task, err := cdextask.Read(relay.NewBody(body, relay.OriginPeerFrame))
	switch {
	case readsTwoWays(err):
		refuse(RefusalIntegrity, http.StatusBadRequest, errCDexRequestTwoWays)
		return
	case err != nil:
		refuse(RefusalAuthority, http.StatusForbidden, errCDexRequestNotTask)
		return
	case !task.Code.Has(cdextask.PASTempCodes, cdextask.CodeAttachmentRequest):
		refuse(RefusalAuthority, http.StatusForbidden, errCDexRequestCode)
		return
	}
	member, ok := cdexTaskMember(task)
	if !ok {
		refuse(RefusalAuthority, http.StatusUnprocessableEntity, errCDexRequestNoPatient)
		return
	}
	subjectPCI, status, msg := g.bindInboundSubjectAs(ctx, member, body, http.StatusUnprocessableEntity)
	if status != 0 {
		g.refuseInbound(w, r, legCDexAttachmentRequest, env, tok, answerTok, status, msg, nil)
		return
	}
	g.noteSubjectBinding(ctx, legCDexRequest, env.Metadata.CorrelationID, tok.Subject, subjectPCI)
	payer := env.Metadata.Sender
	ids := g.attestedPayerIDs(payer)
	g.noteCDexRequester(task, ids, env.Metadata.CorrelationID)
	payload, err := g.cdexTaskReturnRekey(ctx, body, "application/fhir+json", payer)
	if err != nil {
		g.refuseCDexTaskRekey(w, r, env, tok, answerTok, err)
		return
	}
	// The forward is the leg's one call to the provider's system: it carries
	// the leg's id as every forward to a participant's system does, and is
	// tracked, so a failure can say whether that system was asked.
	ctx = withNativeForward(withResponderCorrelation(ctx, env.Metadata.CorrelationID))
	exchangeOf(ctx).trackForward()
	ctx = withForwardHeaders(ctx, http.Header{RequesterHeader: {requesterHeaderValue(payer, ids)}})
	result, err := st.cdexTask.forwardCDexTask(ctx, payload)
	r = r.WithContext(ctx)
	if err != nil {
		g.responderFailed(w, r, legCDexAttachmentRequest, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, legCDexAttachmentRequest.frame, legCDexAttachmentRequest.op, legCDexRequest,
			env.Metadata.CorrelationID, result, tok.Subject, payer, "", answerTok)
		return
	}
	g.respondLeg(w, r, legCDexAttachmentRequest.frame, legCDexAttachmentRequest.op, legCDexRequest, env.Metadata.CorrelationID,
		result.Response, result.stated(), tok.Subject, payer, "", answerTok)
}

// refuseCDexTaskRekey writes the fidelity refusal of a re-key
// cdexTaskReturnRekey could not make: the Task is not forwarded with its
// return address off the network. No return path to write is the build of
// the edit failing; any other failure is its verification.
func (g *Gateway) refuseCDexTaskRekey(w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, answerTok string, err error) {
	if errors.Is(err, errNoReturnPath) {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageBuild)
	} else {
		exchangeOf(r.Context()).refusingFidelity(vocab.RefusalStageVerify)
	}
	g.refuseInbound(w, r, legCDexAttachmentRequest, env, tok, answerTok, http.StatusUnprocessableEntity, refusalCDexTaskRekey+err.Error(), nil)
}

// attestedPayerIDs are the payer identities the network's registry attests
// for payer (its operator-attested payerIds), none when it attests none.
func (g *Gateway) attestedPayerIDs(payer string) []shnsdk.PayerIdentifier {
	e, ok := g.cfg.Reg.Lookup(payer)
	if !ok {
		return nil
	}
	return append([]shnsdk.PayerIdentifier(nil), e.PayerIDs...)
}

// requesterHeaderValue is RequesterHeader's value for payer and the payer
// identities the network attests for it. A registry identity holds no
// whitespace and no '|' (the registrar's rule), so the list reads back
// unambiguously; a quote or a backslash in one is escaped as a
// quoted-string's.
func requesterHeaderValue(payer string, ids []shnsdk.PayerIdentifier) string {
	if len(ids) == 0 {
		return payer
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.System + "|" + id.Value
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(strings.Join(parts, " "))
	return payer + `; identifiers="` + esc + `"`
}

// noteCDexRequester raises CDexRequesterDiffersEvent when the Task's
// requester identifier is not one the network attests for the payer that
// sent it, or cannot be read: either way the Task does not say it is the
// payer's own. A Task that names its requester by a reference alone, or names
// none, states no identifier to compare. The Task is carried as sent.
func (g *Gateway) noteCDexRequester(task cdextask.Task, attested []shnsdk.PayerIdentifier, corrID string) {
	id := task.Requester.Identifier
	differs := false
	switch {
	case task.Requester.State == cdextask.Unreadable || id.State == cdextask.Unreadable:
		differs = true
	case id.State == cdextask.Present:
		differs = true
		for _, a := range attested {
			if a.System == id.Identifier.System && a.Value == id.Identifier.Value {
				differs = false
				break
			}
		}
	}
	if differs {
		g.observe(ObserverEvent{Kind: CDexRequesterDiffersEvent, Direction: "ingress", LegType: legCDexRequest, CorrelationID: corrID})
	}
}

// forwardCDexTask sends the Task to the provider's own Task endpoint, as
// prepared (relayed exactly, or with its return path re-keyed), and relays
// whatever comes back with the provider's own media type: its 201 and an
// error alike.
func (n *nativeResponder) forwardCDexTask(ctx context.Context, p relay.Payload) (LegResult, error) {
	up, bad, err := n.post(endpointcert.WithTag(ctx, endpointcert.Of(endpointcert.CDexTask)), n.baseURL, "", p, legCDexRequest, "CDex Task create")
	if err != nil {
		return LegResult{}, err
	}
	if bad.Status != 0 {
		return bad, nil
	}
	return LegResult{Response: relay.Exact(up.body, up.contentType), AnswerStatus: up.status, AnswerHeaders: up.headers, ResponseSubjectForeign: true}, nil
}
