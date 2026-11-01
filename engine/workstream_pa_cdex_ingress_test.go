//go:build !shn_coreonly

package engine

// workstream_pa_cdex_ingress_test.go — a participant's own system sends documentation for a
// prior authorization through its gateway: CDex $submit-attachment, at the
// plain endpoint (routed by its PayerId) or at a payer's return path (routed
// to the payer the path names). The submission is carried to the payer exactly
// as sent, and the payer's answer is relayed exactly. The network refuses one
// only on its own primitives: no registered payer to route to, and no
// readable MemberId to identify the patient by (D121).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdexSubmission is a $submit-attachment for member, naming payer by PayerId
// when payer is set, with one QuestionnaireResponse attachment.
func cdexSubmissionBody(member string, payer *shnsdk.PayerIdentifier) []byte {
	params := []string{
		`{"name":"TrackingId","valueIdentifier":{"system":"http://example.org/trace","value":"TRN-1"}}`,
		`{"name":"AttachTo","valueCode":"preauthorization"}`,
		`{"name":"OrganizationId","valueIdentifier":{"system":"http://hl7.org/fhir/sid/us-npi","value":"1234567893"}}`,
	}
	if member != "" {
		params = append(params, `{"name":"MemberId","valueIdentifier":{"system":"http://example.org/member","value":"`+member+`"}}`)
	}
	if payer != nil {
		params = append(params, `{"name":"PayerId","valueIdentifier":{"system":"`+payer.System+`","value":"`+payer.Value+`"}}`)
	}
	params = append(params, `{"name":"Attachment","part":[{"name":"Content","resource":{"resourceType":"QuestionnaireResponse","status":"completed","questionnaire":"http://example.org/Questionnaire/q"}}]}`)
	return []byte(`{"resourceType":"Parameters","parameter":[` + strings.Join(params, ",") + `]}`)
}

// withSecondPayerId is body, a submission naming one PayerId, with a second
// PayerId value p after the first.
func withSecondPayerId(t *testing.T, body []byte, p shnsdk.PayerIdentifier) []byte {
	t.Helper()
	const mark = `{"name":"Attachment"`
	if bytes.Count(body, []byte(`"name":"PayerId"`)) != 1 || bytes.Count(body, []byte(mark)) != 1 {
		t.Fatalf("withSecondPayerId needs a body with one PayerId and one Attachment: %s", body)
	}
	second := `{"name":"PayerId","valueIdentifier":{"system":"` + p.System + `","value":"` + p.Value + `"}},`
	return bytes.Replace(body, []byte(mark), []byte(second+mark), 1)
}

var cdexPayerCMS = &shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: shnsdk.CMSPayerIdentity.Value}

