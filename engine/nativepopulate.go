package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// (errPopulateUpstream is defined in populator.go, alongside errNoClinicalContext.)

// nativePopulator forwards population to an SDC Questionnaire/$populate endpoint (the
// provider's own DTR/CQL server, or — later — one we operate). Holds *http.Client +
// a post helper (the nativeResponder precedent, native.go) so the loopback is a real
// httptest server, not a mock interface. A non-2xx from the endpoint — including a 401
// from the auth gate in front of it, or a token refusal surfaced by an authenticated
// client — is errPopulateUpstream: the populator never retries unauthenticated.
type nativePopulator struct {
	client   *http.Client
	url      string // PROVIDER_DTR_POPULATE_URL, the Questionnaire/$populate endpoint
	observer func(PopulateFailure)
}

// NewNativePopulator builds the pass-through backend. client is the populate client
// (SMART-authenticated when the PROVIDER_DTR_POPULATE_* block is set; else the
// substrate client).
func NewNativePopulator(client *http.Client, url string) *nativePopulator {
	return NewNativePopulatorWithFailureObserver(client, url, nil)
}

// NewNativePopulatorWithFailureObserver builds the pass-through backend with an
// optional synchronous failure observer. The observer receives one payload-free
// record per upstream failure, never a success or subject/canonical refusal. It
// must return promptly and support concurrent calls. A nil observer is disabled;
// the old constructor is equivalent to passing nil here.
func NewNativePopulatorWithFailureObserver(client *http.Client, url string, observer func(PopulateFailure)) *nativePopulator {
	return &nativePopulator{client: client, url: url, observer: observer}
}

func (n *nativePopulator) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	q, err := extractQuestionnaireFromPackage(packageJSON)
	if err != nil {
		return nil, nil, err // no-Questionnaire → consumer 502
	}
	params, err := buildPopulateParameters(q, pc)
	if err != nil {
		return nil, nil, err
	}
	body, status, failure := n.post(ctx, params)
	if failure != nil {
		n.observeFailure(*failure)
		return nil, nil, errPopulateUpstream
	}
	qr, err := extractQuestionnaireResponse(body)
	if err != nil {
		reason := populateReasonInvalidJSON
		if err == errPopulateUpstream {
			reason = populateReasonWrongResourceType
		}
		n.observeFailure(PopulateFailure{Stage: populateStageQRExtract, Reason: reason, Status: status})
		return nil, nil, errPopulateUpstream
	}
	// FOREIGN-SUBJECT FENCE (native owns it — it knows the store-resolvable ref it sent): the
	// engine MUST return a QR about the patient we asked to populate. A remote engine could return
	// a QR about a foreign patient (egress validate accepts a valid-but-wrong subject). Verify
	// against the SENT ref (SubjectFHIRRef, the possibly-scoped store id), THEN normalize the
	// verified subject → the logical PatientRef so the consumer fence + the downstream PAS bundle
	// (which reference PatientRef) stay consistent.
	expected := pc.SubjectFHIRRef
	if expected == "" {
		expected = pc.PatientRef
	}
	if subj, serr := questionnaireResponseSubject(qr); serr != nil || subj != expected {
		return nil, nil, errPopulateForeignSubject
	}
	qr = setQuestionnaireResponseSubject(qr, pc.PatientRef)
	qr, err = identifyCQLSoftwareAuthor(qr)
	if err != nil {
		return nil, nil, errPopulateUpstream
	}
	// fill summary nil — the REMOTE engine filled; the gateway has no per-item attribution.
	return qr, nil, nil
}

func (n *nativePopulator) post(ctx context.Context, body []byte) ([]byte, int, *PopulateFailure) {
	// Each Do owns fresh evidence, including when the callback is disabled. Never
	// reuse caller evidence: its prior acquisition may belong to another request.
	var acquisition *smartauth.TokenAcquisitionObservation
	if ctx != nil { // Preserve NewRequestWithContext's existing nil-context error.
		ctx, acquisition = smartauth.WithTokenAcquisitionObservation(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, populateBoundaryFailure(populateStageRequestBuild, 0, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		stage := populateStageTransport
		if smartauth.IsTokenAcquisitionError(err) || (acquisition != nil && acquisition.Failed()) {
			stage = populateStageTokenAcquisition
		}
		status := 0
		if resp != nil {
			status = populateObservedStatus(resp.StatusCode)
		}
		return nil, status, populateBoundaryFailure(stage, status, err)
	}
	defer resp.Body.Close()
	status := populateObservedStatus(resp.StatusCode)
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, status, populateBoundaryFailure(populateStageBodyRead, status, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, status, &PopulateFailure{Stage: populateStageHTTPStatus, Reason: populateReasonNon2xx, Status: status}
	}
	return rb, status, nil
}

// buildPopulateParameters builds the SDC $populate Parameters: the inline questionnaire + the
// `subject`. Subject alone is sufficient — the engine binds the CQL `context Patient` from it
// (validated against HAPI CR). The questionnaires SHN originates against declare no launchContext (the SDC
// launchContext CodeSystem is unresolvable by the US-Core egress validator), so sending a
// `context` param would be unmatched; subject is the clean, sufficient binding.
func buildPopulateParameters(questionnaire []byte, pc PopulateContext) ([]byte, error) {
	// The subject must be the FHIR-store-resolvable Patient ref (a scoped id) so the engine's CQL
	// retrieves hit the right compartment; the logical SHN ref does not resolve. SubjectFHIRRef
	// falls back to PatientRef when the SoR couldn't resolve a store id.
	subject := pc.SubjectFHIRRef
	if subject == "" {
		subject = pc.PatientRef
	}
	params := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{"name": "questionnaire", "resource": json.RawMessage(questionnaire)},
			{"name": "subject", "valueReference": map[string]any{"reference": subject}},
		},
	}
	return json.Marshal(params)
}

// extractQuestionnaireResponse returns the QuestionnaireResponse from a $populate response
// body. The operation returns the QR directly (HTTP body IS the resource); reject anything
// else as an upstream fault.
func extractQuestionnaireResponse(body []byte) ([]byte, error) {
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if probe.ResourceType != "QuestionnaireResponse" {
		return nil, errPopulateUpstream
	}
	return body, nil
}
