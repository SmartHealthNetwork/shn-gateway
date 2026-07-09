package scenariodriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// This file drives the br-provider BFF origination chain (a real HL7-DaVinci/br-provider
// instance, config-only, alongside the SHN Kit's other bundled binaries). It is honest about
// three DIFFERENT auth/transport paths, so a reader can tell exactly what's real and what's
// direct-mint at each hop:
//
//  1. CRD origination goes THROUGH br-provider's real BFF (full fidelity): a conformant CDS
//     Hooks order-sign request is POSTed to the BFF, which injects its own signed CDS-client
//     JWT before forwarding to the SHN ingress. X-Bypass-Auth only disables the BFF's INBOUND
//     auth check on this call — it never touches the outbound JWT the BFF signs and sends on.
//
//  2. DTR-fetch and PAS-submit are DIRECT-MINT bearer calls straight to the ingress, not routed
//     through the BFF (Gap A): the BFF's DTR/PAS proxy controllers authenticate with
//     UDAP B2B client_credentials, which the SHN ingress cannot satisfy (it hosts no
//     /.well-known/udap), so those calls 401 through the BFF. The Driver's existing
//     PostQuestionnairePackage/SubmitPAS transport (Config.Key, faithful direct bearer) carries
//     these two legs instead — the auth shape is faithful, the transport just isn't BFF-relayed.
//
//  3. DTR populate calls br-provider's real /api/dtr/populate endpoint (genuine cqf-fhir SDC
//     output) — the returned QuestionnaireResponse is never authored by this driver.

