//go:build !shn_coreonly

package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The CDS Hooks and $questionnaire-package ingresses resolve a Coverage.payor
// reference among the resources the request carries by the rule the PAS
// routes apply to a Bundle's entries (resolveAmong): by fullUrl, or a relative
// reference by type and id; several that answer route only when each is an
// Organization naming the same payer. Routing refuses at every level.

// refOrg is an Organization with id (none when "") naming payer identifier
// value (none when "").
func refOrg(id, value string) string {
	org := `{"resourceType":"Organization"`
	if id != "" {
		org += `,"id":"` + id + `"`
	}
	if value != "" {
		org += `,"identifier":[{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + value + `"}]`
	}
	return org + `}`
}

// refCoverage is a Coverage for patient whose payor is the reference ref.
func refCoverage(patient, ref string) string {
	return `{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + patient + `"},"payor":[{"reference":"` + ref + `"}]}`
}

// inlineCoverage is a Coverage for "example" naming payer identifier value
// inline.
func inlineCoverage(id, value string) string {
	return `{"resourceType":"Coverage","id":"` + id + `","status":"active","beneficiary":{"reference":"Patient/example"},"payor":[{"identifier":{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + value + `"}}]}`
}

// refCollection is a collection Bundle holding entries, each a whole entry
// object.
func refCollection(entries ...string) string {
	return `{"resourceType":"Bundle","type":"collection","entry":[` + strings.Join(entries, ",") + `]}`
}

// refEntry is a Bundle entry holding res at fullURL ("" for none).
func refEntry(fullURL, res string) string {
	if fullURL == "" {
		return `{"resource":` + res + `}`
	}
	return `{"fullUrl":"` + fullURL + `","resource":` + res + `}`
}

// crdPayorRequest is an EHR order-select request whose prefetch carries
// coverage under "coverage" and every other value in extra (JSON members).
func crdPayorRequest(coverage, extra string) []byte {
	p := patientOnly + `,"coverage":` + coverage + `,"serviceHistory":null,"deviceHistory":null,"medicationHistory":null,"questionnaireResponses":null`
	if extra != "" {
		p += "," + extra
	}
	return ehrRequest(p)
}

