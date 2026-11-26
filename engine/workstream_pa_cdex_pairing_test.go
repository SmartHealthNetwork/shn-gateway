//go:build !shn_coreonly

package engine

// workstream_pa_cdex_pairing_test.go — a CDex attachment submission pairs with
// the attachment request it answers, on the payer's gateway and the
// provider's: each records the request it carried, by the other participant
// and the request's tracking identifier, with the patient it bound it to, and
// a submission from that participant naming that tracking identifier and the
// same member binds to that patient. The request and its submission go through
// two gateways sharing one store, as two replicas of one participant's gateway
// do. Pairing never refuses: an unpaired submission is carried, bound by the
// rule for a leg with no subject Patient over its top-level resources, and
// observed with its reason.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The IG example's tracking identifier, member and subject Patient.
const (
	pairTrackSystem = "http://example.org/payer"
	pairTrackValue  = "trackingid123"
	pairMember      = "Member123"
)

var (
	// pairPCI is the IG example's binding from its own subject Patient.
	pairPCI = derivedPCI(pairMember, "1987-02-20", "Shaw")
	// pairAlone is the binding by the member number alone.
	pairAlone = derivedPCI(pairMember, "", "")
)

// pairSubmission is a $submit-attachment for member naming the tracking
// identifier (none when value is ""), whose attachment content is a
// QuestionnaireResponse, or with patientInContent a Bundle carrying a Patient
// for the member, under shnsdk.MemberSystem, whose demographics are not the
// request's.
func pairSubmission(member, system, value string, patientInContent bool) []byte {
	var params []any
	if value != "" {
		params = append(params, map[string]any{"name": "TrackingId", "valueIdentifier": map[string]any{"system": system, "value": value}})
	}
	params = append(params,
		map[string]any{"name": "AttachTo", "valueCode": "preauthorization"},
		map[string]any{"name": "OrganizationId", "valueIdentifier": map[string]any{"system": "http://hl7.org/fhir/sid/us-npi", "value": "1234567893"}},
		map[string]any{"name": "MemberId", "valueIdentifier": map[string]any{"system": "http://example.org/member", "value": member}})
	content := map[string]any{"resourceType": "QuestionnaireResponse", "status": "completed", "questionnaire": "http://example.org/Questionnaire/q"}
	if patientInContent {
		content = map[string]any{"resourceType": "Bundle", "type": "collection", "entry": []any{map[string]any{"resource": map[string]any{
			"resourceType": "Patient", "id": member,
			"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": member}},
			"name":       []any{map[string]any{"family": "Elsewhere"}}, "birthDate": "1950-05-05"}}}}
	}
	params = append(params, map[string]any{"name": "Attachment", "part": []any{map[string]any{"name": "Content", "resource": content}}})
	b, err := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": params})
	if err != nil {
		panic(err)
	}
	return b
}

// pairTask is the IG example Task with its subject Patient's family name
// family, its member number member and its tracking value value.
func pairTask(t *testing.T, family, member, value string) []byte {
	t.Helper()
	return cdexRequestMutate(t, func(m map[string]any) {
		for _, c := range m["contained"].([]any) {
			p := c.(map[string]any)
			if p["resourceType"] != "Patient" {
				continue
			}
			p["name"] = []any{map[string]any{"family": family}}
			for _, id := range p["identifier"].([]any) {
				id.(map[string]any)["value"] = member
			}
		}
		for _, id := range m["identifier"].([]any) {
			id.(map[string]any)["value"] = value
		}
	})
}

// eventLog collects a gateway's observer events.
type eventLog struct {
	mu     sync.Mutex
	events []ObserverEvent
}

func (l *eventLog) observe(e ObserverEvent) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

// unpaired is the reason of each CDexRequestUnpairedEvent, and whether any
// binding-differs event was raised.
func (l *eventLog) unpaired() (reasons []string, differs bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		switch e.Kind {
		case CDexRequestUnpairedEvent:
			if e.LegType != legCDexSubmit || e.Direction != "ingress" {
				panic("an unpaired event on " + e.LegType + "/" + e.Direction)
			}
			reasons = append(reasons, e.Detail)
		case SubjectBindingDiffersEvent:
			differs = true
		}
	}
	return reasons, differs
}

// payerPairing is a payer's gateway: its ingress sends a provider attachment
// requests, and its inbound handler takes that provider's submissions, the two
// sharing one store.
type payerPairing struct {
	p   *levelPayer
	log *eventLog
}

func newPayerPairing(t *testing.T, level ConformanceEnforcement) *payerPairing {
	t.Helper()
	p := newLevelPayer(t, level)
	p.partner.respByPath[cdexSubmitPath] = cdexPayersAnswer
	l := &eventLog{}
	p.g.cfg.Observer = l.observe
	return &payerPairing{p: p, log: l}
}

// request sends body to provider through a payer ingress on the same store,
// and returns the patient the request was authorized for.
func (pp *payerPairing) request(t *testing.T, body []byte, provider string) string {
	t.Helper()
	env, _ := payerIngressExchange(t, pp.p.level)
	env.originator.cfg.Store = pp.p.g.cfg.Store
	env.originator.cfg.Reg.Set(provider, shnsdk.RegistryEntry{ID: provider, Role: "provider",
		EncPub: env.substrate.recipientEncPub, Legs: []string{legCDexRequest}, MessageFrames: shnsdk.SupportedMessageFrames()})
	env.payerReturns(LegResult{Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated})
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(RouteCDexRequestBase)(rec, cdexRequestPost(body, provider))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request answered %d %s, want it carried", rec.Code, rec.Body)
	}
	env.substrate.mu.Lock()
	defer env.substrate.mu.Unlock()
	if len(env.substrate.authorizedSubjects) != 1 {
		t.Fatalf("request authorized for %q", env.substrate.authorizedSubjects)
	}
	return env.substrate.authorizedSubjects[0]
}

