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

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// homeOxygenMember is the seeded provider-data persona (the E0431 HomeOxygen order).
const homeOxygenMember = "MBR-OX"

// handleHomeOxygen originates the HomeOxygen PA off the member's seeded DeviceRequest.
func (g *Gateway) handleHomeOxygen(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, homeOxygenMember, homeOxygenMember, homeOxygenMember) // homeOxygenMember = "MBR-OX"
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
	}))
	g.originateDispatch(w, r, member)
}

// dispatchOrder is how an order-dispatch scenario obtains its DeviceRequest + supplier
// Organization: read from the member's seeded SoR (originateDispatch's OWN members, whose
// order-code/dx/supplier ALL come from the data — no answer book), or built from a literal
// HCPCS/dx tuple (UC-03's demo arm, matching every OTHER demo-lane scenario's literal-code
// convention, §4.3 — the demo lane never reads a seeded SoR order). Either shape feeds the
// SAME crd-order-dispatch → DTR → populate prefix (runCRDDispatch) so there is one dispatch
// implementation, not two.
type dispatchOrder struct {
	orderJSON, supplierJSON []byte
	orderRef, performerRef  string
}

// dispatchResult carries everything a runCRDDispatch caller needs past the populate step:
// the PAS tail inputs (originateDispatch's existing shape) plus the fetched bare
// Questionnaire and its canonical (needed by a caller that must ATTEST a required item
// into the populated QR before submitting, which originateDispatch's own callers do not).
type dispatchResult struct {
	qrSource                               *dtrBuildSource
	dtrLine                                string
	pci, patientRef, coverageRef, orderRef string
	// coverage is the member's own Coverage record — the same single read that
	// resolved the payer identity and the route — which the PAS request carries as
	// the resolvable entry the Claim names.
	coverage []byte
	// insurer is the payer's own Organization record — see crdDtrResult.insurer.
	insurer                                            []byte
	orderJSON, supplierJSON, qrJSON, questionnaireJSON []byte
	qrAnswers                                          map[string]string
	member                                             string
	// memberSystem is the namespace the participant's own system names that
	// member under, carried from the one reading of their Patient (crdOriginRecords).
	memberSystem string
	payer        shnsdk.PayerIdentifier
	recipient    string
	canonical    string
}