// BRProviderReady probes br-provider's single-port BFF/FHIR host: a GET to /fhir/metadata that
// must return 200. It returns a descriptive error (not a skip) — callers decide whether an
// unreachable br-provider means skip-this-test or fail-this-run.
func (d *Driver) BRProviderReady() error {
	resp, err := d.cfg.HTTP.Get(d.cfg.BFFURL + "/fhir/metadata")
	if err != nil {
		return fmt.Errorf("scenariodriver: br-provider BFF unreachable at %s: %w", d.cfg.BFFURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scenariodriver: br-provider BFF /fhir/metadata = %d at %s", resp.StatusCode, d.cfg.BFFURL)
	}
	return nil
}

// BRPResult is the outcome of one CRD origination leg through br-provider's BFF: the raw
// status/body, plus the parsed cards projection when the response is actually a cards envelope.
type BRPResult struct {
	Status int
	Body   []byte
	Cards  Cards // parsed only when Status==200 and Body looks like a cards envelope
	Member string
}

// Covered delegates to the parsed Cards projection.
func (r BRPResult) Covered() string { return r.Cards.Covered() }

// PANeeded delegates to the parsed Cards projection.
func (r BRPResult) PANeeded() string { return r.Cards.PANeeded() }

// Questionnaires delegates to the parsed Cards projection.
func (r BRPResult) Questionnaires() []string { return r.Cards.Questionnaires() }

// OriginateThroughBRProvider builds a conformant CDS Hooks order-sign request for the scenario's
// persona-selected code (PersonaOrders) on the given member, and POSTs it through br-provider's
// real BFF so the request carries br-provider's own CDS-client JWT (auth path 1 above). The BFF
// endpoint is POST {BFFURL}/api/cds-services/order-select-crd?server=<URL-escaped {IngressBase}/cds-services>;
// the BFF forwards the body to that server and injects its signed JWT before relaying to the SHN
// ingress. Cards are parsed onto the result only when the response is a 200 cards envelope — a
// non-card response (auth failure, error) is left for the caller to inspect via Status/Body.
func (d *Driver) OriginateThroughBRProvider(scenario, member string) (BRPResult, error) {
	so, ok := PersonaOrders[scenario]
	if !ok {
		return BRPResult{}, fmt.Errorf("scenariodriver: unknown scenario %q (want noPA|approve|deny|pend)", scenario)
	}
	reqBody, err := BuildCRDRequest(member, SystemHCPCS, so.Code, so.Display)
	if err != nil {
		return BRPResult{}, err
	}

	endpoint := d.cfg.BFFURL + "/api/cds-services/order-select-crd?server=" + url.QueryEscape(d.cfg.IngressBase+"/cds-services")
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return BRPResult{}, fmt.Errorf("scenariodriver: build BFF CRD request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bypass-Auth", "true") // skips the BFF's inbound auth only; its outbound CDS-client JWT is still signed
	resp, err := d.cfg.HTTP.Do(req)
	if err != nil {
		return BRPResult{}, fmt.Errorf("scenariodriver: CRD origination via br-provider BFF: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return BRPResult{}, fmt.Errorf("scenariodriver: read BFF CRD response body: %w", err)
	}

	r := BRPResult{Status: resp.StatusCode, Body: respBody, Member: member}
	if resp.StatusCode == http.StatusOK && bytes.Contains(respBody, []byte(`"cards"`)) {
		if cards, err := ParseCards(respBody); err == nil {
			r.Cards = cards
		}
	}
	return r, nil
}

// DTRPackage carries the $questionnaire-package Bundle fetched for one CRD card's canonical, plus
// the member and canonical needed to drive br-provider's populate endpoint next.
type DTRPackage struct {
	Status    int
	Body      []byte
	Canonical string
	Member    string
}

// FetchDTRPackage drives the DTR $questionnaire-package leg via the Driver's direct-mint bearer
// transport (auth path 2 above — not through the BFF). It uses the first questionnaire canonical
// carried on the CRD result's cards; a result with no canonical is an error (the caller has
// nothing to fetch a package for). The fetch's HTTP status is NOT asserted here — a non-200 is
// returned on DTRPackage.Status for the caller to assert on.
func (d *Driver) FetchDTRPackage(r BRPResult) (DTRPackage, error) {
	qs := r.Questionnaires()
	if len(qs) == 0 {
		return DTRPackage{}, fmt.Errorf("scenariodriver: CRD card carried no questionnaire canonical to fetch a DTR package for; cards=%+v", r.Cards)
	}
	canonical := qs[0]
	res, err := d.PostQuestionnairePackage(canonical, r.Member)
	if err != nil {
		return DTRPackage{}, err
	}
	return DTRPackage{Status: res.Status, Body: res.Body, Canonical: canonical, Member: r.Member}, nil
}

// PopulateViaBRProvider drives br-provider's real /api/dtr/populate (auth path 3 above) with a
// fetched DTR package, returning the populated QuestionnaireResponse exactly as br-provider's
// cqf-fhir SDC populator produced it. It extracts the Questionnaire resource matching p.Canonical
// out of the package Bundle (falling back to the first Questionnaire entry), unwraps a
// Parameters{return: Bundle} package to its inner Bundle (populate requires a bare Bundle for
// packagebundle, not the Parameters wrapper), and POSTs Parameters{subject, questionnaire,
// packagebundle}.
func (d *Driver) PopulateViaBRProvider(p DTRPackage) ([]byte, error) {
	var pkg map[string]any
	if err := json.Unmarshal(p.Body, &pkg); err != nil {
		return nil, fmt.Errorf("scenariodriver: parse $questionnaire-package Bundle: %w body=%s", err, p.Body)
	}
	q, err := questionnaireFromPackage(pkg, p.Canonical)
	if err != nil {
		return nil, err
	}
	bundle, err := packageBundleResource(pkg)
	if err != nil {
		return nil, err
	}

	params := map[string]any{
		"resourceType": "Parameters",
		"parameter": []any{
			map[string]any{"name": "subject", "valueReference": map[string]any{"reference": "Patient/" + p.Member}},
			map[string]any{"name": "questionnaire", "resource": q},
			map[string]any{"name": "packagebundle", "resource": bundle},
		},
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: marshal populate params: %w", err)
	}

	endpoint := d.cfg.BFFURL + "/api/dtr/populate"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: build populate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set("Accept", "application/fhir+json")
	req.Header.Set("X-Bypass-Auth", "true")
	resp, err := d.cfg.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: br-provider /api/dtr/populate: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: read populate response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scenariodriver: br-provider /api/dtr/populate status=%d, want 200; body=%s", resp.StatusCode, respBody)
	}
	return respBody, nil
}

// questionnaireFromPackage pulls the Questionnaire resource matching canonical out of a
// $questionnaire-package Bundle, falling back to the first Questionnaire entry if none matches
// exactly (the package may carry it un-versioned).
func questionnaireFromPackage(pkg map[string]any, canonical string) (map[string]any, error) {
	var first map[string]any
	for _, e := range bundleEntries(pkg) {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Questionnaire" {
			continue
		}
		if first == nil {
			first = res
		}
		if u, _ := res["url"].(string); u == canonical {
			return res, nil
		}
	}
	if first != nil {
		return first, nil
	}
	return nil, fmt.Errorf("scenariodriver: no Questionnaire resource in the $questionnaire-package Bundle (canonical=%q)", canonical)
}

