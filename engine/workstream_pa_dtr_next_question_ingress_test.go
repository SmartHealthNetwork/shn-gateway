//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"github.com/golang-jwt/jwt/v5"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
)

// The provider DTR client's $next-question routes ride the package fetch's
// leg and route. Every check is made, in order, and a round that passes them
// all is forwarded to the address it names. These rows prove the checks, that
// no frame leaves the gateway on a refused path, and that a passing round is
// sent once.

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
		e.RequestFrames = append(e.RequestFrames, shnsdk.RequestFrameV1OpSub)
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

// Every path, at every level: a round failing a check is refused with the
// network primitive named, and nothing is sent; a round that passes every
// check is forwarded once, naming its address.
func TestDTRNextQuestionIngress_RefusesWhatFailsAndForwardsWhatPasses(t *testing.T) {
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
		// Every check passed: forwarded, on each family and form.
		"a Parameters round": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqParams(good), true, nil,
			http.StatusOK, forwardedRound, ""},
		"the payer's id percent-encoded": {"/dtr/payer/%70ayer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusOK, forwardedRound, ""},
		"an empty remainder": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), good, true, nil,
			http.StatusOK, forwardedRound, ""},
		"a clean remainder": {payer("2.2/" + nqTag + "/a-b/c.d~e/Questionnaire/$next-question"), good, true, nil,
			http.StatusOK, forwardedRound, ""},
		"the operation's segment encoded": {payer("2.2/" + nqTag + "/Questionnaire/%24next-question"), good, true, nil,
			http.StatusOK, forwardedRound, ""},
		// Another line the network defines is forwarded: the path's line is
		// only checked here; the forward's carried selection picks the route.
		"another defined line": {payer("2.0/" + nqTag + "/Questionnaire/$next-question"), good, true, nil,
			http.StatusOK, forwardedRound, ""},
		"fetch-server": {"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question", good, true, nil,
			http.StatusOK, forwardedRound, ""},
		"a member the system does not hold, carried": {payer("2.2/" + nqTag + "/Questionnaire/$next-question"), nqRound("Patient/MBR-UNKNOWN", ""), true, nil,
			http.StatusOK, forwardedRound, ""},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, h, got := nqExchange(t, level, row.sub, row.opts...)
				rec := nqPost(h, row.path, row.body)
				if row.msg == forwardedRound {
					if rec.Code != http.StatusOK {
						t.Fatalf("answer %d %s, want the round forwarded and answered", rec.Code, rec.Body)
					}
					forwardedOnce(t, env, row.path)
					if x := got.only(t); x.Outcome == ExchangeRefused {
						t.Fatalf("a forwarded round recorded as refused: %+v", x)
					}
					return
				}
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
// route's own answer, recorded as routing as the package route records it, at
// every level, before anything is sent.
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
			x := got.only(t)
			if refusesAt(level, RulePatientMixed) {
				nothingSent(t, env)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errNextQuestionMixed) {
					t.Fatalf("at %s answer %d %s, want 403 %q", level, rec.Code, rec.Body, errNextQuestionMixed)
				}
				// A content rule's refusal is recorded as conformance, the
				// participant's own opt-in, never as a network primitive.
				wantRefusal(t, x, http.StatusForbidden, RefusedByProviderGateway, RefusalConformance)
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("at %s answer %d %s, want the round carried to its payer", level, rec.Code, rec.Body)
			}
			forwardedOnce(t, env, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question")
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
func TestDTRNextQuestionIngress_APayerDeclaringOtherLegsIsForwardedTo(t *testing.T) {
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
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s, want the round carried to its payer", rec.Code, rec.Body)
				}
				forwardedOnce(t, env, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question")
				if x := got.only(t); x.Outcome == ExchangeRefused {
					t.Fatalf("a forwarded round recorded as refused: %+v", x)
				}
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
			if refusesAt(level, RulePatientMixed) {
				nothingSent(t, env)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("at %s answer %d %s, want 403", level, rec.Code, rec.Body)
				}
				wantRefusal(t, got.only(t), http.StatusForbidden, RefusedByProviderGateway, RefusalConformance)
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("at %s answer %d %s, want the round carried to its payer", level, rec.Code, rec.Body)
			}
			forwardedOnce(t, env, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question")
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

// The production mux mounts the $next-question routes beside the package
// route: each answers through the gateway's own handler, here refusing
// a call that carries no credential as the package route does, never with the
// mux's 404.
func TestDTRNextQuestionIngress_Mounted(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	for _, p := range []string{
		"/Questionnaire/$questionnaire-package",
		"/Questionnaire/$next-question",
		"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
	} {
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}")))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s answered %d %s on the production mux, want the package route's 401", p, w.Code, w.Body)
		}
	}
	// Each mounted pattern reaches the round handler, not the package's: the
	// pattern handleDTRIngress dispatches on is the one paRoutes mounts.
	for _, pat := range []string{patternDTRNextQuestionPayer, patternDTRNextQuestionFetchServer, patternDTRNextQuestionPlain} {
		if !slices.Contains(g.RoutePatternsForTest(), pat) {
			t.Fatalf("paRoutes does not mount %q, the pattern the round handler is dispatched on", pat)
		}
	}
}

