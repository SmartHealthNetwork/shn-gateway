//go:build !shn_coreonly

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// dotSegmentIDs are the two FHIR ids that are a dot segment in a path.
var dotSegmentIDs = []string{".", ".."}

// dotIDSoR is a system of record that answers every member's Patient id as id
// (a participant's own connector may answer "." or ".."), and notes every
// reference it is asked to read. It is read only as a legacy SystemOfRecord,
// which has no search, so a reference read is every read made with an id.
type dotIDSoR struct {
	SystemOfRecord
	id    string
	mu    *sync.Mutex
	named *[]string
}

func newDotIDSoR(id string) dotIDSoR {
	return dotIDSoR{SystemOfRecord: newCensusSoR(), id: id, mu: &sync.Mutex{}, named: new([]string)}
}

func (s dotIDSoR) PatientFHIRRef(string) (string, bool) { return "Patient/" + s.id, true }

func (s dotIDSoR) ResolveByReference(ref string) ([]byte, bool) {
	s.mu.Lock()
	*s.named = append(*s.named, ref)
	s.mu.Unlock()
	return s.SystemOfRecord.ResolveByReference(ref)
}

// reads are the references read that name id as a path segment.
func (s dotIDSoR) reads() []string { return s.readsNaming(s.id) }

func (s dotIDSoR) readsNaming(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ref := range *s.named {
		if strings.HasSuffix(ref, "/"+id) || strings.Contains(ref, "/"+id+"/") {
			out = append(out, ref)
		}
	}
	return out
}

// A Patient id a system of record answers that is a dot segment ("." or "..")
// is that system's invalid answer at every site that would write it into a
// path or a search, and nothing is read with it; an id that only carries a
// dot ("p.1") is read as any other.
func TestASystemOfRecordsDotSegmentPatientIDIsAnInvalidAnswer(t *testing.T) {
	ctx := context.Background()
	invalid := (&SoRReadError{Kind: SoRInvalidResponse}).Error()
	sites := map[string]func(g *Gateway) (int, string){
		"CRD origination's records": func(g *Gateway) (int, string) {
			_, status, msg, _ := g.originCRDRecords(ctx, "crd-order-select", "MBR-COVERED")
			return status, msg
		},
		"CRD origination's draft order": func(g *Gateway) (int, string) {
			_, status, msg, _ := g.draftOrderContext(ctx, "MBR-COVERED")
			return status, msg
		},
		"the facility's records": func(g *Gateway) (int, string) {
			_, _, status, msg := g.facilityRecordsBundle(ctx, "MBR-COVERED", []shnsdk.CDexQuery{{ResourceType: "DiagnosticReport", PatientRef: "Patient/MBR-COVERED", Start: "2024-01-01", End: "2026-12-31"}}, "Consent/c-1")
			return status, msg
		},
		"the native enrichment's Patient": func(g *Gateway) (int, string) {
			_, status, msg := g.obtainDTRPatient(ctx, "MBR-COVERED", patientFence{})
			return status, msg
		},
		"the DTR ingress's coverage": func(g *Gateway) (int, string) {
			_, _, status, msg := g.obtainDTRCoverage(ctx, &dtrIngressRequest{member: "MBR-COVERED"}, patientFence{})
			return status, msg
		},
		// A request carrying no coverage needs one obtained to be routed, so
		// the system of record's Patient id is needed.
		"the CRD ingress's own records": func(g *Gateway) (int, string) {
			var req map[string]any
			if err := json.Unmarshal(crdReqJSON("MBR-COVERED", "Patient/MBR-COVERED", "Patient/MBR-COVERED"), &req); err != nil {
				return 0, err.Error()
			}
			delete(req["prefetch"].(map[string]any), "coverage")
			raw, _ := json.Marshal(req)
			_, status, msg := g.ingressEnsureSelfContainedContext(ctx, "crd-order-select", raw, "MBR-COVERED")
			return status, msg
		},
	}
	for name, site := range sites {
		for _, id := range dotSegmentIDs {
			t.Run(name+"/"+id, func(t *testing.T) {
				sor := newDotIDSoR(id)
				status, msg := site(&Gateway{cfg: Config{SoR: sor}})
				if status != http.StatusBadGateway || !strings.Contains(msg, invalid) {
					t.Fatalf("answer %d %q; want 502 %q", status, msg, invalid)
				}
				if reads := sor.reads(); len(reads) != 0 {
					t.Fatalf("read %v with the id %q; want nothing read with it", reads, id)
				}
			})
		}
		t.Run(name+"/control p.1", func(t *testing.T) {
			if status, msg := site(&Gateway{cfg: Config{SoR: newDotIDSoR("p.1")}}); strings.Contains(msg, invalid) {
				t.Fatalf("an id carrying a dot: answer %d %q; want it read as any other", status, msg)
			}
		})
	}
}

