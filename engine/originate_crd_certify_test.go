package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// withExtraCard is answer with card appended to its cards.
func withExtraCard(t *testing.T, answer []byte, card map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(answer, &doc); err != nil {
		t.Fatal(err)
	}
	cards, _ := doc["cards"].([]any)
	doc["cards"] = append(cards, card)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// crdFindings is every CDS Hooks finding the gateway observed.
type crdFindings struct {
	mu   sync.Mutex
	list []ConformanceFinding
}

func (c *crdFindings) observe(e ObserverEvent) {
	if e.Kind != ConformanceObservedEvent {
		return
	}
	var f ConformanceFinding
	if json.Unmarshal([]byte(e.Detail), &f) == nil && f.Kind == string(KindCDSEnvelope) {
		c.mu.Lock()
		c.list = append(c.list, f)
		c.mu.Unlock()
	}
}

// A CRD answer to a leg the provider's gateway originates is certified against
// the CDS Hooks response rules at the gateway's own level, as a relayed one
// is: none carries it unchecked, observe records and carries, structural
// refuses a structural rule and records a deeper one, strict refuses both. A
// repeated member name is message integrity and refuses at every level.
func TestOriginatedCRD_AnswerCertifiedAtTheProvidersLevel(t *testing.T) {
	topic := map[string]any{"system": "http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp", "code": "coverage-info"}
	source := map[string]any{"label": "payer", "topic": topic}
	structural := map[string]any{"summary": "Coverage note", "indicator": "bogus", "source": source}     // card.indicator
	deeper := map[string]any{"summary": strings.Repeat("x", 150), "indicator": "info", "source": source} // card.summary.length
	for _, tc := range []struct {
		name       string
		level      ConformanceEnforcement
		card       map[string]any
		dupKey     bool
		wantStatus int
		wantRule   string // "" = no finding
		wantDec    string
	}{
		{"none carries a broken answer unchecked", EnforcementNone, structural, false, http.StatusOK, "", ""},
		{"observe records and carries", EnforcementObserve, structural, false, http.StatusOK, "card.indicator", "relayed"},
		{"structural refuses a structural rule", EnforcementStructural, structural, false, http.StatusBadGateway, "card.indicator", "refused"},
		{"structural records a deeper rule", EnforcementStructural, deeper, false, http.StatusOK, "card.summary.length", "relayed"},
		{"strict refuses a deeper rule", EnforcementStrict, deeper, false, http.StatusBadGateway, "card.summary.length", "refused"},
		{"strict refuses a structural rule", EnforcementStrict, structural, false, http.StatusBadGateway, "card.indicator", "refused"},
		{"none refuses a repeated member name", EnforcementNone, nil, true, http.StatusBadGateway, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sor, _ := originSystem(t, prefetchMember)
			_, _, order := originRecords(prefetchMember)
			g, payer := originGateway(t, "provider-data", sor, nil, func(tx string, req []byte) (int, []byte) {
				answer := payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
				if tc.card != nil {
					answer = withExtraCard(t, answer, tc.card)
				}
				if tc.dupKey {
					answer = []byte(`{"cards":[],` + strings.TrimPrefix(string(answer), "{"))
				}
				return 0, answer
			})
			g.cfg.ConformanceEnforcement = tc.level
			var found crdFindings
			g.cfg.Observer = found.observe

			rec := httptest.NewRecorder()
			g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", nil), prefetchMember)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if len(payer.sent("crd-order-select")) != 1 {
				t.Fatal("the originated CRD leg was not sent")
			}
			if tc.wantStatus == http.StatusBadGateway && !strings.Contains(rec.Body.String(), "payer CRD response is not a valid CDS Hooks response") {
				t.Fatalf("a refusal must name the CDS Hooks rules: %s", rec.Body)
			}
			found.mu.Lock()
			defer found.mu.Unlock()
			if tc.wantRule == "" {
				if len(found.list) != 0 {
					t.Fatalf("unexpected CDS Hooks findings %+v", found.list)
				}
				return
			}
			var got *ConformanceFinding
			for i, f := range found.list {
				if f.Rule == tc.wantRule {
					got = &found.list[i]
				}
			}
			if got == nil {
				t.Fatalf("no %s finding in %+v", tc.wantRule, found.list)
			}
			if got.Decision != tc.wantDec || got.Seam != "originate" || got.Whose != "peer" || got.LegType != "crd-order-select" {
				t.Fatalf("finding %+v: want decision %s, seam originate, whose peer, leg crd-order-select", got, tc.wantDec)
			}
		})
	}
}

// OriginateLeg certifies both originated CRD legs (order-select and
// order-dispatch), and leaves a carried leg (the Da Vinci ingress, which
// certifies the answer itself in crdAnswerOutcome) to its caller, so an
// answer is never judged twice.
func TestOriginateLeg_CertifiesOriginatedCRDLegsOnly(t *testing.T) {
	topic := map[string]any{"system": "http://hl7.org/fhir/us/davinci-crd/CodeSystem/temp", "code": "coverage-info"}
	broken := map[string]any{"summary": "Coverage note", "indicator": "bogus", "source": map[string]any{"label": "payer", "topic": topic}}
	_, _, order := originRecords(prefetchMember)
	sor, _ := originSystem(t, prefetchMember)
	for _, tc := range []struct {
		leg     string
		carried bool
		refused bool
	}{
		{"crd-order-select", false, true},
		{"crd-order-dispatch", false, true},
		{"crd-order-select", true, false},
	} {
		t.Run(tc.leg+map[bool]string{true: " carried", false: " originated"}[tc.carried], func(t *testing.T) {
			g, _ := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, withExtraCard(t, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, ""), broken)
			})
			g.cfg.ConformanceEnforcement = EnforcementStrict
			_, err := g.OriginateLeg(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), "payer", tc.leg, "pci-1", strings.Repeat("0", 31)+"9", "",
				Content{WorkstreamType: workstreamPA, Payload: testRequest([]byte(`{"hook":"order-select"}`)), Carried: tc.carried})
			switch {
			case tc.refused && (err == nil || !strings.Contains(err.Error(), "not a valid CDS Hooks response")):
				t.Fatalf("want the CDS Hooks refusal, got %v", err)
			case !tc.refused && err != nil:
				t.Fatalf("want the answer returned for its caller to certify, got %v", err)
			}
		})
	}
}
