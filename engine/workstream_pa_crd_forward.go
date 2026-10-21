package engine

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

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

// WithCRDDispatchService names the partner's CDS service for the crd-order-dispatch
// leg. Empty (the default) selects the listed service for the order-dispatch hook.
func WithCRDDispatchService(serviceID string) NativeOption {
	return func(n *nativeResponder) { n.crdDispatchServiceID = serviceID }
}

// forwardCRD sends a CDS Hooks request to the partner service chosen by its hook:
// exactly as the network carried it, or with only the payer identity of its
// prefetch coverage mapped (when that mapping is configured). The hook and every
// other byte are never changed. The partner's answer is relayed exactly, with its
// own media type, once it passes the CDS Hooks response rules at the served CRD
// line; an answer that breaks one is refused (502) rather than repaired, and a
// non-2xx answer is relayed as the partner's error.
func (n *nativeResponder) forwardCRD(ctx context.Context, contract, leg string, in relay.Body) (LegResult, error) {
	serviceID, refused, err := n.selectCRDService(ctx, leg, in)
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	request, refused, err := n.applyPayorEdgeToCRDRequest(in)
	if err != nil || refused.Status != 0 {
		return refused, err
	}
	up, bad, err := n.post(ctx, n.cdsBaseURL, "/cds-services/"+serviceID, request, leg, "CRD")
	if err != nil {
		return LegResult{}, err // no-response fault → engine 500 → "hub routing failed"
	}
	if bad.Status != 0 {
		return bad, nil // upstream non-2xx → relayable LegResult (Response carries the body)
	}
	if refused := certifyCDSHooksAnswer(ctx, n.module(), n.emitFinding, up.raw, answerLineOr(ctx, contract), "own"); refused.Status != 0 {
		return refused, nil
	}
	// A CDS Hooks answer is JSON: one sent without a media type is carried as
	// application/json.
	ct := up.declared
	if ct == "" {
		ct = "application/json"
	}
	return LegResult{Response: relay.Exact(up.body, ct), AnswerStatus: up.status}, nil
}

// certifyCDSHooksAnswer applies the CDS Hooks response rules (and, at a CRD
// line, the CRD card rules) to a participant's answer. It never changes the
// answer. At none no rule runs. At every other level each violation is
// recorded as a finding; whether a refusing violation actually refuses is the
// receiving participant's choice, which module 3 holds. A SHOULD-level
// finding is logged and the answer passes, as before.
//
// whose classifies WHOSE SYSTEM the certified bytes came from, for the
// finding: "own" when the bytes are this gateway's own backend answering
// (forwardCRD, certifying before it ever relays to a peer) or "peer" when
// they are a peer's answer received over the network (crdAnswerOutcome).
// This is independent of the refusal MESSAGE, which always names the
// participant the requester asked ("payer") regardless of which side is
// doing the certifying — a requester reading a refusal never sees "own".
//
// emit may be nil: a responder the engine never wired still certifies and
// still refuses at strict, it simply records nothing.
func certifyCDSHooksAnswer(ctx context.Context, mod conformanceModule, emit func(ConformanceFinding), body []byte, line, whose string) LegResult {
	// A repeated member name is message integrity (RuleDuplicateKey), refused at
	// every level: an answer that can be read two ways cannot be carried
	// faithfully.
	// The refusal names the repeated member and where, so the payer can fix it.
	if dup, ok := repeatedMember(body); ok && guardDefect(ctx, mod, emit, KindNetwork, RuleDuplicateKey, VerdictInvalid, body) {
		return LegResult{Status: http.StatusBadGateway, Message: "payer CRD response repeats " + dup + ": it can be read two ways, so it is not carried"}
	}
	if !mod.RunsKind(KindCDSEnvelope) {
		return LegResult{}
	}
	emit = countingEmit(ctx, emit)
	violations := shnsdk.CheckCDSHooksResponse(body, line)
	if len(violations) == 0 {
		return LegResult{}
	}
	fc := findingContextFrom(ctx)
	var refusing, advisory []string
	var refuse bool
	for _, v := range violations {
		if !mod.Runs(KindCDSEnvelope, v.Rule) {
			continue
		}
		desc := v.Rule
		if v.Path != "" {
			desc += " at " + v.Path
		}
		if v.Severity != shnsdk.SeverityError {
			// One finding per violation (§5), advisory included: a SHOULD-level
			// finding never refuses, but it is still something the participant
			// should be able to read back.
			advisory = append(advisory, desc)
			if emit != nil {
				emit(ConformanceFinding{
					Kind: string(KindCDSEnvelope), Direction: "validate",
					LegType: fc.LegType, CorrelationID: fc.CorrelationID, Seam: fc.Seam,
					Whose: whose, Line: line,
					Level: mod.Level().String(), Decision: Record.String(),
					Rule: v.Rule, Path: v.Path,
					PayloadSHA256: sha256hex(body),
				})
			}
			continue
		}
		decision := mod.Decide(KindCDSEnvelope, v.Rule, VerdictInvalid)
		if decision == Refuse {
			refuse = true
			refusing = append(refusing, desc)
		}
		if emit != nil {
			emit(ConformanceFinding{
				Kind: string(KindCDSEnvelope), Direction: "validate",
				LegType: fc.LegType, CorrelationID: fc.CorrelationID, Seam: fc.Seam,
				Whose: whose, Line: line,
				Level: mod.Level().String(), Decision: decision.String(),
				Rule: v.Rule, Path: v.Path,
				PayloadSHA256: sha256hex(body),
			})
		}
	}
	if len(advisory) > 0 {
		log.Printf("gateway: payer CRD response: CDS Hooks recommendations not met: %s", strings.Join(advisory, "; "))
	}
	if !refuse {
		return LegResult{}
	}
	const shown = 5
	if len(refusing) > shown {
		refusing = append(refusing[:shown], fmt.Sprintf("and %d more", len(refusing)-shown))
	}
	return LegResult{
		Status:  http.StatusBadGateway,
		Message: "payer CRD response is not a valid CDS Hooks response: " + strings.Join(refusing, "; "),
	}
}

// applyPayorEdgeToCRDRequest is the CRD legs' payer-edge identity mapping
// (payoredge.go): maps the payer identity of every Coverage in the CDS Hooks request's
// prefetch.coverage (a bare Coverage or a Bundle) to n.payorEdgeBackend, ONLY when they
// name an identity this gateway owns (ownPayerIdentities: the identities this holder
// publishes on the network feed, union the configured one). Unconfigured, the request is
// sent exactly. Configured, a request that carries no Coverage at all has no payor to map
// and is sent exactly too: the network routed it here, and this payer's own system
// answers it. That is a request whose prefetch is absent or null, whose prefetch.coverage
// is absent or null, or whose prefetch.coverage Bundle's entry is absent, null or only
// OperationOutcomes,
// and no Coverage anywhere else in the body (relaylocate.go, carriesNoCoverage). A
// Coverage naming no readable payor, or another payer, is refused, and so is a request
// whose only Coverage is outside prefetch.coverage.
func (n *nativeResponder) applyPayorEdgeToCRDRequest(in relay.Body) (relay.Payload, LegResult, error) {
	return n.payorEdgeRequest(in, payorEdgeCRDRequest, "application/json", nil)
}
