package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The coverage a CRD request this gateway originates is routed by, and the one
// it carries, is the routing choice (routingCoverageChoice): the active
// Coverages when any is active, else the others when they name one payer. It is
// the gateway's own request, so the coverage template's status filter does not
// apply, and every leg of the exchange names the same coverage: the CRD
// request's coverage, the questionnaire-package request's coverage and the
// coverage the PAS leg is built under (memberRoutingCoverage).

// coveragesOriginSoR is originSystem's system holding the Coverage records
// given: the member's Coverage read (OpenCoverageContext) and the Coverage
// search answer the same records, the search including each payor.
type coveragesOriginSoR struct {
	originSoR
	coverages [][]byte
}

func (s coveragesOriginSoR) OpenCoverageContext(context.Context, string) ([][]byte, error) {
	return s.coverages, nil
}

// The payers' Organization records: pay-1 is the payer the test router knows
// (00001), pay-2 one it does not.
const otherPayerOrganization = `{"resourceType":"Organization","id":"pay-2","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"99998"}]}`

// originCoverage is a Coverage for prefetchMember with status, naming org as its payor.
func originCoverage(id, status, org string) []byte {
	return []byte(`{"resourceType":"Coverage","id":"` + id + `","status":"` + status + `","beneficiary":{"reference":"Patient/` + prefetchMember +
		`"},"subscriberId":"` + prefetchMember + `","payor":[{"reference":"Organization/` + org + `"}]}`)
}

// coveragesSystem is a system of record holding coverages, the Coverage search
// answering each followed by its payor as an included record, as a FHIR server
// answers _include.
func coveragesSystem(t *testing.T, coverages ...[]byte) coveragesOriginSoR {
	t.Helper()
	sor, _ := originSystem(t, prefetchMember)
	sor.reads["Organization/pay-2"] = []byte(otherPayerOrganization)
	orgs := map[string]string{"pay-1": payerOrganization, "pay-2": otherPayerOrganization}
	var entries []string
	seen := map[string]bool{}
	for _, c := range coverages {
		entries = append(entries, matchOf(string(c)))
		for org, rec := range orgs {
			if bytes.Contains(c, []byte(`"Organization/`+org+`"`)) && !seen[org] {
				seen[org] = true
				entries = append(entries, `{"resource":`+rec+`,"search":{"mode":"include"}}`)
			}
		}
	}
	sor.answer(t, "Coverage", searchsetOf(entries...))
	return coveragesOriginSoR{originSoR: sor, coverages: coverages}
}

// originDrive runs the order-select origination (UC-02's coverage check,
// originateNoPACRD) and the order-sign origination (runCRDThenDTROrder)
// through the questionnaire fetch, and returns
// each one's answer and the requests the payer received.
func originDrive(t *testing.T, sor SystemOfRecord) (selectRec, signRec *httptest.ResponseRecorder, selectPayer, signPayer *originPayer) {
	t.Helper()
	const canonical = "http://example.org/fhir/Questionnaire/PriorAuthRequired"
	questionnaire := []byte(`{"resourceType":"Questionnaire","id":"q","url":"` + canonical + `","status":"active","item":[{"linkId":"1","text":"x","type":"string"}]}`)
	pkg, err := testQuestionnairePackage(questionnaire)
	if err != nil {
		t.Fatal(err)
	}
	_, _, order := originRecords(prefetchMember)
	g, selectPayer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
		return 0, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
	})
	selectRec = httptest.NewRecorder()
	g.originateNoPACRD(selectRec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", nil), prefetchMember)

	g, signPayer = originGateway(t, "provider-data", sor, &capturePopulator{}, func(tx string, _ []byte) (int, []byte) {
		if tx == "dtr-questionnaire-fetch" {
			return 0, pkg
		}
		return 0, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededAuthNeeded, canonical)
	})
	signRec = httptest.NewRecorder()
	g.runCRDThenDTROrder(signRec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
	return selectRec, signRec, selectPayer, signPayer
}