// A search the gateway builds for a Patient id never names a dot segment.
func TestSoRSearchQueryRefusesADotSegmentPatientID(t *testing.T) {
	for _, id := range dotSegmentIDs {
		var se *SearchError
		if _, err := SoRSearchQuery("Condition", id); !errors.As(err, &se) || se.Outcome != SearchMalformed {
			t.Errorf("SoRSearchQuery(%q): %v; want a malformed search", id, err)
		}
	}
	if _, err := SoRSearchQuery("Condition", "p.1"); err != nil {
		t.Fatalf("control p.1: %v", err)
	}
}

// A Coverage party named by a dot-segment Patient reference is never read:
// no record can carry it. A party whose id only carries a dot is read.
func TestCoveragePartyRecordsNeverReadADotSegment(t *testing.T) {
	for _, id := range dotSegmentIDs {
		sor := newDotIDSoR("p-unused")
		g := &Gateway{cfg: Config{SoR: sor}}
		cov := []byte(`{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/MBR-COVERED"},"subscriber":{"reference":"Patient/` + id + `"},"policyHolder":{"reference":"Patient/p.1"}}`)
		_, _ = g.coveragePartyRecords(context.Background(), cov)
		if reads := sor.readsNaming(id); len(reads) != 0 {
			t.Fatalf("subscriber %q: read %v; want no read of a dot segment", id, reads)
		}
		if reads := sor.readsNaming("p.1"); len(reads) != 1 {
			t.Fatalf("control: the policyHolder Patient/p.1 was read %v; want once", reads)
		}
	}
}

// A CDS Hooks ingress request that carries its coverage is routed by it, but a
// reference in it to a dot-segment id its system of record answered is no
// longer the member's own patient: the patient fence holds only the member.
// At strict the patient-mixed check refuses it; at none it is carried. A
// beneficiary that names the member is carried at strict.
func TestCRDIngressNeverReadsADotSegmentIDAsTheMembersPatient(t *testing.T) {
	ctx := context.Background()
	request := func(t *testing.T, beneficiary string) []byte {
		var req map[string]any
		if err := json.Unmarshal(crdReqJSON("MBR-COVERED", "Patient/MBR-COVERED", "Patient/MBR-COVERED"), &req); err != nil {
			t.Fatal(err)
		}
		req["prefetch"].(map[string]any)["coverage"] = map[string]any{"resourceType": "Bundle", "type": "searchset", "entry": []any{
			map[string]any{"resource": map[string]any{"resourceType": "Coverage", "id": "c1", "status": "active", "beneficiary": map[string]any{"reference": beneficiary}}},
		}}
		raw, _ := json.Marshal(req)
		return raw
	}
	for _, id := range dotSegmentIDs {
		for _, level := range []ConformanceEnforcement{EnforcementNone, EnforcementStrict} {
			t.Run(id+"/"+level.String(), func(t *testing.T) {
				g := &Gateway{cfg: Config{SoR: newDotIDSoR(id), ConformanceEnforcement: level}}
				_, status, msg := g.ingressEnsureSelfContainedContext(ctx, "crd-order-select", request(t, "Patient/"+id), "MBR-COVERED")
				if level == EnforcementStrict {
					if status != http.StatusForbidden || !strings.HasPrefix(msg, "prefetch coverage refused") {
						t.Fatalf("answer %d %q; want 403 the prefetch coverage refused as another patient's", status, msg)
					}
					return
				}
				if status != 0 {
					t.Fatalf("answer %d %q; want the request carried at none", status, msg)
				}
			})
		}
		t.Run(id+"/control", func(t *testing.T) {
			g := &Gateway{cfg: Config{SoR: newDotIDSoR(id), ConformanceEnforcement: EnforcementStrict}}
			if _, status, msg := g.ingressEnsureSelfContainedContext(ctx, "crd-order-select", request(t, "Patient/MBR-COVERED"), "MBR-COVERED"); status != 0 {
				t.Fatalf("a beneficiary naming the member: answer %d %q; want it carried at strict", status, msg)
			}
		})
	}
}