// A round in a content coding the gateway does not decode is refused by the
// production mount, before the handler, as the package route's call is: 415
// naming gzip, with the same answer on every route.
func TestDTRNextQuestionIngress_AnUndecodedCodingIsThePackageRoutes415(t *testing.T) {
	priv, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	tok := postToken(t, g.Handler(), mintAssertion(t, priv, jwt.SigningMethodES384, validClaims("br-provider", g.ingressAuth.tokenURL(), ingressFixedClock()())))
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if tok.Code != http.StatusOK || json.Unmarshal(tok.Body.Bytes(), &tr) != nil || tr.AccessToken == "" {
		t.Fatalf("token: %d %s", tok.Code, tok.Body)
	}
	// A payer the routes' routing checks pass for, so the body is read: a
	// registered payer declaring framed DTR operations and v1opsub.
	g.cfg.Reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer",
		RequestFrames: []string{shnsdk.RequestFrameV1, shnsdk.RequestFrameV1Op, shnsdk.RequestFrameV1OpSub}})
	var want string
	for i, p := range []string{
		"/Questionnaire/$questionnaire-package",
		"/Questionnaire/$next-question",
		"/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
		"/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question",
	} {
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/fhir+json")
		req.Header.Set("Content-Encoding", "br")
		req.Header.Set("Authorization", "Bearer "+tr.AccessToken)
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnsupportedMediaType || w.Header().Get("Accept-Encoding") != "gzip" || !strings.Contains(w.Body.String(), errUnsupportedCoding) {
			t.Fatalf("%s: %d (Accept-Encoding %q) %s, want 415 naming gzip", p, w.Code, w.Header().Get("Accept-Encoding"), w.Body)
		}
		got := w.Result().Status + " " + w.Body.String()
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("%s answered\n%s\nwant the package route's\n%s", p, got, want)
		}
	}
}

// The rounds take the package route's own ingress credential: a call the
// package route refuses for its credential is refused on each $next-question
// route with the same status, answer and recorded class. The routes are
// served by the gateway's own handler, as the package route is.
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
				req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}"))
				if b := bearer(); b != "" {
					req.Header.Set("Authorization", b)
				}
				w := httptest.NewRecorder()
				g.Handler().ServeHTTP(w, req)
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
		req := httptest.NewRequest(http.MethodPost, testIngressBaseURL+p, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, req)
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

// forwardedRound marks a row whose round passes every check and is
// forwarded to its payer.
const forwardedRound = "(forwarded)"

