package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The ordering clinician and the order an originated request carries come
// from the participant's own records; nothing stands in for them.

// draftOrder is a draft ServiceRequest for prefetchMember with requester.
func draftOrder(id, requester string) string {
	r := ""
	if requester != "" {
		r = `,"requester":{"reference":"` + requester + `"}`
	}
	return `{"resourceType":"ServiceRequest","id":"` + id + `","status":"draft","intent":"order","code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0250"}]},"subject":{"reference":"Patient/` + prefetchMember + `"}` + r + `}`
}

func activeOrderEntry(id string) string {
	return `{"resource":{"resourceType":"ServiceRequest","id":"` + id + `","status":"active","intent":"order","code":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0250"}]},"subject":{"reference":"Patient/` + prefetchMember + `"}},"search":{"mode":"match"}}`
}

func searchsetOf(entries ...string) []byte {
	return []byte(`{"resourceType":"Bundle","type":"searchset","entry":[` + strings.Join(entries, ",") + `]}`)
}

func matchOf(res string) string { return `{"resource":` + res + `,"search":{"mode":"match"}}` }

// TestOriginatedCRD_OrderSelectCarriesTheDraftOrder: UC-02's coverage check is
// about an order still being chosen, so it carries the member's draft order,
// found by searching the system of record — never the member's active order,
// and never an order whose status the network changed.
func TestOriginatedCRD_OrderSelectCarriesTheDraftOrder(t *testing.T) {
	uc02 := func(t *testing.T, sor SystemOfRecord) (*httptest.ResponseRecorder, *originPayer) {
		t.Helper()
		g, payer := originGateway(t, "provider-data", sor, nil, func(_ string, req []byte) (int, []byte) {
			return 0, payerAnswerFor(t, []byte(draftOrder("sr-d", "Practitioner/dr-1")), shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
		})
		rec := httptest.NewRecorder()
		g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
		return rec, payer
	}
	t.Run("the draft order is carried exactly", func(t *testing.T) {
		sor, _ := originSystem(t, prefetchMember)
		draft := draftOrder("sr-d", "Practitioner/dr-1")
		sor.answer(t, "ServiceRequest", searchsetOf(activeOrderEntry("sr-a"), matchOf(draft)))
		rec, payer := uc02(t, sor)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d %s", rec.Code, rec.Body)
		}
		sent := decodeSent(t, payer.sent("crd-order-select")[0])
		if sent.Hook != "order-select" || len(sent.Context.Selections) != 1 || sent.Context.Selections[0] != "ServiceRequest/sr-d" {
			t.Fatalf("hook %q selections %v", sent.Hook, sent.Context.Selections)
		}
		if !bytes.Contains(sent.Context.DraftOrders, []byte(draft)) || bytes.Contains(sent.Context.DraftOrders, []byte("sr-a")) {
			t.Fatalf("draft orders %s, want exactly the draft order", sent.Context.DraftOrders)
		}
		if sent.Context.UserID != "Practitioner/dr-1" {
			t.Fatalf("userId %q, want the draft order's requester", sent.Context.UserID)
		}
	})
	for name, row := range map[string]struct {
		sor    func(t *testing.T) SystemOfRecord
		status int
		msg    string
	}{
		"no draft order": {func(t *testing.T) SystemOfRecord {
			sor, _ := originSystem(t, prefetchMember)
			sor.answer(t, "ServiceRequest", searchsetOf(activeOrderEntry("sr-a")))
			return sor
		}, http.StatusBadGateway, "no draft order for member in system of record"},
		"several draft orders": {func(t *testing.T) SystemOfRecord {
			sor, _ := originSystem(t, prefetchMember)
			sor.answer(t, "ServiceRequest", searchsetOf(matchOf(draftOrder("sr-d", "")), matchOf(draftOrder("sr-e", ""))))
			return sor
		}, http.StatusUnprocessableEntity, "several draft orders for member in system of record"},
		"search unavailable": {func(t *testing.T) SystemOfRecord {
			sor, _ := originSystem(t, prefetchMember)
			sor.searches["ServiceRequest"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}
			return sor
		}, http.StatusServiceUnavailable, ""},
		"connector cannot search": {func(t *testing.T) SystemOfRecord {
			sor, _ := originSystem(t, prefetchMember)
			return noSearchOrigin{sor}
		}, http.StatusUnprocessableEntity, "the system of record cannot search for the member's draft order"},
	} {
		t.Run(name, func(t *testing.T) {
			rec, payer := uc02(t, row.sor(t))
			if rec.Code != row.status || !strings.Contains(rec.Body.String(), row.msg) {
				t.Fatalf("got %d %s, want %d %q", rec.Code, rec.Body, row.status, row.msg)
			}
			if n := len(payer.sent("crd-order-select")); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}

// noSearchOrigin is an originSoR whose connector cannot search.
type noSearchOrigin struct{ originSoR }

func (noSearchOrigin) SearchPatientContext(context.Context, string, string, ...SearchDateRange) (SearchResult, error) {
	return SearchResult{}, &SearchError{Outcome: SearchUnsupported, Reason: "connector does not search"}
}

// TestOriginatedCRD_UserIDIsTheOrderingClinician: userId is the order's
// requester; without one, the configured NPI's Practitioner; without either,
// the request is refused before anything is sent.
func TestOriginatedCRD_UserIDIsTheOrderingClinician(t *testing.T) {
	_, _, order := originRecords(prefetchMember)
	withRequester := bytes.Replace(order, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "PractitionerRole/role-7" },`), 1)
	for name, row := range map[string]struct {
		order  []byte
		npi    string
		userID string
	}{
		"the order's requester":               {withRequester, "1234567890", "PractitionerRole/role-7"},
		"the requester without any NPI":       {withRequester, "", "PractitionerRole/role-7"},
		"the configured NPI":                  {order, "1234567890", "Practitioner/1234567890"},
		"neither":                             {order, "", ""},
		"a requester that is not a clinician": {bytes.Replace(order, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "Organization/o-1" },`), 1), "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			sor.order = row.order
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, row.order, shnsdk.CoveredNotCovered, "", "")
			})
			g.cfg.NPI = row.npi
			rec := httptest.NewRecorder()
			g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
			reqs := payer.sent("crd-order-select")
			if row.userID == "" {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "order names no requester; configure NPI") || len(reqs) != 0 {
					t.Fatalf("got %d %s with %d requests sent", rec.Code, rec.Body, len(reqs))
				}
				return
			}
			if len(reqs) != 1 {
				t.Fatalf("%d requests (%d %s)", len(reqs), rec.Code, rec.Body)
			}
			if got := decodeSent(t, reqs[0]).Context.UserID; got != row.userID {
				t.Fatalf("userId %q, want %q", got, row.userID)
			}
		})
	}
}

