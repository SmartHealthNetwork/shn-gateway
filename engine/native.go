// native.go — the native-forward payer LegResponder (Case 1). It
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

const relayBodyCap = 6 << 20 // 6 MiB — headroom under the 8 MiB MaxResponseBytes for seal + wrapper

type nativeResponder struct {
	client               *http.Client
	baseURL              string // FHIR base ($questionnaire-package, $submit, CoverageEligibilityRequest)
	cdsBaseURL           string // CDS Hooks base (/cds-services/{id}); defaults to baseURL when co-located
	crdServiceID         string // discovered or overridden at construction (FR-G26)
	crdHookOverride      string // when set, the request hook is rewritten to this before CRD forward (FR-G26; br-payer's order-sign)
	crdDispatchServiceID string // partner's order-dispatch CDS service id (order-dispatch leg)
	crdDispatchHook      string // when set, the request hook is rewritten to this before order-dispatch forward
	// crdCoverageBundle wraps the CRD request's bare prefetch.coverage into a searchset Bundle
	// before forwarding — for a partner whose order-sign `coverage` prefetch is a SEARCH
	// template (Coverage?beneficiary=…) that demands a searchset Bundle (bare Coverage → 412
	// "Missing Coverage"). The SHN spine carries a BARE Coverage (provider routing +
	// the payer-side bind both read bare, crd_native.go), so the wrap is a partner-scoped EGRESS
	// step run AFTER the bind. Gated by PAYER_DAVINCI_CRD_COVERAGE_BUNDLE; off ⇒ bare verbatim
	// (br-payer conformance untouched).
	crdCoverageBundle bool
	store             Store // gateway-owned shadow ledger + EOB Store for the PAS legs (nil ⇒ read-only only)
	clock             func() time.Time
	// PAS pend re-query: a real Da Vinci payer (br-payer) re-pends a conformant
	// amendment (persistUpdatePath keeps a conditional item A4 + reschedules) and auto-resolves
	// A4→A1 only on its own timer (PasPendedResolutionService, pas.pended-resolution-delay-seconds).
	// On a re-pended ClaimUpdate the responder polls GET ClaimResponse/{id} until A1 (or timeout →
	// 422, no silent pass). Timeout MUST exceed the payer's resolution delay (E4 lowers it to 3s for
	// the harness) and stay under the originator's 30s Hub-client timeout.
	pendReQueryTimeout  time.Duration
	pendReQueryInterval time.Duration
}

const (
	defaultPendReQueryTimeout  = 12 * time.Second
	defaultPendReQueryInterval = 1 * time.Second
)

// NativeOption configures optional nativeResponder behavior.
type NativeOption func(*nativeResponder)

// WithCDSBaseURL overrides the base used for CDS Hooks (CRD) posts, for partners whose
// CDS Hooks endpoint is NOT co-located with their FHIR base — e.g. br-payer serves CDS
// Hooks at root /cds-services but FHIR ops under /fhir. Unset ⇒ CDS posts use the FHIR
// baseURL (co-located default, the prior behavior). FR-G28 / OWD-G8.
func WithCDSBaseURL(cdsBaseURL string) NativeOption {
	return func(n *nativeResponder) {
		if cdsBaseURL != "" {
			n.cdsBaseURL = cdsBaseURL
		}
	}
}

// WithCRDHook sets the CDS Hooks hook value the native-forward stamps on the CRD request
// before forwarding, to match the partner's configured CRD service (e.g. br-payer's
// order-sign-crd demands hook:order-sign while SHN originates the canonical order-select).
// Unset ⇒ forward the originator's hook verbatim (the prior behavior).
func WithCRDHook(hook string) NativeOption {
	return func(n *nativeResponder) {
		if hook != "" {
			n.crdHookOverride = hook
		}
	}
}

// WithCRDDispatchService configures the partner's order-dispatch CDS service id + hook for the
// crd-order-dispatch leg (distinct from order-select's crdServiceID/crdHookOverride). ONE payer-gw
// serves both: crd-order-select→order-sign-crd, crd-order-dispatch→order-dispatch-crd.
func WithCRDDispatchService(serviceID, hook string) NativeOption {
	return func(n *nativeResponder) { n.crdDispatchServiceID, n.crdDispatchHook = serviceID, hook }
}

