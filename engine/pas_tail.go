// pas_tail.go — the shared LEAN PAS submit→resolve tail, extracted from handleHomeOxygen
// (the provider-data order-dispatch handler) so the provider-data order-select single-shot
// lanes (UC-02/03/04, D-PD-1) reuse one path instead of forking a second.
//
// submitPASClaim is the SINGLE-SHOT submit tail: build the conformant Claim Bundle →
// egress-$validate → originate the pas-claim leg → ingress-$validate. Its caller
// (submitClaimAndFollow, originate_wait.go) classifies the payer's answer and, when the
// payer pended, keeps a continuation and follows the decision with an inquiry. There is
// NO amendment leg on this tail (that is the UC-04/06 path).
// A single-shot submit is a FRESH submission and states no information change: nothing here
// stamps the Da Vinci PAS infoChanged item extension. That stamp existed only to make a payer
// gateway poll for a later answer, and a relayed decision needs no such signal — the payer's
// answer to this $submit is the answer, and a decision that comes later comes from the
// `Claim/$inquire` the caller performs (submitClaimAndFollow, originate_wait.go).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"strings"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// buildPASSubmitBundle assembles the single-shot conformant $submit Claim Bundle for the lean PAS
// tail. brPayer mirrors relaysReferencePayerBytes(OriginationProfile) (NOT the narrower
// targetsBrPayer — both provider-data over a live HTTP dial to br-payer AND demo through the
// in-process mirror of the SAME reference-payer bytes need the br-payer-resolvable wire shape;
// see relaysReferencePayerBytes's doc, gateway/engine/originate.go): when true the bundle carries the
// br-payer-resolvable forms (ContainedInsurer/AbsoluteRefs/PayerOrgEntry), exactly as the existing
// HomeOxygen path built them. InfoChanged is never set on either order type: a fresh submit
// states no information change, and the stamp only ever existed to make a payer gateway poll.
// Pulled out as a standalone func so the byte-parity guard can unit-test it directly.
// line is the routed contract line the bundle is BUILT at (select before build) —
// resolved by submitClaimAndResolve before this call, so the wire bytes and the
// routed token cannot disagree.
// providerJSON is the requesting provider's own record, selected from the order
// by the SHARED rule (shnsdk.SelectPASProvider) and read from the participant's
// own system. It rides the bundle as a resolvable entry that Claim.provider
// names, because a payer matches a later inquiry on the member id plus the
// ordering or rendering provider identifier.
func buildPASSubmitBundle(line string, brPayer bool, orderJSON, qrJSON, providerJSON, coverageJSON, insurerJSON []byte, memberSystem, patientRef, coverageRef, member, corr string, created time.Time, payer shnsdk.PayerIdentifier) ([]byte, error) {
	return buildAuthoredPASSubmit(line, shnsdk.ConformantClaimInputs{
		QR: qrJSON, SR: orderJSON, Provider: providerJSON, Coverage: coverageJSON, Insurer: insurerJSON, PatientRef: patientRef, CoverageRef: coverageRef, MemberID: member, MemberIDSystem: memberSystem,
		Corr: corr, Created: created,
		ContainedInsurer: brPayer,
		AbsoluteRefs:     brPayer,
		PayerOrgEntry:    brPayer, // payer Org as a resolvable PAS bundle entry (br-payer findInBundle)
		// The payer identity derives from the member's REAL Coverage (threaded in from the fresh
		// origination site), not a synthetic CMS literal (FR-G40).
		Payer: payer,
	})
}

// pasSubmission is what one PAS $submit leg produced: the correlation it ran
// under, the route it ran at, the bytes SHN sent, and the payer's own answer.
// The request bytes are kept because a continuation is derived from THEM — the
// identifiers and item trace numbers SHN actually put on the wire — never from a
// second reading of what the gateway meant to send.
type pasSubmission struct {
	corr       string
	route      legRoute
	bundleJSON []byte
	respJSON   []byte
}

