//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"slices"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The insurer of the coverage-eligibility answer a payer's gateway builds from its own records
// (PAYER_ELIGIBILITY_URL unset). The answer goes back to a network participant, so it names the
// identity the network knows this payer by, read from the payer's own record, never by position:
// of the identifiers the record lists (every payor of the member's Coverage, or the payer's own
// Organization when the member has no Coverage), the first in document order that this holder
// publishes on the network feed. When the record lists none of them, the insurer is read as
// before: the first identifier of payor[0], or of the payer's own Organization.
//
// Each payor is read on its own, in document order, until one names a published identity; a later
// payor is not read. Each reference is read at most once. A payor whose reference the system of
// record failed to read adds nothing, and the answer is built from what could be read: a
// published identity found on a later payor, else as before. Each such failed read is recorded
// once the answer is built (ExchangeRecord.ReadsFailed), so the answer is never worse than
// reading payor[0] alone and the gap is visible.

// publishedPayerIdentities is every payer identity this holder publishes on the network feed:
// nothing on a gateway that is not a payer's.
func (g *Gateway) publishedPayerIdentities() []shnsdk.PayerIdentifier {
	if g.cfg.Role != "payer" || g.cfg.HolderID == "" {
		return nil
	}
	if e, ok := g.cfg.Reg.Lookup(g.cfg.HolderID); ok {
		return e.PayerIDs
	}
	return nil
}

// firstPublished returns the first of listed (the record's identifiers, in document order) that
// published holds; false when listed holds none of them.
func firstPublished(published, listed []shnsdk.PayerIdentifier) (shnsdk.PayerIdentifier, bool) {
	for _, p := range listed {
		if slices.Contains(published, p) {
			return p, true
		}
	}
	return shnsdk.PayerIdentifier{}, false
}

// coverageAnswerInsurer is the insurer of the answer for a member whose Coverage is coverage: the
// first published identity its payors name, of those that could be read (coverageFirstPublished),
// else the first identifier of payor[0], as before. ok is false when
// that names no payer. readErr is a failed read the answer cannot be built without: payor[0]'s,
// when it is read as before, exactly as before. readsFailed is the class of each other failed
// read, in read order, for the caller to record once the answer is built
// (ExchangeRecord.ReadsFailed). Each reference is read at most once.
func (g *Gateway) coverageAnswerInsurer(ctx context.Context, coverage []byte) (insurer shnsdk.PayerIdentifier, ok bool, readsFailed []string, readErr error) {
	reads := &insurerReads{ctx: ctx, sor: g.cfg.SoR}
	if published := g.publishedPayerIdentities(); len(published) > 0 {
		if p, ok := coverageFirstPublished(reads, coverage, published); ok {
			return p, true, reads.failed, nil
		}
	}
	var before error
	insurer, ok = shnsdk.ParsePayerIdentifier(coverage, func(ref string) ([]byte, bool) {
		b, found, err := reads.read(ref)
		if err != nil && before == nil {
			before = safeSoRError(err)
		}
		return b, found && err == nil
	})
	if before != nil {
		return insurer, ok, nil, before
	}
	return insurer, ok, reads.failed, nil
}

// insurerReads reads payor references from the system of record, each at most once, keeping the
// class of each failed read in read order: the class of the read itself (sorClass), which the
// error a caller sees (safeSoRError) may not preserve.
type insurerReads struct {
	ctx    context.Context
	sor    SystemOfRecord
	got    map[string]insurerRead
	failed []string
}

type insurerRead struct {
	b     []byte
	found bool
	err   error
}

func (r *insurerReads) read(ref string) ([]byte, bool, error) {
	if res, ok := r.got[ref]; ok {
		return res.b, res.found, res.err
	}
	var res insurerRead
	if r.sor != nil {
		res.b, res.found, res.err = ReadSystemOfRecord(r.sor).ResolveByReferenceContext(r.ctx, ref)
	}
	if r.got == nil {
		r.got = map[string]insurerRead{}
	}
	r.got[ref] = res
	if res.err != nil {
		r.failed = append(r.failed, sorReadClass(r.ctx, res.err))
	}
	return res.b, res.found, res.err
}

// coverageFirstPublished is the first identity of published that coverage's payors name, in
// document order, reading them in order and stopping at the first payor that names one; false
// when none does. Each payor is read on its own, as a provider's routing reads it (shnsdk.CoveragePayerCandidates; a
// payor carrying its own identifier, or stating a type other than Organization, is never read
// from the system of record, unreadPayorRefs), so a payor naming no readable payer (a clinic
// Organization the record does not hold, a self-pay payor, one whose read failed) adds nothing
// and does not hide this payer's identity on another. A Coverage that is not one JSON object with
// a payor array names none.
func coverageFirstPublished(reads *insurerReads, coverage []byte, published []shnsdk.PayerIdentifier) (shnsdk.PayerIdentifier, bool) {
	var cov map[string]json.RawMessage
	var payors []json.RawMessage
	if json.Unmarshal(coverage, &cov) != nil || json.Unmarshal(cov["payor"], &payors) != nil {
		return shnsdk.PayerIdentifier{}, false
	}
	unread := unreadPayorRefs(coverage)
	for _, p := range payors {
		one := map[string]json.RawMessage{"resourceType": cov["resourceType"], "payor": json.RawMessage("[" + string(p) + "]")}
		if c, ok := cov["contained"]; ok {
			one["contained"] = c
		}
		b, err := json.Marshal(one)
		if err != nil {
			continue
		}
		// A payor whose read failed names no payer the SDK can read, so it
		// adds nothing (an *shnsdk.UnresolvedPayorError).
		cands, err := shnsdk.CoveragePayerCandidates(b, func(ref string) ([]byte, bool) {
			if unread[ref] {
				return nil, false
			}
			res, found, err := reads.read(ref)
			return res, found && err == nil
		})
		if err != nil {
			continue
		}
		for _, l := range cands {
			if p, ok := firstPublished(published, l); ok {
				return p, true
			}
		}
	}
	return shnsdk.PayerIdentifier{}, false
}

// organizationAnswerInsurer is the insurer of the answer for a member with no Coverage: the
// first published identity among the payer's own Organization's identifiers, else its first
// identifier, as before. ok is false when the resource is not an Organization or names no identifier.
func (g *Gateway) organizationAnswerInsurer(org []byte) (shnsdk.PayerIdentifier, bool) {
	if p, ok := firstPublished(g.publishedPayerIdentities(), shnsdk.OrganizationIdentifiers(org)); ok {
		return p, true
	}
	return shnsdk.ParseOrganizationIdentifier(org)
}