// twoRegisteredPayers is a payer directory registering the CMS payer id
// 00001 to the harness's "payer" holder and 00002 to another payer.
func twoRegisteredPayers(t *testing.T) PayerRouter {
	t.Helper()
	r, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: shnsdk.CMSPayerIdentity.System, Value: "00001", HolderID: "payer"},
		{System: shnsdk.CMSPayerIdentity.System, Value: "00002", HolderID: "payer-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// orgWithIDs is an Organization with id (none when empty) listing ids, each
// a [system, value] pair, in order.
func orgWithIDs(id string, ids ...[2]string) string {
	org := `{"resourceType":"Organization"`
	if id != "" {
		org += `,"id":"` + id + `"`
	}
	org += `,"identifier":[`
	for i, p := range ids {
		if i > 0 {
			org += ","
		}
		org += `{"system":"` + p[0] + `","value":"` + p[1] + `"}`
	}
	return org + `]}`
}

var (
	npiOf   = [2]string{"http://hl7.org/fhir/sid/us-npi", "1234567893"}
	einOf   = [2]string{"http://terminology.hl7.org/NamingSystem/USEIN", "7239994834"}
	cmsOf   = func(v string) [2]string { return [2]string{shnsdk.CMSPayerIdentity.System, v} }
	payer01 = cmsOf("00001")
)

const (
	payorAbs            = "https://ehr.example/fhir/Organization/pay-1"
	payorUUID           = "urn:uuid:3f6c1a8e-2b4d-4e7f-9a10-5c8d7e6f4a21"
	payorDisagree       = noPayerIdentifier + ": " + payorDisagreesInRequest
	payorDisagreeSearch = noPayerIdentifier + ": " + payorDisagreesWithSearch
)

func TestCRDIngressPayorReference_ResolvesAmongPrefetch(t *testing.T) {
	t.Parallel()
	rows := map[string][]byte{
		"an absolute reference equal to a prefetch Bundle entry's fullUrl": crdPayorRequest(refCoverage("example", payorAbs),
			`"organizations":`+refCollection(refEntry(payorAbs, refOrg("pay-1", "00001")))),
		"a urn:uuid reference equal to a prefetch Bundle entry's fullUrl": crdPayorRequest(refCoverage("example", payorUUID),
			`"organizations":`+refCollection(refEntry(payorUUID, refOrg("", "00001")))),
		"the same Organization under two prefetch keys": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry("", refOrg("pay-1", "00001")))),
		"two resources naming the same payer": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry(payorAbs, `{"resourceType":"Organization","id":"pay-1","name":"Payer","identifier":[{"system":"`+shnsdk.CMSPayerIdentity.System+`","value":"00001"}]}`))),
		"a relative reference to a resource value": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")),
		"a coverage Bundle whose own entry and another value name the same payer": crdPayorRequest(
			refCollection(refEntry("", refCoverage("example", "Organization/pay-1")), refEntry("", refOrg("pay-1", "00001"))),
			`"payer":`+refOrg("pay-1", "00001")),
		// Several entries answering one reference name every identifier
		// they name, and the directory decides: identifiers no participant
		// registers are passed over.
		"two resources, one naming an identifier no participant registers": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry("", refOrg("pay-1", "00002")))),
		"two resources, each naming the payer beside an unregistered identifier": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+orgWithIDs("pay-1", npiOf, payer01)+`,"organizations":`+refCollection(refEntry("", orgWithIDs("pay-1", payer01, einOf)))),
		"one of two answering resources naming no identifier": crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry("", refOrg("pay-1", "")))),
		// An inline identifier routes as before, whatever the request's
		// Organizations say.
		"an inline payor identifier beside disagreeing Organizations": crdPayorRequest(
			`{"resourceType":"Coverage","id":"c1","beneficiary":{"reference":"Patient/example"},"payor":[{"reference":"Organization/pay-1","identifier":{"system":"`+shnsdk.CMSPayerIdentity.System+`","value":"00001"}}]}`,
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry("", refOrg("pay-1", "00002")))),
	}
	for name, body := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				env, rec, _ := levelIngressRowWith(t, s, level, body, false)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				// The prefetch is carried exactly (fhirServer and
				// fhirAuthorization are the EHR's addressing and authority,
				// never carried).
				prefetch := body[bytes.Index(body, []byte(`"prefetch"`)):]
				if !bytes.HasSuffix(sentRequest(t, env), prefetch) {
					t.Fatalf("the EHR's prefetch changed:\n%s", sentRequest(t, env))
				}
				if _, read := s.calls(); len(read) != 0 {
					t.Fatalf("the system of record was read for a payor the request carries: %v", read)
				}
			})
		}
	}
}

// Resources of the request that answer the payor reference name every
// identifier they name: when those name two registered payers, the request is
// refused as ambiguous before the network, at every level; several that are
// not all Organizations are refused as such. No other source is asked to pick
// a payer.
func TestCRDIngressPayorReference_DisagreementRefused(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		body []byte
		msg  string
	}{
		"two resources naming two registered payers": {crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+refOrg("pay-1", "00001")+`,"organizations":`+refCollection(refEntry("", refOrg("pay-1", "00002")))), msgCoverageNamesTwoPayers},
		"two resources listing an NPI first, then two registered payers": {crdPayorRequest(refCoverage("example", "Organization/pay-1"),
			`"payer":`+orgWithIDs("pay-1", npiOf, payer01)+`,"organizations":`+refCollection(refEntry("", orgWithIDs("pay-1", npiOf, einOf, cmsOf("00002"))))), msgCoverageNamesTwoPayers},
		"an absolute reference entries of two values answer with two registered payers": {crdPayorRequest(refCoverage("example", payorUUID),
			`"organizations":`+refCollection(refEntry(payorUUID, refOrg("", "00001")))+`,"payers":`+refCollection(refEntry(payorUUID, refOrg("", "00002")))), msgCoverageNamesTwoPayers},
		"an Organization and a resource that is not one": {crdPayorRequest(refCoverage("example", payorUUID),
			`"organizations":`+refCollection(refEntry(payorUUID, refOrg("", "00001")))+`,"practitioners":`+refCollection(refEntry(payorUUID, `{"resourceType":"Practitioner","id":"p9"}`))), payorDisagree},
		// Routing reads a coverage Bundle's own entries first; another value
		// naming another registered payer leaves the payer in doubt all the
		// same.
		"a coverage Bundle's own entry and another value naming another registered payer": {crdPayorRequest(
			refCollection(refEntry("", refCoverage("example", "Organization/pay-1")), refEntry("", refOrg("pay-1", "00001"))),
			`"payer":`+refOrg("pay-1", "00002")), msgCoverageNamesTwoPayers},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				s.reads["Organization/pay-1"] = []byte(refOrg("pay-1", "00001"))
				env, rec, _ := levelIngressRowRouted(t, s, level, row.body, false, twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, row.msg)
				if env.routeHitCount() != 0 {
					t.Fatal("a refused request reached the Hub")
				}
				if _, read := s.calls(); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
		}
	}
}