// runCRDDispatch is the shared order-dispatch prefix: CRD(order-dispatch) → DIVERGENCE-3
// advisory-card gate (NeedsDTR, not PARequired) → DTR fetch → operated $populate → the QR
// fences. Extracted from originateDispatch (behavior-preserving) so UC-03's demo arm — which
// needs to ATTEST a required item into the populated QR before the PAS tail, unlike
// originateDispatch's own callers — rides the identical CRD/DTR/populate plumbing instead of
// a second hand-rolled copy. On any failure it writes the HTTP error and returns ok=false.
func (g *Gateway) runCRDDispatch(w http.ResponseWriter, r *http.Request, member string, order dispatchOrder) (dispatchResult, bool) {
	ctx := r.Context()

	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return dispatchResult{}, false
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return dispatchResult{}, false
	}
	patientRef := "Patient/" + member
	coverageRef := "Coverage/" + member

	orderJSON := order.orderJSON
	// The order's product coding comes from the DATA (DeviceRequest.codeCodeableConcept),
	// never a literal — fail closed if it carries no {CPT,HCPCS} coding.
	if _, _, _, err := shnsdk.ParseOrderProductCoding(orderJSON); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "open order has no recognized product coding"})
		return dispatchResult{}, false
	}
	orderRef := order.orderRef
	performerRef := order.performerRef
	supplierJSON := order.supplierJSON

	// Read the member's OWN open Coverage as the routing/identity SOURCE (FR-G40): the dispatch leg's
	// payer identity derives from the patient's real Coverage, not a synthetic CMS literal. realCov
	// stays a LOCAL (the recipient is resolved from it); the per-leg emit shapes are unchanged.
	realCov, hasCov, status, msg := g.memberCoverage(r.Context(), member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	if !hasCov {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no coverage on file for member"})
		return dispatchResult{}, false
	}
	// Resolve the payer HOLDER every dispatch/DTR/PAS leg routes to AND the parsed payer identity in
	// ONE parse of the member's own Coverage (FR-G40): no default — a miss fails closed HERE before
	// any leg (AI-G11 / OWD-G10). `payer` threads to the dispatch/coverage/PAS builders, so
	// routed-payer and payload-payer cannot diverge (one payer fact, read once).
	recipient, payer, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	// And that payer's own Organization record, from the same system: the
	// submission names it, and so does every inquiry about the submission.
	realPayerOrg, status, msg := g.memberPayerOrganization(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}

	// The order-dispatch request carries the participant's own Patient, Coverage
	// search result and history (originate_crd.go); the order names the patient
	// the same way. The Coverage is read twice — above for routing (memberCoverage),
	// here as the search result the request carries — so each read keeps its own
	// refusal rules.
	recs, status, msg := g.originCRDRecords(ctx, "crd-order-dispatch", member)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	orderJSON, err := originOrder(recs, orderJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "name the patient in the open order: " + err.Error()})
		return dispatchResult{}, false
	}

	// DIVERGENCE 2 — build + originate the ORDER-DISPATCH leg (not order-select).
	// Select-before-build — same rationale as the two
	// crd-order-select sites (originate.go): the pa.crd builder is line-inert, so
	// BuildLine changes no byte, but the routing axis is real and the arm-1-only
	// backfill refused pa.crd@2.2-only peers this build serves byte-identically.
	// crdCorr is hoisted so selection, adaptation and the leg share one correlation.
	crdCorr := g.cfg.CorrelationGen()
	crdRoute, ok := g.selectLegLineOrFail(w, recipient, "crd-order-dispatch", crdCorr)
	if !ok {
		return dispatchResult{}, false
	}
	// The dispatched order is resolved by the payer from the request's device history;
	// the supplier is named as the performer, and the system of record's device search
	// includes it (DeviceRequest:performer). An order this gateway authored (the demo
	// lane) is carried as a collection Bundle holding that one order: the system of
	// record does not hold it, and no supplier record is added.
	crdReq, err := g.originatedDispatchRequest(crdCorr, recs, orderJSON, performerRef)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "build order-dispatch request failed: " + err.Error()})
		return dispatchResult{}, false
	}
	// Validation posture UNCHANGED (routing-only promotion): crdReq is a CDS
	// Hooks envelope, not a FHIR resource; the Patient, Coverage and history are the
	// participant's own records. No enforcement point is added or removed after
	// egressAdapt.
	adaptedCRDReq, _, err := g.egressAdapt(crdRoute, crdReq, ExchangeIdentity{CorrelationID: crdCorr, LegType: "crd-order-dispatch", Counterpart: recipient})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return dispatchResult{}, false
	}
	crdRespJSON, err := g.OriginateLeg(ctx, r, recipient, "crd-order-dispatch", pci, crdCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: crdRoute.Token, Route: routeInfoFor(crdRoute), Payload: sealRequest(relay.BuilderSDKCRDRequest, adaptedCRDReq, "application/json")})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return dispatchResult{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return dispatchResult{}, false
	}
	answer, err := readOriginatedAnswer(crdRespJSON, orderJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return dispatchResult{}, false
	}
	cov := answer.coverage

	// AI-1: a coverage denial STOPS — never routes DTR/PAS. Discovered missing here (R3,
	// while re-keying UC-03 onto this shared prefix): runCRDThenDTROrder's sibling gate
	// (originate.go) has always checked cov.Covered FIRST regardless of the doc-needed
	// axis, but this order-dispatch prefix never did — DIVERGENCE 3 below gates on
	// NeedsDTR alone, which does not imply covered. Every mirrored oxygen family
	// (E0431/E1390) is unconditionally Covered=true (brpayerfamilies.go), so this never
	// changes any of the 8 UCs' live-pinned behavior; it closes a real gap a not-covered
	// order-dispatch card would have silently proceeded through.
	if cov.Covered == shnsdk.CoveredNotCovered {
		writeJSON(w, http.StatusOK, map[string]any{"paRequired": false, "covered": false, "outcome": "not-covered"})
		return dispatchResult{}, false
	}

	// DIVERGENCE 3 — the order-dispatch card is ADVISORY ("Supplier Status Unknown",
	// conditional), NOT PA-required. Do NOT gate on cov.PARequired() (false here). The card's
	// job is to advertise the HomeOxygen questionnaire; gate on NeedsDTR / a questionnaire
	// being present.
	if !cov.NeedsDTR() || len(cov.Questionnaires) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected an advisory card advertising the HomeOxygen questionnaire"})
		return dispatchResult{}, false
	}
	canonical := shnsdk.StripCanonicalVersion(cov.Questionnaires[0])

	// --- DTR — operated $populate (the crux). ---
	// SELECT-BEFORE-BUILD on the fetch envelope too, not just the package
	// answer: the fetch is LINE-DEPENDENT (2.2's DTRDef makes `coverage` 1..1),
	// so the routed line must exist BEFORE the literal below is marshalled —
	// exactly the runCRDThenDTROrder sibling's ordering (originate.go). The
	// selected line also picks the validator lane the package answer is checked
	// against (F7).
	dtrCorr := g.cfg.CorrelationGen()
	route, ok := g.selectLegLineOrFail(w, recipient, "dtr-questionnaire-fetch", dtrCorr)
	if !ok {
		return dispatchResult{}, false
	}
	dtrLine := shnsdk.LineOf(route.Token)
	// The request is the $questionnaire-package operation's own input
	// (originatedPackageRequest, as the sibling site in originate.go): the
	// system of record's Coverage search result, the dispatched order as the
	// payer returned it, the canonical as the payer stated it and the payer's
	// coverage-assertion-id as context, framed with the operation and sent only
	// to a payer that declares framed DTR operations.
	if status, msg := g.framedDTRRefusal(recipient); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	dtrReq, dtrPayload, err := originatedPackageRequest(dtrLine, recs, dispatchQuestionnaireOrder(answer, orderJSON), cov.Questionnaires[0], answer.assertionID)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "build questionnaire-package request failed: " + err.Error()})
		return dispatchResult{}, false
	}
	if status, msg := g.carryUnchanged(route, dtrReq, dtrCorr, recipient); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	packageJSON, err := g.OriginateLeg(ctx, r, recipient, "dtr-questionnaire-fetch", pci, dtrCorr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: dtrPayload,
			Operation: shnsdk.FrameOperationQuestionnairePackage})
	if err != nil {
		if g.relayOriginationError(w, err) {
			return dispatchResult{}, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return dispatchResult{}, false
	}
	// This helper owns the dtr-questionnaire-fetch leg (and, above, crd-order-dispatch)
	// regardless of which caller's headline leg dispatched here — retag rather than
	// inherit, so a finding from this check never borrows the caller's leg name.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, packageJSON, dtrLine, "pa.dtr"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}

	questionnaireJSON, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire package has no Questionnaire"})
		return dispatchResult{}, false
	}

	// F5: verify the fetched Questionnaire's url matches the canonical the payer advertised
	// in the CRD card. A mismatch means the payer returned a different questionnaire than
	// the card claimed — reject to prevent canonical substitution. Discovered missing here
	// (R3, alongside the AI-1 gap above): runCRDThenDTROrder's sibling site has always had
	// this fence; this order-dispatch prefix never did.
	fetchedURL, err := shnsdk.ParseQuestionnaireURL(questionnaireJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire url parse failed"})
		return dispatchResult{}, false
	}
	if fetchedURL != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire does not match advertised canonical"})
		return dispatchResult{}, false
	}

	// The operated $populate engine reads the FHIR store directly, so its subject must be
	// the store-resolvable Patient ref: the system of record's, read for the CRD request.
	subjectFHIRRef := "Patient/" + recs.sorID
	authored := g.cfg.Clock()
	qrJSON, _, err := g.cfg.Populator.Populate(ctx, packageJSON, PopulateContext{
		Member:         member,
		PatientRef:     patientRef,
		SubjectFHIRRef: subjectFHIRRef,
		CoverageRef:    coverageRef,
		OrderRef:       orderRef,
		Order:          dispatchQuestionnaireOrder(answer, orderJSON),
		Line:           dtrLine,
		Authored:       authored,
	})
	if err != nil {
		writeJSON(w, statusForPopulateErr(err), map[string]string{"error": messageForPopulateErr(err)})
		return dispatchResult{}, false
	}
	// QR-SUBJECT FENCE — the populated QR must be about the bound patient (logical ref).
	if subj, serr := questionnaireResponseSubject(qrJSON); serr != nil || subj != patientRef {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR subject does not match patient"})
		return dispatchResult{}, false
	}
	// QR-QUESTIONNAIRE FENCE — the QR must self-declare the canonical the card advertised.
	if qq, qerr := questionnaireResponseCanonical(qrJSON); qerr != nil || qq != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR questionnaire does not match canonical"})
		return dispatchResult{}, false
	}
	// CRUX EVIDENCE (C1) — capture the operated-$populate computed quantity answers off the QR
	// (linkIds 2.2 = O₂-sat / 2.3 = PaO₂). The native populator drops per-item FilledItem
	// attribution, so these are read straight from the returned QR. Surfaced in the response so the
	// live gate can prove the $populate ran br-payer's real prepop CQL against the seeded
	// observations (NOT an answer book). Empty when nothing populated (e.g. aged-out obs).
	qrAnswers := questionnaireResponseNumericAnswers(qrJSON)
	ctx = withFindingContext(ctx, findingContext{
		LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "own",
	})
	if status, msg := g.validateFHIRForContract(ctx, qrJSON, "egress", "pa.dtr", dtrLine, baseQRProfile); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}

	source := newRawDTRBuildSource(qrJSON, questionnaireJSON, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef, Authored: authored})
	qrJSON, status, msg = g.completeDTRContext(ctx, qrJSON, dtrLine, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef})
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return dispatchResult{}, false
	}
	return dispatchResult{
		qrSource: source,
		dtrLine:  dtrLine,
		pci:      pci, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, orderRef: orderRef,
		orderJSON: orderJSON, supplierJSON: supplierJSON, qrJSON: qrJSON, questionnaireJSON: questionnaireJSON, qrAnswers: qrAnswers,
		memberSystem: recs.memberSystem,
		member:       member, payer: payer, recipient: recipient, canonical: canonical,
	}, true
}

