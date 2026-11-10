//go:build !shn_coreonly

// module1_originate_homeoxygen.go — originateDispatch, the provider-data order-dispatch
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

// handleHomeOxygen originates the HomeOxygen PA off the member's seeded DeviceRequest.
func (g *Gateway) handleHomeOxygen(w http.ResponseWriter, r *http.Request) {
	member, ok := g.scenarioMember(w, r, "homeoxygen")
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
	}))
	g.originateDispatch(w, r, member, member)
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
	// supplierStatus and supplierMsg are why the supplier's record could not
	// be read (422: the system holds none for the order's performer; else a
	// read failure's status). The order-dispatch leg carries only the
	// performer reference, so this is refused at the PAS claim's build, the
	// first leg that carries the supplier's record (pasClaimSourcesAt).
	supplierStatus int
	supplierMsg    string
	supplierErr    error
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
	// sorID is the id the participant's own system holds the member's Patient
	// under (crdOriginRecords), which the PAS build names member: the evidence
	// the PAS completion reads is named the same way (evidenceReadByMember).
	sorID string
	// memberSystem is the namespace the participant's own system names that
	// member under, carried from the one reading of their Patient (crdOriginRecords).
	memberSystem string
	payer        shnsdk.PayerIdentifier
	recipient    string
	canonical    string
	// requester is the records the order's requester leads to, read from the
	// participant's own system once, at the PAS claim's build
	// (pasClaimSourcesAt). Every PAS request about the order carries them
	// (carryOrderRequester).
	requester [][]byte
	// coverageParties are the participant's own records of the people the
	// member's Coverage names beside the member (a dependent's parent), read
	// with requester (coveragePartyRecords). Every PAS request under that
	// Coverage carries them.
	coverageParties [][]byte
	// pasDeferred is what the PAS build needs that the participant's records
	// could not supply before the first leg, refused at that build.
	pasDeferred pasDeferred
	// statedMember is the member the request must name for the claim facts
	// it states (statedMemberFault), or "" on a route whose request names the
	// member it addresses (dispatch).
	statedMember string
}