// A coverage Bundle's own entries are read by their exact member names, and
// every payor reference resolves by the agreement rule: a member named in
// another case never lets routing take an entry the rule did not check.
func TestCRDIngressPayorReference_MemberNamesInAnotherCase(t *testing.T) {
	t.Parallel()
	inBundle := func(cov, org string) string {
		return refCollection(refEntry("", cov), refEntry("", org))
	}
	rows := map[string]struct {
		body []byte
		msg  string
		// strict, when set, is what strict refuses the request's own shape
		// with first.
		strict string
		// twoPayers registers 00002 to a second payer (twoRegisteredPayers).
		twoPayers bool
	}{
		"a Coverage's Payor, the Bundle's own entry naming another registered payer than another value": {crdPayorRequest(
			inBundle(strings.Replace(refCoverage("example", "Organization/pay-1"), `"payor"`, `"Payor"`, 1), refOrg("pay-1", "00002")),
			`"payer":`+refOrg("pay-1", "00001")), msgCoverageNamesTwoPayers, "", true},
		"a Coverage's Payor, the Bundle's own entry alone": {crdPayorRequest(
			inBundle(strings.Replace(refCoverage("example", "Organization/pay-1"), `"payor"`, `"Payor"`, 1), refOrg("pay-1", "00002")),
			""), "no registered payer for identifier urn:oid:2.16.840.1.113883.6.300|00002", "", false},
		"a Bundle's Entry": {crdPayorRequest(
			strings.Replace(inBundle(refCoverage("example", "Organization/pay-1"), refOrg("pay-1", "00001")), `"entry"`, `"Entry"`, 1),
			`"payer":`+refOrg("pay-1", "00001")), noPayerIdentifier, "", false},
		"an entry's Resource": {crdPayorRequest(
			strings.Replace(inBundle(refCoverage("example", "Organization/pay-1"), refOrg("pay-1", "00001")), `{"resource":{"resourceType":"Coverage"`, `{"Resource":{"resourceType":"Coverage"`, 1),
			`"payer":`+refOrg("pay-1", "00001")), noPayerIdentifier, "prefetch coverage refused: Bundle refused: entry has no resource", false},
		"a Bundle's ResourceType, its own entry disagreeing with another value": {crdPayorRequest(
			strings.Replace(inBundle(refCoverage("example", "Organization/pay-1"), refOrg("pay-1", "00002")), `"resourceType":"Bundle"`, `"ResourceType":"Bundle"`, 1),
			`"payer":`+refOrg("pay-1", "00001")), noPayerIdentifier, "prefetch coverage refused: resource refused: no resource type", false},
		"a Coverage value's ResourceType": {crdPayorRequest(
			strings.Replace(refCoverage("example", "Organization/pay-1"), `"resourceType"`, `"ResourceType"`, 1),
			`"payer":`+refOrg("pay-1", "00001")), noPayerIdentifier, "prefetch coverage refused: resource refused: no resource type", false},
		// A Bundle naming two payers is refused, never routed on the one
		// Coverage a reader by exact names alone would find.
		"a second Coverage's ResourceType": {crdPayorRequest(
			refCollection(refEntry("", inlineCoverage("c1", "00001")), refEntry("", strings.Replace(inlineCoverage("c2", "00002"), `"resourceType"`, `"ResourceType"`, 1))),
			""), noPayerIdentifier, "prefetch coverage refused: resource refused: no resource type", false},
		"a second Coverage's entry Resource": {crdPayorRequest(
			refCollection(refEntry("", inlineCoverage("c1", "00001")), `{"Resource":`+inlineCoverage("c2", "00002")+`}`),
			""), noPayerIdentifier, "prefetch coverage refused: Bundle refused: entry has no resource", false},
	}
	for name, row := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				var router PayerRouter
				if row.twoPayers {
					router = twoRegisteredPayers(t)
				}
				env, rec, _ := levelIngressRowRouted(t, newPrefetchSoR(), level, row.body, false, router)
				if level == EnforcementStrict && row.strict != "" {
					refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, row.strict)
					return
				}
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, row.msg)
				if env.routeHitCount() != 0 {
					t.Fatal("a refused request reached the Hub")
				}
			})
		}
	}
}

