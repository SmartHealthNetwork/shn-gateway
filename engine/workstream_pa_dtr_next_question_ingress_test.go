//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"
)

// The provider DTR client's $next-question routes ride the package fetch's
// leg and route. Until the payer's gateway can verify the adaptive address,
// nothing is sent: every check is made, in order, and a round that passes them
// all is refused 422 routing. These rows prove the checks, the refusal of every
// round, and that no frame leaves the gateway on any path.

// nqTag is a well-formed address tag (22 base64url characters). Paths with a
// dot segment spell it out in one literal, never joined to a value, so they
// read as data, not as a step up the tree.
const nqTag = "AbCdEfGhIjKlMnOpQrStUv"

// nqRound is a $next-question input: a bare QuestionnaireResponse about
// subject, with its contained Questionnaire, as an adaptive round carries it.
func nqRound(subject, extra string) []byte {
	return []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress",` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active"}],"questionnaire":"#q",` +
		`"subject":{"reference":"` + subject + `"}` + extra + `}`)
}

// nqParams is round wrapped as the DTR $next-question input Parameters, as
// module 1 and some DTR clients send it.
func nqParams(round []byte) []byte {
	return []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` + string(round) + `}]}`)
}

// nqExchange is the provider's gateway of an in-process exchange, its routes
// mounted on a mux as the gateway mounts them (so the handler sees the
// pattern the mux matched), its payer declaring framed DTR operations and,
// when sub, the adaptive-address token. Records are observed into got.
func nqExchange(t *testing.T, level ConformanceEnforcement, sub bool, opts ...func(*Config)) (*inProcessExchange, http.Handler, *exchangeRecords) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = newPrefetchSoR().sor()
	env.originator.cfg.ConformanceEnforcement = level
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	for _, o := range opts {
		o(&env.originator.cfg)
	}
	declareFramedDTR(t, env, true)
	if sub {
		e, _ := env.originator.cfg.Reg.Lookup(env.payerID)
		e.RequestFrames = append(e.RequestFrames, requestFrameV1OpSub)
		env.originator.cfg.Reg.Set(env.payerID, e)
	}
	mux := http.NewServeMux()
	for _, p := range []string{patternDTRNextQuestionPayer, patternDTRNextQuestionFetchServer, patternDTRNextQuestionPlain} {
		mux.HandleFunc(p, env.originator.ingressRoute(RouteDTR))
	}
	return env, mux, &got
}

