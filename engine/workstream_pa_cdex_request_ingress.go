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
// with a bearer granted TaskWriteScope, creates a CDex 2.1.0 Task Attachment
// Request at a provider's base on its gateway
// (POST <ingress base>/cdex/provider/{provider}/Task), and the network carries
// it to that provider as sent. The network refuses one only on its own
// primitives:
//   - a body that reads two ways (message integrity);
//   - a Task outside the leg's scope: not a Task, or a Task.code other than
//     PASTempCodes#attachment-request-code (the leg's authority);
//   - a Task it cannot route: a base that names no registered provider
//     (routing);
//   - a Task it cannot bind to a patient: one that names no member number
//     for Task.for, or two that differ (the leg's authority, D121).
//
// The provider's answer is relayed exactly, its headers as on the other
// carried legs. No Location is added: the base's Task read is a later leg's.
//
// The gateway mounts no plain Task endpoint: a Task names its provider only in
// Task.owner, by an identifier (an NPI) the network holds for no registered
// provider, so a payer's system creates the Task at the provider's base.

// RouteCDexRequestBase is the attachment-request ingress route at a provider's
// base on this gateway, as exchange records name it.
const RouteCDexRequestBase = "cdex-request-base"

// CDexProvidersPath is the payer gateway's listing of the providers a Task can
// be created for, each with its base.
const CDexProvidersPath = "/cdex/providers"

// The refusal of the attachment-request ingress that only it makes: the
// refusals the provider's gateway makes of the same Task alike are
// workstream_pa_cdex_task.go's.
const errCDexRequestNoProvider = "this base names no registered provider: " + CDexProvidersPath + " lists the providers' bases"

// handleCDexRequestIngress is the payer-facing Task create at a provider's
// base (POST /cdex/provider/{provider}/Task): it is routed to the provider the
// base names, carried as sent, and the provider's answer comes back exactly as
// the provider wrote it.
func (g *Gateway) handleCDexRequestIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	if g.ingressScopeRefused(w, r, TaskWriteScope) {
		return
	}
	leg := legCDexRequest
	// Every refusal from here is recorded as this leg's.
	exchangeOf(r.Context()).leg(leg)
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
		exchangeOf(r.Context()).refusing(RefusalIntegrity)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errCDexRequestTwoWays})
		return
	case err != nil:
		exchangeOf(r.Context()).refusing(RefusalAuthority)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errCDexRequestNotTask})
		return
	case !task.Code.Has(cdextask.PASTempCodes, cdextask.CodeAttachmentRequest):
		exchangeOf(r.Context()).refusing(RefusalAuthority)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errCDexRequestCode})
		return
	}
	recipient, status, msg := g.cdexRequestRecipient(r)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	member, ok := cdexTaskMember(task)
	if !ok {
		exchangeOf(r.Context()).refusing(RefusalAuthority)
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
		exchangeOf(r.Context()).refusing(RefusalAuthority)
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
	g.recordLeg(ex.ID, legProj.Project(child, "answered"))
	// The provider's own media type, as on the other carried legs.
	ct := carriedFHIRMediaType(answerType)
	if ct == "" {
		ct = mediaFHIRJSON
	}
	g.writeRelayedAnswer(w, stated, ct,
		relay.Exact(relay.NewBody(answer, relay.OriginPeerFrame), ct),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// cdexRequestRecipient is the provider a Task is routed to: the one its base
// names, which must be a registered provider. A Task it cannot route is
// refused (routing): there is no default provider, and Task.owner names its
// provider only by an identifier the network holds for no registered
// provider, so it is never read to guess one.
func (g *Gateway) cdexRequestRecipient(r *http.Request) (string, int, string) {
	path := r.PathValue("provider")
	if e, ok := g.cfg.Reg.Lookup(path); path == "" || !ok || e.Role != "provider" {
		return "", http.StatusUnprocessableEntity, errCDexRequestNoProvider
	}
	return path, 0, ""
}

// cdexProvider is one entry of the payer gateway's provider listing.
type cdexProvider struct {
	Participant string `json:"participant"`
	Base        string `json:"base"`
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
		out = append(out, cdexProvider{Participant: id, Base: base})
	}
	return out, nil
}

// handleCDexProviders is GET /cdex/providers: the providers a payer's system
// can create an attachment-request Task for, each with its base.
func (g *Gateway) handleCDexProviders(w http.ResponseWriter, r *http.Request) {
	if g.ingressScopeRefused(w, r, TaskWriteScope) {
		return
	}
	providers, err := g.cdexProviders()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}