// A coverage the system of record supplied resolves its payor among the
// prefetch values (its search's records among them) before that system; when
// their identifiers name two registered payers, the request is refused as
// ambiguous and the system is not asked to pick one.
func TestCRDIngressPayorReference_SystemCoverageDisagreementRefused(t *testing.T) {
	cov := strings.Replace(refCoverage(prefetchSoRID, "Organization/pay-9"), `"id":"c1"`, `"id":"cov-9"`, 1)
	for _, enrich := range []bool{false, true} {
		for _, level := range allLevels {
			t.Run(level.String()+map[bool]string{false: "", true: "/enrich"}[enrich], func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov)))
				s.reads["Organization/pay-9"] = []byte(refOrg("pay-9", "00001"))
				body := ehrRequest(patientOnly + `,"payer":` + refOrg("pay-9", "00001") + `,"organizations":` + refCollection(refEntry("", refOrg("pay-9", "00002"))))
				env, rec, _ := levelIngressRowRouted(t, s, level, body, enrich, twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, msgCoverageNamesTwoPayers)
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
		}
	}
}

// A coverage read through the request's fhirServer resolves its payor among
// the prefetch values before that server; when their identifiers name two
// registered payers, the request is refused and the server is not asked to
// pick one.
func TestCRDIngressPayorReference_FHIRServerCoverageDisagreementRefused(t *testing.T) {
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) { fhirServerDisagreementRow(t, level) })
	}
}

func fhirServerDisagreementRow(t *testing.T, level ConformanceEnforcement) {
	var paths []string
	var mu sync.Mutex
	e := newEHRServer(t, nil)
	e.handler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		ehrRoutes(bareRefSearchset("o1"), ehrAnswer(200, "application/fhir+json", payorOrganization("o1", "00001")))(w, r)
	}
	env, _ := fhirServerEnv(t, e, FHIRServerReadPrivate)
	env.originator.cfg.ConformanceEnforcement = level
	env.originator.cfg.PayerRouter = twoRegisteredPayers(t)
	// fhirServerRequest's request, carrying two more prefetch values.
	body := ehrRequest(patientOnly + `,"payer":` + refOrg("o1", "00001") + `,"organizations":` + refCollection(refEntry("", refOrg("o1", "00002"))))
	body = bytes.ReplaceAll(body, []byte(`"example"`), []byte(`"`+strangerMember+`"`))
	body = bytes.ReplaceAll(body, []byte(`Patient/example"`), []byte(`Patient/`+strangerMember+`"`))
	body = bytes.Replace(body, []byte("https://ehr.example/fhir"), []byte(e.base), 1)
	rec := httptest.NewRecorder()
	env.originator.handleCRDIngress(rec, crdIngressPost(body))
	refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, msgCoverageNamesTwoPayers)
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/fhir/Coverage"}; !slices.Equal(paths, want) {
		t.Fatalf("read %v, want %v: the server was asked to pick the payer", paths, want)
	}
}

// A reference no resource of the request answers is looked up in the system
// of record, as before: a Coverage the system supplied resolves its payor
// there.
func TestCRDIngressPayorReference_LocalMissReadsTheSystemOfRecord(t *testing.T) {
	cov := strings.Replace(refCoverage(prefetchSoRID, "Organization/pay-9"), `"id":"c1"`, `"id":"cov-9"`, 1)
	for _, level := range allLevels {
		t.Run(level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(cov)))
			s.reads["Organization/pay-9"] = []byte(refOrg("pay-9", "00001"))
			_, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), true)
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			if _, read := s.calls(); len(read) != 1 || read[0] != "Organization/pay-9" {
				t.Fatalf("read %v, want the payor read from the system of record", read)
			}
		})
	}
}

