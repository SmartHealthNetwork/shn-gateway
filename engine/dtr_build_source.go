package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// dtrBuildSource retains private immutable construction inputs for two separately
// authored artifacts. It is never reconstructed by stripping a supplied QR.
type dtrBuildSource struct {
	raw, tree  []byte
	answers    map[string]shnsdk.Answer
	qc         shnsdk.QRContext
	amendments []dtrSourceAmendment
	qrID       string
}
type dtrSourceAmendment struct {
	item        []byte
	errorPrefix string
}

func newRawDTRBuildSource(raw, tree []byte, qc shnsdk.QRContext) *dtrBuildSource {
	return &dtrBuildSource{raw: append([]byte(nil), raw...), tree: append([]byte(nil), tree...), qc: qc}
}
func newAutomaticDTRBuildSource(tree []byte, answers map[string]shnsdk.Answer, qc shnsdk.QRContext) *dtrBuildSource {
	copied := make(map[string]shnsdk.Answer, len(answers))
	for key, value := range answers {
		if value.Boolean != nil {
			v := *value.Boolean
			value.Boolean = &v
		}
		if value.Integer != nil {
			v := *value.Integer
			value.Integer = &v
		}
		if value.String != nil {
			v := *value.String
			value.String = &v
		}
		if value.Coding != nil {
			v := *value.Coding
			value.Coding = &v
		}
		copied[key] = value
	}
	return &dtrBuildSource{tree: append([]byte(nil), tree...), answers: copied, qc: qc}
}
func (s *dtrBuildSource) withAmendment(item []byte, errorPrefix string) *dtrBuildSource {
	if s == nil {
		return nil
	}
	copy := *s
	copy.amendments = append(append([]dtrSourceAmendment(nil), s.amendments...), dtrSourceAmendment{append([]byte(nil), item...), errorPrefix})
	return &copy
}
func (s *dtrBuildSource) withQRID(id string) *dtrBuildSource {
	if s == nil {
		return nil
	}
	copy := *s
	copy.qrID = id
	return &copy
}
func (s *dtrBuildSource) buildAtLine(line string) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("DTR construction source unavailable")
	}
	var raw []byte
	var err error
	if s.answers != nil {
		raw, err = shnsdk.FillQuestionnaireFromAutoAnswersAtLine(line, s.tree, s.answers, s.qc)
	} else {
		raw = append([]byte(nil), s.raw...)
	}
	if err != nil {
		return nil, err
	}
	for _, event := range s.amendments {
		raw, err = shnsdk.AmendQRWithItemIn(raw, s.tree, event.item)
		if err != nil {
			if event.errorPrefix != "" {
				return nil, fmt.Errorf("%s: %w", event.errorPrefix, err)
			}
			return nil, err
		}
	}
	if s.qrID != "" {
		raw, err = shnsdk.SetQuestionnaireResponseID(raw, s.qrID)
		if err != nil {
			return nil, err
		}
	}
	return composeDTRContextAtLine(raw, line, s.qc)
}

// pairedDTRLine follows the package generation paired with the selected PAS
// target. This is construction, not a second route selection.
func pairedDTRLine(pasLine string) (string, error) {
	if _, ok := shnsdk.PASLineDef(pasLine); !ok {
		return "", fmt.Errorf("unsupported PAS attachment line")
	}
	if _, ok := shnsdk.DTRLineDef(pasLine); !ok {
		return "", fmt.Errorf("unsupported DTR attachment line")
	}
	return pasLine, nil
}
func buildPASAttachment(source *dtrBuildSource, pasLine string) ([]byte, error) {
	line, err := pairedDTRLine(pasLine)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, nil
	}
	return source.buildAtLine(line)
}

// validatePASAttachments reads every final embedded QR without re-marshaling it.
// Callers validate the unchanged whole PAS Bundle next, then send those bytes.
func (g *Gateway) validatePASAttachments(ctx context.Context, bundle []byte, line string, expected bool) (int, string) {
	if _, err := dtrQRProfile(line); err != nil {
		return http.StatusInternalServerError, "unsupported DTR attachment line"
	}
	var b struct {
		ResourceType string
		Entry        []struct{ Resource json.RawMessage }
	}
	if json.Unmarshal(bundle, &b) != nil || b.ResourceType != "Bundle" {
		return http.StatusBadGateway, "invalid PAS attachment container"
	}
	found := 0
	for _, entry := range b.Entry {
		var resource struct{ ResourceType string }
		if json.Unmarshal(entry.Resource, &resource) != nil {
			return http.StatusBadGateway, "invalid PAS attachment resource"
		}
		if resource.ResourceType != "QuestionnaireResponse" {
			continue
		}
		found++
		if status, msg := g.validateDTRQuestionnaireResponse(ctx, entry.Resource, line); status != 0 {
			return status, msg
		}
	}
	if expected && found == 0 {
		return http.StatusBadGateway, "expected PAS QuestionnaireResponse missing"
	}
	return 0, ""
}