// WithCRDCoverageBundle enables wrapping the CRD request's bare prefetch.coverage into a
// searchset Bundle before forwarding — such a partner's order-sign `coverage` prefetch is a SEARCH
// template (Coverage?beneficiary=…) demanding a searchset Bundle, while the SHN spine carries a
// bare Coverage (provider routing + payer-side bind both read bare, crd_native.go). Idempotent:
// an already-Bundle coverage is left as-is. Unset ⇒ forward the coverage verbatim (br-payer's
// shape). Set via PAYER_DAVINCI_CRD_COVERAGE_BUNDLE (per-partner, opt-in).
func WithCRDCoverageBundle(on bool) NativeOption {
	return func(n *nativeResponder) { n.crdCoverageBundle = on }
}

// WithPendReQuery overrides the PAS pend re-query poll timeout + interval (E2). Zero values keep
// the defaults. Used to make the hermetic nativeResponder tests fast (short interval) and to let
// the harness tune the bound relative to the payer's resolution delay (E4).
func WithPendReQuery(timeout, interval time.Duration) NativeOption {
	return func(n *nativeResponder) {
		if timeout > 0 {
			n.pendReQueryTimeout = timeout
		}
		if interval > 0 {
			n.pendReQueryInterval = interval
		}
	}
}

var _ LegResponder = (*nativeResponder)(nil)

