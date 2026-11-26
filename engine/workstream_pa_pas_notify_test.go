//go:build !shn_coreonly

package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/pasgraph"
	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
	"github.com/SmartHealthNetwork/shn-gateway/pa/pastopic"
	"github.com/SmartHealthNetwork/shn-gateway/pa/subscription"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The PAS topic's content leg at the payer's gateway: payer-x's system posts
// a full-resource notification of provider-x's Subscription, its decision a
// PAS Response Bundle, to the notification address its gateway wrote. The
// rows hold the content leg's own rules (the receiver, the patient, and the
// conformance-gated mixed patients and defects elsewhere); the route's shared
// rules are the signal leg's rows (TestPASNotify_ThePayersGatewaysChecks).

// decisionBase is the payer's base its decision's entries are under.
const decisionBase = "https://payer.example/fhir"

// decisionCR is a ClaimResponse entry naming patientRef and, as its
// applicationReceiverCode, each of codes.
func decisionCR(id, patientRef string, codes ...string) string {
	ext := ""
	if len(codes) > 0 {
		var subs []string
		for _, c := range codes {
			subs = append(subs, `{"url":"`+pastopic.ReceiverCode+`","valueString":"`+c+`"}`)
		}
		ext = `"extension":[{"url":"` + pastopic.TransmissionIdentifiersURL + `","extension":[` + strings.Join(subs, ",") + `]}],`
	}
	return `{"fullUrl":"` + decisionBase + `/ClaimResponse/` + id + `","resource":{"resourceType":"ClaimResponse","id":"` + id + `",` + ext +
		`"patient":{"reference":"` + patientRef + `"},"outcome":"complete"}}`
}

// unreadCoverage is a decision naming Patient/p1, its clean Patient p1, and a
// Coverage entry the network cannot read (its fullUrl does not end in its
// id), parties its members.
func unreadCoverage(parties string) []byte {
	return contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
		`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c2",`+parties+`}}`))
}

// decisionPatient is a Patient entry, id at the payer's base.
func decisionPatient(id string) string {
	return `{"fullUrl":"` + decisionBase + `/Patient/` + id + `","resource":{"resourceType":"Patient","id":"` + id + `","birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
}

// decision is a PAS Response Bundle of entries, of type typ.
func decision(typ string, entries ...string) string {
	return `{"resourceType":"Bundle","type":"` + typ + `","entry":[` + strings.Join(entries, ",") + `]}`
}

// contentEvent is a full-resource notification of the PAS topic carrying
// resource as its decision, its second entry in the shape a captured br-payer
// notification has: request.method
// PUT beside the resource, no request.url and no fullUrl.
func contentEvent(resource string) []byte {
	return []byte(`{"resourceType":"Bundle","type":"history","entry":[{"fullUrl":"urn:uuid:0b6a1f8e-2c3d-4e5f-8a9b-0c1d2e3f4a5b","resource":{"resourceType":"Parameters","parameter":[` +
		`{"name":"subscription","valueReference":{"reference":"Subscription/s1"}},{"name":"topic","valueCanonical":"` + pastopic.Canonical + `"},` +
		`{"name":"type","valueCode":"event-notification"},{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus"}]}]}},` +
		`{"request":{"method":"PUT"},"resource":` + resource + `}]}`)
}

// withRequest is a ClaimResponse entry (decisionCR) that names its request
// Claim by identifier, as a PAS 2.1 or 2.2 answer must.
func withRequest(cr string) string {
	return strings.Replace(cr, `"outcome":"complete"`, `"request":{"identifier":{"value":"claim-1"}},"outcome":"complete"`, 1)
}

// bareCR is a ClaimResponse resource (no entry) naming patientRef and, as
// its applicationReceiverCode, code.
func bareCR(id, patientRef, code string) string {
	return strings.TrimSuffix(strings.SplitN(decisionCR(id, patientRef, code), `"resource":`, 2)[1], "}")
}

// contentEvents is contentEvent with a later entry for each of entries, each
// written whole (an entry object), after the SubscriptionStatus.
func contentEvents(entries ...string) []byte {
	head := string(contentEvent(`{}`))
	head = head[:strings.Index(head, `,{"request":{"method":"PUT"},"resource":{}}`)]
	return []byte(head + "," + strings.Join(entries, ",") + `]}`)
}

// decisionEntry is a later entry of a notification carrying resource, as a
// payer's FHIR server writes it.
func decisionEntry(resource string) string {
	return `{"request":{"method":"PUT"},"resource":` + resource + `}`
}

// withStatusParameter is a notification whose SubscriptionStatus carries
// param as one more parameter.
func withStatusParameter(body []byte, param string) []byte {
	return []byte(strings.Replace(string(body), `{"name":"type","valueCode":"event-notification"}`, `{"name":"type","valueCode":"event-notification"},`+param, 1))
}

// The payer's gateway's checks of its system's PAS decision notification,
// each at every conformance level: valid notification − one mutation →
// refused on its rule, recorded as the payer gateway's refusal on the notify
// leg, nothing sent; a conformance-gated defect is carried at the levels that
// do not refuse its rule. The controls (one patient by a relative and by an
// absolute reference, one no entry resolves) pass every check and go to the
// network on the notify leg.
func TestPASNotify_TheDecisionNotificationsChecks(t *testing.T) {
	sys := newSubStore(t)
	one := func(ref string) []byte {
		return contentEvent(decision("collection", decisionCR("cr1", ref, pasProviderNPI), decisionPatient("p1")))
	}
	many := make([]string, 0, 260)
	many = append(many, decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))
	for i := 0; len(many) <= 257; i++ {
		many = append(many, `{"fullUrl":"`+decisionBase+`/Basic/b`+strconv.Itoa(i)+`","resource":{"resourceType":"Basic","id":"b`+strconv.Itoa(i)+`"}}`)
	}
	type row struct {
		body []byte
		// refusedAt are the levels that refuse it (every level when nil
		// and status is set), with its status, rule and reason.
		refusedAt map[ConformanceEnforcement]bool
		status    int
		rule      string
		reason    string
	}
	strictOnly := map[ConformanceEnforcement]bool{EnforcementStrict: true}
	structuralUp := map[ConformanceEnforcement]bool{EnforcementStructural: true, EnforcementStrict: true}
	for name, r := range map[string]row{
		"control: a relative reference":           {body: one("Patient/p1")},
		"control: an absolute reference":          {body: one(decisionBase + "/Patient/p1")},
		"control: a reference no entry resolves":  {body: contentEvent(decision("collection", withRequest(decisionCR("cr1", "Patient/p9", pasProviderNPI))))},
		"control: a bare ClaimResponse":           {body: contentEvent(strings.TrimSuffix(strings.SplitN(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"resource":`, 2)[1], "}"))},
		"another provider's receiver code":        {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", "1111111111"), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		"no receiver code":                        {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1"), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		"a ClaimResponse naming no patient":       {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"patient":{"reference":"Patient/p1"},`, "", 1))), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"the bound Patient's entry defective":     {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), strings.Replace(decisionPatient("p1"), `"id":"p1"`, `"id":"p2"`, 1))), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"over the limit the gateway binds within": {body: contentEvent(decision("collection", many...)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyOverLimit},
		// An unreadable entry bears on the binding exactly when the
		// binding's own reader, given the decision as sent, binds otherwise:
		// a Coverage that makes the member a party there leaves whether its
		// number is shared unknown, so it is refused at every level.
		"an unreadable Coverage naming the patient as its subscriber":   {body: unreadCoverage(`"subscriber":{"reference":"Patient/p1"}`), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"an unreadable Coverage whose subscriber is another, undecided": {body: unreadCoverage(`"beneficiary":{"reference":"Patient/p3"},"subscriber":{"reference":"Patient/p2"}`), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"an unreadable Coverage spelling its slot Subscriber":           {body: unreadCoverage(`"Subscriber":{"reference":"Patient/p1"}`), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"an unreadable Coverage naming the patient in an extension":     {body: unreadCoverage(`"subscriber":{"extension":[{"url":"http://example.org/x","valueReference":{"reference":"Patient/p1"}}]}`), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"a Coverage contained in an unreadable entry": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Basic/b1","resource":{"resourceType":"Basic","id":"b2","contained":[{"resourceType":"Coverage","id":"c1","subscriber":{"reference":"Patient/p1"}}]}}`)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		// One that binds the same either way is a defect elsewhere.
		"an unreadable Coverage naming the patient only as its beneficiary": {body: unreadCoverage(`"beneficiary":{"reference":"Patient/p1"}`), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		"an unreadable Coverage naming an Organization payor":               {body: unreadCoverage(`"beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Organization/o1"}]`), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		// A PAS answer's graph carries one ClaimResponse; two in one
		// decision are outside it.
		"two ClaimResponses in one decision": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionCR("cr2", "Patient/p2", pasProviderNPI), decisionPatient("p1"), decisionPatient("p2"))), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		// A reference that resolves to no entry, at the line that requires
		// every one to, and with no request Claim named for the lines that
		// let one go unresolved.
		"a dangling insurer reference":                    {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"outcome"`, `"insurer":{"reference":"Organization/o9"},"outcome"`, 1), decisionPatient("p1"))), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		"a reference no entry resolves, no request named": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p9", pasProviderNPI))), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		// Two decisions, one patient each: two patients.
		"two decisions, two patients": {body: contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			decisionEntry(decision("collection", decisionCR("cr2", "Patient/p2", pasProviderNPI), decisionPatient("p2")))), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// A decision naming another patient beside its ClaimResponse's, by
		// the subject rule a PAS answer is held to and the involved list's own
		// reading (pasNotifiedIdentities).
		"a Task for another patient": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), decisionPatient("p2"),
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`)), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// The same member id at another base, another Patient with other
		// demographics: two patients, as the PAS answer's identity rule reads
		// them.
		"the member's id at another base": {body: contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			decisionEntry(decision("collection", decisionCR("cr2", "https://other.example/fhir/Patient/p1", pasProviderNPI), otherBasePatient))), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// A reference only the whole-request reading finds (an untyped
		// extension reference), and a subject only the PAS answer's subject
		// rule tells apart (the member's id at another base).
		"an extension naming another patient": {body: contentEvent(decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Basic/b1","resource":{"resourceType":"Basic","id":"b1","extension":[{"url":"http://example.org/x","valueReference":{"reference":"Patient/p2"}}]}}`)), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		"a Task for another patient beside the decision": {body: withStatusParameter(one("Patient/p1"), `{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		"a Task for the member's id at another base": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), otherBasePatient,
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","for":{"reference":"https://other.example/fhir/Patient/p1"}}}`)), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// A ClaimResponse beside the decision is bound as the decision's is,
		// its names in any case: one naming its patient by a Reference
		// member names p2; one naming none cannot be bound, at every level.
		"a ClaimResponse beside the decision, its Reference p2": {body: withStatusParameter(one("Patient/p1"), `{"name":"x","resource":`+strings.Replace(bareCR("cr2", "Patient/p2", pasProviderNPI), `"reference"`, `"Reference"`, 1)+`}`), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		"a ClaimResponse beside the decision naming no patient": {body: withStatusParameter(one("Patient/p1"), `{"name":"x","resource":`+strings.Replace(bareCR("cr2", "Patient/p2", pasProviderNPI), `"patient":{"reference":"Patient/p2"},`, "", 1)+`}`), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		// A notification whose resources cannot be read: unknown is not
		// empty, at every level.
		"a resourceType that is no type's name": {body: withStatusParameter(one("Patient/p1"), `{"name":"x","resource":{"resourceType":{"name":"ClaimResponse"}}}`), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		// A receiver a lenient reader takes: a second receiver sub-extension
		// spelling its url URL, naming another provider; and one naming its
		// receiver by no string.
		"a second receiver spelled URL, another provider's": {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"valueString":"`+pasProviderNPI+`"}`, `"valueString":"`+pasProviderNPI+`"},{"URL":"`+pastopic.ReceiverCode+`","valueString":"1111111111"}`, 1), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		// A receiver sub-extension carrying a value[x] beside its
		// valueString, or a valueCode alone: a receiver the check cannot
		// read, refused as an absent one.
		"a receiver's valueString beside a valueCode, another's": {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"valueString":"`+pasProviderNPI+`"}`, `"valueString":"`+pasProviderNPI+`","valueCode":"1111111111"}`, 1), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		"a receiver's valueCode alone":                           {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"valueString":"`+pasProviderNPI+`"}`, `"valueCode":"`+pasProviderNPI+`"}`, 1), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		// An unreadable ClaimResponse beside a clean one, its type or its
		// entry's resource member in another case: a patient the network
		// cannot bind, at every level.
		"an unreadable claimResponse beside a clean one": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/ClaimResponse/cr2","resource":`+strings.Replace(bareCR("cr2", "Patient/p2", pasProviderNPI), `"ClaimResponse"`, `"claimResponse"`, 1)+`}`)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"an unreadable ClaimResponse under Resource beside a clean one": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/ClaimResponse/cr2","Resource":`+bareCR("cr2", "Patient/p2", pasProviderNPI)+`}`)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		// A decision written with urn:uuid fullUrls and references, as a
		// PAS Bundle may be: its patient resolves to the Patient entry whose
		// fullUrl the reference is, by FHIR R4's Bundle rule, and binds; a
		// urn:uuid reference that resolves to no entry does not.
		"control: a decision written with urn:uuid": {body: contentEvent(urnDecision(urnPatient))},
		"a urn:uuid patient no entry resolves":      {body: contentEvent(urnDecision("urn:uuid:a0000000-0000-4000-8000-00000000ffff")), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		// A subject only the PAS answer's subject rule tells apart: a Task
		// for the member's id at another base the decision does not carry.
		"a Task for the member's id at another base, not carried": {body: contentEvent(decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","for":{"reference":"https://other.example/fhir/Patient/p1"}}}`)), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// A Coverage's party, a Patient entry of the decision, is no member:
		// read in its decision, not from the notification as a whole.
		"control: a Coverage's party carried in the decision": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), decisionPatient("p0"),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/p0"}}}`))},
		// An unreadable ClaimResponse beside a clean one, its resourceType
		// member named in another case: unbindable, at every level.
		"an unreadable ClaimResponse by ResourceType beside a clean one": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/ClaimResponse/cr2","resource":`+strings.Replace(bareCR("cr2", "Patient/p2", pasProviderNPI), `"resourceType"`, `"ResourceType"`, 1)+`}`)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		// Each reference keys against its own entry's base: a second
		// decision whose ClaimResponse sits at another server's base names
		// that server's Patient/p1, another patient; at the payer's base, the
		// same member in two decisions is one.
		"two decisions, the second's ClaimResponse at another base": {body: otherBaseDecisions, refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		// The same, its Patient's demographics the first's: two servers'
		// Patient/p1 are two patients by identity alone.
		"two decisions, the second at another base, one demographics": {body: contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			decisionEntry(decision("collection", strings.Replace(decisionCR("cr2", "Patient/p1", pasProviderNPI), decisionBase+"/ClaimResponse/cr2", "https://other.example/fhir/ClaimResponse/cr2", 1), strings.Replace(decisionPatient("p1"), decisionBase, "https://other.example/fhir", 1)))), refusedAt: strictOnly, status: http.StatusUnprocessableEntity, reason: errPASNotifyMixed},
		"control: one member in two decisions at one base": {body: contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			decisionEntry(decision("collection", decisionCR("cr2", "Patient/p1", pasProviderNPI), decisionPatient("p1"))))},
		// A later entry whose resource is neither a Bundle nor a
		// ClaimResponse, beside the decision: a patient the network cannot
		// identify, at every level.
		"an entry that is neither a Bundle nor a ClaimResponse": {body: contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			decisionEntry(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`)), status: http.StatusUnprocessableEntity, rule: RefusalAuthority, reason: errPASNotifyNoPatient},
		"a receiver named by no string": {body: contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"valueString":"`+pasProviderNPI+`"}`, `"valueString":"`+pasProviderNPI+`"},{"url":"`+pastopic.ReceiverCode+`","valueIdentifier":{"value":"1111111111"}}`, 1), decisionPatient("p1"))), status: http.StatusForbidden, rule: RefusalAuthority, reason: errPASNotifyNotSubscribers},
		// A notification written as a payer's FHIR server writes one (a
		// synthetic fixture, not a capture): entries with fullUrl, request and
		// response beside the resource, a response.outcome among them,
		// carried as sent.
		"control: a notification with request beside resource": {body: serverWrittenEvent},
		// A lone surrogate the provider's gateway could not make E-22 on:
		// refused by the payer's gateway before it sends, on fidelity.
		"a lone surrogate":                  {body: []byte(strings.Replace(string(one("Patient/p1")), `"outcome":"complete"`, `"outcome":"complete","disposition":"\ud800"`, 1)), status: http.StatusBadRequest, rule: RefusalFidelity},
		"a Bundle that is not a collection": {body: contentEvent(decision("searchset", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))), refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
		"a defective Patient no ClaimResponse names": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), strings.Replace(decisionPatient("p7"), `"id":"p7"`, `"id":"p8"`, 1))),
			refusedAt: structuralUp, status: http.StatusUnprocessableEntity, reason: errPASNotifyAnswerDefect},
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSubPayer(t, level, sys, nil)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, r.body, true))
				x := p.got.only(t)
				if x.Exchange != legPASSubscriptionNotify {
					t.Fatalf("recorded on %q, want %s", x.Exchange, legPASSubscriptionNotify)
				}
				refused := r.status != 0 && (r.refusedAt == nil || r.refusedAt[level])
				if !refused {
					// Past every check, the notification is sent: with no
					// Hub here, its sending fails, never a refusal of it.
					if x.Outcome == ExchangeRefused {
						t.Fatalf("carried row refused: %d %s, record %+v", rec.Code, rec.Body.String(), x)
					}
					return
				}
				if rec.Code != r.status || !strings.Contains(rec.Body.String(), strings.TrimSuffix(r.reason, ": ")) {
					t.Fatalf("%d %s, want %d naming %q", rec.Code, rec.Body.String(), r.status, r.reason)
				}
				if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway {
					t.Fatalf("record %s by %q, want refused by the payer's gateway", x.Outcome, x.RefusedBy)
				}
				if r.rule != "" && x.Rule != r.rule {
					t.Fatalf("recorded on %q, want %q", x.Rule, r.rule)
				}
			})
		}
	}
}

