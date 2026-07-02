// ingress.go — the DaVinciIngress origination driver: the second origination driver over
// OriginateLeg. Terminates the three inbound Da Vinci protocols, resolves+inlines prefetch
// (CRD), drives OriginateLeg per call through the ExchangeStore seam, and wraps each response
// back into its native envelope. Mounted on the provider role when Config.IngressEnabled.
package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// resolverFromResources builds a resolveRef that matches "<Type>/<id>" against a flat list of FHIR
// resource JSON blobs (a Bundle's entry.resource, a CDS Hooks prefetch value, or a Parameters
// parameter.resource). It is the shared core of the inbound-payload payor resolvers: an EXTERNAL
// Coverage.payor Organization a conformant partner references lives among THESE resources, not in the
// provider SoR — resolving it here (not via SoR) is the Finding-1 fix. refs are unique (Type/id), so
// the first match is deterministic regardless of the source's iteration order.
func resolverFromResources(resources [][]byte) func(ref string) ([]byte, bool) {
	return func(ref string) ([]byte, bool) {
		for _, res := range resources {
			var rt struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			}
			if json.Unmarshal(res, &rt) != nil || rt.ResourceType == "" || rt.ID == "" {
				continue
			}
			if rt.ResourceType+"/"+rt.ID == ref {
				return res, true
			}
		}
		return nil, false
	}
}