// provider is the provider whose submissions this payer takes.
func (pp *payerPairing) provider() string { return pp.p.requester.ID }

// submit delivers body from the provider under a token for want, and reports
// the unpaired reasons and whether the payer's binding differs from want.
func (pp *payerPairing) submit(t *testing.T, body []byte, want string) ([]string, bool) {
	t.Helper()
	pp.log.mu.Lock()
	pp.log.events = nil
	pp.log.mu.Unlock()
	ans := pp.p.sendAs(t, legCDexSubmit, "", body, want)
	if ans.status != http.StatusOK || pp.p.partner.lastPath != cdexSubmitPath {
		t.Fatalf("submission answered %d %s, want it carried to the payer's system", ans.status, ans.body)
	}
	return pp.log.unpaired()
}

// providerPairing is a provider's gateway: its inbound handler takes a
// payer's attachment requests, and its ingress sends that payer the
// submissions, the two sharing one store.
type providerPairing struct {
	level ConformanceEnforcement
	store Store
	req   *cdexRequestProvider
	log   *eventLog
	// metric is the submitting gateway's StoreErrorMetric, when set.
	metric func(string)
}

func newProviderPairing(t *testing.T, level ConformanceEnforcement, store Store) *providerPairing {
	t.Helper()
	l := &eventLog{}
	sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
	req := configuredProvider(t, level, sys, func(c *Config) { c.Store = store; c.Observer = l.observe })
	return &providerPairing{level: level, store: store, req: req, log: l}
}

// payer is the payer the requests come from.
func (pp *providerPairing) payer() string { return pp.req.payer.ID }

// request delivers body from the payer and reports whether the provider's
// binding differs from want.
func (pp *providerPairing) request(t *testing.T, body []byte, want string) bool {
	t.Helper()
	pp.log.mu.Lock()
	pp.log.events = nil
	pp.log.mu.Unlock()
	ans := pp.req.sendWith(t, body, func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Subject = want })
	if ans.status != http.StatusCreated {
		t.Fatalf("request answered %d %s, want it carried", ans.status, ans.body)
	}
	_, differs := pp.log.unpaired()
	return differs
}

// submit sends body to payer's return path through a provider ingress on the
// same store, and returns the patient it was authorized for and the unpaired
// reasons.
func (pp *providerPairing) submit(t *testing.T, body []byte, payer string) (string, []string) {
	t.Helper()
	status, subject, reasons := pp.submitRaw(t, body, payer)
	if status != http.StatusOK {
		t.Fatalf("submission answered %d (unpaired %q), want it carried", status, reasons)
	}
	return subject, reasons
}

// submitRaw is submit, answering the status, and the patient only when the
// submission was sent ("" otherwise).
func (pp *providerPairing) submitRaw(t *testing.T, body []byte, payer string) (int, string, []string) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.ConformanceEnforcement = pp.level
	env.originator.cfg.Store = pp.store
	l := &eventLog{}
	env.originator.cfg.Observer = l.observe
	env.originator.cfg.StoreErrorMetric = pp.metric
	e, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatal("no payer in the exchange's registry")
	}
	e.ID = payer
	env.originator.cfg.Reg.Set(payer, e)
	env.payerReturns(LegResult{Response: testResponse(cdexPayersAnswer)})
	rec := httptest.NewRecorder()
	env.originator.ingressRoute(cdexRoute(payer))(rec, cdexPost(body, payer))
	env.substrate.mu.Lock()
	subjects := slices.Clone(env.substrate.authorizedSubjects)
	env.substrate.mu.Unlock()
	reasons, _ := l.unpaired()
	if rec.Code != http.StatusOK {
		if len(subjects) != 0 {
			t.Fatalf("a submission answered %d was authorized for %q", rec.Code, subjects)
		}
		return rec.Code, "", reasons
	}
	if len(subjects) != 1 {
		t.Fatalf("submission authorized for %q", subjects)
	}
	return rec.Code, subjects[0], reasons
}

func wantReasons(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("unpaired %q, want %q", got, want)
	}
}

