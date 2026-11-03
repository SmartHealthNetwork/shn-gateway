//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/fhirseed"
	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
)

// The questionnaire fill is SHN's own edit, made after the coverage check and
// the questionnaire legs were carried. A filled QuestionnaireResponse about
// another patient than the bound one, or for another questionnaire than the
// one fetched, fails this gateway's own check of its fill: a fidelity refusal
// at the verify stage, 422, the PAS claim that would carry it never sent, and
// the answer says which legs were carried. A fill that matches is carried.

// populatedQRRewrite is the gateway's populator with its fill rewritten by
// edit, or failing with err.
type populatedQRRewrite struct {
	inner Populator
	edit  func(qr []byte, pc PopulateContext) []byte
	err   error
}

func (p populatedQRRewrite) Populate(ctx context.Context, packageJSON []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	if p.err != nil {
		return nil, nil, p.err
	}
	qr, fill, err := p.inner.Populate(ctx, packageJSON, pc)
	if err != nil || p.edit == nil {
		return qr, fill, err
	}
	return p.edit(qr, pc), fill, nil
}

// qrField sets one top-level field of a QuestionnaireResponse.
func qrField(t *testing.T, qr []byte, field string, value any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(qr, &m); err != nil {
		t.Fatalf("filled QR: %v", err)
	}
	m[field] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// qrWithout removes one top-level field of a QuestionnaireResponse.
func qrWithout(t *testing.T, qr []byte, field string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(qr, &m); err != nil {
		t.Fatalf("filled QR: %v", err)
	}
	if _, ok := m[field]; !ok {
		t.Fatalf("filled QR states no %s to remove", field)
	}
	delete(m, field)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOrigination_FilledQRFailingItsOwnCheckIsRefusedOnFidelity(t *testing.T) {
	type site struct {
		name, route string
		setup       func(t *testing.T) (*Gateway, string)
	}
	sites := []site{
		{"order select", "POST /scenario/uc04", func(t *testing.T) (*Gateway, string) {
			g, _ := uc04RouteFixture(t, "2.0")
			mountOriginationRoutes(g)
			return g, testRouteBody(g, "/scenario/uc04", "")
		}},
		{"order dispatch", "POST /scenario/dispatch", func(t *testing.T) (*Gateway, string) {
			fix := newMBROXDispatchFixture(t)
			mountOriginationRoutes(fix.gw)
			return fix.gw, testRouteBody(fix.gw, "/scenario/dispatch", `"member":"MBR-OX"`)
		}},
	}
	type fill struct {
		name string
		// want is the refusal, "" for a fill that is carried.
		want string
		rw   func(t *testing.T) populatedQRRewrite
	}
	fills := []fill{
		{"foreign subject", "populated QR subject does not match patient", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, _ PopulateContext) []byte {
				return qrField(t, qr, "subject", map[string]string{"reference": "Patient/MBR-SOMEONE-ELSE"})
			}}
		}},
		{"foreign canonical", "populated QR questionnaire does not match canonical", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, _ PopulateContext) []byte {
				return qrField(t, qr, "questionnaire", "https://example.test/Questionnaire/another")
			}}
		}},
		// A fill that names no subject, or no questionnaire, is refused as
		// one naming another: nobody supplied the value it lacks.
		{"no subject", "populated QR subject does not match patient", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, _ PopulateContext) []byte {
				return qrWithout(t, qr, "subject")
			}}
		}},
		{"no questionnaire", "populated QR questionnaire does not match canonical", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, _ PopulateContext) []byte {
				return qrWithout(t, qr, "questionnaire")
			}}
		}},
		{"foreign subject the operated engine returned", "populated QR subject does not match patient", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{err: errPopulateForeignSubject}
		}},
		// Matching fills are carried, however they state it: the bound
		// patient's reference with a display beside it, the canonical with a
		// version.
		{"bound subject with a display", "", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, pc PopulateContext) []byte {
				return qrField(t, qr, "subject", map[string]string{"reference": pc.PatientRef, "display": "the member"})
			}}
		}},
		{"canonical with a version", "", func(t *testing.T) populatedQRRewrite {
			return populatedQRRewrite{edit: func(qr []byte, _ PopulateContext) []byte {
				url, err := questionnaireResponseCanonical(qr)
				if err != nil || url == "" {
					t.Fatalf("filled QR states no questionnaire: %v", err)
				}
				return qrField(t, qr, "questionnaire", url+"|9.9.9")
			}}
		}},
	}
	for _, s := range sites {
		for _, f := range fills {
			for _, level := range allLevels {
				t.Run(s.name+"/"+f.name+"/"+level.String(), func(t *testing.T) {
					g, body := s.setup(t)
					g.cfg.ConformanceEnforcement = level
					rw := f.rw(t)
					rw.inner = g.paState().populator
					g.paState().populator = rw
					answered := recordAnsweredLegs(g)
					var mu sync.Mutex
					var failed []ObserverEvent
					observe := g.cfg.Observer
					g.cfg.Observer = func(e ObserverEvent) {
						if e.Kind == "leg.failed" {
							mu.Lock()
							failed = append(failed, e)
							mu.Unlock()
						}
						observe(e)
					}
					rec, recs := serveRecorded(t, g, s.route, body)
					legs := answered.list()
					if f.want == "" {
						if rec.Code == http.StatusUnprocessableEntity || !legAttempted(legs, "pas-claim") {
							t.Fatalf("%s = %d %s legs %v, want the fill carried to the PAS claim", s.route, rec.Code, rec.Body, legs)
						}
						if r := onlyRecord(t, recs); r.Rule == RefusalFidelity {
							t.Fatalf("a matching fill recorded a fidelity refusal at %q", r.RefusalStage)
						}
						return
					}
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), f.want) {
						t.Fatalf("%s = %d %s, want 422 %q", s.route, rec.Code, rec.Body, f.want)
					}
					if legAttempted(legs, "pas-claim") || !legAttempted(legs, "dtr-questionnaire-fetch") {
						t.Fatalf("legs %v: want the questionnaire fetched and no PAS claim", legs)
					}
					wantCarried(t, rec.Body.String(), legs...)
					mu.Lock()
					defer mu.Unlock()
					if len(failed) != 1 || failed[0].LegType != "pas-claim" || failed[0].Detail != f.want {
						t.Fatalf("leg.failed %+v, want one for the pas-claim leg naming %q", failed, f.want)
					}
					r := onlyRecord(t, recs)
					wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
					if r.RefusalStage != vocab.RefusalStageVerify {
						t.Fatalf("stage %q, want %s: SHN's fill failed its own check", r.RefusalStage, vocab.RefusalStageVerify)
					}
				})
			}
		}
	}
}