// submitPASClaim builds and egress-validates the conformant Claim Bundle,
// originates the pas-claim leg and ingress-validates the payer's answer. It does
// NOT classify that answer: the determination is the payer's, and a pend is one
// of the payer's answers rather than a failure of this function. The caller
// (submitClaimAndFollow, originate_wait.go) classifies it, keeps a continuation
// when the payer pended, and does the FR-23 StoreAuthNumber.
//
// A dispatched DeviceRequest carries its actual supplier Organization before
// validation. The trailing err carries the raw error on the leg-failure path and
// is nil on every other path — it exists SOLELY so the caller can attempt
// relayOriginationError before the writeJSON(status,msg) fallback; a bare
// (status,msg) return would re-synthesize the error to a string and DROP the
// *RelayError sentinel (the %w audit).
func (g *Gateway) submitPASClaim(ctx context.Context, r *http.Request, pci string, orderJSON, supplierJSON []byte, source *dtrBuildSource, coverageJSON, insurerJSON []byte, patientRef, coverageRef, member, memberSystemOfFlow string, payer shnsdk.PayerIdentifier, recipient string) (pasSubmission, int, string, error) {
	out := pasSubmission{corr: g.cfg.CorrelationGen()}
	// Select-before-build: this tail used to let OriginateLeg select
	// INTERNALLY off an empty Content.ProfileID, which put the choice AFTER the bundle
	// was already built. The routed line now chooses the builder, so it is hoisted here
	// — a refusal returns the *RouteRefusalError through the existing err channel, which
	// the caller already relays as the legible 422.
	route, terr := g.selectLegLine(recipient, "pas-claim", out.corr)
	if terr != nil {
		return out, http.StatusBadGateway, terr.Error(), terr
	}
	out.route = route
	targetLine := shnsdk.LineOf(route.Token)
	qrJSON, err := buildPASAttachment(source, targetLine)
	if err != nil {
		return out, http.StatusBadGateway, "build PAS attachment failed", err
	}
	// The party this request comes from, read from the participant's own system
	// through the one selection the inquiry also uses. A refusal here is named:
	// nothing invents a provider so that a request can be sent.
	_, providerJSON, status, msg := g.pasProvider(ctx, orderJSON, supplierJSON)
	if status != 0 {
		return out, status, msg, nil
	}
	// The member, named as the participant's own system names them — the other
	// half of what a payer matches an inquiry on.
	memberSystem, status, msg := g.pasMemberSystem(memberSystemOfFlow, member)
	if status != 0 {
		return out, status, msg, nil
	}
	bundleJSON, err := buildPASSubmitBundle(route.BuildLine, relaysReferencePayerBytes(g.cfg.OriginationProfile), orderJSON, qrJSON, providerJSON, coverageJSON, insurerJSON, memberSystem, patientRef, coverageRef, member, out.corr, g.cfg.Clock(), payer)
	if err != nil {
		return out, http.StatusInternalServerError, "build bundle failed: " + err.Error(), nil
	}
	bundleJSON, err = retainPASSubmitSupplier(bundleJSON, supplierJSON)
	if err != nil {
		return out, http.StatusInternalServerError, "PAS supplier linkage failed", err
	}
	bundleJSON, err = g.completeAuthoredPASRequest(ctx, bundleJSON, qrJSON, orderJSON, coverageRef, relaysReferencePayerBytes(g.cfg.OriginationProfile))
	if err != nil {
		return out, http.StatusBadGateway, "PAS evidence linkage failed", err
	}
	bundleJSON, _, aerr := g.egressAdapt(route, bundleJSON, ExchangeIdentity{CorrelationID: out.corr, LegType: "pas-claim", Counterpart: recipient})
	if aerr != nil {
		return out, adaptFailureStatus(aerr), aerr.Error(), aerr
	}
	// This helper owns the pas-claim leg wholesale — a single-shot submit+resolve —
	// regardless of which caller's headline leg dispatched here, so it retags rather
	// than inherit: every check below is this leg, on its own minted correlation.
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: out.corr, Seam: "originate", Whose: "own",
	})
	if status, msg := g.validatePASAttachments(ctx, bundleJSON, targetLine, source != nil); status != 0 {
		return out, status, msg, nil
	}
	if status, msg := g.validateFHIREgressOrBridged(ctx, bundleJSON, "pa.pas", targetLine, len(route.Chain) > 0); status != 0 {
		return out, status, msg, nil
	}
	out.bundleJSON = bundleJSON
	// recipient is the payer HOLDER resolved from the member's real Coverage at the fresh origination
	// site (FR-G40) — no default; it replaced the deleted Config.CounterpartID here.
	respJSON, err := g.OriginateLeg(ctx, r, recipient, "pas-claim", pci, out.corr, "",
		Content{WorkstreamType: workstreamPA, ProfileID: route.Token, Route: routeInfoFor(route), Payload: sealRequest(relay.BuilderSDKPASSubmit, bundleJSON, "application/fhir+json")})
	if err != nil {
		// Return the RAW err (not just err.Error()) so the caller can relayOriginationError a framed
		// *RelayError verbatim; msg stays for the non-relay writeJSON fallback (byte-identical).
		return out, http.StatusBadGateway, err.Error(), err
	}
	out.respJSON = respJSON
	ctx = withFindingContext(ctx, findingContext{
		LegType: "pas-claim", CorrelationID: out.corr, Seam: "originate", Whose: "peer",
	})
	if status, msg := g.validateFHIRPayerIngress(ctx, respJSON, targetLine, "pa.pas", payer); status != 0 {
		return out, status, msg, nil
	}
	return out, 0, "", nil
}

