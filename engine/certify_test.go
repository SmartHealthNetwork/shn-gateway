package engine

import (
	"context"
	"encoding/json"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"strings"
	"testing"
	"time"
)

type certificationValidatorFunc func(context.Context, []byte, string) (shnsdk.Result, error)

func (f certificationValidatorFunc) Validate(c context.Context, b []byte, p string) (shnsdk.Result, error) {
	return f(c, b, p)
}
func (f certificationValidatorFunc) ValidateEvidence(c context.Context, b []byte, p string) (shnsdk.ValidationEvidence, error) {
	r, err := f(c, b, p)
	if err != nil {
		return unavailableValidatorEvidence(), err
	}
	ev := *syntheticEvidence()
	if !r.Valid {
		ev.Profile.State = shnsdk.ValidationInvalid
	}
	return ev, nil
}
func certificationGateway(t *testing.T, v shnsdk.Validator, observer func(ObserverEvent)) *Gateway {
	g := &Gateway{cfg: Config{ConformanceEnforcement: EnforcementObserve, Clock: time.Now, Validator: v, Observer: observer}}
	g.startCertification()
	t.Cleanup(func() { g.Close() })
	return g
}
func certificationSubmit(g *Gateway, id string) {
	in := observationInput(EnforcementObserve)
	in.Exchange.correlationID = id
	g.observeContent(in)
}
func certificationFlush(t *testing.T, g *Gateway) { observationFlush(t, g) }

func TestCertificationSpeciesProfiles(t *testing.T) {
	for _, row := range []struct{ raw, species, profile string }{
		{`{"resourceType":"Claim"}`, "Claim", "profile-claim"},
		{`{"resourceType":"ClaimResponse"}`, "ClaimResponse", "profile-claimresponse"},
		{`{"resourceType":"QuestionnaireResponse"}`, "QuestionnaireResponse", "dtr-questionnaireresponse"},
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Claim"}}]}`, "PASRequestBundle", "profile-pas-request-bundle"},
		{`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"ClaimResponse"}}]}`, "PASResponseBundle", "profile-pas-response-bundle"},
	} {
		t.Run(row.species, func(t *testing.T) {
			if got := detectSpecies([]byte(row.raw)); got != row.species {
				t.Fatalf("species %q", got)
			}
			for _, line := range []string{"2.0", "2.1", "2.2"} {
				p, ok := profileFor(row.species, line, "pas-claim")
				d, _ := shnsdk.PASLineDef(line)
				version := d.PackageVersion
				if row.species == "QuestionnaireResponse" {
					d, _ := shnsdk.DTRLineDef(line)
					version = d.PackageVersion
				}
				if !ok || !strings.HasSuffix(p, "/"+row.profile+"|"+version) {
					t.Fatalf("profile %q %v", p, ok)
				}
			}
		})
	}
	for _, raw := range []string{"", `null`, `[]`, `{`, `{"resourceType":"Parameters"}`, `{"resourceType":"Bundle"}`, `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire"}}]}`} {
		if got := detectSpecies([]byte(raw)); got != "" {
			t.Errorf("unsupported %q -> %q", raw, got)
		}
	}
	for _, row := range [][3]string{{"Claim", "9.9", "pas-claim"}, {"x", "2.0", "pas-claim"}, {"Claim", "2.0", "other"}} {
		if p, ok := profileFor(row[0], row[1], row[2]); ok || p != "" {
			t.Fatal(row, p, ok)
		}
	}
	p, _ := profileFor("Claim", "2.1", "pas-claim-update")
	if !strings.Contains(p, "/profile-claim-update|") {
		t.Fatal(p)
	}
	p, _ = profileFor("Claim", "2.1", "")
	if strings.Contains(p, "update") {
		t.Fatal(p)
	}
}

// Fixture-only historical species helper; runtime observation never infers a line.
// detectSpecies identifies only supported resource envelopes. A PAS response
// may also carry its original Claim; the response resource determines species.
func detectSpecies(payload []byte) string {
	var r struct {
		ResourceType string
		Entry        []struct{ Resource struct{ ResourceType string } }
	}
	if json.Unmarshal(payload, &r) != nil {
		return ""
	}
	switch r.ResourceType {
	case "Claim", "ClaimResponse", "QuestionnaireResponse":
		return r.ResourceType
	case "Bundle":
		request := false
		for _, e := range r.Entry {
			if e.Resource.ResourceType == "ClaimResponse" {
				return "PASResponseBundle"
			}
			request = request || e.Resource.ResourceType == "Claim"
		}
		if request {
			return "PASRequestBundle"
		}
	}
	return ""
}