// forwardedOnce fails unless exactly one frame left the gateway: a
// next-question round whose operationSubpath names the address path was
// called at (its family, line, tag and remainder, unescaped).
func forwardedOnce(t *testing.T, env *inProcessExchange, path string) {
	t.Helper()
	env.substrate.mu.Lock()
	names, req := slices.Clone(env.substrate.sentNames), slices.Clone(env.substrate.lastReq)
	env.substrate.mu.Unlock()
	if len(names) != 1 {
		t.Fatalf("frames sent: %v, want the one round", names)
	}
	hdr, _, err := shnsdk.DecodeHTTPFrame(req)
	if err != nil {
		t.Fatalf("the round was not framed: %v", err)
	}
	unescaped, err := url.PathUnescape(path)
	if err != nil {
		t.Fatal(err)
	}
	segs := strings.Split(strings.TrimSuffix(unescaped, "/Questionnaire/$next-question"), "/")
	want := "/" + segs[2] + "/" + strings.Join(segs[4:], "/")
	if hdr.Headers[shnsdk.FrameHeaderOperation] != shnsdk.FrameOperationNextQuestion || hdr.Headers[shnsdk.FrameHeaderOperationSubpath] != want {
		t.Fatalf("frame headers %v, want next-question naming %q", hdr.Headers, want)
	}
}

// The plain route is routed by the one adaptive address this gateway issued
// that the round's own Questionnaire carries, as if the client had called it:
// forwarded naming that address. The same address twice is one; two distinct
// ones, or none (an address on another gateway's base, or one in its own
// prefix it cannot read, among them), name no one payer and are refused as
// routing, with nothing sent. The one address is checked as a per-payer path
// is: a remainder that is not clean is refused as that path would be.
func TestDTRNextQuestionIngress_ThePlainRouteIsRoutedByTheIssuedAddress(t *testing.T) {
	const ingress = "https://provider-gw.example"
	issued := func(rest string) string { return ingress + "/dtr/payer/payer/2.2/" + nqTag + rest }
	round := func(addrs ...string) []byte {
		var exts []string
		for _, a := range addrs {
			exts = append(exts, `{"url":"`+questionnaireAdaptiveExtension+`","valueUrl":"`+a+`"}`)
		}
		return []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress",` +
			`"contained":[{"resourceType":"Questionnaire","id":"q","status":"active","extension":[` + strings.Join(exts, ",") + `]}],` +
			`"questionnaire":"#q","subject":{"reference":"Patient/` + prefetchMember + `"}}`)
	}
	for name, row := range map[string]struct {
		body      []byte
		forwarded string // the path it is routed as, "" = refused
		msg       string
		reason    refusalReason
	}{
		"one issued address":          {round(issued("/forms/a")), "/dtr/payer/payer/2.2/" + nqTag + "/forms/a/Questionnaire/$next-question", "", ""},
		"the same address twice":      {round(issued("/forms/a"), issued("/forms/a")), "/dtr/payer/payer/2.2/" + nqTag + "/forms/a/Questionnaire/$next-question", "", ""},
		"a Parameters round":          {nqParams(round(issued(""))), "/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question", "", ""},
		"a fetch-server address":      {round(ingress + "/dtr/fetch-server/payer/2.2/" + nqTag), "/dtr/fetch-server/payer/2.2/" + nqTag + "/Questionnaire/$next-question", "", ""},
		"two distinct addresses":      {round(issued("/forms/a"), issued("/forms/b")), "", errNextQuestionTwoAddresses, reasonNextQuestionTwoAddresses},
		"no address":                  {round(), "", errNextQuestionNoPayer, reasonNextQuestionNoPayer},
		"another gateway's address":   {round("https://other-gw.example/dtr/payer/payer/2.2/" + nqTag), "", errNextQuestionNoPayer, reasonNextQuestionNoPayer},
		"the payer's own address":     {round("https://payer.example/fhir/dtr"), "", errNextQuestionNoPayer, reasonNextQuestionNoPayer},
		"its own prefix, unreadable":  {round(ingress + "/dtr/payer/payer/two/" + nqTag), "", errNextQuestionNoPayer, reasonNextQuestionNoPayer},
		"a dot-dot segment":           {round(issued("/forms/../b")), "", errNextQuestionAddress, reasonNextQuestionAddress},
		"an encoded slash":            {round(issued("/a%2Fb")), "", errNextQuestionAddress, reasonNextQuestionAddress},
		"a trailing slash":            {round(issued("/forms/a/")), "", errNextQuestionAddress, reasonNextQuestionAddress},
		"an unclean and a clean copy": {round(issued("/forms/a/"), issued("/forms/a")), "", errNextQuestionTwoAddresses, reasonNextQuestionTwoAddresses},
	} {
		t.Run(name, func(t *testing.T) {
			env, h, got := nqExchange(t, EnforcementStrict, true)
			env.originator.cfg.IngressBaseURL = ingress + "/"
			rec := nqPost(h, "/Questionnaire/$next-question", row.body)
			if row.forwarded == "" {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), row.msg) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, row.msg)
				}
				nothingSent(t, env)
				if x := got.only(t); x.Rule != RefusalRouting || x.RefusalReason != string(row.reason) {
					t.Fatalf("recorded %q %q, want routing %q", x.Rule, x.RefusalReason, row.reason)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s, want the round forwarded", rec.Code, rec.Body)
			}
			forwardedOnce(t, env, row.forwarded)
		})
	}
}