// dispatchDrive runs the order-dispatch origination (runCRDDispatch) for an
// E0431 order the system of record's device search holds, and returns its
// answer and the requests the payer received.
func dispatchDrive(t *testing.T, sor coveragesOriginSoR) (*httptest.ResponseRecorder, *originPayer) {
	t.Helper()
	const performer = "Organization/org-dme-ox"
	order, err := buildHomeOxygenDeviceRequest("dr-ox", "Patient/"+prefetchMember, performer)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := buildHomeOxygenSupplier("org-dme-ox")
	if err != nil {
		t.Fatal(err)
	}
	sor.answer(t, "DeviceRequest", searchsetOf(matchOf(string(order))))
	g, payer := originGateway(t, "provider-data", sor, &capturePopulator{}, func(string, []byte) (int, []byte) {
		return 0, []byte(`{"cards":[]}`)
	})
	rec := httptest.NewRecorder()
	g.runCRDDispatch(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember,
		dispatchOrder{orderJSON: order, supplierJSON: supplier, orderRef: "DeviceRequest/dr-ox", performerRef: performer})
	return rec, payer
}

func TestOriginatedCRD_RoutesAndCarriesTheRoutingChoice(t *testing.T) {
	cancelled := originCoverage("cov-0", "cancelled", "pay-1")
	active := originCoverage("cov-1", "active", "pay-1")
	staleOther := originCoverage("cov-2", "cancelled", "pay-2")
	for _, row := range []struct {
		name      string
		coverages [][]byte
		want      []byte   // the coverage every leg names
		absent    []string // what no leg carries
	}{
		{"a member whose only Coverage is cancelled is sent to that coverage's payer, with it",
			[][]byte{cancelled}, cancelled, nil},
		{"an active and a cancelled coverage naming different payers: only the active one, and its payor",
			[][]byte{staleOther, active}, active, []string{`"cov-2"`, `"pay-2"`}},
		{"a payor a stale coverage shares with the active one is kept",
			[][]byte{cancelled, active}, active, []string{`"cov-0"`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			sor := coveragesSystem(t, row.coverages...)
			selectRec, _, selectPayer, signPayer := originDrive(t, sor)
			if selectRec.Code != http.StatusOK {
				t.Fatalf("order-select origination %d %s", selectRec.Code, selectRec.Body)
			}
			legs := map[string][]byte{}
			for name, p := range map[string]*originPayer{"order-select": selectPayer, "order-sign": signPayer} {
				reqs := p.sent("crd-order-select")
				if len(reqs) != 1 {
					t.Fatalf("%s: %d CRD requests", name, len(reqs))
				}
				legs[name+" CRD coverage"] = decodeSent(t, reqs[0]).Prefetch["coverage"]
			}
			_, dispatchPayer := dispatchDrive(t, sor)
			dispatched := dispatchPayer.sent("crd-order-dispatch")
			if len(dispatched) != 1 {
				t.Fatalf("%d order-dispatch requests", len(dispatched))
			}
			legs["order-dispatch CRD coverage"] = decodeSent(t, dispatched[0]).Prefetch["coverage"]
			fetches := signPayer.sent("dtr-questionnaire-fetch")
			if len(fetches) != 1 {
				t.Fatalf("%d questionnaire requests", len(fetches))
			}
			legs["questionnaire-package"] = fetches[0]
			for leg, b := range legs {
				if !bytes.Contains(b, row.want) {
					t.Errorf("%s does not carry the chosen Coverage %s:\n%s", leg, row.want, b)
				}
				if !bytes.Contains(b, []byte(payerOrganization)) && strings.HasSuffix(leg, "CRD coverage") {
					t.Errorf("%s does not carry the chosen Coverage's payor: %s", leg, b)
				}
				for _, a := range row.absent {
					if bytes.Contains(b, []byte(a)) {
						t.Errorf("%s carries %s: %s", leg, a, b)
					}
				}
			}
			// The carried coverage is searched without the coverage template's
			// status filter: every Coverage the system holds.
			searched, _ := sor.calls()
			coverageSearches := 0
			for _, q := range searched {
				if strings.HasPrefix(q, "Coverage?") {
					coverageSearches++
					if q != routingCoverageQuery {
						t.Errorf("an originated request's coverage was searched as %s, want %s", q, routingCoverageQuery)
					}
				}
			}
			if coverageSearches == 0 {
				t.Fatal("no Coverage search ran")
			}
			// The PAS leg is built under the coverage the request was routed by.
			g := &Gateway{cfg: Config{SoR: sor}}
			cov, found, status, msg := g.memberRoutingCoverage(context.Background(), prefetchMember)
			if !found || status != 0 || !bytes.Equal(cov, row.want) {
				t.Fatalf("routing coverage %s (found %v, %d %s), want %s", cov, found, status, msg, row.want)
			}
		})
	}

	t.Run("only cancelled coverages naming two payers: ambiguous", func(t *testing.T) {
		sor := coveragesSystem(t, cancelled, staleOther)
		selectRec, signRec, selectPayer, signPayer := originDrive(t, sor)
		dispatchRec, dispatchPayer := dispatchDrive(t, sor)
		for name, rec := range map[string]*httptest.ResponseRecorder{"order-select": selectRec, "order-sign": signRec, "order-dispatch": dispatchRec} {
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ambiguous coverage for routing") {
				t.Errorf("%s: %d %s, want 422 ambiguous coverage for routing", name, rec.Code, rec.Body)
			}
		}
		if n := len(selectPayer.sent("crd-order-select")) + len(signPayer.sent("crd-order-select")) + len(dispatchPayer.sent("crd-order-dispatch")); n != 0 {
			t.Fatalf("%d requests reached the payer", n)
		}
	})
}

