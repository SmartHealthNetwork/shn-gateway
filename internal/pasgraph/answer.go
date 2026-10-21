package pasgraph

import (
	"errors"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

// ErrDecision is the refusal of an answer whose graph reads but whose payer
// decision this rule does not read.
var ErrDecision = errors.New("invalid native PAS response decision")

// Facts are what a PAS $submit or amendment answer states, read by
// InspectAnswerAt: the payer's decision and the three names of its
// ClaimResponse, each exactly as the payer stated it and none minted here.
type Facts struct {
	Decision                string
	ClaimResponseID         string
	ClaimResponseIdentity   string
	ClaimResponseIdentifier string
}

// InspectAnswerAt reads a PAS $submit or amendment answer exchanged at IG
// line by the response graph rule at that line (ValidateAnswerAt), then the
// payer's explicit decision. A refusal keeps the graph's own cause and text
// (RefusalCause), or is ErrDecision.
func InspectAnswerAt(body []byte, line string) (Facts, error) {
	// A repeated member name, as the gateway's check finds one (jsonmember.Repeated,
	// which also counts names equal but for case), is no one reading of the
	// answer; the gateway names it in its own text.
	if _, ok := jsonmember.Repeated(body); ok {
		return Facts{}, Refuse(RefusalUndecodable)
	}
	g, err := Read(body)
	if err != nil {
		return Facts{}, err
	}
	if err = g.ValidateAnswerAt(line); err != nil {
		return Facts{}, err
	}
	pended, _, err := shnsdk.ParsePendedResponseDetail(body)
	if err != nil {
		return Facts{}, ErrDecision
	}
	decision := "pending"
	if !pended {
		result, err := shnsdk.ParseClaimResponse(body)
		if err != nil {
			return Facts{}, ErrDecision
		}
		decision = result.Outcome
		if result.Partial {
			decision = "partial"
		}
	}
	id, _ := g.Response.Resource["id"].(string)
	return Facts{
		Decision:                decision,
		ClaimResponseID:         id,
		ClaimResponseIdentity:   g.Response.FullURL,
		ClaimResponseIdentifier: firstIdentifierKey(g.Response.Resource),
	}, nil
}

// RefusalCause is the cause a response graph refusal names, one of the
// Refusal* constants.
func RefusalCause(err error) string {
	var c *CauseError
	if errors.As(err, &c) {
		return c.cause
	}
	if RefusalOf(err) != nil {
		return RefusalReference
	}
	return RefusalUndecodable
}

// firstIdentifierKey is the resource's first identifier as "system|value" (or
// the bare value when it has no system), "" when it states none usable.
func firstIdentifierKey(r map[string]any) string {
	list, _ := r["identifier"].([]any)
	for _, v := range list {
		ident, _ := v.(map[string]any)
		system, _ := ident["system"].(string)
		value, _ := ident["value"].(string)
		if value == "" {
			continue
		}
		if system == "" {
			return value
		}
		return system + "|" + value
	}
	return ""
}
