package engine

import shnsdk "github.com/SmartHealthNetwork/shn-sdk"

// A dependent's Coverage names the parent as its subscriber or policyHolder,
// often as a Patient contained in the Coverage, carrying only an MRN. A PAS
// answer (a $submit response, an inquiry's answer) carries the Coverage the
// payer answered for, so the subject rules over it (subjectMismatch,
// inquiryPatientScope.collect) read the parent as the Coverage's party,
// another person, rather than as a second patient. They decide it as the
// sdk's PAS response check does: by shnsdk.CoverageParty, for the contained
// list of a Coverage that is itself an entry of the answer. The rule also
// requires that the party holds as a contained resource (it contains
// nothing, its identifier is a list, and it spells the members the rule
// reads exactly), so a contained Patient that does not is no party and is
// read as the patient.

// coveragePartyRole is what an object is to an entry-level Coverage's party
// rule.
type coveragePartyRole uint8

const (
	roleNone coveragePartyRole = iota
	// roleCoverageParty is a Coverage's party: its identity is not read as
	// the patient's. Its references are read as any other resource's.
	roleCoverageParty
	// roleCoveragePartySlot is the Coverage's subscriber or policyHolder
	// whose own reference names its party: the one typed Patient reference
	// that names another person.
	roleCoveragePartySlot
)

// entryParties are an entry-level Coverage's parties: by index into its
// contained list, and the local references naming them.
type entryParties struct {
	byIndex []bool
	refs    map[string]bool
}

// entryCoverageParties returns the parties of cov, an entry-level Coverage:
// each resource in its own contained list that shnsdk.CoverageParty names
// the Coverage's party. The rule requires the party's id to be unique among
// the Coverage's contained resources ("#id" then names one resource), so a
// contained Patient sharing its id with another is read as the patient.
func entryCoverageParties(cov map[string]any) entryParties {
	list, _ := cov["contained"].([]any)
	var p entryParties
	for i, c := range list {
		cr, ok := c.(map[string]any)
		if !ok || !shnsdk.CoverageParty(cov, cr) {
			continue
		}
		if p.refs == nil {
			p.byIndex = make([]bool, len(list))
			p.refs = map[string]bool{}
		}
		p.byIndex[i] = true
		p.refs["#"+cr["id"].(string)] = true
	}
	return p
}

// any reports whether the Coverage has a party.
func (p entryParties) any() bool { return p.refs != nil }

// roleOf is the role of the contained resource at index i.
func (p entryParties) roleOf(i int) coveragePartyRole {
	if i < len(p.byIndex) && p.byIndex[i] {
		return roleCoverageParty
	}
	return roleNone
}

// slotRole is the role of the Coverage's member field, value v: a
// subscriber or policyHolder whose own reference names a party is that
// party's slot.
//
// The field check is redundant, kept as defence in depth: shnsdk.CoverageParty
// names a party only when nothing in the Coverage but its subscriber or
// policyHolder references it, so no other member's own reference can name a
// party here. No row can tell the check apart, so none pins it; were the rule
// to loosen, the check still keeps the exemption to the two slots.
func (p entryParties) slotRole(field string, v any) coveragePartyRole {
	if p.refs == nil || (field != "subscriber" && field != "policyHolder") {
		return roleNone
	}
	slot, _ := v.(map[string]any)
	if ref, _ := slot["reference"].(string); p.refs[ref] {
		return roleCoveragePartySlot
	}
	return roleNone
}
