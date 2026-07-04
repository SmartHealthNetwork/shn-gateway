// Package scenariodriver drives the eight Prior Authorization scenarios
// (UC-01…08) against a Smart Gateway's edges: the Da Vinci ingress (CRD /
// DTR $questionnaire-package / PAS $submit, UDAP B2B direct bearer), a real
// br-provider BFF (origination + SDC populate), the provider-data
// /scenario/* origination routes, the ops console, and the patient surface.
// It is the importable core of the live conformance gate (FR-G28) and the
// scenario-driving engine the SHN Kit's daemon consumes. Transport methods
// return errors and raw results — assertions and environment gating stay
// with callers.
package scenariodriver

import _ "embed"

// The two committed br-payer $submit request goldens (Da Vinci PAS bundles;
// br-payer keys its decision on the order code, not the patient):
// prior-auth-required → A1 approve; home-oxygen (E0424) → A4 pend.

//go:embed goldens/pas-submit-request.json
var pasApproveGolden []byte

//go:embed goldens/pas-submit-request-pended.json
var pasPendGolden []byte

// PASApproveGolden returns a copy of the prior-auth-required $submit golden (→ A1).
func PASApproveGolden() []byte { return append([]byte(nil), pasApproveGolden...) }

// PASPendGolden returns a copy of the home-oxygen $submit golden (→ A4 pended).
func PASPendGolden() []byte { return append([]byte(nil), pasPendGolden...) }