// otherBaseDecisions is a notification of two decisions: the first at the
// payer's base, its ClaimResponse naming Patient/p1 (Doe), the second with
// its ClaimResponse at https://other.example/fhir, naming Patient/p1
// relatively, there another server's Patient p1 (Roe).
var otherBaseDecisions = contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
	decisionEntry(decision("collection", strings.Replace(decisionCR("cr2", "Patient/p1", pasProviderNPI), decisionBase+"/ClaimResponse/cr2", "https://other.example/fhir/ClaimResponse/cr2", 1), otherBasePatient)))

// urnPatient is the urn:uuid fullUrl of urnDecision's Patient.
const urnPatient = "urn:uuid:a0000000-0000-4000-8000-000000000002"

// urnDecision is a PAS Response Bundle written entirely with urn:uuid
// fullUrls and references, its ClaimResponse's patient patientRef.
func urnDecision(patientRef string) string {
	cr := strings.Replace(bareCR("cr1", patientRef, pasProviderNPI), `"outcome"`, `"insurer":{"reference":"urn:uuid:a0000000-0000-4000-8000-000000000003"},"outcome"`, 1)
	return decision("collection", `{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-000000000001","resource":`+cr+`}`,
		`{"fullUrl":"`+urnPatient+`","resource":{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`,
		`{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-000000000003","resource":{"resourceType":"Organization","id":"o1"}}`)
}

// otherBasePatient is a Patient entry with the member id p1 at another
// server's base, with other demographics.
const otherBasePatient = `{"fullUrl":"https://other.example/fhir/Patient/p1","resource":{"resourceType":"Patient","id":"p1","birthDate":"1990-05-06","name":[{"family":"Roe"}]}}`

// serverWrittenEvent is a full-resource notification of the PAS topic
// written as a payer's FHIR server writes one (a synthetic fixture, not a
// capture), request beside resource: every entry with its fullUrl, request and
// response, the SubscriptionStatus's own GET of $status, the decision's PUT,
// and a response.outcome.
var serverWrittenEvent = []byte(`{"resourceType":"Bundle","type":"history","timestamp":"2026-11-17T10:00:00Z","entry":[` +
	`{"fullUrl":"urn:uuid:6c3f0e2a-9b5d-4e57-8a1c-1f2d3e4f5a6b","resource":{"resourceType":"Parameters","parameter":[` +
	`{"name":"subscription","valueReference":{"reference":"` + decisionBase + `/Subscription/s1"}},{"name":"topic","valueCanonical":"` + pastopic.Canonical + `"},` +
	`{"name":"status","valueCode":"active"},{"name":"type","valueCode":"event-notification"},{"name":"events-since-subscription-start","valueString":"1"},` +
	`{"name":"notification-event","part":[{"name":"event-number","valueString":"1"},{"name":"focus","valueReference":{"reference":"` + decisionBase + `/Bundle/d1"}}]}]},` +
	`"request":{"method":"GET","url":"` + decisionBase + `/Subscription/s1/$status"},"response":{"status":"200"}},` +
	`{"fullUrl":"` + decisionBase + `/Bundle/d1","resource":` + strings.Replace(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")), `"resourceType":"Bundle",`, `"resourceType":"Bundle","id":"d1",`, 1) + `,` +
	`"request":{"method":"PUT","url":"Bundle/d1"},"response":{"status":"200","outcome":{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational"}]}}}]}`)

// decisionWithTask is a content notification whose decision carries a PAS
// Task with a payer-url (pendRekeyTask), the ClaimResponse naming the
// attested NPI, with extra members on the decision Bundle (a signature).
func decisionWithTask(bundleExtra string) []byte {
	d := decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), pendRekeyTask(`"valueUrl":"https://payer.example/doc-return"`))
	if bundleExtra != "" {
		d = strings.Replace(d, `"type":"collection",`, `"type":"collection",`+bundleExtra+`,`, 1)
	}
	return contentEvent(d)
}

// The provider's gateway delivers a payer's PAS decision notification, at
// every level, byte for byte but E-22: each PAS Task's payer-url becomes its
// own documentation return path for the payer that sent it, when that payer
// takes the documentation submission, named in X-SHN-Edited, and made even
// where a signature covers it (named with signed=). A payer that does not take
// the submission, a decision whose payer-url is not a string valueUrl, and a
// decision with no payer-url, are delivered exactly, the first two recording
// why E-22 was not made. A no-content message on the decision leg is refused 403
// on authority, and one reaching a provider that does not take the leg 422 on
// routing, nothing delivered.
func TestPASNotify_TheProvidersDeliveryMakesE22(t *testing.T) {
	ret, err := cdexroute.ReturnBase(pasSignalProviderIngress, "payer-x")
	if err != nil {
		t.Fatal(err)
	}
	signature := `"signature":{"type":[{"system":"urn:iso-astm:E1762-95:2013","code":"1.2.840.10065.1.12.1.1"}],"when":"2026-11-17T00:00:00Z","who":{"reference":"Organization/payer"},"data":"c2ln"}`
	for _, level := range allLevels {
		for name, r := range map[string]struct {
			body      []byte
			takesDocs bool
			notTaken  bool
			status    int
			edited    string // the X-SHN-Edited value wanted ("" none)
			notMade   string
			rekeyed   bool
			fidelity  bool // refused on the fidelity of SHN's own edit
		}{
			"a payer that takes the submission": {body: decisionWithTask(""), takesDocs: true, status: http.StatusOK, edited: "pas-notified-return-rekey", rekeyed: true},
			"signed, a payer that takes it":     {body: decisionWithTask(signature), takesDocs: true, status: http.StatusOK, edited: `pas-notified-return-rekey; signed=`, rekeyed: true},
			"a payer that does not take it":     {body: decisionWithTask(""), status: http.StatusOK, notMade: EditNotMadeNotifiedPayerDoesNotTake},
			// A payer-url input that is not a string valueUrl names no address
			// E-22 can re-point: delivered exactly, the value recorded.
			"a payer-url that is not a string valueUrl": {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), pendRekeyTask(`"valueString":"https://payer.example/doc-return"`))), takesDocs: true, status: http.StatusOK, notMade: EditNotMadeNotifiedNoStructuralPayerURL},
			"a decision with no payer-url":              {body: contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))), takesDocs: true, status: http.StatusOK},
			"a no-content message on the decision leg":  {body: []byte(pasEvent(pasSignalPayerBase)), takesDocs: true, status: http.StatusForbidden},
			// One relay cannot read one way only: E-22 cannot be made, so it
			// is refused on fidelity, never delivered with the payer-url.
			"a lone surrogate, a payer that takes it": {body: []byte(strings.Replace(string(decisionWithTask("")), `"outcome":"complete"`, `"outcome":"complete","disposition":"\ud800"`, 1)), takesDocs: true, status: http.StatusBadRequest, fidelity: true},
			"a lone surrogate, no payer-url":          {body: []byte(strings.Replace(string(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))), `"outcome":"complete"`, `"outcome":"complete","disposition":"\ud800"`, 1)), status: http.StatusBadRequest, fidelity: true},
			"a provider that does not take the leg":   {body: decisionWithTask(""), takesDocs: true, notTaken: true, status: http.StatusUnprocessableEntity},
		} {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				var opts []func(*Config)
				if r.notTaken {
					opts = append(opts, func(c *Config) { c.ProviderPASSubscription = false })
				}
				p := newPASSignalProvider(t, level, opts...)
				if r.takesDocs {
					e, _ := p.g.cfg.Reg.Lookup("payer-x")
					e.Legs = append(e.Legs, legCDexSubmit)
					p.g.cfg.Reg.Set("payer-x", e)
				}
				a := p.deliverOn(t, legPASSubscriptionNotify, "pci:p1", p.frame(t, pasBinding, pasProviderEndpoint, []string{"Authorization: Bearer ehr-secret"}), r.body)
				if a.status != r.status {
					t.Fatalf("answered %d %s, want %d", a.status, a.body, r.status)
				}
				x := p.got.only(t)
				if x.Exchange != legPASSubscriptionNotify {
					t.Fatalf("recorded on %q, want %s", x.Exchange, legPASSubscriptionNotify)
				}
				if r.status != http.StatusOK {
					if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByProviderGateway || len(p.bodies) != 0 {
						t.Fatalf("record %+v, %d delivered; want refused by the provider's gateway, nothing delivered", x, len(p.bodies))
					}
					if r.fidelity && (x.Rule != RefusalFidelity || !strings.Contains(string(a.body), strings.TrimSuffix(refusalPASNotifyUnreadable, "; nothing was delivered"))) {
						t.Fatalf("recorded on %q, answered %s; want a fidelity refusal of SHN's own edit", x.Rule, a.body)
					}
					return
				}
				if len(p.bodies) != 1 {
					t.Fatalf("delivered %d notifications, want one", len(p.bodies))
				}
				got, edited := p.bodies[0], p.received[0].Header.Get(EditedHeader)
				if r.rekeyed {
					if !strings.Contains(string(got), `"valueUrl":"`+ret+`"`) || strings.Contains(string(got), "payer.example/doc-return") {
						t.Fatalf("delivered %s, want the payer-url re-keyed to %s", got, ret)
					}
					if strings.Replace(string(got), ret, "https://payer.example/doc-return", 1) != string(r.body) {
						t.Fatal("E-22 changed more than the payer-url")
					}
				} else if string(got) != string(r.body) {
					t.Fatalf("delivered %s, want the payer's bytes as sent", got)
				}
				if !strings.HasPrefix(edited, r.edited) || (r.edited == "") != (edited == "") {
					t.Fatalf("X-SHN-Edited %q, want %q", edited, r.edited)
				}
				if r.notMade != "" && !slices.Contains(x.EditsNotMade, r.notMade) {
					t.Fatalf("not made %v, want %s", x.EditsNotMade, r.notMade)
				}
			})
		}
	}
}

// The content checks' conformance findings name the leg they judge: a
// decision naming two patients, at observe, is carried and its finding names
// pas-subscription-notify (patient.mixed), as a defect elsewhere's does
// (answer.shape).
func TestPASNotify_TheDecisionsFindingsNameTheLeg(t *testing.T) {
	sys := newSubStore(t)
	for name, body := range map[string][]byte{
		"patient.mixed": contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionCR("cr2", "Patient/p2", pasProviderNPI), decisionPatient("p1"), decisionPatient("p2"))),
		"answer.shape":  contentEvent(decision("searchset", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
	} {
		t.Run(name, func(t *testing.T) {
			var events []ObserverEvent
			p := newPASSubPayer(t, EnforcementObserve, sys, &events)
			rec := httptest.NewRecorder()
			codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
			var found bool
			for _, e := range events {
				if e.Kind != ConformanceObservedEvent || !strings.Contains(e.Detail, `"`+name+`"`) {
					continue
				}
				found = true
				if e.LegType != legPASSubscriptionNotify {
					t.Fatalf("the %s finding names leg %q, want %s", name, e.LegType, legPASSubscriptionNotify)
				}
			}
			if !found {
				t.Fatalf("no %s finding at observe: %d events", name, len(events))
			}
		})
	}
}

// One member is one patient, however its references are spelled: two
// decisions whose ClaimResponses name the same member, one relatively and
// one at the payer's base, name no mixed patients, carried at strict. (At
// another base it is another patient: "the member's id at another base".)
func TestPASNotify_OneMemberTwoSpellingsIsOnePatient(t *testing.T) {
	p := newPASSubPayer(t, EnforcementStrict, newSubStore(t), nil)
	body := contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
		decisionEntry(decision("collection", decisionCR("cr2", decisionBase+"/Patient/p1", pasProviderNPI), decisionPatient("p1"))))
	rec := httptest.NewRecorder()
	codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
	if x := p.got.only(t); x.Outcome == ExchangeRefused {
		t.Fatalf("one member named twice refused at strict: %d %s, %+v", rec.Code, rec.Body.String(), x)
	}
}

