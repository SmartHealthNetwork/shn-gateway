//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS request profiles a carried request that declares none is judged
// against at the payer's gateway at structural and strict (at the payer's
// own level), without a version.
const (
	pasRequestBundleLineProfile = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
	pasInquiryRequestProfile    = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-inquiry-request-bundle"
)

// requestCall is one call a requestLane received.
type requestCall struct {
	line, profile string
	body          []byte
}

// requestLane is one line's validator lane for the carried-request rows: it
// records each call's profile and body and answers as scripted.
type requestLane struct {
	line    string
	verdict scriptedVerdict
	mu      *sync.Mutex
	calls   *[]requestCall
}

func (l requestLane) Validate(_ context.Context, body []byte, profile string) (shnsdk.Result, error) {
	l.mu.Lock()
	*l.calls = append(*l.calls, requestCall{line: l.line, profile: profile, body: append([]byte(nil), body...)})
	l.mu.Unlock()
	return scriptedResult(l.verdict)
}

// requestSeen is what the payer's own system received: the bytes of each
// request handed to it.
type requestSeen struct {
	mu   sync.Mutex
	got  [][]byte
	next LegResponder
}

func (s *requestSeen) Handle(ctx context.Context, leg, corrID, subjectPCI string, requestFHIR []byte) (LegResult, error) {
	s.mu.Lock()
	s.got = append(s.got, append([]byte(nil), requestFHIR...))
	s.mu.Unlock()
	return s.next.Handle(ctx, leg, corrID, subjectPCI, requestFHIR)
}

func (s *requestSeen) received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got
}

// carriedRequestLeg is one of the three payer handlers that judge a carried PAS
// request, with a request it accepts.
type carriedRequestLeg struct {
	leg     string
	request func(t *testing.T) []byte
	drive   func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string)
	// profile is the line profile an undeclared request is judged against.
	profile string
}

var carriedRequestLegs = []carriedRequestLeg{
	{"pas-claim", func(t *testing.T) []byte { return requestBundleShaped(t, conformantPASBundleWithQR(t, "MBR-COVERED")) },
		func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
			g.handlePASNativeInbound(w, r, env, tok, body, answerTok)
		}, pasRequestBundleLineProfile},
	{"pas-claim-update", func(t *testing.T) []byte { return originatorBuiltConformantUpdateBundle(t) },
		func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
			g.handlePASUpdateNativeInbound(w, r, env, tok, body, answerTok)
		}, pasRequestBundleLineProfile},
	{"pas-claim-inquire", func(*testing.T) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") },
		func(g *Gateway, w http.ResponseWriter, r *http.Request, env shnsdk.Envelope, tok shnsdk.Token, body []byte, answerTok string) {
			g.handlePASInquireInbound(w, r, env, tok, body, answerTok)
		}, pasInquiryRequestProfile},
}

