package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/exchangecontext"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// Only an absent header permits selection of the unsigned metadata adapter.
// Missing fields inside a presented assertion never permit that fallback.
var errIngressContextAbsent = errors.New("ingress exchange context absent")

type ingressContextError struct {
	status int
	code   string
	absent bool
}

func (e *ingressContextError) Error() string { return e.code }
func (e *ingressContextError) Is(target error) bool {
	return e.absent && target == errIngressContextAbsent
}
func contextError(status int, code string) error {
	return &ingressContextError{status: status, code: code}
}

// contextLeg maps application operation grants to existing catalog legs. DTR
// sub-operations share the catalog's dtr-questionnaire-fetch authority operation.
func contextLeg(operation string) string {
	switch operation {
	case shnsdk.FrameOperationQuestionnairePackage, shnsdk.FrameOperationNextQuestion:
		return "dtr-questionnaire-fetch"
	case "pas-submit":
		return "pas-claim"
	case "pas-update-submit":
		return "pas-claim-update"
	case "pas-inquire":
		return "pas-claim-inquire"
	case "crd-order-select", "crd-order-dispatch":
		return operation
	default:
		return ""
	}
}

// ValidateIngressContextGrants checks participant onboarding facts. These grants
// permit a registered connector to assert context or completed boundary edits;
// they never disable conformance checks or confer network authorization.
func ValidateIngressContextGrants(operations, preparations []string) error {
	for _, op := range operations {
		leg := contextLeg(op)
		if _, ok := paCatalog[leg]; !ok {
			return fmt.Errorf("unknown context operation %q", op)
		}
	}
	for _, id := range preparations {
		if id != "E-01" {
			return fmt.Errorf("unknown boundary preparation %q", id)
		}
	}
	return nil
}

func validCRDHook(leg, hook string) bool {
	for _, svc := range cdsIngressServices {
		if svc.Leg == leg && svc.Hook == hook {
			return true
		}
	}
	return false
}

// resolveIngressContext is the signed path only. Callers may select a legacy
// metadata adapter exclusively on errIngressContextAbsent. No system-of-record
// reads, payload parsing or dispatch occur here, including at none and observe.
func (g *Gateway) resolveIngressContext(ctx context.Context, r *http.Request, principal IngressPrincipal, body []byte) (ExchangeContext, error) {
	_ = ctx
	values, present := r.Header[http.CanonicalHeaderKey(exchangecontext.Header)]
	if !present {
		return ExchangeContext{}, &ingressContextError{status: 400, code: "context_missing", absent: true}
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > exchangecontext.MaxTokenBytes {
		return ExchangeContext{}, contextError(401, "context_invalid")
	}
	if g.ingressAuth == nil || principal.ClientID == "" {
		return ExchangeContext{}, contextError(401, "context_invalid")
	}
	s := g.ingressAuth
	reg, ok := s.clients[principal.ClientID]
	pub := s.pubKeys[principal.ClientID]
	if !ok || pub == nil {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	// Peek only to form expected application addressing; no claim is trusted until
	// Verify authenticates it. The route/catalog checks below independently bind it.
	var peek exchangecontext.Claims
	if _, _, err := jwt.NewParser().ParseUnverified(values[0], &peek); err != nil {
		return ExchangeContext{}, contextError(401, "context_invalid")
	}
	binding := exchangecontext.Binding{ClientID: principal.ClientID, Holder: g.cfg.HolderID, Recipient: peek.Recipient, Leg: peek.Leg, Operation: peek.Operation, ContentType: r.Header.Get("Content-Type"), Audience: s.baseURL + r.URL.EscapedPath()}
	now := s.now()
	claims, err := exchangecontext.Verify(values[0], body, binding, reg.Alg, pub, now)
	if err != nil {
		status := 403
		if errors.Is(err, exchangecontext.ErrAuthentication) {
			status = 401
		}
		return ExchangeContext{}, contextError(status, "context_invalid")
	}
	if len(claims.ID) > MaxReplayKeyBytes || !slices.Contains(reg.ContextOperations, claims.Operation) || contextLeg(claims.Operation) != claims.Leg {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	if _, ok := paCatalog[claims.Leg]; !ok {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	if _, ok := g.cfg.Reg.Lookup(claims.Recipient); !ok {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	if r.Method != http.MethodPost {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	routeOK := false
	switch r.URL.Path {
	case "/Claim/$submit":
		routeOK = claims.Leg == "pas-claim" || claims.Leg == "pas-claim-update"
	case "/Claim/$inquire":
		routeOK = claims.Leg == "pas-claim-inquire"
	case "/Questionnaire/$questionnaire-package":
		routeOK = claims.Operation == shnsdk.FrameOperationQuestionnairePackage
	default:
		if strings.HasPrefix(r.URL.Path, "/cds-services/") {
			svc, _, known := g.advertisedCDSServiceByID(strings.TrimPrefix(r.URL.Path, "/cds-services/"))
			if known && claims.Leg == svc.Leg && claims.CRDHook == "" {
				return ExchangeContext{}, contextError(400, "context_missing")
			}
			routeOK = known && claims.Leg == svc.Leg && claims.CRDHook == svc.Hook
		}
	}
	if !routeOK || (claims.CRDHook != "" && !validCRDHook(claims.Leg, claims.CRDHook)) {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	boundary := make([]BoundaryCompletion, 0, len(claims.Completed))
	seen := map[string]bool{}
	for _, c := range claims.Completed {
		if c.ID != "E-01" || c.Version != "1" || !slices.Contains(reg.BoundaryPreparations, c.ID) || seen[c.ID] || !validCRDHook(claims.Leg, claims.CRDHook) {
			return ExchangeContext{}, contextError(403, "boundary_evidence_invalid")
		}
		seen[c.ID] = true
		boundary = append(boundary, BoundaryCompletion{c.ID, c.Version})
	}
	if s.replay == nil {
		return ExchangeContext{}, contextError(503, "context_invalid")
	}
	replay, err := s.replay.CheckAndRecord(ReplayScopeIngressContext, principal.ClientID, claims.ID, now, claims.ExpiresAt.Time)
	if err != nil {
		s.noteStoreError(storeErrReplay)
		return ExchangeContext{}, contextError(503, "context_invalid")
	}
	if replay {
		return ExchangeContext{}, contextError(403, "context_invalid")
	}
	versionSource := ""
	if claims.ContractVersion != "" {
		versionSource = "producer"
	}
	return ExchangeContext{holder: claims.Holder, clientID: principal.ClientID, recipient: claims.Recipient, legType: claims.Leg, subjectPCI: claims.SubjectPCI, correlationID: claims.CorrelationID, custodian: claims.Custodian, consentRef: claims.ConsentRef, operation: claims.Operation, contractVersion: claims.ContractVersion, versionSource: versionSource, contentType: claims.ContentType, crdHook: claims.CRDHook, bodySHA256: claims.BodySHA256, boundary: boundary, policy: g.policy()}, nil
}