// A payer that requires known members (an opt-in check) refuses a decision
// naming a member its system of record does not hold, 400 recorded as
// conformance, and carries it otherwise.
func TestPASNotify_AnUnknownMemberWhereKnownMembersAreRequired(t *testing.T) {
	body := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))
	for _, required := range []bool{true, false} {
		p := newPASSubPayer(t, EnforcementNone, newSubStore(t), nil, func(c *Config) { c.RequireKnownMembers = required })
		rec := httptest.NewRecorder()
		codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
		x := p.got.only(t)
		if !required {
			if x.Outcome == ExchangeRefused {
				t.Fatalf("not required: refused %d %s", rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest || x.Outcome != ExchangeRefused || x.Rule != RefusalConformance {
			t.Fatalf("required: %d %s, record %s on %q; want 400 recorded as conformance", rec.Code, rec.Body.String(), x.Outcome, x.Rule)
		}
	}
}

// An entry the network could not read never decides a decision's patient:
// one the binding's own reader would draw on, given the decision as sent (a
// member identifier on a Patient whose identity is another's, a Patient
// contained in an unreadable entry, each carrying the member's
// demographics), refuses the decision 422 on authority, since what it would
// read there is unknown. A clean Patient carrying the member's demographics
// binds otherwise than the decision without it does (the control: such
// demographics matter to the binding, so each refusal is the binding's).
func TestPASNotify_AnUnreadableEntryNeverSuppliesThePatient(t *testing.T) {
	p := newPASSubPayer(t, EnforcementNone, newSubStore(t), nil)
	demo := `"birthDate":"1971-02-03","name":[{"family":"Unread"}]`
	bind := func(t *testing.T, entries ...string) (string, *notifierRefusal) {
		t.Helper()
		body := contentEvent(decision("collection", entries...))
		n, rf := subscription.ParseNotification(body, pasTopic)
		if rf != nil {
			t.Fatalf("fixture: %+v", rf)
		}
		return p.g.bindPASNotified(context.Background(), n, body, "")
	}
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	alone, nr := bind(t, cr)
	withDemographics, nr2 := bind(t, cr, `{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1",`+demo+`}}`)
	if nr != nil || nr2 != nil || withDemographics == alone {
		t.Fatalf("control: bound %q and %q (%+v, %+v); want two bindings, the demographics changing it", alone, withDemographics, nr, nr2)
	}
	for name, entry := range map[string]string{
		"a member identifier on an unreadable Patient": `{"fullUrl":"` + decisionBase + `/Patient/p9","resource":{"resourceType":"Patient","id":"p8","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"p1"}],` + demo + `}}`,
		"a Patient contained in an unreadable entry":   `{"fullUrl":"` + decisionBase + `/Basic/b1","resource":{"resourceType":"Basic","id":"b2","contained":[{"resourceType":"Patient","id":"p1",` + demo + `}]}}`,
	} {
		pci, nr := bind(t, cr, entry)
		if nr == nil || nr.status != http.StatusUnprocessableEntity || nr.rule != RefusalAuthority || !strings.Contains(nr.msg, "could not read bears on the binding") {
			t.Errorf("%s: bound %q (%+v), want refused 422 on authority, the unreadable entry bearing on the binding", name, pci, nr)
		}
	}
}

// Every resource a notification carries is read, wherever it sits: a
// ClaimResponse for another patient placed beside a valid decision (in a
// SubscriptionStatus parameter's resource, a later entry's Resource spelled
// in another case, an entry's response.outcome, a resource array, contained
// in the decision's ClaimResponse, or an entry of a Bundle nested in the
// decision) is held to the receiver (N6) and to the binding as the decision's
// own is. Naming another provider, it is refused 403 on authority at every
// level, nothing sent. Naming the subscriber, its patient is the decision's
// other patient: on the involved list (pasNotifiedMembers), and refused 422
// at strict (patient.mixed), carried at every other level; a nested Bundle,
// outside a PAS answer's graph, is refused at structural too (answer.shape).
func TestPASNotify_EveryClaimResponseAnywhereIsRead(t *testing.T) {
	sys := newSubStore(t)
	valid := func(extra ...string) string {
		return decision("collection", append([]string{decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), decisionPatient("p2")}, extra...)...)
	}
	placements := map[string]func(cr2 string) []byte{
		"a SubscriptionStatus parameter's resource": func(cr2 string) []byte {
			return withStatusParameter(contentEvent(valid()), `{"name":"x","resource":`+cr2+`}`)
		},
		"a later entry's Resource, in another case": func(cr2 string) []byte {
			return contentEvents(decisionEntry(valid()), `{"request":{"method":"PUT"},"Resource":`+cr2+`}`)
		},
		"an entry's response.outcome": func(cr2 string) []byte {
			return contentEvents(`{"request":{"method":"PUT"},"resource":` + valid() + `,"response":{"status":"200","outcome":` + cr2 + `}}`)
		},
		"a resource array": func(cr2 string) []byte {
			return contentEvents(decisionEntry(valid()), `{"request":{"method":"PUT"},"resource":[`+cr2+`]}`)
		},
		"contained in the decision's ClaimResponse": func(cr2 string) []byte {
			cr1 := strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"outcome":"complete"`, `"outcome":"complete","contained":[`+cr2+`]`, 1)
			return contentEvent(decision("collection", cr1, decisionPatient("p1"), decisionPatient("p2")))
		},
		"an entry of a Bundle nested in the decision": func(cr2 string) []byte {
			return contentEvent(valid(`{"fullUrl":"` + decisionBase + `/Bundle/b1","resource":{"resourceType":"Bundle","id":"b1","type":"collection","entry":[{"fullUrl":"` + decisionBase + `/ClaimResponse/cr2","resource":` + cr2 + `}]}}`))
		},
	}
	for name, place := range placements {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				send := func(body []byte) (*httptest.ResponseRecorder, ExchangeRecord) {
					t.Helper()
					p := newPASSubPayer(t, level, sys, nil)
					rec := httptest.NewRecorder()
					codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
					return rec, p.got.only(t)
				}
				// Another provider's receiver: N6, at every level.
				rec, x := send(place(bareCR("cr2", "Patient/p2", "1111111111")))
				if rec.Code != http.StatusForbidden || x.Outcome != ExchangeRefused || x.Rule != RefusalAuthority || !strings.Contains(rec.Body.String(), strings.TrimSuffix(errPASNotifyNotSubscribers, ": ")) {
					t.Fatalf("another provider's receiver: %d %s, %s on %q; want 403 on authority (N6)", rec.Code, rec.Body.String(), x.Outcome, x.Rule)
				}
				// The subscriber's receiver: the other patient is named.
				body := place(bareCR("cr2", "Patient/p2", pasProviderNPI))
				members := map[string]bool{}
				for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
					members[m.member] = true
				}
				if !members["p1"] || !members["p2"] {
					t.Fatalf("involved members %v, want p1 and p2", members)
				}
				rec, x = send(body)
				// A PAS answer's graph carries no nested Bundle: outside its
				// shape at the levels that refuse answer.shape.
				if name == "an entry of a Bundle nested in the decision" && (level == EnforcementStructural || level == EnforcementStrict) {
					if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused || !strings.Contains(rec.Body.String(), strings.TrimSuffix(errPASNotifyAnswerDefect, ": ")) {
						t.Fatalf("%s: %d %s, want 422 (answer.shape)", level, rec.Code, rec.Body.String())
					}
					return
				}
				if level == EnforcementStrict {
					if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
						t.Fatalf("strict: %d %s, want 422 (patient.mixed)", rec.Code, rec.Body.String())
					}
					return
				}
				if x.Outcome == ExchangeRefused {
					t.Fatalf("carried row refused: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// The involved list's members come from every resource the notification
// carries, by the whole-request reading the other PA legs use: a decision
// whose Task is for Patient/p2, with a Patient p2 entry, names p2 beside the
// leg's own p1, bound from the decision; and the member id p1 at another
// base, with other demographics, is another patient, bound from its own
// decision to another identifier than the leg's. A resource beside the
// decision names its patient as the decision does.
func TestPASNotify_TheInvolvedListReadsTheWholeDecision(t *testing.T) {
	p := newPASSubPayer(t, EnforcementNone, newSubStore(t), nil)
	task := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), decisionPatient("p2"),
		`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`))
	got := map[string]bool{}
	for _, m := range mustMembers(t)(pasNotifiedMembers(task, "")) {
		got[m.member] = m.subject
	}
	if subject, ok := got["p1"]; !ok || !subject {
		t.Fatalf("members %v: want p1 as the leg's own patient", got)
	}
	if subject, ok := got["p2"]; !ok || subject {
		t.Fatalf("members %v: want p2 as another patient", got)
	}
	// Beside the decision, a Task for p2 in a SubscriptionStatus parameter,
	// and a ClaimResponse naming p2 by a Reference member: p2 is named,
	// bound from the resource that names it.
	one := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))
	for name, stray := range map[string]string{
		"a Task":                         `{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}`,
		"a ClaimResponse, its Reference": strings.Replace(bareCR("cr2", "Patient/p2", pasProviderNPI), `"reference"`, `"Reference"`, 1),
	} {
		var p2 *namedMember
		for _, m := range mustMembers(t)(pasNotifiedMembers(withStatusParameter(one, `{"name":"x","resource":`+stray+`}`), "")) {
			if m.member == "p2" {
				p2 = &m
			}
		}
		if p2 == nil || p2.subject || string(p2.scope) != string(canonicalCopy([]byte(stray))) {
			t.Fatalf("%s beside the decision: p2 %+v, want another patient bound from %s", name, p2, stray)
		}
	}
	// A decision written with urn:uuid names its patient by the Patient
	// entry its reference resolves to: p1, the leg's own, and no other.
	urn := mustMembers(t)(pasNotifiedMembers(contentEvent(urnDecision(urnPatient)), ""))
	if len(urn) != 1 || urn[0].member != "p1" || !urn[0].subject {
		t.Fatalf("urn:uuid decision: members %+v, want p1 alone, the leg's own", urn)
	}
	// An unreadable entry naming that patient by its urn:uuid as a
	// Coverage's subscriber bears on the binding as one naming it by
	// Patient/p1 does: refused.
	unread := contentEvent(strings.TrimSuffix(urnDecision(urnPatient), `]}`) + `,{"fullUrl":"` + decisionBase + `/Coverage/c1","resource":{"resourceType":"Coverage","id":"c2","subscriber":{"reference":"` + urnPatient + `"}}}]}`)
	if n, rf := subscription.ParseNotification(unread, pasTopic); rf != nil {
		t.Fatal(rf)
	} else if pci, nr := p.g.bindPASNotified(context.Background(), n, unread, decisionBase); nr == nil || nr.status != http.StatusUnprocessableEntity || !strings.Contains(nr.msg, "could not read bears on the binding") {
		t.Fatalf("an unreadable Coverage naming the urn:uuid patient: bound %q (%+v), want refused 422, the entry bearing on the binding", pci, nr)
	}
	other := contentEvents(decisionEntry(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
		decisionEntry(decision("collection", decisionCR("cr2", "https://other.example/fhir/Patient/p1", pasProviderNPI), otherBasePatient)))
	n, rf := subscription.ParseNotification(other, pasTopic)
	if rf != nil {
		t.Fatalf("fixture: %+v", rf)
	}
	leg, nr := p.g.bindPASNotified(context.Background(), n, other, decisionBase)
	if nr != nil {
		t.Fatalf("bound none: %+v", nr)
	}
	var others []string
	for _, m := range mustMembers(t)(pasNotifiedMembers(other, "")) {
		if m.member != "p1" || m.subject {
			continue
		}
		pci, _, err := p.g.resolveSubjectPCI(context.Background(), m.member, m.scope)
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, pci)
	}
	if len(others) != 1 || others[0] == leg {
		t.Fatalf("the member id at another base bound to %v, the leg's %q; want one other patient", others, leg)
	}
}

// E-22's maker refuses a notification it cannot read rather than carry it
// (the delivery refuses one first; this is its last guard), and records no
// edit as not made.
func TestPASNotify_E22RefusesWhatItCannotRead(t *testing.T) {
	p := newPASSignalProvider(t, EnforcementNone)
	x := &exchangeRecorder{}
	ops, err := p.g.pasNotifiedReturnRekey(x, nil, deliveryInput{notifier: "payer-x"})
	if !errors.Is(err, errNotifiedUnreadable) || len(ops) != 0 {
		t.Fatalf("ops %v, err %v; want errNotifiedUnreadable", ops, err)
	}
}

// Patient references in the notification's own members, outside every
// resource it carries (a SubscriptionStatus parameter's or part's
// valueReference, an entry's own extension, the Bundle's extension), are
// read by the whole-request reading the other PA legs use: the member is on
// the involved list, bound from the whole notification, at every level, and
// refused 422 at strict (patient.mixed).
func TestPASNotify_TheNotificationsOwnMembersAreRead(t *testing.T) {
	sys := newSubStore(t)
	base := string(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))))
	ref := `{"reference":"Patient/p2"}`
	for name, body := range map[string]string{
		"a SubscriptionStatus parameter": strings.Replace(base, `{"name":"type","valueCode":"event-notification"}`, `{"name":"type","valueCode":"event-notification"},{"name":"x","valueReference":`+ref+`}`, 1),
		"a SubscriptionStatus part":      strings.Replace(base, `{"name":"focus"}`, `{"name":"focus"},{"name":"y","valueReference":`+ref+`}`, 1),
		"an entry's own extension":       strings.Replace(base, `{"request":{"method":"PUT"},"resource":`, `{"request":{"method":"PUT"},"extension":[{"url":"http://example.org/x","valueReference":`+ref+`}],"resource":`, 1),
		"the Bundle's extension":         strings.Replace(base, `{"resourceType":"Bundle","type":"history",`, `{"resourceType":"Bundle","type":"history","extension":[{"url":"http://example.org/x","valueReference":`+ref+`}],`, 1),
	} {
		if body == base {
			t.Fatalf("%s: fixture unchanged", name)
		}
		var p2 *namedMember
		for _, m := range mustMembers(t)(pasNotifiedMembers([]byte(body), "")) {
			if m.member == "p2" {
				p2 = &m
			}
		}
		if p2 == nil || p2.subject || string(p2.scope) != string(canonicalCopy([]byte(body))) {
			t.Fatalf("%s: p2 %+v, want another patient bound from the whole notification", name, p2)
		}
		for _, level := range allLevels {
			p := newPASSubPayer(t, level, sys, nil)
			rec := httptest.NewRecorder()
			codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, []byte(body), true))
			x := p.got.only(t)
			if level == EnforcementStrict {
				if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
					t.Fatalf("%s, strict: %d %s, want 422 (patient.mixed)", name, rec.Code, rec.Body.String())
				}
				continue
			}
			if x.Outcome == ExchangeRefused {
				t.Fatalf("%s, %s: refused %d %s", name, level, rec.Code, rec.Body.String())
			}
		}
	}
}

// Two Patients under one member id in one decision, at two identities
// (Patient/p1 at the payer's base and https://other.example/fhir/Patient/p1
// with other demographics, by a reference or as two entries), are two
// patients: below strict the decision is carried and the involved list names
// both, the leg's own and the other, each bound from its own entry to its own
// identifier; at strict it is refused 422 (patient.mixed).
func TestPASNotify_TwoPatientsUnderOneIDAreTwo(t *testing.T) {
	sys := newSubStore(t)
	for name, body := range map[string][]byte{
		"two entries": contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), otherBasePatient)),
		"a reference to the other": contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), otherBasePatient,
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","for":{"reference":"https://other.example/fhir/Patient/p1"}}}`)),
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSubPayer(t, level, sys, nil)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
				x := p.got.only(t)
				if level == EnforcementStrict {
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
						t.Fatalf("strict: %d %s, want 422 (patient.mixed)", rec.Code, rec.Body.String())
					}
					return
				}
				if x.Outcome == ExchangeRefused {
					t.Fatalf("refused %d %s", rec.Code, rec.Body.String())
				}
				n, rf := subscription.ParseNotification(body, pasTopic)
				if rf != nil {
					t.Fatal(rf)
				}
				leg, nr := p.g.bindPASNotified(context.Background(), n, body, decisionBase)
				if nr != nil && level != EnforcementStrict {
					t.Fatalf("bound none: %+v", nr)
				}
				var subjects int
				pcis := map[string]bool{}
				for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
					if m.member != "p1" {
						continue
					}
					if m.subject {
						subjects++
						continue
					}
					pci, _, err := p.g.resolveSubjectPCI(context.Background(), m.member, m.scope)
					if err != nil {
						t.Fatal(err)
					}
					pcis[pci] = true
				}
				if subjects != 1 || len(pcis) != 1 || pcis[leg] {
					t.Fatalf("the leg's own p1 %d times, others %v (the leg's %q); want the leg's and one other patient", subjects, pcis, leg)
				}
			})
		}
	}
}