// requestBundleShaped is a request Bundle with what the request Bundle
// profile asks of the Bundle itself at every line (pasRequestBundleRules): an
// identifier, a timestamp, a fullUrl on every entry and the Claim first.
func requestBundleShaped(t *testing.T, body []byte) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	b["identifier"] = map[string]any{"system": "urn:example:pas-request", "value": "REQ-CARRIED-1"}
	b["timestamp"] = "2026-11-13T00:00:00Z"
	entries, _ := b["entry"].([]any)
	var claims, rest []any
	for i, e := range entries {
		m := e.(map[string]any)
		if _, ok := m["fullUrl"]; !ok {
			m["fullUrl"] = fmt.Sprintf("urn:uuid:5f1c2a64-0000-4000-8000-%012d", i+1)
		}
		if r, _ := m["resource"].(map[string]any); r["resourceType"] == "Claim" {
			claims = append(claims, m)
		} else {
			rest = append(rest, m)
		}
	}
	b["entry"] = append(claims, rest...)
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// withMetaProfile is body with its meta.profile set to profiles (none removes
// it).
func withMetaProfile(t *testing.T, body []byte, profiles ...string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	meta, _ := m["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	if len(profiles) == 0 {
		delete(meta, "profile")
	} else {
		meta["profile"] = profiles
	}
	if len(meta) == 0 {
		delete(m, "meta")
	} else {
		m["meta"] = meta
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// carriedRequestRun is one carried request driven through its payer handler.
type carriedRequestRun struct {
	rec *httptest.ResponseRecorder
	// status and answer are the leg's answer as the requester reads it.
	status   int
	answer   []byte
	calls    []requestCall
	seen     [][]byte
	findings []ConformanceFinding
}

// driveCarriedRequest drives body on leg through a payer gateway at level whose
// validator lanes are verdicts, routed at answerTok's line.
func driveCarriedRequest(t *testing.T, leg carriedRequestLeg, level ConformanceEnforcement, verdicts map[string]scriptedVerdict, body []byte, answerTok string) carriedRequestRun {
	t.Helper()
	var mu sync.Mutex
	var calls []requestCall
	lanes := map[string]shnsdk.Validator{}
	for line, v := range verdicts {
		lanes[line] = requestLane{line: line, verdict: v, mu: &mu, calls: &calls}
	}
	run := driveCarriedRequestOn(t, leg, level, lanes, body, answerTok)
	mu.Lock()
	defer mu.Unlock()
	run.calls = append([]requestCall(nil), calls...)
	return run
}

// driveCarriedRequestOn is driveCarriedRequest on the given validator lanes.
func driveCarriedRequestOn(t *testing.T, leg carriedRequestLeg, level ConformanceEnforcement, lanes map[string]shnsdk.Validator, body []byte, answerTok string) carriedRequestRun {
	t.Helper()
	g, requester := newInboundTestGateway(t, true)
	g.cfg.ConformanceEnforcement = level
	findings := answerFindings(g)
	g.cfg.Validator = lanes[shnsdk.LineOf(answerTok)]
	g.cfg.ValidatorsByContractLine = contractLanes(lanes)
	seen := &requestSeen{next: pasResultResponder{result: LegResult{Response: relayedResponse(unreadablePASAnswer), ResponseSubjectForeign: true}}}
	g.cfg.Responder = seen
	pci, _, _ := g.cfg.SoR.ResolvePatient("MBR-COVERED")
	env := shnsdk.Envelope{}
	env.Metadata.CorrelationID, env.Metadata.Sender = "corr-"+leg.leg+"-d47-request", requester.ID
	tok := shnsdk.Token{Subject: pci, CorrelationID: env.Metadata.CorrelationID}
	r := newSignedInboundRequest(t, g, requester.ID)
	r = r.WithContext(withFindingContext(r.Context(), findingContext{
		LegType: leg.leg, CorrelationID: env.Metadata.CorrelationID, Seam: paCatalog[leg.leg].Seam, Whose: "peer",
	}))
	rec := httptest.NewRecorder()
	leg.drive(g, rec, r, env, tok, body, answerTok)
	// At observe a check would be queued off the request path: wait for any,
	// so a row reads every call and finding the leg produced.
	g.drainObserveChecks()
	run := carriedRequestRun{rec: rec, status: rec.Code, answer: rec.Body.Bytes()}
	if rec.Code == http.StatusOK {
		hdr, answer, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("decode the answer frame: %v", err)
		}
		run.status, run.answer = hdr.Status, answer
	}
	run.seen, run.findings = seen.received(), append([]ConformanceFinding(nil), *findings...)
	return run
}

// carried reports whether the payer's system received exactly body, once.
func (run carriedRequestRun) carried(body []byte) bool {
	return len(run.seen) == 1 && bytes.Equal(run.seen[0], body)
}

// refusedWith reports whether the requester got status and the payer's system
// nothing.
func (run carriedRequestRun) refusedWith(status int, text string) bool {
	return len(run.seen) == 0 && run.status == status && strings.Contains(string(run.answer), text)
}

// refusedRequest reports whether run recorded the carried request's own
// refusal: a finding on it, the peer's, on leg, decided refused.
func (run carriedRequestRun) refusedRequest(leg string) bool {
	for _, f := range run.findings {
		if f.LegType == leg && f.Whose == "peer" && f.Kind == string(KindFHIRIngress) && f.Decision == "refused" {
			return true
		}
	}
	return false
}

// requestPeerFinding reports whether run recorded a finding on the carried
// request itself (the peer's), on leg.
func (run carriedRequestRun) requestPeerFinding(leg string) bool {
	for _, f := range run.findings {
		if f.LegType == leg && f.Whose == "peer" {
			return true
		}
	}
	return false
}

// A payer's gateway judges the carried PAS request it receives, on $submit,
// the update and $inquire, at the payer's own level before its system sees it,
// and never changes a byte:
//   - none and observe call no validator for it and carry it exactly as sent;
//   - structural refuses a structural fault 422 "ingress validation failed"
//     (nothing reaches the payer's system), and records and carries a deeper
//     issue and a check that could not run;
//   - strict refuses a deeper issue 422 too, and a check that could not run 500;
//   - a code system the lane cannot load is a gap: carried at every level.
//
// A request that declares no profile is judged against its line's request
// Bundle profile without a version (the inquiry request Bundle's on $inquire).
func TestCarriedPASRequest_JudgedAtThePayersLevel(t *testing.T) {
	for _, leg := range carriedRequestLegs {
		for _, c := range []struct {
			name    string
			verdict scriptedVerdict
			// want, by level: "carried", "carried+finding", 422 or 500.
			want map[ConformanceEnforcement]string
		}{
			{"a valid request", scriptValid, map[ConformanceEnforcement]string{
				EnforcementNone: "carried", EnforcementObserve: "carried", EnforcementStructural: "carried", EnforcementStrict: "carried"}},
			{"a structural fault", scriptStructural, map[ConformanceEnforcement]string{
				EnforcementNone: "carried", EnforcementObserve: "carried", EnforcementStructural: "422", EnforcementStrict: "422"}},
			{"a deeper rule", scriptDeeper, map[ConformanceEnforcement]string{
				EnforcementNone: "carried", EnforcementObserve: "carried", EnforcementStructural: "carried+finding", EnforcementStrict: "422"}},
			// A gap is recorded as unchecked, never refused.
			{"a terminology gap", scriptUnchecked, map[ConformanceEnforcement]string{
				EnforcementNone: "carried", EnforcementObserve: "carried", EnforcementStructural: "carried+finding", EnforcementStrict: "carried+finding"}},
			{"a check that cannot run", scriptOutage, map[ConformanceEnforcement]string{
				EnforcementNone: "carried", EnforcementObserve: "carried", EnforcementStructural: "carried+finding", EnforcementStrict: "500"}},
		} {
			for _, level := range allLevels {
				t.Run(leg.leg+"/"+c.name+"/"+level.String(), func(t *testing.T) {
					body := withMetaProfile(t, leg.request(t))
					run := driveCarriedRequest(t, leg, level, map[string]scriptedVerdict{"2.0": c.verdict}, body, "pa.pas@2.0")
					if level == EnforcementNone || level == EnforcementObserve {
						if len(run.calls) != 0 {
							t.Fatalf("at %s no validator is called for the request, got %d calls", level, len(run.calls))
						}
					} else {
						if len(run.calls) == 0 {
							t.Fatal("no lane was asked")
						}
						for _, call := range run.calls {
							if call.profile != leg.profile {
								t.Fatalf("a lane was asked %q, want the line profile %q", call.profile, leg.profile)
							}
						}
					}
					switch want := c.want[level]; want {
					case "carried", "carried+finding":
						if !run.carried(body) {
							t.Fatalf("want the request carried as sent, got %d to the payer's system; answer %d %.300s", len(run.seen), run.status, run.answer)
						}
						if strings.Contains(string(run.answer), refusalIngressValidation) {
							t.Fatalf("a carried request was refused: %.300s", run.answer)
						}
						if got := run.requestPeerFinding(leg.leg); got != (want == "carried+finding") {
							t.Fatalf("a finding on the request: %v, want %v (%+v)", got, want == "carried+finding", run.findings)
						}
					case "422":
						if !run.refusedWith(http.StatusUnprocessableEntity, refusalIngressValidation) {
							t.Fatalf("want 422 %q and nothing to the payer's system, got %d to it; answer %.300s", refusalIngressValidation, len(run.seen), run.answer)
						}
						// An OperationOutcome on these FHIR legs, echoing the
						// validator's issue, and the request's refusal recorded.
						if !bytes.Contains(run.answer, []byte(`"resourceType":"OperationOutcome"`)) || !bytes.Contains(run.answer, []byte("RAW-DIAGNOSTIC")) {
							t.Fatalf("want an OperationOutcome echoing the issue, got %.300s", run.answer)
						}
						if !run.refusedRequest(leg.leg) {
							t.Fatalf("no refusal recorded on the request: %+v", run.findings)
						}
					case "500":
						if !run.refusedWith(http.StatusInternalServerError, "validator unavailable") {
							t.Fatalf("want 500 and nothing to the payer's system, got %d to it; answer %.300s", len(run.seen), run.answer)
						}
					}
				})
			}
		}
	}
}

// A request that declares a profile is judged against what it declares (the
// lane is asked for no profile of its own), at structural and strict.
func TestCarriedPASRequest_DeclaredProfileIsJudgedAsDeclared(t *testing.T) {
	for _, leg := range carriedRequestLegs {
		for _, level := range []ConformanceEnforcement{EnforcementStructural, EnforcementStrict} {
			t.Run(leg.leg+"/"+level.String(), func(t *testing.T) {
				body := withMetaProfile(t, leg.request(t), leg.profile+"|2.0.1")
				run := driveCarriedRequest(t, leg, level, map[string]scriptedVerdict{"2.0": scriptValid}, body, "pa.pas@2.0")
				if len(run.calls) == 0 {
					t.Fatal("no lane was asked")
				}
				for _, call := range run.calls {
					if call.profile != "" {
						t.Fatalf("a declared request was judged at %q, want what it declares", call.profile)
					}
				}
				if !run.carried(body) {
					t.Fatalf("want the request carried as sent, answer %.300s", run.answer)
				}
			})
		}
	}
}

// A request is judged at its candidate lines, valid when any accepts it: one
// the routed line refuses but another line accepts is carried, at strict; one
// no line accepts is refused.
func TestCarriedPASRequest_ValidAtAnyCandidateLine(t *testing.T) {
	for _, leg := range carriedRequestLegs {
		body := withMetaProfile(t, leg.request(t))
		t.Run(leg.leg+"/valid only at another line", func(t *testing.T) {
			run := driveCarriedRequest(t, leg, EnforcementStrict, map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptValid}, body, "pa.pas@2.0")
			if !run.carried(body) {
				t.Fatalf("want the request carried, answer %.300s", run.answer)
			}
			if len(run.calls) < 2 || run.calls[0].line != "2.0" {
				t.Fatalf("want the routed line judged first, then another, got %+v", run.calls)
			}
		})
		t.Run(leg.leg+"/no line accepts", func(t *testing.T) {
			run := driveCarriedRequest(t, leg, EnforcementStrict, map[string]scriptedVerdict{"2.0": scriptStructural, "2.2": scriptStructural}, body, "pa.pas@2.0")
			if !run.refusedWith(http.StatusUnprocessableEntity, refusalIngressValidation) {
				t.Fatalf("want 422 and nothing to the payer's system, answer %.300s", run.answer)
			}
		})
	}
}

