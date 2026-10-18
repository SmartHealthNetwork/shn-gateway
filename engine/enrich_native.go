package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// onBehalfEnrichment is on-behalf building's enrichment of a participant's
// own request, behind the core's one enrichment hook (nativeEnrichment). It
// only reads the participant's own system of record, as on-behalf building
// does; the prefetch read is the one on-behalf building's CDS Hooks requests
// are built with.
type onBehalfEnrichment struct {
	g *Gateway
}

func (e onBehalfEnrichment) prefetchValue(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	return e.g.obtainPrefetch(ctx, leg, key, sorID, fence)
}

func (e onBehalfEnrichment) dtrCoverageFilters() []SearchDateRange {
	return prefetchSearchFilters("Coverage")
}

func (e onBehalfEnrichment) dtrPatient(ctx context.Context, member string, fence patientFence) ([]byte, int, string) {
	return e.g.obtainDTRPatient(ctx, member, fence)
}

// obtainPrefetch reads the value of an advertised prefetch key from the
// system of record for the patient sorID, fences it, and records the
// attempt (PrefetchObtainedEvent and a log line). It returns the value to
// insert (the JSON literal null for a search with no match), or nil with the
// search outcome when the key is left out; a non-zero status refuses the
// request.
func (g *Gateway) obtainPrefetch(ctx context.Context, leg, key, sorID string, fence patientFence) ([]byte, SearchOutcome, int, string) {
	refused := func(err error) ([]byte, SearchOutcome, int, string) {
		status, msg := fillFenceRefusal(err)
		return nil, "", status, msg
	}
	if key == prefetchPatientKey {
		query := "Patient/" + sorID
		patient, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, query)
		if err != nil {
			status, msg := SoRFailureResponse(err)
			outcome := SearchMalformed
			if status == http.StatusServiceUnavailable {
				outcome = SearchUnavailable
			}
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: outcome, Reason: string(safeSoRError(err).Kind)})
			return nil, "", status, msg
		}
		if !found {
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchZero})
			return nil, "", http.StatusUnprocessableEntity, "patient not found in system of record"
		}
		var head struct {
			ResourceType string `json:"resourceType"`
		}
		if decodeMessage(patient, &head) != nil || head.ResourceType != "Patient" {
			g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchMalformed, Reason: "not a Patient"})
			status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
			return nil, "", status, msg
		}
		g.recordPrefetch(leg, prefetchObtained{Key: key, Query: query, Outcome: SearchOK, Count: 1})
		if err := fence.check(patient); err != nil {
			return refused(err)
		}
		return patient, SearchOK, 0, ""
	}
	resourceType, ok := prefetchSearchTypes[key]
	if !ok {
		return nil, SearchUnsupported, 0, ""
	}
	s := searchSystemOfRecord(ctx, g.cfg.SoR, resourceType, sorID)
	g.recordPrefetch(leg, prefetchObtained{Key: key, Query: s.Query, Outcome: s.Outcome, Reason: s.Reason, Count: s.Count, Pages: s.Pages})
	switch s.Outcome {
	case SearchOK:
		if err := fence.check(s.Value); err != nil {
			return refused(err)
		}
		return s.Value, SearchOK, 0, ""
	case SearchZero:
		return []byte("null"), SearchZero, 0, ""
	}
	return nil, s.Outcome, 0, ""
}

// obtainDTRPatient reads the Patient a request carrying none is sent with
// under Config.EnrichNativeRequests: the system of record's own record for
// the bound patient, by the reference the system names it by, recorded as a
// PrefetchObtainedEvent (key patient, operation questionnaire-package). The
// payer's side, which may not hold the member, derives the subject from
// this record exactly as this side derives it from the same record. Nothing
// is added when the system does not hold the patient, or names it by another
// id (the request's references would not resolve to it): the request is sent
// as it is and binds by member id alone on both sides. A record about another
// patient, or not a Patient, is refused.
func (g *Gateway) obtainDTRPatient(ctx context.Context, member string, fence patientFence) ([]byte, int, string) {
	const leg = "dtr-questionnaire-fetch"
	ref, found, err := ReadSystemOfRecord(g.cfg.SoR).PatientFHIRRefContext(ctx, member)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		return nil, status, msg
	}
	if !found {
		return nil, 0, ""
	}
	id, ok := strings.CutPrefix(ref, "Patient/")
	if !ok || !fhirIDRE.MatchString(id) {
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	if id != member {
		return nil, 0, ""
	}
	query := "Patient/" + id
	record := func(outcome SearchOutcome, reason string, count int) {
		g.recordPrefetch(leg, prefetchObtained{Key: prefetchPatientKey, Operation: shnsdk.FrameOperationQuestionnairePackage,
			Query: query, Outcome: outcome, Reason: reason, Count: count})
	}
	patient, found, err := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(ctx, query)
	if err != nil {
		status, msg := SoRFailureResponse(err)
		outcome := SearchMalformed
		if status == http.StatusServiceUnavailable {
			outcome = SearchUnavailable
		}
		record(outcome, string(safeSoRError(err).Kind), 0)
		return nil, status, msg
	}
	if !found {
		record(SearchZero, "", 0)
		return nil, http.StatusUnprocessableEntity, "patient not found in system of record"
	}
	var head struct {
		ResourceType string `json:"resourceType"`
	}
	if decodeMessage(patient, &head) != nil || head.ResourceType != "Patient" {
		record(SearchMalformed, "not a Patient", 0)
		status, msg := SoRFailureResponse(&SoRReadError{Kind: SoRInvalidResponse})
		return nil, status, msg
	}
	record(SearchOK, "", 1)
	if err := fence.check(patient); err != nil {
		var ce *CompartmentError
		if errors.As(err, &ce) && ce.Reason == opaqueContentReason {
			return nil, http.StatusBadGateway, fillFencedBinary
		}
		return nil, http.StatusBadGateway, fillFencedOtherPatient
	}
	return patient, 0, ""
}