// NewNativeResponder builds the native-forward Responder over a ready *http.Client
// (in production a smartauth bearer client; in tests a fixed-bearer client).
// crdServiceID is the partner's CDS Hooks order-select service id, resolved at boot
// via DiscoverCRDServiceID (FR-G26). store is the gateway-owned Store the PAS legs
// use (pended ledger + EOB); a nil store is valid for a read-only-only native
// responder. clock is used for the gateway-projected EOB `created`; nil ⇒ time.Now.
func NewNativeResponder(client *http.Client, baseURL, crdServiceID string, store Store, clock func() time.Time, opts ...NativeOption) LegResponder {
	if clock == nil {
		clock = time.Now
	}
	n := &nativeResponder{
		client: client, baseURL: baseURL, cdsBaseURL: baseURL, crdServiceID: crdServiceID, store: store, clock: clock,
		pendReQueryTimeout: defaultPendReQueryTimeout, pendReQueryInterval: defaultPendReQueryInterval,
	}
	for _, o := range opts {
		o(n)
	}
	return n
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

// markForeignRelay marks a LegResult as a verbatim foreign-far-end relay: ResponseFHIR (when
// present) is the real RI's bytes in the RI's OWN patient namespace. The engine then skips the
// response member-fence (R-7) and the response egress-$validate (R-8) for this result, while
// fencing+validating the SHN-produced side-effects (EOB) unconditionally. Both flags are set
// together here (native = produced-by-foreign AND foreign-namespace); the conformant-mock north
// star is the only producer that would set them apart. Single declaration site per leg
// (covers every internal return of handlePASClaim*Native) — fail-closed if ever missed (zero value
// = strict fence + $validate).
func markForeignRelay(r LegResult) LegResult {
	r.ResponseRelayed = true
	r.ResponseSubjectForeign = true
	return r
}

func (n *nativeResponder) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	switch leg {
	case "coverage-eligibility":
		body, bad, err := n.post(ctx, n.baseURL, "/CoverageEligibilityRequest", requestFHIR, "eligibility")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		return LegResult{ResponseFHIR: body}, nil

	case "crd-order-select":
		// The request is ALREADY a conformant CDS Hooks request (br-provider's bytes via
		// the ingress); forward it VERBATIM — no augmentCRDHook minimized shaping.
		// Rung-1 faithful pass-through: br-payer receives br-provider's actual request
		// bytes. The response side is identical to the minimized leg (FR-G25).
		// When crdHookOverride is set, rewrite the request's top-level "hook" field to
		// match the partner CRD service's declared hook (e.g. br-payer's order-sign-crd
		// demands hook:order-sign while SHN originates hook:order-select — 400 otherwise).
		fwd := requestFHIR
		if n.crdHookOverride != "" {
			rewritten, herr := rewriteCDSHook(requestFHIR, n.crdHookOverride)
			if herr != nil {
				return LegResult{}, herr // malformed request envelope → 500 (gateway fault)
			}
			fwd = rewritten
		}
		// Per-partner egress shaping (AFTER the payer-side bind + ingress-$validate read the
		// bare Coverage, crd_native.go): wrap prefetch.coverage bare→searchset-Bundle so the partner's
		// order-sign search-template `coverage` prefetch is satisfied (bare → 412 "Missing Coverage").
		if n.crdCoverageBundle {
			wrapped, werr := wrapCRDCoverageSearchset(fwd)
			if werr != nil {
				return LegResult{}, werr // malformed request envelope → 500 (gateway fault)
			}
			fwd = wrapped
		}
		body, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+n.crdServiceID, fwd, "CRD")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		res, nerr := normalizeCRDResponse(body)
		if nerr != nil {
			return res, nerr
		}
		// Same scope as the coverage-bundle wrap: relay the partner's CRD
		// systemActions — the coverage-annotated order carrying the coverage-assertion-id —
		// alongside the SHN cards, so the provider can drive DTR. The partner's $questionnaire-package
		// REQUIRES that CRD-updated order (it 501s "ServiceRequest without a Coverage Assertion Id
		// extension is not supported"); the normalized-cards-only response drops it. This is the
		// faithful Da Vinci CRD→DTR handoff. br-payer (flag off) is byte-unchanged.
		if n.crdCoverageBundle {
			res.ResponseFHIR = mergeCRDSystemActions(res.ResponseFHIR, body)
		}
		return res, nil

	case "dtr-questionnaire-fetch":
		// Parse the published leg request: canonical (required) + an OPTIONAL coverage
		// resource carried verbatim from the inbound $questionnaire-package (FR-G28).
		// Fail-closed posture: malformed JSON or a missing/empty canonical → 400 (parity
		// with the sandbox's 400, not 500).
		var fetch dtrLegRequest
		if err := json.Unmarshal(requestFHIR, &fetch); err != nil || (fetch.Canonical == "" && len(fetch.Order) == 0) {
			return LegResult{Status: http.StatusBadRequest, Message: "parse questionnaire fetch failed"}, nil
		}
		// Two shapes: an ORDER-driven request (the CRD-updated order carries the
		// coverage-assertion-id the partner keys the questionnaire off; it has no `questionnaire` param)
		// or the canonical request (br-payer / sandbox). The provider's coverage is carried through
		// in both so the partner's required `coverage` parameter is satisfied (FR-G28).
		var params []byte
		var err error
		if len(fetch.Order) > 0 {
			params, err = buildQuestionnairePackageOrderRequest(fetch.Order, fetch.Coverage)
		} else {
			params, err = buildQuestionnairePackageRequest(fetch.Canonical, fetch.Coverage)
		}
		if err != nil {
			return LegResult{}, err // build fault → 500
		}
		body, bad, err := n.post(ctx, n.baseURL, "/Questionnaire/$questionnaire-package", params, "DTR")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		// Forward the partner's $questionnaire-package Bundle VERBATIM (the
		// dependent Libraries/ValueSets are preserved for Step 3). The package→
		// Questionnaire extraction — and the no-Questionnaire 502 — is now a consumer
		// concern (originate.go), so this leg no longer inspects the body.
		return LegResult{ResponseFHIR: body}, nil

	case "crd-order-dispatch":
		if n.crdDispatchServiceID == "" {
			return LegResult{Status: http.StatusBadGateway, Message: "crd-order-dispatch not configured (set PAYER_DAVINCI_DISPATCH_SERVICE_ID)"}, nil
		}
		fwd := requestFHIR
		if n.crdDispatchHook != "" {
			rewritten, herr := rewriteCDSHook(requestFHIR, n.crdDispatchHook)
			if herr != nil {
				return LegResult{}, herr // malformed request envelope → 500 (gateway fault)
			}
			fwd = rewritten
		}
		body, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+n.crdDispatchServiceID, fwd, "CRD-dispatch")
		if err != nil {
			return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
		}
		if bad.Status != 0 {
			return bad, nil // upstream non-2xx → relayable LegResult (ResponseFHIR carries the body)
		}
		return normalizeCRDResponse(body)

	case "pas-claim":
		res, err := n.handlePASClaimNative(ctx, corrID, subjectPCI, requestFHIR)
		return markForeignRelay(res), err

	case "pas-claim-update":
		res, err := n.handlePASClaimUpdateNative(ctx, corrID, subjectPCI, requestFHIR)
		return markForeignRelay(res), err

	default:
		// The br-payer-targeting lane routes the read-only + PAS legs here; this is defensive
		// for an unrouted leg.
		return LegResult{}, fmt.Errorf("engine: nativeResponder: unhandled leg %q", leg)
	}
}

// post forwards body to base+path. An upstream that RETURNS an HTTP response — any
// status — is the recipient's answer: 2xx → (body, LegResult{}, nil); non-2xx →
// (nil, LegResult{Status:<code>, ResponseFHIR:<upstream body>}, nil) for verbatim
// relay. A NO-RESPONSE fault (build/dial/read) is (nil, LegResult{}, error) → the
// engine maps it to 500 → "hub routing failed".
func (n *nativeResponder) post(ctx context.Context, base, path string, body []byte, label string) ([]byte, LegResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s read failed: %w", label, err)
	}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return nil, LegResult{}, fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb))
		}
		return nil, LegResult{Status: resp.StatusCode, ResponseFHIR: rb}, nil
	}
	return rb, LegResult{}, nil
}