// runCRDDispatch is the shared order-dispatch prefix: CRD(order-dispatch) → DIVERGENCE-3
// advisory-card gate (NeedsDTR, not PARequired) → DTR fetch → operated $populate → the QR
// fences. Extracted from originateDispatch (behavior-preserving) so UC-03's demo arm — which
// needs to ATTEST a required item into the populated QR before the PAS tail, unlike
// originateDispatch's own callers — rides the identical CRD/DTR/populate plumbing instead of
// a second hand-rolled copy. On any failure it writes the HTTP error and returns ok=false.
func (g *Gateway) runCRDDispatch(w http.ResponseWriter, r *http.Request, member, statedMember string, order dispatchOrder) (dispatchResult, bool) {
	ctx := r.Context()

	pci, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if failOriginReadErr(r.Context(), w, readErr) {
		return dispatchResult{}, false
	}
	if !found {
		refuseOriginRouting(r.Context(), w, http.StatusBadRequest, "unknown member", reasonUnknownMember)
		return dispatchResult{}, false
	}
	patientRef := "Patient/" + member
	coverageRef := "Coverage/" + member

	// The order's product coding comes from the DATA (DeviceRequest.codeCodeableConcept),
	// never a literal. One with no {CPT,HCPCS} coding is carried by the order-dispatch and
	// questionnaire legs as the system holds it, and refused at the PAS claim's build, whose
	// item names the product (statedOrderFault).
	orderJSON := order.orderJSON
	orderRef := order.orderRef
	performerRef := order.performerRef
	supplierJSON := order.supplierJSON

	// Read the member's OWN open Coverage as the routing/identity SOURCE (FR-G40): the dispatch leg's
	// payer identity derives from the patient's real Coverage, not a synthetic CMS literal. realCov
	// stays a LOCAL (the recipient is resolved from it); the per-leg emit shapes are unchanged.
	realCov, ok := g.originCoverage(r.Context(), w, member, true)
	if !ok {
		return dispatchResult{}, false
	}
	// Resolve the payer HOLDER every dispatch/DTR/PAS leg routes to AND the parsed payer identity in
	// ONE parse of the member's own Coverage (FR-G40): no default — a miss fails closed HERE before
	// any leg (AI-G11 / OWD-G10). `payer` threads to the dispatch/coverage/PAS builders, so
	// routed-payer and payload-payer cannot diverge (one payer fact, read once).
	recipient, payer, status, msg := g.recipientForSoR(ctx, realCov)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": withCarriedLegs(ctx, msg)})
		return dispatchResult{}, false
	}
	// And that payer's own Organization record, from the same system: the
	// submission names it, and so does every inquiry about the submission. A
	// record the system cannot supply is refused at the PAS claim's build, the
	// first leg that carries it (pasClaimSourcesAt).
	realPayerOrg, realPayerRef, payerOrgStatus, payerOrgMsg, payerOrgErr := g.memberPayerOrganization(ctx, realCov, payer)
	// The request's claim input is readable, or it is refused here, before any
	// leg; what it states is checked at the PAS claim's build.
	if !pasOriginationRequestOrFail(w, r) {
		return dispatchResult{}, false
	}
	deferred := pasDeferred{payerOrgStatus: payerOrgStatus, payerOrgMsg: payerOrgMsg, payerOrgErr: payerOrgErr, supplierStatus: order.supplierStatus, supplierMsg: order.supplierMsg, supplierErr: order.supplierErr}

	// The order-dispatch request carries the participant's own Patient, Coverage
	// search result and history (module1_originate_crd.go); the order names the patient
	// the same way. The Coverage is read twice — above for routing (memberRoutingCoverage),
	// here as the search result the request carries — so each read keeps its own
	// refusal rules; both choose the same coverage (routingCoverageChoice).
	recs, status, msg, err := g.originCRDRecords(ctx, "crd-order-dispatch", member)
	if status != 0 {
		g.refuseOriginRecord(ctx, w, recipient, "crd-order-dispatch", status, msg, err)
		return dispatchResult{}, false
	}
	orderJSON, err = originOrder(recs, orderJSON)
	if err != nil {
		g.refuseOriginRecord(ctx, w, recipient, "crd-order-dispatch", http.StatusUnprocessableEntity, "name the patient in the open order: "+err.Error(), nil)
		return dispatchResult{}, false
	}

	// DIVERGENCE 2 — build + originate the ORDER-DISPATCH leg (not order-select).
	// Select-before-build — same rationale as the two
	// crd-order-select sites (module1_originate.go): the pa.crd builder is line-inert, so
	// BuildLine changes no byte, but the routing axis is real and the arm-1-only
	// backfill refused pa.crd@2.2-only peers this build serves byte-identically.
	// crdCorr is hoisted so selection, adaptation and the leg share one correlation.
	crdCorr := g.cfg.CorrelationGen()
	crdRoute, ok := g.selectLegLineOrFail(r.Context(), w, recipient, "crd-order-dispatch", crdCorr, relay.BuilderSDKCRDRequest)
	if !ok {
		return dispatchResult{}, false
	}
	// The dispatched order is resolved by the payer from the request's device history;
	// the supplier is named as the performer, and the system of record's device search
	// includes it (DeviceRequest:performer). Where no history was searched, the order
	// travels as a collection Bundle holding that one order (originatedDispatchRequest),
	// and no supplier record is added.
	crdReq, err := g.originatedDispatchRequest(crdCorr, recs, orderJSON, performerRef)
	if err != nil {
		g.refuseBuildFailure(r.Context(), w, recipient, "crd-order-dispatch", "build order-dispatch request failed: "+err.Error(), err)
		return dispatchResult{}, false
	}
	// Validation posture UNCHANGED (routing-only promotion): crdReq is a CDS
	// Hooks envelope, not a FHIR resource; the Patient, Coverage and history are the
	// participant's own records. No enforcement point is added or removed after
	// egressAdapt.
	sent, failed := g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "crd-order-dispatch", Recipient: recipient, PCI: pci, Corr: crdCorr, Route: &crdRoute,
		Build: &moduleBuild{
			Builder: relay.BuilderSDKCRDRequest, ContentType: "application/json", Body: crdReq,
		},
	})
	if failed != nil {
		g.writeLegFailure(r.Context(), w, failed)
		return dispatchResult{}, false
	}
	crdRespJSON := sent.Answer
	answer, err := readOriginatedAnswer(crdRespJSON, orderJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": withCarriedLegs(r.Context(), "card parse failed")})
		return dispatchResult{}, false
	}
	cov := answer.coverage

	// AI-1: a coverage denial STOPS — never routes DTR/PAS. Discovered missing here (R3,
	// while re-keying UC-03 onto this shared prefix): runCRDThenDTROrder's sibling gate
	// (module1_originate.go) has always checked cov.Covered FIRST regardless of the doc-needed
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
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": withCarriedLegs(r.Context(), "expected an advisory card advertising the HomeOxygen questionnaire")})
		return dispatchResult{}, false
	}
	canonical := shnsdk.StripCanonicalVersion(cov.Questionnaires[0])

	// --- DTR — operated $populate (the crux). ---
	// SELECT-BEFORE-BUILD on the fetch envelope too, not just the package
	// answer: the fetch is LINE-DEPENDENT (2.2's DTRDef makes `coverage` 1..1),
	// so the routed line must exist BEFORE the literal below is marshalled —
	// exactly the runCRDThenDTROrder sibling's ordering (module1_originate.go). The
	// selected line also picks the validator lane the package answer is checked
	// against (F7).
	dtrCorr := g.cfg.CorrelationGen()
	route, ok := g.selectLegLineOrFail(r.Context(), w, recipient, "dtr-questionnaire-fetch", dtrCorr, relay.BuilderSDKDTRPackage)
	if !ok {
		return dispatchResult{}, false
	}
	dtrLine := shnsdk.LineOf(route.Token)
	// The request is the $questionnaire-package operation's own input
	// (originatedPackageRequest, as the sibling site in module1_originate.go): the
	// system of record's Coverage search result, the dispatched order as the
	// payer returned it, the canonical as the payer stated it and the payer's
	// coverage-assertion-id as context, framed with the operation and sent only
	// to a payer that declares framed DTR operations.
	if status, msg := g.framedDTRRefusal(recipient); status != 0 {
		refuseOriginRouting(ctx, w, http.StatusUnprocessableEntity, msg, reasonFramedDTRUnsupported)
		return dispatchResult{}, false
	}
	// As at the order-select prefix: a payer-record read the system failed
	// to answer fails here, never carried as a record left out.
	if failOriginRead(ctx, w, payerOrgStatus, payerOrgMsg, payerOrgErr) {
		return dispatchResult{}, false
	}
	dtrReq, dtrPayload, err := originatedPackageRequest(dtrLine, recs, dispatchQuestionnaireOrder(answer, orderJSON), cov.Questionnaires[0], answer.assertionID, realPayerRef, realPayerOrg, g.referencedLeftOut(dtrCorr, recipient))
	if err != nil {
		g.refuseBuildFailure(r.Context(), w, recipient, "dtr-questionnaire-fetch", "build questionnaire-package request failed: "+err.Error(), err)
		return dispatchResult{}, false
	}
	sendRoute, carryStatus, carryMsg := g.carryUnchanged(ctx, route, dtrReq, dtrLine, dtrCorr, recipient)
	if carryStatus != 0 {
		writeJSON(w, carryStatus, map[string]string{"error": withCarriedLegs(r.Context(), carryMsg)})
		return dispatchResult{}, false
	}
	route = sendRoute
	sent, failed = g.sendLeg(ctx, r, legSend{
		Workstream: workstreamPA, Leg: "dtr-questionnaire-fetch", Recipient: recipient, PCI: pci, Corr: dtrCorr, Route: &route,
		Sealed: dtrPayload, Operation: shnsdk.FrameOperationQuestionnairePackage,
	})
	if failed != nil {
		g.writeLegFailure(r.Context(), w, failed)
		return dispatchResult{}, false
	}
	packageJSON := sent.Answer
	// This helper owns the dtr-questionnaire-fetch leg (and, above, crd-order-dispatch)
	// regardless of which caller's headline leg dispatched here — retag rather than
	// inherit, so a finding from this check never borrows the caller's leg name.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "dtr-questionnaire-fetch", CorrelationID: dtrCorr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, packageJSON, dtrLine, "pa.dtr", payer); status != 0 {
		writeJSON(w, status, map[string]string{"error": afterLegs(r.Context(), msg)})
		return dispatchResult{}, false
	}

	questionnaireJSON, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": afterLegs(r.Context(), "fetched questionnaire package has no Questionnaire")})
		return dispatchResult{}, false
	}

	// F5: verify the fetched Questionnaire's url matches the canonical the payer advertised
	// in the CRD card. A mismatch means the payer returned a different questionnaire than
	// the card claimed — reject to prevent canonical substitution. Discovered missing here
	// (R3, alongside the AI-1 gap above): runCRDThenDTROrder's sibling site has always had
	// this fence; this order-dispatch prefix never did.
	fetchedURL, err := shnsdk.ParseQuestionnaireURL(questionnaireJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": afterLegs(r.Context(), "fetched questionnaire url parse failed")})
		return dispatchResult{}, false
	}
	if fetchedURL != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": afterLegs(r.Context(), "fetched questionnaire does not match advertised canonical")})
		return dispatchResult{}, false
	}

	// The operated $populate engine reads the FHIR store directly, so its subject must be
	// the store-resolvable Patient ref: the system of record's, read for the CRD request.
	subjectFHIRRef := "Patient/" + recs.sorID
	authored := g.cfg.Clock()
	populator := g.populator()
	own := ownEngine(populator)
	qrJSON, _, err := populator.Populate(ctx, packageJSON, PopulateContext{
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
		g.failPopulate(r.Context(), w, recipient, err)
		return dispatchResult{}, false
	}
	// QR-SUBJECT FENCE — the populated QR must be about the bound patient (logical ref).
	if subj, serr := questionnaireResponseSubject(qrJSON); serr != nil || subj != patientRef {
		g.refuseOwnFill(r.Context(), w, recipient, msgFillForeignSubject)
		return dispatchResult{}, false
	}
	// QR-QUESTIONNAIRE FENCE — the QR must self-declare the canonical the card advertised.
	if qq, qerr := questionnaireResponseCanonical(qrJSON); qerr != nil || qq != canonical {
		g.refuseOwnFill(r.Context(), w, recipient, msgFillForeignCanonical)
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
	if status, msg := g.validateFHIRForContract(withOwnDeclaredProfiles(ctx, own), qrJSON, "egress", "pa.dtr", dtrLine, baseQRProfile); status != 0 {
		writeJSON(w, status, map[string]string{"error": afterLegs(r.Context(), msg)})
		return dispatchResult{}, false
	}

	source := newRawDTRBuildSource(qrJSON, questionnaireJSON, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef, Authored: authored}, own)
	qrJSON, status, msg = g.completeDTRContext(ctx, qrJSON, dtrLine, shnsdk.QRContext{PatientRef: patientRef, CoverageRef: coverageRef, OrderRef: orderRef}, source.declaresOwn())
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": afterLegs(r.Context(), msg)})
		return dispatchResult{}, false
	}
	return dispatchResult{
		qrSource: source,
		dtrLine:  dtrLine,
		pci:      pci, patientRef: patientRef, coverageRef: coverageRef, coverage: realCov, insurer: realPayerOrg, orderRef: orderRef,
		orderJSON: orderJSON, supplierJSON: supplierJSON, qrJSON: qrJSON, questionnaireJSON: questionnaireJSON, qrAnswers: qrAnswers,
		memberSystem: recs.memberSystem, sorID: recs.sorID,
		member: member, payer: payer, recipient: recipient, canonical: canonical,
		pasDeferred: deferred, statedMember: statedMember,
	}, true
}

