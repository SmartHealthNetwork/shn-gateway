// dtr_adaptive.go — the provider-side drive of an SDC ADAPTIVE questionnaire
// (sdc-questionnaire-questionnaireAdaptive): $questionnaire-package delivers the first
// group(s); every later, enableWhen-gated group is handed out by the payer's
// $next-question as answers accumulate. A client that never calls $next-question holds a
// PREFIX of the questionnaire, and every answer it has for an undelivered group is
// silently dropped by a tree-driven fill — the Da Vinci reference payer's
// HomeHealthAssessment delivers group 1 (1.1) alone, so the provider-data lane's attested
// 3.1 never reached the wire until this drive existed.
//
// attestAdaptiveQuestionnaire is the one attestation site for both the single-shot
// (UC-04) and the pend-then-amend (UC-06/UC-07) provider-data lanes: fill what is
// delivered, ask the payer for the next groups those answers unlock, repeat until a round
// delivers nothing new, then fill the complete tree. The round trip rides the EXISTING
// dtr-questionnaire-fetch substrate leg (holder ↔ Hub ↔ holder, captured, audited, per-leg
// authority) with the gateway-internal dtrLegRequest widened by a NextQuestion field — the
// same precedent the Order field set: no new leg type, no new authority op, nothing the
// published SDK has to know about. A non-adaptive questionnaire takes the unchanged
// single-fill path, byte-identical.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// sdcQuestionnaireAdaptiveExt marks an SDC adaptive questionnaire (the extension's presence
// is the declaration; the same url the SDK's amend placement keys on).
const sdcQuestionnaireAdaptiveExt = "http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-questionnaire-questionnaireAdaptive"

// nextQuestionContainedID is the id of the contained Questionnaire a $next-question request
// carries — the delivered-so-far tree, derivedFrom the source canonical (the SDC adaptive
// shape; the Da Vinci reference payer's own shell uses the same id).
const nextQuestionContainedID = "contained-questionnaire"

// maxNextQuestionRounds bounds the adaptive drive: a payer that keeps delivering new groups
// past this many rounds is refused (502), never looped on. The reference HomeHealthAssessment
// converges in two rounds (group 3 delivered, then the empty completing round).
const maxNextQuestionRounds = 8

// questionnaireIsAdaptive reports whether the bare Questionnaire carries the SDC
// questionnaireAdaptive extension.
func questionnaireIsAdaptive(questionnaireJSON []byte) bool {
	var probe struct {
		Extension []struct {
			URL string `json:"url"`
		} `json:"extension"`
	}
	if err := json.Unmarshal(questionnaireJSON, &probe); err != nil {
		return false
	}
	for _, e := range probe.Extension {
		if e.URL == sdcQuestionnaireAdaptiveExt {
			return true
		}
	}
	return false
}

// attestAdaptiveQuestionnaire builds the attested QuestionnaireResponse for res's fetched
// questionnaire from answers (author = the attesting Organization) at the DTR line the
// package was fetched at, driving $next-question first when the questionnaire is adaptive.
// It returns the attested QR AND the questionnaire tree it was filled against (the
// delivered tree, grown by every $next-question round) — the tree a later amendment must
// place its item by. On failure status/msg are set (the caller writes them) and err, when
// non-nil, is the RAW origination error for relayOriginationError (the submitClaimAndResolve
// convention).
func (g *Gateway) attestAdaptiveQuestionnaire(ctx context.Context, r *http.Request, res crdDtrResult, answers map[string]shnsdk.Answer, author string, qc shnsdk.QRContext) (qrJSON, questionnaireJSON []byte, status int, msg string, err error) {
	tree := res.questionnaireJSON
	if !questionnaireIsAdaptive(tree) {
		qr, ferr := shnsdk.FillQuestionnaireFromAnswersAtLine(res.dtrLine, tree, answers, author, qc)
		if ferr != nil {
			return nil, nil, http.StatusInternalServerError, "attest questionnaire failed", nil
		}
		return qr, tree, 0, "", nil
	}
	canonical, perr := shnsdk.ParseQuestionnaireURL(tree)
	if perr != nil {
		return nil, nil, http.StatusBadGateway, "fetched questionnaire url parse failed", nil
	}
	for round := 0; ; round++ {
		if round == maxNextQuestionRounds {
			return nil, nil, http.StatusBadGateway, fmt.Sprintf("adaptive questionnaire did not converge after %d $next-question rounds", maxNextQuestionRounds), nil
		}
		// Fill the tree delivered SO FAR. Every required item the payer has delivered must be
		// answerable from the attestation — a required item the provider's record cannot
		// source is a data fault of the seeded order (422), never a fabricated answer.
		partial, ferr := shnsdk.FillQuestionnaireFromAnswersAtLine(res.dtrLine, tree, answers, author, qc)
		if ferr != nil {
			return nil, nil, http.StatusUnprocessableEntity, "attestation cannot answer the delivered questionnaire: " + ferr.Error(), nil
		}
		reqQR, berr := buildNextQuestionQR(partial, tree, canonical)
		if berr != nil {
			return nil, nil, http.StatusInternalServerError, "build next-question request failed", nil
		}
		delivered, status, msg, lerr := g.nextQuestionLeg(ctx, r, res, canonical, reqQR)
		if status != 0 {
			return nil, nil, status, msg, lerr
		}
		merged, grew, merr := mergeDeliveredGroups(tree, delivered)
		if merr != nil {
			return nil, nil, http.StatusBadGateway, merr.Error(), nil
		}
		if !grew {
			// The completing round: nothing new unlocked — `partial` IS the fill of the complete
			// delivered tree.
			return partial, tree, 0, "", nil
		}
		tree = merged
	}
}