// sameJSONResource reports whether two resources are the same record, compared
// as JSON rather than as bytes: the request has been assembled and re-marshalled
// by the time this runs, so the member ORDER of an object says nothing about
// whether the record changed. Both sides are re-marshalled from decoded values,
// which orders object members the same way on each.
func sameJSONResource(a, b []byte) bool {
	norm := func(v []byte) ([]byte, bool) {
		var any any
		if json.Unmarshal(v, &any) != nil {
			return nil, false
		}
		out, err := json.Marshal(any)
		return out, err == nil
	}
	x, okX := norm(a)
	y, okY := norm(b)
	return okX && okY && bytes.Equal(x, y)
}

// retainPASSubmitSupplier carries the actual dispatched supplier into the PAS
// request (FR-G28). Its identity must match the order; it is never synthesized.
func retainPASSubmitSupplier(body, supplier []byte) ([]byte, error) {
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(body, &bundle); err != nil {
		return nil, err
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(bundle["entry"], &entries); err != nil {
		return nil, err
	}
	// Performer is read as RAW bytes and interpreted only for the dispatched
	// DeviceRequest, whose performer is ONE reference. A ServiceRequest's
	// performer is a LIST, and decoding every entry through one object-shaped
	// probe made an ordinary authored order unreadable here — every entry passes
	// through this loop, not just the one this function is about.
	type identity struct {
		ResourceType string          `json:"resourceType"`
		ID           string          `json:"id"`
		Performer    json.RawMessage `json:"performer"`
	}
	performerRefOf := func(raw json.RawMessage) string {
		var one struct {
			Reference string `json:"reference"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &one) != nil {
			return ""
		}
		return one.Reference
	}
	var order *identity
	var orderURL string
	for _, entry := range entries {
		var resource identity
		if err := json.Unmarshal(entry["resource"], &resource); err != nil {
			return nil, err
		}
		if resource.ResourceType == "DeviceRequest" {
			if order != nil {
				return nil, fmt.Errorf("multiple dispatched orders")
			}
			order = &resource
			if err := json.Unmarshal(entry["fullUrl"], &orderURL); err != nil {
				return nil, err
			}
		}
	}
	if order == nil {
		if len(supplier) != 0 {
			return nil, fmt.Errorf("supplier without a dispatched order")
		}
		return body, nil
	}
	var org identity
	if err := json.Unmarshal(supplier, &org); err != nil {
		return nil, fmt.Errorf("missing or invalid supplier: %w", err)
	}
	if org.ResourceType != "Organization" || org.ID == "" {
		return nil, fmt.Errorf("supplier must identify an Organization")
	}
	relative := "Organization/" + org.ID
	ref := performerRefOf(order.Performer)
	parsed, err := url.Parse(ref)
	if err != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, fmt.Errorf("invalid supplier reference")
	}
	fullURL := ref
	if !parsed.IsAbs() {
		if ref != relative || order.ID == "" || !strings.HasSuffix(orderURL, "/DeviceRequest/"+order.ID) {
			return nil, fmt.Errorf("supplier identity does not match dispatched performer")
		}
		fullURL = strings.TrimSuffix(orderURL, "DeviceRequest/"+order.ID) + relative
	} else if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || !strings.HasSuffix(parsed.Path, "/"+relative) {
		return nil, fmt.Errorf("supplier identity does not match dispatched performer")
	}
	target, err := url.Parse(fullURL)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return nil, fmt.Errorf("invalid supplier fullUrl")
	}
	for _, entry := range entries {
		var resource identity
		if err := json.Unmarshal(entry["resource"], &resource); err != nil {
			return nil, err
		}
		var existingURL string
		if raw, ok := entry["fullUrl"]; ok {
			if err := json.Unmarshal(raw, &existingURL); err != nil {
				return nil, err
			}
		}
		if existingURL != fullURL && !(resource.ResourceType == org.ResourceType && resource.ID == org.ID) {
			continue
		}
		// The supplier is ALREADY in the request at exactly this identity, this
		// fullUrl and this content, because the Claim names the dispatched
		// performer as its requesting provider: that is ONE party, carried once.
		// Anything else under either of those identities is still a conflict —
		// two different suppliers claiming the same entry — and the CONTENT is
		// compared, not just the identity, so a second, different record cannot
		// be swallowed by the entry already there.
		if existingURL == fullURL && resource.ResourceType == org.ResourceType && resource.ID == org.ID &&
			sameJSONResource(entry["resource"], supplier) {
			return body, nil
		}
		return nil, fmt.Errorf("conflicting supplier entry")
	}
	encodedURL, _ := json.Marshal(fullURL)
	entries = append(entries, map[string]json.RawMessage{"fullUrl": encodedURL, "resource": supplier})
	bundle["entry"], err = json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return json.Marshal(bundle)
}
