package engine

import (
	"context"
	"slices"
	"strings"
)

// routingCoverageChoice picks, from the Coverage records a system of record
// returned for one patient, the ones a request is routed by: the active ones
// when any is active, so a stale cancelled coverage naming another payer
// never makes routing ambiguous; otherwise every one of them, so the payer of
// a coverage that is no longer in force is still the one asked, and answers
// that it does not cover the member. It returns the indexes of the chosen
// records, in order. Whether the chosen records name one payer is the
// router's to decide: several payers is ambiguous, never a guess.
//
// A record is active when its status is "active"; one whose status cannot be
// read is not. The choice is made for every coverage this gateway routes by.
// A CRD request this gateway originates carries the routing choice
// (obtainRoutingCoverage); only the value filled into a partner's request
// under the enrichment opt-in carries the search the advertised coverage
// template names (status=active), and that request is still routed by this
// choice.
func routingCoverageChoice(coverages [][]byte) []int {
	var active, all []int
	for i, c := range coverages {
		all = append(all, i)
		var head struct {
			Status string `json:"status"`
		}
		if decodeMessage(c, &head) == nil && head.Status == "active" {
			active = append(active, i)
		}
	}
	if len(active) > 0 {
		return active
	}
	return all
}

// obtainRoutingCoverage reads the Coverage a request is routed by from the
// system of record, for the patient sorID: every Coverage the system holds,
// with its payor (`Coverage?patient=Patient/<id>&_include=Coverage:payor`, no
// status filter, unlike the coverage template), so the payer of a coverage
// that is no longer in force can still be asked. It is the coverage a CDS Hooks
// request that carries none is routed by when the participant has not opted in
// to enrichment, or has and the template's search found none or the system
// could not answer it (never inserted), and the coverage a CRD request this
// gateway originates carries (originCRDRecords): the gateway's own request,
// nothing filled into a partner's message.
//
// Every record the search returned is fenced. The value returned is the
// searchset the gateway writes (assembleSoRSearchset) of the Coverages
// routingCoverageChoice picks (the active ones, else all of them) and the
// records the search included, in the server's order, less an included record
// that only a Coverage not chosen references (the payor of a stale coverage).
// Whether the chosen Coverages name one payer is the router's to decide. It is
// recorded and returns as obtainPrefetch does.
func (g *Gateway) obtainRoutingCoverage(ctx context.Context, leg, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	s := runSoRSearch(ctx, g.cfg.SoR, "Coverage", sorID, true)
	g.recordPrefetch(leg, prefetchObtained{Key: "coverage", Query: s.Query, Outcome: s.Outcome, Reason: s.Reason, Count: s.Count, Pages: s.Pages})
	switch s.Outcome {
	case SearchOK:
	case SearchZero:
		return []byte("null"), SearchZero, 0, ""
	default:
		return nil, s.Outcome, 0, ""
	}
	if err := fence.check(s.Value); err != nil {
		status, msg := fillFenceRefusal(err)
		return nil, "", status, msg
	}
	record := func(m searchMatch) []byte { return s.pages[m.page][m.start:m.end] }
	covs := make([][]byte, len(s.matches))
	for i, m := range s.matches {
		covs[i] = record(m)
	}
	// A search with a match has a Coverage, so the choice is never empty.
	chosen := routingCoverageChoice(covs)
	isChosen := make([]bool, len(covs))
	for _, i := range chosen {
		isChosen[i] = true
	}
	var keptRefs, droppedRefs []string
	for i, c := range covs {
		if isChosen[i] {
			keptRefs = append(keptRefs, payorReferences(c)...)
		} else {
			droppedRefs = append(droppedRefs, payorReferences(c)...)
		}
	}
	type positioned struct {
		m    searchMatch
		mode string
	}
	var entries []positioned
	for i, m := range s.matches {
		if isChosen[i] {
			entries = append(entries, positioned{m, "match"})
		}
	}
	for _, m := range s.includes {
		if names := recordNames(record(m)); namedBy(droppedRefs, names) && !namedBy(keptRefs, names) {
			continue
		}
		entries = append(entries, positioned{m, "include"})
	}
	slices.SortStableFunc(entries, func(a, b positioned) int {
		if a.m.page != b.m.page {
			return a.m.page - b.m.page
		}
		return a.m.start - b.m.start
	})
	assembly := make([]assemblyEntry, len(entries))
	for i, e := range entries {
		assembly[i] = assemblyEntry{res: EntrySpan{Page: e.m.page, Start: e.m.start, End: e.m.end}, mode: e.mode}
	}
	value, _, err := assembleSoRSearchset(s.pages, assembly, len(chosen))
	if err != nil {
		return nil, SearchMalformed, 0, ""
	}
	return value, SearchOK, 0, ""
}

// payorReferences are the payor references a Coverage names.
func payorReferences(coverage []byte) []string {
	var c struct {
		Payor []struct {
			Reference string `json:"reference"`
		} `json:"payor"`
	}
	if decodeMessage(coverage, &c) != nil {
		return nil
	}
	var out []string
	for _, p := range c.Payor {
		if p.Reference != "" {
			out = append(out, p.Reference)
		}
	}
	return out
}

// recordNames is the "<type>/<id>" a record is referenced by ("" when it
// cannot be read).
func recordNames(record []byte) string {
	var head struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
	}
	if decodeMessage(record, &head) != nil || head.ResourceType == "" || head.ID == "" {
		return ""
	}
	return head.ResourceType + "/" + head.ID
}

// namedBy reports whether one of refs names the record named name, relatively
// or by an absolute URL ending in it, either of them with or without a
// version (`<ref>/_history/<version>`).
func namedBy(refs []string, name string) bool {
	if name == "" {
		return false
	}
	for _, r := range refs {
		r = stripHistoryRef(r)
		if r == name || strings.HasSuffix(r, "/"+name) {
			return true
		}
	}
	return false
}
