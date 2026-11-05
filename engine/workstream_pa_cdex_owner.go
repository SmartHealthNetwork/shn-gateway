//go:build !shn_coreonly

package engine

import (
	"slices"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdextask"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// CDexOwnerDiffersEvent is the observer event a payer's gateway raises when
// a Task created at a provider's base names in Task.owner an organization the
// network attests for another registered provider, or names its owner in a
// form that cannot be read. The Task is carried to the base's provider as
// sent; the event carries no identifier.
const CDexOwnerDiffersEvent = "cdex.owner-differs"

// providerIDSystem is the one identifier system the network attests a
// provider for: the NPI, matched exactly.
const providerIDSystem = "http://hl7.org/fhir/sid/us-npi"

// ownerOrganizationNPIs are the organization identifiers Task.owner states
// in providerIDSystem: owner.identifier, the contained PractitionerRole's
// organization, and the identifiers of the contained Organization the owner
// or that role names. The practitioner's identifier is read and never routed
// on, and an identifier in another system names no one the network holds. readable is false when the owner, or a part of it
// that could state its organization, cannot be read: unknown, never no owner.
func ownerOrganizationNPIs(o cdextask.Owner) (ids []shnsdk.ProviderIdentifier, readable bool) {
	if o.State == cdextask.Unreadable || o.Identifier.State == cdextask.Unreadable ||
		o.Organization.State == cdextask.Unreadable || o.ContainedOrganization.State == cdextask.Unreadable {
		return nil, false
	}
	add := func(id cdextask.Identifier) {
		pid := shnsdk.ProviderIdentifier{System: id.System, Value: id.Value}
		if id.System == providerIDSystem && !slices.Contains(ids, pid) {
			ids = append(ids, pid)
		}
	}
	if o.Identifier.State == cdextask.Present {
		add(o.Identifier.Identifier)
	}
	if o.Organization.State == cdextask.Present {
		add(o.Organization.Identifier)
	}
	for _, id := range o.ContainedOrganization.Identifiers {
		add(id)
	}
	return ids, true
}

// providersAttestedFor are the registered providers the network attests for
// any of ids, in participant id order. Only a provider's attested
// identifiers count, as only a payer's route a Coverage: an identifier on a
// holder of another role is never read.
func (g *Gateway) providersAttestedFor(ids []shnsdk.ProviderIdentifier) []string {
	var out []string
	for _, id := range g.cfg.Reg.IDs() {
		e, ok := g.cfg.Reg.Lookup(id)
		if !ok || e.Role != "provider" {
			continue
		}
		for _, pid := range e.ProviderIDs {
			if slices.Contains(ids, pid) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// noteCDexOwner raises CDexOwnerDiffersEvent when a Task created at
// provider's base names in its owner an organization the network attests for
// another registered provider, or names its owner in a form that cannot be
// read. An owner whose identifiers match no one, or none but the base's
// provider, states no difference the network can see. The Task is carried as
// sent either way.
func (g *Gateway) noteCDexOwner(task cdextask.Task, provider, corrID string) {
	ids, readable := ownerOrganizationNPIs(task.Owner)
	differs := !readable
	for _, p := range g.providersAttestedFor(ids) {
		if p != provider {
			differs = true
		}
	}
	if differs {
		g.observe(ObserverEvent{Kind: CDexOwnerDiffersEvent, Direction: "ingress", LegType: legCDexRequest, CorrelationID: corrID})
	}
}