// A payor Organization only a stale Coverage names is left out of the
// originated request however that Coverage writes the reference: relatively,
// by an absolute URL ending in it, or with a version.
func TestOriginatedCRD_DropsAStalePayorHoweverReferenced(t *testing.T) {
	active := originCoverage("cov-1", "active", "pay-1")
	for name, ref := range map[string]string{
		"absolute":           "https://ehr.example/fhir/Organization/pay-2",
		"versioned":          "Organization/pay-2/_history/1",
		"absolute versioned": "https://ehr.example/fhir/Organization/pay-2/_history/3",
	} {
		t.Run(name, func(t *testing.T) {
			stale := bytes.Replace(originCoverage("cov-2", "cancelled", "pay-2"), []byte(`"Organization/pay-2"`), []byte(`"`+ref+`"`), 1)
			if !bytes.Contains(stale, []byte(ref)) {
				t.Fatalf("the stale coverage does not name %s: %s", ref, stale)
			}
			sor := coveragesSystem(t, stale, active)
			// The server includes each payor the Coverages name.
			include := func(rec string) string { return `{"resource":` + rec + `,"search":{"mode":"include"}}` }
			sor.answer(t, "Coverage", searchsetOf(matchOf(string(stale)), matchOf(string(active)), include(otherPayerOrganization), include(payerOrganization)))
			selectRec, signRec, selectPayer, signPayer := originDrive(t, sor)
			if selectRec.Code != http.StatusOK {
				t.Fatalf("order-select origination %d %s", selectRec.Code, selectRec.Body)
			}
			for leg, p := range map[string]*originPayer{"order-select": selectPayer, "order-sign": signPayer} {
				reqs := p.sent("crd-order-select")
				if len(reqs) != 1 {
					t.Fatalf("%s: %d CRD requests (order-sign answered %d %s)", leg, len(reqs), signRec.Code, signRec.Body)
				}
				cov := decodeSent(t, reqs[0]).Prefetch["coverage"]
				if !bytes.Contains(cov, active) || !bytes.Contains(cov, []byte(payerOrganization)) {
					t.Errorf("%s does not carry the active Coverage and its payor: %s", leg, cov)
				}
				if bytes.Contains(cov, []byte(`"cov-2"`)) || bytes.Contains(cov, []byte(otherPayerOrganization)) {
					t.Errorf("%s carries the stale coverage or its payor: %s", leg, cov)
				}
			}
		})
	}
}

// The routing choice is the prior-authorization legs' alone: the Coverage
// read for eligibility and on the payer side (memberCoverage) still counts every
// Coverage, so an active and a cancelled one naming different payers is
// ambiguous there.
func TestMemberCoverage_NoRoutingChoice(t *testing.T) {
	sor := coveragesSystem(t, originCoverage("cov-2", "cancelled", "pay-2"), originCoverage("cov-1", "active", "pay-1"))
	g := &Gateway{cfg: Config{SoR: sor}}
	if _, _, status, msg := g.memberCoverage(context.Background(), prefetchMember); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "ambiguous coverage for routing") {
		t.Fatalf("memberCoverage: %d %s, want 422 ambiguous", status, msg)
	}
	if cov, found, status, msg := g.memberRoutingCoverage(context.Background(), prefetchMember); !found || status != 0 || !bytes.Contains(cov, []byte(`"cov-1"`)) {
		t.Fatalf("memberRoutingCoverage: %s %v %d %s, want the active Coverage", cov, found, status, msg)
	}
}
