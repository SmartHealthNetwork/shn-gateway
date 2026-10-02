// ingress.go — the DaVinciIngress origination driver: the second origination driver over
// OriginateLeg. Terminates the three inbound Da Vinci protocols, resolves+inlines prefetch
// (CRD), drives OriginateLeg per call through the ExchangeStore seam, and wraps each response
// back into its native envelope. Mounted on the provider role when Config.IngressEnabled.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// prefetchValues returns a CDS Hooks request's prefetch values in key order,
// null values left out: an external payor Organization arrives as another
// prefetch value (a resource, or an entry of a Bundle value), so the CRD
// ingress resolves against them (carriedRefs). A request may carry the same
// record under two keys.
func prefetchValues(prefetch map[string][]byte) [][]byte {
	keys := make([]string, 0, len(prefetch))
	for k := range prefetch {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([][]byte, 0, len(prefetch))
	for _, key := range keys {
		v := bytes.TrimSpace(prefetch[key])
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// crdIngressRecipient routes a prepared CDS Hooks request by the coverage it
// carries (FR-G40; no default): a bare Coverage or a Bundle of Coverages that
// name one payer. The payer's Organization is looked up among the request's
// own prefetch values and, for a coverage read from the system of record, in
// that system; for one read through the request's fhirServer, there (one
// read, fhirServerPayor). For a coverage the EHR sent naming its payor by
// reference alone (as the coverage template, which asks for no _include,
// lets it), the Organization is read only to route (sentPayorResolver): a
// payor nothing resolves is refused 422 with its reason and the remedy that
// would resolve that reference (unresolvedPayor), a refused
// fhirServer read of it with the read's status and reason under that
// coverage's prefix (sentPayorRefusal), and a failed system-of-record read
// with the system-of-record failure status.
func (g *Gateway) crdIngressRecipient(ctx context.Context, prepared crdIngressRequest) (string, int, string) {
	coverage, carried := prepared.values["coverage"]
	switch {
	case !carried && prepared.coverageStatus != 0:
		return "", prepared.coverageStatus, prepared.coverageMsg
	case !carried || string(bytes.TrimSpace(coverage)) == "null":
		// CDS Hooks' 412: the service could not obtain the data the request
		// left out (a null coverage prefetch, or none the system holds).
		return "", http.StatusPreconditionFailed, "no coverage in request or system of record"
	}
	// The payor is resolved among the prefetch values first (a coverage the
	// system of record supplied among them, with the records its search
	// included), by fullUrl or Type/id; matches that do not name one payer
	// are refused, never picked (bundleref.go).
	refs := &payorRefs{local: carriedRefs(prefetchValues(prepared.values)), disagree: payorDisagreesInRequest}
	if prepared.coverageFromSoR {
		refs.disagree = payorDisagreesWithSearch
	}
	readErr := new(error)
	fhirStatus, fhirMsg := new(int), new(string)
	if prepared.fhirServer != nil {
		// A payor the read through fhirServer did not resolve is read there
		// once more: the only other place that coverage's references live.
		refs.next, fhirStatus, fhirMsg = g.fhirServerPayor(ctx, prepared.fhirServer)
	}
	if prepared.coverageFromSoR {
		refs.next, readErr = sorReferenceCallback(ctx, g.cfg.SoR)
	}
	sent := !prepared.coverageFromSoR && prepared.fhirServer == nil
	unresolved := new(unresolvedPayor)
	if sent {
		// A coverage the EHR sent: a payor reference the request's own
		// resources do not answer is read only to route (sentPayorResolver).
		refs.next, unresolved, fhirStatus, fhirMsg, readErr = g.sentPayorResolver(ctx, prepared.sentPayor)
	}
	recipient, status, msg := g.recipientForCoverages(coverageResources(coverage), refs)
	if *readErr != nil {
		status, msg := SoRFailureResponse(*readErr)
		return "", status, msg
	}
	if *fhirStatus != 0 {
		if sent {
			// The request carried a coverage: its refusal is that coverage's
			// (no payer identifier), with the read's reason and status.
			return "", *fhirStatus, sentPayorRefusal(*fhirMsg)
		}
		return "", *fhirStatus, *fhirMsg
	}
	if sent && msg == noPayerIdentifier {
		// An Organization or urn reference nothing resolved names the
		// remedy that resolves it (unresolvedPayor); another kind of payor
		// keeps the bare text. A resolved payor with no payer identifier
		// already says why (recipientForCoverages).
		return "", status, unresolved.refusal(msg)
	}
	return recipient, status, msg
}

// handleIngressMetadata serves the provider ingress CapabilityStatement
// (FR-37 per-role). Public like the payer's
// /metadata — a conformance statement is discovery surface, not PHI.
func (g *Gateway) handleIngressMetadata(w http.ResponseWriter, _ *http.Request) {
	// D1a: the published conformance surface names THE DECLARED SET, single-sourced
	// through the same accessor selection and the registry stamp read — a gateway
	// cannot advertise one set and route on another.
	b, err := shnsdk.BuildProviderIngressCapabilityStatement(g.cfg.Clock(), g.declaredContractVersions())
	if err != nil {
		http.Error(w, "capability statement build failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/fhir+json")
	_, _ = w.Write(b)
}

func (g *Gateway) handleCDSDiscovery(w http.ResponseWriter, r *http.Request) {
	if g.ingressAuthRefused(w, r) {
		return
	}
	body, err := g.cdsDiscoveryJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build discovery failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleCRDIngress terminates a CDS Hooks request from the EHR at
// POST /cds-services/{id}: {id} must be an advertised service (404 otherwise)
// and the request's hook must be that service's hook (400 otherwise). The hook
// picks the leg (order-sign and order-select ride crd-order-select;
// order-dispatch rides crd-order-dispatch). The handler subject-binds the
// request, carries the EHR's own bytes (with the callback removed, a coverage
// read through the removed fhirServer to route by carried and, when the
// participant opts in to enrichment, absent prefetch obtained from its system
// of record), threads a
// metadata-only Exchange, and relays the payer's answer back to the EHR exactly
// once it meets the CDS Hooks response rules.
func (g *Gateway) handleCRDIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	if g.ingressAuthRefused(w, r) {
		return
	}
	svc, offered, known := g.advertisedCDSServiceByID(r.PathValue("id"))
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown CDS service " + strconv.Quote(r.PathValue("id")), "offered": offered})
		return
	}
	legType := svc.Leg
	scope.leg = legType
	exchangeOf(r.Context()).leg(legType)
	exchangeOf(r.Context()).operation(svc.Hook)
	// Tag the leg for every check before routing; the correlation id is added
	// once the leg is routed.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: legType, Seam: "provider-ingress", Whose: "own"}))
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	var head struct {
		Hook string `json:"hook"`
	}
	// A hook of the wrong type is decided, once, with every other value of
	// the wrong type when the request is read for its subject below.
	err = decodeMessage(body, &head)
	if err != nil && !valueTypeError(err) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse cds request failed"})
		return
	}
	// A hook other than the service's is the request's own shape: below strict
	// it is carried on the leg of the service the EHR addressed. The payer's
	// gateway picks its service by the body's hook and refuses, at every level,
	// a hook that leg does not carry (selectCRDService).
	if err == nil && head.Hook != svc.Hook && g.guard(r.Context(), KindContent, RuleRequestShape, body) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("CDS service %s is for hook %s, not %s", svc.ID, svc.Hook, strconv.Quote(head.Hook))})
		return
	}
	// Bind the subject — every patient reference must resolve to one pci.
	pci, status, msg := g.ingressCRDSubjectPCIContext(r.Context(), body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	member := g.memberForPCI(body)
	// Keep the EHR's bytes; remove the callback and, under enrichment, add the
	// prefetch values it left out, from this participant's own system of record.
	prepared, status, msg := g.ingressEnsureSelfContainedContext(r.Context(), legType, body, member)
	if status != 0 {
		if status == http.StatusPreconditionFailed {
			// No coverage to route by could be obtained: a routing refusal.
			exchangeOf(r.Context()).routed(status)
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	recipient, status, msg := g.crdIngressRecipient(r.Context(), prepared)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// A coverage read through the request's own fhirServer is carried
	// (E-07) once routing has used it: the request is built now, after any
	// payor Organization read routing made. A request refused above builds
	// nothing and sends nothing.
	if status, msg := g.carryFHIRServerCoverage(&prepared); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Route selection (select-before-build promotion, site census 2026-08-14). This site
	// relays bytes the PARTNER built, so OriginateLeg's arm-1-only backfill would
	// normally be the honest posture (its own comment: an arm-2/3 token would
	// mis-stamp already-built bytes). For pa.crd that rationale is VACUOUS: pa.crd
	// bytes are LINE-INERT by verified derivation — compat.go's 2.0->2.1 and
	// 2.1->2.2 rows are identity, live-re-derived against the real CRD
	// StructureDefinitions — so stamping the routed line is exactly as truthful
	// for partner-built CDS Hooks JSON as for our own. RE-ADJUDICATION TRIGGER —
	// this promotion must be re-opened, not silently carried, if EITHER (a) a
	// future CRD delta makes a line behaviorally distinguishable, OR (b) this
	// site starts relaying partner content outside the derivation's scope: the
	// line-inertness claim is PRODUCE-IFF, established over the four
	// sub-extensions sdk/crd.go's own producer/consumer touch, and a partner's
	// bytes are broader than that — a partner exercising CRD fields SHN neither
	// builds nor reads is not covered by it. Selection precedes the exchange so a
	// refusal costs no Exchange record.
	child := g.ingressCorrelation(w, r)
	route, ok := g.selectLegLineOrFail(w, recipient, legType, child)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	// Validation posture UNCHANGED: this driver $validates nothing on egress (it
	// relays a partner's envelope), and the promotion adds no enforcement point.
	// The CDS Hooks request is the same at every line, so the walk changes no
	// byte; a walk that would change one is refused rather than sent. The walk
	// reads the bytes this gateway sends (the callback removed, obtained
	// prefetch added), never the EHR's raw request.
	requestKey := relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	sent, terr := relay.Transmit(prepared.request, relay.Check(requestKey))
	if terr != nil {
		g.ownershipRefused(requestKey, terr)
		writeOwnershipFault(w)
		return
	}
	adapted, _, aerr := g.egressAdapt(route, sent, ExchangeIdentity{CorrelationID: child, LegType: legType, Counterpart: recipient})
	if aerr != nil {
		writeJSON(w, adaptFailureStatus(aerr), map[string]string{"error": aerr.Error()})
		return
	}
	if !bytes.Equal(adapted, sent) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the CDS Hooks request cannot be carried to the payer's line unchanged"})
		return
	}
	// The request is routed and carried: record the prefetch values it is
	// carried without (below strict), under the routed leg's correlation id.
	prepared.carried.record(r.Context())
	// One Exchange, one leg (the EHR owns grouping in pure pass-through).
	ex := g.exchanges.Begin(workstreamPA)
	request := prepared.request
	// The media type the payer stated for its answer, relayed to the EHR with it.
	var answerType string
	respJSON, err := g.OriginateLeg(withAnswerMediaTypeSink(r.Context(), &answerType), r, recipient, legType, pci, child, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: request, Carried: true})
	leg := Leg{Type: legType, Physics: paCatalog[legType].Physics,
		Content: Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: request, Carried: true}, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		// The recipient answered non-2xx — relay its framed answer verbatim (Content-Type
		// from the frame; a non-empty answer stating none is application/fhir+json) via
		// the shared origination helper.
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// The payer's answer is relayed exactly once it meets the CDS Hooks response
	// rules at the routed line; the outcome label is read from its coverage
	// information (metadata; never clinical content).
	outcome, status, msg := g.crdAnswerOutcome(r.Context(), respJSON, shnsdk.LineOf(route.Token))
	if status != 0 {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	g.recordLeg(ex.ID, leg.Project(child, outcome))
	ct := cdsAnswerMediaType(answerType)
	g.writePayload(w, http.StatusOK, ct, relay.Exact(relay.NewBody(respJSON, relay.OriginPeerFrame), ct),
		relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// handleDTRIngress terminates an EHR's $questionnaire-package request and
// carries the EHR's own Parameters to the payer on the
// dtr-questionnaire-fetch leg, naming the operation in the request frame:
// exactly as sent, or, when the participant opts in to enrichment, with the
// patient's Coverage and Patient added from its system of record when the
// request carries none (prepareDTRPackageRequest). Every resource is bound to
// one patient, the request is routed by every coverage it carries (or, when
// it carries none, by the one its system of record holds), a payer that does not
// accept framed DTR operations is refused before anything is sent, and the
// payer's answer is relayed to the EHR exactly. The ingress does not invoke
// the Populator: the EHR's own DTR application populates.
func (g *Gateway) handleDTRIngress(w http.ResponseWriter, r *http.Request) {
	const legType = "dtr-questionnaire-fetch"
	w, scope := g.withScope(w, relay.RoleRequester)
	scope.leg = legType
	w = &fhirOperationWriter{w}
	if g.ingressAuthRefused(w, r) {
		return
	}
	// Named once the caller is authenticated, as on the other routes.
	exchangeOf(r.Context()).leg(legType)
	// Tag the leg for every check before routing; the correlation id is added
	// once the leg is routed.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: legType, Seam: "provider-ingress", Whose: "own"}))
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	prepared, status, msg := g.prepareDTRPackageRequest(r.Context(), body)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Route by every coverage the request carries (FR-G40; no default).
	recipient, status, msg := g.dtrIngressRecipient(r.Context(), prepared)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// A payer that has not declared framed DTR operations would drop the
	// operation header: refused before anything is sent.
	if status, msg := g.framedDTRRefusal(recipient); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	// Select-before-send: the routed line is stamped on the request frame and
	// picks nothing in the EHR's bytes, which are carried as they are at every
	// line. The walk to the payer's line changes no byte of a questionnaire
	// request (envelopeEgressLegs); a walk that would change one is refused
	// rather than sent.
	child := g.ingressCorrelation(w, r)
	route, ok := g.selectLegLineOrFail(w, recipient, legType, child)
	if !ok {
		return
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: legType, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	requestKey := relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionRequest, Outcome: relay.OutcomeCarried}
	sent, terr := relay.Transmit(prepared.request, relay.Check(requestKey))
	if terr != nil {
		g.ownershipRefused(requestKey, terr)
		writeOwnershipFault(w)
		return
	}
	adapted, _, aerr := g.egressAdapt(route, sent, ExchangeIdentity{CorrelationID: child, LegType: legType, Counterpart: recipient})
	if aerr != nil {
		writeJSON(w, adaptFailureStatus(aerr), map[string]string{"error": aerr.Error()})
		return
	}
	if !bytes.Equal(adapted, sent) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the questionnaire-package request cannot be carried to the payer's line unchanged"})
		return
	}
	// The request is routed and carried: record the Patient it is carried
	// without (below strict), under the routed leg's correlation id.
	prepared.carried.record(r.Context())
	ex := g.exchanges.Begin(workstreamPA)
	content := Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route),
		Payload: prepared.request, Carried: true, Operation: shnsdk.FrameOperationQuestionnairePackage,
		MediaType: carriedFHIRMediaType(r.Header.Get("Content-Type"))}
	pkgJSON, err := g.OriginateLeg(r.Context(), r, recipient, legType, prepared.pci, child, "", content)
	leg := Leg{Type: legType, Physics: paCatalog[legType].Physics, Content: content, Subjects: subjectsOf(prepared.pci)}
	if err != nil {
		g.recordLeg(ex.ID, leg.Project(child, "error"))
		// The recipient answered non-2xx — relay its framed answer verbatim (Content-Type
		// from the frame; a non-empty answer stating none is application/fhir+json) via
		// the shared origination helper.
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	g.recordLeg(ex.ID, leg.Project(child, "ok"))
	g.writePayload(w, http.StatusOK, "application/fhir+json",
		relay.Exact(relay.NewBody(pkgJSON, relay.OriginPeerFrame), "application/fhir+json"),
		relay.Key{Leg: legType, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// subjectsOf returns a 1-element subjects slice for a non-empty pci, else nil.
func subjectsOf(pci string) []string {
	if pci == "" {
		return nil
	}
	return []string{pci}
}

func (g *Gateway) handlePASIngress(w http.ResponseWriter, r *http.Request) {
	w, scope := g.withScope(w, relay.RoleRequester)
	w = &fhirOperationWriter{w}
	if g.ingressAuthRefused(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, shnsdk.MaxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	// F-PB-INGRESS: discriminate $submit vs amended re-POST. A conformant $submit carrying
	// Claim.related[prior] is an AMENDMENT (FR-21) and MUST route the conformant UPDATE leg
	// (pas-claim-update) — its own provider-tpo PA-update authority + the FR-32 inbound
	// gate (conformantPASUpdateBind). Originating it as pas-claim would mis-bind the
	// authority/responder. An initial submit (no related[prior]) routes pas-claim. The
	// FR-32 Provenance/DR enforcement still fires DOWNSTREAM at the payer; the ingress only picks
	// the leg. One parse (F-B2 extractor) serves BOTH discrimination AND the corr-threading below.
	// It is read before the subject bind so every check is tagged with its leg.
	// A value of the wrong type outside Claim.related is the bundle's own shape:
	// below strict it is read past (the facts hold every value that fit), so an
	// amendment still picks the update leg; at strict the read fails as it always
	// has. Nothing is recorded here: the payer's gateway judges the update.
	f, fstatus, _ := readConformantPASUpdateFacts(body, func(rule string) bool {
		return g.policy().Decide(KindContent, rule, VerdictInvalid) == Refuse
	})
	leg := "pas-claim"
	if fstatus == 0 && f.relatedClaim != "" {
		leg = "pas-claim-update"
	}
	// Tag the leg for every check before routing; the correlation id is added
	// once the leg is routed.
	r = r.WithContext(withFindingContext(r.Context(), findingContext{LegType: leg, Seam: "provider-ingress", Whose: "own"}))
	// Bind the subject across the conformant bundle (every patient reference → one pci). The
	// minimized ParseClaimBundle path is retired here — a real Da Vinci partner sends the full
	// conformant bundle (Patient + Coverage + payor Org + …), which ParseClaimBundle rejects. The
	// minimized pas-claim leg stays for the SDK / 8-scenario origination path (originate.go).
	pci, status, msg := g.ingressPASNativeSubjectPCIContext(r.Context(), body)
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
	// findInBundle form), NOT in the provider SoR; bundleref.go says which entry a reference
	// names. Contained / inline payor forms still route without hitting resolveRef.
	recipient, _, status, msg := g.pasRecipient(body)
	if status != 0 {
		exchangeOf(r.Context()).routed(status)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	scope.leg = leg
	exchangeOf(r.Context()).leg(leg)
	// R8 re-home (FR-16/FR-27): fence at the provider-facing edge too, before this
	// gateway ever originates the bundle onward — a nonconformant clinician/patient
	// QR item is rejected here regardless of which leg it routes as (the property
	// belongs to any QR item, not only to amends; mirrors the payer-side fence in
	// inbound.go). The attestation is the QR's own content (RuleAttestation): not
	// checked at none, recorded at observe and carried, refused at strict. The
	// payer's gateway fences it on its own side.
	if reason, ok := fenceAttestedItems(body); !ok && g.guard(r.Context(), KindContent, RuleAttestation, body) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": reason})
		return
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
	// Security: the pend is keyed by (subjectPCI, corr) where subjectPCI is this gateway's own
	// binding of the member the request names (ingressPASNativeSubjectPCI above). A corr threads
	// only to that member's pends — no cross-member hijack via a crafted identifier.
	// The leg's id: the Claim's own urn:shn:correlation when it names one (the
	// payer's key for this authorization); else the caller's trace value when
	// that value is one of this Claim's own identifiers — the caller already
	// uses it as the Claim's identity, and an amend names it in related[] — else
	// a freshly minted id. A trace value that is not the Claim's identity never
	// becomes the key, so reusing it cannot collide two claims at the payer.
	var child string
	switch trace := ingressTrace(r); {
	case fstatus == 0 && f.claimCorrelation != "":
		child = f.claimCorrelation
	case fstatus == 0 && trace != "" && slices.Contains(f.claimIdentifiers, trace):
		child = trace
	}
	if child != "" {
		// The Claim's identity is the leg's id and the value the caller is told.
		stampIngressIDs(w, child, child)
		noteIngressTrace(r, ingressTrace(r), child)
	} else {
		child = g.ingressCorrelation(w, r)
	}
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: leg, CorrelationID: child, Seam: "provider-ingress", Whose: "own",
	}))
	// D-7 census, adjudicated 2026-08-14 — this site DELIBERATELY STAYS on
	// OriginateLeg's arm-1-only empty-ProfileID backfill while its CRD and
	// DTR-fetch siblings in this same driver were promoted to select-before-build.
	// The reason is contract-specific, not driver-specific: `body` is the
	// partner's PAS Bundle relayed VERBATIM, and pa.pas is LINE-SENSITIVE (a 2.2
	// Claim carries item extensions a 2.0 Claim must not — the very reason
	// selection had to precede the build for our own PAS legs). Routing these
	// bytes at an arm-2/3 line would either mis-stamp a payload built elsewhere
	// or transform a counterparty's content at the forward edge — the
	// transform-at-the-forward-edge deferral class, whose current posture is
	// native.go's arm-1 pin and which goes live with the strict-extensions work, not
	// here. pa.crd could be promoted precisely because it is line-INERT; pa.pas
	// cannot. Do not "finish the sweep" without re-adjudicating that deferral.
	requestObservation := append([]byte(nil), body...)
	var responseObservation []byte
	var observedTarget string
	defer func() {
		g.certificationPair(leg, "provider-ingress", child, shnsdk.LineOf(observedTarget), requestObservation, responseObservation)
	}()
	observationContext := context.WithValue(r.Context(), certificationTargetKey{}, &observedTarget)
	// The participant's Bundle is carried exactly as it arrived.
	request := relay.Exact(relay.NewBody(body, relay.OriginIngressRequest), "application/fhir+json")
	crJSON, err := g.OriginateLeg(observationContext, r, recipient, leg, pci, child, "",
		Content{WorkstreamType: workstreamPA, Payload: request, Carried: true})
	responseObservation = append([]byte(nil), crJSON...)
	var relayed *RelayError
	if errors.As(err, &relayed) {
		responseObservation = append([]byte(nil), relayed.Body...)
	}
	legProj := Leg{Type: leg, Physics: paCatalog[leg].Physics,
		Content: Content{WorkstreamType: workstreamPA, Payload: request, Carried: true}, Subjects: []string{pci}}
	if err != nil {
		g.recordLeg(ex.ID, legProj.Project(child, "error"))
		// The recipient answered non-2xx — relay its framed answer verbatim (Content-Type
		// from the frame; a non-empty answer stating none is application/fhir+json) via
		// the shared origination helper.
		if g.relayOriginationError(w, err) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// Every native operation response, including an external SDK holder's,
	// must satisfy the complete Bundle contract before reaching the caller.
	// Validate without rewriting the payer's bytes or repairing missing evidence.
	// The answer is the payer's content: an answer graph or decision this
	// gateway cannot read (RuleAnswerShape) and an answer whose subjects do not
	// bind to its ClaimResponse's patient (RulePatientAnswer) refuse at strict;
	// below strict the payer's bytes are relayed exactly as received, recorded
	// at observe. The answer is read at every level for the local record below,
	// which never acts on an answer it could not read.
	answerCtx := withFindingContext(r.Context(), findingContext{LegType: leg, CorrelationID: child, Seam: "provider-ingress", Whose: "peer"})
	unread := ""
	if _, bad := validateNativePASResponse(crJSON); bad.Status != 0 {
		// A repeated member name is read one way only (RuleDuplicateKey): it
		// refuses at every level, with strict's refusal.
		if repeatsAMember(answerCtx, crJSON) || g.guard(answerCtx, KindContent, RuleAnswerShape, crJSON) {
			g.recordLeg(ex.ID, legProj.Project(child, "error"))
			writeJSON(w, bad.Status, map[string]string{"error": bad.Message})
			return
		}
		unread = RuleAnswerShape
	}
	// An answer that cannot be read has no subjects to bind; the rule above
	// already covers it.
	if unread == "" && pasResponseSubjectMismatch(crJSON) != nil {
		if g.guard(answerCtx, KindContent, RulePatientAnswer, crJSON) {
			status, msg := refusePASResponseSubjects(ex.ID, "pas-claim", http.StatusBadGateway, crJSON)
			g.recordLeg(ex.ID, legProj.Project(child, "error"))
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
		unread = RulePatientAnswer
	}
	// Decision fields distinguish pending and terminal responses; both are Bundles.
	// The exchange's leg outcome is metadata; it is read from the decision only
	// when the answer was read. An answer relayed unread, or whose decision may
	// be about another patient, is recorded "answered" and nothing is derived
	// from it (localWriteSkipped).
	outcome := "complete"
	if unread != "" {
		outcome = "answered"
		g.localWriteSkipped(leg, child, unread)
	} else if pended, _, perr := shnsdk.ParsePendedResponse(crJSON); perr == nil && pended {
		outcome = "pended"
	} else if res, perr := shnsdk.ParseClaimResponse(crJSON); perr == nil && res.Outcome != "" {
		outcome = res.Outcome // approved | denied
	}
	g.recordLeg(ex.ID, legProj.Project(child, outcome))
	// Near-relay: return the validated payer Bundle verbatim.
	g.writePayload(w, http.StatusOK, "application/fhir+json",
		relay.Exact(relay.NewBody(crJSON, relay.OriginPeerFrame), "application/fhir+json"),
		relay.Key{Leg: leg, Role: relay.RoleRequester, Direction: relay.DirectionResponse, Outcome: relay.OutcomeAnswered})
}

// LocalWriteSkippedEvent is the observer event emitted when a provider ingress
// relays a payer's answer it could not read (below strict) and so derives
// nothing from it for its own records. It carries the leg, the correlation id
// and the rule the answer broke; never the answer.
const LocalWriteSkippedEvent = "pa.local-write-skipped"

// localWriteSkipped records that the exchange's local record for leg took
// nothing from a relayed answer that broke rule: the leg outcome is recorded
// as "answered" rather than read from the answer. The provider ingress keeps
// no pend, continuation or authorization-number state from an answer (those
// belong to this gateway's own originator flows), so the outcome is the only
// thing the answer would have been read for. Metadata only: a log line and an
// observer event, no payload.
func (g *Gateway) localWriteSkipped(leg, correlationID, rule string) {
	detail := "answer not read for the local record (" + rule + "); leg outcome recorded as answered"
	log.Printf("gateway: %s: leg %s correlation %s: %s", LocalWriteSkippedEvent, leg, correlationID, detail)
	g.observe(ObserverEvent{
		Kind: LocalWriteSkippedEvent, Direction: "originate", LegType: leg,
		CorrelationID: correlationID, Detail: detail,
	})
}