// cdexPayersAnswer is a payer's informational answer to a submission.
var cdexPayersAnswer = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"attachment received"}]}`)

// cdexPost is the EHR's POST of body to the plain endpoint, or, with payer
// set, to that payer's return path.
func cdexPost(body []byte, payer string) *http.Request {
	path := "/$submit-attachment"
	if payer != "" {
		path = cdexroute.Prefix + payer
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	if payer != "" {
		req.SetPathValue("payer", payer)
	}
	return req
}

// cdexRoute is the ingress route a submission to payer's return path, or
// with payer "" to the plain endpoint, is mounted with.
func cdexRoute(payer string) string {
	if payer != "" {
		return RouteCDexReturnSubmit
	}
	return RouteCDexSubmit
}

// runCDexIngress sends body through the provider's ingress (to payer's
// return path when payer is set) at level, with the payer answering answer,
// and returns the exchange, the caller's answer and the one exchange record.
func runCDexIngress(t *testing.T, level ConformanceEnforcement, body []byte, payer string, answer LegResult) (*inProcessExchange, *httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = level
	env.payerReturns(answer)
	var got exchangeRecords
	env.originator.cfg.ExchangeObserved = got.observe
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(cdexRoute(payer))(rec, cdexPost(body, payer))
	return env, rec, got.only(t)
}

// A submission is carried to the payer exactly as sent, and the payer's
// answer relayed exactly, at every level: at the plain endpoint routed by its
// PayerId, at a payer's return path to that payer with or without a PayerId.
func TestCDexIngress_CarriesTheSubmissionAsSent(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		body  []byte
		payer string
		route string
	}{
		"plain endpoint by PayerId":   {cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "", RouteCDexSubmit},
		"return path":                 {cdexSubmissionBody("MBR-COVERED", nil), "payer", RouteCDexReturnSubmit},
		"return path naming its own":  {cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "payer", RouteCDexReturnSubmit},
		"member the system holds not": {cdexSubmissionBody("MBR-NOBODY", cdexPayerCMS), "", RouteCDexSubmit},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env, rec, x := runCDexIngress(t, level, row.body, row.payer, LegResult{Response: testResponse(cdexPayersAnswer)})
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cdexPayersAnswer) {
					t.Fatalf("answer %d %s, want the payer's answer relayed exactly", rec.Code, rec.Body)
				}
				if sent := env.lastRequestPayload(); !bytes.Equal(sent, row.body) {
					t.Fatalf("the payer's side received\n%s\nwant the submission as sent\n%s", sent, row.body)
				}
				if x.Outcome != ExchangeAnswered || x.Route != row.route || x.Exchange != legCDexSubmit ||
					x.Recipient != env.payerID || len(x.Edits) != 0 {
					t.Fatalf("record %+v, want an answered %s exchange with the payer on route %s, carried unedited", x, legCDexSubmit, row.route)
				}
			})
		}
	}
}

// A submission to a payer's return path whose PayerId names another payer, or
// none the router knows, is carried to the payer the path names: the path is
// the network's address, the PayerId the payer's own content. The difference
// is observed, once, with no identifier, including when one of two PayerId
// values names the path's payer and the other does not. One whose PayerId
// names that payer is not.
func TestCDexIngress_ReturnPathPayerIdThatDiffersIsCarriedAndObserved(t *testing.T) {
	t.Parallel()
	other := &shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "99999"}
	for name, row := range map[string]struct {
		payer    *shnsdk.PayerIdentifier
		second   *shnsdk.PayerIdentifier // a second PayerId value
		observed bool
	}{
		"another payer":                  {other, nil, true},
		"the path's payer":               {cdexPayerCMS, nil, false},
		"the path's payer twice":         {cdexPayerCMS, cdexPayerCMS, false},
		"the path's payer, then another": {cdexPayerCMS, other, true},
		"another, then the path's payer": {other, cdexPayerCMS, true},
		"no PayerId":                     {nil, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
			var events []ObserverEvent
			env.originator.cfg.Observer = func(e ObserverEvent) { events = append(events, e) }
			body := cdexSubmissionBody("MBR-COVERED", row.payer)
			if row.second != nil {
				body = withSecondPayerId(t, body, *row.second)
			}
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexReturnSubmit)(rec, cdexPost(body, "payer"))
			if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), body) {
				t.Fatalf("answer %d %s, want the submission carried to the path's payer", rec.Code, rec.Body)
			}
			var seen int
			for _, e := range events {
				if e.Kind == PayerIdDiffersEvent {
					seen++
					if e.Detail != "" || e.CorrelationID != "" {
						t.Fatalf("event %+v carries more than its kind and leg", e)
					}
				}
			}
			if want := map[bool]int{true: 1}[row.observed]; seen != want {
				t.Fatalf("%s observed %d times, want %d (events %+v)", PayerIdDiffersEvent, seen, want, events)
			}
		})
	}
}

// A submission the network cannot route, or whose patient it cannot
// identify, is refused before anything is sent, at every level, recorded on
// the primitive it fails: routing (no registered payer to route to; there is
// no default payer), per-leg authority (no readable MemberId, D121), and
// message integrity (a body that can be read two ways).
func TestCDexIngress_RefusesOnlyOnTheNetworksPrimitives(t *testing.T) {
	t.Parallel()
	unregistered := &shnsdk.PayerIdentifier{System: shnsdk.CMSPayerIdentity.System, Value: "99999"}
	twoMembers := bytes.Replace(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), []byte(`{"name":"Attachment"`),
		[]byte(`{"name":"MemberId","valueIdentifier":{"value":"MBR-OTHER"}},{"name":"Attachment"`), 1)
	repeated := bytes.Replace(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), []byte(`"resourceType":"Parameters"`),
		[]byte(`"resourceType":"Parameters","resourceType":"Bundle"`), 1)
	for name, row := range map[string]struct {
		body   []byte
		payer  string
		status int
		msg    string
		rule   string
	}{
		"plain endpoint, no PayerId":         {cdexSubmissionBody("MBR-COVERED", nil), "", http.StatusUnprocessableEntity, "the submission names no payer to route to", RefusalRouting},
		"plain endpoint, unregistered payer": {cdexSubmissionBody("MBR-COVERED", unregistered), "", http.StatusUnprocessableEntity, "no registered payer for identifier " + unregistered.System + "|99999", RefusalRouting},
		"return path, no such participant":   {cdexSubmissionBody("MBR-COVERED", nil), "nobody", http.StatusUnprocessableEntity, "this return path names no registered payer", RefusalRouting},
		// The provider is registered, but not as a payer.
		"return path, not a payer":     {cdexSubmissionBody("MBR-COVERED", nil), "provider", http.StatusUnprocessableEntity, "this return path names no registered payer", RefusalRouting},
		"no MemberId":                  {cdexSubmissionBody("", cdexPayerCMS), "", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"two MemberIds that differ":    {twoMembers, "", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"not a Parameters resource":    {[]byte(`{"resourceType":"Bundle","type":"collection"}`), "", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"not JSON":                     {[]byte(`not json`), "", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"a member name repeated":       {repeated, "", http.StatusBadRequest, "it repeats a member name", RefusalIntegrity},
		"MemberId with no value":       {bytes.Replace(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), []byte(`"value":"MBR-COVERED"`), []byte(`"value":""`), 1), "", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"return path, no MemberId":     {cdexSubmissionBody("", nil), "payer", http.StatusBadRequest, errCDexNoMember, RefusalAuthority},
		"return path, a name repeated": {bytes.Replace(repeated, []byte(`,{"name":"PayerId"`), []byte(`,{"name":"x"`), 1), "payer", http.StatusBadRequest, "it repeats a member name", RefusalIntegrity},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				env := newInProcessExchange(t)
				env.originator.cfg.ConformanceEnforcement = level
				var got exchangeRecords
				env.originator.cfg.ExchangeObserved = got.observe
				rec := httptest.NewRecorder()
				env.originator.ingressRoute(cdexRoute(row.payer))(rec, cdexPost(row.body, row.payer))
				refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
				x := got.only(t)
				wantRefusal(t, x, row.status, RefusedByProviderGateway, row.rule)
				if x.Exchange != legCDexSubmit {
					t.Fatalf("refusal recorded as exchange %q, want %s", x.Exchange, legCDexSubmit)
				}
			})
		}
	}
}

// The patient is bound as on the other native legs: a member this
// participant's own system does not hold is carried (above), unless the
// participant opted in to checking members, at every level: a check it opted
// into, recorded as conformance.
func TestCDexIngress_UnknownMemberRefusedOnlyWhenOptedIn(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			env.originator.cfg.RequireKnownMembers = true
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(cdexSubmissionBody("MBR-NOBODY", cdexPayerCMS), ""))
			refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "unknown member")
			wantRefusal(t, got.only(t), http.StatusBadRequest, RefusedByProviderGateway, RefusalConformance)
		})
	}
}

// The payer's answer is its own: an error relayed exactly as the payer sent
// it, and one that repeats a member name refused on message integrity, at
// every level.
func TestCDexIngress_RelaysThePayersAnswer(t *testing.T) {
	t.Parallel()
	oo := []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid","diagnostics":"TrackingId is required."}]}`)
	repeated := []byte(`{"resourceType":"OperationOutcome","resourceType":"Bundle","issue":[]}`)
	for _, level := range allLevels {
		t.Run("error/"+level.String(), func(t *testing.T) {
			_, rec, _ := runCDexIngress(t, level, cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "",
				LegResult{Status: http.StatusBadRequest, Response: testResponse(oo)})
			if rec.Code != http.StatusBadRequest || !bytes.Equal(rec.Body.Bytes(), oo) {
				t.Fatalf("answer %d %s, want the payer's 400 relayed exactly", rec.Code, rec.Body)
			}
		})
		t.Run("media type/"+level.String(), func(t *testing.T) {
			_, rec, _ := runCDexIngress(t, level, cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "",
				LegResult{Response: relay.ForTest(cdexPayersAnswer, "application/json"), AnswerStatus: http.StatusOK})
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || !bytes.Equal(rec.Body.Bytes(), cdexPayersAnswer) {
				t.Fatalf("answer %d %q %s, want the payer's answer with its own media type", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
		})
		t.Run("repeated member/"+level.String(), func(t *testing.T) {
			_, rec, x := runCDexIngress(t, level, cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), "", LegResult{Response: testResponse(repeated)})
			if rec.Code != http.StatusBadGateway || bytes.Contains(rec.Body.Bytes(), []byte("Bundle")) {
				t.Fatalf("answer %d %s, want 502 and the payer's bytes not relayed", rec.Code, rec.Body)
			}
			wantRefusal(t, x, http.StatusBadGateway, RefusedByProviderGateway, RefusalIntegrity)
		})
	}
}

// A body over the attachment ceiling is refused 413 before anything is sent,
// never cut short and read as another message, and recorded as routing, at
// every level.
func TestCDexIngress_OversizeSubmissionIsRefused413(t *testing.T) {
	t.Parallel()
	big := bytes.Replace(cdexSubmissionBody("MBR-COVERED", cdexPayerCMS), []byte(`"status":"completed"`),
		[]byte(`"status":"completed","text":{"div":"`+strings.Repeat("a", int(shnsdk.MaxAttachmentBytes))+`"}`), 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.ConformanceEnforcement = level
			var got exchangeRecords
			env.originator.cfg.ExchangeObserved = got.observe
			rec := httptest.NewRecorder()
			env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(big, ""))
			refusedBeforeTheNetwork(t, env, rec, http.StatusRequestEntityTooLarge, "request body too large")
			x := got.only(t)
			wantRefusal(t, x, http.StatusRequestEntityTooLarge, RefusedByProviderGateway, RefusalRouting)
			if x.Exchange != legCDexSubmit {
				t.Fatalf("refusal recorded as exchange %q, want %s", x.Exchange, legCDexSubmit)
			}
		})
	}
}

