// native.go — the native-forward payer LegResponder (Case 1, design §0/§3). It
// forwards each read-only leg to a partner's real Da Vinci endpoint over a
// SMART-authenticated *http.Client and returns the partner's FHIR. The engine still
// owns authority (the (A)/(B) inbound fences + the (C) outbound subject fence, now
// defending a real party), sealing, edge $validate, and audit (AI-11). native.go
// itself imports no shnsdk symbols; the PAS legs (nativepas.go) reuse the originator's
// PUBLISHED shnsdk parsers (standalone-build safe). It implements the internal, unstable
// engine.LegResponder (STABILITY: connectors/* is the supported surface); it
// graduates to connectors/davinci when LegResponder promotes to shnsdk.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"
)

// crdServiceID is the conventional CDS Hooks order-select service id this slice
// posts to. Endpoint discovery (/cds-services) is the deferred refinement (§6.4).
const crdServiceID = "shn-order-select"

const maxPartnerBody = 8 << 20 // 8 MiB cap on a partner response body

type nativeResponder struct {
	client  *http.Client
	baseURL string
	store   Store // gateway-owned shadow ledger + EOB Store for the PAS legs (nil ⇒ read-only only)
	clock   func() time.Time
}

var _ LegResponder = (*nativeResponder)(nil)

// NewNativeResponder builds the native-forward Responder over a ready *http.Client
// (in production a smartauth bearer client; in tests a fixed-bearer client). store is
// the gateway-owned Store the PAS legs use (pended ledger + EOB); a nil store is valid
// for a read-only-only native responder. clock is used for the gateway-projected EOB
// `created`; nil ⇒ time.Now.
func NewNativeResponder(client *http.Client, baseURL string, store Store, clock func() time.Time) LegResponder {
	if clock == nil {
		clock = time.Now
	}
	return &nativeResponder{client: client, baseURL: baseURL, store: store, clock: clock}
}

func (n *nativeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	switch leg {
	case "coverage-eligibility":
		body, bad := n.post(ctx, "/CoverageEligibilityRequest", requestFHIR, "eligibility")
		if bad.Status != 0 {
			return bad, nil
		}
		return LegResult{ResponseFHIR: body}, nil

	case "crd-order-select":
		hi, err := newHookInstance()
		if err != nil {
			return LegResult{}, fmt.Errorf("engine: hookInstance: %w", err)
		}
		hook, err := augmentCRDHook(requestFHIR, hi)
		if err != nil {
			return LegResult{}, err // gateway's own build fault → 500
		}
		body, bad := n.post(ctx, "/cds-services/"+crdServiceID, hook, "CRD")
		if bad.Status != 0 {
			return bad, nil
		}
		return LegResult{ResponseFHIR: body}, nil

	case "dtr-questionnaire-fetch":
		var fetch struct {
			Canonical string `json:"canonical"`
		}
		if err := jsonUnmarshalStrictCanonical(requestFHIR, &fetch.Canonical); err != nil {
			// Malformed CLIENT request → 400 (parity with sandbox's 400, not 500).
			return LegResult{Status: http.StatusBadRequest, Message: "parse questionnaire fetch failed"}, nil
		}
		params, err := buildQuestionnairePackageRequest(fetch.Canonical)
		if err != nil {
			return LegResult{}, err // build fault → 500
		}
		body, bad := n.post(ctx, "/Questionnaire/$questionnaire-package", params, "DTR")
		if bad.Status != 0 {
			return bad, nil
		}
		// §6.2: forward the partner's $questionnaire-package Bundle VERBATIM (the
		// dependent Libraries/ValueSets are preserved for Step 3). The package→
		// Questionnaire extraction — and the no-Questionnaire 502 — is now a consumer
		// concern (originate.go), so this leg no longer inspects the body.
		return LegResult{ResponseFHIR: body}, nil

	case "pas-claim":
		return n.handlePASClaim(ctx, corrID, subjectPCI, requestFHIR)

	case "pas-claim-update":
		return n.handlePASClaimUpdate(ctx, corrID, subjectPCI, requestFHIR)

	default:
		// The composite routes the read-only + PAS legs here; this is defensive for an
		// unrouted leg.
		return LegResult{}, fmt.Errorf("engine: nativeResponder: unhandled leg %q", leg)
	}
}

// post forwards body to baseURL+path. A transport error or a non-2xx status maps to a
// 502 LegResult (upstream payer failure); never an error return (reserved for the
// gateway's own faults → 500). Returns (responseBody, LegResult{}) on success or
// (nil, LegResult{Status:502,…}) on upstream failure.
func (n *nativeResponder) post(ctx context.Context, path string, body []byte, label string) ([]byte, LegResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, LegResult{Status: http.StatusBadGateway, Message: "upstream payer " + label + " request build failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, LegResult{Status: http.StatusBadGateway, Message: "upstream payer " + label + " unreachable"}
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, LegResult{Status: http.StatusBadGateway, Message: "upstream payer " + label + " read failed"}
	}
	if resp.StatusCode/100 != 2 {
		return nil, LegResult{Status: http.StatusBadGateway, Message: "upstream payer " + label + " returned " + resp.Status}
	}
	return rb, LegResult{}
}

func newHookInstance() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
