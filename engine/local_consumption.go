package engine

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ConsumptionOutcome describes a specific local consumer's ability to use a
// delivered reply. Available is set only after that consumer's parsing and
// required semantic/source guards succeed; delivery alone cannot establish it.
type ConsumptionOutcome struct {
	State   string              `json:"state"`
	Code    string              `json:"code,omitempty"`
	Refusal *ConsumptionRefusal `json:"refusal,omitempty"`
}

// ConsumptionRefusal preserves closed local enforcement metadata separately
// from the participant's application status. It contains no validator diagnostics.
type ConsumptionRefusal struct {
	Status    int    `json:"status"`
	Category  string `json:"category"`
	Rule      string `json:"rule"`
	Gateway   string `json:"gateway"`
	Level     string `json:"level"`
	Direction string `json:"direction"`
}

func localConsumptionRefusal(e *conformanceError) *ConsumptionRefusal {
	safe := e.safeRefusal()
	return &ConsumptionRefusal{Status: safe.status, Category: safe.Category, Rule: safe.Rule, Gateway: safe.Gateway, Level: safe.Level, Direction: safe.Direction}
}

// writeReceivedRefusal applies only to explicit workflow consumption after a
// received reply passed the ownership and size checks. Native refusals and
// pre-exchange failures retain their existing response contract.
func (d PASDecision) writeReceivedRefusal(w http.ResponseWriter) bool {
	if d.ReplyView == nil || d.Consumption.Refusal == nil {
		return false
	}
	refusal := d.Consumption.Refusal
	writeConsumptionFailure(w, refusal.Status, refusal.Category+": "+refusal.Rule, d.ReplyView, d.Consumption)
	return true
}

// writePASConsumptionFailure preserves the last received PAS answer at an
// explicit workflow error surface. It never turns a failed local action into a
// clinical decision or changes an upstream application error.
func (g *Gateway) writePASConsumptionFailure(w http.ResponseWriter, d PASDecision, status int, message string, err error) {
	var ce *conformanceError
	if d.ReplyView != nil && errors.As(err, &ce) {
		d.Consumption = unavailableConsumption("response_enforcement_failed")
		d.Consumption.Refusal = localConsumptionRefusal(ce)
	}
	if d.writeReceivedRefusal(w) {
		return
	}
	var backend *RelayError
	if d.ReplyView == nil || errors.As(err, &backend) {
		if g.relayOriginationError(w, err) {
			return
		}
	} else {
		var ice *ingressContextError
		var route *RouteRefusalError
		switch {
		case errors.As(err, &ice):
			status, message = ice.status, ice.code
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Cache-Control", "no-store")
			}
		case isOwnershipFault(err):
			status, message = http.StatusInternalServerError, errOwnershipFault
		case errors.As(err, &route):
			status, message = http.StatusUnprocessableEntity, route.Error()
		case errors.Is(err, errHubTimeout):
			status, message = http.StatusGatewayTimeout, err.Error()
		}
	}
	if d.Consumption.State == "available" {
		d.Consumption = unavailableConsumption("local_action_unavailable")
	}
	writeConsumptionFailure(w, status, message, d.ReplyView, d.Consumption)
}

func unavailableConsumption(code string) ConsumptionOutcome {
	return ConsumptionOutcome{State: "unavailable", Code: code}
}

// ApplicationReplyView is the bounded wire representation exposed to the
// workflow caller that received the exchange. BodyBase64 preserves arbitrary
// application bytes, including malformed JSON, without serializing ownership
// internals. Leg and correlation identify the particular exchange in a workflow.
type ApplicationReplyView struct {
	Leg             string `json:"leg"`
	CorrelationID   string `json:"correlationId"`
	Status          int    `json:"status"`
	ContentType     string `json:"contentType"`
	DeclaredVersion string `json:"declaredVersion,omitempty"`
	VersionSource   string `json:"versionSource,omitempty"`
	BodyBase64      string `json:"bodyBase64"`
}

