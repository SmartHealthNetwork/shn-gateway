package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// boundaryPreparationError retains a registered edit's refusal without exposing
// parser diagnostics at the HTTP boundary.
type boundaryPreparationError struct {
	*ingressContextError
	cause error
}

func (e *boundaryPreparationError) Unwrap() error { return e.cause }
func (e *boundaryPreparationError) As(target any) bool {
	if p, ok := target.(**ingressContextError); ok {
		*p = e.ingressContextError
		return true
	}
	return false
}

func hasCompletion(ex ExchangeContext, id, version string) bool {
	for _, c := range ex.boundary {
		if c.ID == id && c.Version == version {
			return true
		}
	}
	return false
}

// prepareBoundary prepares source-ready bytes for the source participant's
// boundary. Its context has already passed ingress authentication, grants and
// byte binding. It never obtains source data or checks clinical content.
func (g *Gateway) prepareBoundary(ctx context.Context, ex ExchangeContext, payload relay.Payload) (ExchangeContext, relay.Payload, error) {
	var none relay.Payload
	raw, err := relay.Transmit(payload, relay.Check(requestKey(ex.legType, true)))
	if err != nil {
		return ex, none, err
	}
	sum := sha256.Sum256(raw)
	if ex.bodySHA256 != hex.EncodeToString(sum[:]) || (ex.contentType != "" && ex.contentType != payload.ContentType()) {
		return ex, none, contextError(http.StatusForbidden, "boundary_evidence_invalid")
	}
	if ex.legType != "crd-order-select" && ex.legType != "crd-order-dispatch" {
		return ex, payload, nil
	}
	if hasCompletion(ex, string(relay.EditCDSCallbackStrip), "1") {
		g.recordBoundaryPreparation(ex, "connector", ex.clientID)
		return ex, payload, nil
	}
	body := relay.NewBody(raw, relay.OriginIngressRequest)
	doc, err := relay.Doc(body)
	if err != nil || doc.Kind(doc.Root()) != relay.KindObject {
		return ex, none, contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
	}
	ops := callbackRemovalOps(doc)
	if len(ops) != 0 {
		// An edit must keep its original source proof. Recasting an already edited
		// payload as a new source would erase that proof; assembly runs separately.
		if payload.Ownership() != relay.OwnershipRelayed {
			return ex, none, contextError(http.StatusServiceUnavailable, "adaptation_unavailable")
		}
		payload, err = relay.Apply(body, payload.ContentType(), relay.EditCDSCallbackStrip, ops...)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, relay.ErrSignedContent) {
				status = http.StatusUnprocessableEntity
			}
			return ex, none, &boundaryPreparationError{ingressContextError: &ingressContextError{status: status, code: "adaptation_unavailable"}, cause: err}
		}
		raw, err = relay.Transmit(payload, relay.Check(requestKey(ex.legType, true)))
		if err != nil {
			return ex, none, err
		}
		sum = sha256.Sum256(raw)
		ex.bodySHA256 = hex.EncodeToString(sum[:])
		// A connector declaration covers only the source bytes. The new bytes are
		// backed by Payload's registered edit proof, never by that declaration.
		ex.boundary = nil
	}
	g.recordBoundaryPreparation(ex, "gateway", ex.holder)
	return ex, payload, nil
}

// callbackRemovalOps is the closed addressing edit, independent of content
// conformance and source-data assembly.
func callbackRemovalOps(doc *relay.Document) []relay.Op {
	var ops []relay.Op
	for _, key := range []string{"fhirServer", "fhirAuthorization"} {
		if _, ok := doc.Member(doc.Root(), key); ok {
			ops = append(ops, doc.RemoveMember(doc.Root(), key))
		}
	}
	return ops
}

func (g *Gateway) recordBoundaryPreparation(ex ExchangeContext, source, preparer string) {
	if g.cfg.Observer == nil {
		return
	}
	detail, _ := json.Marshal(struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		Source   string `json:"source"`
		Preparer string `json:"preparer"`
	}{string(relay.EditCDSCallbackStrip), "1", source, preparer})
	g.observe(ObserverEvent{Kind: "boundary.prepared", LegType: ex.legType, Direction: "ingress", CorrelationID: ex.correlationID, Op: ex.operation, Detail: string(detail)})
}