func nqPost(h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// nothingSent fails if a request envelope left the gateway.
func nothingSent(t *testing.T, env *inProcessExchange) {
	t.Helper()
	env.substrate.mu.Lock()
	defer env.substrate.mu.Unlock()
	if len(env.substrate.sentNames) != 0 || env.substrate.lastReq != nil {
		t.Fatalf("a frame left the gateway: %v", env.substrate.sentNames)
	}
}

// Every path, at every level: the round is refused with the network primitive
// named, and nothing is sent. A round that passes every check meets the
// verification seam.
func TestDTRNextQuestionIngress_RefusesEveryRoundAndSendsNothing(t *testing.T) {
	t.Parallel()
	good := nqRound("Patient/"+prefetchMember, "")
	payer := func(rest string) string { return "/dtr/payer/payer/" + rest }
	for name, row := range map[string]struct {
		path   string
		body   []byte
		sub    bool
		opts   []func(*Config)
		status int
		msg    string
		rule   string
	}{
		// Routing and addressing.
		"the plain route names no payer": {"/Questionnaire/$next-question", good, true, nil, http.StatusUnprocessableEntity, errNextQuestionNoPayer, RefusalRouting},
		"an unregistered payer":          {"/dtr/payer/nobody/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil, http.StatusUnprocessableEntity, errNextQuestionUnknownPayer, RefusalRouting},
		"a participant that is no payer": {"/dtr/payer/provider/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil, http.StatusUnprocessableEntity, errNextQuestionUnknownPayer, RefusalRouting},
		"a payer without the adaptive-address token": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), good, false, nil,
			http.StatusUnprocessableEntity, errNextQuestionNoSubpath, RefusalRouting},
		"fetch-server, a payer without the token": {"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, false, nil,
			http.StatusUnprocessableEntity, errNextQuestionNoSubpath, RefusalRouting},
		"not a line":               {payer("two/" + nqTag + "/Questionnaire/$next-question"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"a short tag":              {payer("2.2/short/Questionnaire/$next-question"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"no operation":             {payer("2.2/" + nqTag + "/Questionnaire/$questionnaire-package"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"an encoded traversal":     {payer("2.2/" + nqTag + "/%2e%2e/Questionnaire/$next-question"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"an encoded slash":         {payer("2.2/" + nqTag + "/a%2Fb/Questionnaire/$next-question"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"a reserved character":     {"/dtr/payer/payer/2.2/AbCdEfGhIjKlMnOpQrStUv/..;/Questionnaire/$next-question", good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"a percent-encoded letter": {payer("2.2/" + nqTag + "/%61/Questionnaire/$next-question"), good, true, nil, http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"fetch-server, a short tag": {"/dtr/fetch-server/payer/2.2/short/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		// The patient (D121), as the package route binds its subject.
		"not a next-question input": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), []byte(`{"resourceType":"Parameters"}`), true, nil,
			http.StatusBadRequest, errNextQuestionNotInput, RefusalAuthority},
		"no subject": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"),
			[]byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress"}`), true, nil,
			http.StatusUnprocessableEntity, errNextQuestionNoSubject, RefusalAuthority},
		"a subject that is not a Patient": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqRound("Group/1", ""), true, nil,
			http.StatusForbidden, errNextQuestionNotPatient, RefusalAuthority},
		// A subject present as a logical identifier is named but is no Patient
		// reference, which binds on neither side.
		"a subject that is a logical identifier": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"),
			[]byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"identifier":{"system":"urn:x","value":"1"}}}`), true, nil,
			http.StatusForbidden, errNextQuestionNotPatient, RefusalAuthority},
		"a member the system does not hold, known members required": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqRound("Patient/MBR-UNKNOWN", ""), true,
			[]func(*Config){func(c *Config) { c.RequireKnownMembers = true }},
			http.StatusForbidden, errNextQuestionUnheld, RefusalConformance},
		// The mux matches its literal segments unescaped, so an encoded
		// prefix reaches the handler, which reads the escaped path.
		"an encoded payer prefix": {"/dtr/p%61yer/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"an encoded fetch-server prefix": {"/dtr/f%65tch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"a line the network does not define": {payer("9.9/" + nqTag + "/Questionnaire/$next-question"), good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		"fetch-server, an encoded line": {"/dtr/fetch-server/payer/%32.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionAddress, RefusalRouting},
		// Every check passed: the verification seam, on each family and form.
		"a Parameters round": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqParams(good), true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"the payer's id percent-encoded": {"/dtr/payer/%70ayer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"an empty remainder": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"a clean remainder": {payer("2.2/" + nqTag + "/a-b/c.d~e/Questionnaire/$next-question"), good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"the operation's segment encoded": {payer("2.2/" + nqTag + "/Questionnaire/%24next-question"), good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		// Another line the network defines reaches the seam: the line is
		// only checked here, never selected; selection comes with the forward.
		"another defined line": {payer("2.0/" + nqTag + "/Questionnaire/$next-question"), good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"fetch-server": {"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
		"a member the system does not hold, carried": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqRound("Patient/MBR-UNKNOWN", ""), true, nil,
			http.StatusUnprocessableEntity, errNextQuestionUnverifiable, RefusalRouting},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, h, got := nqExchange(t, level, row.sub, row.opts...)
				rec := nqPost(h, row.path, row.body)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %s, want %d %q", rec.Code, rec.Body, row.status, row.msg)
				}
				nothingSent(t, env)
				x := got.only(t)
				wantRefusal(t, x, row.status, RefusedByProviderGateway, row.rule)
				if row.rule == RefusalConformance {
					// An opt-in check names no reason: only network rules do.
					if x.RefusalReason != "" {
						t.Fatalf("an opt-in check recorded the reason %q", x.RefusalReason)
					}
				} else {
					wantReasonInAnswer(t, x, rec.Body.String())
				}
				if x.Route != RouteDTR {
					t.Fatalf("recorded on route %q, want the package fetch's %q", x.Route, RouteDTR)
				}
			})
		}
	}
}

// A payer without framed DTR operations is refused 502 with the package
// route's own answer, recorded as routing, at every level, before anything is
// sent. The package route records that refusal under no class today; recording
// it as routing there too is deferred to the change that mounts these routes,
// so this change makes no partner-visible change.
func TestDTRNextQuestionIngress_APayerWithoutFramedDTRIsRefused502(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, h, got := nqExchange(t, level, true)
			declareFramedDTR(t, env, false)
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), shnsdk.ErrFramedDTRUnsupported.Error()) {
				t.Fatalf("answer %d %s, want 502 naming the upgrade", rec.Code, rec.Body)
			}
			nothingSent(t, env)
			wantRefusal(t, got.only(t), http.StatusBadGateway, RefusedByProviderGateway, RefusalRouting)
		})
	}
}

// Another patient the round names is the payload's own consistency
// (RulePatientMixed): refused at strict, carried below it to the verification
// seam, where nothing is sent either way.
func TestDTRNextQuestionIngress_AnotherPatientIsTheMixedPatientRule(t *testing.T) {
	t.Parallel()
	for _, wrap := range []struct {
		name string
		of   func([]byte) []byte
	}{
		{"a bare QuestionnaireResponse", func(b []byte) []byte { return b }},
		{"a Parameters", nqParams},
		// The other patient in a second parameter beside the round, not in
		// the QuestionnaireResponse: every resource the Parameters carries is
		// fenced.
		{"a Parameters naming another patient in a parameter's part", func([]byte) []byte {
			return []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` +
				string(nqRound("Patient/"+prefetchMember, "")) + `},{"name":"note","part":[{"name":"evidence","resource":` +
				`{"resourceType":"Observation","id":"o","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}}]}]}`)
		}},
		{"a Parameters naming another patient in a second parameter", func([]byte) []byte {
			return []byte(`{"resourceType":"Parameters","parameter":[{"name":"questionnaire-response","resource":` +
				string(nqRound("Patient/"+prefetchMember, "")) + `},{"name":"note","resource":` +
				`{"resourceType":"Observation","id":"o","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}}]}`)
		}},
	} {
		t.Run(wrap.name, func(t *testing.T) { mixedPatientRows(t, wrap.of) })
	}
}

func mixedPatientRows(t *testing.T, wrap func([]byte) []byte) {
	// A contained Observation about another patient beside the round's own.
	body := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress",` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active"},` +
		`{"resourceType":"Observation","id":"o","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}],` +
		`"questionnaire":"#q","subject":{"reference":"Patient/` + prefetchMember + `"}}`)
	body = wrap(body)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			var findings []ConformanceFinding
			env, h, got := nqExchange(t, level, true, func(c *Config) {
				c.Observer = func(e ObserverEvent) {
					var f ConformanceFinding
					if e.Kind == ConformanceObservedEvent && json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindContent) {
						findings = append(findings, f)
					}
				}
			})
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", body)
			nothingSent(t, env)
			x := got.only(t)
			if refusesAt(level, RulePatientMixed) {
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errNextQuestionMixed) {
					t.Fatalf("at %s answer %d %s, want 403 %q", level, rec.Code, rec.Body, errNextQuestionMixed)
				}
				// A content rule's refusal is recorded as conformance, the
				// participant's own opt-in, never as a network primitive.
				wantRefusal(t, x, http.StatusForbidden, RefusedByProviderGateway, RefusalConformance)
				return
			}
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errNextQuestionUnverifiable) {
				t.Fatalf("at %s answer %d %s, want the round carried to the verification seam", level, rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			wantReasonInAnswer(t, x, rec.Body.String())
			// Every level that runs the rule records the finding on this leg;
			// none records nothing.
			wantFinding := level != EnforcementNone
			if got := len(findings) == 1 && findings[0].Rule == RulePatientMixed && findings[0].LegType == dtrLeg; got != wantFinding {
				t.Fatalf("at %s findings %+v, want a %s finding on %s: %v", level, findings, RulePatientMixed, dtrLeg, wantFinding)
			}
		})
	}
}

