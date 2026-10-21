// workstream_pa_enrich.go — the PA workstream's enrichment hook: the one
// seam through which a participant's own (native) request on a PA leg is
// enriched, held in the PA workstream's state on the gateway
// (paGatewayState).
package engine

import "context"

// nativeEnrichment is the one hook through which a participant's own
// (native) request is enriched. The PA legs call it on each enriching native
// leg: the CDS Hooks prefetch fill (E-02) on the CRD legs, and the Coverage
// (E-04) and Patient (E-05) obtains on the questionnaire-package leg, one
// method for each read. Behind the hook is on-behalf building's code
// (module1_enrich_native.go): it only reads the participant's own system of record.
// The gateway keeps, with or without the hook, everything that routes the
// request or guards what is inserted: the routing reads and the payer choice,
// the fill fence (every value the hook returns is fenced again before it is
// inserted), the refusals of RulePrefetchFill, and the registered edit that
// inserts the value.
type nativeEnrichment interface {
	// prefetchValue reads the value of an advertised prefetch key the request
	// leaves out, for the patient sorID: the value to insert (the JSON
	// literal null for a search with no match), or nil with the search
	// outcome when the key is left out. A non-zero status refuses the
	// request.
	prefetchValue(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string)
	// dtrCoverageFilters are the coverage template's search filters
	// (status=active) for a questionnaire-package request that carries no
	// Coverage. The core runs and records the search, routes by its matches,
	// and inserts the Coverage it routes by, only when it answered SearchOK.
	dtrCoverageFilters() []SearchDateRange
	// dtrPatient reads the Patient a questionnaire-package request carrying
	// none is sent with, or nil when nothing is added. A non-zero status
	// refuses the request, or is decided by RulePrefetchFill.
	dtrPatient(ctx context.Context, member string, fence patientFence) ([]byte, int, string)
}

// paGatewayState is the PA workstream's own state on one gateway
// (workstream.newGatewayState): the enrichment hook.
type paGatewayState struct {
	// enrich is the enrichment hook, installed at boot only under the
	// participant's opt-in (installEnrichment); nil otherwise.
	enrich nativeEnrichment
}

// newGatewayState: the PA workstream's state on a new gateway, its hook
// installed (or not) as the participant's opt-in says.
func (paWorkstream) newGatewayState(g *Gateway) gatewayState {
	st := &paGatewayState{}
	g.installEnrichment(st)
	return st
}

// paState is the PA workstream's state on g, or nil on a gateway assembled
// without New.
func (g *Gateway) paState() *paGatewayState {
	st, _ := g.states[workstreamPA].(*paGatewayState)
	return st
}

// installEnrichment installs the enrichment hook in st when the participant
// has opted in (Config.EnrichNativeRequests, ENRICH_NATIVE_REQUESTS), and none
// otherwise: no enrichment code then runs. New calls it at boot,
// through the PA workstream's state; it is the one place the engine reads the
// opt-in.
func (g *Gateway) installEnrichment(st *paGatewayState) {
	st.enrich = nil
	if g.cfg.EnrichNativeRequests {
		st.enrich = onBehalfEnrichment{g: g}
	}
}

// enrichment is the enrichment hook, or nil when the participant has not
// opted in or the gateway holds no PA state.
func (g *Gateway) enrichment() nativeEnrichment {
	if st := g.paState(); st != nil {
		return st.enrich
	}
	return nil
}
