// originate.go — the provider-side scenario drivers (UC-01…08): originate a PA
// exchange, run it through the Hub, and surface the result. Part of package gateway
// (the Smart Gateway runs every holder role; this file is the provider-origination
// surface). Behavior-preserving split of gateway.go (finding C); no logic change.
// See gateway.go for the package doc.
package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// FilledItem is the gateway-engine-LOCAL attribution surface for a DTR auto-filled
// QR item (console response, QRItems field). The SDK's FillQuestionnaire drops the
// FilledItem summary (UI-only); the gateway reconstructs it via fillSummary
// from the ClinicalContext. JSON tags are byte-for-byte compatible with the original
// dtr.FilledItem shape so the console response format is unchanged.
type FilledItem struct {
	LinkID    string `json:"linkId"`
	Answer    string `json:"answer"`
	Origin    string `json:"origin"`
	SourceRef string `json:"sourceRef,omitempty"`
}

// fillSummary reconstructs the []FilledItem summary from the ClinicalContext —
// the same items AutoFill would have populated, in the same order. Called after
// shnsdk.FillQuestionnaire (which drops FilledItem) to preserve the console surface.
// Items with a negative/absent flag (prior-surgery=false, etc.) are omitted, matching
// AutoFill's behaviour. functional-status-oswestry is intentionally absent (no local source).
func fillSummary(cc shnsdk.ClinicalContext) []FilledItem {
	var out []FilledItem
	out = append(out, FilledItem{
		LinkID:    "conservative-therapy-weeks",
		Answer:    itoa(cc.ConservativeTherapyWeeks),
		Origin:    "auto",
		SourceRef: cc.ConservativeTherapyRef,
	})
	out = append(out, FilledItem{
		LinkID:    "neuro-deficit",
		Answer:    boolStr(cc.NeuroDeficit),
		Origin:    "auto",
		SourceRef: cc.NeuroDeficitRef,
	})
	out = append(out, FilledItem{
		LinkID:    "prior-imaging",
		Answer:    boolStr(cc.PriorImaging),
		Origin:    "auto",
		SourceRef: cc.PriorImagingRef,
	})
	if cc.PriorSurgery {
		out = append(out, FilledItem{
			LinkID:    "prior-surgery",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.PriorSurgeryRef,
		})
	}
	if cc.HighDisability {
		out = append(out, FilledItem{
			LinkID:    "high-disability",
			Answer:    "true",
			Origin:    "auto",
			SourceRef: cc.HighDisabilityRef,
		})
	}
	if cc.PatientReported {
		out = append(out, FilledItem{
			LinkID: "patient-reported-required",
			Answer: "true",
			Origin: "auto",
		})
	}
	return out
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---- Provider role ----

type scenarioReq struct {
	Branch string `json:"branch"`
}

type scenarioResp struct {
	Covered bool   `json:"covered"`
	Reason  string `json:"reason"`
}

func (g *Gateway) handleScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req scenarioReq
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &req); err != nil {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		}
		return
	}

	// A participant-facing gateway rejects unknown branches rather than silently
	// treating anything non-"covered" as not-covered.
	var memberID string
	switch req.Branch {
	case "covered":
		memberID = "MBR-COVERED"
	case "notcovered":
		memberID = "MBR-NOTCOVERED"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}

	pci, _, found := g.cfg.SoR.ResolvePatient(memberID)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}

	cerJSON, err := shnsdk.BuildEligibilityRequest(memberID, g.cfg.NPI, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build request failed"})
		return
	}

	// Egress validation is load-bearing: an invalid resource must never reach the
	// substrate. Empty profile = base-R4 + meta.profile pinning (see roundTrip).
	res, err := g.cfg.Validator.Validate(ctx, cerJSON, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "validator unavailable"})
		return
	}
	if !res.Valid {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "egress validation failed",
			"issues": res.Issues,
		})
		return
	}

	// Generate the correlationID BEFORE authorizing so the token is bound to the
	// exact envelope it will ride in (C2): token.CorrelationID == envelope CID.
	correlationID := g.cfg.CorrelationGen()

	// UC-01 uses the SAME authorized-sealed-leg helper as UC-02/03 (OriginateLeg →
	// roundTrip): authorize(eligibility-inquiry) → seal → Hub /route → verify the response leg
	// (respOp eligibility-response, bound to this correlationID, Sender=="payer",
	// subject==pci) → decrypt. Folding UC-01 onto the shared helper keeps the
	// trust-critical response-leg verification in ONE place — no duplicated copy to
	// drift.
	crrJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "coverage-eligibility", pci, correlationID, "", Content{WorkstreamType: workstreamPA, Bytes: cerJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Ingress-validate the decrypted response (load-bearing). A payer returning an
	// invalid CRR is an UPSTREAM failure → 502 (preserves the UC-01 contract; only
	// the response-leg token verification was folded into roundTrip (via OriginateLeg),
	// not the validation-status semantics).
	ingress, err := g.cfg.Validator.Validate(ctx, crrJSON, "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "validator unavailable"})
		return
	}
	if !ingress.Valid {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "ingress validation failed"})
		return
	}
	covered, reason, err := shnsdk.ParseEligibilityResponse(crrJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "response parse failed"})
		return
	}

	writeJSON(w, http.StatusOK, scenarioResp{Covered: covered, Reason: reason})
}

