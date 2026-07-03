package scenariodriver

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Config wires a Driver to a running SHN Kit deployment: the Da Vinci ingress it
// authenticates against (UDAP B2B direct bearer), plus the unauthenticated
// gateway-edge surfaces (provider-data origination, ops console, patient surface).
type Config struct {
	IngressURL      string           // host-published Da Vinci ingress, e.g. http://localhost:8091
	IngressBase     string           // config-pinned IngressBaseURL — every minted aud must be under it
	ClientID        string           // registered UDAP B2B client_id (JWT iss)
	Key             *rsa.PrivateKey  // RS384 signing key for the direct bearer
	BFFURL          string           // br-provider single-port host base (BFF + FHIR)
	ProviderDataURL string           // provider-data origination gateway base (/scenario/* routes)
	ConsoleURL      string           // ops console base (optional)
	PHGURL          string           // patient surface base (optional)
	HTTP            *http.Client     // nil → http.DefaultClient
	Now             func() time.Time // nil → time.Now (bearer iat/exp + jti)
}

// Driver drives a live SHN Kit deployment end to end: it mints the same UDAP B2B
// direct bearer a real holder BFF would send, POSTs conformant Da Vinci requests to
// the ingress, and reads back the gateway-edge surfaces (provider-data origination,
// ops console, patient-surface) — the transport core the Kit desktop app and its
// scenario runners drive.
type Driver struct {
	cfg Config
}

// New constructs a Driver, defaulting HTTP to http.DefaultClient and Now to
// time.Now when unset.
func New(cfg Config) *Driver {
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Driver{cfg: cfg}
}

// HTTPResult is a raw HTTP response: the status code and the fully-read body.
// Non-2xx statuses are not errors — callers assert on Status themselves.
type HTTPResult struct {
	Status int
	Body   []byte
}

// MintDirectBearer mints a UDAP B2B direct bearer FAITHFUL to a holder BFF's CDS
// client JWT: iss = the registered client_id, aud = the called endpoint URL (under
// the config's IngressBase), exp ≤ 5min, a fresh jti, and NO sub. Signed RS384 with
// Config.Key. The clock comes from Config.Now (house clock rule), not the wall clock.
func (d *Driver) MintDirectBearer(audURL string) (string, error) {
	if d.cfg.Key == nil {
		return "", errors.New("scenariodriver: MintDirectBearer: no signing key configured")
	}
	now := d.cfg.Now()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS384, jwt.MapClaims{
		"iss": d.cfg.ClientID,
		"aud": audURL,
		"jti": fmt.Sprintf("shn-drv-%d", now.UnixNano()),
		"iat": now.Unix(),
		"exp": now.Add(4 * time.Minute).Unix(),
	}).SignedString(d.cfg.Key)
	if err != nil {
		return "", fmt.Errorf("scenariodriver: mint direct bearer: %w", err)
	}
	return s, nil
}

// postBearer POSTs body to {IngressURL}{path} with a minted direct bearer whose aud
// is {IngressBase}{path} — the audUnder contract the ingress enforces on every leg.
func (d *Driver) postBearer(path string, body []byte) (HTTPResult, error) {
	endpoint := d.cfg.IngressURL + path
	aud := d.cfg.IngressBase + path
	tok, err := d.MintDirectBearer(aud)
	if err != nil {
		return HTTPResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.cfg.HTTP.Do(req)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: read response body for %s: %w", path, err)
	}
	return HTTPResult{Status: resp.StatusCode, Body: respBody}, nil
}

// postJSON is the bare unauthenticated POST used by the provider-data/console
// scenario routes (they are internal, un-auth'd surfaces — the same way the ops
// console addresses any provider gateway).
func (d *Driver) postJSON(base, path, jsonBody string) (HTTPResult, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(jsonBody))
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: build request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.cfg.HTTP.Do(req)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: POST %s%s: %w", base, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return HTTPResult{}, fmt.Errorf("scenariodriver: read response body for %s: %w", path, err)
	}
	return HTTPResult{Status: resp.StatusCode, Body: body}, nil
}

// PostCRD mints a bearer and POSTs a CRD order-select request to the ingress.
func (d *Driver) PostCRD(reqBody []byte) (HTTPResult, error) {
	return d.postBearer("/cds-services/order-select-crd", reqBody)
}

// PostQuestionnairePackage builds a DTR $questionnaire-package request for the given
// questionnaire canonical and member, then mints a bearer and POSTs it to the ingress.
func (d *Driver) PostQuestionnairePackage(canonical, member string) (HTTPResult, error) {
	body, err := BuildQuestionnairePackageRequest(canonical, member)
	if err != nil {
		return HTTPResult{}, err
	}
	return d.postBearer("/Questionnaire/$questionnaire-package", body)
}