// bundleRefResolver resolves "<Type>/<id>" against the entries of an inbound FHIR Bundle (payor
// Organizations et al. live IN the partner's bundle, not the provider SoR). Used by the PAS ingress.
func bundleRefResolver(bundleJSON []byte) func(ref string) ([]byte, bool) {
	var b struct {
		Entry []struct {
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return func(string) ([]byte, bool) { return nil, false }
	}
	resources := make([][]byte, 0, len(b.Entry))
	for _, e := range b.Entry {
		if len(e.Resource) > 0 {
			resources = append(resources, e.Resource)
		}
	}
	return resolverFromResources(resources)
}

// prefetchResources flattens a CDS Hooks prefetch map's values into a resource list — an external
// payor Organization arrives as another prefetch entry, so the CRD ingress resolves against it.
func prefetchResources(prefetch map[string]json.RawMessage) [][]byte {
	out := make([][]byte, 0, len(prefetch))
	for _, v := range prefetch {
		if len(v) > 0 {
			out = append(out, v)
		}
	}
	return out
}

func (g *Gateway) handleCDSDiscovery(w http.ResponseWriter, r *http.Request) {
	if !g.ingressAuthOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
		return
	}
	body, err := cdsDiscoveryJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build discovery failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleCRDIngress terminates a conformant CDS Hooks order-select request from br-provider,
// subject-binds it, makes it self-contained + neutralizes the callback, originates a conformant
// crd-order-select leg through the substrate, threads a metadata-only Exchange, and
// relays the rendered cards envelope back to the EHR.
//
// The route's {id} path value is deliberately NOT validated against crdIngressServiceID: any CDS
// service id the EHR was configured to call (the advertised order-select-crd, or a partner's own)
// normalizes to the single crd-order-select leg. The CDS service id matters only at the
// payer egress (DiscoverCRDServiceID), not here.
func (g *Gateway) handleCRDIngress(w http.ResponseWriter, r *http.Request) {
	if !g.ingressAuthOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	// Bind the subject — every patient reference must resolve to one pci.
	pci, status, msg := g.ingressCRDSubjectPCI(body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	member := g.memberForPCI(body)
	// Ensure self-contained + neutralize the callback.
	sealed, status, msg := g.ingressEnsureSelfContained(body, member, pci)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Route off the INBOUND Coverage the partner sent (Case 2): the ingress has no member to
	// OpenCoverage, so recipientFor reads the request's prefetch.coverage. NO default — a CRD hook
	// with no coverage in prefetch (or a coverage with no parseable payer) FAILS CLOSED with 422
	// rather than routing to a former default (FR-G40 / AI-G11 / OWD-G10).
	var cdsReq ingressCDSRequest
	_ = json.Unmarshal(body, &cdsReq)
	// Resolve an EXTERNAL Coverage.payor Organization against the request's OWN prefetch resources
	// (Finding 1) — the partner's payor Org rides in prefetch, not the provider SoR. Contained /
	// inline payor forms still route without ever hitting resolveRef.
	recipient, _, status, msg := g.recipientForWith(cdsReq.Prefetch["coverage"], resolverFromResources(prefetchResources(cdsReq.Prefetch)))
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// One Exchange, one leg (the EHR owns grouping in pure pass-through).
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	respJSON, err := g.OriginateLeg(r.Context(), r, recipient, "crd-order-select", pci, child, "",
		Content{WorkstreamType: workstreamPA, Bytes: sealed})
	leg := Leg{Type: "crd-order-select", Physics: paCatalog["crd-order-select"].Physics,
		Content: Content{WorkstreamType: workstreamPA, Bytes: sealed}, Subjects: []string{pci}}
	if err != nil {
		_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// The substrate response is already a rendered conformant cards envelope (BuildCards);
	// wrap/relay it into the CDS Hooks {cards:[…]} response. Derive the outcome from machine
	// fields only (metadata; never store clinical content).
	cardsEnvelope, outcome, status, msg := wrapCards(respJSON)
	if status != 0 {
		_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, outcome))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(cardsEnvelope)
}

// handleDTRIngress terminates a conformant SDC $questionnaire-package request from br-provider,
// extracts the questionnaire canonical (and, for per-patient authz, the coverage beneficiary),
// originates the EXISTING dtr-questionnaire-fetch substrate leg, threads a metadata-only
// Exchange, and relays the package Bundle response verbatim (near-relay). The ingress does NOT
// invoke the Populator — br-provider's own DTR app populates locally.
func (g *Gateway) handleDTRIngress(w http.ResponseWriter, r *http.Request) {
	if !g.ingressAuthOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	canonical, patientRef, coverage, ok := dtrFromPackageParams(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing questionnaire canonical"})
		return
	}
	// Per-patient authz binding when the package carries a patient (the connectathon case); the
	// DTR fetch is otherwise patient-agnostic (the payer DTR handler does not subject-bind). A
	// CARRIED subject must AGREE — a present-but-unresolvable coverage patient fails closed rather
	// than degrading to an unbound (patient-agnostic) leg.
	var pci string
	if patientRef != "" {
		member := strings.TrimPrefix(patientRef, "Patient/")
		p, _, found := g.cfg.SoR.ResolvePatient(member)
		if !found {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "carried coverage patient does not resolve"})
			return
		}
		pci = p
	}
	// Route off the INBOUND Coverage the partner carried (Case 2): the DTR ingress has no member to
	// OpenCoverage, so recipientFor reads the carried coverage param. NO default — a patient-agnostic
	// $questionnaire-package (coverage == nil) or a coverage with no parseable payer FAILS CLOSED
	// with 422 rather than routing to a former default (FR-G40 / AI-G11 / OWD-G10).
	// Resolve an EXTERNAL Coverage.payor Organization against the request's OWN parameter resources
	// (Finding 1) — the partner's payor Org, if present, rides alongside the coverage parameter, not
	// in the provider SoR. Contained / inline payor forms still route without hitting resolveRef.
	recipient, _, status, msg := g.recipientForWith(coverage, resolverFromResources(dtrParamResources(body)))
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Carry the provider's inbound Coverage VERBATIM through the leg (FR-G28): the
	// native-forward rebuild re-emits it as the payer-required `coverage` parameter. nil
	// coverage marshals away (omitempty) → byte-identical to the canonical-only request, so
	// the demo path is unchanged. The payer-gw never fabricates coverage (non-aggregation).
	fetch, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{
		Canonical: shnsdk.StripCanonicalVersion(canonical),
		Coverage:  coverage,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr fetch failed"})
		return
	}
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	pkgJSON, err := g.OriginateLeg(r.Context(), r, recipient, "dtr-questionnaire-fetch", pci, child, "",
		Content{WorkstreamType: workstreamPA, Bytes: fetch})
	leg := Leg{Type: "dtr-questionnaire-fetch", Physics: paCatalog["dtr-questionnaire-fetch"].Physics,
		Content: Content{WorkstreamType: workstreamPA, Bytes: fetch}, Subjects: subjectsOf(pci)}
	if err != nil {
		_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, "ok"))
	// Near-relay: the package Bundle is the payer's response shape; return verbatim.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pkgJSON)
}

// subjectsOf returns a 1-element subjects slice for a non-empty pci, else nil.
func subjectsOf(pci string) []string {
	if pci == "" {
		return nil
	}
	return []string{pci}
}

