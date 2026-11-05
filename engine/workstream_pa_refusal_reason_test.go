//go:build !shn_coreonly

package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// Rows for the reason a gateway records for a refusal it makes before the request
// reaches the Hub (ExchangeRecord.RefusalReason, refusalreason.go).

// wantReasonClosed: a recorded reason is a registered sentence, never other,
// and only on this gateway's own refusal of its participant's call. Every
// record a test collects is held to it (wantClosed).
func wantReasonClosed(t *testing.T, rec ExchangeRecord) {
	t.Helper()
	if rec.RefusalReason == "" {
		return
	}
	if rec.Outcome != ExchangeRefused || rec.Direction != DirectionIngress ||
		(rec.RefusedBy != RefusedByProviderGateway && rec.RefusedBy != RefusedByPayerGateway) {
		t.Errorf("reason %q recorded on %s %s by %q: only a gateway's own refusal of its participant's call carries one", rec.RefusalReason, rec.Direction, rec.Outcome, rec.RefusedBy)
	}
	if closedReason(refusalReason(rec.RefusalReason)) != rec.RefusalReason || rec.RefusalReason == ExchangeOther {
		t.Errorf("reason %q is not a registered sentence: a refusing site named a reason that is not in the registry", rec.RefusalReason)
	}
}

// wantPreHubReason: a refusal this gateway made of its participant's call on a
// network primitive, answered 4xx, names its reason. Every refusal row that
// asserts its record is held to it (wantRefusal).
func wantPreHubReason(t *testing.T, rec ExchangeRecord) {
	t.Helper()
	switch rec.Rule {
	case RefusalAuthentication, RefusalAuthority, RefusalIntegrity, RefusalRouting, RefusalLimit:
	default:
		return
	}
	if rec.Direction == DirectionIngress && rec.Status/100 == 4 &&
		(rec.RefusedBy == RefusedByProviderGateway || rec.RefusedBy == RefusedByPayerGateway) && rec.RefusalReason == "" {
		t.Errorf("%s refusal (%d) on route %s recorded no reason", rec.Rule, rec.Status, rec.Route)
	}
}

// answerText is the refusal text an answer gives its caller: the "error" of a
// JSON answer, the issues' diagnostics of an OperationOutcome, else the body.
func answerText(body string) string {
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		return body
	}
	if e, ok := doc["error"].(string); ok {
		return e
	}
	var out []string
	issues, _ := doc["issue"].([]any)
	for _, i := range issues {
		if m, ok := i.(map[string]any); ok {
			if d, ok := m["diagnostics"].(string); ok {
				out = append(out, d)
			}
		}
	}
	if len(out) > 0 {
		return strings.Join(out, "\n")
	}
	return body
}

// wantReasonInAnswer: the record names a reason, and it is the text the
// participant was answered with (the whole of it, or the sentence an added
// detail follows).
func wantReasonInAnswer(t *testing.T, rec ExchangeRecord, body string) {
	t.Helper()
	if rec.RefusalReason == "" {
		t.Fatalf("record names no reason for its %s refusal (%d)", rec.Rule, rec.Status)
	}
	if text := answerText(body); !strings.Contains(text, rec.RefusalReason) {
		t.Fatalf("recorded reason %q is not what the participant was answered: %q", rec.RefusalReason, text)
	}
}

// Every ingress route records the authentication refusal's reason.
func TestRefusalReason_IngressAuthentication(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{CorrelationGen: func() string { return "minted-401" }, ExchangeObserved: got.observe, HolderID: "provider"}}
	for _, route := range workstreamIngressRouteNames() {
		t.Run(route, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
			r.SetPathValue("id", "shn-order-sign")
			w := httptest.NewRecorder()
			g.ingressRoute(route)(w, r)
			rec := got.only(t)
			wantRefusal(t, rec, http.StatusUnauthorized, RefusedByProviderGateway, RefusalAuthentication)
			if rec.RefusalReason != string(reasonIngressAuthentication) {
				t.Fatalf("reason %q, want %q", rec.RefusalReason, reasonIngressAuthentication)
			}
			wantReasonInAnswer(t, rec, w.Body.String())
		})
	}
}

// A payer identifier no participant registers: the answer names the
// identifier the request's Coverage carries; the record keeps the sentence
// alone.
func TestRefusalReason_UnregisteredPayerKeepsNoIdentifier(t *testing.T) {
	const marker = "77441"
	env := newInProcessExchange(t)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader(pasIngressBundle(marker, "")))
	env.originator.ingressRoute(RoutePAS)(w, r)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), marker) {
		t.Fatalf("answer %d %s, want 422 naming %s", w.Code, w.Body.String(), marker)
	}
	rec := got.only(t)
	wantRefusal(t, rec, http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalRouting)
	if rec.RefusalReason != string(reasonUnregisteredPayer) {
		t.Fatalf("reason %q, want %q", rec.RefusalReason, reasonUnregisteredPayer)
	}
	wantReasonInAnswer(t, rec, w.Body.String())
	if strings.Contains(rec.RefusalReason, marker) {
		t.Fatalf("the record carries the request's identifier: %q", rec.RefusalReason)
	}
}