type uc02Resp struct {
	PARequired  bool   `json:"paRequired"`
	CardSummary string `json:"cardSummary"`
}

type uc03Resp struct {
	PARequired  bool         `json:"paRequired"`
	AuthNumber  string       `json:"authNumber"`
	ValidUntil  string       `json:"validUntil"`
	QRItems     []FilledItem `json:"qrItems"`
	PendedItems []string     `json:"pendedItems,omitempty"`
}

// crdDtrResult carries the outputs of the CRD+DTR prefix shared by UC-03/04/06.
type crdDtrResult struct {
	qrJSON, srJSON          []byte
	patientRef, coverageRef string
	pci                     string
	filled                  []FilledItem
}

// runCRDThenDTR executes the shared CRD order-select + DTR fetch + local auto-fill
// for a covered member: it resolves the patient, builds+validates the SR and
// Coverage, runs the CRD round-trip (must come back PA-required with a canonical),
// fetches+validates the Questionnaire, verifies its url matches the advertised
// canonical, and auto-fills the QR from local clinical context. On any failure it
// writes the HTTP error and returns ok=false. Shared by UC-03/04/06 (DRY).
func (g *Gateway) runCRDThenDTR(w http.ResponseWriter, r *http.Request, member string) (crdDtrResult, bool) {
	ctx := r.Context()

	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return crdDtrResult{}, false
	}
	patientRef := "Patient/" + member
	coverageRef := "Coverage/" + member

	srJSON, err := shnsdk.BuildServiceRequest("72148", "MRI lumbar spine w/o contrast", "M51.16", patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build request failed"})
		return crdDtrResult{}, false
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	coverageJSON, err := shnsdk.BuildCoverage(patientRef, coverageRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build coverage failed"})
		return crdDtrResult{}, false
	}
	if status, msg := g.validateFHIR(ctx, coverageJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	// --- CRD round-trip: must come back PA-required with a canonical. ---
	crdReq, err := shnsdk.BuildConformantOrderSelectRequest(srJSON, coverageJSON, patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build order-select failed"})
		return crdDtrResult{}, false
	}
	crdRespJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "crd-order-select", pci, g.cfg.CorrelationGen(), "", Content{WorkstreamType: workstreamPA, Bytes: crdReq})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	cov, err := shnsdk.ParseCards(crdRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return crdDtrResult{}, false
	}
	// Per-value switch on the CRD card result (FR-G25). Every non-happy-path value
	// is handled fail-closed with no silent fall-through. AI-1: a coverage denial
	// STOPS, never proceeds silently.
	switch {
	case cov.Covered == shnsdk.CoveredNotCovered:
		// Explicit terminal stop — a coverage denial STOPS, never silently "proceeds".
		// Patient-facing denial UX is deferred.
		writeJSON(w, http.StatusOK, map[string]any{"paRequired": false, "covered": false, "outcome": "not-covered"})
		return crdDtrResult{}, false
	case cov.PANeeded == shnsdk.PANeededSatisfied:
		// TYPE-ready (SatisfiedPaID carried); the proceed-with-existing-auth
		// short-circuit is a new terminal-success path, deferred fail-closed this
		// slice. Distinct message — a real conformant payer is most likely to hit
		// this branch.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "PA already satisfied — short-circuit not yet implemented"})
		return crdDtrResult{}, false
	case cov.PANeeded == shnsdk.PANeededConditional || cov.Covered == shnsdk.CoveredConditional:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "conditional coverage unsupported this slice"})
		return crdDtrResult{}, false
	case !cov.NeedsDTR() || !cov.PARequired():
		// This path (UC-03+) requires BOTH a questionnaire (doc-needed axis) and
		// PA (pa-needed axis).
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected PA-required card with questionnaire"})
		return crdDtrResult{}, false
	}
	canonical := shnsdk.StripCanonicalVersion(cov.Questionnaires[0])

	// --- DTR round-trip: fetch Questionnaire, validate, auto-fill locally. ---
	dtrReq, err := json.Marshal(shnsdk.QuestionnaireFetchRequest{Canonical: canonical})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build dtr request failed"})
		return crdDtrResult{}, false
	}
	packageJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "dtr-questionnaire-fetch", pci, g.cfg.CorrelationGen(), "", Content{WorkstreamType: workstreamPA, Bytes: dtrReq})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	if status, msg := g.validateFHIR(ctx, packageJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	// The DTR-fetch leg carries the full $questionnaire-package collection
	// Bundle (its dependent Libraries/ValueSets survive the wire for Step 3, which
	// will read them from packageJSON here). Extract the bare Questionnaire for the
	// F5 canonical check + auto-fill. A package with no Questionnaire is a partner
	// fault → 502 (the guard relocated from native.go's producer-side extract).
	questionnaireJSON, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire package has no Questionnaire"})
		return crdDtrResult{}, false
	}

	// F5: verify the fetched Questionnaire's url matches the canonical the payer
	// advertised in the CRD card. A mismatch means the payer returned a different
	// questionnaire than the card claimed — reject to prevent canonical substitution.
	fetchedURL, err := shnsdk.ParseQuestionnaireURL(questionnaireJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire url parse failed"})
		return crdDtrResult{}, false
	}
	if fetchedURL != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetched questionnaire does not match advertised canonical"})
		return crdDtrResult{}, false
	}

	// The operated $populate engine reads the FHIR store directly, so its subject must be the
	// store-resolvable Patient ref (a scoped id), NOT the logical SHN ref. Resolve it via the SoR
	// (the stub returns the logical ref, so the managed/hermetic path is unchanged). Falls back to
	// the logical ref when the SoR can't resolve it.
	subjectFHIRRef := patientRef
	if ref, ok := g.cfg.SoR.PatientFHIRRef(member); ok && ref != "" {
		subjectFHIRRef = ref
	}
	qrJSON, fill, err := g.cfg.Populator.Populate(ctx, packageJSON, PopulateContext{
		Member:         member,
		PatientRef:     patientRef,
		SubjectFHIRRef: subjectFHIRRef,
		CoverageRef:    coverageRef,
		OrderRef:       "ServiceRequest/sr-" + member,
		Authored:       g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, statusForPopulateErr(err), map[string]string{"error": err.Error()})
		return crdDtrResult{}, false
	}
	// QR-SUBJECT FENCE — uniform across backends, compared against the LOGICAL PatientRef. Managed
	// fills with PatientRef directly. The native backend reads the FHIR store by the (possibly
	// scoped) SubjectFHIRRef, verifies the returned QR is about THAT patient, and normalizes
	// QR.subject → PatientRef before returning — so by here both backends present the logical ref.
	// (Comparing against the scoped SubjectFHIRRef here would wrongly reject the managed+real-SoR
	// combination, where managed sets the logical ref but the SoR resolves a scoped id.) 502.
	if subj, serr := questionnaireResponseSubject(qrJSON); serr != nil || subj != patientRef {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR subject does not match patient"})
		return crdDtrResult{}, false
	}
	// QR-QUESTIONNAIRE FENCE — uniform across backends. F5 (above) checks the FETCHED
	// Questionnaire's url before the seam; it never sees the returned QR's self-declared
	// `questionnaire`. A native engine can return a QR for a DIFFERENT questionnaire. Reject
	// any QR whose questionnaire (url-part) ≠ canonical. 502.
	if qq, qerr := questionnaireResponseCanonical(qrJSON); qerr != nil || qq != canonical {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "populated QR questionnaire does not match canonical"})
		return crdDtrResult{}, false
	}

	if status, msg := g.validateFHIR(ctx, qrJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return crdDtrResult{}, false
	}

	return crdDtrResult{
		qrJSON:      qrJSON,
		srJSON:      srJSON,
		patientRef:  patientRef,
		coverageRef: coverageRef,
		pci:         pci,
		filled:      fill,
	}, true
}