// nqAnswer is a payer's answer to a round: the QuestionnaireResponse with its
// contained Questionnaire holding the round's adaptive address on the payer's
// base, the base and the address tagged.
func nqAnswer(env *inProcessExchange) {
	answer := `{"resourceType":"QuestionnaireResponse","status":"in-progress","contained":[` +
		adaptiveQ("q", `"valueUrl":"`+rekeyBase+`/forms/a"`) + `],"questionnaire":"#q","subject":{"reference":"Patient/` + prefetchMember + `"}}`
	env.payerReturns(LegResult{Response: testResponse([]byte(answer)), AnswerStatus: http.StatusOK,
		AddressHeaders: map[string]string{shnsdk.FrameHeaderOperationBase: rekeyBase, shnsdk.FrameHeaderOperationAddressTags: "/forms/a=" + rekeyTagA}})
}

const nqRoundAtA = "/dtr/payer/payer/2.2/" + nqTag + "/forms/a/Questionnaire/$next-question"

// A round's answer, through the route: the round's own address re-keyed (at
// the line its path names, which fixes the tag and the base) and E-12 named in
// X-SHN-Edited and in the record, which records the line the route was sealed
// at, never the path's; with module 2 off and on. The path names 2.1, which
// the carried selection does not pick here, so the two lines differ.
func TestDTRNextQuestionIngress_TheRoundsAnswerIsReKeyedAndDisclosed(t *testing.T) {
	for _, module2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "module 2 off", true: "module 2 on"}[module2], func(t *testing.T) {
			env, h, got := nqExchange(t, EnforcementNone, true)
			env.originator.cfg.TranslateIGLines = module2
			env.originator.cfg.IngressBaseURL = rekeyIngress + "/"
			nqAnswer(env)
			rec := nqPost(h, "/dtr/payer/payer/2.1/"+nqTag+"/forms/a/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"`+rekeyIngress+"/dtr/payer/payer/2.1/"+rekeyTagA+`/forms/a"`) {
				t.Fatalf("not re-keyed: %s", rec.Body)
			}
			if v := rec.Header().Values(EditedHeader); !slices.Equal(v, []string{"dtr-adaptive-address-rekey"}) {
				t.Fatalf("%s = %q", EditedHeader, v)
			}
			x := got.only(t)
			// The line recorded is the one the route was sealed at (the
			// carried selection's), never the path's.
			if !slices.Contains(x.Edits, string(relay.EditDTRAdaptiveAddressRekey)) || x.AnswerLine == "" || x.AnswerLine != "pa.dtr@"+x.ContractLine || x.AnswerLine == "pa.dtr@2.1" {
				t.Fatalf("recorded edits %v, answer line %q, contract line %q", x.Edits, x.AnswerLine, x.ContractLine)
			}
		})
	}
}