// A system of record that cannot be read for the round's patient is answered
// as the package route answers it, never as the verification seam, at every
// level. Nothing is sent.
func TestDTRNextQuestionIngress_ASystemOfRecordOutage(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sorErr := errors.New("system of record unreachable")
			wantStatus, wantMsg := SoRFailureResponse(sorErr)
			env, h, got := nqExchange(t, level, true, func(c *Config) {
				c.SoR = failingSoR{SystemOfRecord: c.SoR, err: sorErr}
			})
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
			nothingSent(t, env)
			if rec.Code != wantStatus || !strings.Contains(rec.Body.String(), wantMsg) {
				t.Fatalf("answer %d %s, want %d %q, as the package route answers it", rec.Code, rec.Body, wantStatus, wantMsg)
			}
			if x := got.only(t); x.Outcome == ExchangeAnswered {
				t.Fatalf("record %+v, want no answer", x)
			}
		})
	}
}

// A round about a member the system of record does not hold, carrying no
// Patient of the member's own but another person (a Coverage's subscriber)
// under the member's own number, names no one patient: refused 400 as
// authority, as the package route refuses it, at every level. Nothing is sent.
func TestDTRNextQuestionIngress_AnUnidentifiablePatientIsRefused400(t *testing.T) {
	t.Parallel()
	body := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","questionnaire":"#q",` +
		`"subject":{"reference":"Patient/stranger"},` +
		`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active"},` +
		`{"resourceType":"Coverage","id":"cov","status":"active","subscriber":{"reference":"#stranger"},` +
		`"beneficiary":{"reference":"Patient/other"},"payor":[{"reference":"Organization/p"}],` +
		`"contained":[{"resourceType":"Patient","id":"stranger","birthDate":"1960-01-01","name":[{"family":"Other","given":["Sub"]}]}]}]}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, h, got := nqExchange(t, level, true)
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", body)
			nothingSent(t, env)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgPatientUnidentifiable) {
				t.Fatalf("answer %d %s, want 400 %q", rec.Code, rec.Body, msgPatientUnidentifiable)
			}
			xr := got.only(t)
			wantRefusal(t, xr, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
			wantReasonInAnswer(t, xr, rec.Body.String())
		})
	}
}

// The legs a payer declares are not this gateway's to judge: the network
// checks declared legs only for a leg the taxonomy marks with the release
// that brings it, and the questionnaire fetch is not marked, so a payer
// declaring other legs, or none, is one the Hub carries the round to. The
// round reaches the verification seam at every level, as the package fetch
// to that payer is carried.
func TestDTRNextQuestionIngress_APayerDeclaringOtherLegsReachesTheSeam(t *testing.T) {
	t.Parallel()
	for name, legs := range map[string][]string{
		"another leg": {"pas-claim"},
		// An empty list declares that the payer takes no leg at all.
		"no leg": {},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, h, got := nqExchange(t, level, true)
				e, _ := env.originator.cfg.Reg.Lookup(env.payerID)
				e.Legs = legs
				env.originator.cfg.Reg.Set(env.payerID, e)
				rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
				nothingSent(t, env)
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errNextQuestionUnverifiable) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, errNextQuestionUnverifiable)
				}
				xr := got.only(t)
				wantRefusal(t, xr, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
				wantReasonInAnswer(t, xr, rec.Body.String())
			})
		}
	}
}

// The checks run in the design's order: where two fail together, the first
// one's refusal is the answer, at every level.
func TestDTRNextQuestionIngress_TheChecksRunInOrder(t *testing.T) {
	t.Parallel()
	good := nqRound("Patient/"+prefetchMember, "")
	big := nqRound("Patient/"+prefetchMember, `,"text":{"div":"`+strings.Repeat("x", shnsdk.MaxRequestBytes)+`"}`)
	for name, row := range map[string]struct {
		path    string
		body    []byte
		sub     bool
		noFrame bool
		status  int
		msg     string
		prepare func(*inProcessExchange)
	}{
		"an unregistered payer and a bad shape": {"/dtr/payer/nobody/two/short/Questionnaire/$next-question", good, true, false,
			http.StatusUnprocessableEntity, errNextQuestionUnknownPayer, nil},
		"no framed DTR and no adaptive-address token": {"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, false, true,
			http.StatusBadGateway, shnsdk.ErrFramedDTRUnsupported.Error(), nil},
		"no adaptive-address token and a bad shape": {"/dtr/payer/payer/two/short/Questionnaire/$next-question", good, false, false,
			http.StatusUnprocessableEntity, errNextQuestionNoSubpath, nil},
		// The bind runs before the fence: an unheld member is refused as
		// unheld even when the round also names another patient.
		"an unheld member and another patient": {"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
			[]byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress",` +
				`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active"},` +
				`{"resourceType":"Observation","id":"o","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}],` +
				`"questionnaire":"#q","subject":{"reference":"Patient/MBR-UNKNOWN"}}`),
			true, false, http.StatusForbidden, errNextQuestionUnheld, func(env *inProcessExchange) {
				env.originator.cfg.RequireKnownMembers = true
			}},
		"a bad line and an oversized body": {"/dtr/payer/payer/9.9/" + nqTag + "/Questionnaire/$next-question", big, true, false,
			http.StatusUnprocessableEntity, errNextQuestionAddress, nil},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, h, _ := nqExchange(t, level, row.sub)
				if row.noFrame {
					declareFramedDTR(t, env, false)
				}
				if row.prepare != nil {
					row.prepare(env)
				}
				rec := nqPost(h, row.path, row.body)
				nothingSent(t, env)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %.200s, want %d %q", rec.Code, rec.Body, row.status, row.msg)
				}
			})
		}
	}
}

