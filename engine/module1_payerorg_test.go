//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	vocab "github.com/SmartHealthNetwork/shn-gateway/recording"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// payorByIdentifier is a system of record whose member Coverage read, the one
// origination routes on and reads the payer record through, names its payer by
// identifier alone: the same payer, routable, with no Organization to read.
type payorByIdentifier struct{ SystemOfRecord }

func (s payorByIdentifier) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.SystemOfRecord.OpenCoverage(member)
	if !found {
		return raw, found
	}
	payer, err := shnsdk.ParseCoveragePayer(raw, s.ResolveByReference)
	if err != nil {
		return raw, found
	}
	var coverage map[string]any
	if err := json.Unmarshal(raw, &coverage); err != nil {
		return raw, found
	}
	coverage["payor"] = []any{map[string]any{"identifier": map[string]any{"system": payer.System, "value": payer.Value}}}
	out, err := json.Marshal(coverage)
	if err != nil {
		return raw, found
	}
	return out, found
}

func (s payorByIdentifier) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	return s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
}

// REJECTION, at every level: a PAS submit whose payer no record names is refused
// at its build at every module 1 site that sends one, 422, observed as the
// pas-claim leg failing (TestOrigination_PayorByIdentifierAloneRecord holds the
// record). The leg is not sent, and no payer record is minted.
func TestOrigination_PayorByIdentifierAloneRefusedAtThePASBuild(t *testing.T) {
	var rows int
	for _, row := range legSiteRows {
		if row.leg != "pas-claim" {
			continue
		}
		rows++
		for _, level := range allLevels {
			t.Run(row.site+"/"+level.String(), func(t *testing.T) {
				rec := httptest.NewRecorder()
				f := row.run(t, rec, func(gw *Gateway) *legFault {
					gw.cfg.SoR, gw.cfg.ConformanceEnforcement = payorByIdentifier{gw.cfg.SoR}, level
					return failLeg(gw, row.leg)
				})
				f.mu.Lock()
				defer f.mu.Unlock()
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msgNoPayerRecord) {
					t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, msgNoPayerRecord)
				}
				if f.hits != 0 {
					t.Fatalf("the pas-claim leg was sent %d times", f.hits)
				}
				if len(f.failed) != 1 || f.failed[0].Detail != msgNoPayerRecord {
					t.Fatalf("leg.failed for pas-claim = %+v, want one naming the refusal", f.failed)
				}
			})
		}
	}
	if rows < 7 {
		t.Fatalf("%d pas-claim sites, want at least 7", rows)
	}
}