// A round's answer is judged at the participant's level as the package
// answer is, a check the participant opts into: at none it is relayed
// unjudged, at observe relayed with the defect recorded, and at strict a
// defect refuses it as conformance, unless the participant tolerates the
// payer (D122), read for a round, which carries no Coverage, as an identity
// it tolerates that routes to the round's payer. One that routes to another
// payer tolerates nothing here.
func TestDTRNextQuestionIngress_TheRoundsAnswerIsJudgedAndTolerated(t *testing.T) {
	cms := shnsdk.CMSPayerIdentity
	other := shnsdk.PayerIdentifier{System: cms.System, Value: "00999"}
	for name, row := range map[string]struct {
		level    ConformanceEnforcement
		tolerate []shnsdk.PayerIdentifier
		relayed  bool
	}{
		"at none, unjudged":                {EnforcementNone, nil, true},
		"at observe, recorded":             {EnforcementObserve, nil, true},
		"not tolerated":                    {EnforcementStrict, nil, false},
		"tolerated":                        {EnforcementStrict, []shnsdk.PayerIdentifier{cms}, true},
		"tolerating one routed elsewhere":  {EnforcementStrict, []shnsdk.PayerIdentifier{other}, false},
		"tolerating both, one routed here": {EnforcementStrict, []shnsdk.PayerIdentifier{cms, other}, true},
	} {
		t.Run(name, func(t *testing.T) {
			env, h, got := nqExchange(t, row.level, true)
			g := env.originator
			router, err := NewConfigPayerRouter([]PayerDirectoryEntry{
				{System: cms.System, Value: cms.Value, HolderID: env.payerID},
				{System: other.System, Value: other.Value, HolderID: "another-payer"},
			})
			if err != nil {
				t.Fatal(err)
			}
			g.cfg.PayerRouter = router
			g.cfg.ConformanceToleratePeers = row.tolerate
			invalid := &recordingValidator{valid: false}
			g.cfg.Validator = invalid
			g.cfg.ValidatorsByContractLine = contractLanes(map[string]shnsdk.Validator{"2.0": invalid, "2.1": invalid, "2.2": invalid})
			g.cfg.IngressBaseURL = rekeyIngress + "/"
			nqAnswer(env)
			var findings int
			g.cfg.Observer = func(e ObserverEvent) {
				if e.Kind == ConformanceObservedEvent {
					findings++
				}
			}
			rec := nqPost(h, nqRoundAtA, nqRound("Patient/"+prefetchMember, ""))
			g.drainObserveChecks()
			if judged := len(invalid.calls) != 0; judged != (row.level != EnforcementNone) {
				t.Fatalf("judged %v at %s", judged, row.level)
			}
			if row.level == EnforcementObserve && findings == 0 {
				t.Fatal("observe recorded no finding for the defect")
			}
			if row.relayed != (rec.Code == http.StatusOK) {
				t.Fatalf("answer %d %s, want relayed %v", rec.Code, rec.Body, row.relayed)
			}
			x := got.only(t)
			if !row.relayed {
				if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "forms/a") || x.Rule != RefusalConformance {
					t.Fatalf("answer %d %s recorded %q, want the 502 conformance refusal and no answer", rec.Code, rec.Body, x.Rule)
				}
			} else if x.Outcome == ExchangeRefused {
				t.Fatalf("a relayed answer recorded as refused: %+v", x)
			}
		})
	}
}

// flippingSoR is a system of record that, on the round's patient read, runs
// flip first: a registry change between the ingress's checks and the send.
type flippingSoR struct {
	SystemOfRecord
	flip func()
}

func (s flippingSoR) ResolvePatient(member string) (string, Demo, bool) {
	s.flip()
	return s.SystemOfRecord.ResolvePatient(member)
}

