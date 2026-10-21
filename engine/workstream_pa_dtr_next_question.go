// workstream_pa_dtr_next_question.go — the PA adapter's reads of an SDC
// $next-question exchange: the payer's answer (the delivered tree) and a framed
// input's subject. The DTR inbound responder reads both, and the provider-side
// adaptive drive (module1_dtr_adaptive.go) reads the answer.
package engine

import (
	"encoding/json"
	"fmt"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// parseNextQuestionResponse reads a $next-question answer: a Parameters whose
// questionnaire-response parameter carries the QuestionnaireResponse, whose contained
// Questionnaire holds the delivered tree. Anything else — a package Bundle, a Parameters
// without the parameter, a response with no contained Questionnaire — is an error.
func parseNextQuestionResponse(body []byte) (qrJSON []byte, deliveredItems []json.RawMessage, err error) {
	var top struct {
		ResourceType string `json:"resourceType"`
		Parameter    []struct {
			Name     string          `json:"name"`
			Resource json.RawMessage `json:"resource"`
		} `json:"parameter"`
	}
	if err := decodeMessage(body, &top); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	if top.ResourceType != "Parameters" {
		return nil, nil, fmt.Errorf("resourceType %q, want Parameters", top.ResourceType)
	}
	for _, p := range top.Parameter {
		if p.Name != "questionnaire-response" {
			continue
		}
		var qr struct {
			ResourceType string            `json:"resourceType"`
			Contained    []json.RawMessage `json:"contained"`
		}
		if err := decodeMessage(p.Resource, &qr); err != nil || qr.ResourceType != "QuestionnaireResponse" {
			return nil, nil, fmt.Errorf("questionnaire-response parameter is not a QuestionnaireResponse")
		}
		for _, c := range qr.Contained {
			var q struct {
				ResourceType string            `json:"resourceType"`
				Item         []json.RawMessage `json:"item"`
			}
			if err := json.Unmarshal(c, &q); err == nil && q.ResourceType == "Questionnaire" {
				return p.Resource, q.Item, nil
			}
		}
		return nil, nil, fmt.Errorf("questionnaire-response carries no contained Questionnaire")
	}
	return nil, nil, fmt.Errorf("no questionnaire-response parameter")
}

// framedNextQuestionSubject reads the subject of a framed $next-question
// input, the SDC operation's own body: a bare QuestionnaireResponse, or a
// Parameters with exactly one questionnaire-response parameter whose resource
// is a QuestionnaireResponse. ok is false for any other body. The subject is
// "" when the QuestionnaireResponse names none, which the bind refuses.
func framedNextQuestionSubject(body []byte) (subject string, ok bool) {
	d, err := relay.Doc(relay.NewBody(body, relay.OriginPeerFrame))
	if err != nil || d.Kind(d.Root()) != relay.KindObject {
		return "", false
	}
	qr := d.Root()
	switch docText(d, qr, "resourceType") {
	case "QuestionnaireResponse":
	case "Parameters":
		params, ok := d.Member(qr, "parameter")
		if !ok || d.Kind(params) != relay.KindArray {
			return "", false
		}
		found := 0
		for _, p := range d.Elems(params) {
			if docText(d, p, "name") != "questionnaire-response" {
				continue
			}
			found++
			if res, ok := d.Member(p, "resource"); ok {
				qr = res
			}
		}
		if found != 1 || qr == d.Root() || docText(d, qr, "resourceType") != "QuestionnaireResponse" {
			return "", false
		}
	default:
		return "", false
	}
	if s, ok := d.Member(qr, "subject"); ok {
		subject = docText(d, s, "reference")
	}
	return subject, true
}