// The refusal is recorded on the exchange as a fidelity refusal at the build
// stage, on a route that sends the leg itself and on the shared tail.
func TestOrigination_PayorByIdentifierAloneRecord(t *testing.T) {
	for name, row := range map[string]struct {
		gw   func(*testing.T) *Gateway
		path string
	}{
		"the route's own send": {func(t *testing.T) *Gateway {
			gw, _ := newPendResumeFixture(t, pendFixtureOpts{member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen", pendedItem: "operative-diagnostic-report"})
			return gw
		}, "/scenario/uc04"},
		"the shared tail": {func(t *testing.T) *Gateway { return newMBROXDispatchFixture(t).gw }, "/scenario/homeoxygen"},
	} {
		t.Run(name, func(t *testing.T) {
			gw := row.gw(t)
			mountOriginationRoutes(gw)
			gw.cfg.SoR = payorByIdentifier{gw.cfg.SoR}
			route := "POST " + row.path
			rec, recs := serveRecorded(t, gw, route, testRouteBody(gw, row.path, ""))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msgNoPayerRecord) {
				t.Fatalf("%s = %d %s, want 422 %q", route, rec.Code, rec.Body, msgNoPayerRecord)
			}
			r := onlyRecord(t, recs)
			wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
			if r.RefusalStage != vocab.RefusalStageBuild {
				t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
			}
		})
	}
}

// RELAY: the no-PA coverage check does not need the payer record, so the payer's
// covered no-auth answer reaches the caller.
func TestOrigination_PayorByIdentifierAloneNoPACheckIsCarried(t *testing.T) {
	gw, stub, _ := crdTestSystem(t, shnsdk.CardCoverage{Covered: shnsdk.CoveredCovered, PANeeded: shnsdk.PANeededNoAuth})
	declareOriginationProfile(gw, "demo")
	aimStubAt(t, stub, "MBR-D-UC02")
	gw.cfg.SoR = payorByIdentifier{gw.cfg.SoR}
	rec := httptest.NewRecorder()
	gw.handleUC02(rec, post(gw, "/scenario/uc02", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"paRequired":false`) {
		t.Fatalf("answer %d %s, want 200 with no authorization required", rec.Code, rec.Body)
	}
	if !legAttempted(stub.legTypes, "crd-order-select") {
		t.Fatalf("legs %v, want the coverage check", stub.legTypes)
	}
}

// RELAY, at every level: an origination the payer ends not-covered at CRD needs
// no PAS leg, so a Coverage naming its payer by identifier alone is carried: the
// payer's answer reaches the caller.
func TestOrigination_PayorByIdentifierAloneNotCoveredIsCarried(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			_, coverage, order := originRecords(prefetchMember)
			identifierOnly := bytes.Replace(coverage, []byte(`"reference" : "Organization/pay-1"`), []byte(`"identifier" : { "system" : "urn:oid:2.16.840.1.113883.6.300", "value" : "00001" }`), 1)
			if bytes.Equal(identifierOnly, coverage) {
				t.Fatal("the Coverage still names its payer by reference")
			}
			sor.answer(t, "Coverage", []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://sor.example/fhir/Coverage/cov-1","resource":`+string(identifierOnly)+`,"search":{"mode":"match"}}]}`))
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredNotCovered, "", "")
			})
			g.cfg.ConformanceEnforcement = level
			rec := httptest.NewRecorder()
			g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(g, prefetchMember, "", false))), prefetchMember, "", "", "", "", false)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"not-covered"`) {
				t.Fatalf("answer %d %s, want 200 not-covered", rec.Code, rec.Body)
			}
			payer.mu.Lock()
			defer payer.mu.Unlock()
			if len(payer.requests) != 1 {
				t.Fatalf("%d legs sent, want the CRD leg alone", len(payer.requests))
			}
		})
	}
}

// REJECTION, at every level and line: a request with both defects, a stated
// reference the amendment cannot carry and a payer no record names, is refused
// for the stated reference, the earlier of the two checks: it runs before the
// PAS build, where the payer record is read. The answer is 422 naming the
// reference, a fidelity refusal at the build stage, with no claim and no
// amendment sent.
func TestOrigination_StatedReferenceRefusedBeforeThePayerRecord(t *testing.T) {
	t.Parallel()
	for _, level := range allLevels {
		for _, line := range []string{"2.1", "2.2"} {
			t.Run(level.String()+"/"+line, func(t *testing.T) {
				g, stub := newPendResumeFixture(t, statedReferenceOpts("MBR-D-UC04", "1958-12-19", "Okereke", "operative-diagnostic-report", line, nil))
				statedReferenceGateway(g, line, level)
				mountOriginationRoutes(g)
				g.cfg.SoR = payorByIdentifier{g.cfg.SoR}
				rec, recs := serveRecorded(t, g, "POST /scenario/uc04", editBody(t, testRouteBody(g, "/scenario/uc04", ""), statedReference))
				if rec.Code != http.StatusUnprocessableEntity || !bytes.Contains(rec.Body.Bytes(), []byte("Observation/stated")) || strings.Contains(rec.Body.String(), msgNoPayerRecord) {
					t.Fatalf("%d %s, want 422 naming the stated reference, not the payer record", rec.Code, rec.Body)
				}
				if n := len(stub.requests["pas-claim"]) + len(stub.requests["pas-claim-update"]); n != 0 {
					t.Fatalf("%d claims sent, want none", n)
				}
				r := onlyRecord(t, recs)
				wantRefusal(t, r, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalFidelity)
				if r.RefusalStage != vocab.RefusalStageBuild {
					t.Fatalf("stage %q, want %s", r.RefusalStage, vocab.RefusalStageBuild)
				}
			})
		}
	}
}

// payorShape is a system of record whose member Coverage, read to route and
// found by the Coverage search, names the fixture's payer Organization in one
// shape, each of them a participant's own records:
//   - "carried": by reference in the search, which includes the Organization
//     (as the FHIR connector's search does, _include=Coverage:payor), while
//     the Coverage read to route names the payer by identifier alone, so only
//     the search answers the reference;
//   - "carried-known": by reference, the search including no Organization,
//     so the record read for the Coverage the gateway routes by answers it;
//   - "several-answer": by reference, the search including the Organization
//     and a different one with its type and id;
//   - "none-answers": by reference in the search, which includes no
//     Organization, while the Coverage read to route names the payer by
//     identifier alone, so no record answers the reference;
//   - "contained": by a reference to the Organization the Coverage contains;
//   - "identifier": by identifier alone.
type payorShape struct {
	SystemOfRecord
	shape string
	ref   string // the Organization's reference, Organization/<id>
	org   []byte // the Organization, as the system holds it
}

// shapePayor puts the fixture system's own payer Organization behind shape.
func shapePayor(t *testing.T, sor SystemOfRecord, member, shape string) payorShape {
	t.Helper()
	cov, found := sor.OpenCoverage(member)
	if !found {
		t.Fatalf("the fixture holds no Coverage for %s", member)
	}
	ref := coveragePayorRef(cov)
	var org []byte
	if strings.HasPrefix(ref, "#") {
		org, found = containedResource(cov, strings.TrimPrefix(ref, "#"))
	} else {
		org, found = sor.ResolveByReference(ref)
	}
	if ref == "" || !found {
		t.Fatalf("the fixture's Coverage names no payer Organization to shape: %s", cov)
	}
	_, id, err := resourceTypeAndID(org)
	if err != nil {
		t.Fatal(err)
	}
	return payorShape{SystemOfRecord: sor, shape: shape, ref: "Organization/" + id, org: org}
}

func (s payorShape) ResolveByReference(ref string) ([]byte, bool) {
	if ref == s.ref {
		return s.org, true
	}
	return s.SystemOfRecord.ResolveByReference(ref)
}

// shaped is coverage with its payor (and contained records) in shape; routing
// is whether it is the Coverage read to route by.
func (s payorShape) shaped(coverage []byte, routing bool) []byte {
	var c map[string]any
	if json.Unmarshal(coverage, &c) != nil {
		return coverage
	}
	delete(c, "contained")
	var org map[string]any
	_ = json.Unmarshal(s.org, &org)
	shape := s.shape
	if (shape == "none-answers" || shape == "carried") && routing {
		shape = "identifier"
	}
	switch shape {
	case "contained":
		contained := map[string]any{}
		for k, v := range org {
			contained[k] = v
		}
		contained["id"] = "payer"
		c["contained"] = []any{contained}
		c["payor"] = []any{map[string]any{"reference": "#payer"}}
	case "identifier":
		ids, _ := org["identifier"].([]any)
		if len(ids) == 0 {
			return coverage
		}
		c["payor"] = []any{map[string]any{"identifier": ids[0]}}
	default:
		c["payor"] = []any{map[string]any{"reference": s.ref}}
	}
	out, err := json.Marshal(c)
	if err != nil {
		return coverage
	}
	return out
}

func (s payorShape) OpenCoverage(member string) ([]byte, bool) {
	cov, found := s.SystemOfRecord.OpenCoverage(member)
	if !found {
		return cov, found
	}
	return s.shaped(cov, true), true
}

func (s payorShape) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
	if err != nil || resourceType != "Coverage" || len(res.Pages) != 1 {
		return res, err
	}
	var page struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
			Search   struct {
				Mode string `json:"mode"`
			} `json:"search"`
		} `json:"entry"`
	}
	if json.Unmarshal(res.Pages[0], &page) != nil {
		return res, nil
	}
	var entries []string
	for _, e := range page.Entry {
		if e.Search.Mode != "match" {
			continue
		}
		head := `{`
		if e.FullURL != "" {
			head += `"fullUrl":"` + e.FullURL + `",`
		}
		entries = append(entries, head+`"resource":`+string(s.shaped(e.Resource, false))+`,"search":{"mode":"match"}}`)
	}
	include := func(org []byte) {
		entries = append(entries, `{"resource":`+string(org)+`,"search":{"mode":"include"}}`)
	}
	switch s.shape {
	case "carried":
		include(s.org)
	case "several-answer":
		include(s.org)
		var other map[string]any
		_ = json.Unmarshal(s.org, &other)
		other["identifier"] = []any{map[string]any{"system": "urn:oid:2.16.840.1.113883.6.300", "value": "99999"}}
		b, _ := json.Marshal(other)
		include(b)
	}
	out := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
	parsed, err := ParseSearchPage(out, resourceType)
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Pages: [][]byte{out}, Entries: parsed.Entries, Total: len(parsed.Entries)}, nil
}

// The questionnaire request both module 1 package sites send carries the
// payer Organization the participant's search result included for the
// carried Coverage's payor reference, byte for byte as the system holds it;
// a reference several different Organizations answer, or none does, is left
// out and recorded as a dtr.referenced event, at every conformance level,
// with closed values and the reference's resource type only; a carried, a
// contained or an identifier-only payor records none. Nothing is minted
// (TestCoveragePayorRecords holds every case of the rule).
func TestOrigination_DTRCarriesThePayorTheCoverageNames(t *testing.T) {
	type site struct {
		member string
		row    legSiteRow
	}
	var sites []site
	for _, row := range legSiteRows {
		// The package request's two sites; nextQuestionLeg's $next-question
		// carries a QuestionnaireResponse, not the member's Coverage.
		if row.leg != "dtr-questionnaire-fetch" || strings.HasSuffix(row.site, ":nextQuestionLeg") {
			continue
		}
		member := map[string]string{"module1_originate.go:runCRDThenDTROrder": "MBR-UC04", "module1_originate_homeoxygen.go:runCRDDispatch": "MBR-OX"}[row.site]
		if member == "" {
			t.Fatalf("questionnaire package site %s has no member here", row.site)
		}
		sites = append(sites, site{member, row})
	}
	if len(sites) != 2 {
		t.Fatalf("%d questionnaire package sites, want 2", len(sites))
	}
	for _, st := range sites {
		for _, shape := range []string{"carried", "carried-known", "several-answer", "none-answers", "contained", "identifier"} {
			for _, level := range allLevels {
				t.Run(st.row.site+"/"+shape+"/"+level.String(), func(t *testing.T) {
					var mu sync.Mutex
					var sent [][]byte
					var events []ObserverEvent
					var shaped payorShape
					rec := httptest.NewRecorder()
					st.row.run(t, rec, func(gw *Gateway) *legFault {
						shaped = shapePayor(t, gw.cfg.SoR, st.member, shape)
						gw.cfg.SoR, gw.cfg.ConformanceEnforcement = shaped, level
						observe := gw.cfg.Observer
						gw.cfg.Observer = func(e ObserverEvent) {
							mu.Lock()
							switch {
							case e.Kind == "leg.originated" && e.LegType == "dtr-questionnaire-fetch":
								sent = append(sent, bytes.Clone(e.Payload))
							case e.Kind == ReferencedLeftOutEvent:
								events = append(events, e)
							}
							mu.Unlock()
							if observe != nil {
								observe(e)
							}
						}
						return nil
					})
					mu.Lock()
					defer mu.Unlock()
					if len(sent) != 1 {
						t.Fatalf("%d questionnaire requests sent, want 1 (the flow answered %d %s)", len(sent), rec.Code, rec.Body)
					}
					var req struct {
						Parameter []struct {
							Name     string          `json:"name"`
							Resource json.RawMessage `json:"resource"`
						} `json:"parameter"`
					}
					if err := json.Unmarshal(sent[0], &req); err != nil {
						t.Fatal(err)
					}
					var referenced []json.RawMessage
					for _, p := range req.Parameter {
						if p.Name == "referenced" {
							referenced = append(referenced, p.Resource)
						}
					}
					switch shape {
					case "carried", "carried-known":
						if len(referenced) != 1 || !bytes.Equal(referenced[0], shaped.org) {
							t.Fatalf("referenced = %s, want exactly the system's %s", referenced, shaped.org)
						}
					default:
						if len(referenced) != 0 {
							t.Fatalf("referenced = %s, want none", referenced)
						}
					}
					switch shape {
					case "several-answer", "none-answers":
						want := `{"outcome":"left-out","reason":"` + shape + `","type":"Organization"}`
						if len(events) != 1 || events[0].Detail != want || events[0].LegType != "dtr-questionnaire-fetch" || events[0].Direction != "originate" {
							t.Fatalf("dtr.referenced events %+v, want one %s on the questionnaire leg", events, want)
						}
					default:
						if len(events) != 0 {
							t.Fatalf("dtr.referenced events %+v, want none", events)
						}
					}
				})
			}
		}
	}
}