// At 2.1 and 2.2 a $submit's or an update's request Bundle checked at its line
// profile is read per entry (paCertification.reading): the lane is asked once,
// for no profile, with each Claim declaring its candidate profile; the bytes
// the payer's system receives are the request as sent. An inquiry's request
// Bundle is the lane's one call at the inquiry profile.
func TestCarriedPASRequest_ReadPerEntryAtTwoOneAndTwoTwo(t *testing.T) {
	for _, line := range []string{"2.1", "2.2"} {
		for _, leg := range carriedRequestLegs {
			t.Run(line+"/"+leg.leg, func(t *testing.T) {
				body := withMetaProfile(t, leg.request(t))
				run := driveCarriedRequest(t, leg, EnforcementStrict, map[string]scriptedVerdict{line: scriptValid}, body, "pa.pas@"+line)
				if !run.carried(body) {
					t.Fatalf("want the request carried as sent, answer %.300s", run.answer)
				}
				if len(run.calls) != 1 {
					t.Fatalf("want one call on the %s lane, got %+v", line, run.calls)
				}
				call := run.calls[0]
				if leg.leg == "pas-claim-inquire" {
					if call.profile != pasInquiryRequestProfile {
						t.Fatalf("an inquiry was asked at %q, want %q whole", call.profile, pasInquiryRequestProfile)
					}
					return
				}
				if call.profile != "" {
					t.Fatalf("a request Bundle at %s was asked at %q, want the per-entry reading (no profile)", line, call.profile)
				}
				if !bytes.Contains(call.body, []byte(`StructureDefinition/profile-claim`)) {
					t.Fatalf("the lane read no Claim declaring its candidate profile: %.400s", call.body)
				}
			})
		}
	}
}

