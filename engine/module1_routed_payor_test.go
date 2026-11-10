//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// clinicFirst is a system of record whose member Coverages, read to route and
// found by the Coverage search, name a clinic Organization as payor[0], before
// the payers they named: an Organization the system holds that lists an NPI
// alone, which no participant registers. Routing passes over it (every payor
// counts, and the directory decides), so the payer is still the member's.
type clinicFirst struct {
	SystemOfRecord
	// unheld makes the clinic's payor carry its NPI inline and reference an
	// Organization the system does not hold: it names its payer by its own
	// identifier, so the unreadable record is never what routing chose.
	unheld bool
}

const (
	clinicFirstRef = "Organization/clinic-npi"
	clinicFirstNPI = "1234567893"
)

func (s clinicFirst) ResolveByReference(ref string) ([]byte, bool) {
	if ref == clinicFirstRef {
		return []byte(`{"resourceType":"Organization","id":"clinic-npi","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"` + clinicFirstNPI + `"}]}`), true
	}
	return s.SystemOfRecord.ResolveByReference(ref)
}

func (s clinicFirst) OpenCoverage(member string) ([]byte, bool) {
	raw, found := s.SystemOfRecord.OpenCoverage(member)
	if !found {
		return raw, found
	}
	return s.clinicFirst(raw), found
}

func (s clinicFirst) SearchPatientContext(ctx context.Context, resourceType, id string, dates ...SearchDateRange) (SearchResult, error) {
	res, err := s.SystemOfRecord.(SearchSystemOfRecord).SearchPatientContext(ctx, resourceType, id, dates...)
	if resourceType != "Coverage" {
		return res, err
	}
	return coverageSearchRewritten(res, err, s.clinicFirst)
}

// clinicFirst is coverage with the clinic's payor before its own.
func (s clinicFirst) clinicFirst(raw []byte) []byte {
	var coverage map[string]any
	if json.Unmarshal(raw, &coverage) != nil {
		return raw
	}
	payors, _ := coverage["payor"].([]any)
	clinic := map[string]any{"reference": clinicFirstRef}
	if s.unheld {
		clinic = map[string]any{"reference": "Organization/clinic-unheld",
			"identifier": map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": clinicFirstNPI}}
	}
	coverage["payor"] = append([]any{clinic}, payors...)
	out, err := json.Marshal(coverage)
	if err != nil {
		return raw
	}
	return out
}

// namesTheClinic reports whether org is the clinic's Organization: its id, or
// the NPI it lists.
func namesTheClinic(org []byte) bool {
	return bytes.Contains(org, []byte(`"clinic-npi"`)) || bytes.Contains(org, []byte(clinicFirstNPI))
}

// claimInsurer is the Organization entry the Claim of a PAS request Bundle
// names as its insurer, resolved among the Bundle's entries by fullUrl, or by
// type and id.
func claimInsurer(t *testing.T, bundle []byte) []byte {
	t.Helper()
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatalf("read the PAS request: %v", err)
	}
	var ref string
	for _, e := range b.Entry {
		var claim struct {
			ResourceType string `json:"resourceType"`
			Insurer      struct {
				Reference string `json:"reference"`
			} `json:"insurer"`
		}
		if json.Unmarshal(e.Resource, &claim) == nil && claim.ResourceType == "Claim" {
			ref = claim.Insurer.Reference
		}
	}
	if ref == "" {
		t.Fatalf("the PAS request's Claim names no insurer:\n%s", bundle)
	}
	for _, e := range b.Entry {
		typ, id := entryHead(e.Resource)
		if entryAnswers(ref, e.FullURL, typ, id) {
			if typ != "Organization" {
				t.Fatalf("the Claim's insurer %q is a %s", ref, typ)
			}
			return e.Resource
		}
	}
	t.Fatalf("no entry answers the Claim's insurer %q:\n%s", ref, bundle)
	return nil
}

// REJECTION of a divergence, at every module 1 site that builds a prior
// authorization request or a questionnaire request from the member's Coverage,
// at every level: a Coverage whose payor[0] is an Organization no participant
// registers (an NPI alone), before the payer's, routes to the payer, and the
// request names that payer's Organization, the one the routed payor references,
// never payor[0]'s. Routed payer and payload payer are one fact: a request
// naming the clinic would go to one payer while saying it is for another.
func TestOrigination_NamesThePayorRoutingChose(t *testing.T) {
	legs := []string{"pas-claim", "pas-claim-update", "pas-claim-inquire", "dtr-questionnaire-fetch"}
	seen := map[string]int{}
	for _, row := range legSiteRows {
		// nextQuestionLeg's $next-question carries a QuestionnaireResponse,
		// not the member's Coverage.
		if !slices.Contains(legs, row.leg) || strings.HasSuffix(row.site, ":nextQuestionLeg") {
			continue
		}
		seen[row.leg]++
		for _, level := range allLevels {
			for _, unheld := range []bool{false, true} {
				shape := map[bool]string{false: "the clinic's Organization held", true: "the clinic named by its NPI, its Organization not held"}[unheld]
				t.Run(row.site+"/"+row.leg+"/"+shape+"/"+level.String(), func(t *testing.T) {
					var mu sync.Mutex
					var sent [][]byte
					rec := httptest.NewRecorder()
					row.run(t, rec, func(gw *Gateway) *legFault {
						gw.cfg.SoR, gw.cfg.ConformanceEnforcement = clinicFirst{gw.cfg.SoR, unheld}, level
						observe := gw.cfg.Observer
						gw.cfg.Observer = func(e ObserverEvent) {
							if e.Kind == "leg.originated" && e.LegType == row.leg {
								mu.Lock()
								sent = append(sent, bytes.Clone(e.Payload))
								mu.Unlock()
							}
							if observe != nil {
								observe(e)
							}
						}
						// leg.originated is emitted before the exchange: the site's
						// request is read, and the leg then refused, as each row
						// expects.
						return failLeg(gw, row.leg)
					})
					mu.Lock()
					defer mu.Unlock()
					if len(sent) == 0 {
						t.Fatalf("no %s request sent (the flow answered %d %s)", row.leg, rec.Code, rec.Body)
					}
					for _, body := range sent {
						if row.leg == "dtr-questionnaire-fetch" {
							// The package request references the Organization of
							// the payor routing chose when that payor references
							// one outside the Coverage (a contained payer is
							// carried in it); the clinic's is never among them (the
							// search included no record for it).
							var params struct {
								Parameter []struct {
									Name     string          `json:"name"`
									Resource json.RawMessage `json:"resource"`
								} `json:"parameter"`
							}
							if err := json.Unmarshal(body, &params); err != nil {
								t.Fatalf("read the questionnaire request: %v", err)
							}
							var referenced [][]byte
							for _, p := range params.Parameter {
								if p.Name == "referenced" {
									referenced = append(referenced, p.Resource)
								}
							}
							if len(referenced) > 1 || slices.ContainsFunc(referenced, namesTheClinic) {
								t.Fatalf("the questionnaire request references %d records (%s), want at most the payer's Organization", len(referenced), bytes.Join(referenced, []byte(", ")))
							}
							continue
						}
						if insurer := claimInsurer(t, body); namesTheClinic(insurer) {
							t.Fatalf("the %s request names the clinic as its insurer: %s", row.leg, insurer)
						}
					}
				})
			}
		}
	}
	for _, leg := range legs {
		if seen[leg] == 0 {
			t.Fatalf("no module 1 site sends %s here", leg)
		}
	}
}