// statusForPopulateErr maps a Populator error to an HTTP status. A managed
// FillQuestionnaire marshal/unsupported error is the gateway's own fault → 500
// (behavior-preserving: the inline path returned 500 here, and this never trips on the
// 8 sandbox scenarios). errNoClinicalContext (a data fault) and errPopulateUpstream
// (a native $populate fault) are partner/data faults → 502.
func statusForPopulateErr(err error) int {
	switch {
	case errors.Is(err, errNoClinicalContext):
		return http.StatusBadGateway
	case errors.Is(err, errPopulateUpstream):
		return http.StatusBadGateway
	case errors.Is(err, errPopulateForeignSubject):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// questionnaireResponseSubject returns the QR's subject.reference.
func questionnaireResponseSubject(qrJSON []byte) (string, error) {
	var probe struct {
		Subject struct {
			Reference string `json:"reference"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(qrJSON, &probe); err != nil {
		return "", err
	}
	return probe.Subject.Reference, nil
}

// setQuestionnaireResponseSubject rewrites the QR's subject.reference (JSON-level, preserving every
// other field/order). Used to normalize the operated engine's store-resolvable subject (a scoped
// FHIR id) back to the logical SHN ref after the QR-subject fence has verified it. On a parse
// failure it returns the input unchanged (the egress validate that follows then rejects it).
func setQuestionnaireResponseSubject(qrJSON []byte, ref string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(qrJSON, &m); err != nil {
		return qrJSON
	}
	subj, err := json.Marshal(map[string]string{"reference": ref})
	if err != nil {
		return qrJSON
	}
	m["subject"] = subj
	out, err := json.Marshal(m)
	if err != nil {
		return qrJSON
	}
	return out
}

// questionnaireResponseCanonical returns the URL-PART of the QR's `questionnaire`
// canonical (version stripped). The managed QR sets a VERSIONED canonical
// (e.g. ".../pa-lumbar-mri|1.0.0") while the F5 `canonical` is the bare url — so the
// fence compares url-parts, not the raw versioned string.
func questionnaireResponseCanonical(qrJSON []byte) (string, error) {
	var probe struct {
		Questionnaire string `json:"questionnaire"`
	}
	if err := json.Unmarshal(qrJSON, &probe); err != nil {
		return "", err
	}
	if i := strings.IndexByte(probe.Questionnaire, '|'); i >= 0 {
		return probe.Questionnaire[:i], nil
	}
	return probe.Questionnaire, nil
}

// handleUC02 runs the no-PA CRD round-trip: a covered member's X-ray order is
// CRD-checked and comes back with an info card → paRequired=false.
func (g *Gateway) handleUC02(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pci, _, found := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown member"})
		return
	}
	patientRef := "Patient/MBR-COVERED"

	srJSON, err := shnsdk.BuildServiceRequest("72100", "X-ray lumbar spine", "M51.16", patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build request failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, srJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	coverageJSON, err := shnsdk.BuildCoverage(patientRef, "Coverage/MBR-COVERED")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build coverage failed"})
		return
	}
	// Every FHIR resource crossing the substrate is validated — the Coverage in
	// the CRD prefetch included.
	if status, msg := g.validateFHIR(ctx, coverageJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	reqJSON, err := shnsdk.BuildConformantOrderSelectRequest(srJSON, coverageJSON, patientRef)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build order-select failed"})
		return
	}

	correlationID := g.cfg.CorrelationGen()
	respJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "crd-order-select", pci, correlationID, "", Content{WorkstreamType: workstreamPA, Bytes: reqJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	cov, err := shnsdk.ParseCards(respJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "card parse failed"})
		return
	}

	// The SDK's ParseCards drops the card Summary; do a small inline parse of the
	// raw cards JSON to extract the first card's summary for the console response.
	var rawCards struct {
		Cards []struct {
			Summary string `json:"summary"`
		} `json:"cards"`
	}
	cardSummary := ""
	if json.Unmarshal(respJSON, &rawCards) == nil && len(rawCards.Cards) > 0 {
		cardSummary = rawCards.Cards[0].Summary
	}

	writeJSON(w, http.StatusOK, uc02Resp{
		PARequired:  cov.PARequired(),
		CardSummary: cardSummary,
	})
}

// handleUC03 runs the full PA-required path: CRD (must require PA) → DTR fetch +
// local auto-fill → PAS submit → approval. On approval the provider stores the
// auth number for the SR (FR-23) and answers paRequired=true.
func (g *Gateway) handleUC03(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const srRef = "ServiceRequest/sr-uc03"

	res, ok := g.runCRDThenDTR(w, r, "MBR-COVERED")
	if !ok {
		return
	}

	// --- PAS round-trip: submit the preauth bundle, expect an approval. ---
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Corr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	claimRespJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim", res.pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, claimRespJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsed, err := shnsdk.ParseClaimResponse(claimRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsed.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved"})
		return
	}

	// FR-23: persist the payer-issued auth number against the SR reference.
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsed.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}

	writeJSON(w, http.StatusOK, uc03Resp{
		PARequired: true,
		AuthNumber: parsed.PreAuthRef,
		ValidUntil: parsed.ValidUntil,
		QRItems:    res.filled,
	})
}

// handleUC04 runs the pended-then-approved PA path: CRD+DTR (same prefix as
// UC-03) → PAS submit → PENDED (no operative DiagnosticReport yet) → ClaimUpdate
// with the provider-LOCAL operative report + Provenance → approved (FR-20/21).
func (g *Gateway) handleUC04(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const srRef = "ServiceRequest/sr-uc04"

	res, ok := g.runCRDThenDTR(w, r, "MBR-UC04")
	if !ok {
		return
	}

	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Corr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim", res.pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, pendedResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "parse pended response failed"})
		return
	}
	if !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return
	}
	// Map []NeededItem → []string using .Code (the Task.input valueString, matching
	// what the internal ParsePendedOrApproved returned as a plain []string).
	needed := neededItemCodes(neededItems)

	// Amend: attach the provider-LOCAL operative DiagnosticReport + Provenance.
	drJSON, drOK := g.cfg.SoR.SupplementalReport("MBR-UC04")
	if !drOK {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no supplemental report"})
		return
	}
	if status, msg := g.validateFHIR(ctx, drJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	drRef, ok := resourceRef(drJSON)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "supplemental report missing id"})
		return
	}
	provJSON, err := shnsdk.BuildProvenance(drRef, "Organization/"+g.cfg.HolderID, g.cfg.Clock())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build provenance failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, provJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateCorr := g.cfg.CorrelationGen()
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// ClaimUpdate exchange — expect APPROVED.
	updateResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim-update", res.pci, updateCorr, "", Content{WorkstreamType: workstreamPA, Bytes: updateBundle})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsedUpd, err := shnsdk.ParseClaimResponse(updateResp)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsedUpd.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after amendment"})
		return
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsedUpd.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}
	writeJSON(w, http.StatusOK, uc03Resp{PARequired: true, AuthNumber: parsedUpd.PreAuthRef, ValidUntil: parsedUpd.ValidUntil, QRItems: res.filled, PendedItems: needed})
}

// uc05Resp is the UC-05 result. ConsentDenied/Pended mark the negative branch
// (federated query refused, PA stays pended); the positive branch carries the
// approval + the facility the evidence came from (source attribution).
type uc05Resp struct {
	PARequired    bool         `json:"paRequired"`
	AuthNumber    string       `json:"authNumber,omitempty"`
	ValidUntil    string       `json:"validUntil,omitempty"`
	QRItems       []FilledItem `json:"qrItems,omitempty"`
	PendedItems   []string     `json:"pendedItems,omitempty"`
	FacilityID    string       `json:"facilityId,omitempty"`
	Pended        bool         `json:"pended,omitempty"`
	ConsentDenied bool         `json:"consentDenied,omitempty"`
}

// handleUC05 runs the federated EXTERNAL-retrieval PA path (the non-aggregation
// showcase): CRD+DTR → PAS submit → PENDED (operative report not local) →
// consent-gated federated query to the external facility (provider→Hub→facility),
// which returns ONLY the named operative DiagnosticReport + a source Provenance
// citing the consent → ClaimUpdate with those → APPROVED. Branch "noconsent" uses
// Linda's no-consent twin: the federated query is denied and the PA stays pended.
func (g *Gateway) handleUC05(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Branch string `json:"branch"`
	}
	// The body selects the branch (default consent when absent). An EMPTY body is
	// allowed (io.EOF ⇒ default), but a MALFORMED body is REJECTED rather than
	// silently falling through to the happy path — a caller sending bad JSON gets a
	// clear 400, not an unintended consented run.
	if tooLarge, err := shnsdk.DecodeJSONBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		if tooLarge {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		}
		return
	}
	// Reject unknown branch values (defense-in-depth, mirroring UC-01): only the
	// empty default, "consent", and "noconsent" are valid. An unrecognized branch
	// must NOT silently run the consented path.
	switch req.Branch {
	case "", "consent", "noconsent":
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown branch"})
		return
	}
	member := "MBR-UC05"
	srRef := "ServiceRequest/sr-uc05"
	if req.Branch == "noconsent" {
		member = "MBR-UC05-NOCONSENT"
		srRef = "ServiceRequest/sr-uc05-noconsent"
	}

	res, ok := g.runCRDThenDTR(w, r, member)
	if !ok {
		return
	}

	// PAS submit — expect PENDED (no operative DiagnosticReport yet).
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Corr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pendedResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim", res.pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, pendedResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	pended, neededItems, err := shnsdk.ParsePendedResponse(pendedResp)
	if err != nil || !pended {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected pended response"})
		return
	}
	needed := neededItemCodes(neededItems)

	// --- Federated query to the external facility (consent-gated). ---
	facility, fok := g.cfg.Reg.LookupByRole("facility")
	if !fok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no facility registered"})
		return
	}
	queryJSON, err := shnsdk.BuildQuery(res.patientRef, []string{"DiagnosticReport", "DocumentReference"},
		"2024-01-01", g.cfg.Clock().UTC().Format("2006-01-02"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build query failed"})
		return
	}
	fqCorr := g.cfg.CorrelationGen()
	// custodian = the facility id, so the Authorization Framework checks consent for
	// THIS source. No consent (noconsent branch) → authorize fails → PA stays pended.
	recordsJSON, err := g.OriginateLeg(ctx, r, facility.ID, "federated-query", res.pci, fqCorr, facility.ID, Content{WorkstreamType: workstreamPA, Bytes: queryJSON})
	if err != nil {
		// Distinguish a genuine consent DENIAL from an infrastructure/integrity
		// failure. ONLY an authorization denial (the no-consent branch) leaves the PA
		// validly pended; a facility outage, a tampered response, or a transport error
		// is a real 502 and must NOT be misreported to the operator as "consent denied".
		if errors.Is(err, errAuthorizationDenied) {
			writeJSON(w, http.StatusOK, uc05Resp{PARequired: true, Pended: true, ConsentDenied: true, PendedItems: needed})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated query failed: " + err.Error()})
		return
	}
	// Ingress-validate the facility's searchset BEFORE trusting/extracting its
	// resources (defense in depth — every resource crossing the substrate is validated).
	if status, msg := g.validateFHIR(ctx, recordsJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	drJSON, provJSON, err := shnsdk.ExtractOperativeEvidence(recordsJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "federated response parse failed: " + err.Error()})
		return
	}

	// --- ClaimUpdate with the externally-retrieved DiagnosticReport + Provenance. ---
	updateCorr := g.cfg.CorrelationGen()
	updateBundle, err := shnsdk.BuildConformantClaimUpdateBundle(shnsdk.ConformantClaimUpdateInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Provenance: provJSON, DiagnosticReport: drJSON, Corr: updateCorr, OriginalCorr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build update bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateBundle, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	updateResp, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim-update", res.pci, updateCorr, "", Content{WorkstreamType: workstreamPA, Bytes: updateBundle})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, updateResp, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	parsedUpd, err := shnsdk.ParseClaimResponse(updateResp)
	if err != nil || parsedUpd.Outcome != "approved" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "preauthorization not approved after federated retrieval"})
		return
	}
	if err := g.cfg.Store.StoreAuthNumber(srRef, parsedUpd.PreAuthRef); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "holder write failed (auth number)"})
		return
	}
	writeJSON(w, http.StatusOK, uc05Resp{PARequired: true, AuthNumber: parsedUpd.PreAuthRef, ValidUntil: parsedUpd.ValidUntil, QRItems: res.filled, PendedItems: needed, FacilityID: facility.ID})
}

// uc08Resp is the provider-side result of the UC-08 denial scenario.
// PARequired is always true (a PA was needed); Denied is true when the payer
// issued a denial (no auth number). Rationale is the ClaimResponse disposition.
// PatientDenialReason is the CARC reason code from the PHG denial view (the
// patient surface reads the payer's PDex PA EOB; this field surfaces it for the
// operator console demo — the PHG call stands in for the patient app).
type uc08Resp struct {
	PARequired          bool   `json:"paRequired"`
	Denied              bool   `json:"denied"`
	AuthNumber          string `json:"authNumber,omitempty"`
	Rationale           string `json:"rationale,omitempty"`
	PatientDenialReason string `json:"patientDenialReason,omitempty"`
	// PatientAppeal is the appeal-window text the PHG read FROM the EOB.processNote
	// (FR-28: data-driven from the FHIR resource, not a UI string).
	PatientAppeal string `json:"patientAppeal,omitempty"`
}

// handleUC08 runs the PA-denied path (UC-08, FR-22): CRD+DTR → PAS submit →
// denied ClaimResponse (reviewAction A3, no preAuthRef) → the provider queries
// the PHG denial view (which reads the payer's PDex PA EOB) to obtain the
// patient-rendered reason (CARC 50). The denial is TERMINAL — it does NOT pend.
func (g *Gateway) handleUC08(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	res, ok := g.runCRDThenDTR(w, r, "MBR-UC08")
	if !ok {
		return
	}

	// PAS submit — expect DENIED (4 weeks conservative therapy < 6, no prior surgery,
	// not high-disability → Adjudicate returns Denied).
	pasCorr := g.cfg.CorrelationGen()
	bundleJSON, err := shnsdk.BuildConformantClaimBundle(shnsdk.ConformantClaimInputs{
		QR: res.qrJSON, SR: res.srJSON, PatientRef: res.patientRef, CoverageRef: res.coverageRef,
		Corr: pasCorr, Created: g.cfg.Clock(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "build bundle failed"})
		return
	}
	if status, msg := g.validateFHIR(ctx, bundleJSON, "egress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	claimRespJSON, err := g.OriginateLeg(ctx, r, g.cfg.CounterpartID, "pas-claim", res.pci, pasCorr, "", Content{WorkstreamType: workstreamPA, Bytes: bundleJSON})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status, msg := g.validateFHIR(ctx, claimRespJSON, "ingress"); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}

	// A denial is TERMINAL (does NOT pend) — use ParseClaimResponse directly and
	// expect Outcome == "denied". ParsePendedResponse is not used here.
	parsed, err := shnsdk.ParseClaimResponse(claimRespJSON)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "claim response parse failed"})
		return
	}
	if parsed.Outcome == "approved" {
		// Unexpected approval — surface the auth number for diagnostics.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "expected denial but got approval: " + parsed.PreAuthRef})
		return
	}

	// Extract the human-readable rationale from the denied ClaimResponse via
	// res.Denial.Rationale (replaces pas.ParseDeniedRationale).
	rationale := ""
	if parsed.Denial != nil {
		rationale = parsed.Denial.Rationale
	}

	// Demo orchestration ONLY: the provider scenario queries the PHG denial view so
	// the console can show the patient view in one click. This stands in for the
	// patient app (the patient would query the PHG directly). It is INTENTIONALLY
	// fail-open — a PHG hiccup must not fail the real denial decision, which already
	// succeeded on the substrate. The patient-surfacing requirement is proven
	// INDEPENDENTLY of this convenience path by TestUC08_PatientSurfacingDirect
	// (which queries the PHG directly and fails if surfacing is skipped), so this
	// fail-open cannot silently hide a broken patient surface.
	var patientDenialReason, patientAppeal string
	if g.cfg.PHGURL != "" {
		phgURL := g.cfg.PHGURL + "/denial?pci=" + res.pci
		phgReq, err2 := http.NewRequestWithContext(ctx, http.MethodGet, phgURL, nil)
		if err2 == nil {
			phgResp, err2 := g.cfg.Client.Do(phgReq)
			if err2 == nil {
				defer phgResp.Body.Close()
				var views []struct {
					ReasonCode string `json:"reasonCode"`
					Appeal     string `json:"appeal"`
				}
				if json.NewDecoder(io.LimitReader(phgResp.Body, shnsdk.MaxResponseBytes)).Decode(&views) == nil && len(views) > 0 {
					patientDenialReason = views[0].ReasonCode
					patientAppeal = views[0].Appeal
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, uc08Resp{
		PARequired:          true,
		Denied:              true,
		AuthNumber:          "",
		Rationale:           rationale,
		PatientAppeal:       patientAppeal,
		PatientDenialReason: patientDenialReason,
	})
}

// neededItemCodes maps []shnsdk.NeededItem → []string using .Code, matching what
// the internal ParsePendedOrApproved returned as a plain []string (Task.input
// valueString). The console/operator surface reads these as opaque codes.
func neededItemCodes(items []shnsdk.NeededItem) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Code
	}
	return out
}
