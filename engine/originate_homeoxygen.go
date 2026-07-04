// originate_homeoxygen.go — originateDispatch, the provider-data order-dispatch
// origination prong (handleHomeOxygen = the MBR-OX wrapper; UC-03 provider-data = the
// MBR-PD-UC03 wrapper).
//
// Unlike the UC-01…08 handlers, originateDispatch originates OFF PROVIDER DATA ONLY: it
// reads the given member's seeded open order (a DeviceRequest) from the FHIR SoR and drives
// CRD(order-dispatch) → DTR(operated $populate) → PAS through the substrate. There is no
// hardcoded order code and no answer book — the order code, the diagnosis, and the
// supplier ALL come from the SoR (OpenOrder + ResolveByReference of the order's performer).
// It serves any seeded order-dispatch member: HomeOxygen = MBR-OX (E0431), UC-03 = MBR-PD-UC03 (E1390).
//
// It deliberately does NOT call runCRDThenDTROrder: that helper builds a ServiceRequest
// from a literal code and originates crd-order-SELECT, whose verdict switch REJECTS any
// non-PA-required card. The order-dispatch card here is ADVISORY (conditional coverage,
// NOT auth-needed) — its job is to advertise the HomeOxygen questionnaire — so the gate is
// NeedsDTR / a questionnaire being present, NOT PARequired(). The genuine verdict is the
// conditional-coverage A4-pended → A1 (the supplier NPI is verdict-IRRELEVANT; there is no
// supplier-NPI-verdict branch anywhere here).
package engine