func (a ApplicationReply) view(leg, correlation string) (*ApplicationReplyView, error) {
	if a.Payload.Ownership() == 0 {
		return nil, nil
	}
	body, err := a.bytes(leg)
	if err != nil {
		return nil, err
	}
	if len(body) > shnsdk.MaxResponseBytes {
		return nil, fmt.Errorf("application reply exceeds workflow response bound")
	}
	return &ApplicationReplyView{Leg: leg, CorrelationID: correlation, Status: a.Status, ContentType: a.Payload.ContentType(), DeclaredVersion: a.DeclaredVersion, VersionSource: a.VersionSource, BodyBase64: base64.StdEncoding.EncodeToString(body)}, nil
}

// ConsumptionAttempt retains the received reply and the outcome of its explicit
// local consumer. A workflow retains only its latest received attempt.
type ConsumptionAttempt struct {
	reply            ApplicationReply
	ApplicationReply *ApplicationReplyView `json:"applicationReply"`
	Consumption      ConsumptionOutcome    `json:"consumption"`
}

// writeConsumptionFailure is called only by explicit workflow consumers. It
// retains a received reply while keeping the local failure distinct from the
// participant's application status. It neither parses content nor runs actions.
func writeConsumptionFailure(w http.ResponseWriter, status int, message string, reply *ApplicationReplyView, outcome ConsumptionOutcome) {
	if reply == nil {
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	writeJSON(w, status, struct {
		Error            string                `json:"error"`
		ApplicationReply *ApplicationReplyView `json:"applicationReply"`
		Consumption      ConsumptionOutcome    `json:"consumption"`
	}{message, reply, outcome})
}

// receivedDecision retains the submission reply even if its local consumer
// cannot read a decision. It makes no clinical claim from transport status.
func (s pasSubmission) receivedDecision() PASDecision {
	leg := s.leg
	if leg == "" {
		leg = "pas-claim"
	}
	view, err := s.reply.view(leg, s.corr)
	outcome := s.consumption
	if err != nil {
		outcome = unavailableConsumption("application_reply_unavailable")
	}
	return PASDecision{ApplicationReply: s.reply, ReplyLeg: leg, ReplyCorrelation: s.corr, ReplyView: view, Consumption: outcome, PayerResponse: s.respJSON}
}

// receive retains transport evidence without authorizing or performing any local
// action. A failure before a later exchange preserves the earlier received reply.
func (a *ConsumptionAttempt) receive(reply ApplicationReply, leg, correlation string, err error) ([]byte, error) {
	view, viewErr := reply.view(leg, correlation)
	if reply.Payload.Ownership() != 0 {
		a.reply, a.ApplicationReply = reply, view
	}
	a.Consumption = unavailableConsumption("local_consumption_unavailable")
	if err == nil {
		err = viewErr
	}
	body, err := reply.legacy(leg, err)
	var ce *conformanceError
	if errors.As(err, &ce) {
		a.Consumption = unavailableConsumption("response_enforcement_failed")
		a.Consumption.Refusal = localConsumptionRefusal(ce)
	}
	return body, err
}

func (g *Gateway) writeAttemptFailure(w http.ResponseWriter, a ConsumptionAttempt, status int, message string, err error) {
	g.writePASConsumptionFailure(w, PASDecision{ReplyView: a.ApplicationReply, Consumption: a.Consumption}, status, message, err)
}

func (g *Gateway) writeAttemptSoRFailure(w http.ResponseWriter, a ConsumptionAttempt, err error) bool {
	if err == nil {
		return false
	}
	status, message := SoRFailureResponse(err)
	g.writeAttemptFailure(w, a, status, message, nil)
	return true
}

// withPriorAttempt carries the earlier exchange only when the later dispatch
// received no reply. It never substitutes earlier bytes for a received answer.
func (d PASDecision) withPriorAttempt(a ConsumptionAttempt) PASDecision {
	if d.ApplicationReply.Payload.Ownership() == 0 && d.ReplyView == nil && a.ApplicationReply != nil {
		d.ApplicationReply, d.ReplyView = a.reply, a.ApplicationReply
		d.ReplyLeg, d.ReplyCorrelation = a.ApplicationReply.Leg, a.ApplicationReply.CorrelationID
		d.Consumption = unavailableConsumption("next_exchange_unavailable")
	}
	return d
}

func priorAttempt(attempts []ConsumptionAttempt) ConsumptionAttempt {
	if len(attempts) == 0 {
		return ConsumptionAttempt{}
	}
	a := attempts[0]
	a.Consumption = unavailableConsumption("local_action_unavailable")
	return a
}
