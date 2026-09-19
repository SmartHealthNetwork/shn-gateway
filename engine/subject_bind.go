package engine

import (
	"context"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolveSubjectPCI binds a member id to its network patient identifier (pci) through this
// holder's OWN system of record. It is the one read the Da Vinci CRD, DTR and PAS legs make
// to bind a subject, on the ingress (origination) side and the inbound (payer) side alike.
//
// Seam (connectathon test lane): with Config.AcceptUnknownMembers set, a member the
// system of record does not hold binds to shnsdk.ResolvePCI(member, "", "") instead of
// being refused. The identifier is derived from the member id the partner sent — nothing is
// minted — and both sides of an exchange derive it the same way, so the payer-side
// token-subject check holds unchanged. Default off (the zero value); never set outside the
// preview test lane (test/testdoorposture fences where it may appear in infra). Everything
// else on these legs is untouched: member-mixing refusals, prefetch read only from this
// system or the request, and the payer's own independent member resolution.
//
// A read failure is returned as is, with the flag set or not: an unreadable system of record
// is never mistaken for a member it does not hold.
func (g *Gateway) resolveSubjectPCI(ctx context.Context, member string) (pci string, found bool, readErr error) {
	pci, _, found, readErr = ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(ctx, member)
	if readErr != nil || found || !g.cfg.AcceptUnknownMembers {
		return pci, found, readErr
	}
	return shnsdk.ResolvePCI(member, "", ""), true, nil
}