// The per-entry reading's own rules on the request Bundle reach the payer's
// check, though the lane finds nothing: at 2.1 a $submit's request Bundle
// without an identifier is a structural fault, refused at structural; at 2.2
// one whose Claim is not first breaks the ClaimFirst invariant, a deeper rule,
// refused at strict; at 2.0, read whole by its lane, the rule is the lane's.
func TestCarriedPASRequest_RequestBundleRulesReachThePayersCheck(t *testing.T) {
	leg := carriedRequestLegs[0]
	shaped := withMetaProfile(t, leg.request(t))
	for _, c := range []struct {
		name, line string
		level      ConformanceEnforcement
		mutate     func(map[string]any)
		refused    bool
	}{
		{"no identifier at 2.1", "2.1", EnforcementStructural, func(b map[string]any) { delete(b, "identifier") }, true},
		{"the Claim not first at 2.2", "2.2", EnforcementStrict, func(b map[string]any) {
			first := map[string]any{"fullUrl": "urn:uuid:5f1c2a64-0000-4000-8000-0000000000f1", "resource": map[string]any{"resourceType": "Organization", "id": "ahead-of-the-claim", "name": "An organization ahead of the Claim"}}
			b["entry"] = append([]any{first}, b["entry"].([]any)...)
		}, true},
		{"no identifier at 2.0, read whole by its lane", "2.0", EnforcementStrict, func(b map[string]any) { delete(b, "identifier") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b map[string]any
			if err := json.Unmarshal(shaped, &b); err != nil {
				t.Fatal(err)
			}
			c.mutate(b)
			body, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			run := driveCarriedRequest(t, leg, c.level, map[string]scriptedVerdict{c.line: scriptValid}, body, "pa.pas@"+c.line)
			if c.refused {
				if !run.refusedWith(http.StatusUnprocessableEntity, refusalIngressValidation) {
					t.Fatalf("want 422 and nothing to the payer's system, answer %d %.300s", run.status, run.answer)
				}
				return
			}
			if !run.carried(body) {
				t.Fatalf("want the request carried as sent, answer %d %.300s", run.status, run.answer)
			}
		})
	}
}

