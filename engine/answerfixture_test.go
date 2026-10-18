package engine

import (
	"encoding/json"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// testAnswerRequest is what this package's test answers restate from the
// request they answer (shnsdk.PASAnswerRequest): a professional claim,
// addressed to the request's own payer Organization entry.
var testAnswerRequest = shnsdk.PASAnswerRequest{
	ClaimType: json.RawMessage(`{"coding":[{"system":"http://terminology.hl7.org/CodeSystem/claim-type","code":"professional"}]}`),
	Insurer:   json.RawMessage(`{"reference":"Organization/org-cms-payer"}`),
}
