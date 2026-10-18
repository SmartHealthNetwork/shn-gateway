package engine

import "context"

// nativeEnrichment is the one core hook through which a participant's own
// (native) request is enriched. The core calls it on each enriching native
// leg: the CDS Hooks prefetch fill (E-02) on the CRD legs, and the Coverage
// (E-04) and Patient (E-05) obtains on the questionnaire-package leg, one
// method for each read. Behind the hook is on-behalf building's code
// (enrich_native.go): it only reads the participant's own system of record.
// The core keeps, with or without the hook, everything that routes the
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

// installEnrichment installs the enrichment hook when the participant has
// opted in (Config.EnrichNativeRequests, ENRICH_NATIVE_REQUESTS), and none
// otherwise: the core then runs no enrichment code. New calls it at boot; it
// is the one place the engine reads the opt-in.
func (g *Gateway) installEnrichment() {
	g.enrich = nil
	if g.cfg.EnrichNativeRequests {
		g.enrich = onBehalfEnrichment{g: g}
	}
}

// enrichment is the enrichment hook, or nil when the participant has not
// opted in.
func (g *Gateway) enrichment() nativeEnrichment { return g.enrich }