// handleDispatch originates the order-dispatch PA for a caller-named member — the SHN Kit's
// free-form "run against your data" entry. Same internal /scenario/* posture as its siblings
// (never public); the origination itself is originateDispatch, unchanged: order code,
// coverage, and supplier all come from the SoR, nothing persona-baked.
//
// Deliberately NOT on the scenarioMember personaSet seam (observability Phase 3): every
// sibling handler resolves a fixture member the seam remaps to a canary twin, but here the
// caller names the member explicitly — there is no default resolution to remap, the console
// never exposes this route, and the monitor canary never drives it. A personaSet query param
// is simply ignored, exactly like any other unknown query param on this Kit-facing surface.
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
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
	}))
	g.originateDispatch(w, r, req.Member)
}

// dispatchOrderOfRecord reads the member's open order-DISPATCH order, and the
// supplier it was dispatched to, out of the participant's OWN system of record.
// Both ARE the participant's records: the order code, the diagnosis and the
// supplier all come from the data, never from a literal. ok=false means the
// response is already written.
func (g *Gateway) dispatchOrderOfRecord(w http.ResponseWriter, r *http.Request, member string) (dispatchOrder, bool) {
	orderJSON, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).OpenOrderContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return dispatchOrder{}, false
	}
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no open order for member in SoR"})
		return dispatchOrder{}, false
	}
	orderID, performerRef, ok := parseOrderIDAndPerformer(orderJSON)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "open order missing id or performer"})
		return dispatchOrder{}, false
	}
	// The supplier (performer) is resolved from the order's performer ref via a SoR read —
	// not a literal. Fail closed if the supplier Organization is absent.
	supplierJSON, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(r.Context(), performerRef)
	if writeSoRFailure(w, readErr) {
		return dispatchOrder{}, false
	}
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "order performer (supplier) not resolvable from SoR"})
		return dispatchOrder{}, false
	}
	return dispatchOrder{
		orderJSON: orderJSON, supplierJSON: supplierJSON,
		orderRef: "DeviceRequest/" + orderID, performerRef: performerRef,
	}, true
}

