//go:build !shn_coreonly

package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// module1_pas_qr_carried_test.go: the questionnaire response a PAS request
// carries is the one the DTR exchange produced, rebuilt at the PAS line
// (buildPASAttachment): every answer the fill returned carried, none dropped,
// changed or added, and its context naming the same order and Coverage,
// re-keyed to the PAS Bundle's own entries.

// When the DTR exchange runs at the payer's line, 2.2, and the PAS request is
// built at this gateway's line, 2.0, module 1 rebuilds the response at the PAS
// line for the request it builds. At both sites that originate the exchange
// (order-select and order-dispatch), for a response the fill returned and the
// flow carries with no amendment, each response a PAS request carries (the
// submission's, and the update's where the claim pends) holds exactly the
// answer items the fill returned, as the response checked at 2.2 does, and its
// context references resolve, in the PAS Bundle, to the order and Coverage the
// 2.2 response named: the order the system of record holds, apart from its id,
// and the member's Coverage.
//
// The row does not reach a response the gateway's automatic fill builds from
// answers (it stamps each answer's information origin in its line's own code)
// or an amended one (an amendment adds an answer).
func TestPASRequestCarriesTheDTRResponsesEveryAnswer(t *testing.T) {
	sites := []struct {
		name, member string
		run          func(t *testing.T) (*Gateway, func(*Gateway, *httptest.ResponseRecorder))
	}{
		{"order-select", "MBR-UC04", func(t *testing.T) (*Gateway, func(*Gateway, *httptest.ResponseRecorder)) {
			gw, _ := newPendResumeFixture(t, pendFixtureOpts{
				member: "MBR-UC04", birthDate: "1982-11-03", familyName: "Chen",
				pendedItem: "operative-diagnostic-report",
			})
			return gw, func(gw *Gateway, rec *httptest.ResponseRecorder) {
				gw.handleUC04(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc04", strings.NewReader(testRouteBody(gw, "/scenario/uc04", ""))))
			}
		}},
		{"order-dispatch", "MBR-OX", func(t *testing.T) (*Gateway, func(*Gateway, *httptest.ResponseRecorder)) {
			_, _, gw := homeOxygenRig(t, fakePopulator{canonical: "http://smarthealth.network/fhir/Questionnaire/home-oxygen"}, shnsdk.NewFakeValidator())
			return gw, func(gw *Gateway, rec *httptest.ResponseRecorder) {
				gw.handleHomeOxygen(rec, httptest.NewRequest(http.MethodPost, "/scenario/homeoxygen", strings.NewReader(testRouteBody(gw, "/scenario/homeoxygen", ""))))
			}
		}},
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			gw, drive := site.run(t)
			lanes := routeDTRByChainTo22(t, gw, EnforcementStructural)
			pas := &dtrLineLane{pass: func(dtrCall) bool { return true }}
			gw.cfg.ValidatorsByContractLine["pa.pas@2.0"] = pas
			rec := httptest.NewRecorder()
			drive(gw, rec)
			lanes.assertAtThePayersLine(t, rec)

			atDTR := lanes.payersLine.calls[2].payload
			if len(lanes.fill.out) != 1 {
				t.Fatalf("the fill ran %d times, want once", len(lanes.fill.out))
			}
			dtrItems := pasQRCanonical(t, pasQRField(t, lanes.fill.out[0], "item"))
			if got := pasQRCanonical(t, pasQRField(t, atDTR, "item")); got != dtrItems {
				t.Fatalf("the response checked at the DTR line carries the answers\n%s\nwant the fill's\n%s", got, dtrItems)
			}
			dtrTargets := pasQRContextTargets(t, atDTR)
			order, ok := gw.cfg.SoR.OpenOrder(site.member)
			if !ok {
				t.Fatalf("the system of record holds no order for %s", site.member)
			}
			coverage, ok := gw.cfg.SoR.OpenCoverage(site.member)
			if !ok {
				t.Fatalf("the system of record holds no Coverage for %s", site.member)
			}
			orderType := pasQRResourceType(t, order)
			// The 2.2 response names the system of record's order by its id, and
			// the member's Coverage by the member.
			if got, want := dtrTargets[orderType], orderType+"/"+pasQRString(t, order, "id"); got != want {
				t.Fatalf("the response at the DTR line names the order %q, want %q", got, want)
			}
			if got, want := dtrTargets["Coverage"], "Coverage/"+site.member; got != want {
				t.Fatalf("the response at the DTR line names the Coverage %q, want %q", got, want)
			}

			// The PAS requests checked, in order: a Bundle whose first entry is
			// its Claim (the payer's answers are checked on the same lane).
			var bundles [][]byte
			for _, c := range pas.calls {
				var b struct {
					Entry []struct {
						Resource struct {
							ResourceType string `json:"resourceType"`
						} `json:"resource"`
					} `json:"entry"`
				}
				if c.resourceType == "Bundle" && json.Unmarshal(c.payload, &b) == nil && len(b.Entry) > 0 && b.Entry[0].Resource.ResourceType == "Claim" {
					bundles = append(bundles, c.payload)
				}
			}
			if len(bundles) == 0 || len(bundles) != len(lanes.buildLine.calls) {
				t.Fatalf("PAS requests checked %d, responses they carry checked %d: want one response per request", len(bundles), len(lanes.buildLine.calls))
			}
			for i, carried := range lanes.buildLine.calls {
				// The response checked is the one this request's Bundle carries.
				pasQRBundleEntry(t, bundles[i], "QuestionnaireResponse/"+pasQRString(t, carried.payload, "id"))
				if got := pasQRCanonical(t, pasQRField(t, carried.payload, "item")); got != dtrItems {
					t.Fatalf("PAS request %d carries the answers\n%s\nwant the DTR exchange's\n%s", i, got, dtrItems)
				}
				targets := pasQRContextTargets(t, carried.payload)
				if len(targets) != len(dtrTargets) {
					t.Fatalf("PAS request %d's response names %v, want the same kinds of record as the DTR exchange's %v", i, targets, dtrTargets)
				}
				gotOrder := pasQRBundleEntry(t, bundles[i], targets[orderType])
				if pasQRCanonical(t, pasQRWithoutID(t, gotOrder)) != pasQRCanonical(t, pasQRWithoutID(t, order)) {
					t.Fatalf("PAS request %d's response names the order\n%s\nwant the system of record's, apart from its id\n%s", i, gotOrder, order)
				}
				gotCoverage := pasQRBundleEntry(t, bundles[i], targets["Coverage"])
				for _, field := range []string{"identifier", "beneficiary"} {
					if g, w := pasQRCanonical(t, pasQRField(t, gotCoverage, field)), pasQRCanonical(t, pasQRField(t, coverage, field)); g != w {
						t.Fatalf("PAS request %d's response names a Coverage whose %s is %s, want the member's %s", i, field, g, w)
					}
				}
			}
		})
	}
}