// nextQuestionLeg originates one $next-question round on the dtr-questionnaire-fetch leg:
// the same select-before-build / egressAdapt / OriginateLeg / ingress-$validate pipeline
// the package fetch runs, PINNED to the line the package was fetched at (an adaptive round
// that re-negotiated onto another line would fill at one line and be amended at another).
// It returns the contained Questionnaire's top-level items the payer answered with, after
// two fences: the answer must be a questionnaire-response Parameters (a package, a bare
// Bundle, anything else is refused — a payer that ignores the op must not be mistaken for
// one that answered it) and its QuestionnaireResponse must be about THIS patient.
func (g *Gateway) nextQuestionLeg(ctx context.Context, r *http.Request, res crdDtrResult, canonical string, reqQR []byte) (delivered []json.RawMessage, status int, msg string, err error) {
	corr := g.cfg.CorrelationGen()
	route, rerr := g.selectLegLine(res.recipient, "dtr-questionnaire-fetch", corr)
	if rerr != nil {
		return nil, http.StatusBadGateway, rerr.Error(), rerr
	}
	if line := shnsdk.LineOf(route.Token); line != res.dtrLine {
		return nil, http.StatusBadGateway, fmt.Sprintf("next-question routed at DTR line %q, the package was fetched at %q", line, res.dtrLine), nil
	}
	reqBytes, merr := json.Marshal(dtrLegRequest{Canonical: canonical, NextQuestion: reqQR})
	if merr != nil {
		return nil, http.StatusInternalServerError, "build next-question leg failed", nil
	}
	x := ExchangeIdentity{CorrelationID: corr, LegType: "dtr-questionnaire-fetch", Counterpart: res.recipient}
	adapted, _, aerr := g.egressAdapt(route, reqBytes, x)
	if aerr != nil {
		return nil, http.StatusBadGateway, aerr.Error(), aerr
	}
	body, oerr := g.OriginateLeg(ctx, r, res.recipient, "dtr-questionnaire-fetch", res.pci, corr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Bytes: adapted})
	if oerr != nil {
		return nil, http.StatusBadGateway, oerr.Error(), oerr
	}
	if vstatus, vmsg := g.validateFHIR(ctx, body, "ingress", res.dtrLine); vstatus != 0 {
		return nil, vstatus, vmsg, nil
	}
	qr, items, perr := parseNextQuestionResponse(body)
	if perr != nil {
		return nil, http.StatusBadGateway, "next-question response is not a questionnaire-response: " + perr.Error(), nil
	}
	if subj, serr := questionnaireResponseSubject(qr); serr != nil || subj != res.patientRef {
		return nil, http.StatusBadGateway, "next-question response subject does not match patient", nil
	}
	return items, 0, "", nil
}

// buildNextQuestionQR shapes the $next-question request body: the partially filled
// QuestionnaireResponse, in progress, whose `questionnaire` points at a CONTAINED copy of
// the delivered-so-far tree — derivedFrom the source canonical, its own url distinct from
// the source (the SDC adaptive shape the reference payer both emits and expects: it reads
// the contained Questionnaire's derivedFrom to resolve the source and its top-level items
// as the delivered set). JSON-level so every field of the delivered tree survives.
func buildNextQuestionQR(partialQR, tree []byte, canonical string) ([]byte, error) {
	var qr map[string]json.RawMessage
	if err := json.Unmarshal(partialQR, &qr); err != nil {
		return nil, err
	}
	var contained map[string]json.RawMessage
	if err := json.Unmarshal(tree, &contained); err != nil {
		return nil, err
	}
	contained["id"] = json.RawMessage(`"` + nextQuestionContainedID + `"`)
	urlRaw, err := json.Marshal(canonical + "-adaptive")
	if err != nil {
		return nil, err
	}
	contained["url"] = urlRaw
	derived, err := json.Marshal([]string{canonical})
	if err != nil {
		return nil, err
	}
	contained["derivedFrom"] = derived
	containedRaw, err := json.Marshal([]map[string]json.RawMessage{contained})
	if err != nil {
		return nil, err
	}
	qr["contained"] = containedRaw
	qr["status"] = json.RawMessage(`"in-progress"`)
	qr["questionnaire"] = json.RawMessage(`"#` + nextQuestionContainedID + `"`)
	return json.Marshal(qr)
}

