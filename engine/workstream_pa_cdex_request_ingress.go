//go:build !shn_coreonly

package engine

import (
	"errors"
	"net/http"
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The payer gateway's Da Vinci ingress for the payer's CDex attachment
// request (D127): the payer's own system, authenticated as a registered client
// with a bearer granted a scope that permits creating a Task (taskCreate:
// TaskWriteScope, system/Task.* or a v2 scope with c), creates a CDex 2.1.0
// Task Attachment Request at a provider's base on its gateway
// (POST <ingress base>/cdex/provider/{provider}/Task), and the network carries
// it to that provider as sent. The network refuses one only on its own
// primitives:
//   - a body that reads two ways (message integrity);
//   - a Task outside the leg's scope: not a Task, or a Task.code other than
//     PASTempCodes#attachment-request-code or #attachment-request-questionnaire
//     (the leg's authority; cdexRequestCodeTaken);
//   - a Task it cannot route: a base that names no registered provider
//     (routing);
//   - a Task it cannot bind to a patient: one that names no member number
//     for Task.for, or two that differ (the leg's authority, D121).
//
// The provider's answer is relayed exactly, its headers as on the other
// carried legs. A 2xx answer gains a Location naming the created Task at this
// gateway's base for the provider the Task was routed to, at either route,
// from the tail the provider's gateway framed (E-16, cdexTaskLocated); its body
// and ETag are the provider's, unchanged.
//
// A Task created at a provider's base goes to that provider whatever its
// Task.owner names; an owner naming an organization the network attests for
// another provider is recorded (CDexOwnerDiffersEvent) and carried.

// RouteCDexRequestBase is the attachment-request ingress route at a provider's
// base on this gateway, as exchange records name it.
const RouteCDexRequestBase = "cdex-request-base"

// RouteCDexRequest is the attachment-request ingress route at this gateway's
// plain Task endpoint, routed by Task.owner, as exchange records name it.
const RouteCDexRequest = "cdex-request"

// CDexProvidersPath is the payer gateway's listing of the providers a Task can
// be created for, each with its base.
const CDexProvidersPath = "/cdex/providers"

// The refusal of the attachment-request ingress that only it makes: the
// refusals the provider's gateway makes of the same Task alike are
// workstream_pa_cdex_task.go's.
const errCDexRequestNoProvider = "this base names no registered provider: " + CDexProvidersPath + " lists the providers' bases"

// The plain Task endpoint's routing refusals: the Task's owner names no one
// provider the network can route it to. Each names the primitive and where
// the Task can be created instead.
const (
	cdexOwnerElsewhere              = ": create the Task at its provider's base (" + CDexProvidersPath + " lists them)"
	errCDexRequestOwnerUnreadable   = "the network cannot route this Task (routing): its owner cannot be read" + cdexOwnerElsewhere
	errCDexRequestOwnerNoProvider   = "the network cannot route this Task (routing): its owner names no organization NPI the network attests for a registered provider" + cdexOwnerElsewhere
	errCDexRequestOwnerTwoProviders = "the network cannot route this Task (routing): its owner names organizations the network attests for different providers" + cdexOwnerElsewhere
)

// handleCDexRequestIngress is the payer-facing Task create at a provider's
// base (POST /cdex/provider/{provider}/Task): it is routed to the provider the
// base names, carried as sent, and the provider's answer comes back exactly as
// the provider wrote it.
func (g *Gateway) handleCDexRequestIngress(w http.ResponseWriter, r *http.Request) {
	w, ok := g.admitCDexRequest(w, r)
	if !ok {
		return
	}
	// Every refusal from here is recorded as this leg's.
	exchangeOf(r.Context()).leg(legCDexRequest)
	g.serveCDexRequest(w, r, func(task cdextask.Task) (string, int, string, refusalReason) { return g.cdexRequestRecipient(r) })
}

// handleCDexRequestByOwner is the payer-facing Task create at the plain Task
// endpoint (POST /Task): routed to the one registered provider the
// organization NPIs its Task.owner states are attested for, then carried and
// answered as at a provider's base.
func (g *Gateway) handleCDexRequestByOwner(w http.ResponseWriter, r *http.Request) {
	w, ok := g.admitCDexRequest(w, r)
	if !ok {
		return
	}
	// Every refusal from here is recorded as this leg's.
	exchangeOf(r.Context()).leg(legCDexRequest)
	g.serveCDexRequest(w, r, g.cdexRequestOwnerRecipient)
}

// admitCDexRequest checks the payer system's bearer for a Task route, on the
// writer scoped to the requester it returns: a refusal (401, or 403 for
// another scope) is written, before the Task is read and before the exchange
// is the leg's. ok is false when it refused.
func (g *Gateway) admitCDexRequest(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, bool) {
	w, _ = g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	return w, !g.ingressScopeRefused(w, r, taskCreate)
}

// serveCDexRequest serves the payer's Task, routed to the provider recipientOf
// names for it. An owner naming another provider than the recipient is
// recorded (noteCDexOwner); at the plain Task endpoint, where the owner names
// the recipient, it never is.
func (g *Gateway) serveCDexRequest(w http.ResponseWriter, r *http.Request, recipientOf func(cdextask.Task) (string, int, string, refusalReason)) {
	// The scope admitCDexRequest put on the writer.
	w, scope := g.withScope(w, relay.RoleRequester)
	leg := legCDexRequest
	body, ok := readIngressBody(w, r, shnsdk.MaxRequestBytes)
	if !ok {
		return
	}
	builder, ok := g.ingressBuilder(w, r, leg)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "payer-ingress", Whose: "own"}))
	task, err := cdextask.Read(relay.NewBody(body, relay.OriginIngressRequest))
	switch {
	case readsTwoWays(err):
		exchangeOf(r.Context()).refusingFor(RefusalIntegrity, reasonCDexRequestTwoWays)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errCDexRequestTwoWays})
		return
	case err != nil:
		exchangeOf(r.Context()).refusingFor(RefusalAuthority, reasonCDexRequestNotTask)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errCDexRequestNotTask})
		return
	case !cdexRequestCodeTaken(task):
		exchangeOf(r.Context()).refusingFor(RefusalAuthority, reasonCDexRequestCode)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errCDexRequestCode})
		return
	}
	recipient, status, msg, reason := recipientOf(task)
	if status != 0 {
		exchangeOf(r.Context()).routedFor(status, reason)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	member, ok := cdexTaskMember(task)
	if !ok {
		exchangeOf(r.Context()).refusingFor(RefusalAuthority, reasonCDexRequestNoPatient)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": errCDexRequestNoPatient})
		return
	}
	// The patient is bound by the member number, by the shared rule
	// (resolveSubjectPCI), so this leg and the provider's CDex submission,
	// keyed by the same number, bind one patient alike: the payer's own member
	// when its system of record holds it; otherwise an identifier derived from
	// the member number and the demographics of a carried Patient the shared
	// rule reads as the member's, one whose id is the number or that carries it
	// under shnsdk.MemberSystem. A Patient naming the number only under the
	// payer's own identifier system is not read as the member's, so such a Task
	// derives from the number alone, as a submission does. A member the
	// provider may not hold is carried (unknown is not empty).
	pci, found, readErr := g.resolveSubjectPCI(r.Context(), member, body)
	if errors.Is(readErr, errPatientUnidentifiable) {
		// The shared rule found the number on another person the Task carries
		// (a Coverage's party it recognizes by the same id or member system)
		// and no Patient of the member's own: it identifies no one patient,
		// refused as any Task the network cannot bind (422, as the other bind
		// failures here).
		exchangeOf(r.Context()).refusingFor(RefusalAuthority, reasonPatientUnidentifiable)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msgPatientUnidentifiable})
		return
	}
	if readErr != nil {
		status, msg := subjectBindingFailure(r.Context(), readErr)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	if !found {
		// Only a participant that requires known members gets here: a check
		// it opted into, recorded as conformance.
		exchangeOf(r.Context()).refusing(RefusalConformance)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	scope.leg = leg
	ex := g.exchanges.Begin(workstreamPA)
	child := g.ingressCorrelation(w, r)
	request, ok := sealModuleRequest(w, builder, relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json"), body)
	if !ok {
		return
	}
	g.noteCDexOwner(task, recipient, child)
	content := Content{WorkstreamType: workstreamPA, Payload: request, Carried: true, OnBehalf: builder != ""}
	var stated statedAnswer
	var answerType string
	answer, err := g.OriginateLeg(withStatedAnswerSink(withAnswerMediaTypeSink(r.Context(), &answerType), &stated), r, recipient, leg, pci, child, "", content)
	spec, _ := legSpecFor(leg)
	legProj := Leg{Type: leg, Physics: spec.Physics, Content: content, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: leg, CorrelationID: child, Seam: "payer-ingress", Whose: "peer"})
	if repeatsAMember(answerCtx, answer) {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the provider's answer can be read two ways: it repeats a member name"})
		return
	}
	// The provider's own media type, whatever it is, as on the other carried
	// legs, with the Task's Location at this gateway's base (E-16).
	ct := writableAnswerMediaType(answerType)
	located, err := g.cdexTaskLocated(r.Context(), relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct), stated, recipient)
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		refuseCDexTaskLocation(w, r, err)
		return
	}
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	g.writeRelayedAnswer(w, stated, ct, located,
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// cdexRequestRecipient is the provider a Task is routed to: the one its base
// names, which must be a registered provider. A Task it cannot route is
// refused (routing): there is no default provider, and the base's provider is
// the recipient whatever Task.owner names (noteCDexOwner records a
// difference). A provider that does not take the leg is refused by the Hub,
// which records the refusal on the network's audit chain.
func (g *Gateway) cdexRequestRecipient(r *http.Request) (string, int, string, refusalReason) {
	path := r.PathValue("provider")
	if e, ok := g.cfg.Reg.Lookup(path); path == "" || !ok || e.Role != "provider" {
		return "", http.StatusUnprocessableEntity, errCDexRequestNoProvider, reasonCDexRequestNoProvider
	}
	return path, 0, "", ""
}

// cdexRequestOwnerRecipient is the provider a Task at the plain Task endpoint
// is routed to: the one registered provider the network attests for the
// organization NPIs its Task.owner states (ownerOrganizationNPIs). Every
// routable identifier counts, and an identifier that matches no one is passed
// over when another matches. An owner that cannot be read, one whose
// identifiers match no provider, and one whose identifiers match different
// providers (an identifier attested for two among them) are refused on
// routing, never picked. A provider that does not take the attachment request
// is refused by the Hub, which records it on the network's audit chain, as at
// its base.
func (g *Gateway) cdexRequestOwnerRecipient(task cdextask.Task) (string, int, string, refusalReason) {
	ids, readable := ownerOrganizationNPIs(task.Owner)
	if !readable {
		return "", http.StatusUnprocessableEntity, errCDexRequestOwnerUnreadable, reasonCDexRequestOwnerUnreadable
	}
	providers := g.providersAttestedFor(ids)
	switch len(providers) {
	case 0:
		return "", http.StatusUnprocessableEntity, errCDexRequestOwnerNoProvider, reasonCDexRequestOwnerNoProvider
	case 1:
	default:
		return "", http.StatusUnprocessableEntity, errCDexRequestOwnerTwoProviders, reasonCDexRequestOwnerTwoProviders
	}
	return providers[0], 0, "", ""
}

// cdexProvider is one entry of the payer gateway's provider listing: the
// provider, the base a Task for it is created at, and the organization
// identifiers the network attests for it (omitted when none), the public
// feed's own.
type cdexProvider struct {
	Participant string                      `json:"participant"`
	Base        string                      `json:"base"`
	Identifiers []shnsdk.ProviderIdentifier `json:"identifiers,omitempty"`
	// Reads says the provider serves a payer's read and vread of its Task at
	// this base: its gateway declares the read's leg. Always stated, so an
	// entry without it is from a gateway that predates the read.
	Reads bool `json:"reads"`
}

// cdexProviders lists every registered provider that declares it takes the
// attachment request, in participant id order, with the base a Task for it is
// created at (cdexroute.ProviderBase): a listed base routes, and none that
// always refuses is advertised. A provider that declares no leg list predates
// the leg and takes none. A base that cannot be built is an error, never a
// shorter list.
func (g *Gateway) cdexProviders() ([]cdexProvider, error) {
	out := []cdexProvider{}
	for _, id := range g.cfg.Reg.IDs() {
		e, ok := g.cfg.Reg.Lookup(id)
		if !ok || e.Role != "provider" || !slices.Contains(e.Legs, legCDexRequest) {
			continue
		}
		base, err := cdexroute.ProviderBase(g.cfg.IngressBaseURL, id)
		if err != nil {
			return nil, err
		}
		out = append(out, cdexProvider{Participant: id, Base: base, Identifiers: e.ProviderIDs, Reads: slices.Contains(e.Legs, legCDexTaskStatus)})
	}
	return out, nil
}

// handleCDexProviders is GET /cdex/providers: the providers a payer's system
// can create an attachment-request Task for, each with its base.
func (g *Gateway) handleCDexProviders(w http.ResponseWriter, r *http.Request) {
	if g.ingressScopeRefused(w, r, taskCreate) {
		return
	}
	providers, err := g.cdexProviders()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}
