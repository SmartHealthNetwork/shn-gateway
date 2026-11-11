//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The full-tier routes' relayed success answers (PAS, DTR, CRD) keep the
// media type the payer's system stated, as the evolving routes' do
// (workstream_pa_answer_mediatype_test.go): each route at every conformance
// level, its re-keys included, and CRD's rule for an older payer gateway's
// application/fhir+json stamp.

// pasAnswerRun serves leg at level, the payer's gateway framing answer with
// the stated type, the gateway tuned by tune, and returns the participant
// system's answer.
func pasAnswerRun(t *testing.T, leg carriedAnswerLeg, level ConformanceEnforcement, answer []byte, stated string, tune func(*Gateway)) *httptest.ResponseRecorder {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	if tune != nil {
		tune(env.originator)
	}
	env.payerReturns(framedAnswer(answer, stated, http.StatusOK))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, leg.path, strings.NewReader(leg.body(t)))
	req.Header.Set("Content-Type", "application/fhir+json")
	env.originator.ingressRoute(leg.route)(rec, req)
	env.originator.drainObserveChecks()
	return rec
}

// The PAS $submit, update and $inquire answers keep the payer's stated type:
// an answer carried as sent, and a pended one whose documentation return
// address is re-keyed to the network (E-10).
func TestPASIngress_WritesThePayersStatedAnswerType(t *testing.T) {
	t.Parallel()
	returnBase, err := cdexroute.ReturnBase(pendRekeyBase, "payer")
	if err != nil {
		t.Fatal(err)
	}
	for _, leg := range carriedAnswerLegs() {
		pended := func(t *testing.T) []byte {
			return withEntry(t, levelPASPayerAnswer, pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
		}
		if leg.leg == "pas-claim-inquire" {
			pended = func(t *testing.T) []byte {
				return withEntry(t, levelInquiryPayerAnswer(t), pendRekeyTask(`"valueUrl":"https://payer.example/fhir"`))
			}
		}
		for _, row := range statedAnswerTypes {
			for _, level := range allLevels {
				t.Run(leg.leg+"/as sent/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
					answer := leg.answer(t)
					rec := pasAnswerRun(t, leg, level, answer, row.stated, nil)
					wantWritten(t, rec, http.StatusOK, row.written, relayedPASAnswer(t, answer))
				})
				t.Run(leg.leg+"/pend re-keyed/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
					answer := pended(t)
					rec := pasAnswerRun(t, leg, level, answer, row.stated, func(g *Gateway) {
						g.cfg.IngressBaseURL = pendRekeyBase
						payer, ok := g.cfg.Reg.Lookup("payer")
						if !ok {
							t.Fatal("fixture: the routed payer is not in the registry")
						}
						payer.Legs = []string{legCDexSubmit}
						g.cfg.Reg.Set("payer", payer)
					})
					wantWritten(t, rec, http.StatusOK, row.written, nil)
					// control: the re-key was made, so this row is the edited answer's.
					if !onlyPayerURLsRekeyed(t, answer, rec.Body.Bytes(), returnBase) {
						t.Fatalf("control: want every payer-url re-keyed to %s and nothing else changed, got %s", returnBase, rec.Body)
					}
					if h := rec.Header().Values(EditedHeader); len(h) != 1 || !strings.HasPrefix(h[0], "pas-pend-return-rekey") {
						t.Fatalf("control: %s = %q, want the pend re-key named", EditedHeader, h)
					}
				})
			}
		}
	}
}

