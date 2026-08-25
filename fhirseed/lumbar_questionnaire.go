package fhirseed

import _ "embed"

// LumbarMRIQuestionnaireCanonical is the absolute canonical URL for the demo lumbar-MRI
// PA questionnaire. Must equal internal/crd.QuestionnaireCanonicalLumbarMRI /
// internal/dtr.LumbarMRICanonical — the three are independent module-local copies of the
// SAME fact, not a hierarchy (register §8, 2026-08-24 retirement: the SDK's public
// DemoLumbarQuestionnaire/QuestionnaireCanonicalLumbarMRI exports were retired from
// shn-sdk's published API before the v0.46.0 breaking window closed; the fixture content
// itself is still genuinely needed — shnsdk.FillQuestionnaire is fenced to exactly this
// questionnaire's shape — so each module that must produce or consume it now carries its
// own copy instead of importing the SDK's retired convenience accessor).
const LumbarMRIQuestionnaireCanonical = "http://smarthealth.network/fhir/Questionnaire/pa-lumbar-mri"

// lumbarQuestionnaireJSON is the demo lumbar-MRI PA questionnaire, captured byte-for-byte
// from the last shnsdk.DemoLumbarQuestionnaire() output before that export was retired
// (test/sdkparity's byte-comparison proved the two identical for as long as both existed).
// Its cqf-library extension MUST match DemoLumbarLibrary's Library.url — pinned by
// internal/fhirseed's TestSDKQuestionnaireCanonicalMatchesLibrary.
//
//go:embed testdata/lumbar-mri-questionnaire.json
var lumbarQuestionnaireJSON []byte

// DemoLumbarQuestionnaire returns the FHIR Questionnaire JSON for the demo lumbar-MRI PA
// questionnaire — the gateway module's own copy of the fixture the retired
// shnsdk.DemoLumbarQuestionnaire used to serve (see LumbarMRIQuestionnaireCanonical's doc
// comment). DEMO fixture — a real payer serves its own questionnaires from its own
// Adjudicator. Each call returns a fresh copy so callers may mutate the slice without
// affecting future calls.
func DemoLumbarQuestionnaire() []byte {
	cp := make([]byte, len(lumbarQuestionnaireJSON))
	copy(cp, lumbarQuestionnaireJSON)
	return cp
}