// TestAttestingNPI: the clinician who attests is the one the caller names;
// otherwise the configured NPI; otherwise the NPI on the order's requester,
// read from the system of record; otherwise the attestation is refused.
func TestAttestingNPI(t *testing.T) {
	npiPractitioner := `{"resourceType":"Practitioner","id":"dr-1","identifier":[{"system":"http://example.org/other","value":"x"},{"system":"http://hl7.org/fhir/sid/us-npi","value":"1500000009"}]}`
	role := `{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/dr-1"}}`
	noNPI := `{"resourceType":"Practitioner","id":"dr-2","identifier":[{"system":"http://example.org/other","value":"x"}]}`
	orderBy := func(ref string) []byte { return []byte(draftOrder("sr-1", ref)) }
	for name, row := range map[string]struct {
		order     []byte
		sent, cfg string
		want      string
		status    int
	}{
		"sent":                             {orderBy("Practitioner/dr-2"), "1111111111", "2222222222", "1111111111", 0},
		"configured":                       {orderBy("Practitioner/dr-2"), "", "2222222222", "2222222222", 0},
		"the requester's NPI":              {orderBy("Practitioner/dr-1"), "", "", "1500000009", 0},
		"through a PractitionerRole":       {orderBy("PractitionerRole/role-1"), "", "", "1500000009", 0},
		"a requester without an NPI":       {orderBy("Practitioner/dr-2"), "", "", "", http.StatusUnprocessableEntity},
		"an unknown requester":             {orderBy("Practitioner/dr-9"), "", "", "", http.StatusUnprocessableEntity},
		"no requester":                     {orderBy(""), "", "", "", http.StatusUnprocessableEntity},
		"a requester that is no clinician": {orderBy("Organization/o-1"), "", "", "", http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			s := newPrefetchSoR()
			s.reads["Practitioner/dr-1"] = []byte(npiPractitioner)
			s.reads["Practitioner/dr-2"] = []byte(noNPI)
			s.reads["PractitionerRole/role-1"] = []byte(role)
			g := &Gateway{cfg: Config{SoR: s, NPI: row.cfg}}
			got, status, msg := g.attestingNPI(context.Background(), row.order, row.sent)
			if got != row.want || status != row.status {
				t.Fatalf("got %q %d %q, want %q %d", got, status, msg, row.want, row.status)
			}
			if status != 0 && !strings.Contains(msg, "attestation names no clinician NPI") {
				t.Fatalf("refusal %q", msg)
			}
		})
	}
}

// TestEligibilityRequestProvider: the eligibility request names the provider
// only when an NPI is configured; nothing stands in for it.
func TestEligibilityRequestProvider(t *testing.T) {
	for _, npi := range []string{"1234567890", ""} {
		g := &Gateway{cfg: Config{NPI: npi, Clock: fixedClock}}
		b, err := g.eligibilityRequest("MBR-1")
		if err != nil {
			t.Fatal(err)
		}
		var cer map[string]json.RawMessage
		if err := json.Unmarshal(b, &cer); err != nil {
			t.Fatal(err)
		}
		provider, has := cer["provider"]
		switch {
		case npi != "" && string(provider) != `{"reference":"Practitioner/`+npi+`"}`:
			t.Fatalf("npi %q: provider %s", npi, provider)
		case npi == "" && has:
			t.Fatalf("no NPI: provider %s", provider)
		}
		if string(cer["resourceType"]) != `"CoverageEligibilityRequest"` || string(cer["patient"]) != `{"reference":"Patient/MBR-1"}` {
			t.Fatalf("request %s", b)
		}
	}
}