// dtrPackageAnswerRun posts the DTR client's package request through the
// provider's ingress at level, module 2 on or off, the payer's gateway
// declaring v1opsub and framing answer with the stated type and, when tagged,
// the adaptive address's tag (E-12).
func dtrPackageAnswerRun(t *testing.T, level ConformanceEnforcement, module2, tagged bool, answer []byte, stated string) *httptest.ResponseRecorder {
	t.Helper()
	env := newInProcessExchange(t)
	g := env.originator
	g.cfg.TranslateIGLines = module2
	g.cfg.SoR = newPrefetchSoR().sor()
	g.cfg.ConformanceEnforcement = level
	g.cfg.IngressBaseURL = rekeyIngress + "/"
	declareFramedDTR(t, env, true)
	e, _ := g.cfg.Reg.Lookup(env.payerID)
	e.RequestFrames = append(slices.Clone(e.RequestFrames), shnsdk.RequestFrameV1OpSub)
	g.cfg.Reg.Set(env.payerID, e)
	lr := framedAnswer(answer, stated, http.StatusOK)
	if tagged {
		lr.AddressHeaders = map[string]string{shnsdk.FrameHeaderOperationBase: rekeyBase, shnsdk.FrameHeaderOperationAddressTags: "/forms/a=" + rekeyTagA}
	}
	env.payerReturns(lr)
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(dtrFixture(t, "dtr-package-params-2.0.json")))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	g.ingressRoute(RouteDTR)(rec, req)
	g.drainObserveChecks()
	return rec
}

// The DTR $questionnaire-package answer keeps the payer's stated type, with
// module 2 off and on: an answer carried as sent, and one whose adaptive
// address is re-keyed to the network (E-12).
func TestDTRIngress_WritesThePayersStatedPackageAnswerType(t *testing.T) {
	t.Parallel()
	answer := packageOf(adaptiveQ("a", `"valueUrl":"`+rekeyBase+`/forms/a"`))
	for _, module2 := range []bool{false, true} {
		m2 := map[bool]string{false: "module 2 off", true: "module 2 on"}[module2]
		for _, row := range statedAnswerTypes {
			for _, level := range allLevels {
				t.Run(m2+"/as sent/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
					rec := dtrPackageAnswerRun(t, level, module2, false, answer, row.stated)
					wantWritten(t, rec, http.StatusOK, row.written, answer)
				})
				t.Run(m2+"/re-keyed/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
					rec := dtrPackageAnswerRun(t, level, module2, true, answer, row.stated)
					wantWritten(t, rec, http.StatusOK, row.written, nil)
					// control: the re-key was made, so this row is the edited answer's.
					if !strings.Contains(rec.Body.String(), rekeyIngress+"/dtr/payer/payer/") {
						t.Fatalf("control: not re-keyed: %s", rec.Body)
					}
					if got := rec.Header().Values(EditedHeader); !slices.Equal(got, []string{"dtr-adaptive-address-rekey"}) {
						t.Fatalf("control: %s = %q", EditedHeader, got)
					}
				})
			}
		}
	}
}

// A DTR package answer module 2 renames into this gateway's line (E-08) keeps
// the payer's stated type, as one carried as sent does, at every level.
func TestDTRIngress_ARenamedPackageAnswerKeepsThePayersStatedType(t *testing.T) {
	t.Parallel()
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false, "operationOutcome")
	want := e08Bridged(t, answer, "2.0", "2.2")
	if bytes.Equal(want, answer) {
		t.Fatal("fixture: nothing to rename")
	}
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run(statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				r := e08RowFramed(t, level, declares22, framedAnswer(answer, row.stated, http.StatusOK),
					nil, nil, nil, nil, dtrFixture(t, "dtr-package-params-2.0.json"))
				// control: E-08 renamed it, so this row is the translated answer's.
				r.wantAnswered(t, want, true)
				wantWritten(t, r.rec, http.StatusOK, row.written, want)
			})
		}
	}
}

// E-08 keeps the payer's stated type on the payload it gives the writer,
// whether it carries the answer as sent or renames it.
func TestDTRAnswerBridge_KeepsTheStatedTypeOnItsPayload(t *testing.T) {
	t.Parallel()
	answer := e08Answer("PackageBundle", []string{"Questionnaire", "QuestionnaireResponse"}, false, "operationOutcome")
	const stated = "application/json; charset=utf-8"
	env := newInProcessExchange(t)
	// A gateway declaring two DTR lines carries every answer as sent.
	carried, _, status, msg := chainBridge{g: env.originator}.carryDTRAnswer(context.Background(), answer, stated, "pa.dtr@2.0", declaresBoth, ExchangeIdentity{})
	if status != 0 || carried.ContentType() != stated {
		t.Fatalf("carried as sent: status %d %q, type %q, want %q", status, msg, carried.ContentType(), stated)
	}
	plan := planDTRAnswerNames(answer, "2.2")
	if len(plan.Renames) == 0 {
		t.Fatal("fixture: nothing to rename")
	}
	renamed, err := renameDTRAnswer(answer, stated, plan, func(qr []byte) ([]byte, error) { return qr, nil })
	if err != nil || renamed.ContentType() != stated {
		t.Fatalf("renamed: %v, type %q, want %q", err, renamed.ContentType(), stated)
	}
}