// answer.shape names each IG line a decision was read at and its failure:
// at any line it could be on (2.0.1, 2.2, 2.1) for a payer that declares no
// PAS line, at its own lines for one that does. A defect only the declared
// line has (a dangling insurer, the request Claim named, which 2.1 lets go
// unresolved) is refused for a payer declaring 2.2 alone, and the same bytes
// are carried for one that declares none.
func TestPASNotify_TheDecisionsShapeIsReadAtItsLines(t *testing.T) {
	sys := newSubStore(t)
	send := func(t *testing.T, declared []string, body []byte) (*httptest.ResponseRecorder, ExchangeRecord) {
		t.Helper()
		p := newPASSubPayer(t, EnforcementStructural, sys, nil, func(c *Config) {
			c.Responder = NewNativeResponder(sys.srv.Client(), sys.srv.URL+"/fhir", c.Clock, WithDeclaredContractVersions(declared))
		})
		rec := httptest.NewRecorder()
		codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
		return rec, p.got.only(t)
	}
	dangling := contentEvent(decision("collection", strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"outcome"`, `"insurer":{"reference":"Organization/o9"},"outcome"`, 1), decisionPatient("p1")))
	rec, _ := send(t, nil, dangling)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at none of the IG lines it could be on (2.0.1: ") ||
		!strings.Contains(rec.Body.String(), "; 2.2: ") || !strings.Contains(rec.Body.String(), "; 2.1: ") {
		t.Fatalf("no PAS line declared: %d %s, want 422 naming 2.0.1, 2.2 and 2.1 and each failure", rec.Code, rec.Body.String())
	}
	only22 := contentEvent(decision("collection", withRequest(strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `"outcome"`, `"insurer":{"reference":"Organization/o9"},"outcome"`, 1)), decisionPatient("p1")))
	rec, x := send(t, []string{"pa.pas@2.2"}, only22)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at none of the IG lines the payer declares (2.2: ") || strings.Contains(rec.Body.String(), "2.1:") {
		t.Fatalf("declaring 2.2: %d %s, want 422 naming 2.2 alone", rec.Code, rec.Body.String())
	}
	if x.Outcome != ExchangeRefused {
		t.Fatalf("declaring 2.2: recorded %s", x.Outcome)
	}
	// A payer declaring 2.0 is held to the 2.0.1 rule, the line its token
	// names.
	if rec, _ := send(t, []string{"pa.pas@2.0"}, only22); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at none of the IG lines the payer declares (2.0.1: ") {
		t.Fatalf("declaring 2.0: %d %s, want 422 naming 2.0.1 alone", rec.Code, rec.Body.String())
	}
	if rec, x := send(t, nil, only22); x.Outcome == ExchangeRefused {
		t.Fatalf("declaring none: refused %d %s, want carried (2.1 accepts it)", rec.Code, rec.Body.String())
	}
}

// The leg's patient is bound from the Patient its ClaimResponse's reference
// resolves to, never from another Patient under the same member id: with
// another Patient p1 (other demographics) beside the decision's own, at the
// top level, in a nested Bundle, contained in a Task, or in another decision
// whose ClaimResponse sits at another base, or in a SubscriptionStatus
// parameter, the leg's identifier is exactly
// the binding of the ClaimResponse's own Patient entry, and the other Patient
// is on the involved list as another identity, bound from itself. Each is
// carried below strict and refused 422 at strict (patient.mixed; the nested
// Bundle, outside a PAS answer's graph, by answer.shape at structural and
// strict), at every level.
func TestPASNotify_TheLegBindsItsOwnPatient(t *testing.T) {
	sys := newSubStore(t)
	roe := `{"resourceType":"Patient","id":"p1","birthDate":"1990-05-06","name":[{"family":"Roe"}]}`
	own := `{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	for name, c := range map[string]struct {
		body  []byte
		shape bool // refused by answer.shape at structural and strict
	}{
		"top level": {body: contentEvent(decision("collection", cr, decisionPatient("p1"), otherBasePatient))},
		"a nested Bundle": {body: contentEvent(decision("collection", cr, decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Bundle/b1","resource":{"resourceType":"Bundle","id":"b1","type":"collection","entry":[{"fullUrl":"https://other.example/fhir/Patient/p1","resource":`+roe+`}]}}`)), shape: true},
		"contained in a Task": {body: contentEvent(decision("collection", cr, decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[`+roe+`]}}`))},
		"another decision at another base": {body: otherBaseDecisions},
		"a SubscriptionStatus parameter":   {body: withStatusParameter(contentEvent(decision("collection", cr, decisionPatient("p1"))), `{"name":"x","resource":`+roe+`}`)},
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				p := newPASSubPayer(t, level, sys, nil)
				rec := httptest.NewRecorder()
				codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, c.body, true))
				x := p.got.only(t)
				switch {
				case c.shape && (level == EnforcementStructural || level == EnforcementStrict):
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), strings.TrimSuffix(errPASNotifyAnswerDefect, ": ")) {
						t.Fatalf("%d %s, want 422 (answer.shape)", rec.Code, rec.Body.String())
					}
				case level == EnforcementStrict:
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
						t.Fatalf("%d %s, want 422 (patient.mixed)", rec.Code, rec.Body.String())
					}
				case x.Outcome == ExchangeRefused:
					t.Fatalf("refused %d %s, want carried", rec.Code, rec.Body.String())
				}
				// The binding, read at none (no conformance rule stands
				// before it), whatever level the route ran at.
				none := newPASSubPayer(t, EnforcementNone, sys, nil)
				n, rf := subscription.ParseNotification(c.body, pasTopic)
				if rf != nil {
					t.Fatal(rf)
				}
				leg, nr := none.g.bindPASNotified(context.Background(), n, c.body, decisionBase)
				if nr != nil {
					t.Fatalf("bound none: %+v", nr)
				}
				want, _, err := none.g.resolveSubjectPCI(context.Background(), "p1", []byte(own))
				if err != nil || leg != want {
					t.Fatalf("the leg bound to %q, want %q, the binding of the ClaimResponse's own Patient (%v)", leg, want, err)
				}
				others := map[string]bool{}
				for _, m := range mustMembers(t)(pasNotifiedMembers(c.body, "")) {
					if m.member != "p1" || m.subject {
						continue
					}
					pci, _, err := none.g.resolveSubjectPCI(context.Background(), m.member, m.scope)
					if err != nil {
						t.Fatal(err)
					}
					if pci != leg {
						others[pci] = true
					}
				}
				if len(others) != 1 {
					t.Fatalf("other patients under p1 %v, want the other Patient alone", others)
				}
			})
		}
	}
}

// sendNotified posts body to a payer's gateway at level, answering its
// status, body and record.
func sendNotified(t *testing.T, sys *subStore, level ConformanceEnforcement, body []byte, opts ...func(*Config)) (*httptest.ResponseRecorder, ExchangeRecord) {
	t.Helper()
	p := newPASSubPayer(t, level, sys, nil, opts...)
	rec := httptest.NewRecorder()
	codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
	return rec, p.got.only(t)
}

// notifiedBinding is body's leg binding and the bindings of every other
// identity its involved list names (pasNotifiedMembers), read at none.
func notifiedBinding(t *testing.T, sys *subStore, body []byte) (leg string, others map[string]bool) {
	t.Helper()
	g := newPASSubPayer(t, EnforcementNone, sys, nil).g
	n, rf := subscription.ParseNotification(body, pasTopic)
	if rf != nil {
		t.Fatal(rf)
	}
	stated := sys.srv.URL + "/fhir"
	leg, nr := g.bindPASNotified(context.Background(), n, body, stated)
	if nr != nil {
		t.Fatalf("bound none: %+v", nr)
	}
	others = map[string]bool{}
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, stated)) {
		if m.subject {
			continue
		}
		pci, _, _, err := g.resolveBinding(context.Background(), m.member, m.scope, true)
		if err != nil {
			t.Fatal(err)
		}
		if pci != leg {
			others[m.member+"="+pci] = true
		}
	}
	return leg, others
}

// expectAt checks body's outcome at level: refused 422 naming reason at the
// levels refusing names, carried at every other.
func expectAt(t *testing.T, sys *subStore, level ConformanceEnforcement, body []byte, refusing map[ConformanceEnforcement]string) {
	t.Helper()
	rec, x := sendNotified(t, sys, level, body)
	if reason, ok := refusing[level]; ok {
		if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused || !strings.Contains(rec.Body.String(), strings.TrimSuffix(reason, ": ")) {
			t.Fatalf("%s: %d %s, want 422 naming %q", level, rec.Code, rec.Body.String(), reason)
		}
		return
	}
	if x.Outcome == ExchangeRefused {
		t.Fatalf("%s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
	}
}

// roePatient is a Patient p1 with other demographics than decisionPatient's.
const roePatient = `{"resourceType":"Patient","id":"p1","birthDate":"1990-05-06","name":[{"family":"Roe"}]}`

// When the leg's ClaimResponse's patient reference resolves to no Patient
// entry, the member id is the only identity the decision states for its
// patient: the leg binds from the member alone, whatever other Patient under
// that id the notification carries (at another base, contained in a Task, in
// a nested Bundle, or contained in a bare ClaimResponse that never names
// it), and that Patient is on the involved list as its own identity. So it
// does when the reference resolves to a Patient that carries no
// demographics. Strict
// refuses each (patient.mixed, or answer.shape where the decision is outside
// a PAS answer's graph), every other level carries it.
func TestPASNotify_NothingResolvesBindsTheMemberAlone(t *testing.T) {
	sys := newSubStore(t)
	cr := withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI))
	strict := map[ConformanceEnforcement]string{EnforcementStrict: errPASNotifyMixed}
	shape := map[ConformanceEnforcement]string{EnforcementStructural: errPASNotifyAnswerDefect, EnforcementStrict: errPASNotifyAnswerDefect}
	memberOnly, _, err := newPASSubPayer(t, EnforcementNone, sys, nil).g.resolveSubjectPCI(context.Background(), "p1", memberOnlyScope)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body     []byte
		refusing map[ConformanceEnforcement]string
	}{
		"another base": {contentEvent(decision("collection", cr, otherBasePatient)), strict},
		"contained in a Task": {contentEvent(decision("collection", cr,
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[`+roePatient+`]}}`)), strict},
		"a nested Bundle": {contentEvent(decision("collection", cr,
			`{"fullUrl":"`+decisionBase+`/Bundle/b1","resource":{"resourceType":"Bundle","id":"b1","type":"collection","entry":[{"fullUrl":"https://other.example/fhir/Patient/p1","resource":`+roePatient+`}]}}`)), shape},
		"a bare ClaimResponse's unreferenced contained Patient": {contentEvent(strings.Replace(bareCR("cr1", "Patient/p1", pasProviderNPI), `"outcome":"complete"`, `"outcome":"complete","contained":[`+roePatient+`]`, 1)), strict},
		// Resolved to a Patient that carries no demographics, the leg binds
		// from the member alone too.
		"a resolved Patient without demographics": {contentEvent(decision("collection", cr, `{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1"}}`,
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order","contained":[`+roePatient+`]}}`)), strict},
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				expectAt(t, sys, level, c.body, c.refusing)
				leg, others := notifiedBinding(t, sys, c.body)
				if leg != memberOnly {
					t.Fatalf("the leg bound to %q, want the member alone's %q", leg, memberOnly)
				}
				if len(others) != 1 {
					t.Fatalf("others %v, want the Roe Patient alone as its own identity", others)
				}
			})
		}
	}
}

// patient.mixed reads the involved list's own identities: a bare
// ClaimResponse naming Patient/p1, the leg bound from the member alone, and a
// Patient p1 with demographics in a SubscriptionStatus parameter, which binds
// otherwise, are two patients: listed below strict, refused at strict.
func TestPASNotify_MixedReadsTheInvolvedList(t *testing.T) {
	sys := newSubStore(t)
	body := withStatusParameter(contentEvent(bareCR("cr1", "Patient/p1", pasProviderNPI)), `{"name":"x","resource":`+strings.Replace(decisionPatient("p1"), `{"fullUrl":"`+decisionBase+`/Patient/p1","resource":`, "", 1)[:len(strings.Replace(decisionPatient("p1"), `{"fullUrl":"`+decisionBase+`/Patient/p1","resource":`, "", 1))-1]+`}`)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			expectAt(t, sys, level, body, map[ConformanceEnforcement]string{EnforcementStrict: errPASNotifyMixed})
			if _, others := notifiedBinding(t, sys, body); len(others) != 1 {
				t.Fatalf("others %v, want the parameter's Patient p1", others)
			}
		})
	}
}

// Every member name and resourceType is read under case folding by the
// involved list as by the walk, N6 and the binding: a Patient p1 (Roe) under
// a Task's "Contained", a "patient" p1 in a Task's contained or in a
// SubscriptionStatus parameter, and a parameter's Task "for" by "Reference"
// to Patient/p2, each beside a decision whose patient is p1 (Doe), are each
// listed as their own identity, and refused at strict.
func TestPASNotify_CaseVariantNamesAreListed(t *testing.T) {
	sys := newSubStore(t)
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	lower := strings.Replace(roePatient, `"Patient"`, `"patient"`, 1)
	task := func(contained string) string {
		return `{"fullUrl":"` + decisionBase + `/Task/t1","resource":{"resourceType":"Task","id":"t1","status":"requested","intent":"order",` + contained + `}}`
	}
	for name, body := range map[string][]byte{
		"a Task's Contained":              contentEvent(decision("collection", cr, decisionPatient("p1"), task(`"Contained":[`+roePatient+`]`))),
		"a patient in a Task's contained": contentEvent(decision("collection", cr, decisionPatient("p1"), task(`"contained":[`+lower+`]`))),
		"a patient in a parameter":        withStatusParameter(contentEvent(decision("collection", cr, decisionPatient("p1"))), `{"name":"x","resource":`+lower+`}`),
		"a parameter Task's Reference":    withStatusParameter(contentEvent(decision("collection", cr, decisionPatient("p1"))), `{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"Reference":"Patient/p2"}}}`),
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				rec, x := sendNotified(t, sys, level, body)
				if level == EnforcementStrict {
					if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused {
						t.Fatalf("strict: %d %s, want refused 422", rec.Code, rec.Body.String())
					}
				} else if level != EnforcementStructural && x.Outcome == ExchangeRefused {
					t.Fatalf("refused %d %s, want carried", rec.Code, rec.Body.String())
				}
				if _, others := notifiedBinding(t, sys, body); len(others) != 1 {
					t.Fatalf("others %v, want the case-variant one listed as its own identity", others)
				}
			})
		}
	}
}

// The decision as a whole is read for the leg's patient only for whether the
// member's number is a Coverage party's: a resolved Patient p1 with no
// demographics, and a Coverage whose contained subscriber carries the member
// identifier p1, is a patient the network cannot identify, 422 at every
// level.
func TestPASNotify_TheSharedNumberRefusalStands(t *testing.T) {
	sys := newSubStore(t)
	body := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
		`{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1"}}`,
		`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","contained":[{"resourceType":"Patient","id":"sub","identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"p1"}],"birthDate":"1960-01-01","name":[{"family":"Parent"}]}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#sub"}}}`))
	for _, level := range allLevels {
		rec, x := sendNotified(t, sys, level, body)
		if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused || x.Rule != RefusalAuthority || !strings.Contains(rec.Body.String(), "member number is shared") {
			t.Fatalf("%s: %d %s (%s on %q), want 422 on authority, the patient unidentifiable", level, rec.Code, rec.Body.String(), x.Outcome, x.Rule)
		}
	}
}

// A relative reference that resolves against no base and one at the payer's
// stated base name one patient: a bare ClaimResponse naming Patient/p1 beside
// a decision whose ClaimResponse sits at the stated base is carried at
// strict; at another base, two patients, refused at strict.
func TestPASNotify_TheStatedBaseIsTheRelativeOnesBase(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	at := func(base string) []byte {
		cr := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, base, 1)
		return contentEvents(decisionEntry(decision("collection", cr)), decisionEntry(bareCR("cr2", "Patient/p1", pasProviderNPI)))
	}
	if rec, x := sendNotified(t, sys, EnforcementStrict, at(stated)); x.Outcome == ExchangeRefused {
		t.Fatalf("at the stated base: refused %d %s, want one patient, carried", rec.Code, rec.Body.String())
	}
	if rec, _ := sendNotified(t, sys, EnforcementStrict, at(decisionBase)); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
		t.Fatalf("at another base: %d %s, want 422 (patient.mixed)", rec.Code, rec.Body.String())
	}
}

