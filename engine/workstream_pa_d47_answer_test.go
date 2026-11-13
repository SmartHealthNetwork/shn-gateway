//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS profiles a payer's answer that declares none is judged against at
// structural and strict (owner decision D47), without a version.
const (
	pasResponseBundleProfile = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-response-bundle"
	pasClaimResponseProfile  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse"
	pasInquiryBundleProfile  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-response-bundle"
	pasInquiryAnswerProfile  = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claiminquiryresponse"
)

// Undeclared PAS answers: a bare ClaimResponse and a response Bundle, neither
// declaring a profile.
const (
	undeclaredClaimResponse  = `{"resourceType":"ClaimResponse","id":"cr","status":"active","use":"preauthorization","outcome":"complete"}`
	undeclaredResponseBundle = `{"resourceType":"Bundle","type":"collection","entry":[{"fullUrl":"urn:uuid:5f1c2a64-0000-4000-8000-0000000000c1","resource":{"resourceType":"ClaimResponse","id":"cr","status":"active"}}]}`
)

// profileLane is one line's validator lane: it records the profile each call
// asks for, and answers verdict for a call that names none and atProfile for
// one that names a profile.
type profileLane struct {
	line               string
	verdict, atProfile scriptedVerdict
	mu                 *sync.Mutex
	asked              *[]string
}

func (l profileLane) Validate(_ context.Context, _ []byte, profile string) (shnsdk.Result, error) {
	l.mu.Lock()
	*l.asked = append(*l.asked, l.line+" "+profile)
	l.mu.Unlock()
	if profile != "" {
		return scriptedResult(l.atProfile)
	}
	return scriptedResult(l.verdict)
}

// profileLaneGateway is payerIngressGateway with a profileLane for each line in
// atProfile, valid when asked for no profile; asked collects every call's
// "<line> <profile>".
func profileLaneGateway(t *testing.T, level ConformanceEnforcement, atProfile map[string]scriptedVerdict) (*Gateway, *[]string, *[]ConformanceFinding) {
	t.Helper()
	g, _, findings := payerIngressGateway(t, "demo", level, nil)
	var mu sync.Mutex
	var asked []string
	lanes := map[string]shnsdk.Validator{}
	for line, v := range atProfile {
		lanes[line] = profileLane{line: line, verdict: scriptValid, atProfile: v, mu: &mu, asked: &asked}
	}
	g.cfg.Validator = lanes["2.0"]
	g.cfg.ValidatorsByContractLine = contractLanes(lanes)
	return g, &asked, findings
}

// A payer's PAS answer that declares no profile is judged against its line's
// own profile for what it is at structural and strict: a bare ClaimResponse
// against profile-claimresponse, a response Bundle against the PAS response
// Bundle profile, and an inquiry's against the inquiry ones, each without a
// version so every candidate line's lane resolves its own. At observe it is
// judged as before (its base definition), at none not at all; an answer that
// declares a profile, and a DTR answer, are judged as before at every level.
func TestPayerAnswerIngress_UndeclaredAnswerIsJudgedAtItsLineProfile(t *testing.T) {
	declared := `{"resourceType":"ClaimResponse","id":"cr","meta":{"profile":["` + pasClaimResponseProfile + `"]},"status":"active"}`
	for _, c := range []struct {
		name, leg, contract, body, want string
	}{
		{"a bare ClaimResponse", "pas-claim", "pa.pas", undeclaredClaimResponse, pasClaimResponseProfile},
		{"a response Bundle", "pas-claim-update", "pa.pas", undeclaredResponseBundle, pasResponseBundleProfile},
		{"an inquiry's bare ClaimResponse", "pas-claim-inquire", "pa.pas", undeclaredClaimResponse, pasInquiryAnswerProfile},
		{"an inquiry's response Bundle", "pas-claim-inquire", "pa.pas", undeclaredResponseBundle, pasInquiryBundleProfile},
		{"an answer that declares its profile", "pas-claim", "pa.pas", declared, ""},
		// A DTR answer's profile is its check's own: the PAS line profile
		// never reaches it, whatever the answer holds (a DTR leg names no
		// PAS profile, and the contract is not PAS's).
		{"a DTR answer", "dtr-questionnaire-fetch", "pa.dtr", undeclaredClaimResponse, ""},
	} {
		for _, level := range allLevels {
			t.Run(c.name+"/"+level.String(), func(t *testing.T) {
				g, asked, _ := profileLaneGateway(t, level, map[string]scriptedVerdict{"2.0": scriptValid, "2.1": scriptValid, "2.2": scriptValid})
				if status, msg := drainedPayerIngress(g, answerCtx(c.leg), []byte(c.body), "2.0", c.contract, partner); status != 0 {
					t.Fatalf("a valid answer is carried, got %d %s", status, msg)
				}
				if level == EnforcementNone {
					if len(*asked) != 0 {
						t.Fatalf("at none no validator is called, got %v", *asked)
					}
					return
				}
				want := c.want
				if level == EnforcementObserve {
					want = ""
				}
				if len(*asked) == 0 {
					t.Fatal("no lane was asked")
				}
				for _, a := range *asked {
					if _, profile, _ := strings.Cut(a, " "); profile != want {
						t.Fatalf("a lane was asked %q, want profile %q", a, want)
					}
				}
			})
		}
	}
}

