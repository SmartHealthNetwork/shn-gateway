package engine

import (
	"bytes"
	"context"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// modulebuild.go is how a request module 1 built and then walks reaches its
// leg: one call, the LegClient's (sendLeg, legclient.go), to egressBuild,
// which walks it across lines when module 2 chose a chain for the route,
// checks it, and seals it under its builder. The two module 1 requests sealed
// before the walk (the questionnaire-package request, carryUnchanged, and the
// inquiry, inquireContinuation) take the walk on egressWalk only when it
// changes no byte; otherwise they are carried as built, at their build line,
// or refused as routing under REQUIRE_SHARED_LINE. They are checked at their
// own sites, and sent sealed through sendLeg.

// moduleBuild is a request module 1 built at its route's build line, for
// egressBuild to take to its leg.
type moduleBuild struct {
	Builder     relay.BuilderID
	ContentType string
	Body        []byte
	// Contract, when set, is the contract whose line the request is checked at
	// before it is sealed: the governed egress check of the participant's
	// request, or, when module 2 walked it to the peer's line, module 2's
	// bridged check (Certify) of what it changed. Empty for a request no FHIR
	// check reads (a CDS Hooks request, a next-question envelope).
	Contract string
	// Check, when set, is the site's own check of the bytes to be sent at the
	// line they are sent at, run after the walk and before the line check.
	Check func(line string, sent []byte) (int, string)
}

// builtRequest is a module 1 request ready for its leg: the payload to send,
// its bytes, and the loss reports of the walk that made them (none when the
// route has no chain).
type builtRequest struct {
	Payload relay.Payload
	Bytes   []byte
	Reports []LossReport
}

// egressBuild takes a request module 1 built at route.BuildLine to its leg:
// walked to the peer's line by module 2 when route has a chain, then the
// site's own check, then the check at the leg's line (bridged when walked),
// then sealed under its builder. err is a walk that failed (the leg's
// transform refusal); a non-zero status is a check's refusal, with its
// message. Every module 1 build site whose request is walked after it is
// built reaches its leg through here, by way of sendLeg (moduleBuildSites).
func (g *Gateway) egressBuild(ctx context.Context, route legRoute, x ExchangeIdentity, m moduleBuild) (builtRequest, int, string, error) {
	// The builder's message as built, sealed before the walk, which reads a
	// copy: a step that wrote into its input could not pass its output off as
	// the message as built. A walk that changed it is sealed as that message
	// carried by the registered transform (E-09), never under the builder
	// alone, and only once the registered steps make the same bytes again.
	payload := sealRequest(m.Builder, m.Body, m.ContentType)
	out, reports, err := g.egressAdapt(route, bytes.Clone(m.Body), x)
	if err != nil {
		return builtRequest{}, 0, "", err
	}
	if !bytes.Equal(out, m.Body) {
		var status int
		var msg string
		if payload, status, msg = g.sealTransformed(ctx, route, x, payload, out); status != 0 {
			return builtRequest{}, status, msg, nil
		}
	}
	line := shnsdk.LineOf(route.Token)
	if m.Check != nil {
		if status, msg := m.Check(line, out); status != 0 {
			return builtRequest{}, status, msg, nil
		}
	}
	if m.Contract != "" {
		var status int
		var msg string
		if len(route.Chain) > 0 {
			status, msg = g.bridge().Certify(ctx, m.Contract, line, "", out).refusal()
		} else {
			status, msg = g.validateFHIRForContract(ctx, out, "egress", m.Contract, line, "")
		}
		if status != 0 {
			return builtRequest{}, status, msg, nil
		}
	}
	return builtRequest{Payload: payload, Bytes: out, Reports: reports}, 0, "", nil
}

// sealTransformed seals out, built's bytes walked along route's chain, as
// built carried by the registered transform (relay.Transformed): verified
// against the compat manifest's own steps (registeredRederive), run again
// independently of the walk that made out. A chain naming a step the manifest
// does not register, bytes those steps do not make, or steps that do not make
// the same bytes twice is the fidelity of SHN's own edit refusing: nothing is
// sent (500, the gateway's fault, at every level).
func (g *Gateway) sealTransformed(ctx context.Context, route legRoute, x ExchangeIdentity, built relay.Payload, out []byte) (relay.Payload, int, string) {
	rederive, err := registeredRederive(route, x)
	if err == nil {
		var p relay.Payload
		if p, err = relay.Transformed(built, relay.EditRegisteredTransform, out, rederive); err == nil {
			return p, 0, ""
		}
	}
	g.observe(ObserverEvent{
		Kind: "leg.failed", Direction: "originate",
		LegType: x.LegType, CorrelationID: x.CorrelationID,
		Counterpart: x.Counterpart, Detail: err.Error(),
		Route: routeInfoFor(route),
	})
	exchangeOf(ctx).refused(RefusalFidelity)
	return built, http.StatusInternalServerError, "the gateway could not verify its own translation of the request to the payer's IG line, so nothing was sent: " + err.Error()
}