// On the payer's gateway, a submission from the provider the request went to,
// naming its tracking identifier and member, binds to the request's patient,
// at every level: with no Patient of its own, and with a Patient in its
// attachment content whose demographics are not the request's (attachment
// content is never read for binding). A request carried again the same
// changes nothing. Every other submission is carried and bound by the member
// and its top-level resources, never its attachment content, with the reason
// observed: another tracking value (no-record), another member
// (member-differs), another provider (counterparty-differs), none
// (no-tracking-id), and a tracking identifier carried twice for different
// patients (member-conflict). A member the payer's system holds binds from it,
// and nothing is observed.
func TestCDexPairing_PayerSide(t *testing.T) {
	held, _, ok := newCensusSoR().ResolvePatient(dtrFrameMember)
	if !ok {
		t.Fatal("no held member")
	}
	type row struct {
		requests   func(t *testing.T, pp *payerPairing)
		submission func(pp *payerPairing) []byte
		want       string
		reasons    []string
	}
	ig := func(t *testing.T, pp *payerPairing) {
		if got := pp.request(t, cdexRequestTask(t), pp.provider()); got != pairPCI {
			t.Fatalf("request bound %s, want its subject Patient's %s", got, pairPCI)
		}
	}
	plain := func(pp *payerPairing) []byte {
		return pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false)
	}
	rows := map[string]row{
		"paired, no Patient": {ig, plain, pairPCI, nil},
		"paired, a Patient in its attachment content": {ig, func(*payerPairing) []byte {
			return pairSubmission(pairMember, pairTrackSystem, pairTrackValue, true)
		}, pairPCI, nil},
		"paired, the request carried twice the same": {func(t *testing.T, pp *payerPairing) { ig(t, pp); ig(t, pp) }, plain, pairPCI, nil},
		"another tracking value": {ig, func(*payerPairing) []byte {
			return pairSubmission(pairMember, pairTrackSystem, "other", true)
		}, pairAlone, []string{CDexUnpairedNoRecord}},
		"another tracking system": {ig, func(*payerPairing) []byte {
			return pairSubmission(pairMember, "http://example.org/other", pairTrackValue, false)
		}, pairAlone, []string{CDexUnpairedNoRecord}},
		"no request carried": {func(*testing.T, *payerPairing) {}, plain, pairAlone, []string{CDexUnpairedNoRecord}},
		// A Patient carried at the submission's top level, not in its
		// attachment content, is read, as every carried Patient is on a leg
		// with no subject Patient.
		"no request carried, a Patient at its top level named Resource": {func(*testing.T, *payerPairing) {}, func(*payerPairing) []byte {
			b := pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false)
			patient := `{"name":"Patient","Resource":{"resourceType":"Patient","id":"` + pairMember + `","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + pairMember + `"}],"name":[{"family":"Toplevel"}],"birthDate":"1945-03-03"}},`
			return []byte(strings.Replace(string(b), `{"name":"Attachment"`, patient+`{"name":"Attachment"`, 1))
		}, derivedPCI(pairMember, "1945-03-03", "Toplevel"), []string{CDexUnpairedNoRecord}},
		"no request carried, a Patient at its top level": {func(*testing.T, *payerPairing) {}, func(*payerPairing) []byte {
			var m map[string]any
			if err := json.Unmarshal(pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), &m); err != nil {
				panic(err)
			}
			m["parameter"] = append(m["parameter"].([]any), map[string]any{"name": "Patient", "resource": map[string]any{
				"resourceType": "Patient", "id": pairMember,
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": pairMember}},
				"name":       []any{map[string]any{"family": "Toplevel"}}, "birthDate": "1945-03-03"}})
			b, err := json.Marshal(m)
			if err != nil {
				panic(err)
			}
			return b
		}, derivedPCI(pairMember, "1945-03-03", "Toplevel"), []string{CDexUnpairedNoRecord}},
		"another member": {ig, func(*payerPairing) []byte {
			return pairSubmission("Member999", pairTrackSystem, pairTrackValue, false)
		}, derivedPCI("Member999", "", ""), []string{CDexUnpairedMemberDiffers}},
		"no TrackingId": {ig, func(*payerPairing) []byte { return pairSubmission(pairMember, "", "", false) }, pairAlone, []string{CDexUnpairedNoTrackingID}},
		"two TrackingIds that differ": {ig, func(*payerPairing) []byte {
			b := pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false)
			return []byte(strings.Replace(string(b), `{"name":"AttachTo"`, `{"name":"TrackingId","valueIdentifier":{"system":"`+pairTrackSystem+`","value":"other"}},{"name":"AttachTo"`, 1))
		}, pairAlone, []string{CDexUnpairedNoTrackingID}},
		"the request went to another provider": {func(t *testing.T, pp *payerPairing) {
			pp.request(t, cdexRequestTask(t), "cdex-provider-2")
		}, plain, pairAlone, []string{CDexUnpairedCounterpartyDiffers}},
		"the tracking identifier carried for two patients": {func(t *testing.T, pp *payerPairing) {
			ig(t, pp)
			if got := pp.request(t, pairTask(t, "Other", pairMember, pairTrackValue), pp.provider()); got == pairPCI {
				t.Fatal("the second request bound as the first")
			}
		}, plain, pairAlone, []string{CDexUnpairedMemberConflict}},
		"the tracking identifier carried for two members": {func(t *testing.T, pp *payerPairing) {
			ig(t, pp)
			pp.request(t, pairTask(t, "Shaw", "Member999", pairTrackValue), pp.provider())
		}, plain, pairAlone, []string{CDexUnpairedMemberConflict}},
		"a held member": {func(t *testing.T, pp *payerPairing) {
			if got := pp.request(t, pairTask(t, "Shaw", dtrFrameMember, pairTrackValue), pp.provider()); got != held {
				t.Fatalf("request bound %s, want the held member's %s", got, held)
			}
		}, func(*payerPairing) []byte {
			return pairSubmission(dtrFrameMember, pairTrackSystem, pairTrackValue, true)
		}, held, nil},
		"a held member, no request carried": {func(*testing.T, *payerPairing) {}, func(*payerPairing) []byte {
			return pairSubmission(dtrFrameMember, pairTrackSystem, "other", false)
		}, held, nil},
	}
	for name, r := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				pp := newPayerPairing(t, level)
				r.requests(t, pp)
				reasons, differs := pp.submit(t, r.submission(pp), r.want)
				if differs {
					t.Fatalf("the payer bound the submission to another patient than %s", r.want)
				}
				wantReasons(t, reasons, r.reasons...)
			})
		}
	}
}