// bundleEntries returns the entry list from a Bundle, or from a `return` Bundle nested in a
// Parameters (a $questionnaire-package response may be wrapped either way).
func bundleEntries(res map[string]any) []any {
	if res["resourceType"] == "Bundle" {
		entries, _ := res["entry"].([]any)
		return entries
	}
	if res["resourceType"] == "Parameters" {
		for _, p := range asList(res["parameter"]) {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if inner, _ := pm["resource"].(map[string]any); inner != nil && inner["resourceType"] == "Bundle" {
				entries, _ := inner["entry"].([]any)
				return entries
			}
		}
	}
	return nil
}

func asList(v any) []any { l, _ := v.([]any); return l }

// packageBundleResource returns the $questionnaire-package Bundle resource, unwrapping a
// Parameters{return: Bundle} wrapper — br-provider's /api/dtr/populate requires packagebundle to
// be a bare Bundle, not the Parameters wrapper.
func packageBundleResource(res map[string]any) (map[string]any, error) {
	if res["resourceType"] == "Bundle" {
		return res, nil
	}
	if res["resourceType"] == "Parameters" {
		for _, p := range asList(res["parameter"]) {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if inner, _ := pm["resource"].(map[string]any); inner != nil && inner["resourceType"] == "Bundle" {
				return inner, nil
			}
		}
	}
	return nil, fmt.Errorf("scenariodriver: $questionnaire-package response has no Bundle to use as packagebundle (resourceType=%v)", res["resourceType"])
}

// BuildGoldenPASBundleWithQR builds the PAS $submit Claim envelope from the committed prior-auth-
// required golden (PASApproveGolden), rebound onto member, and injects qr as a new Bundle entry
// referenced from the Claim's supportingInfo. Per R-5 the payer ignores the extra supportingInfo
// (its decision keys on the golden's order code) — the QR is carried, not adjudicated on. An
// empty qr is the QR-less fallback: the plain rebound golden is returned unchanged.
func BuildGoldenPASBundleWithQR(member string, qr []byte) ([]byte, error) {
	bundle, err := BuildPASBundle(PASApproveGolden(), member)
	if err != nil {
		return nil, err
	}
	if len(qr) == 0 {
		return bundle, nil
	}

	var b map[string]any
	if err := json.Unmarshal(bundle, &b); err != nil {
		return nil, fmt.Errorf("scenariodriver: parse rebound golden: %w", err)
	}
	var qrRes map[string]any
	if err := json.Unmarshal(qr, &qrRes); err != nil {
		return nil, fmt.Errorf("scenariodriver: parse populated QR: %w", err)
	}
	qrID, _ := qrRes["id"].(string)
	if qrID == "" {
		qrID = "brp-qr"
		qrRes["id"] = qrID
	}

	entries, _ := b["entry"].([]any)
	for _, e := range entries {
		res, _ := e.(map[string]any)["resource"].(map[string]any)
		if res == nil || res["resourceType"] != "Claim" {
			continue
		}
		res["supportingInfo"] = []any{map[string]any{
			"sequence": 1,
			"category": map[string]any{"coding": []any{map[string]any{
				"system": "http://hl7.org/us/davinci-pas/CodeSystem/PASSupportingInfoType",
				"code":   "questionnaire",
			}}},
			"valueReference": map[string]any{"reference": "QuestionnaireResponse/" + qrID},
		}}
	}
	entries = append(entries, map[string]any{
		"fullUrl":  "urn:uuid:QuestionnaireResponse-" + qrID,
		"resource": qrRes,
	})
	b["entry"] = entries

	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: marshal golden+QR bundle: %w", err)
	}
	return out, nil
}

// SubmitPASWithQR builds the golden+QR PAS bundle (BuildGoldenPASBundleWithQR) and submits it via
// the Driver's direct-mint bearer transport (auth path 2 above).
func (d *Driver) SubmitPASWithQR(member string, qr []byte) (PASOutcome, error) {
	bundle, err := BuildGoldenPASBundleWithQR(member, qr)
	if err != nil {
		return PASOutcome{}, err
	}
	return d.SubmitPAS(bundle)
}