// A round's address is sent only to a payer gateway that reads it: one that
// stops declaring v1opsub after the ingress checked it is refused as the
// ingress refuses it (422, routing), with nothing sent; and the round trip
// itself refuses an address without v1opsub, or without the operation.
func TestDTRNextQuestionIngress_ARoundsAddressGoesOnlyToAGatewayThatReadsIt(t *testing.T) {
	t.Run("the payer's gateway stops declaring v1opsub", func(t *testing.T) {
		env, h, got := nqExchange(t, EnforcementNone, true)
		g := env.originator
		g.cfg.SoR = flippingSoR{SystemOfRecord: g.cfg.SoR, flip: func() {
			e, _ := g.cfg.Reg.Lookup(env.payerID)
			e.RequestFrames = slices.DeleteFunc(slices.Clone(e.RequestFrames), func(f string) bool { return f == shnsdk.RequestFrameV1OpSub })
			g.cfg.Reg.Set(env.payerID, e)
		}}
		rec := nqPost(h, nqRoundAtA, nqRound("Patient/"+prefetchMember, ""))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errNextQuestionNoSubpath) {
			t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, errNextQuestionNoSubpath)
		}
		nothingSent(t, env)
		if x := got.only(t); x.Rule != RefusalRouting || x.RefusalReason != string(reasonNextQuestionNoSubpath) {
			t.Fatalf("recorded %q %q, want routing %q", x.Rule, x.RefusalReason, reasonNextQuestionNoSubpath)
		}
	})
	for name, row := range map[string]struct {
		sub       bool
		operation string
	}{
		"a payer gateway without v1opsub": {false, shnsdk.FrameOperationNextQuestion},
		"an address without an operation": {true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			env, _, _ := nqExchange(t, EnforcementNone, row.sub)
			_, err := env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "dtr-questionnaire-fetch", "pci-1", "corr-1", "",
				Content{WorkstreamType: workstreamPA, ProfileID: "pa.dtr@2.2", Payload: testRequest(nqRound("Patient/"+prefetchMember, "")),
					Operation: row.operation, Subpath: "/payer/2.2/" + nqTag})
			if !errors.Is(err, errRoundAddressUnsupported) {
				t.Fatalf("err %v, want errRoundAddressUnsupported", err)
			}
			nothingSent(t, env)
		})
	}
}

// A round's lines are the participant's own setting's to refuse: toward a
// payer that declares no DTR line this gateway shares, the round is refused
// 422 as routing only under REQUIRE_SHARED_LINE, with nothing sent, as the
// package route refuses its request; without the setting the same pair is
// carried, at every level.
func TestDTRNextQuestionIngress_ARoundIsRefusedForItsLinesOnlyUnderRequireSharedLine(t *testing.T) {
	t.Parallel()
	path := "/dtr/payer/payer/2.2/" + nqTag + "/Questionnaire/$next-question"
	for _, level := range allLevels {
		for _, shared := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/require-shared-line=%v", level, shared), func(t *testing.T) {
				env, h, got := nqExchange(t, level, true, func(c *Config) { c.RequireSharedLine = shared })
				declareRecipientVersions(t, env, []string{"pa.crd@2.0"})
				rec := nqPost(h, path, nqRound("Patient/"+prefetchMember, ""))
				if !shared {
					if rec.Code != http.StatusOK {
						t.Fatalf("answer %d %s, want the round carried", rec.Code, rec.Body)
					}
					forwardedOnce(t, env, path)
					return
				}
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("answer %d %s, want 422", rec.Code, rec.Body)
				}
				nothingSent(t, env)
				wantRefusal(t, got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
			})
		}
	}
}

// nqPackageBody is a $questionnaire-package input for the in-process
// exchange's covered member, routed by its Coverage to the payer.
const nqPackageBody = `{"resourceType":"Parameters","parameter":[{"name":"questionnaire","valueCanonical":"urn:test:questionnaire"},` +
	`{"name":"coverage","resource":{"resourceType":"Coverage","id":"cov1","beneficiary":{"reference":"Patient/MBR-COVERED"},` +
	`"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}}]}`