// parseNextQuestionResponse reads a $next-question answer: a Parameters whose
// questionnaire-response parameter carries the QuestionnaireResponse, whose contained
// Questionnaire holds the delivered tree. Anything else — a package Bundle, a Parameters
// without the parameter, a response with no contained Questionnaire — is an error.
func parseNextQuestionResponse(body []byte) (qrJSON []byte, deliveredItems []json.RawMessage, err error) {
	var top struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if top.ResourceType != "Parameters" {
		return nil, nil, fmt.Errorf("resourceType %q, want Parameters", top.ResourceType)
	}
	for _, p := range top.Parameter {
		if p.Name != "questionnaire-response" {
			continue
		}
		var qr struct {
			ResourceType string            `json:"resourceType"`
			Contained    []json.RawMessage `json:"contained"`
		}
		if err := json.Unmarshal(p.Resource, &qr); err != nil || qr.ResourceType != "QuestionnaireResponse" {
			return nil, nil, fmt.Errorf("questionnaire-response parameter is not a QuestionnaireResponse")
		}
		for _, c := range qr.Contained {
			var q struct {
				ResourceType string            `json:"resourceType"`
				Item         []json.RawMessage `json:"item"`
			}
			if err := json.Unmarshal(c, &q); err == nil && q.ResourceType == "Questionnaire" {
				return p.Resource, q.Item, nil
			}
		}
		return nil, nil, fmt.Errorf("questionnaire-response carries no contained Questionnaire")
	}
	return nil, nil, fmt.Errorf("no questionnaire-response parameter")
}

// mergeDeliveredGroups grows the delivered tree by the top-level groups the payer's answer
// added: every group the tree already holds keeps ITS bytes (the payer re-sends copies of
// delivered groups; the client's own copy is the one it filled), new groups are appended in
// the payer's order. grew reports whether anything new arrived. A payer answer that DROPS
// a delivered group is refused — the delivered set only ever grows.
func mergeDeliveredGroups(tree []byte, delivered []json.RawMessage) (merged []byte, grew bool, err error) {
	var q map[string]json.RawMessage
	if err := json.Unmarshal(tree, &q); err != nil {
		return nil, false, fmt.Errorf("parse delivered questionnaire: %w", err)
	}
	var have []json.RawMessage
	if raw, ok := q["item"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &have); err != nil {
			return nil, false, fmt.Errorf("parse delivered items: %w", err)
		}
	}
	// Keyed on the top-level linkId. An item with no linkId keys as "" — two such groups would
	// collide silently; unreachable today (a Questionnaire item's linkId is 1..1 and the
	// delivered tree is the payer's copy of the source), noted so a later widening sees it.
	linkOf := func(raw json.RawMessage) string {
		var p struct {
			LinkID string `json:"linkId"`
		}
		_ = json.Unmarshal(raw, &p)
		return p.LinkID
	}
	got := map[string]bool{}
	for _, it := range delivered {
		got[linkOf(it)] = true
	}
	known := map[string]bool{}
	for _, it := range have {
		l := linkOf(it)
		if !got[l] {
			return nil, false, fmt.Errorf("next-question response dropped delivered group %q", l)
		}
		known[l] = true
	}
	items := append([]json.RawMessage{}, have...)
	for _, it := range delivered {
		if l := linkOf(it); !known[l] {
			items = append(items, it)
			known[l] = true
			grew = true
		}
	}
	if !grew {
		return tree, false, nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, false, err
	}
	q["item"] = raw
	merged, err = json.Marshal(q)
	return merged, true, err
}

// nextQuestionRequestSubject reads the dtr-questionnaire-fetch leg's NextQuestion carriage:
// ok=false when the request is an ordinary package fetch; otherwise the carried
// QuestionnaireResponse's subject (the (A)-bind input on the payer side — "" when absent,
// which the bind refuses).
func nextQuestionRequestSubject(reqJSON []byte) (subject string, ok bool) {
	var fetch dtrLegRequest
	if err := json.Unmarshal(reqJSON, &fetch); err != nil || len(fetch.NextQuestion) == 0 {
		return "", false
	}
	subject, _ = questionnaireResponseSubject(fetch.NextQuestion)
	return subject, true
}

// buildNextQuestionParameters wraps the carried QuestionnaireResponse as the Da Vinci DTR
// $next-question input Parameters (questionnaire-response, 1..1).
func buildNextQuestionParameters(qrJSON json.RawMessage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"resourceType": "Parameters",
		"parameter":    []map[string]any{{"name": "questionnaire-response", "resource": qrJSON}},
	})
}