// On the provider's gateway, a submission to the payer the request came from,
// naming its tracking identifier and member, binds to the request's patient,
// at every level, with no Patient of its own and with one in its attachment
// content; to another payer, or naming another tracking value or member, it is
// carried unpaired, observed with its reason. A held member binds from the
// provider's system. A request the provider's system answers with an error was
// still carried, and pairs.
func TestCDexPairing_ProviderSide(t *testing.T) {
	held, _, ok := newCensusSoR().ResolvePatient(dtrFrameMember)
	if !ok {
		t.Fatal("no held member")
	}
	for name, r := range map[string]struct {
		task       func(t *testing.T) []byte
		requestPCI string
		submission []byte
		toAnother  bool
		want       string
		reasons    []string
	}{
		"paired, no Patient":                          {cdexRequestTask, pairPCI, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), false, pairPCI, nil},
		"paired, a Patient in its attachment content": {cdexRequestTask, pairPCI, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, true), false, pairPCI, nil},
		"another tracking value":                      {cdexRequestTask, pairPCI, pairSubmission(pairMember, pairTrackSystem, "other", true), false, pairAlone, []string{CDexUnpairedNoRecord}},
		"another member": {cdexRequestTask, pairPCI, pairSubmission("Member999", pairTrackSystem, pairTrackValue, false), false,
			derivedPCI("Member999", "", ""), []string{CDexUnpairedMemberDiffers}},
		"to another payer": {cdexRequestTask, pairPCI, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), true, pairAlone, []string{CDexUnpairedCounterpartyDiffers}},
		"no TrackingId":    {cdexRequestTask, pairPCI, pairSubmission(pairMember, "", "", false), false, pairAlone, []string{CDexUnpairedNoTrackingID}},
		"a held member": {func(t *testing.T) []byte { return pairTask(t, "Shaw", dtrFrameMember, pairTrackValue) }, held,
			pairSubmission(dtrFrameMember, pairTrackSystem, pairTrackValue, true), false, held, nil},
		"a held member, another tracking value": {func(t *testing.T) []byte { return pairTask(t, "Shaw", dtrFrameMember, pairTrackValue) }, held,
			pairSubmission(dtrFrameMember, pairTrackSystem, "other", false), false, held, nil},
	} {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				pp := newProviderPairing(t, level, NewMemStore())
				if pp.request(t, r.task(t), r.requestPCI) {
					t.Fatalf("the provider bound the request to another patient than %s", r.requestPCI)
				}
				payer := pp.payer()
				if r.toAnother {
					payer = "payer-y"
				}
				got, reasons := pp.submit(t, r.submission, payer)
				if got != r.want {
					t.Fatalf("submission bound %s, want %s", got, r.want)
				}
				wantReasons(t, reasons, r.reasons...)
			})
		}
	}
	t.Run("the provider's system answered an error", func(t *testing.T) {
		l := &eventLog{}
		store := NewMemStore()
		sys := newProviderTaskSystem(t, http.StatusBadRequest, []byte(`{"resourceType":"OperationOutcome"}`))
		req := configuredProvider(t, EnforcementNone, sys, func(c *Config) { c.Store = store; c.Observer = l.observe })
		if ans := req.send(t, cdexRequestTask(t)); ans.status != http.StatusBadRequest {
			t.Fatalf("request answered %d, want the provider's error relayed", ans.status)
		}
		pp := &providerPairing{level: EnforcementNone, store: store, req: req, log: l}
		got, reasons := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer())
		if got != pairPCI || len(reasons) != 0 {
			t.Fatalf("submission bound %s (%q), want paired %s", got, reasons, pairPCI)
		}
	})
}

// A restart of the in-memory store loses its record: the submission is
// carried unpaired (no-record). A record past retention is gone, purged from
// the store, and a submission naming it is unpaired (no-record); a request
// carried again then records afresh.
func TestCDexPairing_RestartAndRetention(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		pp := newProviderPairing(t, EnforcementNone, NewMemStore())
		pp.request(t, cdexRequestTask(t), pairPCI)
		pp.store = NewMemStore()
		got, reasons := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer())
		if got != pairAlone {
			t.Fatalf("bound %s, want %s", got, pairAlone)
		}
		wantReasons(t, reasons, CDexUnpairedNoRecord)
	})
	t.Run("retention", func(t *testing.T) {
		store := NewMemStore()
		st, _ := memPAStateOf(store)
		now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
		st.cdexRequests.now = func() time.Time { return now }
		pp := newProviderPairing(t, EnforcementNone, store)
		pp.request(t, cdexRequestTask(t), pairPCI)
		key := CDexRequestKey{Counterparty: pp.payer(), System: pairTrackSystem, Value: pairTrackValue}
		now = now.Add(CDexRequestRetention - time.Hour)
		if got, _ := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer()); got != pairPCI {
			t.Fatalf("inside retention bound %s, want paired", got)
		}
		now = now.Add(2 * time.Hour)
		got, reasons := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer())
		if got != pairAlone {
			t.Fatalf("past retention bound %s, want %s", got, pairAlone)
		}
		wantReasons(t, reasons, CDexUnpairedNoRecord)
		st.cdexRequests.mu.Lock()
		_, kept := st.cdexRequests.rows[key]
		st.cdexRequests.mu.Unlock()
		if kept {
			t.Fatal("the record past retention is still in the store")
		}
		pp.request(t, cdexRequestTask(t), pairPCI)
		if got, _ := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer()); got != pairPCI {
			t.Fatalf("recorded afresh, bound %s, want paired", got)
		}
	})
}