// A gateway that declares its contract lines explicitly, naming no CDex line
// (a SHN_CONTRACT_VERSIONS of CRD, DTR and PAS lines only), still carries a
// submission, with REQUIRE_SHARED_LINE set and a payer declaring other lines:
// the leg rides no declared contract line, so no line can refuse it.
func TestCDexIngress_CarriedWhateverTheDeclaredLines(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.DeclaredContractVersions = []string{shnsdk.ContractPACRD20, shnsdk.ContractPADTR20, shnsdk.ContractPAPAS20}
	env.originator.cfg.RequireSharedLine = true
	payer, _ := env.originator.cfg.Reg.Lookup(env.payerID)
	payer.ContractVersions = []string{shnsdk.ContractPAPAS22}
	env.originator.cfg.Reg.Set(env.payerID, payer)
	env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
	body := cdexSubmissionBody("MBR-COVERED", cdexPayerCMS)
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCDexSubmit)(rec, cdexPost(body, ""))
	if rec.Code != http.StatusOK || !bytes.Equal(env.lastRequestPayload(), body) {
		t.Fatalf("answer %d %s, want the submission carried whatever the declared lines", rec.Code, rec.Body)
	}
}

// GET /cdex/payers lists every registered payer that takes the leg with its
// return path: the address cdexroute.ReturnBase builds, byte for byte. A
// participant that is no payer is not listed, nor is a payer that does not take
// the leg, whose return path would always refuse; a
// registry with no payer lists none (an empty list, not an error).
func TestCDexPayers_ListsEachPayersReturnPath(t *testing.T) {
	env := newInProcessExchange(t)
	env.originator.cfg.IngressBaseURL = "https://provider.example.test/fhir"
	payer, _ := env.originator.cfg.Reg.Lookup(env.payerID)
	payer.PayerIDs = []shnsdk.PayerIdentifier{*cdexPayerCMS}
	env.originator.cfg.Reg.Set(env.payerID, payer)
	list := func() (int, []cdexPayer) {
		rec := httptest.NewRecorder()
		env.originator.handleCDexPayers(rec, httptest.NewRequest(http.MethodGet, CDexPayersPath, nil))
		var doc struct {
			Payers []cdexPayer `json:"payers"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("listing %s: %v", rec.Body, err)
			}
		}
		return rec.Code, doc.Payers
	}
	want, err := cdexroute.ReturnBase(env.originator.cfg.IngressBaseURL, env.payerID)
	if err != nil {
		t.Fatal(err)
	}
	// A holder that is no payer is left out, whatever legs it declares.
	provider, ok := env.originator.cfg.Reg.Lookup("provider")
	if !ok {
		t.Fatal("fixture: no provider in the registry")
	}
	provider.Legs = []string{legCDexSubmit}
	env.originator.cfg.Reg.Set("provider", provider)
	code, payers := list()
	if code != http.StatusOK || len(payers) != 1 || payers[0].Participant != env.payerID || payers[0].ReturnBase != want ||
		len(payers[0].PayerIdentifiers) != 1 || payers[0].PayerIdentifiers[0] != *cdexPayerCMS {
		t.Fatalf("listing %d %+v, want the one payer at %s", code, payers, want)
	}
	for name, legs := range map[string][]string{"no list": nil, "an empty list": {}, "a list without the leg": {"crd-order-select"}} {
		notTaking := payer
		notTaking.Legs = legs
		env.originator.cfg.Reg.Set(env.payerID, notTaking)
		if code, payers := list(); code != http.StatusOK || len(payers) != 0 {
			t.Fatalf("%s: listing %d %+v, want it left out", name, code, payers)
		}
	}
	env.originator.cfg.Reg.Set(env.payerID, payer)
	env.originator.cfg.Reg.Delete(env.payerID)
	rec := httptest.NewRecorder()
	env.originator.handleCDexPayers(rec, httptest.NewRequest(http.MethodGet, CDexPayersPath, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"payers":[]}` {
		t.Fatalf("listing with no payer %d %s, want an empty list", rec.Code, rec.Body)
	}
	// A return path that cannot be built is an error, never a shorter list.
	env.originator.cfg.Reg.Set(env.payerID, payer)
	env.originator.cfg.IngressBaseURL = ""
	if code, _ := list(); code != http.StatusServiceUnavailable {
		t.Fatalf("listing with no ingress base %d, want 503", code)
	}
}