// nqTypedAnswer has the payer's gateway frame a $next-question round's answer
// with the stated type and, when tagged, the adaptive address's tag (E-12).
func nqTypedAnswer(env *inProcessExchange, answer []byte, stated string, tagged bool) {
	lr := framedAnswer(answer, stated, http.StatusOK)
	if tagged {
		lr.AddressHeaders = map[string]string{shnsdk.FrameHeaderOperationBase: rekeyBase, shnsdk.FrameHeaderOperationAddressTags: "/forms/a=" + rekeyTagA}
	}
	env.payerReturns(lr)
}

// The DTR $next-question round's answer keeps the payer's stated type: an
// answer carried as sent, and one whose adaptive address is re-keyed to the
// network (E-12).
func TestDTRNextQuestionIngress_WritesThePayersStatedAnswerType(t *testing.T) {
	t.Parallel()
	answer := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","contained":[` +
		adaptiveQ("q", `"valueUrl":"`+rekeyBase+`/forms/a"`) + `],"questionnaire":"#q","subject":{"reference":"Patient/` + prefetchMember + `"}}`)
	path := "/dtr/payer/payer/2.1/" + nqTag + "/forms/a/Questionnaire/$next-question"
	for _, row := range statedAnswerTypes {
		for _, level := range allLevels {
			t.Run("as sent/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				env, h, got := nqExchange(t, level, true)
				env.originator.cfg.IngressBaseURL = rekeyIngress + "/"
				nqTypedAnswer(env, answer, row.stated, false)
				rec := nqPost(h, path, nqRound("Patient/"+prefetchMember, ""))
				wantWritten(t, rec, http.StatusOK, row.written, answer)
				got.only(t)
			})
			t.Run("re-keyed/"+statedTypeName(row.stated)+"/"+level.String(), func(t *testing.T) {
				env, h, got := nqExchange(t, level, true)
				env.originator.cfg.IngressBaseURL = rekeyIngress + "/"
				nqTypedAnswer(env, answer, row.stated, true)
				rec := nqPost(h, path, nqRound("Patient/"+prefetchMember, ""))
				wantWritten(t, rec, http.StatusOK, row.written, nil)
				// control: the re-key was made, so this row is the edited answer's.
				if !strings.Contains(rec.Body.String(), `"`+rekeyIngress+"/dtr/payer/payer/2.1/"+rekeyTagA+`/forms/a"`) {
					t.Fatalf("control: not re-keyed: %s", rec.Body)
				}
				if v := rec.Header().Values(EditedHeader); !slices.Equal(v, []string{"dtr-adaptive-address-rekey"}) {
					t.Fatalf("control: %s = %q", EditedHeader, v)
				}
				got.only(t)
			})
		}
	}
}