import (
	"encoding/json"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// homeOxygenMember is the seeded provider-data persona (the E0431 HomeOxygen order).
const homeOxygenMember = "MBR-OX"

// handleHomeOxygen originates the HomeOxygen PA off the member's seeded DeviceRequest.
func (g *Gateway) handleHomeOxygen(w http.ResponseWriter, r *http.Request) {
	g.originateDispatch(w, r, homeOxygenMember) // homeOxygenMember = "MBR-OX"
}

// handleDispatch originates the order-dispatch PA for a caller-named member — the SHN Kit's
// free-form "run against your data" entry. Same internal /scenario/* posture as its siblings
// (never public); the origination itself is originateDispatch, unchanged: order code,
// coverage, and supplier all come from the SoR, nothing persona-baked.
func (g *Gateway) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Member string `json:"member"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // lenient like the sibling handlers
	}
	if strings.TrimSpace(req.Member) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "member is required"})
		return
	}
	g.originateDispatch(w, r, req.Member)
}

// originateDispatch originates an order-dispatch PA off the given member's seeded DeviceRequest.
func (g *Gateway) originateDispatch(w http.ResponseWriter, r *http.Request, member string) {
	ctx := r.Context()

	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	patientRef := "Patient/" + member
	coverageRef := "Coverage/" + member

	// DIVERGENCE 1 — read the ORDER from the SoR (no literal code, no BuildServiceRequestCoded).
	orderJSON, ok := g.cfg.SoR.OpenOrder(member)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no open order for member in SoR"})
		return
	}
	// The order's product coding comes from the DATA (DeviceRequest.codeCodeableConcept),
	// never a literal — fail closed if it carries no {CPT,HCPCS} coding.
	if _, _, _, err := shnsdk.ParseOrderProductCoding(orderJSON); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "open order has no recognized product coding"})
		return
	}
	orderID, performerRef, ok := parseOrderIDAndPerformer(orderJSON)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "open order missing id or performer"})
		return
	}
	orderRef := "DeviceRequest/" + orderID

	// The supplier (performer) is resolved from the order's performer ref via a SoR read —
	// not a literal. Fail closed if the supplier Organization is absent.
	supplierJSON, ok := g.cfg.SoR.ResolveByReference(performerRef)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order performer (supplier) not resolvable from SoR"})
		return
	}

	// Read the member's OWN open Coverage as the routing/identity SOURCE (FR-G40): the dispatch leg's
	// payer identity derives from the patient's real Coverage, not a synthetic CMS literal. realCov
	// stays a LOCAL (the recipient is resolved from it); the per-leg emit shapes are unchanged.
	realCov, hasCov := g.cfg.SoR.OpenCoverage(member)
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return
	}
	// Resolve the payer HOLDER every dispatch/DTR/PAS leg routes to AND the parsed payer identity in
	// ONE parse of the member's own Coverage (FR-G40): no default — a miss fails closed HERE before
	// any leg (AI-G11 / OWD-G10). `payer` threads to the dispatch/coverage/PAS builders, so
	// routed-payer and payload-payer cannot diverge (one payer fact, read once).
	recipient, payer, status, msg := g.recipientForWith(realCov, g.cfg.SoR.ResolveByReference)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	coverageJSON, err := shnsdk.BuildCoverageWithPayer(patientRef, coverageRef, payer)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build coverage failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, coverageJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// DIVERGENCE 2 — build + originate the ORDER-DISPATCH leg (not order-select).
	crdReq, err := shnsdk.BuildConformantOrderDispatchRequest(shnsdk.OrderDispatchInputs{
		PatientID:     member,
		PatientRef:    patientRef,
		OrderRef:      orderRef,
		PerformerRef:  performerRef,
		DeviceRequest: orderJSON,
		Supplier:      supplierJSON,
		Coverage:      coverageJSON,
		Payer:         payer,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build order-dispatch failed"})
		return
	}
	crdRespJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-dispatch", pci, g.cfg.CorrelationGen(), "", Content{WorkstreamType: workstreamPA, Bytes: crdReq})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cov, err := shnsdk.ParseCards(crdRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return
	}

	// DIVERGENCE 3 — the order-dispatch card is ADVISORY ("Supplier Status Unknown",
	// conditional), NOT PA-required. Do NOT gate on cov.PARequired() (false here). The card's
	// job is to advertise the HomeOxygen questionnaire; gate on NeedsDTR / a questionnaire
	// being present.
	if !cov.NeedsDTR() || len(cov.Questionnaires) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected an advisory card advertising the HomeOxygen questionnaire"})
		return
	}
	canonical := shnsdk.StripCanonicalVersion(cov.Questionnaires[0])

	// --- DTR — operated $populate (the crux). Carry the Coverage when targeting br-payer
	// (a real Da Vinci payer 400s $questionnaire-package without it). ---
	fetch := shnsdk.QuestionnaireFetchRequest{Canonical: canonical}
	if targetsBrPayer(g.cfg.OriginationProfile) {
		fetch.Coverage = coverageJSON
	}
	dtrReq, err := json.Marshal(fetch)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr request failed"})
		return
	}
	packageJSON, err := g.OriginateLeg(ctx, r, recipient, "dtr-questionnaire-fetch", pci, g.cfg.CorrelationGen(), "", Content{WorkstreamType: workstreamPA, Bytes: dtrReq})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, packageJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// The operated $populate engine reads the FHIR store directly, so its subject must be
	// the store-resolvable Patient ref. Resolve it via the SoR (falls back to the logical
	// ref when the SoR can't resolve it — the managed/hermetic path is unchanged).
	subjectFHIRRef := patientRef
	if ref, ok := g.cfg.SoR.PatientFHIRRef(member); ok && ref != "" {
		subjectFHIRRef = ref
	}
	qrJSON, _, err := g.cfg.Populator.Populate(ctx, packageJSON, PopulateContext{
		Member:         member,
		PatientRef:     patientRef,
		SubjectFHIRRef: subjectFHIRRef,
		CoverageRef:    coverageRef,
		OrderRef:       orderRef,
		Authored:       g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, statusForPopulateErr(err), map[string]string{"error": err.Error()})
		return
	}
	// QR-SUBJECT FENCE — the populated QR must be about the bound patient (logical ref).
	if subj, serr := questionnaireResponseSubject(qrJSON); serr != nil || subj != patientRef {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR subject does not match patient"})
		return
	}
	// QR-QUESTIONNAIRE FENCE — the QR must self-declare the canonical the card advertised.
	if qq, qerr := questionnaireResponseCanonical(qrJSON); qerr != nil || qq != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR questionnaire does not match canonical"})
		return
	}
	// CRUX EVIDENCE (C1) — capture the operated-$populate computed quantity answers off the QR
	// (linkIds 2.2 = O₂-sat / 2.3 = PaO₂). The native populator drops per-item FilledItem
	// attribution, so these are read straight from the returned QR. Surfaced in the response so the
	// live gate can prove the $populate ran br-payer's real prepop CQL against the seeded
	// observations (NOT an answer book). Empty when nothing populated (e.g. aged-out obs).
	qrAnswers := questionnaireResponseNumericAnswers(qrJSON)
	if status, msg := g.validateFHIR(ctx, qrJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// --- PAS — the shared lean single-shot tail (submitClaimAndResolve). The order resource is the
	// DeviceRequest, so InfoChanged stays false (orderIsDeviceRequest) — its order type alone routes
	// the payer gate to poll the timer-resolved A1. The genuine outcome is conditional-coverage
	// A4-pended → A1; the payer responder's pend re-query resolves A4→A1, so the FINAL observed
	// Outcome is "approved" (A1). ---
	parsed, _, status, msg := g.submitClaimAndResolve(ctx, r, pci, orderJSON, qrJSON, patientRef, coverageRef, payer, recipient)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-23: persist the payer-issued auth number against the order reference.
	if err := g.cfg.Store.StoreAuthNumber(orderRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}

	writeJSON(w, http.StatusOK, uc03Resp{
		PARequired: true,
		AuthNumber: parsed.PreAuthRef,
		ValidUntil: parsed.ValidUntil,
		QRAnswers:  qrAnswers,
	})
}

// parseOrderIDAndPerformer extracts the order's id and performer.reference from a
// DeviceRequest (or ServiceRequest) JSON. Both must be present (the order-dispatch leg
// needs the order ref + the supplier ref) — ok=false otherwise (fail closed).
func parseOrderIDAndPerformer(orderJSON []byte) (id, performerRef string, ok bool) {
	var probe struct {
		ID        string `json:"id"`
		Performer struct {
			Reference string `json:"reference"`
		} `json:"performer"`
	}
	if err := json.Unmarshal(orderJSON, &probe); err != nil {
		return "", "", false
	}
	if probe.ID == "" || probe.Performer.Reference == "" {
		return "", "", false
	}
	return probe.ID, probe.Performer.Reference, true
}