// authorizeAsked is what one call asked the Authorization Framework for.
type authorizeAsked struct{ Frame, Operation string }

// askedFor records each /authorize request the exchange's gateway makes, and
// answers with the ordinary signed token, or with refuse when it is set.
func askedFor(env *inProcessExchange, refuse *http.Response) *[]authorizeAsked {
	var got []authorizeAsked
	fault := func(req *http.Request) (*http.Response, error) {
		var a authorizeAsked
		_ = json.NewDecoder(req.Body).Decode(&a)
		got = append(got, a)
		return refuse, nil
	}
	env.substrate.mu.Lock()
	env.substrate.authorizeFaults = []func(*http.Request) (*http.Response, error){fault, fault, fault, fault}
	env.substrate.mu.Unlock()
	return &got
}

// A round is authorized as the package fetch is: the same leg, operation and
// frame asked of the Authorization Framework. And when the framework refuses
// the leg for this payer (a leg it does not take), the round is refused as the
// package fetch is: the same status, answer and recorded class, with nothing
// delivered to the payer.
func TestDTRNextQuestionIngress_ARoundTakesThePackageFetchsAuthority(t *testing.T) {
	refused := func() *http.Response {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"forbidden"}`))}
	}
	type outcome struct {
		asked []authorizeAsked
		got   string
	}
	run := func(t *testing.T, round bool, refuse bool) outcome {
		t.Helper()
		env, h, recs := nqExchange(t, EnforcementObserve, true)
		var r *http.Response
		if refuse {
			r = refused()
		}
		asked := askedFor(env, r)
		var rec *httptest.ResponseRecorder
		if round {
			rec = nqPost(h, "/dtr/payer/payer/2.2/"+nqTag+"/Questionnaire/$next-question", nqRound("Patient/"+prefetchMember, ""))
		} else {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /Questionnaire/$questionnaire-package", env.originator.ingressRoute(RouteDTR))
			rec = nqPost(mux, "/Questionnaire/$questionnaire-package", []byte(nqPackageBody))
		}
		if refuse {
			env.substrate.mu.Lock()
			hits := env.substrate.routeHits
			env.substrate.mu.Unlock()
			if hits != 0 {
				t.Fatalf("a leg the framework refused reached the Hub (%d routes)", hits)
			}
			x := recs.only(t)
			return outcome{*asked, fmt.Sprintf("%d %s recorded %s", rec.Code, rec.Body.String(), x.Rule)}
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s, want it carried", rec.Code, rec.Body)
		}
		return outcome{*asked, ""}
	}
	pkg, nq := run(t, false, false), run(t, true, false)
	if len(pkg.asked) != 1 || len(nq.asked) != 1 || pkg.asked[0].Operation == "" || nq.asked[0] != pkg.asked[0] {
		t.Fatalf("the round asked the Authorization Framework for %+v, the package fetch for %+v", nq.asked, pkg.asked)
	}
	pkgRefused, nqRefused := run(t, false, true), run(t, true, true)
	if nqRefused.got != pkgRefused.got {
		t.Fatalf("a refused leg: the round answered\n%s\nthe package fetch\n%s", nqRefused.got, pkgRefused.got)
	}
}

// The provider ingress CapabilityStatement (GET /metadata) states exactly the
// Questionnaire operations the ingress mounts, in both directions: each
// operation it states is served at [base]/Questionnaire/$<operation>, and each
// mounted route that serves a Questionnaire operation serves one it states. A
// route, whatever its method, serves the operation its path ends in when a
// Questionnaire segment comes before it, at the type or at an instance
// (Questionnaire/$<operation>, Questionnaire/{id}/$<operation>). A route
// that leaves its operation or its type to a wildcard (a wildcard last
// segment, or one before its $<operation>) names neither in its pattern, so
// each one is classified here and an unclassified one fails the row: the
// adaptive address family at /dtr/payer/{payer}/{rest...} names its operation inside
// {rest...}, which the round handler admits only when it ends in
// Questionnaire/$next-question (the "no operation" row of
// TestDTRNextQuestionIngress_RefusesWhatFailsAndForwardsWhatPasses).
func TestProviderIngressStatement_QuestionnaireOperationsAreTheMountedRoutes(t *testing.T) {
	_, pub := newTestClientKey(t)
	g := gatewayWithAuth(t, "br-provider", pub)
	rr := httptest.NewRecorder()
	g.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metadata", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metadata: want 200, got %d %s", rr.Code, rr.Body)
	}
	var cs struct {
		Rest []struct {
			Resource []struct {
				Type      string `json:"type"`
				Operation []struct {
					Name string `json:"name"`
				} `json:"operation"`
			} `json:"resource"`
		} `json:"rest"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cs); err != nil {
		t.Fatal(err)
	}
	stated := map[string]bool{}
	entries := 0
	for _, rest := range cs.Rest {
		for _, res := range rest.Resource {
			if res.Type != "Questionnaire" {
				continue
			}
			entries++
			for _, op := range res.Operation {
				stated[op.Name] = true
			}
		}
	}
	if entries != 1 {
		t.Fatalf("the statement has %d Questionnaire entries, want 1", entries)
	}

	// The Questionnaire operation each mounted route serves.
	mounted := map[string][]string{}
	// wildcardOperation is the Questionnaire operation each route that leaves
	// its operation or type to a wildcard serves, "" for none.
	wildcardOperation := map[string]string{
		patternDTRNextQuestionPayer: shnsdk.FrameOperationNextQuestion,
		// A CDS Hooks service.
		"POST /cds-services/{id}": "",
		// CDex $submit-attachment at a payer's return path.
		"POST /cdex/payer/{payer}":                    "",
		"POST /cdex/payer/{payer}/$submit-attachment": "",
	}
	wild := func(seg string) bool { return strings.HasPrefix(seg, "{") }
	for _, pattern := range g.RoutePatternsForTest() {
		path := pattern
		if _, p, ok := strings.Cut(pattern, " "); ok {
			path = p
		}
		// A pattern's path starts with "/", so a last segment that is not
		// the only one has one before it.
		segs := strings.Split(path, "/")
		n := len(segs)
		last := segs[n-1]
		classify := wild(last)
		if strings.HasPrefix(last, "$") && n >= 2 {
			prev := segs[n-2]
			if prev == "Questionnaire" || (wild(prev) && n >= 3 && segs[n-3] == "Questionnaire") {
				op := strings.TrimPrefix(last, "$")
				mounted[op] = append(mounted[op], pattern)
				continue
			}
			classify = wild(prev)
		}
		if !classify {
			continue
		}
		op, ok := wildcardOperation[pattern]
		if !ok {
			t.Errorf("the provider ingress mounts %s, whose wildcard may name a Questionnaire operation or its type: classify it in this row", pattern)
			continue
		}
		if op != "" {
			mounted[op] = append(mounted[op], pattern)
		}
	}
	for pattern := range wildcardOperation {
		if !slices.Contains(g.RoutePatternsForTest(), pattern) {
			t.Errorf("this row classifies %s, which the provider ingress does not mount", pattern)
		}
	}
	// Control: the ingress mounts both DTR operations, so neither direction
	// below is vacuous.
	for _, op := range []string{shnsdk.FrameOperationQuestionnairePackage, shnsdk.FrameOperationNextQuestion} {
		if len(mounted[op]) == 0 {
			t.Fatalf("control: the provider ingress mounts no route for %s: %v", op, g.RoutePatternsForTest())
		}
	}

	for op := range stated {
		if !slices.Contains(mounted[op], "POST /Questionnaire/$"+op) {
			t.Errorf("the statement lists Questionnaire $%s, which the ingress does not serve at POST /Questionnaire/$%s (mounted for it: %v)", op, op, mounted[op])
		}
	}
	for op, patterns := range mounted {
		if !stated[op] {
			t.Errorf("the ingress serves Questionnaire $%s at %v, which the statement does not list", op, patterns)
		}
	}
}
