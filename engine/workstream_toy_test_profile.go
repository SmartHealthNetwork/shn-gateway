//go:build shn_toyworkstream

package engine

import (
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/legtaxonomy"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The toy workstream is a test profile, compiled in only under the
// shn_toyworkstream build tag: one request-response leg (toy-echo) at one
// contract line (toy.echo@1), carried as each participant sends it, so a test
// can run a second workstream through the network beside PA. A release build
// never carries it (workstream_profiles_release.go).

// toyWorkstreamTag is the WorkstreamType the toy's content is tagged with.
const toyWorkstreamTag = "toy"

// toyContractVersion is the one contract line the toy builds and declares.
const toyContractVersion = "toy.echo@1"

// toyProfileWorkstream is the toy's adapter.
type toyProfileWorkstream struct{ legs map[string]legSpec }

// toyAdapter is the adapter the registry compiles in under the tag.
var toyAdapter = toyProfileWorkstream{legs: mustCatalog(legtaxonomy.Toy(), toyWorkstreamTag, toyLegs)}

// toyLegs is what the toy adapter adds to its taxonomy's one leg.
var toyLegs = map[string]legDetail{
	"toy-echo": {Physics: LegPhysics{Kind: KindRequestResponse, Effect: EffectReadOnly, Timing: TimingSync, Locality: LocalitySubstrate}, Contract: "toy.echo", Seam: "toy-inbound"},
}

// profileWorkstreams are the test profiles' workstreams this build compiles
// in: the toy.
func profileWorkstreams() []workstream { return []workstream{toyAdapter} }

func (toyProfileWorkstream) id() string                                    { return toyWorkstreamTag }
func (w toyProfileWorkstream) catalog() map[string]legSpec                 { return w.legs }
func (toyProfileWorkstream) routes(g *Gateway, role string) []mountedRoute { return nil }
func (toyProfileWorkstream) ingressRoutes() map[string]ingressHandler      { return nil }
func (toyProfileWorkstream) involvement(string) (bool, bool)               { return false, false }
func (toyProfileWorkstream) requestMembers(string, []byte) []namedMember   { return nil }
func (toyProfileWorkstream) canonicalContractVersion() string              { return "" }
func (toyProfileWorkstream) originatedAnswerCheck(string) legAnswerCheck   { return nil }
func (toyProfileWorkstream) newMemState() memState                         { return nil }
func (toyProfileWorkstream) checkStore(Store) error                        { return nil }
func (toyProfileWorkstream) newGatewayState(*Gateway) gatewayState         { return nil }

// forward: the toy has no native responder send; its recipient answers
// through the Responder (answer).
func (toyProfileWorkstream) forward(string) forwardHandler { return nil }

// contractVersions: the toy builds its one line and is in no default
// declaration: a test that runs the toy declares it, so a tagged build's
// default declaration is still PA's.
func (toyProfileWorkstream) contractVersions() (native, defaults []string) {
	return []string{toyContractVersion}, nil
}

// answer: the recipient's participant answers the toy leg through its
// Responder, relayed as it answered.
func (toyProfileWorkstream) answer(leg string) answerHandler {
	if leg != "toy-echo" {
		return nil
	}
	return answerFromResponder
}

// certifiedBuilds: the toy workstream builds nothing on a participant's behalf.
func (toyProfileWorkstream) certifiedBuilds() map[certifiedBuild]string { return nil }

// certification: the toy's payloads are certified by no IG profile.
func (toyProfileWorkstream) certification() certificationAdapter { return nil }

// ownershipRows: each side carries the leg as its participant sent it, both
// ways, the recipient's participant's error answer included
// (relay.RegisterOwnership adds the refusal rows every leg has, and the
// requester's relayed error answer).
func (toyProfileWorkstream) ownershipRows() map[relay.Key]relay.Rule {
	relayed := relay.Rule{Allowed: []relay.Ownership{relay.OwnershipRelayed}}
	return map[relay.Key]relay.Rule{
		{Leg: "toy-echo", Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}:        relayed,
		{Leg: "toy-echo", Role: relay.RoleRecipient, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}:        relayed,
		{Leg: "toy-echo", Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}:      relayed,
		{Leg: "toy-echo", Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered}:      relayed,
		{Leg: "toy-echo", Role: relay.RoleRecipient, Direction: relay.DirectionResponse, Outcome: relay.OutcomeUpstreamError}: relayed,
	}
}

// answerFromResponder is the toy leg's answer: what the participant's own
// system answers through the Responder, relayed as it answered (the request as
// it arrived, bound to the token's subject, with no content check of this
// gateway's own). It is the toy's alone: it arms no LegResult Commit or
// Rollback, which a leg that claims state at its recipient needs.
func answerFromResponder(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
	leg := inboundLegs[env.Metadata.TransactionType]
	result, err := g.cfg.Responder.Handle(r.Context(), leg.tx, env.Metadata.CorrelationID, tok.Subject, body)
	if err != nil {
		g.responderFailed(w, r, leg, env, tok, answerTok, err)
		return
	}
	if result.Status != 0 {
		g.respondLegError(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, result, tok.Subject, env.Metadata.Sender, "", answerTok)
		return
	}
	g.respondLeg(w, r, leg.frame, leg.op, leg.tx, env.Metadata.CorrelationID, result.Response, result.answerStatus(), tok.Subject, env.Metadata.Sender, "", answerTok)
}