func (g *Gateway) handlePASIngress(w http.ResponseWriter, r *http.Request) {
	if !g.ingressAuthOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "ingress authentication required"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	// Bind the subject across the conformant bundle (every patient reference → one pci). The
	// minimized ParseClaimBundle path is retired here — a real Da Vinci partner sends the full
	// conformant bundle (Patient + Coverage + payor Org + …), which ParseClaimBundle rejects. The
	// minimized pas-claim leg stays for the SDK / 8-scenario origination path (originate.go).
	pci, status, msg := g.ingressPASNativeSubjectPCI(body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Route off the INBOUND bundle's Coverage (Case 2): a conformant $submit carries the patient's
	// Coverage; recipientFor derives the payer HOLDER from it. NO default — a bundle whose Coverage
	// carries no parseable payer FAILS CLOSED with 422 (FR-G40 / AI-G11 / OWD-G10). Runs AFTER the
	// subject-bind above so a subject-divergent bundle still 403s before routing.
	// Resolve an EXTERNAL Coverage.payor Organization against the inbound bundle's OWN entries
	// (Finding 1): a conformant $submit carries the payor Org as a sibling bundle entry (br-payer's
	// findInBundle form), NOT in the provider SoR. Contained / inline payor forms still route without
	// hitting resolveRef.
	recipient, _, status, msg := g.recipientForWith(pasBundleCoverage(body), bundleRefResolver(body))
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// F-PB-INGRESS: discriminate $submit vs amended re-POST. A conformant $submit carrying
	// Claim.related[prior] is an AMENDMENT (FR-21) and MUST route the conformant UPDATE leg
	// (pas-claim-update) — its own provider-tpo PA-update authority + the FR-32 inbound
	// gate (conformantPASUpdateBind). Originating it as pas-claim would mis-bind the
	// authority/responder. An initial submit (no related[prior]) routes pas-claim. The
	// FR-32 Provenance/DR enforcement still fires DOWNSTREAM at the payer; the ingress only picks
	// the leg. One parse (F-B2 extractor) serves BOTH discrimination AND the corr-threading below.
	f, fstatus, _ := parseConformantPASUpdateFacts(body)
	leg := "pas-claim"
	if fstatus == 0 && f.relatedClaim != "" {
		leg = "pas-claim-update"
	}
	ex := g.exchanges.Begin(workstreamPA)
	// Finding A (OWD-G6 corr-threading): when the bundle carries a Claim.identifier with
	// system=="urn:shn:correlation", use ITS value as the leg correlation. This keys the payer's
	// RecordPendedClaim(subjectPCI, corr) on the partner-supplied identifier, so the follow-up
	// amended re-POST can reference it via Claim.related[prior].claim.identifier — the submit→amend
	// handoff the two-RI proof requires. Falls back to a fresh generated corr when absent, so
	// the existing br-payer goldens (which use PATIENT_EVENT_TRACE_NUMBER, not urn:shn:correlation)
	// are unaffected: TestTwoRI_DVApprovePAS and TestTwoRI_DVPendPAS fall back unchanged.
	//
	// Security: the pend is keyed by (subjectPCI, corr) where subjectPCI is bound to the
	// authenticated token subject (ingressPASNativeSubjectPCI above). A partner can only thread
	// a corr for their own member's pends — no cross-member hijack via a crafted identifier.
	child := g.cfg.CorrelationGen()
	if fstatus == 0 && f.claimCorrelation != "" {
		child = f.claimCorrelation
	}
	crJSON, err := g.OriginateLeg(r.Context(), r, recipient, leg, pci, child, "",
		Content{WorkstreamType: workstreamPA, Bytes: body})
	legProj := Leg{Type: leg, Physics: paCatalog[leg].Physics,
		Content: Content{WorkstreamType: workstreamPA, Bytes: body}, Subjects: []string{pci}}
	if err != nil {
		_ = g.exchanges.AppendLeg(ex.ID, legProj.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// R-3: label the Exchange projection by SHAPE — a top-level Bundle is a PENDED (A4) response;
	// ParseClaimResponse ERRORS on a pended Bundle (pas.go:574), so calling it alone would mislabel
	// an A4 as the default "complete". Non-clinical label only (the Hub stays payload-blind, AI-2).
	outcome := "complete"
	if pended, _, perr := shnsdk.ParsePendedResponse(crJSON); perr == nil && pended {
		outcome = "pended"
	} else if res, perr := shnsdk.ParseClaimResponse(crJSON); perr == nil && res.Outcome != "" {
		outcome = res.Outcome // approved | denied
	}
	_ = g.exchanges.AppendLeg(ex.ID, legProj.Project(child, outcome))
	// Near-relay: the ClaimResponse is the payer's response shape; return verbatim.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(crJSON)
}