// With no validator lane for any candidate line the check cannot run: strict
// refuses 500 naming the missing lane, and structural records it and carries
// the request as sent.
func TestCarriedPASRequest_NoLane(t *testing.T) {
	for _, leg := range carriedRequestLegs {
		body := withMetaProfile(t, leg.request(t))
		t.Run(leg.leg+"/strict", func(t *testing.T) {
			run := driveCarriedRequest(t, leg, EnforcementStrict, nil, body, "pa.pas@2.0")
			if !run.refusedWith(http.StatusInternalServerError, "no FHIR validator lane configured for this leg") {
				t.Fatalf("want 500 naming the missing lane and nothing to the payer's system, answer %d %.300s", run.status, run.answer)
			}
		})
		t.Run(leg.leg+"/structural", func(t *testing.T) {
			run := driveCarriedRequest(t, leg, EnforcementStructural, nil, body, "pa.pas@2.0")
			if !run.carried(body) {
				t.Fatalf("want the request carried as sent, answer %d %.300s", run.status, run.answer)
			}
			if !run.requestPeerFinding(leg.leg) {
				t.Fatalf("want the check that could not run recorded, got %+v", run.findings)
			}
		})
	}
}

// A PAS 2.0 update that carries its prior Claim as a second Claim entry (an
// update walked down from 2.1 or 2.2, or one from an earlier SDK) breaks PAS
// 2.0.1's request Bundle, which allows exactly one Claim: a payer's gateway at
// structural or strict refuses it with only a 2.0 lane, and carries it at
// none and observe. Read by the line fake, which models each line's request
// Bundle profile.
func TestCarriedPASRequest_ATwoZeroUpdateCarryingItsPriorClaim(t *testing.T) {
	leg := carriedRequestLegs[1]
	var b map[string]any
	if err := json.Unmarshal(withMetaProfile(t, leg.request(t)), &b); err != nil {
		t.Fatal(err)
	}
	b["entry"] = append(entries(b), anotherClaim(t, b, "prior-claim"))
	claims := 0
	for _, e := range b["entry"].([]any) {
		if r, _ := e.(map[string]any)["resource"].(map[string]any); r["resourceType"] == "Claim" {
			claims++
		}
	}
	if claims != 2 {
		t.Fatalf("fixture: the update carries %d Claims, want the update and its prior", claims)
	}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range allLevels {
		t.Run("a 2.0 lane only/"+level.String(), func(t *testing.T) {
			run := driveCarriedRequestOn(t, leg, level, map[string]shnsdk.Validator{"2.0": NewLineFakeValidator("2.0")}, body, "pa.pas@2.0")
			if level == EnforcementStructural || level == EnforcementStrict {
				if !run.refusedWith(http.StatusUnprocessableEntity, "Bundle.entry:Claim") {
					t.Fatalf("want 422 naming Bundle.entry:Claim and nothing to the payer's system, answer %d %.300s", run.status, run.answer)
				}
				return
			}
			if !run.carried(body) {
				t.Fatalf("want the update carried as sent, answer %d %.300s", run.status, run.answer)
			}
		})
	}
}