// handleDispatch originates the order-dispatch PA for a caller-named member — the SHN Kit's
// free-form "run against your data" entry. Same internal /scenario/* posture as its siblings
// (never public); the origination itself is originateDispatch, unchanged: order code,
// coverage, and supplier all come from the SoR, nothing persona-baked.
//
// Deliberately NOT on the scenarioMember personaSet seam (observability Phase 3): every
// sibling handler resolves a fixture member the seam remaps to a canary twin, but here the
// caller names the member explicitly — there is no default resolution to remap, only the Kit
// drives this route, and the monitor canary never does. A personaSet query param
// is simply ignored, exactly like any other unknown query param on this Kit-facing surface.
func (g *Gateway) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Member string `json:"member"`
	}
	r = ensureOriginationClaimInput(r)
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // lenient like the sibling handlers
	}
	if strings.TrimSpace(req.Member) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": afterLegs(r.Context(), "member is required")})
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: "crd-order-dispatch", Seam: "originate", Whose: "own",
	}))
	g.originateDispatch(w, r, req.Member, "")
}

// dispatchOrderOfRecord reads the member's open order-DISPATCH order, and the
// supplier it was dispatched to, out of the participant's OWN system of record.
// Both ARE the participant's records: the order code, the diagnosis and the
// supplier all come from the data, never from a literal. ok=false means the
// response is already written.
func (g *Gateway) dispatchOrderOfRecord(w http.ResponseWriter, r *http.Request, member string) (dispatchOrder, bool) {
	orderJSON, ok, readErr := ReadSystemOfRecord(g.cfg.SoR).OpenOrderContext(r.Context(), member)
	if failOriginReadErr(r.Context(), w, readErr) {
		return dispatchOrder{}, false
	}
	if !ok {
		g.refuseOriginRecord(r.Context(), w, "", "crd-order-dispatch", http.StatusUnprocessableEntity, "no open order for member in SoR", nil)
		return dispatchOrder{}, false
	}
	orderID, performerRef, ok := parseOrderIDAndPerformer(orderJSON)
	if !ok {
		g.refuseOriginRecord(r.Context(), w, "", "crd-order-dispatch", http.StatusUnprocessableEntity, "open order missing id or performer", nil)
		return dispatchOrder{}, false
	}
	// The supplier (performer) is resolved from the order's performer ref via a SoR read —
	// not a literal. A supplier the system cannot supply is refused where its record is
	// first carried, at the PAS claim's build (dispatchOrder.supplierStatus).
	out := dispatchOrder{orderJSON: orderJSON, orderRef: "DeviceRequest/" + orderID, performerRef: performerRef}
	supplierJSON, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext(r.Context(), performerRef)
	switch {
	case readErr != nil:
		out.supplierStatus, out.supplierMsg = SoRFailureResponse(readErr)
		out.supplierErr = readErr
	case !found:
		out.supplierStatus, out.supplierMsg = http.StatusUnprocessableEntity, "the system of record has no supplier record "+performerRef+" for the order's performer, which the claim carries (fidelity of SHN's own build)"
	default:
		out.supplierJSON = supplierJSON
	}
	return out, true
}