// An eligibility request that repeats a member name: the answer names the
// member; the record keeps the sentence without it.
func TestRefusalReason_RepeatedMemberKeepsNoName(t *testing.T) {
	const patient = `"patient":{"reference":"Patient/` + dtrFrameMember + `"}`
	cer := string(eligibilityRequest(t, dtrFrameMember))
	body := strings.Replace(cer, patient, patient+`,"patient":{"reference":"Patient/`+anotherHeldMember+`"}`, 1)
	if body == cer {
		t.Fatal("the fixture names its patient another way")
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, x := runEligibilityIngress(t, level, []byte(body), payersEligibilityAnswer("Patient/"+dtrFrameMember))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, `repeats the JSON member \"patient\"`)
			wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalIntegrity)
			if x.RefusalReason != string(reasonEligibilityTwoWays) {
				t.Fatalf("reason %q, want %q", x.RefusalReason, reasonEligibilityTwoWays)
			}
			if strings.Contains(x.RefusalReason, `"patient"`) || strings.Contains(x.RefusalReason, "JSON member") {
				t.Fatalf("the record carries the request's member name: %q", x.RefusalReason)
			}
		})
	}
}

// A federated-query Task that is no data request: the answer carries the
// reader's text, read from the Task; the record keeps the sentence alone.
func TestRefusalReason_FederatedQueryKeepsNoReaderText(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env, rec, x := runFQIngress(t, level, []byte(`{"resourceType":"Claim"}`), nil)
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, `expected Task, got \"Claim\"`)
			wantRefusal(t, x, http.StatusBadRequest, RefusedByProviderGateway, RefusalAuthority)
			if x.RefusalReason != string(reasonFederatedQueryNoPatient) {
				t.Fatalf("reason %q, want %q", x.RefusalReason, reasonFederatedQueryNoPatient)
			}
			if strings.Contains(x.RefusalReason, "Claim") {
				t.Fatalf("the record carries the request's text: %q", x.RefusalReason)
			}
		})
	}
}

// A reason the registry does not name is recorded as other, never as given:
// a value read from a request cannot reach the record even through a site
// that names one.
func TestRefusalReason_AnUnregisteredValueIsOther(t *testing.T) {
	const fromTheBody = "Member123 Jane Doe 1970-01-01"
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe, HolderID: "provider"}}
	h := g.recordExchange(RoutePAS, DirectionIngress, func(w http.ResponseWriter, r *http.Request) {
		exchangeOf(r.Context()).refusedFor(RefusalRouting, refusalReason(fromTheBody))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": fromTheBody})
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader("{}")))
	// Read without wantClosed, which refuses other: it is what this row makes.
	if len(got.recs) != 1 {
		t.Fatalf("want one record, got %d", len(got.recs))
	}
	if rec := got.recs[0]; rec.RefusalReason != ExchangeOther || strings.Contains(rec.RefusalReason, "Member123") {
		t.Fatalf("reason %q, want %q", rec.RefusalReason, ExchangeOther)
	}
}

// Only a refusal carries a reason: a later settled outcome clears it, and an
// answered call names none.
func TestRefusalReason_OnlyARefusalCarriesOne(t *testing.T) {
	var got exchangeRecords
	g := &Gateway{cfg: Config{ExchangeObserved: got.observe, HolderID: "provider"}}
	for name, row := range map[string]struct {
		handle func(w http.ResponseWriter, r *http.Request)
		want   string
	}{
		"answered": {func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{})
		}, ""},
		"a refusal named, then answered": {func(w http.ResponseWriter, r *http.Request) {
			exchangeOf(r.Context()).refusingFor(RefusalRouting, reasonNoPayerRouter)
			writeJSON(w, http.StatusOK, map[string]string{})
		}, ""},
		"a refusal settled, then another outcome": {func(w http.ResponseWriter, r *http.Request) {
			x := exchangeOf(r.Context())
			x.refusedFor(RefusalRouting, reasonNoPayerRouter)
			x.ownFailure()
			writeJSON(w, http.StatusInternalServerError, map[string]string{})
		}, ""},
		"a refusal named and written": {func(w http.ResponseWriter, r *http.Request) {
			exchangeOf(r.Context()).refusingFor(RefusalRouting, reasonNoPayerRouter)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msgNoPayerRouter})
		}, string(reasonNoPayerRouter)},
		"a refusal named with no reason": {func(w http.ResponseWriter, r *http.Request) {
			exchangeOf(r.Context()).refusingFor(RefusalRouting, reasonNoPayerRouter)
			exchangeOf(r.Context()).refusing(RefusalRouting)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msgNoPayerRouter})
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g.recordExchange(RoutePAS, DirectionIngress, row.handle)(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/Claim/$submit", strings.NewReader("{}")))
			if rec := got.only(t); rec.RefusalReason != row.want {
				t.Fatalf("reason %q, want %q", rec.RefusalReason, row.want)
			}
		})
	}
}

// The framed-DTR reason is the SDK's refusal text, which the answer carries.
func TestRefusalReason_FramedDTRIsTheSDKsText(t *testing.T) {
	if string(reasonFramedDTRUnsupported) != shnsdk.ErrFramedDTRUnsupported.Error() {
		t.Fatalf("reason %q, SDK text %q", reasonFramedDTRUnsupported, shnsdk.ErrFramedDTRUnsupported.Error())
	}
}

// The batch refusal's sentence names the bound the gateway reads.
func TestEligibilityBatch_BoundTextIsTheBound(t *testing.T) {
	if eligibilityBatchBoundText != strconv.Itoa(maxEligibilityBatch) {
		t.Fatalf("eligibilityBatchBoundText %q, maxEligibilityBatch %d", eligibilityBatchBoundText, maxEligibilityBatch)
	}
}