// get reads base+path (the read sibling of post), reusing the same authed client. Used by the PAS
// pend re-query (GET ClaimResponse/{id}). Same relay-non-2xx / error-on-no-response contract as post.
func (n *nativeResponder) get(ctx context.Context, base, path, label string) ([]byte, LegResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s request build failed: %w", label, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPartnerBody))
	if err != nil {
		return nil, LegResult{}, fmt.Errorf("upstream payer %s read failed: %w", label, err)
	}
	if resp.StatusCode/100 != 2 {
		if len(rb) > relayBodyCap { // headroom under MaxResponseBytes for seal + wrapper
			return nil, LegResult{}, fmt.Errorf("upstream payer %s body too large to relay (%d bytes)", label, len(rb))
		}
		return nil, LegResult{Status: resp.StatusCode, ResponseFHIR: rb}, nil
	}
	return rb, LegResult{}, nil
}

// normalizeCRDResponse is the CRD response tail (FR-G25): normalize the partner's
// coverage-information to the canonical CardCoverage (davincimap.go), then re-render
// SHN cards with shnsdk.BuildCards. Used by the conformant crd-order-select
// native CRD case. Fails closed (Status 502) when no canonical coverage is resolvable.
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

// mergeCRDSystemActions returns cardsJSON with the partner's raw CRD `systemActions` merged in
// (the coverage-annotated order the provider needs to drive DTR). A no-op when the partner
// returned no systemActions or either side is unparseable — the SHN cards pass through unchanged.
func mergeCRDSystemActions(cardsJSON, rawResp []byte) []byte {
	var raw struct {
		SystemActions json.RawMessage `json:"systemActions"`
	}
	if err := json.Unmarshal(rawResp, &raw); err != nil || len(raw.SystemActions) == 0 {
		return cardsJSON
	}
	var cards map[string]json.RawMessage
	if err := json.Unmarshal(cardsJSON, &cards); err != nil {
		return cardsJSON
	}
	cards["systemActions"] = raw.SystemActions
	if merged, err := json.Marshal(cards); err == nil {
		return merged
	}
	return cardsJSON
}

// rewriteCDSHook returns reqJSON with its top-level "hook" set to hook, preserving every
// other field verbatim. Used to adapt SHN's canonical order-select request to a partner
// CRD service registered under a different hook (br-payer's order-sign-crd).
func rewriteCDSHook(reqJSON []byte, hook string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: rewrite CDS hook: %w", err)
	}
	hookJSON, err := json.Marshal(hook)
	if err != nil {
		return nil, fmt.Errorf("engine: rewrite CDS hook: %w", err)
	}
	m["hook"] = hookJSON
	return json.Marshal(m)
}

// wrapCRDCoverageSearchset rewrites a CDS Hooks request's prefetch.coverage from a BARE
// Coverage resource into a searchset Bundle wrapping it verbatim, preserving every other field.
// A partner whose `coverage` prefetch is a SEARCH template (Coverage?beneficiary=…, the partner's
// order-sign) requires the searchset shape; the SHN spine carries a bare Coverage. No-op
// (returns reqJSON unchanged) when prefetch or coverage is absent, or coverage is already a
// Bundle (idempotent — never a Bundle-in-a-Bundle).
func wrapCRDCoverageSearchset(reqJSON []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(reqJSON, &m); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage: %w", err)
	}
	pfRaw, ok := m["prefetch"]
	if !ok {
		return reqJSON, nil
	}
	var pf map[string]json.RawMessage
	if err := json.Unmarshal(pfRaw, &pf); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage prefetch: %w", err)
	}
	covRaw, ok := pf["coverage"]
	if !ok {
		return reqJSON, nil
	}
	var probe struct {
		ResourceType string `json:"resourceType"`
	}
	if err := json.Unmarshal(covRaw, &probe); err != nil {
		return nil, fmt.Errorf("engine: wrap crd coverage resource: %w", err)
	}
	if probe.ResourceType != "Coverage" {
		return reqJSON, nil // absent-shaped or already a Bundle — leave verbatim (idempotent)
	}
	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"entry":        []any{map[string]any{"resource": covRaw}},
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal crd coverage searchset: %w", err)
	}
	pf["coverage"] = bundleJSON
	pfJSON, err := json.Marshal(pf)
	if err != nil {
		return nil, fmt.Errorf("engine: marshal crd prefetch: %w", err)
	}
	m["prefetch"] = pfJSON
	return json.Marshal(m)
}