// failingCDexStore is an in-memory Store whose record of attachment requests
// cannot be written or read.
type failingCDexStore struct{ *MemStore }

func (failingCDexStore) RecordCDexRequest(CDexRequestKey, CDexRequestRecord) error {
	return errors.New("store down")
}
func (failingCDexStore) LookupCDexRequest(CDexRequestKey) (CDexRequestRecord, CDexRequestLookup, error) {
	return CDexRequestRecord{}, CDexRequestNone, errors.New("store down")
}

// A record that cannot be written never fails the request it records, and a
// lookup that fails is a submission carried unpaired (store-error), never
// refused or answered 503, at every level; both are counted as store errors.
func TestCDexPairing_AStoreErrorNeverRefuses(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			store := failingCDexStore{NewMemStore()}
			var counted []string
			var mu sync.Mutex
			count := func(s string) { mu.Lock(); counted = append(counted, s); mu.Unlock() }
			pp := newProviderPairing(t, level, store)
			pp.req.g.cfg.StoreErrorMetric = count
			pp.metric = count
			if pp.request(t, cdexRequestTask(t), pairPCI) {
				t.Fatal("the request bound to another patient")
			}
			got, reasons := pp.submit(t, pairSubmission(pairMember, pairTrackSystem, pairTrackValue, false), pp.payer())
			if got != pairAlone {
				t.Fatalf("bound %s, want %s", got, pairAlone)
			}
			wantReasons(t, reasons, CDexUnpairedStoreError)
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(counted, []string{storeErrCDexRequest, storeErrCDexRequest}) {
				t.Fatalf("counted %q, want the failed write and the failed lookup", counted)
			}
		})
	}
}

// The record: a key recorded again the same changes nothing; for another
// patient or member it is conflicted, never overwritten, and stays so; keys
// are scoped to the other participant; a key with no participant or value, or
// one over MaxCDexTrackingBytes, is refused. Retention counts from the last
// change, and a record the same again does not extend it.
func TestMemCDexRequests(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	m := NewMemCDexRequests()
	m.now = func() time.Time { return now }
	k := CDexRequestKey{Counterparty: "p1", System: "s", Value: "v"}
	a := CDexRequestRecord{Member: "M1", PCI: "pci:a"}
	lookup := func(k CDexRequestKey) (CDexRequestRecord, CDexRequestLookup) {
		t.Helper()
		rec, l, err := m.LookupCDexRequest(k)
		if err != nil {
			t.Fatal(err)
		}
		return rec, l
	}
	if _, l := lookup(k); l != CDexRequestNone {
		t.Fatalf("empty store: %v", l)
	}
	if err := m.RecordCDexRequest(k, a); err != nil {
		t.Fatal(err)
	}
	if rec, l := lookup(k); l != CDexRequestFound || rec != a {
		t.Fatalf("found %v %+v", l, rec)
	}
	if _, l := lookup(CDexRequestKey{Counterparty: "p2", System: "s", Value: "v"}); l != CDexRequestOtherCounterparty {
		t.Fatalf("another participant: %v", l)
	}
	if _, l := lookup(CDexRequestKey{Counterparty: "p1", System: "", Value: "v"}); l != CDexRequestNone {
		t.Fatalf("another system: %v", l)
	}
	// The same again, later, changes nothing: retention still counts from
	// the first record.
	now = now.Add(100 * 24 * time.Hour)
	if err := m.RecordCDexRequest(k, a); err != nil {
		t.Fatal(err)
	}
	now = now.Add(CDexRequestRetention - 100*24*time.Hour + time.Second)
	if _, l := lookup(k); l != CDexRequestNone {
		t.Fatalf("past retention from the first record: %v", l)
	}
	for name, second := range map[string]CDexRequestRecord{
		"another patient": {Member: "M1", PCI: "pci:b"},
		"another member":  {Member: "M2", PCI: "pci:a"},
	} {
		k := CDexRequestKey{Counterparty: "p1", System: "s", Value: name}
		if err := m.RecordCDexRequest(k, a); err != nil {
			t.Fatal(err)
		}
		if err := m.RecordCDexRequest(k, second); err != nil {
			t.Fatal(err)
		}
		if _, l := lookup(k); l != CDexRequestConflicted {
			t.Fatalf("%s: %v, want conflicted", name, l)
		}
		// Neither record nor the first again clears it.
		_ = m.RecordCDexRequest(k, a)
		_ = m.RecordCDexRequest(k, second)
		if _, l := lookup(k); l != CDexRequestConflicted {
			t.Fatalf("%s, recorded again: %v, want conflicted", name, l)
		}
	}
	// A record past retention that the throttled sweep has not yet removed
	// is gone all the same: never found, and recorded afresh, never
	// conflicted.
	expiring := CDexRequestKey{Counterparty: "p1", System: "s", Value: "expiring"}
	if err := m.RecordCDexRequest(expiring, a); err != nil {
		t.Fatal(err)
	}
	now = now.Add(CDexRequestRetention - 30*time.Second)
	if _, l := lookup(expiring); l != CDexRequestFound {
		t.Fatalf("inside retention: %v", l)
	}
	now = now.Add(40 * time.Second) // past retention, inside PendPurgeInterval of the last sweep
	if _, l := lookup(expiring); l != CDexRequestNone {
		t.Fatalf("past retention, before the sweep: %v, want none", l)
	}
	b := CDexRequestRecord{Member: "M2", PCI: "pci:b"}
	if err := m.RecordCDexRequest(expiring, b); err != nil {
		t.Fatal(err)
	}
	if rec, l := lookup(expiring); l != CDexRequestFound || rec != b {
		t.Fatalf("recorded afresh past retention: %v %+v, want the new record", l, rec)
	}
	for name, bad := range map[string]CDexRequestKey{
		"no participant": {System: "s", Value: "v"},
		"no value":       {Counterparty: "p1", System: "s"},
		"too long":       {Counterparty: "p1", System: strings.Repeat("s", 256), Value: strings.Repeat("v", 257)},
	} {
		if err := m.RecordCDexRequest(bad, a); !errors.Is(err, ErrCDexRequestKey) {
			t.Fatalf("%s: record %v", name, err)
		}
		if _, _, err := m.LookupCDexRequest(bad); !errors.Is(err, ErrCDexRequestKey) {
			t.Fatalf("%s: lookup %v", name, err)
		}
	}
	if !ValidCDexRequestKey(CDexRequestKey{Counterparty: "p1", System: strings.Repeat("s", 256), Value: strings.Repeat("v", 256)}) {
		t.Fatal("a key of exactly MaxCDexTrackingBytes is refused")
	}
}