// Another decision naming the leg's member, with no Patient for it, names
// the leg's own patient, a name of it as pasSubjectNames reads a request's
// subject: it adds no other identity, and strict carries the notification.
func TestPASNotify_AnotherDecisionsMemberIsTheLegs(t *testing.T) {
	sys := newSubStore(t)
	first := decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)))
	second := decision("collection", withRequest(decisionCR("cr2", "Patient/p1", pasProviderNPI)))
	body := contentEvents(decisionEntry(first), decisionEntry(second))
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
		if !m.subject {
			t.Fatalf("another identity %s from %s, want the leg's own alone", m.member, m.scope)
		}
	}
	if rec, x := sendNotified(t, sys, EnforcementStrict, body); x.Outcome == ExchangeRefused {
		t.Fatalf("strict: refused %d %s, want carried", rec.Code, rec.Body.String())
	}
}

// heldSoR is a system of record that holds the members held names, each
// under its own identifier, and reads every other through SystemOfRecord.
type heldSoR struct {
	SystemOfRecord
	held map[string]string
}

func (h heldSoR) ResolvePatient(m string) (string, Demo, bool) {
	if pci, ok := h.held[m]; ok {
		return pci, Demo{}, true
	}
	return h.SystemOfRecord.ResolvePatient(m)
}

func (h heldSoR) ResolvePatientContext(ctx context.Context, m string) (string, Demo, bool, error) {
	if pci, ok := h.held[m]; ok {
		return pci, Demo{}, true, nil
	}
	return ReadSystemOfRecord(h.SystemOfRecord).ResolvePatientContext(ctx, m)
}

// holding is the option that makes the payer's system of record hold p1.
func holding(c *Config) {
	c.SoR = heldSoR{SystemOfRecord: c.SoR, held: map[string]string{"p1": "held-p1"}}
}

// failingForSoR is a system of record whose read of the member fails names
// fails with err, and that reads every other member as its
// ContextSystemOfRecord does.
type failingForSoR struct {
	SystemOfRecord
	ContextSystemOfRecord
	fails string
	err   error
}

func (f failingForSoR) ResolvePatientContext(ctx context.Context, m string) (string, Demo, bool, error) {
	if m == f.fails {
		return "", Demo{}, false, f.err
	}
	return f.ContextSystemOfRecord.ResolvePatientContext(ctx, m)
}

// Another identity the payer's own system of record cannot answer for (a
// Patient the notification carries outside its decision that no reference
// names) is no second patient the notification names: the check could not
// finish. It is recorded as an unavailable patient.mixed finding where the
// level records one (observe, structural), the decision carried; where it
// refuses (strict), the refusal is the system of record's failure (503 for
// one unavailable, as on every leg), never patient.mixed's answer. A second
// patient the notification itself shows is still patient.mixed, whatever
// read failed beside it: another identity binding otherwise, before or after
// the failed one, or a decision failing the PAS answer's subject rule.
func TestPASNotify_AnotherIdentityTheSystemOfRecordCannotReadIsItsFailure(t *testing.T) {
	sys := newSubStore(t)
	patient := func(id, family string) string {
		return `{"name":"x","resource":{"resourceType":"Patient","id":"` + id + `","birthDate":"1990-02-02","name":[{"family":"` + family + `"}]}}`
	}
	one := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))
	// p2 is carried in a SubscriptionStatus parameter, outside the decision,
	// and referenced by nothing: an identity only its binding can tell from
	// the leg's.
	body := withStatusParameter(one, patient("p2", "Roe"))
	failing := func(c *Config) {
		held := heldSoR{SystemOfRecord: c.SoR, held: map[string]string{"p1": "held-p1"}}
		c.SoR = failingForSoR{SystemOfRecord: held, ContextSystemOfRecord: ReadSystemOfRecord(held), fails: "p2", err: &SoRReadError{Kind: SoRUnavailable}}
	}
	for _, level := range allLevels {
		var events []ObserverEvent
		p := newPASSubPayer(t, level, sys, &events, failing)
		rec := httptest.NewRecorder()
		codedRoute(shnsdk.MaxRequestBytes, noCredential, p.g.ingressRoute(RoutePASNotify)).ServeHTTP(rec, pasNotifyRequest(t, p, pastopic.TokenValue, body, true))
		x := p.got.only(t)
		var findings []string
		for _, e := range events {
			if e.Kind == ConformanceObservedEvent && strings.Contains(e.Detail, `"rule":"patient.mixed"`) {
				findings = append(findings, e.Detail)
			}
		}
		for _, f := range findings {
			if !strings.Contains(f, `"verdict":"unavailable"`) {
				t.Fatalf("%s: a patient.mixed finding %s, want it unavailable", level, f)
			}
		}
		switch level {
		case EnforcementNone:
			if x.Outcome == ExchangeRefused || len(findings) != 0 {
				t.Fatalf("%s: %s with %d findings (%d %s), want carried with none", level, x.Outcome, len(findings), rec.Code, rec.Body.String())
			}
		case EnforcementStrict:
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), errPASNotifyMixed) {
				t.Fatalf("%s: %d %s, want 503, the system of record's failure, not patient.mixed", level, rec.Code, rec.Body.String())
			}
			if x.Outcome != ExchangeRefused || x.RefusedBy != RefusedByPayerGateway || x.Rule != RefusalConformance || len(findings) != 1 || !strings.Contains(findings[0], `"decision":"refused"`) {
				t.Fatalf("%s: record %s by %q on %q, findings %v; want refused by the payer's gateway on conformance, one refused unavailable finding", level, x.Outcome, x.RefusedBy, x.Rule, findings)
			}
		default:
			if x.Outcome == ExchangeRefused || len(findings) != 1 {
				t.Fatalf("%s: %s with findings %v (%d %s), want carried with one unavailable finding", level, x.Outcome, findings, rec.Code, rec.Body.String())
			}
		}
	}
	// The controls: the system of record answering, p2 is another patient,
	// refused at strict as patient.mixed; and a second patient the
	// notification shows beside the failed read is patient.mixed, the failed
	// read before it or after it, or the decision failing the PAS answer's
	// subject rule (p2, the failed read, an entry of the decision).
	inDecision := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), strings.Replace(decisionPatient("p2"), "Doe", "Roe", 1)))
	for name, c := range map[string]struct {
		body []byte
		opt  func(*Config)
	}{
		"p2 read":                        {body, holding},
		"p2 failing, then p3 another":    {withStatusParameter(withStatusParameter(one, patient("p2", "Roe")), patient("p3", "Poe")), failing},
		"p3 another, then p2 failing":    {withStatusParameter(withStatusParameter(one, patient("p3", "Poe")), patient("p2", "Roe")), failing},
		"p2 failing, the subject rule's": {inDecision, failing},
	} {
		rec, _ := sendNotified(t, sys, EnforcementStrict, c.body, c.opt)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
			t.Fatalf("%s, strict: %d %s, want 422 (patient.mixed)", name, rec.Code, rec.Body.String())
		}
	}
}

// sharedNumberDecision is a decision whose ClaimResponse resolves to Patient
// p1 (patient, an entry's resource), beside a Coverage whose contained
// subscriber carries the member identifier p1: the member's number is a
// Coverage party's.
func sharedNumberDecision(patient string) []byte {
	return contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
		`{"fullUrl":"`+decisionBase+`/Patient/p1","resource":`+patient+`}`,
		`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","contained":[{"resourceType":"Patient","id":"sub","identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"p1"}],"birthDate":"1960-01-01","name":[{"family":"Parent"}]}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#sub"}}}`))
}

// The leg's own Patient is the resolved graph entry itself, never another
// object that states its fullUrl: a {fullUrl, resource} holding Roe hidden in
// an entry's search or response member, a nested Bundle entry under that
// fullUrl placed before the real Patient, or one in a SubscriptionStatus
// parameter, leaves the leg bound from its own Patient (Doe) and lists Roe as
// its own identity; with an absolute reference no entry resolves, the leg
// binds from its member alone. Strict refuses each, every other level carries
// it (structural refusing what is outside a PAS answer's graph).
func TestPASNotify_TheOwnPatientIsTheGraphsEntry(t *testing.T) {
	sys := newSubStore(t)
	none := newPASSubPayer(t, EnforcementNone, sys, nil).g
	doe, _, err := none.resolveSubjectPCI(context.Background(), "p1", canonicalCopy([]byte(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	memberOnly, _, _ := none.resolveSubjectPCI(context.Background(), "p1", memberOnlyScope)
	hidden := `{"fullUrl":"` + decisionBase + `/Patient/p1","resource":` + roePatient + `}`
	crWith := func(member string) string {
		return strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), `,"resource":`, `,"`+member+`":`+hidden+`,"resource":`, 1)
	}
	for name, c := range map[string]struct {
		body []byte
		leg  string
	}{
		"in an entry's search":   {contentEvent(decision("collection", crWith("search"), decisionPatient("p1"))), doe},
		"in an entry's response": {contentEvent(decision("collection", crWith("response"), decisionPatient("p1"))), doe},
		"a nested Bundle entry before the Patient": {contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
			`{"fullUrl":"`+decisionBase+`/Bundle/b1","resource":{"resourceType":"Bundle","id":"b1","type":"collection","entry":[`+hidden+`]}}`, decisionPatient("p1"))), doe},
		"an absolute reference no entry resolves": {contentEvent(decision("collection", strings.Replace(crWith("search"), `"reference":"Patient/p1"`, `"reference":"`+decisionBase+`/Patient/p1"`, 1))), memberOnly},
		"in a SubscriptionStatus parameter": {withStatusParameter(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			`{"name":"x","fullUrl":"`+decisionBase+`/Patient/p1","resource":`+roePatient+`}`), doe},
	} {
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) {
				rec, x := sendNotified(t, sys, level, c.body)
				switch {
				case level == EnforcementStrict:
					if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused {
						t.Fatalf("strict: %d %s, want refused 422", rec.Code, rec.Body.String())
					}
				case level != EnforcementStructural && x.Outcome == ExchangeRefused:
					t.Fatalf("refused %d %s, want carried", rec.Code, rec.Body.String())
				}
				leg, others := notifiedBinding(t, sys, c.body)
				if leg != c.leg {
					t.Fatalf("the leg bound to %q, want %q", leg, c.leg)
				}
				if len(others) != 1 {
					t.Fatalf("others %v, want Roe alone as its own identity", others)
				}
			})
		}
	}
}

// The leg's binding follows the shared rule's own order: the system of
// record first. A member the payer's system holds is bound to its held
// identifier, though its number is a Coverage party's, and carried at every
// level; one it does not hold, where the participant requires known members,
// is the shared rule's answer, 400 recorded as conformance.
func TestPASNotify_TheSystemOfRecordIsReadFirst(t *testing.T) {
	sys := newSubStore(t)
	body := sharedNumberDecision(`{"resourceType":"Patient","id":"p1"}`)
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, body, holding); x.Outcome == ExchangeRefused {
			t.Fatalf("%s, held: refused %d %s, want carried", level, rec.Code, rec.Body.String())
		}
		rec, x := sendNotified(t, sys, level, body, func(c *Config) { c.RequireKnownMembers = true })
		if rec.Code != http.StatusBadRequest || x.Rule != RefusalConformance {
			t.Fatalf("%s, known members required: %d %s on %q, want 400 on conformance", level, rec.Code, rec.Body.String(), x.Rule)
		}
	}
	g := newPASSubPayer(t, EnforcementNone, sys, nil, holding).g
	n, _ := subscription.ParseNotification(body, pasTopic)
	if pci, nr := g.bindPASNotified(context.Background(), n, body, decisionBase); nr != nil || pci != "held-p1" {
		t.Fatalf("held: bound %q (%+v), want the held identifier", pci, nr)
	}
	// Another identity that cannot be bound (an unknown member where known
	// members are required) is another patient: refused at strict.
	unknown := withStatusParameter(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
		`{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`)
	rec, _ := sendNotified(t, sys, EnforcementStrict, unknown, holding, func(c *Config) { c.RequireKnownMembers = true })
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
		t.Fatalf("an unknown other member, strict: %d %s, want 422 (patient.mixed)", rec.Code, rec.Body.String())
	}
}

// Every name the leg's own Patient is read under (its id and member
// identifiers, as pasSubjectNames reads a request's subject) is the leg's, at
// the leg's own base: a Patient p1 carrying the member identifier MBR-9, and
// a Task elsewhere for Patient/MBR-9 resolving to that base, is one identity,
// carried at strict. Its demographics are read as every leg's binding reads
// them: birthDate, name and family spelled as FHIR spells them (a Patient
// spelling them in another case carries none, and is bound as the other
// legs bind it), so one person has one identifier on every leg. A Coverage's party (a contained
// subscriber carrying the member's number, a payor Patient) is no identity.
func TestPASNotify_TheLegsNamesAreOne(t *testing.T) {
	sys := newSubStore(t)
	// The SubscriptionStatus's entry at the decision's base: its relative
	// reference resolves there (FHIR R4's Bundle rule), to the leg's base.
	mbr := withStatusParameter(atStatusURL(decisionBase+"/Parameters/st1", contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
		`{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"MBR-9"}],"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`))),
		`{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/MBR-9"}}}`)
	// The same, the Task for Patient/MBR-9 inside the decision.
	inside := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
		`{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"MBR-9"}],"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`,
		`{"fullUrl":"`+decisionBase+`/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/MBR-9"}}}`))
	party := sharedNumberDecision(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`)
	payor := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), strings.Replace(decisionPatient("p0"), "Doe", "Parent", 1),
		`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"payor":[{"reference":"Patient/p0"}]}}`))
	for name, body := range map[string][]byte{"a member identifier": mbr, "a member identifier in the decision": inside, "a contained subscriber": party, "a payor Patient": payor} {
		for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
			if !m.subject {
				t.Fatalf("%s: another identity %s from %s, want the leg's own alone", name, m.member, m.scope)
			}
		}
	}
	for _, body := range [][]byte{mbr, party} {
		if rec, x := sendNotified(t, sys, EnforcementStrict, body); x.Outcome == ExchangeRefused {
			t.Fatalf("strict: refused %d %s, want carried", rec.Code, rec.Body.String())
		}
	}
	folded := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI),
		`{"fullUrl":"`+decisionBase+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1","BirthDate":"1980-01-01","Name":[{"Family":"Doe"}]}}`))
	leg, _ := notifiedBinding(t, sys, folded)
	g := newPASSubPayer(t, EnforcementNone, sys, nil).g
	want, _, _ := g.resolveSubjectPCI(context.Background(), "p1", []byte(`{"resourceType":"Patient","id":"p1","BirthDate":"1980-01-01","Name":[{"Family":"Doe"}]}`))
	spelled, _, _ := g.resolveSubjectPCI(context.Background(), "p1", []byte(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`))
	if leg != want || leg == spelled {
		t.Fatalf("demographics in another case: bound %q, want %q, as every other leg binds it (not %q, the FHIR spelling's)", leg, want, spelled)
	}
}