// payorReads is the Organization reads the system of record answered (an
// enriching gateway also reads the Patient).
func payorReads(s *prefetchSoR) []string {
	_, read := s.calls()
	var out []string
	for _, r := range read {
		if !strings.HasPrefix(r, "Patient/") {
			out = append(out, r)
		}
	}
	return out
}

// dtrPayorRow posts body to the provider's DTR ingress at level, the payer
// declaring framed operations and answering with packageAnswer.
func dtrPayorRow(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, enrich bool, body []byte) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	return dtrPayorRowRouted(t, s, level, enrich, body, nil)
}

// dtrPayorRowRouted is dtrPayorRow with the provider's payer directory
// replaced by router, when not nil.
func dtrPayorRowRouted(t *testing.T, s *prefetchSoR, level ConformanceEnforcement, enrich bool, body []byte, router PayerRouter) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	env := newInProcessExchange(t)
	if router != nil {
		env.originator.cfg.PayerRouter = router
	}
	env.originator.cfg.SoR = s.sor()
	env.originator.cfg.ConformanceEnforcement = level
	optInEnrichment(env.originator, enrich)
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	return env, postDTRIngress(env, body)
}

// dtrPayorParams is a $questionnaire-package request about prefetchMember
// carrying coverage and the further resource parameters extra.
func dtrPayorParams(coverage string, extra ...string) []byte {
	params := []string{ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire}
	if coverage != "" {
		params = append(params, `{"name":"coverage","resource":`+coverage+`}`)
	}
	for _, e := range extra {
		params = append(params, `{"name":"referenced","resource":`+e+`}`)
	}
	return ehrParams(params...)
}

func TestDTRIngressPayorReference_ResolvesAmongResources(t *testing.T) {
	t.Parallel()
	rows := map[string][]byte{
		"an absolute reference equal to a referenced Bundle entry's fullUrl": dtrPayorParams(refCoverage(prefetchMember, payorAbs),
			refCollection(refEntry(payorAbs, refOrg("pay-1", "00001")))),
		"the same Organization twice": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			refOrg("pay-1", "00001"), refCollection(refEntry("", refOrg("pay-1", "00001")))),
		"a relative reference to a referenced resource": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			refOrg("pay-1", "00001")),
		// Several resources answering one reference name every identifier
		// they name, and the directory decides.
		"two resources, one naming an identifier no participant registers": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			refOrg("pay-1", "00001"), refCollection(refEntry("", refOrg("pay-1", "00002")))),
		"two resources, each naming the payer beside an unregistered identifier": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			orgWithIDs("pay-1", npiOf, payer01), refCollection(refEntry("", orgWithIDs("pay-1", payer01, einOf)))),
		"one of two answering resources naming no identifier": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			refOrg("pay-1", "00001"), refOrg("pay-1", "")),
	}
	for name, body := range rows {
		for _, level := range allLevels {
			t.Run(name+"/"+level.String(), func(t *testing.T) {
				s := newPrefetchSoR()
				env, rec := dtrPayorRow(t, s, level, false, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if _, sent := sentOperation(t, env); string(sent) != string(body) {
					t.Fatalf("the EHR's request changed:\n%s", sent)
				}
				if _, read := s.calls(); len(read) != 0 {
					t.Fatalf("the system of record was read for a payor the request carries: %v", read)
				}
			})
		}
	}
}

func TestDTRIngressPayorReference_DisagreementRefused(t *testing.T) {
	t.Parallel()
	rows := map[string][]byte{
		"two resources naming two registered payers": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			refOrg("pay-1", "00001"), refCollection(refEntry("", refOrg("pay-1", "00002")))),
		"two resources listing an NPI first, then two registered payers": dtrPayorParams(refCoverage(prefetchMember, "Organization/pay-1"),
			orgWithIDs("pay-1", npiOf, payer01), refCollection(refEntry("", orgWithIDs("pay-1", npiOf, einOf, cmsOf("00002"))))),
		"an absolute reference entries of two Bundles answer with two registered payers": dtrPayorParams(refCoverage(prefetchMember, payorUUID),
			refCollection(refEntry(payorUUID, refOrg("", "00001"))), refCollection(refEntry(payorUUID, refOrg("", "00002")))),
	}
	for name, body := range rows {
		for _, level := range allLevels {
			for _, enrich := range []bool{false, true} {
				t.Run(name+"/"+level.String()+map[bool]string{false: "", true: "/enrich"}[enrich], func(t *testing.T) {
					s := newPrefetchSoR()
					s.reads["Organization/pay-1"] = []byte(refOrg("pay-1", "00001"))
					env, rec := dtrPayorRowRouted(t, s, level, enrich, body, twoRegisteredPayers(t))
					refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, msgCoverageNamesTwoPayers)
					if env.routeHitCount() != 0 {
						t.Fatal("a refused request reached the Hub")
					}
					if read := payorReads(s); len(read) != 0 {
						t.Fatalf("the system of record was asked to pick the payer: %v", read)
					}
				})
			}
		}
	}
}