// pasQRContextTargets reads a response's own context references (the DTR qr-context
// and qr-coverage extensions) by the resource type each names.
func pasQRContextTargets(t *testing.T, qr []byte) map[string]string {
	t.Helper()
	var r struct {
		Extension []struct {
			URL            string `json:"url"`
			ValueReference *struct {
				Reference string `json:"reference"`
			} `json:"valueReference"`
		} `json:"extension"`
	}
	if err := json.Unmarshal(qr, &r); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range r.Extension {
		if e.ValueReference == nil || !strings.HasPrefix(e.URL, "http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/qr-") {
			continue
		}
		typ, _, _ := strings.Cut(e.ValueReference.Reference, "/")
		if prev, dup := out[typ]; dup {
			t.Fatalf("the response names two %s records, %s and %s", typ, prev, e.ValueReference.Reference)
		}
		out[typ] = e.ValueReference.Reference
	}
	return out
}

// pasQRBundleEntry is the resource of the Bundle entry a relative reference names,
// by its fullUrl.
func pasQRBundleEntry(t *testing.T, bundle []byte, ref string) []byte {
	t.Helper()
	var b struct {
		Entry []struct {
			FullURL  string          `json:"fullUrl"`
			Resource json.RawMessage `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		t.Fatal(err)
	}
	var found []byte
	for _, e := range b.Entry {
		if ref != "" && strings.HasSuffix(e.FullURL, "/"+ref) {
			if found != nil {
				t.Fatalf("two entries resolve %s", ref)
			}
			found = e.Resource
		}
	}
	if found == nil {
		t.Fatalf("no entry of the PAS Bundle resolves %q", ref)
	}
	return found
}

func pasQRField(t *testing.T, resource []byte, field string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resource, &m); err != nil {
		t.Fatal(err)
	}
	return m[field]
}

func pasQRString(t *testing.T, resource []byte, field string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(pasQRField(t, resource, field), &s); err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	return s
}

func pasQRResourceType(t *testing.T, resource []byte) string {
	t.Helper()
	return pasQRString(t, resource, "resourceType")
}

func pasQRWithoutID(t *testing.T, resource []byte) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resource, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "id")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pasQRCanonical is v decoded and re-encoded, so key order and spacing do not
// count while a number keeps its exact text; null for an absent value.
func pasQRCanonical(t *testing.T, v []byte) string {
	t.Helper()
	if len(v) == 0 {
		return "null"
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
