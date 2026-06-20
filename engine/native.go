// native.go — the native-forward payer LegResponder (Case 1, design §0/§3). It
// forwards each read-only leg to a partner's real Da Vinci endpoint over a
// SMART-authenticated *http.Client and returns the partner's FHIR. The engine still
// owns authority (the (A)/(B) inbound fences + the (C) outbound subject fence, now
// defending a real party), sealing, edge $validate, and audit (AI-11). The PAS legs
// (nativepas.go) reuse the originator's PUBLISHED shnsdk parsers; the CRD leg normalizes
// the partner's coverage-information to the canonical shnsdk.CardCoverage (FR-G25,
// normalizeCRDCoverage in davincimap.go) and re-renders SHN cards with shnsdk.BuildCards,
// so this file references shnsdk too. It implements the internal, unstable
// engine.LegResponder (STABILITY: connectors/* is the supported surface); it
// graduates to connectors/davinci when LegResponder promotes to shnsdk.
package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// crdHook is the CDS Hooks hook name SHN originates for the CRD leg. Discovery
// selects the partner CDS service whose "hook" field matches this value (FR-G26).
const crdHook = "order-select"

const maxPartnerBody = 8 << 20 // 8 MiB cap on a partner response body

type nativeResponder struct {
	client       *http.Client
	baseURL      string
	crdServiceID string // discovered or overridden at construction (FR-G26)
	store        Store  // gateway-owned shadow ledger + EOB Store for the PAS legs (nil ⇒ read-only only)
	clock        func() time.Time
}

var _ LegResponder = (*nativeResponder)(nil)

// NewNativeResponder builds the native-forward Responder over a ready *http.Client
// (in production a smartauth bearer client; in tests a fixed-bearer client).
// crdServiceID is the partner's CDS Hooks order-select service id, resolved at boot
// via DiscoverCRDServiceID (FR-G26). store is the gateway-owned Store the PAS legs
// use (pended ledger + EOB); a nil store is valid for a read-only-only native
// responder. clock is used for the gateway-projected EOB `created`; nil ⇒ time.Now.
func NewNativeResponder(client *http.Client, baseURL, crdServiceID string, store Store, clock func() time.Time) LegResponder {
	if clock == nil {
		clock = time.Now
	}
	return &nativeResponder{client: client, baseURL: baseURL, crdServiceID: crdServiceID, store: store, clock: clock}
}

// DiscoverCRDServiceID resolves the partner's CDS Hooks order-select service id from
// GET {base}/cds-services (FR-G26, OWD-G8). If override is non-empty it is returned
// immediately (escape hatch for partners whose hook name differs from SHN's
// origination hook — e.g. br-payer's order-sign service). Otherwise the listing is
// fetched, filtered to services whose hook matches SHN's crdHook ("order-select"),
// and the id of exactly one match is returned. Zero matches → error (fail-closed);
// multiple matches → error (ambiguous; set PAYER_DAVINCI_CRD_SERVICE_ID). A
// non-2xx or parse error is a fatal boot error (fail-closed per FR-G26).
func DiscoverCRDServiceID(ctx context.Context, client *http.Client, base, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	url := base + "/cds-services"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("engine: build GET %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("engine: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return "", fmt.Errorf("engine: read %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("engine: GET %s returned %s", url, resp.Status)
	}
	var listing struct {
		Services []struct {
			ID   string `json:"id"`
			Hook string `json:"hook"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return "", fmt.Errorf("engine: parse %s: %w", url, err)
	}
	var matches []string
	for _, svc := range listing.Services {
		if svc.Hook == crdHook {
			matches = append(matches, svc.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("engine: no %q service at %s (set PAYER_DAVINCI_CRD_SERVICE_ID to override)", crdHook, url)
	default:
		return "", fmt.Errorf("engine: ambiguous: %d %q services at %s; set PAYER_DAVINCI_CRD_SERVICE_ID to select one", len(matches), crdHook, url)
	}
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
		body, bad := n.post(ctx, "/cds-services/"+n.crdServiceID, hook, "CRD")
		if bad.Status != 0 {
			return bad, nil
		}
		return normalizeCRDResponse(body)

	case "crd-order-select-native":
		// The request is ALREADY a conformant CDS Hooks request (br-provider's bytes via
		// the ingress); forward it VERBATIM — no augmentCRDHook minimized shaping.
		// Rung-1 faithful pass-through: br-payer receives br-provider's actual request
		// bytes. The response side is identical to the minimized leg (FR-G25).
		body, bad := n.post(ctx, "/cds-services/"+n.crdServiceID, requestFHIR, "CRD")
		if bad.Status != 0 {
			return bad, nil
		}
		return normalizeCRDResponse(body)

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

// normalizeCRDResponse is the shared CRD response tail (FR-G25): normalize the
// partner's coverage-information to the canonical CardCoverage (davincimap.go), then
// re-render SHN cards with shnsdk.BuildCards. Used by BOTH the minimized
// (crd-order-select) and conformant (crd-order-select-native) native CRD cases, so
// the response behaviour is byte-identical regardless of which request shape was sent.
// Fails closed (Status 502) when no canonical coverage is resolvable.
func normalizeCRDResponse(body []byte) (LegResult, error) {
	cov, lr := normalizeCRDCoverage(body)
	if lr.Status != 0 {
		return lr, nil
	}
	cardsJSON, err := shnsdk.BuildCards(cov)
	if err != nil {
		return LegResult{}, fmt.Errorf("engine: render normalized cards: %w", err)
	}
	return LegResult{ResponseFHIR: cardsJSON}, nil
}

func newHookInstance() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