// A Store that embeds an in-memory Store and declares only part of
// CDexRequestStore is refused at construction.
type partialCDexStore struct{ *MemStore }

func (partialCDexStore) RecordCDexRequest(CDexRequestKey, CDexRequestRecord) error { return nil }

func TestCDexPairing_APartialStoreIsRefused(t *testing.T) {
	if m := partialCDexRequestStore(partialCDexStore{NewMemStore()}); m != "RecordCDexRequest" {
		t.Fatalf("partial %q", m)
	}
	if err := (paWorkstream{}).checkStore(partialCDexStore{NewMemStore()}); err == nil || !strings.Contains(err.Error(), "CDexRequestStore") {
		t.Fatalf("checkStore %v", err)
	}
	if _, ok := CDexRequestsOf(partialCDexStore{NewMemStore()}); ok {
		t.Fatal("a partial store answered with the embedded record")
	}
	if _, ok := CDexRequestsOf(NewMemStore()); !ok {
		t.Fatal("the in-memory store has no record")
	}
}

// subjectTask is the IG example Task edited for the request-side rows: its
// subject Patient with or without its demographics, and optionally another
// Patient for the member (under shnsdk.MemberSystem) with other demographics,
// a Coverage whose subscriber is another person under the member's id (its
// beneficiary that other Patient, which is then the member's own), or Task.for
// naming a Patient the Task does not carry.
func subjectTask(t *testing.T, subjectDemographics, otherPatient, party, notCarried bool) []byte {
	t.Helper()
	mb := map[string]any{"coding": []any{map[string]any{"system": v20203MB.System, "code": "MB"}}}
	return cdexRequestMutate(t, func(m map[string]any) {
		contained := m["contained"].([]any)
		for _, c := range contained {
			p := c.(map[string]any)
			if p["resourceType"] == "Patient" && !subjectDemographics {
				delete(p, "name")
				delete(p, "birthDate")
			}
		}
		if otherPatient {
			contained = append(contained, map[string]any{"resourceType": "Patient", "id": "other",
				"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": pairMember}},
				"name":       []any{map[string]any{"family": "Elsewhere"}}, "birthDate": "1950-05-05"})
		}
		if party {
			contained = append(contained,
				map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
					"subscriber": map[string]any{"reference": "#" + pairMember}, "beneficiary": map[string]any{"reference": "#other"}},
				map[string]any{"resourceType": "Patient", "id": pairMember,
					"birthDate": "1960-01-01", "name": []any{map[string]any{"family": "Subscriber"}}})
		}
		m["contained"] = contained
		if notCarried {
			m["for"] = map[string]any{"reference": "Patient/elsewhere",
				"identifier": map[string]any{"type": mb, "system": "http://example.org/cdex/payer/member-ids", "value": pairMember}}
		}
	})
}