// A Coverage the system of record supplies for a request that carries none
// resolves its payor among the records the search included, then in that
// system, whether or not the participant opts in to appending it; included
// records whose identifiers name two registered payers are refused as
// ambiguous, and the system is not asked to pick.
func TestDTRIngressPayorReference_ObtainedCoverage(t *testing.T) {
	t.Parallel()
	noCoverage := dtrPayorParams("")
	cov := strings.Replace(refCoverage(prefetchSoRID, "Organization/org-1"), `"id":"c1"`, `"id":"cov-1"`, 1)
	for _, level := range allLevels {
		for _, enrich := range []bool{false, true} {
			name := level.String() + map[bool]string{false: "", true: "/enrich"}[enrich]
			t.Run("an included payor/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov), includeEntry(refOrg("org-1", "00001"))))
				_, rec := dtrPayorRow(t, s, level, enrich, noCoverage)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the included payor was read again: %v", read)
				}
			})
			t.Run("a payor read from the system of record/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov)))
				s.reads["Organization/org-1"] = []byte(refOrg("org-1", "00001"))
				_, rec := dtrPayorRow(t, s, level, enrich, noCoverage)
				if rec.Code != http.StatusOK {
					t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
				}
				if read := payorReads(s); len(read) != 1 || read[0] != "Organization/org-1" {
					t.Fatalf("read %v, want the payor read from the system of record", read)
				}
			})
			// Several Coverages route only when they name one payer: included
			// records naming two registered payers leave it in doubt, and the
			// system is not asked to pick one.
			t.Run("two Coverages whose included payors disagree/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				cov2 := strings.Replace(cov, `"id":"cov-1"`, `"id":"cov-2"`, 1)
				s.answer(t, "Coverage", page("", "", sorEntry(cov), sorEntry(cov2), includeEntry(refOrg("org-1", "00001")), includeEntry(refOrg("org-1", "00002"))))
				s.reads["Organization/org-1"] = []byte(refOrg("org-1", "00001"))
				env, rec := dtrPayorRowRouted(t, s, level, enrich, noCoverage, twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "ambiguous coverage for routing")
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
			// The system's Coverage names its payor in that system's terms;
			// a resource of the request answering the same reference with
			// another payer leaves it in doubt, for one Coverage and for
			// several.
			t.Run("the request and the included payor disagree/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov), includeEntry(refOrg("org-1", "00001"))))
				s.reads["Organization/org-1"] = []byte(refOrg("org-1", "00001"))
				env, rec := dtrPayorRowRouted(t, s, level, enrich, dtrPayorParams("", refOrg("org-1", "00002")), twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, msgCoverageNamesTwoPayers)
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
			t.Run("the request and two Coverages' included payor disagree/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				cov2 := strings.Replace(cov, `"id":"cov-1"`, `"id":"cov-2"`, 1)
				s.answer(t, "Coverage", page("", "", sorEntry(cov), sorEntry(cov2), includeEntry(refOrg("org-1", "00001"))))
				s.reads["Organization/org-1"] = []byte(refOrg("org-1", "00001"))
				env, rec := dtrPayorRowRouted(t, s, level, enrich, dtrPayorParams("", refOrg("org-1", "00002")), twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "ambiguous coverage for routing")
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
			t.Run("included payors that disagree/"+name, func(t *testing.T) {
				s := newPrefetchSoR()
				s.answer(t, "Coverage", page("", "", sorEntry(cov), includeEntry(refOrg("org-1", "00001")), includeEntry(refOrg("org-1", "00002"))))
				s.reads["Organization/org-1"] = []byte(refOrg("org-1", "00001"))
				env, rec := dtrPayorRowRouted(t, s, level, enrich, noCoverage, twoRegisteredPayers(t))
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, msgCoverageNamesTwoPayers)
				if read := payorReads(s); len(read) != 0 {
					t.Fatalf("the system of record was asked to pick the payer: %v", read)
				}
			})
		}
	}
}