// originateDispatch originates an order-dispatch PA off the given member's seeded DeviceRequest.
// DIVERGENCE 1 (unique to this caller of runCRDDispatch): the order + supplier come from the
// SoR (no literal code, no answer book) — the order code, the diagnosis, and the supplier ALL
// come from the data.
func (g *Gateway) originateDispatch(w http.ResponseWriter, r *http.Request, member string) {
	// Ordering preserved from before the runCRDDispatch extraction: an unknown member 400s
	// HERE (before the OpenOrder read), not as a 502 further down.
	_, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if writeSoRFailure(w, readErr) {
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	order, ok := g.dispatchOrderOfRecord(w, r, member)
	if !ok {
		return
	}

	res, ok := g.runCRDDispatch(w, r, member, order)
	if !ok {
		return
	}

	// --- PAS — the shared lean single-shot tail (submitClaimAndFollow). The genuine
	// outcome is conditional-coverage A4-pended → A1, and the A1 comes from the payer's
	// answer to the follow-up inquiry, never from anything this gateway does to the pend.
	// A payer that pends and does NOT resolve is answered with the pend and its
	// continuation, which the caller continues; it is not reported as a failed request. ---
	wait, ok := pasWaitOf(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "wait must be a whole number of seconds"})
		return
	}
	decision, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowInputs{
		pci: res.pci, patientRef: res.patientRef, coverageRef: res.coverageRef, coverage: res.coverage, insurer: res.insurer, member: res.member, memberSystem: res.memberSystem,
		recipient: res.recipient, orderRef: res.orderRef, orderJSON: res.orderJSON,
		supplierJSON: res.supplierJSON, source: res.qrSource, payer: res.payer, wait: wait,
	})
	if status != 0 {
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// FR-23: persist the payer-issued auth number against the order reference. Only an
	// approval has one; a denial and a pend have nothing to persist, and writing an
	// empty authorization number against the order would look like an authorization.
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(res.orderRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
			return
		}
	}

	writeJSON(w, http.StatusOK, decision.applyTo(uc03Resp{
		PARequired: true,
		QRAnswers:  res.qrAnswers,
	}))
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

// dispatchQuestionnaireOrder is the order the questionnaire step works from:
// the payer's updated order when its answer returned one, else the order sent.
func dispatchQuestionnaireOrder(answer crdOriginated, sent []byte) []byte {
	if len(answer.updatedOrder) > 0 {
		return answer.updatedOrder
	}
	return sent
}