// The answer is valid when any candidate line's profile accepts it: at strict,
// an undeclared answer the 2.0 lane's profile refuses and the 2.1 lane's
// accepts is carried, as is one only the second of two lanes accepts, and one
// no line accepts is refused, at structural too, as conformance at the
// participant's own level.
func TestPayerAnswerIngress_UndeclaredAnswerIsValidAtAnyLineThatAccepts(t *testing.T) {
	for _, c := range []struct {
		name     string
		verdicts map[string]scriptedVerdict
		level    ConformanceEnforcement
		refused  bool
		line     string
	}{
		{"one of three lines accepts", map[string]scriptedVerdict{"2.0": scriptStructural, "2.1": scriptValid, "2.2": scriptStructural}, EnforcementStrict, false, "2.1"},
		{"only the second of two lines accepts", map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptValid}, EnforcementStrict, false, "2.2"},
		{"no line accepts, at strict", map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptStructural}, EnforcementStrict, true, ""},
		{"no line accepts, at structural", map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptStructural}, EnforcementStructural, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Each lane answers its script only when asked for the line
			// profile, so a check that did not ask for it would read every
			// line valid.
			g, asked, findings := profileLaneGateway(t, c.level, c.verdicts)
			status, msg := drainedPayerIngress(g, answerCtx("pas-claim"), []byte(undeclaredClaimResponse), "2.0", "pa.pas", partner)
			if c.refused {
				if status == 0 || !strings.Contains(msg, "validation failed") {
					t.Fatalf("an answer no line accepts is refused, got %d %q (asked %v)", status, msg, *asked)
				}
				if len(*findings) != 1 || (*findings)[0].Decision != "refused" || (*findings)[0].Profile != pasClaimResponseProfile {
					t.Fatalf("findings %+v, want one refused, naming the line profile", *findings)
				}
				return
			}
			if status != 0 {
				t.Fatalf("an answer a line accepts is carried, got %d %q (asked %v)", status, msg, *asked)
			}
			if len(*findings) != 1 || (*findings)[0].Line != c.line || (*findings)[0].Verdict != "valid" || (*findings)[0].Decision != "relayed" {
				t.Fatalf("findings %+v, want one relayed, valid at %s", *findings, c.line)
			}
		})
	}
}

// At the provider's ingress, a payer's undeclared answer to a PAS submit,
// update or inquiry is judged against its line's own profile at structural and
// strict, where one its profile refuses is refused as conformance at the
// participant's own level; at observe it is judged as before and carried, and
// at none it is carried exactly with no validator call.
func TestPASIngress_UndeclaredAnswerIsJudgedAtItsLineProfile(t *testing.T) {
	t.Parallel()
	wantProfile := map[string]string{"pas-claim": pasResponseBundleProfile, "pas-claim-update": pasResponseBundleProfile, "pas-claim-inquire": pasInquiryBundleProfile}
	for _, leg := range carriedAnswerLegs() {
		declared := leg.answer
		leg.answer = func(t *testing.T) []byte { return withoutDeclaredProfiles(t, declared(t)) }
		profile := wantProfile[leg.leg]
		for _, level := range allLevels {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				v := &profileRefusingValidator{profile: profile}
				env, rec, findings, xr, answer := carriedAnswerRun(t, leg, level, "", v)
				if env.routeHitCount() != 1 {
					t.Fatalf("the request must be carried once, network hits %d", env.routeHitCount())
				}
				switch level {
				case EnforcementNone:
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), relayedPASAnswer(t, answer)) || v.calls != 0 || len(findings) != 0 {
						t.Fatalf("at none: %d, %d validator calls, %d findings; want the answer carried exactly, unchecked", rec.Code, v.calls, len(findings))
					}
				case EnforcementObserve:
					if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), relayedPASAnswer(t, answer)) || len(v.asked) == 0 || slices.Contains(v.asked, profile) {
						t.Fatalf("at observe: %d, asked %v; want the answer carried, judged as before", rec.Code, v.asked)
					}
				default:
					// Refused after the payer answered: delivered yes, 502 and its text.
					wantDeliveredOutcome(t, rec, http.StatusBadGateway, deliveredYes, "ingress validation failed")
					wantRefusal(t, xr, http.StatusBadGateway, RefusedByProviderGateway, RefusalConformance)
					if len(findings) != 1 || findings[0].Profile != profile || findings[0].Whose != "peer" || findings[0].Decision != "refused" {
						t.Fatalf("findings %+v, want one refused, naming %s, the peer's", findings, profile)
					}
				}
			})
		}
	}
}

// profileRefusingValidator refuses, as a structural defect, every call that
// asks for profile, and accepts every other.
type profileRefusingValidator struct {
	profile string
	mu      sync.Mutex
	calls   int
	asked   []string
}

func (v *profileRefusingValidator) Validate(_ context.Context, _ []byte, profile string) (shnsdk.Result, error) {
	v.mu.Lock()
	v.calls++
	v.asked = append(v.asked, profile)
	v.mu.Unlock()
	if profile == v.profile {
		return scriptedResult(scriptStructural)
	}
	return shnsdk.Result{Valid: true}, nil
}

// withoutDeclaredProfiles is body with every meta.profile removed, the
// resource's own and each Bundle entry's.
func withoutDeclaredProfiles(t *testing.T, body []byte) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	strip := func(r map[string]any) {
		if m, ok := r["meta"].(map[string]any); ok {
			delete(m, "profile")
		}
	}
	strip(doc)
	entries, _ := doc["entry"].([]any)
	for _, e := range entries {
		if r, ok := e.(map[string]any)["resource"].(map[string]any); ok {
			strip(r)
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out, body) {
		t.Fatal("no profile was removed")
	}
	return out
}