// REJECTION, at every level, on CRD and DTR: every payor counts, so a payor
// after payor[0] whose reference nothing resolves, and
// that carries no identifier of its own, leaves the coverage's payer unknown.
// The refusal names that payor, whatever payor[0] is: a registered payer's
// Organization a Bundle entry answers, or a self-pay Patient (which is passed
// over, never blamed as "not an Organization").
func TestPayorReference_AnUnknownLaterPayorIsNamed(t *testing.T) {
	t.Parallel()
	coverage := func(patient, first string) string {
		return `{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + patient + `"},` +
			`"payor":[{"reference":"` + first + `"},{"reference":"urn:uuid:5e7a0c1d-0000-4000-8000-00000000dead"}]}`
	}
	// A urn: reference is read from nothing but the request (an
	// Organization/<id> one would be read on the request's fhirServer).
	want := noPayerIdentifier + `: payor[1] \"urn:uuid:5e7a0c1d-0000-4000-8000-00000000dead\" cannot be read as a payer Organization with an identifier, so the coverage's payer is unknown`
	for name, row := range map[string]struct {
		crd, dtr []byte
	}{
		"after a registered payer's Organization entry": {
			crdPayorRequest(coverage("example", "Organization/pay-1"), `"organizations":`+refCollection(refEntry("", refOrg("pay-1", "00001")))),
			dtrPayorParams(coverage(prefetchMember, "Organization/pay-1"), refCollection(refEntry("", refOrg("pay-1", "00001")))),
		},
		"after a self-pay Patient": {
			crdPayorRequest(coverage("example", "Patient/example"), ""),
			dtrPayorParams(coverage(prefetchMember, "Patient/"+prefetchMember)),
		},
	} {
		for _, level := range allLevels {
			t.Run("CRD/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec, _ := levelIngressRowWith(t, newPrefetchSoR(), level, row.crd, false)
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, want)
			})
			t.Run("DTR/"+name+"/"+level.String(), func(t *testing.T) {
				env, rec := dtrPayorRow(t, newPrefetchSoR(), level, false, row.dtr)
				refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, want)
			})
		}
	}
}

// A payor that carries an identifier of its own already names its payer: its
// reference is resolved among what the request carries alone, never read from
// the system of record, so a read that would fail stops nothing. The request
// is routed and the payer answers, on CRD (a Coverage the system of record
// supplied) and DTR (one the EHR sent), at every level, as earlier releases
// did.
func TestPayorReference_AnInlineNamedPayorIsNotRead(t *testing.T) {
	t.Parallel()
	inline := `{"reference":"Organization/pay-9","identifier":{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"00001"}}`
	sorCov := `{"resourceType":"Coverage","id":"cov-9","status":"active","beneficiary":{"reference":"Patient/` + prefetchSoRID + `"},"payor":[` + inline + `]}`
	ehrCov := `{"resourceType":"Coverage","id":"c1","status":"active","beneficiary":{"reference":"Patient/` + prefetchMember + `"},"payor":[` + inline + `]}`
	failing := map[string]error{"Organization/pay-9": &SoRReadError{Kind: SoRUnavailable}}
	for _, level := range allLevels {
		t.Run("CRD/"+level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", page("", "", sorEntry(sorCov)))
			s.readErrs = failing
			env, rec, _ := levelIngressRowWith(t, s, level, ehrRequest(patientOnly), false)
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s, %d legs; want routed once", rec.Code, rec.Body, env.routeHitCount())
			}
			if read := payorReads(s); len(read) != 0 {
				t.Fatalf("read %v, want no payor read", read)
			}
		})
		t.Run("DTR/"+level.String(), func(t *testing.T) {
			s := newPrefetchSoR()
			s.readErrs = failing
			env, rec := dtrPayorRow(t, s, level, false, dtrPayorParams(ehrCov))
			if rec.Code != http.StatusOK || env.routeHitCount() != 1 {
				t.Fatalf("answer %d %s, %d legs; want routed once", rec.Code, rec.Body, env.routeHitCount())
			}
			if read := payorReads(s); len(read) != 0 {
				t.Fatalf("read %v, want no payor read", read)
			}
		})
	}
}