// authNumberWriteFails is a holder store whose auth-number write fails.
type authNumberWriteFails struct{ Store }

func (authNumberWriteFails) StoreAuthNumber(string, string) error {
	return errors.New("the holder store is unavailable")
}

// The gateway's own answers after a leg, other than the fill's check, keep
// their status and what they are recorded as, and say which legs were carried,
// once: the questionnaire package read, the PAS answer read, and the holder's
// write of the payer's auth number.
func TestOrigination_OwnAnswersAfterALegSayWhichLegsWereCarried(t *testing.T) {
	foreignURL := func(t *testing.T) []byte {
		var q map[string]any
		if err := json.Unmarshal(fhirseed.DemoLumbarQuestionnaire(), &q); err != nil {
			t.Fatal(err)
		}
		q["url"] = "https://example.test/Questionnaire/another"
		raw, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := testQuestionnairePackage(raw)
		if err != nil {
			t.Fatal(err)
		}
		return pkg
	}
	for _, row := range []struct {
		name, want string
		status     int
		// after is the last leg carried before the answer.
		after string
		setup func(t *testing.T, g *Gateway, stub *pendResumeSubstrate)
	}{
		{"package with no Questionnaire", "fetched questionnaire package has no Questionnaire", http.StatusBadGateway, "dtr-questionnaire-fetch", func(t *testing.T, g *Gateway, stub *pendResumeSubstrate) {
			stub.overrideResponse = func(leg string, p []byte) []byte {
				if leg != "dtr-questionnaire-fetch" {
					return p
				}
				return []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`)
			}
		}},
		{"Questionnaire for another canonical than the card's", "fetched questionnaire does not match advertised canonical", http.StatusBadGateway, "dtr-questionnaire-fetch", func(t *testing.T, g *Gateway, stub *pendResumeSubstrate) {
			pkg := foreignURL(t)
			stub.overrideResponse = func(leg string, p []byte) []byte {
				if leg != "dtr-questionnaire-fetch" {
					return p
				}
				return pkg
			}
		}},
		{"PAS answer that is not a pend", "expected pended response", http.StatusBadGateway, "pas-claim", func(t *testing.T, g *Gateway, stub *pendResumeSubstrate) {
			stub.overrideResponse = func(leg string, p []byte) []byte {
				if leg != "pas-claim" {
					return p
				}
				return homeOxygenApprovedClaimResponse()
			}
		}},
		{"auth number write", "holder write failed (auth number)", http.StatusBadGateway, "pas-claim-update", func(t *testing.T, g *Gateway, stub *pendResumeSubstrate) {
			g.cfg.Store = authNumberWriteFails{g.cfg.Store}
		}},
	} {
		for _, level := range allLevels {
			t.Run(row.name+"/"+level.String(), func(t *testing.T) {
				g, stub := uc04RouteFixture(t, "2.0")
				mountOriginationRoutes(g)
				body := testRouteBody(g, "/scenario/uc04", "")
				row.setup(t, g, stub)
				g.cfg.ConformanceEnforcement = level
				answered := recordAnsweredLegs(g)
				rec, recs := serveRecorded(t, g, "POST /scenario/uc04", body)
				if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.want) {
					t.Fatalf("POST /scenario/uc04 = %d %s, want %d %q", rec.Code, rec.Body, row.status, row.want)
				}
				legs := answered.list()
				if len(legs) == 0 || legs[len(legs)-1] != row.after {
					t.Fatalf("legs carried %v, want the answer after %s", legs, row.after)
				}
				wantCarried(t, rec.Body.String(), legs...)
				// Recorded as before: the gateway's own answer, on no
				// network rule (the exchange's fallback for an answer it
				// wrote itself).
				wantRefusal(t, onlyRecord(t, recs), row.status, RefusedByProviderGateway, ExchangeOther)
			})
		}
	}
}

// A resume is a call of its own: an answer it writes after its amendment was
// carried says so, naming that leg alone.
func TestResume_OwnAnswerAfterItsLegSaysItWasCarried(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			gw, _ := uc06Fixture(t)
			gw.cfg.ConformanceEnforcement = level
			st := pendFor(t, gw, "uc06", "MBR-UC06")
			gw.cfg.Store = authNumberWriteFails{gw.cfg.Store}
			rec := httptest.NewRecorder()
			if gw.completeClinician(rec, post(gw, "/scenario/uc06/complete", ""), st, "42") {
				t.Fatal("the resume completed with its auth-number write failing")
			}
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "holder write failed (auth number)") {
				t.Fatalf("%d %s, want 502 naming the holder write", rec.Code, rec.Body)
			}
			wantCarried(t, rec.Body.String(), "pas-claim-update")
		})
	}
}

// An answer written before any leg is left as it was: it says nothing about
// legs.
func TestOrigination_OwnAnswerBeforeAnyLegIsAsItWas(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			g := env.originator
			mountOriginationRoutes(g)
			g.cfg.ConformanceEnforcement = level
			rec, _ := serveRecorded(t, g, "POST /scenario/uc01", testRouteBody(g, "/scenario/uc01", `"branch":"no-such-branch"`))
			if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"unknown branch"}`+"\n" {
				t.Fatalf("%d %q, want 400 unknown branch as it was", rec.Code, rec.Body)
			}
		})
	}
}