// A Coverage's party is read as the shared rule reads it whatever case its
// own members are spelled in: a contained subscriber carrying the member's
// number, its meta or versionId spelled in another case, is still the party,
// never another identity on the involved list, and strict carries the
// decision, as it carries the control spelled as FHIR spells it.
func TestPASNotify_APartySpelledInAnotherCaseIsNoIdentity(t *testing.T) {
	sys := newSubStore(t)
	base := string(sharedNumberDecision(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`))
	const sub = `{"resourceType":"Patient","id":"sub",`
	for name, meta := range map[string]string{
		"the control": `"meta":{"versionId":"1"},`,
		"Meta":        `"Meta":{"versionId":"1"},`,
		"VersionId":   `"meta":{"VersionId":"1"},`,
	} {
		body := []byte(strings.Replace(base, sub, sub+meta, 1))
		if !strings.Contains(string(body), sub+meta) {
			t.Fatalf("%s: the fixture carries no contained subscriber to spell", name)
		}
		for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
			if !m.subject {
				t.Fatalf("%s: the party %s is another identity, want the leg's own alone", name, m.member)
			}
		}
		if rec, x := sendNotified(t, sys, EnforcementStrict, body); x.Outcome == ExchangeRefused {
			t.Fatalf("%s, strict: refused %d %s, want carried", name, rec.Code, rec.Body.String())
		}
	}
}

// A notification whose patients cannot be read names no one for the
// involved list: the leg's reading is errRequestPatientsUnreadable, as every
// leg's is (requestMembers), never an empty list; a readable one names the
// leg's own patient.
func TestPASNotify_AnUnreadableNotificationNamesNoOne(t *testing.T) {
	one := string(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))))
	repeated := strings.Replace(one, `"resourceType":"ClaimResponse"`, `"resourceType":"ClaimResponse","resourceType":"ClaimResponse"`, 1)
	if repeated == one {
		t.Fatal("the fixture carries no ClaimResponse to repeat a name in")
	}
	for name, body := range map[string]string{
		"not JSON":        "not json",
		"a name repeated": repeated,
		"no notification": `{"resourceType":"Bundle","type":"collection"}`,
		// A notification that parses, its decision's ClaimResponse naming no
		// patient: its patients cannot be read (readPASNotified).
		"a ClaimResponse naming no patient": strings.Replace(one, `"patient":{"reference":"Patient/p1"},`, "", 1),
	} {
		if got, err := (paWorkstream{}).requestMembers(legPASSubscriptionNotify, []byte(body)); got != nil || !errors.Is(err, errRequestPatientsUnreadable) {
			t.Errorf("%s: the leg names %d members (%v), want none and that its patients cannot be read", name, len(got), err)
		}
	}
	got := mustMembers(t)((paWorkstream{}).requestMembers(legPASSubscriptionNotify, []byte(one)))
	if len(got) != 1 || !got[0].subject || got[0].member != "p1" {
		t.Fatalf("the control names %v, want the leg's own p1 alone", got)
	}
}

// Another decision whose ClaimResponse sits at another server's base, naming
// Patient/p1 relatively with no Patient entry, names that server's p1: a
// different identity from the leg's, on the involved list, bound from that
// decision, and refused 422 at strict (patient.mixed); carried at every other
// level.
func TestPASNotify_AnotherBasesMemberIsAnotherIdentity(t *testing.T) {
	sys := newSubStore(t)
	first := decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)))
	second := decision("collection", strings.Replace(withRequest(decisionCR("cr2", "Patient/p1", pasProviderNPI)), decisionBase+"/ClaimResponse/cr2", "https://other.example/fhir/ClaimResponse/cr2", 1))
	body := contentEvents(decisionEntry(first), decisionEntry(second))
	var entry *namedMember
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, "")) {
		if m.member == "p1" && !m.subject {
			entry = &m
		}
	}
	if entry == nil || string(entry.scope) != string(canonicalCopy([]byte(second))) {
		t.Fatalf("the other base's p1: %+v, want an identity bound from its decision", entry)
	}
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			expectAt(t, sys, level, body, map[ConformanceEnforcement]string{EnforcementStrict: errPASNotifyMixed})
		})
	}
}

// expectListed checks body at every level: refused 422 at strict
// (patient.mixed), carried at none and observe, and its involved list names
// exactly want other identities beside the leg's.
func expectListed(t *testing.T, sys *subStore, body []byte, want int) {
	t.Helper()
	for _, level := range allLevels {
		rec, x := sendNotified(t, sys, level, body)
		switch level {
		case EnforcementStrict:
			if rec.Code != http.StatusUnprocessableEntity || x.Outcome != ExchangeRefused {
				t.Fatalf("strict: %d %s, want refused 422", rec.Code, rec.Body.String())
			}
		case EnforcementNone, EnforcementObserve:
			if x.Outcome == ExchangeRefused {
				t.Fatalf("%s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
			}
		}
	}
	var others []string
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, sys.srv.URL+"/fhir")) {
		if !m.subject {
			others = append(others, m.member+" from "+string(m.scope))
		}
	}
	if len(others) != want {
		t.Fatalf("other identities %v, want %d", others, want)
	}
}

// A relative reference and one at the payer's stated base name one patient,
// on the involved list as in patient.mixed, the stated base threaded to both
// (withNotifiedBase): the leg's decision at the stated base with its Patient
// (Doe) and a bare ClaimResponse naming Patient/p1 relatively, with no
// Patient, are one patient: no other identity, carried at strict.
func TestPASNotify_TheStatedBaseHoldsWithDemographics(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	cr := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, stated, 1)
	body := contentEvents(decisionEntry(decision("collection", cr, strings.Replace(decisionPatient("p1"), decisionBase, stated, 1))),
		decisionEntry(bareCR("cr2", "Patient/p1", pasProviderNPI)))
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, body); x.Outcome == ExchangeRefused {
			t.Fatalf("%s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
		}
	}
	for _, m := range mustMembers(t)(namedMembersIn(withNotifiedBase(context.Background(), stated), body, legPASSubscriptionNotify)) {
		if !m.subject {
			t.Fatalf("another identity %s from %s, want the leg's own alone", m.member, m.scope)
		}
	}
	// The stated base spelled with its scheme in upper case is that base, as
	// the shared rule compares entries (entryKey).
	spelled := contentEvents(decisionEntry(decision("collection", cr, strings.Replace(decisionPatient("p1"), decisionBase, stated, 1))),
		decisionEntry(bareCR("cr2", "HTTP"+strings.TrimPrefix(stated, "http")+"/Patient/p1", pasProviderNPI)))
	if rec, x := sendNotified(t, sys, EnforcementStrict, spelled); x.Outcome == ExchangeRefused {
		t.Fatalf("the stated base spelled in another case: refused %d %s, want carried", rec.Code, rec.Body.String())
	}
}

// A FHIR decimal beyond float64's range (1e400) is read, never taken for
// nothing: another Patient carrying one is still listed and strict refuses;
// in the leg's own Patient of the shared-number decision the decision is
// still refused 422 at every level.
func TestPASNotify_AHugeDecimalIsRead(t *testing.T) {
	sys := newSubStore(t)
	p2 := `{"fullUrl":"` + decisionBase + `/Patient/p2","resource":{"resourceType":"Patient","id":"p2","birthDate":"1990-05-06","name":[{"family":"Roe"}],"extension":[{"url":"http://example.org/x","valueDecimal":1e400}]}}`
	task := `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`
	expectListed(t, sys, contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"), p2, task)), 1)
	sn := sharedNumberDecision(`{"resourceType":"Patient","id":"p1","extension":[{"url":"http://example.org/x","valueDecimal":1e400}]}`)
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, sn); rec.Code != http.StatusUnprocessableEntity || x.Rule != RefusalAuthority {
			t.Fatalf("%s: %d %s on %q, want 422 on authority", level, rec.Code, rec.Body.String(), x.Rule)
		}
	}
}

// The leg's own Patient's demographics are its own, never those of a Patient
// contained in it: own without demographics and a contained Roe binds the
// member alone; own Doe and a contained Roe binds Doe. Each lists Roe as its
// own identity; strict refuses.
func TestPASNotify_AContainedPatientIsAnother(t *testing.T) {
	sys := newSubStore(t)
	none := newPASSubPayer(t, EnforcementNone, sys, nil).g
	memberOnly, _, _ := none.resolveSubjectPCI(context.Background(), "p1", memberOnlyScope)
	doe, _, _ := none.resolveSubjectPCI(context.Background(), "p1", canonicalCopy([]byte(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`)))
	for name, c := range map[string]struct {
		own, leg string
	}{
		"own without demographics": {`{"resourceType":"Patient","id":"p1","contained":[` + roePatient + `]}`, memberOnly},
		"own Doe":                  {`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}],"contained":[` + roePatient + `]}`, doe},
	} {
		body := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), `{"fullUrl":"`+decisionBase+`/Patient/p1","resource":`+c.own+`}`))
		expectListed(t, sys, body, 1)
		if leg, _ := notifiedBinding(t, sys, body); leg != c.leg {
			t.Fatalf("%s: the leg bound to %q, want %q", name, leg, c.leg)
		}
	}
}

// A reference to the member's id at another server's base is another
// identity wherever it sits: a Task in a SubscriptionStatus parameter, a Task
// contained in a bare ClaimResponse decision, and, inside the leg's
// decision, a contained ClaimResponse or a Task. Each is listed and strict
// refuses.
func TestPASNotify_AnotherBasesReferenceIsListed(t *testing.T) {
	sys := newSubStore(t)
	other := "https://other.example/fhir/Patient/p1"
	task := `{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"` + other + `"}}`
	one := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))
	for name, body := range map[string][]byte{
		"a Task in a SubscriptionStatus parameter": withStatusParameter(one, `{"name":"x","resource":`+task+`}`),
		"a Task contained in a bare ClaimResponse": contentEvent(strings.Replace(bareCR("cr1", "Patient/p1", pasProviderNPI), `"resourceType":"ClaimResponse",`, `"resourceType":"ClaimResponse","contained":[`+task+`],`, 1)),
		"a ClaimResponse contained in the leg's decision": contentEvent(decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Basic/b1","resource":{"resourceType":"Basic","id":"b1","contained":[`+bareCR("cr2", other, pasProviderNPI)+`]}}`)),
		"a Task in the leg's decision": contentEvent(decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Task/t1","resource":`+strings.Replace(task, `"id":"t9"`, `"id":"t1"`, 1)+`}`)),
	} {
		t.Run(name, func(t *testing.T) { expectListed(t, sys, body, 1) })
	}
}

// A Patient with no id, which no member names, is named by its fullUrl, or by
// its place when it states none: a urn:uuid Patient a Task references, and
// one in a SubscriptionStatus parameter, are each listed, and strict refuses.
func TestPASNotify_AnIDlessPatientIsListed(t *testing.T) {
	sys := newSubStore(t)
	for name, body := range map[string][]byte{
		"a urn:uuid Patient a Task references": contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
			`{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000dd","resource":{"resourceType":"Patient","identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"p2"}],"birthDate":"1990-05-06","name":[{"family":"Roe"}]}}`,
			`{"fullUrl":"`+decisionBase+`/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"urn:uuid:a0000000-0000-4000-8000-0000000000dd"}}}`)),
		"in a SubscriptionStatus parameter": withStatusParameter(contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"))),
			`{"name":"x","resource":{"resourceType":"Patient","birthDate":"1990-05-06","name":[{"family":"Roe"}]}}`),
	} {
		t.Run(name, func(t *testing.T) { expectListed(t, sys, body, 1) })
	}
}

// The shared-number read covers the whole notification, as a request leg's
// covers the whole request: with the leg bound from its member alone, a
// Coverage whose contained party carries the member's number, in a
// SubscriptionStatus parameter or in another decision, refuses 422 at every
// level.
func TestPASNotify_TheSharedNumberReadIsWhole(t *testing.T) {
	sys := newSubStore(t)
	cov := `{"resourceType":"Coverage","id":"c1","status":"active","contained":[{"resourceType":"Patient","id":"sub","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"p1"}],"birthDate":"1960-01-01","name":[{"family":"Parent"}]}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#sub"}}`
	leg := decision("collection", withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)))
	for name, body := range map[string][]byte{
		"a SubscriptionStatus parameter": withStatusParameter(contentEvent(leg), `{"name":"x","resource":`+cov+`}`),
		"another decision": contentEvents(decisionEntry(leg), decisionEntry(decision("collection", withRequest(decisionCR("cr2", "Patient/p1", pasProviderNPI)),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":`+cov+`}`))),
	} {
		for _, level := range allLevels {
			if rec, x := sendNotified(t, sys, level, body); rec.Code != http.StatusUnprocessableEntity || x.Rule != RefusalAuthority || !strings.Contains(rec.Body.String(), "member number is shared") {
				t.Fatalf("%s, %s: %d %s on %q, want 422 on authority, the number shared", name, level, rec.Code, rec.Body.String(), x.Rule)
			}
		}
	}
}

// atStatusURL is body with its SubscriptionStatus's entry at fullUrl.
func atStatusURL(fullURL string, body []byte) []byte {
	return []byte(strings.Replace(string(body), `"fullUrl":"urn:uuid:0b6a1f8e-2c3d-4e5f-8a9b-0c1d2e3f4a5b"`, `"fullUrl":"`+fullURL+`"`, 1))
}

// A relative patient reference resolves by FHIR R4's Bundle rule, through the
// shared resolver: in an entry with a RESTful fullUrl, against that entry's
// base; in an entry whose fullUrl is a URN, or outside any entry, against
// the payer's stated base. It is the leg's own patient exactly when it
// resolves to the leg's patient's identity (under any name of the leg's
// Patient, at that base). A Task for Patient/MBR-9 (the leg's Patient's
// member identifier) in a SubscriptionStatus parameter, and the Bundle's
// extension naming it:
//   - the status entry at the decision's base: the leg's own;
//   - the status entry a URN: the stated base's, the leg's own when the
//     decision is at the stated base, another identity otherwise;
//   - outside any entry: the stated base's, likewise.
func TestPASNotify_RelativeReferencesResolveByBundleRules(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	at := func(base string) []byte {
		cr := strings.Replace(decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionBase, base, 1)
		patient := `{"fullUrl":"` + base + `/Patient/p1","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-9"}],"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
		return contentEvent(decision("collection", cr, patient))
	}
	task := `{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/MBR-9"}}}`
	inStatus := func(body []byte) []byte { return withStatusParameter(body, task) }
	inRoot := func(body []byte) []byte {
		return []byte(strings.Replace(string(body), `{"resourceType":"Bundle","type":"history",`, `{"resourceType":"Bundle","type":"history","extension":[{"url":"http://example.org/x","valueReference":{"reference":"Patient/MBR-9"}}],`, 1))
	}
	for name, c := range map[string]struct {
		body []byte
		legs bool // the reference is the leg's own patient
	}{
		"the status entry at the decision's base":                 {inStatus(atStatusURL(decisionBase+"/Parameters/st1", at(decisionBase))), true},
		"the status entry a URN, the decision at the stated base": {inStatus(at(stated)), true},
		"the status entry a URN, the decision at another base":    {inStatus(at(decisionBase)), false},
		"outside any entry, the decision at the stated base":      {inRoot(at(stated)), true},
		"outside any entry, the decision at another base":         {inRoot(at(decisionBase)), false},
	} {
		t.Run(name, func(t *testing.T) {
			var others int
			for _, m := range mustMembers(t)(pasNotifiedMembers(c.body, stated)) {
				if !m.subject {
					others++
				}
			}
			rec, x := sendNotified(t, sys, EnforcementStrict, c.body)
			if c.legs {
				if others != 0 || x.Outcome == ExchangeRefused {
					t.Fatalf("%d other identities, strict %d %s; want the leg's own, carried", others, rec.Code, rec.Body.String())
				}
				return
			}
			if others != 1 || rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
				t.Fatalf("%d other identities, strict %d %s; want another identity, 422 (patient.mixed)", others, rec.Code, rec.Body.String())
			}
		})
	}
}

// A Coverage's party is no member, but a Patient carried as one and named as
// another resource's patient (a Task for it) is: the Task's reference names
// it, whether the party is a Patient entry the Coverage's subscriber names
// or one contained in the Coverage. Each is listed, and strict refuses.
func TestPASNotify_APartyNamedElsewhereIsListed(t *testing.T) {
	sys := newSubStore(t)
	task := `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	for name, body := range map[string][]byte{
		"a party entry": contentEvent(decision("collection", cr, decisionPatient("p1"), strings.Replace(decisionPatient("p2"), "Doe", "Parent", 1),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/p2"}}}`, task)),
		"a party contained in the Coverage": contentEvent(decision("collection", cr, decisionPatient("p1"),
			`{"fullUrl":"`+decisionBase+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","contained":[{"resourceType":"Patient","id":"p2","birthDate":"1960-01-01","name":[{"family":"Parent"}]}],"beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"#p2"}}}`, task)),
	} {
		t.Run(name, func(t *testing.T) { expectListed(t, sys, body, 1) })
	}
}

// The payer's gateway hands its involved pass the stated base its binding
// read the notification at (withNotifiedBase): with the leg's decision at the
// stated base and a bare ClaimResponse naming Patient/p1 relatively, the
// involved list names no other patient, and records none left out.
func TestPASNotify_TheInvolvedPassReadsTheStatedBase(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	cr := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, stated, 1)
	body := contentEvents(decisionEntry(decision("collection", cr, strings.Replace(decisionPatient("p1"), decisionBase, stated, 1))),
		decisionEntry(bareCR("cr2", "Patient/p1", pasProviderNPI)))
	for _, level := range allLevels {
		_, x := sendNotified(t, sys, level, body, func(c *Config) { c.HubAcceptsInvolved = true })
		if len(x.InvolvedOmitted) != 0 {
			t.Fatalf("%s: involved omitted %v, want no other patient", level, x.InvolvedOmitted)
		}
	}
}

// Inside a decision's graph every reference resolves by the graph's own
// rule, the PAS answer's, as the leg's patient is bound, whatever base the
// payer's gateway states: each is one patient, carried at strict:
//   - P17: a bare ClaimResponse naming Patient/p1, in a notification entry at
//     a RESTful base other than the stated one;
//   - P1: a ClaimResponse in a URN entry, its Patient at another RESTful base;
//   - P1b: the same, its Patient at the decision's base;
//   - P13: a Task in a URN entry, for Patient/p1, in the leg's decision;
//   - P15: a second decision, its ClaimResponse in a URN entry, naming the
//     same patient.
//
// Where the graph resolves nothing, the stated base decides (the control: a
// bare ClaimResponse beside a decision at another base is two patients).
func TestPASNotify_InsideAGraphItsRuleResolves(t *testing.T) {
	sys := newSubStore(t)
	cr := withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI))
	urnCR := func(id string) string {
		return `{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-00000000000` + id[len(id)-1:] + `","resource":` + withRequest(bareCR(id, "Patient/p1", pasProviderNPI)) + `}`
	}
	for name, body := range map[string][]byte{
		"P17": contentEvents(`{"fullUrl":"https://payer.example/fhir/ClaimResponse/cr1","request":{"method":"PUT"},"resource":` + withRequest(bareCR("cr1", "Patient/p1", pasProviderNPI)) + `}`),
		"P1":  contentEvent(decision("collection", urnCR("cr1"), otherBasePatient)),
		"P1b": contentEvent(decision("collection", urnCR("cr1"), decisionPatient("p1"))),
		"P13": contentEvent(decision("collection", cr, decisionPatient("p1"),
			`{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000ee","resource":{"resourceType":"Task","status":"requested","intent":"order","for":{"reference":"Patient/p1"}}}`)),
		"P15": contentEvents(decisionEntry(decision("collection", cr, decisionPatient("p1"))), decisionEntry(decision("collection", urnCR("cr2"), decisionPatient("p1")))),
	} {
		t.Run(name, func(t *testing.T) {
			for _, m := range mustMembers(t)(pasNotifiedMembers(body, sys.srv.URL+"/fhir")) {
				if !m.subject {
					t.Fatalf("another identity %s from %s, want one patient", m.member, m.scope)
				}
			}
			for _, level := range allLevels {
				if rec, x := sendNotified(t, sys, level, body); x.Outcome == ExchangeRefused && rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), "patient.mixed") {
					t.Fatalf("%s: refused %d %s, want one patient", level, rec.Code, rec.Body.String())
				}
			}
			if rec, x := sendNotified(t, sys, EnforcementStrict, body); x.Outcome == ExchangeRefused {
				t.Fatalf("strict: refused %d %s, want carried", rec.Code, rec.Body.String())
			}
		})
	}
	control := contentEvents(decisionEntry(decision("collection", cr, decisionPatient("p1"))), decisionEntry(bareCR("cr2", "Patient/p1", pasProviderNPI)))
	if rec, _ := sendNotified(t, sys, EnforcementStrict, control); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), errPASNotifyMixed) {
		t.Fatalf("control: %d %s, want 422 (patient.mixed): the graph resolves nothing, the stated base decides", rec.Code, rec.Body.String())
	}
}

