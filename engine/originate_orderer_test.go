//go:build !shn_coreonly

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/scenarioauth"
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

// TestOriginatedCRD_UserIDIsTheOrderingClinician: userId is the first of
// clinicianSources that names the ordering clinician: a verified caller's npi,
// the order's requester, the configured NPI, an unverified caller's npi; with
// none, the request is refused before anything is sent.
func TestOriginatedCRD_UserIDIsTheOrderingClinician(t *testing.T) {
	t.Parallel()
	_, _, order := originRecords(prefetchMember)
	withRequester := bytes.Replace(order, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "PractitionerRole/role-7" },`), 1)
	for name, row := range map[string]struct {
		order    []byte
		sent     string
		verified bool
		npi      string
		userID   string
	}{
		"the order's requester beats the configured NPI":    {withRequester, "", false, "1234567893", "PractitionerRole/role-7"},
		"the order's requester, with no NPI anywhere":       {withRequester, "", false, "", "PractitionerRole/role-7"},
		"a verified caller's npi beats the requester":       {withRequester, "1111111112", true, "", "Practitioner/1111111112"},
		"the requester beats an unverified caller's npi":    {withRequester, "1111111112", false, "", "PractitionerRole/role-7"},
		"the configured NPI":                                {order, "", false, "1234567893", "Practitioner/1234567893"},
		"the configured NPI beats an unverified caller's":   {order, "1111111112", false, "1234567893", "Practitioner/1234567893"},
		"an unverified caller's npi when nothing else does": {order, "1111111112", false, "", "Practitioner/1111111112"},
		"neither": {order, "", false, "", ""},
		"a requester that is not a clinician, and no source": {bytes.Replace(order, []byte(`"intent" : "order",`), []byte(`"intent" : "order", "requester" : { "reference" : "Organization/o-1" },`), 1), "", false, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			sor.order = row.order
			// The participant's system holds the records its orders name.
			sor.reads["PractitionerRole/role-7"] = []byte(`{"resourceType":"PractitionerRole","id":"role-7"}`)
			sor.reads["Organization/o-1"] = []byte(`{"resourceType":"Organization","id":"o-1","name":"Ordering Group"}`)
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, row.order, shnsdk.CoveredNotCovered, "", "")
			})
			g.cfg.NPI = row.npi
			var events []ObserverEvent
			g.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testStatedBody(g, prefetchMember, "", false)))
			req = req.WithContext(clinicianCtx(row.sent, row.verified))
			g.runCRDThenDTROrder(rec, req, prefetchMember, "", "", "", "", false)
			reqs := payer.sent("crd-order-select")
			if row.userID == "" {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "build order-sign request failed: order ServiceRequest/") ||
					!strings.Contains(rec.Body.String(), errNoOrderingClinician.Error()) || len(reqs) != 0 {
					t.Fatalf("got %d %s with %d requests sent", rec.Code, rec.Body, len(reqs))
				}
				var recorded bool
				for _, e := range events {
					recorded = recorded || (e.Kind == "leg.failed" && strings.Contains(e.Detail, errNoOrderingClinician.Error()))
				}
				if !recorded {
					t.Fatal("the refusal was not recorded as the leg failing")
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

// clinicianCtx is a request context in which the request states sent as its
// clinician's npi, from a caller scenario authentication verified or not.
func clinicianCtx(sent string, verified bool) context.Context {
	ctx := context.Background()
	if sent != "" {
		ctx = context.WithValue(ctx, statedClinicianKey{}, sent)
	}
	if verified {
		ctx = scenarioauth.WithCaller(ctx, scenarioauth.Caller{Label: "provider-app"})
	}
	return ctx
}

// TestAttestingNPI: the clinician who attests is the first of
// clinicianSources that names one: a verified caller's npi, the order's
// requester's NPI in the system of record, the configured NPI, an unverified
// caller's npi; with none, the attestation is refused.
func TestAttestingNPI(t *testing.T) {
	npiPractitioner := `{"resourceType":"Practitioner","id":"dr-1","identifier":[{"system":"http://example.org/other","value":"x"},{"system":"http://hl7.org/fhir/sid/us-npi","value":"1500000009"}]}`
	role := `{"resourceType":"PractitionerRole","id":"role-1","practitioner":{"reference":"Practitioner/dr-1"}}`
	noNPI := `{"resourceType":"Practitioner","id":"dr-2","identifier":[{"system":"http://example.org/other","value":"x"}]}`
	orderBy := func(ref string) []byte { return []byte(draftOrder("sr-1", ref)) }
	for name, row := range map[string]struct {
		order    []byte
		sent     string
		verified bool
		cfg      string
		want     string
		status   int
	}{
		"a verified caller's npi beats the record":         {orderBy("Practitioner/dr-1"), "1111111111", true, "2222222222", "1111111111", 0},
		"the record beats the configured NPI":              {orderBy("Practitioner/dr-1"), "", false, "2222222222", "1500000009", 0},
		"the record beats an unverified caller's npi":      {orderBy("Practitioner/dr-1"), "1111111111", false, "", "1500000009", 0},
		"through a PractitionerRole":                       {orderBy("PractitionerRole/role-1"), "", false, "2222222222", "1500000009", 0},
		"configured beats an unverified npi":               {orderBy(""), "1111111111", false, "2222222222", "2222222222", 0},
		"configured when the requester has no NPI":         {orderBy("Practitioner/dr-2"), "", false, "2222222222", "2222222222", 0},
		"an unverified npi when nothing else names one":    {orderBy(""), "1111111111", false, "", "1111111111", 0},
		"a verified caller's npi when nothing else does":   {orderBy(""), "1111111111", true, "", "1111111111", 0},
		"a requester without an NPI, and nothing else":     {orderBy("Practitioner/dr-2"), "", false, "", "", http.StatusUnprocessableEntity},
		"an unknown requester, and nothing else":           {orderBy("Practitioner/dr-9"), "", false, "", "", http.StatusUnprocessableEntity},
		"no requester, and nothing else":                   {orderBy(""), "", false, "", "", http.StatusUnprocessableEntity},
		"a requester that is no clinician, nothing else":   {orderBy("Organization/o-1"), "", false, "", "", http.StatusUnprocessableEntity},
		"a verified caller stating no npi is not a source": {orderBy(""), "", true, "", "", http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			s := newPrefetchSoR()
			s.reads["Practitioner/dr-1"] = []byte(npiPractitioner)
			s.reads["Practitioner/dr-2"] = []byte(noNPI)
			s.reads["PractitionerRole/role-1"] = []byte(role)
			g := &Gateway{cfg: Config{SoR: s, NPI: row.cfg}}
			got, status, msg := g.attestingNPI(clinicianCtx(row.sent, row.verified), row.order, nil)
			if got != row.want || status != row.status {
				t.Fatalf("got %q %d %q, want %q %d", got, status, msg, row.want, row.status)
			}
			if status != 0 && msg != msgNoAttestingClinician {
				t.Fatalf("refusal %q", msg)
			}
		})
	}
}

// The attesting clinician is read from the records the origination already
// holds, not a second time from the system of record.
func TestAttestingNPI_ReadsTheHeldRecords(t *testing.T) {
	held := `{"resourceType":"Practitioner","id":"dr-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1500000009"}]}`
	s := newPrefetchSoR()
	g := &Gateway{cfg: Config{SoR: s}}
	got, status, _ := g.attestingNPI(context.Background(), []byte(draftOrder("sr-1", "Practitioner/dr-1")), [][]byte{[]byte(held)})
	if got != "1500000009" || status != 0 || len(s.read) != 0 {
		t.Fatalf("got %q %d with %d system-of-record reads, want the held record's NPI and none", got, status, len(s.read))
	}
}

// TestEligibilityRequestProvider: the eligibility request names the provider
// only when an NPI is configured, and the payer the member's coverage names as
// its insurer; nothing stands in for either.
func TestEligibilityRequestProvider(t *testing.T) {
	payer := shnsdk.PayerIdentifier{System: "urn:oid:2.16.840.1.113883.6.300", Value: "00402"}
	for _, npi := range []string{"1234567893", ""} {
		g := &Gateway{cfg: Config{NPI: npi, Clock: fixedClock}}
		b, err := g.eligibilityRequest("MBR-1", payer)
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
		if string(cer["insurer"]) != `{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00402"}}` {
			t.Fatalf("insurer %s, want the routed payer", cer["insurer"])
		}
	}
	g := &Gateway{cfg: Config{NPI: "1234567893", Clock: fixedClock}}
	if b, err := g.eligibilityRequest("MBR-1", shnsdk.PayerIdentifier{}); !errors.Is(err, shnsdk.ErrPayerRecordAbsent) || b != nil {
		t.Fatalf("no payer: err=%v built=%s, want a refusal", err, b)
	}
}