// The attachment request binds a member neither side's system holds from its
// own subject Patient, the contained Patient Task.for names, on the payer's
// ingress and the provider's inbound handler alike, at every level: from its
// demographics beside another Patient for the member whose demographics
// differ; by the number alone when it has none, beside another that has them;
// refused 422 on authority, with its own text, when it has none and the Task
// also carries another person under the member's number beside a Patient for
// the member that has them (with no such Patient, the existing refusal and its
// text); and, when Task.for names no Patient the Task carries, by the rule for
// a leg with no subject Patient, as before.
func TestCDexRequest_TheSubjectPatientRowsOnBothGateways(t *testing.T) {
	for name, r := range map[string]struct {
		body   func(t *testing.T) []byte
		want   string
		refuse string
	}{
		"a subject Patient with demographics beside another that differs": {func(t *testing.T) []byte { return subjectTask(t, true, true, false, false) }, pairPCI, ""},
		"a subject Patient without demographics beside another with them": {func(t *testing.T) []byte { return subjectTask(t, false, true, false, false) }, pairAlone, ""},
		"a subject Patient without demographics, alone":                   {func(t *testing.T) []byte { return subjectTask(t, false, false, false, false) }, pairAlone, ""},
		"that, beside another person under the member's number":           {func(t *testing.T) []byte { return subjectTask(t, false, true, true, false) }, "", msgSubjectUnidentifiable},
		"another person under the number, no Patient with demographics":   {func(t *testing.T) []byte { return subjectTask(t, false, false, true, false) }, "", msgPatientUnidentifiable},
		// Refused 422 "carries no Patient …" in earlier releases, which read
		// no Patient of the member's own in the IG's example.
		"a subject Patient with demographics beside another person under the number": {func(t *testing.T) []byte { return subjectTask(t, true, false, true, false) }, pairPCI, ""},
		"Task.for names no Patient the Task carries":                                 {func(t *testing.T) []byte { return subjectTask(t, true, true, false, true) }, derivedPCI(pairMember, "1950-05-05", "Elsewhere"), ""},
	} {
		for _, level := range allLevels {
			t.Run(name+"/payer/"+level.String(), func(t *testing.T) {
				env, rec, x := runCDexRequestIngress(t, level, r.body(t),
					LegResult{Response: relayedResponse(cdexRequestAnswer), AnswerStatus: http.StatusCreated})
				if r.refuse != "" {
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), r.refuse) {
						t.Fatalf("answer %d %s, want 422 %q", rec.Code, rec.Body, r.refuse)
					}
					wantRefusal(t, x, http.StatusUnprocessableEntity, RefusedByPayerGateway, RefusalAuthority)
					return
				}
				env.substrate.mu.Lock()
				subjects := slices.Clone(env.substrate.authorizedSubjects)
				env.substrate.mu.Unlock()
				if rec.Code != http.StatusCreated || len(subjects) != 1 || subjects[0] != r.want {
					t.Fatalf("answer %d, authorized for %q, want %s", rec.Code, subjects, r.want)
				}
			})
			t.Run(name+"/provider/"+level.String(), func(t *testing.T) {
				l := &eventLog{}
				sys := newProviderTaskSystem(t, http.StatusCreated, []byte(`{"resourceType":"Task"}`))
				p := configuredProvider(t, level, sys, func(c *Config) { c.Observer = l.observe })
				if r.refuse != "" {
					ans := p.send(t, r.body(t))
					if ans.status != http.StatusUnprocessableEntity || !strings.Contains(string(ans.body), r.refuse) || sys.calls() != 0 {
						t.Fatalf("answer %d %s (%d calls), want 422 %q before the provider's system", ans.status, ans.body, sys.calls(), r.refuse)
					}
					wantRefusal(t, p.got.only(t), http.StatusUnprocessableEntity, RefusedByProviderGateway, RefusalAuthority)
					return
				}
				ans := p.sendWith(t, r.body(t), func(_ *shnsdk.Metadata, tok *shnsdk.Token) { tok.Subject = r.want })
				if _, differs := l.unpaired(); ans.status != http.StatusCreated || differs {
					t.Fatalf("answer %d, bound to another patient than %s (%v)", ans.status, r.want, differs)
				}
			})
		}
	}
}

// cdexPartyCoverage is a Coverage whose subscriber is another person under the
// member's id (a Patient it contains) and whose beneficiary is beneficiary.
func cdexPartyCoverage(beneficiary string) map[string]any {
	return map[string]any{"resourceType": "Coverage", "id": "cov", "status": "active",
		"subscriber": map[string]any{"reference": "#" + pairMember}, "beneficiary": map[string]any{"reference": beneficiary},
		"contained": []any{map[string]any{"resourceType": "Patient", "id": pairMember,
			"birthDate": "1960-01-01", "name": []any{map[string]any{"family": "Subscriber"}}}}}
}

// contentSubmission is a submission naming tracking whose parameters are
// top (top-level resources) and whose attachment content is a Bundle of
// content.
func contentSubmission(tracking string, top []map[string]any, content ...map[string]any) []byte {
	params := []any{
		map[string]any{"name": "TrackingId", "valueIdentifier": map[string]any{"system": pairTrackSystem, "value": tracking}},
		map[string]any{"name": "MemberId", "valueIdentifier": map[string]any{"system": "http://example.org/member", "value": pairMember}},
	}
	for _, r := range top {
		params = append(params, map[string]any{"name": "Resource", "resource": r})
	}
	var entries []any
	for _, r := range content {
		entries = append(entries, map[string]any{"resource": r})
	}
	params = append(params, map[string]any{"name": "Attachment", "part": []any{map[string]any{"name": "Content",
		"resource": map[string]any{"resourceType": "Bundle", "type": "collection", "entry": entries}}}})
	b, err := json.Marshal(map[string]any{"resourceType": "Parameters", "parameter": params})
	if err != nil {
		panic(err)
	}
	return b
}