// A Bundle entry that states no fullUrl resolves against the stated base,
// never an outer entry's: a Task for Patient/MBR-9 (the leg's Patient's
// member identifier) in a decision entry with no fullUrl, the decision's own
// entry at the decision's base, names another identity than the leg's.
func TestPASNotify_AnEntryWithNoFullURLResolvesByTheStatedBase(t *testing.T) {
	patient := `{"fullUrl":"` + decisionBase + `/Patient/p1","resource":{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBR-9"}],"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
	d := decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), patient,
		`{"resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/MBR-9"}}}`)
	body := contentEvents(`{"fullUrl":"` + decisionBase + `/Bundle/d1","request":{"method":"PUT"},"resource":` + d + `}`)
	var others int
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, "https://stated.example/fhir")) {
		if !m.subject {
			others++
		}
	}
	if others != 1 {
		t.Fatalf("%d other identities, want the Task's, resolved against the stated base", others)
	}
}

// countOthers is the number of identities body's involved list names beside
// the leg's own, read at the fixtures' stated base.
func countOthers(t *testing.T, sys *subStore, body []byte) int {
	t.Helper()
	n := 0
	for _, m := range mustMembers(t)(pasNotifiedMembers(body, sys.srv.URL+"/fhir")) {
		if !m.subject {
			n++
		}
	}
	return n
}

// A reference is skipped as already named only when the very Patient it
// resolves to is: a Task for Patient/p2, p2 a Coverage's party (Parent), is
// listed though another Patient p2 (Other) in a SubscriptionStatus parameter
// is listed too; and a Task in another decision for that decision's party
// Patient p1 is listed though the leg's own Patient is p1. Strict refuses
// each.
func TestPASNotify_AReferenceIsSkippedOnlyForItsOwnPatient(t *testing.T) {
	sys := newSubStore(t)
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	cov := `{"fullUrl":"` + decisionBase + `/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/p1"},"subscriber":{"reference":"Patient/p2"}}}`
	parent := strings.Replace(strings.Replace(decisionPatient("p2"), "Doe", "Parent", 1), "1980-01-01", "1960-01-01", 1)
	task := `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p2"}}}`
	other := `{"name":"x","resource":{"resourceType":"Patient","id":"p2","birthDate":"1990-02-02","name":[{"family":"Other"}]}}`
	f1a := withStatusParameter(contentEvent(decision("collection", cr, decisionPatient("p1"), parent, cov, task)), other)
	ob := "https://other.example/fhir"
	second := decision("collection", withRequest(strings.Replace(decisionCR("cr2", decisionBase+"/Patient/p1", pasProviderNPI), decisionBase+"/ClaimResponse/cr2", ob+"/ClaimResponse/cr2", 1)),
		`{"fullUrl":"`+ob+`/Patient/p1","resource":{"resourceType":"Patient","id":"p1","birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`,
		`{"fullUrl":"`+ob+`/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"`+decisionBase+`/Patient/p1"},"subscriber":{"reference":"Patient/p1"}}}`,
		`{"fullUrl":"`+ob+`/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p1"}}}`)
	variant := contentEvents(decisionEntry(decision("collection", cr, decisionPatient("p1"))), decisionEntry(second))
	for name, c := range map[string]struct {
		body []byte
		want int
	}{"F1a": {f1a, 2}, "the leg's own and another decision's party": {variant, 1}} {
		t.Run(name, func(t *testing.T) { expectListed(t, sys, c.body, c.want) })
	}
}

// One Patient is one person: a Patient p2 carrying the member identifier
// MBR-77, and a Task for Patient/MBR-77, is one other identity, bound once
// from that Patient; strict refuses it as one other patient. A reference
// under MBR-77 where no carried Patient holds that identifier is an identity
// of its own, listed beside p2.
func TestPASNotify_OnePatientIsOnePerson(t *testing.T) {
	sys := newSubStore(t)
	cr := decisionCR("cr1", "Patient/p1", pasProviderNPI)
	taskM := `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/MBR-77"}}}`
	p2 := func(identifier string) string {
		return `{"fullUrl":"` + decisionBase + `/Patient/p2","resource":{"resourceType":"Patient","id":"p2",` + identifier + `"birthDate":"1990-02-02","name":[{"family":"Roe"}]}}`
	}
	held := contentEvent(decision("collection", cr, decisionPatient("p1"), p2(`"identifier":[{"system":"`+shnsdk.MemberSystem+`","value":"MBR-77"}],`), taskM))
	expectListed(t, sys, held, 1)
	notHeld := contentEvent(decision("collection", cr, decisionPatient("p1"), p2(""), taskM))
	expectListed(t, sys, notHeld, 2)
}

// The notification's own members resolve against the Bundle entry they sit
// in, as the walk reads entries: a later entry with no resource, at another
// server's RESTful fullUrl, whose extension names Patient/p1, names that
// server's p1, another identity (strict refuses); an object that is no
// Bundle entry (a SubscriptionStatus parameter with a resource and a fullUrl)
// takes no base, so its Patient/p1 resolves against the stated base, the
// leg's own (strict carries).
func TestPASNotify_OwnMembersResolveByTheirEntry(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	crS := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, stated, 1)
	dS := decisionEntry(decision("collection", crS, strings.Replace(decisionPatient("p1"), decisionBase, stated, 1)))
	noRes := `{"fullUrl":"https://other.example/fhir/Basic/b1","request":{"method":"DELETE","url":"Basic/b1"},"extension":[{"url":"http://example.org/x","valueReference":{"reference":"Patient/p1"}}]}`
	o1 := contentEvents(dS, noRes)
	if n := countOthers(t, sys, o1); n != 1 {
		t.Fatalf("O1: %d other identities, want that server's p1", n)
	}
	if rec, _ := sendNotified(t, sys, EnforcementStrict, o1); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("O1 strict: %d %s, want 422", rec.Code, rec.Body.String())
	}
	mirror := withStatusParameter(contentEvents(dS), `{"name":"x","fullUrl":"https://other.example/fhir/Basic/b1","resource":{"resourceType":"Basic","id":"b1"},"valueReference":{"reference":"Patient/p1"}}`)
	if n := countOthers(t, sys, mirror); n != 0 {
		t.Fatalf("mirror: %d other identities, want the leg's own", n)
	}
	if rec, x := sendNotified(t, sys, EnforcementStrict, mirror); x.Outcome == ExchangeRefused {
		t.Fatalf("mirror strict: refused %d %s, want carried", rec.Code, rec.Body.String())
	}
}