// A body whose read fails is refused 400 before anything is sent, as on the
// package route, at every level. (A content coding the gateway does not
// decode is refused 415 by the production mount, before the handler, so its
// row runs once the routes are mounted.)
func TestDTRNextQuestionIngress_AnUnreadableBodyIsRefused(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run("a read that fails/"+level.String(), func(t *testing.T) {
			env, h, _ := nqExchange(t, level, true)
			req := httptest.NewRequest(http.MethodPost, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", &failingReader{n: 4})
			req.Header.Set("Content-Type", "application/fhir+json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			nothingSent(t, env)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answer %d %s, want 400", rec.Code, rec.Body)
			}
		})
	}
}

// A body over the request cap is refused 413 as routing, before anything is
// read or sent, at every level.
func TestDTRNextQuestionIngress_AnOversizedRoundIsRefused413(t *testing.T) {
	t.Parallel()
	big := nqRound("Patient/"+prefetchMember, `,"text":{"div":"`+strings.Repeat("x", shnsdk.MaxRequestBytes)+`"}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, h, got := nqExchange(t, level, true)
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", big)
			nothingSent(t, env)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("answer %d, want 413", rec.Code)
			}
			xr := got.only(t)
			wantRefusal(t, xr, http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
			wantReasonInAnswer(t, xr, rec.Body.String())
		})
	}
}

// A module client's request is the gateway's own and names its builder; a
// native client that names one is refused, as on the package route, before
// anything is sent.
func TestDTRNextQuestionIngress_ANativeClientNamingABuilderIsRefused(t *testing.T) {
	t.Parallel()
	env, h, _ := nqExchange(t, EnforcementStrict, true)
	req := httptest.NewRequest(http.MethodPost, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question",
		bytes.NewReader(nqRound("Patient/"+prefetchMember, "")))
	req.Header.Set("Content-Type", "application/fhir+json")
	req.Header.Set(BuilderHeader, "anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), errBuilderNative) {
		t.Fatalf("answer %d %s, want the builder refusal", rec.Code, rec.Body)
	}
	nothingSent(t, env)
}

// An absolute Patient reference binds the subject (patientMember reads it),
// and, as on the package route, whose fence resolves an absolute reference
// only against known bases, the fence reads it as another patient: carried to
// the verification seam below strict, refused 403 at strict. Nothing is sent.
func TestDTRNextQuestionIngress_AnAbsolutePatientReference(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, h, got := nqExchange(t, level, true)
			rec := nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question",
				nqRound("https://ehr.example/fhir/Patient/"+prefetchMember, ""))
			nothingSent(t, env)
			if refusesAt(level, RulePatientMixed) {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("at %s answer %d %s, want 403", level, rec.Code, rec.Body)
				}
				wantRefusal(t, got.only(t), http.StatusForbidden, RefusedByProviderGateway, RefusalConformance)
				return
			}
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errNextQuestionUnverifiable) {
				t.Fatalf("at %s answer %d %s, want the verification seam", level, rec.Code, rec.Body)
			}
		})
	}
}

// The fetch-server family names no remainder: a path with one matches no
// route, and nothing is sent.
func TestDTRNextQuestionIngress_AFetchServerAddressTakesNoRemainder(t *testing.T) {
	t.Parallel()
	env, h, _ := nqExchange(t, EnforcementStrict, true)
	rec := nqPost(h, "/dtr/fetch-server/payer/2.2/"+nqTag+"/x/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("answer %d %s, want no route (404)", rec.Code, rec.Body)
	}
	nothingSent(t, env)
}

// A literal dot or empty segment never reaches the handler: the mux answers
// it with a redirect to the cleaned path (307, which keeps the method), so it is never carried at the path it named,
// and nothing is sent.
func TestDTRNextQuestionIngress_ALiteralDotSegmentMeetsTheMuxRedirect(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]string{
		"a traversal":      "/dtr/payer/payer/2.2/AbCdEfGhIjKlMnOpQrStUv/../Questionnaire/$next-question",
		"an empty segment": "/dtr/payer/payer/2.2/" + nqTag + "//Questionnaire/$next-question",
	} {
		t.Run(name, func(t *testing.T) {
			env, h, _ := nqExchange(t, EnforcementStrict, true)
			rec := nqPost(h, path, nqRound("Patient/"+prefetchMember, ""))
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("answer %d %s, want the mux's 307 redirect", rec.Code, rec.Body)
			}
			nothingSent(t, env)
		})
	}
}

// The production mux does not mount the $next-question routes: they are a
// seam until the forward and the address tag check land. Mounting them is a
// deliberate change that flips this row, with the pinned ingress surface, the
// route patterns and the hosted edge's rule.
func TestDTRNextQuestionIngress_NotMountedYet(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	// The control: the package route is mounted on the same mux, so a 404
	// below is the route's absence, not a mux that serves nothing.
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, testIngressBaseURL+"/Questionnaire/$questionnaire-package", strings.NewReader("{}")))
	if w.Code == http.StatusNotFound {
		t.Fatalf("the package route answered 404 %s; the control needs it mounted", w.Body)
	}
	for _, p := range []string{
		"/Questionnaire/$next-question",
		"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
	} {
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}")))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d %s on the production mux, want it not mounted (404)", p, w.Code, w.Body)
		}
	}
}

// The rounds take the package route's own ingress credential: a call the
// package route refuses for its credential is refused on each $next-question
// route with the same status, answer and recorded class. The routes are
// mounted here on a mux behind the real ingressRoute and ingress auth, as the
// package route is; once they are mounted in production these rows run
// through the gateway's own handler.
func TestDTRNextQuestionIngress_TakesThePackageRoutesCredential(t *testing.T) {
	_, pub := newTestClientKey(t)
	paths := []string{
		"/Questionnaire/$questionnaire-package",
		"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/Questionnaire/$next-question",
	}
	for name, bearer := range map[string]func() string{
		"no bearer":          func() string { return "" },
		"a malformed bearer": func() string { return "Bearer not-a-token" },
		// Signed by a key this gateway does not hold for the client.
		"a bearer signed by another key": func() string {
			other, _ := newTestClientKey(t)
			kid, err := newKID()
			if err != nil {
				t.Fatal(err)
			}
			now := ingressFixedClock()()
			return "Bearer " + mintAssertionKID(t, other, kid, jwt.MapClaims{"client_id": "br-provider", "aud": testIngressBaseURL,
				"iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var want string
			for i, p := range paths {
				g := gatewayWithAuth(t, "br-provider", pub)
				hub, hits := hubHits(t)
				g.cfg.HubURL, g.cfg.AuthzURL = hub, hub
				var obs exchangeRecords
				g.cfg.ExchangeObserved = obs.observe
				mux := http.NewServeMux()
				for _, pat := range []string{"POST /Questionnaire/$questionnaire-package", patternDTRNextQuestionPayer,
					patternDTRNextQuestionFetchServer, patternDTRNextQuestionPlain} {
					mux.HandleFunc(pat, g.ingressRoute(RouteDTR))
				}
				req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}"))
				if b := bearer(); b != "" {
					req.Header.Set("Authorization", b)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if n := hits.Load(); n != 0 {
					t.Fatalf("%s: the refused call reached the network (%d calls)", p, n)
				}
				rule := obs.only(t).Rule
				got := w.Result().Status + " " + w.Body.String() + " recorded " + rule
				if w.Code != http.StatusUnauthorized || rule != RefusalAuthentication {
					t.Fatalf("%s: answer %s, want 401 recorded as %s", p, got, RefusalAuthentication)
				}
				if i == 0 {
					want = got
				} else if got != want {
					t.Fatalf("%s answered\n%s\nwant the package route's\n%s", p, got, want)
				}
			}
		})
	}
}

// A key store that cannot be consulted is answered 503 on each route, the
// package route's answer, before any exchange, and nothing is sent.
func TestDTRNextQuestionIngress_AKeyStoreOutageIsThePackageRoutes503(t *testing.T) {
	priv, pub := newTestClientKey(t)
	now := ingressFixedClock()()
	kid, err := newKID()
	if err != nil {
		t.Fatal(err)
	}
	bearer := mintAssertionKID(t, priv, kid, jwt.MapClaims{"client_id": "br-provider", "aud": testIngressBaseURL, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	var want string
	for i, p := range []string{
		"/Questionnaire/$questionnaire-package",
		"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/Questionnaire/$next-question",
	} {
		eph, err := newEphemeralKeyStore()
		if err != nil {
			t.Fatal(err)
		}
		g := gatewayWithAuthStores(t, "br-provider", pub, &unavailableKeyStore{IngressKeyStore: eph}, nil)
		hub, hits := hubHits(t)
		g.cfg.HubURL, g.cfg.AuthzURL = hub, hub
		mux := http.NewServeMux()
		for _, pat := range []string{"POST /Questionnaire/$questionnaire-package", patternDTRNextQuestionPayer,
			patternDTRNextQuestionFetchServer, patternDTRNextQuestionPlain} {
			mux.HandleFunc(pat, g.ingressRoute(RouteDTR))
		}
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if n := hits.Load(); n != 0 {
			t.Fatalf("%s: the refused call reached the network (%d calls)", p, n)
		}
		got := w.Result().Status + " " + w.Body.String()
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: answer %s, want 503", p, got)
		}
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("%s answered\n%s\nwant the package route's\n%s", p, got, want)
		}
	}
}