// PASOutcome is a PAS $submit result: the raw status/body, plus the decision
// convenience fields filled from a 200 response (Approved+PreAuthRef from a bare
// ClaimResponse, or Pended from a Task Bundle). A non-200 or unparseable body
// leaves the decision fields zero — callers assert on Status/Body themselves.
type PASOutcome struct {
	Status     int
	Body       []byte
	Approved   bool // 200 + ClaimResponse outcome "approved"
	PreAuthRef string
	Pended     bool // 200 + pended (A4) Bundle
}

// SubmitPAS mints a bearer and POSTs a conformant Claim bundle to the PAS $submit
// ingress, filling PASOutcome's decision fields off a 200 response exactly the way
// a holder BFF would after relaying the ingress's response — a bare ClaimResponse
// is parsed for the approved outcome + preAuthRef, a Bundle for the pended shape.
// Parse errors are swallowed here: the fields stay zero and the caller asserts on
// Status/Body directly when the shape is unexpected.
func (d *Driver) SubmitPAS(bundle []byte) (PASOutcome, error) {
	res, err := d.postBearer("/Claim/$submit", bundle)
	if err != nil {
		return PASOutcome{}, err
	}
	out := PASOutcome{Status: res.Status, Body: res.Body}
	if res.Status == http.StatusOK {
		if pr, err := shnsdk.ParseClaimResponse(res.Body); err == nil {
			out.Approved = pr.Outcome == "approved"
			out.PreAuthRef = pr.PreAuthRef
		}
		if pended, _, err := shnsdk.ParsePendedResponse(res.Body); err == nil {
			out.Pended = pended
		}
	}
	return out, nil
}

// RunProviderDataScenario POSTs to a provider-data origination gateway's /scenario/*
// route (an internal, un-auth'd surface — no direct bearer involved).
func (d *Driver) RunProviderDataScenario(path, jsonBody string) (HTTPResult, error) {
	return d.postJSON(d.cfg.ProviderDataURL, path, jsonBody)
}

// RunConsoleScenario POSTs a scenario request to the ops console's /api/run route.
func (d *Driver) RunConsoleScenario(jsonBody string) (HTTPResult, error) {
	return d.postJSON(d.cfg.ConsoleURL, "/api/run", jsonBody)
}

// AuthorizationView is one row of the patient-surface /authorizations render.
// Status is "approved" | "denied" — a pended/failed authorization renders as
// not-approved with Error set describing why, never a literal "pended" status.
type AuthorizationView struct {
	Status string `json:"status"`
	Title  string `json:"title"`
	Error  string `json:"error"`
}

// ResolvePersonaPCI resolves a persona's live PCI off the patient-surface
// /personas list — the PCI is runtime-derived, never a literal (AI-5) — returning
// the PCI of the single persona whose label contains ALL of substrs.
func (d *Driver) ResolvePersonaPCI(substrs ...string) (string, error) {
	resp, err := d.cfg.HTTP.Get(d.cfg.PHGURL + "/personas")
	if err != nil {
		return "", fmt.Errorf("scenariodriver: GET %s/personas: %w", d.cfg.PHGURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("scenariodriver: read /personas body: %w", err)
	}
	var personas []struct {
		Label string `json:"label"`
		PCI   string `json:"pci"`
	}
	if err := json.Unmarshal(body, &personas); err != nil {
		return "", fmt.Errorf("scenariodriver: decode /personas: %w body=%s", err, body)
	}
	for _, p := range personas {
		all := true
		for _, s := range substrs {
			if !strings.Contains(p.Label, s) {
				all = false
				break
			}
		}
		if all {
			return p.PCI, nil
		}
	}
	return "", fmt.Errorf("scenariodriver: no persona label contains all of %v in %s/personas: %s", substrs, d.cfg.PHGURL, body)
}

// GetAuthorizations reads the patient-surface Patient-Access render for a PCI.
func (d *Driver) GetAuthorizations(pci string) ([]AuthorizationView, error) {
	q := url.Values{"pci": []string{pci}}
	resp, err := d.cfg.HTTP.Get(d.cfg.PHGURL + "/authorizations?" + q.Encode())
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: GET %s/authorizations?pci=%s: %w", d.cfg.PHGURL, pci, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("scenariodriver: read /authorizations body: %w", err)
	}
	var views []AuthorizationView
	if err := json.Unmarshal(body, &views); err != nil {
		return nil, fmt.Errorf("scenariodriver: decode /authorizations: %w body=%s", err, body)
	}
	return views, nil
}
