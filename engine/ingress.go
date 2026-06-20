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
// crd-order-select-native leg through the substrate, threads a metadata-only Exchange, and
// relays the rendered cards envelope back to the EHR.
//
// The route's {id} path value is deliberately NOT validated against crdIngressServiceID: any CDS
// service id the EHR was configured to call (the advertised order-select-crd, or a partner's own)
// normalizes to the single crd-order-select-native leg. The CDS service id matters only at the
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
	// §4: bind the subject — every patient reference must resolve to one pci.
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
	// One Exchange, one leg (the EHR owns grouping in pure pass-through).
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	respJSON, err := g.OriginateLeg(r.Context(), r, g.cfg.CounterpartID, "crd-order-select-native", pci, child, "",
		Content{WorkstreamType: workstreamPA, Bytes: sealed})
	leg := Leg{Type: "crd-order-select-native", Physics: paCatalog["crd-order-select-native"].Physics,
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
	canonical, patientRef, ok := dtrFromPackageParams(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing questionnaire canonical"})
		return
	}
	// Per-patient authz binding when the package carries a patient (the connectathon case); the
	// DTR fetch is otherwise patient-agnostic (the payer DTR handler does not subject-bind). §4: a
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
	fetch, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: shnsdk.StripCanonicalVersion(canonical)})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr fetch failed"})
		return
	}
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	pkgJSON, err := g.OriginateLeg(r.Context(), r, g.cfg.CounterpartID, "dtr-questionnaire-fetch", pci, child, "",
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
	pci, status, msg := g.ingressPASSubjectPCI(body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	ex := g.exchanges.Begin(workstreamPA)
	child := g.cfg.CorrelationGen()
	crJSON, err := g.OriginateLeg(r.Context(), r, g.cfg.CounterpartID, "pas-claim", pci, child, "",
		Content{WorkstreamType: workstreamPA, Bytes: body})
	leg := Leg{Type: "pas-claim", Physics: paCatalog["pas-claim"].Physics,
		Content: Content{WorkstreamType: workstreamPA, Bytes: body}, Subjects: []string{pci}}
	if err != nil {
		_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	outcome := "complete"
	if res, perr := shnsdk.ParseClaimResponse(crJSON); perr == nil && res.Outcome != "" {
		outcome = res.Outcome // approved | pended | denied | no-pa-required — a non-clinical label
	}
	_ = g.exchanges.AppendLeg(ex.ID, leg.Project(child, outcome))
	// Near-relay: the ClaimResponse Bundle is the payer's response shape; return verbatim.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(crJSON)
}