// A submission's attachment content is never read for binding, on either
// gateway, at every level. A Coverage in it naming another person under the
// member's number, with no Patient of the member's own, is carried, bound by
// the member number alone (earlier releases refused it 400, reading the
// party). A Coverage at its top level naming such a person, whose member's
// Patient is only in its attachment content, identifies no one patient: it is
// refused 400 on authority, with the existing text (earlier releases carried
// it, bound from that Patient), unless it answers a request this gateway
// carried for the member: a record that matches decides before any of the
// submission is read, so the same submission paired is carried, bound under
// the request's patient.
func TestCDexPairing_ContentIsNeverReadForBinding(t *testing.T) {
	member := map[string]any{"resourceType": "Patient", "id": "p1",
		"identifier": []any{map[string]any{"system": shnsdk.MemberSystem, "value": pairMember}},
		"name":       []any{map[string]any{"family": "Contentish"}}, "birthDate": "1940-04-04"}
	partyInContent := contentSubmission("nope", nil, cdexPartyCoverage("#other"))
	partyOnTop := func(tracking string) []byte {
		return contentSubmission(tracking, []map[string]any{cdexPartyCoverage("Patient/p1")}, member)
	}
	// Refused for what it carries (no Patient for the member anywhere)
	// unless it pairs: a matching record decides first.
	partyNoMember := func(tracking string) []byte {
		return contentSubmission(tracking, []map[string]any{cdexPartyCoverage("#other")})
	}
	for _, level := range allLevels {
		t.Run("a party in the content/payer/"+level.String(), func(t *testing.T) {
			pp := newPayerPairing(t, level)
			reasons, differs := pp.submit(t, partyInContent, pairAlone)
			if differs {
				t.Fatalf("bound to another patient than %s", pairAlone)
			}
			wantReasons(t, reasons, CDexUnpairedNoRecord)
		})
		t.Run("a party in the content/provider/"+level.String(), func(t *testing.T) {
			pp := newProviderPairing(t, level, NewMemStore())
			got, reasons := pp.submit(t, partyInContent, pp.payer())
			if got != pairAlone {
				t.Fatalf("bound %s, want %s", got, pairAlone)
			}
			wantReasons(t, reasons, CDexUnpairedNoRecord)
		})
		t.Run("a party on top, unpaired/payer/"+level.String(), func(t *testing.T) {
			pp := newPayerPairing(t, level)
			ans := pp.p.sendAs(t, legCDexSubmit, "", partyOnTop("nope"), pairAlone)
			if ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), msgPatientUnidentifiable) || pp.p.partner.lastPath != "" {
				t.Fatalf("answer %d %s (payer called at %q), want 400 %q before the payer's system", ans.status, ans.body, pp.p.partner.lastPath, msgPatientUnidentifiable)
			}
			if reasons, _ := pp.log.unpaired(); len(reasons) != 0 {
				t.Fatalf("a refused submission observed unpaired %q", reasons)
			}
		})
		t.Run("a party on top, unpaired/provider/"+level.String(), func(t *testing.T) {
			pp := newProviderPairing(t, level, NewMemStore())
			status, _, reasons := pp.submitRaw(t, partyOnTop("nope"), pp.payer())
			if status != http.StatusBadRequest || len(reasons) != 0 {
				t.Fatalf("answer %d (unpaired %q), want 400 and no observation", status, reasons)
			}
		})
		t.Run("a party on top, paired/payer/"+level.String(), func(t *testing.T) {
			pp := newPayerPairing(t, level)
			pp.request(t, cdexRequestTask(t), pp.provider())
			reasons, differs := pp.submit(t, partyOnTop(pairTrackValue), pairPCI)
			if differs || len(reasons) != 0 {
				t.Fatalf("paired submission bound elsewhere (%v) or unpaired %q, want %s", differs, reasons, pairPCI)
			}
		})
		t.Run("a party on top, no Patient for the member, unpaired/payer/"+level.String(), func(t *testing.T) {
			pp := newPayerPairing(t, level)
			if ans := pp.p.sendAs(t, legCDexSubmit, "", partyNoMember("nope"), pairAlone); ans.status != http.StatusBadRequest || !strings.Contains(string(ans.body), msgPatientUnidentifiable) {
				t.Fatalf("answer %d %s, want 400 %q", ans.status, ans.body, msgPatientUnidentifiable)
			}
		})
		t.Run("a party on top, no Patient for the member, paired/payer/"+level.String(), func(t *testing.T) {
			pp := newPayerPairing(t, level)
			pp.request(t, cdexRequestTask(t), pp.provider())
			reasons, differs := pp.submit(t, partyNoMember(pairTrackValue), pairPCI)
			if differs || len(reasons) != 0 {
				t.Fatalf("paired submission bound elsewhere (%v) or unpaired %q, want %s", differs, reasons, pairPCI)
			}
		})
		t.Run("a party on top, no Patient for the member, unpaired/provider/"+level.String(), func(t *testing.T) {
			pp := newProviderPairing(t, level, NewMemStore())
			if status, _, _ := pp.submitRaw(t, partyNoMember("nope"), pp.payer()); status != http.StatusBadRequest {
				t.Fatalf("answer %d, want 400", status)
			}
		})
		t.Run("a party on top, no Patient for the member, paired/provider/"+level.String(), func(t *testing.T) {
			pp := newProviderPairing(t, level, NewMemStore())
			pp.request(t, cdexRequestTask(t), pairPCI)
			got, reasons := pp.submit(t, partyNoMember(pairTrackValue), pp.payer())
			if got != pairPCI || len(reasons) != 0 {
				t.Fatalf("paired submission bound %s (unpaired %q), want %s", got, reasons, pairPCI)
			}
		})
		t.Run("a party on top, paired/provider/"+level.String(), func(t *testing.T) {
			pp := newProviderPairing(t, level, NewMemStore())
			pp.request(t, cdexRequestTask(t), pairPCI)
			got, reasons := pp.submit(t, partyOnTop(pairTrackValue), pp.payer())
			if got != pairPCI || len(reasons) != 0 {
				t.Fatalf("paired submission bound %s (unpaired %q), want %s", got, reasons, pairPCI)
			}
		})
	}
}