// A CDS Hooks answer keeps the payer's stated type, with one reading kept from
// before v0.57.0, when a payer's gateway stamped application/fhir+json on
// every success frame whatever its system sent: a stated
// application/fhir+json is written as stated only from a payer whose
// registry entry advertises the v1opsub request frame (its gateway is v1.8.0
// or later, so the type is its system's), and as application/json, the CDS
// Hooks answer's own type, from any other. Every other type is written as
// stated either way, and an unstated one is written application/json.
func TestCRDIngress_StatedFHIRTypeIsWrittenOnlyFromAV1OpSubPayer(t *testing.T) {
	t.Parallel()
	answer := []byte(`{"cards":[]}`)
	for _, row := range []struct {
		stated  string
		v1opsub bool
		written string
	}{
		{"application/fhir+json", true, "application/fhir+json"},
		{"application/fhir+json", false, "application/json"},
		{"application/fhir+json; fhirVersion=4.0", true, "application/fhir+json; fhirVersion=4.0"},
		{"application/fhir+json; fhirVersion=4.0", false, "application/json"},
		{"text/json", true, "text/json"},
		{"text/json", false, "text/json"},
		{"application/json", true, "application/json"},
		{"application/json", false, "application/json"},
		{"", true, "application/json"},
		{"", false, "application/json"},
	} {
		for _, level := range allLevels {
			name := statedTypeName(row.stated) + "/" + map[bool]string{true: "v1opsub", false: "no v1opsub"}[row.v1opsub] + "/" + level.String()
			t.Run(name, func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				e, ok := env.originator.cfg.Reg.Lookup(env.payerID)
				if !ok {
					t.Fatal("fixture: the payer is not in the registry")
				}
				// The two entries differ only in v1opsub.
				e.RequestFrames = shnsdk.SupportedRequestFrames()
				if row.v1opsub {
					e.RequestFrames = append(e.RequestFrames, shnsdk.RequestFrameV1OpSub)
				}
				env.originator.cfg.Reg.Set(env.payerID, e)
				// control: the registry entry under test has, or has not, v1opsub.
				held, _ := env.originator.cfg.Reg.Lookup(env.payerID)
				if shnsdk.SupportsRequestFrameV1OpSub(held.RequestFrames) != row.v1opsub {
					t.Fatalf("control: the payer's entry advertises %v, want v1opsub %v", held.RequestFrames, row.v1opsub)
				}
				env.payerReturns(framedAnswer(answer, row.stated, http.StatusOK))
				rec := ingressAt(t, env, "shn-order-select", conformantCRDRequest("MBR-COVERED"))
				wantWritten(t, rec, http.StatusOK, row.written, answer)
			})
		}
	}
}

// A payer the registry does not hold is not known to state its system's own
// type, whatever another holder advertises.
func TestFramesItsSystemsAnswerType_AnUnknownPayerIsNot(t *testing.T) {
	env := newInProcessExchange(t)
	e, _ := env.originator.cfg.Reg.Lookup(env.payerID)
	e.RequestFrames = append(shnsdk.SupportedRequestFrames(), shnsdk.RequestFrameV1OpSub)
	env.originator.cfg.Reg.Set(env.payerID, e)
	// control: a holder that advertises v1opsub is read as stating it.
	if !env.originator.framesItsSystemsAnswerType(env.payerID) {
		t.Fatal("control: the v1opsub payer is not read as stating its system's type")
	}
	if env.originator.framesItsSystemsAnswerType("no-such-payer") {
		t.Fatal("a payer the registry does not hold is read as stating its system's type")
	}
}

// A CDS Hooks answer is written with the type the payer's system stated when a
// header can carry it, else application/json, its own type. A stated
// application/fhir+json is read as an older payer gateway's blanket stamp, and
// written application/json, only when the payer's gateway is not known to
// state its system's own type (systemStated false).
func TestCDSAnswerMediaType(t *testing.T) {
	for _, row := range []struct {
		stated       string
		systemStated bool
		want         string
	}{
		{"application/fhir+json", false, "application/json"},
		{"application/fhir+json", true, "application/fhir+json"},
		{"application/fhir+json; fhirVersion=4.0", false, "application/json"},
		{"application/fhir+json; fhirVersion=4.0", true, "application/fhir+json; fhirVersion=4.0"},
		{"text/json", false, "text/json"},
		{"text/json", true, "text/json"},
		{"application/json; charset=utf-8", false, "application/json; charset=utf-8"},
		{"text/html; charset=utf-8", true, "text/html; charset=utf-8"},
		{"", false, "application/json"},
		{"", true, "application/json"},
		{"not a type", true, "application/json"},
		{"text/", false, "application/json"},
		{"application/json\r\nX-Injected: 1", true, "application/json"},
		{"application/fhir+json; x=" + strings.Repeat("a", 257), true, "application/json"},
		// One that parses but no header can carry: earlier releases wrote it.
		{"text/json; x=" + strings.Repeat("a", 257), false, "application/json"},
	} {
		if got := cdsAnswerMediaType(row.stated, row.systemStated); got != row.want {
			t.Errorf("cdsAnswerMediaType(%q, %v) = %q, want %q", row.stated, row.systemStated, got, row.want)
		}
	}
}