// A reference names the Patient at its exact identity first, whatever order
// the entries come in, and only then one read under the name it states; a
// name two Patients answer to names neither:
//   - CR for Patient/a, whose Patient carries the member identifier MBRX, a
//     Patient at .../Patient/MBRX (Parent, a Coverage's party) and a Task for
//     Patient/MBRX: the Task names Parent exactly, listed, in either order;
//   - two Patients (the leg's and a party) both carrying MBRY, and a Task for
//     Patient/MBRY: the name matches neither, an identity of its own;
//   - another Patient carrying MBRX (c) before Parent: the Task still names
//     Parent exactly, listed; c, carrying the number the Coverage's
//     subscriber names, is that Coverage's party by the shared rule's reading
//     (visitCarriedPatients), so it is no identity itself.
//
// Strict refuses each.
func TestPASNotify_AnExactIdentityWinsOverAName(t *testing.T) {
	sys := newSubStore(t)
	cr := decisionCR("cr1", "Patient/a", pasProviderNPI)
	ownWith := func(id string) string {
		return `{"fullUrl":"` + decisionBase + `/Patient/a","resource":{"resourceType":"Patient","id":"a","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + id + `"}],"birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
	}
	ownPlain := `{"fullUrl":"` + decisionBase + `/Patient/a","resource":{"resourceType":"Patient","id":"a","birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
	parent := `{"fullUrl":"` + decisionBase + `/Patient/MBRX","resource":{"resourceType":"Patient","id":"MBRX","birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`
	cov := func(sub string) string {
		return `{"fullUrl":"` + decisionBase + `/Coverage/c1","resource":{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/a"},"subscriber":{"reference":"Patient/` + sub + `"}}}`
	}
	task := func(ref string) string {
		return `{"fullUrl":"` + decisionBase + `/Task/t9","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/` + ref + `"}}}`
	}
	partyB := `{"fullUrl":"` + decisionBase + `/Patient/b","resource":{"resourceType":"Patient","id":"b","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBRY"}],"birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`
	c := `{"fullUrl":"` + decisionBase + `/Patient/c","resource":{"resourceType":"Patient","id":"c","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"MBRX"}],"birthDate":"1970-01-01","name":[{"family":"Cee"}]}}`
	for name, row := range map[string]struct {
		body []byte
		want int
	}{
		"MBRX, the leg's Patient first": {contentEvent(decision("collection", cr, ownWith("MBRX"), parent, cov("MBRX"), task("MBRX"))), 1},
		"MBRX, Parent first":            {contentEvent(decision("collection", cr, parent, ownWith("MBRX"), cov("MBRX"), task("MBRX"))), 1},
		"MBRY shared, the leg's first":  {contentEvent(decision("collection", cr, ownWith("MBRY"), partyB, cov("b"), task("MBRY"))), 1},
		"MBRY shared, the party first":  {contentEvent(decision("collection", cr, partyB, ownWith("MBRY"), cov("b"), task("MBRY"))), 1},
		// Two other Patients carrying MBRZ: the name names neither, so the
		// Task's reference is an identity of its own beside the two.
		"MBRZ shared by two others": {contentEvent(decision("collection", cr, ownPlain,
			strings.Replace(strings.Replace(strings.Replace(c, "MBRX", "MBRZ", 1), "/Patient/c", "/Patient/x", 1), `"id":"c"`, `"id":"x"`, 1),
			strings.Replace(strings.Replace(strings.Replace(c, "MBRX", "MBRZ", 1), "/Patient/c", "/Patient/y", 1), `"id":"c"`, `"id":"y"`, 1), task("MBRZ"))), 3},
		"another MBRX first":           {contentEvent(decision("collection", cr, ownPlain, c, parent, cov("MBRX"), task("MBRX"))), 1},
		"Parent before the other MBRX": {contentEvent(decision("collection", cr, ownPlain, parent, c, cov("MBRX"), task("MBRX"))), 1},
	} {
		t.Run(name, func(t *testing.T) { expectListed(t, sys, row.body, row.want) })
	}
}

// A urn resolves as FHIR R4's Bundle rule has it: among the entries of the
// Bundle the referencing resource sits in, then outward through each Bundle
// enclosing it, never into a sibling Bundle. A party Patient z at urn:1, a
// Coverage subscriber urn:1 and a Task for urn:1, in one Bundle in a
// SubscriptionStatus parameter or in the decision: z is listed (strict
// refuses). The Task in a Bundle nested in the one carrying z: z is listed.
// The Task in a sibling Bundle of z's: the urn names nothing in its scope,
// unresolved, and z is not listed (strict carries).
func TestPASNotify_AURNResolvesInItsBundleThenOutward(t *testing.T) {
	sys := newSubStore(t)
	const z = "urn:uuid:11111111-1111-4111-8111-111111111111"
	patientZ := `{"fullUrl":"` + z + `","resource":{"resourceType":"Patient","id":"z","birthDate":"1960-01-01","name":[{"family":"Parent"}]}}`
	coverage := `{"fullUrl":"urn:uuid:22222222-2222-4222-8222-222222222222","resource":{"resourceType":"Coverage","status":"active","subscriber":{"reference":"` + z + `"}}}`
	task := `{"fullUrl":"urn:uuid:33333333-3333-4333-8333-333333333333","resource":{"resourceType":"Task","status":"requested","intent":"order","for":{"reference":"` + z + `"}}}`
	bundle := func(id string, entries ...string) string {
		return `{"resourceType":"Bundle",` + id + `"type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
	}
	nestedEntry := func(urn, b string) string { return `{"fullUrl":"` + urn + `","resource":` + b + `}` }
	cr := decisionCR("cr1", "Patient/a", pasProviderNPI)
	own := `{"fullUrl":"` + decisionBase + `/Patient/a","resource":{"resourceType":"Patient","id":"a","birthDate":"1980-01-01","name":[{"family":"Doe"}]}}`
	one := contentEvent(decision("collection", cr, own))
	inParam := func(b string) []byte { return withStatusParameter(one, `{"name":"x","resource":`+b+`}`) }
	own_ := bundle("", patientZ, coverage, task)
	expectListed(t, sys, inParam(own_), 1)
	expectListed(t, sys, contentEvent(decision("collection", cr, own, `{"fullUrl":"`+decisionBase+`/Bundle/n1","resource":`+bundle(`"id":"n1",`, patientZ, coverage, task)+`}`)), 1)
	enclosing := bundle("", patientZ, coverage, nestedEntry("urn:uuid:44444444-4444-4444-8444-444444444444", bundle("", task)))
	expectListed(t, sys, inParam(enclosing), 1)
	sibling := bundle("", nestedEntry("urn:uuid:55555555-5555-4555-8555-555555555555", bundle("", task)),
		nestedEntry("urn:uuid:66666666-6666-4666-8666-666666666666", bundle("", patientZ, coverage)))
	if n := countOthers(t, sys, inParam(sibling)); n != 0 {
		t.Fatalf("a sibling Bundle's urn: %d other identities, want none (unresolved)", n)
	}
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, inParam(sibling)); x.Outcome == ExchangeRefused {
			t.Fatalf("a sibling Bundle's urn, %s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
		}
	}
}

// A bare ClaimResponse is its notification entry's resource: a relative
// reference in it resolves against that entry's fullUrl when it is the
// ClaimResponse's RESTful identity (ending in /ClaimResponse/<its own id>),
// and against the stated base otherwise (a urn, no fullUrl, or one naming
// another id).
//   - the leg's decision at the stated base with its Patient p1, and a bare
//     ClaimResponse for Patient/p1 in an entry at another server's base:
//     another patient, listed, refused at strict;
//   - the same bare ClaimResponse in an entry at the stated base, under a
//     urn, or with no fullUrl: the leg's own, carried at strict;
//   - the leg itself a bare ClaimResponse at another server's base, and a
//     SubscriptionStatus Task for Patient/p1: two patients.
func TestPASNotify_ABareClaimResponseResolvesByItsEntry(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	cr := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, stated, 1)
	leg := decisionEntry(decision("collection", cr, strings.Replace(decisionPatient("p1"), decisionBase, stated, 1)))
	bareAt := func(full string) string {
		if full == "" {
			return decisionEntry(withRequest(bareCR("cr2", "Patient/p1", pasProviderNPI)))
		}
		return `{"fullUrl":"` + full + `","request":{"method":"PUT"},"resource":` + withRequest(bareCR("cr2", "Patient/p1", pasProviderNPI)) + `}`
	}
	expectListed(t, sys, contentEvents(leg, bareAt("https://other.example/fhir/ClaimResponse/cr2")), 1)
	for name, full := range map[string]string{"the stated base": stated + "/ClaimResponse/cr2", "a urn": "urn:uuid:a0000000-0000-4000-8000-0000000000c2", "no fullUrl": ""} {
		body := contentEvents(leg, bareAt(full))
		if n := countOthers(t, sys, body); n != 0 {
			t.Fatalf("%s: %d other identities, want the leg's own", name, n)
		}
		for _, level := range allLevels {
			if rec, x := sendNotified(t, sys, level, body); x.Outcome == ExchangeRefused {
				t.Fatalf("%s, %s: refused %d %s, want carried", name, level, rec.Code, rec.Body.String())
			}
		}
	}
	legBare := withStatusParameter(contentEvents(`{"fullUrl":"https://other.example/fhir/ClaimResponse/cr1","request":{"method":"PUT"},"resource":`+withRequest(bareCR("cr1", "Patient/p1", pasProviderNPI))+`}`),
		`{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p1"}}}`)
	expectListed(t, sys, legBare, 1)
	// The same leg under a fullUrl ending in another id than its own: no base
	// it resolves against, so Patient/p1 is at the stated base, and the Task
	// names the leg's own.
	otherID := withStatusParameter(contentEvents(`{"fullUrl":"https://other.example/fhir/ClaimResponse/cr1","request":{"method":"PUT"},"resource":`+withRequest(bareCR("cr9", "Patient/p1", pasProviderNPI))+`}`),
		`{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"Patient/p1"}}}`)
	if n := countOthers(t, sys, otherID); n != 0 {
		t.Fatalf("another id's fullUrl: %d other identities, want one person", n)
	}
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, otherID); x.Outcome == ExchangeRefused {
			t.Fatalf("another id's fullUrl, %s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
		}
	}
}

// A Patient written under a urn is at the stated base (nameBaseOf), so when
// the leg's ClaimResponse's patient reference resolves at the stated base to
// Patient/<id> and exactly one urn-written Patient of its decision has that
// id (its resourceType and id read under case folding, as the walk reads
// them), that Patient is the leg's own, in the binding as in the involved
// pass: the leg is bound from its demographics and it is one person with the
// leg.
//   - a ClaimResponse at the stated base naming Patient/p1, and a urn Patient
//     p1 (Doe), spelled exactly or with its id as "Id": one person, bound
//     from Doe; none, observe and structural carry it; strict refuses it
//     (patient.mixed) by the subject rule a PAS answer is held to, at 2.1,
//     the line the decision is read at (pasAnswerSubjectMismatch: the urn
//     Patient is a Patient that is not the ClaimResponse's patient);
//   - the same urn Patient spelled "patient": one person, bound from Doe, and
//     carried at every level, strict included (the subject rule reads
//     resourceType exactly, so it finds no other Patient);
//   - a urn ClaimResponse naming Patient/p1 relatively: the same, for the
//     same reason; without a request identifier the graph rule
//     (answer.shape) refuses it at structural and strict, at every line;
//   - two urn Patients with id p1 (Doe, Roe), each spelled exactly or one of
//     them folded: neither is the leg's own (the shared-name rule), so the
//     leg binds its member alone and both are listed; strict refuses it,
//     every other level carries it;
//   - a ClaimResponse at another server's base naming Patient/p1, and a urn
//     Patient p1: the reference resolves elsewhere, so the urn Patient is not
//     the leg's own; the leg binds its member alone, the Patient is listed;
//   - a ClaimResponse at the stated base naming Patient/p1, and only another
//     server's Patient p1 (Roe, a RESTful fullUrl): it is no urn-written
//     Patient, so not the leg's own; the leg binds its member alone, Roe is
//     listed, strict refuses it.
func TestPASNotify_AURNPatientAtTheStatedBaseIsTheLegsOwn(t *testing.T) {
	sys := newSubStore(t)
	stated := sys.srv.URL + "/fhir"
	none := newPASSubPayer(t, EnforcementNone, sys, nil).g
	doe, _, err := none.resolveSubjectPCI(context.Background(), "p1", canonicalCopy([]byte(`{"resourceType":"Patient","id":"p1","birthDate":"1980-01-01","name":[{"family":"Doe"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	memberOnly, _, _ := none.resolveSubjectPCI(context.Background(), "p1", memberOnlyScope)
	spelled := func(n, typ, idName, family string) string {
		return `{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000e` + n + `","resource":{"resourceType":"` + typ + `","` + idName + `":"p1","birthDate":"1980-01-01","name":[{"family":"` + family + `"}]}}`
	}
	urnP := func(n, family string) string { return spelled(n, "Patient", "id", family) }
	atStated := strings.Replace(withRequest(decisionCR("cr1", "Patient/p1", pasProviderNPI)), decisionBase, stated, 1)
	urnCR := func(cr string) string {
		return `{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000e0","resource":` + cr + `}`
	}
	strictMixed := map[ConformanceEnforcement]string{EnforcementStrict: errPASNotifyMixed}
	const notTheCRs = "is a Patient that is not the ClaimResponse's patient"
	for name, c := range map[string]struct {
		body     []byte
		leg      string
		others   int
		refusing map[ConformanceEnforcement]string
		// subject is the subject rule's reason strict refuses it for, at
		// the line the decision is read at (2.1); "" when not asserted.
		subject string
	}{
		"at the stated base":   {contentEvent(decision("collection", atStated, urnP("1", "Doe"))), doe, 0, strictMixed, notTheCRs},
		"spelled as patient":   {contentEvent(decision("collection", atStated, spelled("1", "patient", "id", "Doe"))), doe, 0, nil, ""},
		"its id spelled as Id": {contentEvent(decision("collection", atStated, spelled("1", "Patient", "Id", "Doe"))), doe, 0, strictMixed, notTheCRs},
		"a urn ClaimResponse":  {contentEvent(decision("collection", urnCR(withRequest(bareCR("cr1", "Patient/p1", pasProviderNPI))), urnP("1", "Doe"))), doe, 0, strictMixed, notTheCRs},
		"a urn ClaimResponse with no request": {contentEvent(decision("collection", urnCR(bareCR("cr1", "Patient/p1", pasProviderNPI)), urnP("1", "Doe"))), doe, 0,
			map[ConformanceEnforcement]string{EnforcementStructural: errPASNotifyAnswerDefect, EnforcementStrict: errPASNotifyAnswerDefect}, ""},
		"two urn Patients p1":              {contentEvent(decision("collection", atStated, urnP("1", "Doe"), urnP("2", "Roe"))), memberOnly, 2, strictMixed, ""},
		"two, the other order":             {contentEvent(decision("collection", atStated, urnP("2", "Roe"), urnP("1", "Doe"))), memberOnly, 2, strictMixed, ""},
		"two, one spelled as patient":      {contentEvent(decision("collection", atStated, urnP("1", "Doe"), spelled("2", "patient", "id", "Roe"))), memberOnly, 2, strictMixed, ""},
		"two, one with its id as Id":       {contentEvent(decision("collection", atStated, urnP("1", "Doe"), spelled("2", "Patient", "Id", "Roe"))), memberOnly, 2, strictMixed, ""},
		"at another server's base":         {contentEvent(decision("collection", strings.Replace(atStated, stated, "https://other.example/fhir", 1), urnP("1", "Doe"))), memberOnly, 1, strictMixed, ""},
		"only another server's Patient p1": {contentEvent(decision("collection", atStated, otherBasePatient)), memberOnly, 1, strictMixed, ""},
	} {
		if n := countOthers(t, sys, c.body); n != c.others {
			t.Fatalf("%s: %d other identities, want %d", name, n, c.others)
		}
		if leg, _ := notifiedBinding(t, sys, c.body); leg != c.leg {
			t.Fatalf("%s: the leg bound to %q, want %q", name, leg, c.leg)
		}
		if c.subject != "" {
			n, rf := subscription.ParseNotification(c.body, pasTopic)
			if rf != nil {
				t.Fatal(rf)
			}
			p, nr := readPASNotified(n)
			if nr != nil || len(p.decisions) != 1 {
				t.Fatalf("%s: read %+v, want one decision", name, nr)
			}
			raw := p.decisions[0].raw
			line, err := pasgraph.ValidateNotified(raw, nil)
			if err != nil || line != "2.1" {
				t.Fatalf("%s: read at %q (%v), want 2.1", name, line, err)
			}
			if r := pasAnswerSubjectMismatch(raw, line); r == nil || !strings.Contains(r.Why, c.subject) {
				t.Fatalf("%s: the subject rule says %+v, want %q", name, r, c.subject)
			}
		}
		for _, level := range allLevels {
			t.Run(level.String()+"/"+name, func(t *testing.T) { expectAt(t, sys, level, c.body, c.refusing) })
		}
	}
}

// A urn-written Patient is at the payer's stated base: the notification came
// from that server, and a urn says only that its entry has no RESTful
// address. A reference to Patient/<its id> at the stated base names it,
// through the same matching as every Patient:
//   - (a) the urn decision and a SubscriptionStatus Task for Patient/p1: one
//     person, carried at strict;
//   - (b) the same id at another base: another identity, listed, refused;
//   - (c) a urn Patient with no id is reachable by its urn alone: a relative
//     reference names nothing, an identity of its own beside it.
func TestPASNotify_AURNPatientIsAtTheStatedBase(t *testing.T) {
	sys := newSubStore(t)
	taskFor := func(ref string) string {
		return `{"name":"x","resource":{"resourceType":"Task","id":"t9","status":"requested","intent":"order","for":{"reference":"` + ref + `"}}}`
	}
	a := withStatusParameter(contentEvent(urnDecision(urnPatient)), taskFor("Patient/p1"))
	if n := countOthers(t, sys, a); n != 0 {
		t.Fatalf("(a): %d other identities, want one person", n)
	}
	for _, level := range allLevels {
		if rec, x := sendNotified(t, sys, level, a); x.Outcome == ExchangeRefused {
			t.Fatalf("(a), %s: refused %d %s, want carried", level, rec.Code, rec.Body.String())
		}
	}
	expectListed(t, sys, withStatusParameter(contentEvent(urnDecision(urnPatient)), taskFor("https://other.example/fhir/Patient/p1")), 1)
	idless := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1"),
		`{"fullUrl":"urn:uuid:a0000000-0000-4000-8000-0000000000dd","resource":{"resourceType":"Patient","birthDate":"1990-05-06","name":[{"family":"Roe"}]}}`))
	expectListed(t, sys, withStatusParameter(idless, taskFor("Patient/zz9")), 2)
}

// A urn two Patients in one Bundle share names neither (the shared-name
// rule): its reference is an identity of its own under the urn itself, the
// same whichever Patient comes first.
func TestPASNotify_AURNTwoPatientsShareNamesNeither(t *testing.T) {
	sys := newSubStore(t)
	const u = "urn:uuid:77777777-7777-4777-8777-777777777777"
	pt := func(id, family string) string {
		return `{"fullUrl":"` + u + `","resource":{"resourceType":"Patient","id":"` + id + `","birthDate":"1960-01-01","name":[{"family":"` + family + `"}]}}`
	}
	task := `{"fullUrl":"urn:uuid:33333333-3333-4333-8333-333333333333","resource":{"resourceType":"Task","status":"requested","intent":"order","for":{"reference":"` + u + `"}}}`
	one := contentEvent(decision("collection", decisionCR("cr1", "Patient/p1", pasProviderNPI), decisionPatient("p1")))
	var got [][]string
	for _, order := range [][]string{{pt("z1", "One"), pt("z2", "Two")}, {pt("z2", "Two"), pt("z1", "One")}} {
		body := withStatusParameter(one, `{"name":"x","resource":{"resourceType":"Bundle","type":"collection","entry":[`+order[0]+`,`+order[1]+`,`+task+`]}}`)
		var members []string
		for _, m := range mustMembers(t)(pasNotifiedMembers(body, sys.srv.URL+"/fhir")) {
			if !m.subject {
				members = append(members, m.member)
			}
		}
		sort.Strings(members)
		got = append(got, members)
		if !slices.Contains(members, u) {
			t.Fatalf("members %v, want the urn itself among them", members)
		}
	}
	if !slices.Equal(got[0], got[1]) {
		t.Fatalf("members %v and %v by order, want the same", got[0], got[1])
	}
}