// originateDispatch originates an order-dispatch PA off the given member's seeded DeviceRequest.
// DIVERGENCE 1 (unique to this caller of runCRDDispatch): the order + supplier come from the
// SoR (no literal code, no answer book) — the order code, the diagnosis, and the supplier ALL
// come from the data.
func (g *Gateway) originateDispatch(w http.ResponseWriter, r *http.Request, member, statedMember string) {
	if !waitOrFail(w, r) {
		return
	}
	// Ordering preserved from before the runCRDDispatch extraction: an unknown member 400s
	// HERE (before the OpenOrder read), not as a 502 further down.
	_, _, found, readErr := ReadSystemOfRecord(g.cfg.SoR).ResolvePatientContext(r.Context(), member)
	if failOriginReadErr(r.Context(), w, readErr) {
		return
	}
	if !found {
		refuseOriginRouting(r.Context(), w, http.StatusBadRequest, "unknown member", reasonUnknownMember)
		return
	}
	order, ok := g.dispatchOrderOfRecord(w, r, member)
	if !ok {
		return
	}

	res, ok := g.runCRDDispatch(w, r, member, statedMember, order)
	if !ok {
		return
	}

	// --- PAS — the shared lean single-shot tail (submitClaimAndFollow). The genuine
	// outcome is conditional-coverage A4-pended → A1, and the A1 comes from the payer's
	// answer to the follow-up inquiry, never from anything this gateway does to the pend.
	// A payer that pends and does NOT resolve is answered with the pend and its
	// continuation, which the caller continues; it is not reported as a failed request. ---
	wait, _ := pasWaitOf(r) // checked on the route's entry (waitOrFail)
	decision, status, msg, err := g.submitClaimAndFollow(r.Context(), r, pasFollowInputs{
		pci: res.pci, patientRef: res.patientRef, coverageRef: res.coverageRef, coverage: res.coverage, insurer: res.insurer, member: res.member, sorID: res.sorID, memberSystem: res.memberSystem,
		recipient: res.recipient, orderRef: res.orderRef, orderJSON: res.orderJSON,
		supplierJSON: res.supplierJSON, source: res.qrSource, payer: res.payer, wait: wait,
		pasDeferred: res.pasDeferred, statedMember: res.statedMember,
	})
	if status != 0 {
		g.answerFollowFailure(r.Context(), w, status, msg, err)
		return
	}

	// FR-23: persist the payer-issued auth number against the order reference. Only an
	// approval has one; a denial and a pend have nothing to persist, and writing an
	// empty authorization number against the order would look like an authorization.
	if decision.Decision == PASDecisionApproved {
		if err := g.cfg.Store.StoreAuthNumber(res.orderRef, decision.Parsed.PreAuthRef); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": afterLegs(r.Context(), "holder write failed (auth number)")})
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
