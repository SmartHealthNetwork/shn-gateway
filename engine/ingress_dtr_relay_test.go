package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The DTR ingress rows drive an EHR's $questionnaire-package request through
// the provider gateway and the network: the payer's side receives the EHR's
// own Parameters, byte for byte, except for the one registered edit that adds
// the patient's Coverage when the EHR sent none.

// packageAnswer is the payer's package, in its own layout.
var packageAnswer = []byte("{ \"resourceType\" : \"Parameters\", \"parameter\" : [ { \"name\" : \"PackageBundle\", \"resource\" : { \"resourceType\" : \"Bundle\", \"type\" : \"collection\", \"entry\" : [ { \"resource\" : { \"resourceType\" : \"Questionnaire\", \"url\" : \"http://x/q\", \"text\" : { \"div\" : \"a " + lt + " b\" } } } ] } } ] }")

// dtrFixture reads a vendored DTR input fixture, naming the patient
// prefetchMember and the payer the test router knows.
func dtrFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "relayfidelity", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("Patient/examplepatient"), []byte("Patient/"+prefetchMember))
	return bytes.ReplaceAll(b, []byte(`"system":"urn:oid:2.16.840.1.113883.4.7","value":"10D0202020"`),
		[]byte(`"system":"`+shnsdk.CMSPayerIdentity.System+`","value":"`+shnsdk.CMSPayerIdentity.Value+`"`))
}

// withParameters inserts parameter elements (JSON text) after the first
// element of body's parameter array.
func withParameters(t *testing.T, body []byte, elements ...string) []byte {
	t.Helper()
	doc, err := relay.Doc(relay.NewBody(body, relay.OriginIngressRequest))
	if err != nil {
		t.Fatal(err)
	}
	arr, _ := doc.Member(doc.Root(), "parameter")
	_, end := doc.Span(doc.Elems(arr)[0])
	var b []byte
	b = append(b, body[:end]...)
	for _, e := range elements {
		b = append(b, ",\n    "...)
		b = append(b, e...)
	}
	return append(b, body[end:]...)
}

// ehrCoverageParam is an EHR coverage parameter for patient, naming payer value.
func ehrCoverageParam(patient, payerValue string) string {
	return `{"name":"coverage","resource":{"resourceType":"Coverage","id":"cov-` + payerValue + `","status":"active","beneficiary":{"reference":"Patient/` + patient + `"},"payor":[{"identifier":{"system":"` + shnsdk.CMSPayerIdentity.System + `","value":"` + payerValue + `"}}]}}`
}

// ehrOrderParam is an order parameter about patient.
func ehrOrderParam(id, patient string) string {
	return `{"name":"order","resource":{"resourceType":"ServiceRequest","id":"` + id + `","status":"active","intent":"order","subject":{"reference":"Patient/` + patient + `"},"quantityQuantity":{"value":2.50}}}`
}

// ehrParams is a Parameters in the EHR's layout holding elements.
func ehrParams(elements ...string) []byte {
	return []byte("{\n  \"resourceType\" : \"Parameters\",\n  \"parameter\" : [\n    " + strings.Join(elements, ",\n    ") + "\n  ]\n}\n")
}

// jsonEscapeE is the JSON escape of U+00E9, built at run time so no editor
// turns it into the character.
var jsonEscapeE = string(rune(92)) + "u00e9"

const dtrQuestionnaire = `{"name":"questionnaire","valueCanonical":"http://x/q|2.1.0"}`

// dtrOperationFrames declares framed DTR operations only (a manifest payer
// declares them; other transactions stay unframed to such a test peer).
var dtrOperationFrames = []string{shnsdk.RequestFrameV1Op}

// declareFramedDTR makes the in-process payer declare framed DTR operations
// (or, with capable false, only the base request frame).
func declareFramedDTR(t *testing.T, env *inProcessExchange, capable bool) {
	t.Helper()
	entry, ok := env.originator.cfg.Reg.Lookup(env.payerID)
	if !ok {
		t.Fatal("no payer in the registry")
	}
	entry.RequestFrames = []string{shnsdk.RequestFrameV1}
	if capable {
		entry.RequestFrames = append(entry.RequestFrames, shnsdk.RequestFrameV1Op)
	}
	env.originator.cfg.Reg.Set(env.payerID, entry)
}

// dtrIngressRow posts body to the provider's DTR ingress, the payer
// declaring framed operations, and answering with packageAnswer.
func dtrIngressRow(t *testing.T, s *prefetchSoR, body []byte) (*inProcessExchange, *httptest.ResponseRecorder) {
	t.Helper()
	env := newInProcessExchange(t)
	env.originator.cfg.SoR = s.sor()
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	return env, postDTRIngress(env, body)
}

func postDTRIngress(env *inProcessExchange, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/fhir+json")
	rec := httptest.NewRecorder()
	env.originator.handleDTRIngress(rec, req)
	return rec
}

// sentOperation returns the request frame's operation header and the body
// the payer's side received.
func sentOperation(t *testing.T, env *inProcessExchange) (string, []byte) {
	t.Helper()
	b := env.lastRequestPayload()
	if !shnsdk.IsFramed(b) {
		t.Fatalf("the questionnaire request was not framed: %.80q", b)
	}
	hdr, body, err := shnsdk.DecodeHTTPFrame(b)
	if err != nil {
		t.Fatalf("decode request frame: %v", err)
	}
	return hdr.Headers[shnsdk.FrameHeaderOperation], body
}

func TestDTRIngress_ParametersRelayedExactly(t *testing.T) {
	reference := dtrFixture(t, "dtr-package-params-2.0.json")
	for _, row := range []struct {
		name string
		body []byte
	}{
		{"the reference provider's input: repeated orders and canonicals, a |version, context, meta, markup and decimals", reference},
		{"the 2.2 example: id, meta, language, two orders, a |version and context", dtrFixture(t, "dtr-package-params-2.2.json")},
		{"referenced records and changedsince", withParameters(t, reference,
			`{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/example"},"valueQuantity":{"value":98.60}}}]}}`,
			`{"name":"changedsince","valueDateTime":"2026-01-01T00:00:00.000+00:00"}`)},
		{"an unknown parameter and an unknown member", withParameters(t, reference,
			`{"name":"x-unknown","valueString":"kept `+jsonEscapeE+`","extension":[{"url":"http://example.org/x","valueInteger":1}]}`)},
		{"a canonical beside the order", ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)},
		{"an order alone", ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"))},
		{"context and a coverage only", ehrParams(ehrCoverageParam(prefetchMember, "00001"), `{"name":"context","valueString":"ctx-1"}`)},
		{"the same parameter repeated", ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire, dtrQuestionnaire, `{"name":"context","valueString":"a"}`, `{"name":"context","valueString":"a"}`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newPrefetchSoR()
			env, rec := dtrIngressRow(t, s, row.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			op, sent := sentOperation(t, env)
			if op != shnsdk.FrameOperationQuestionnairePackage {
				t.Fatalf("operation header %q", op)
			}
			if !bytes.Equal(sent, row.body) {
				t.Fatalf("the payer's side received other bytes:\n got %s\nwant %s", sent, row.body)
			}
			if searched, _ := s.calls(); len(searched) != 0 {
				t.Fatalf("a request carrying coverage searched the system of record: %v", searched)
			}
			if !bytes.Equal(rec.Body.Bytes(), packageAnswer) {
				t.Fatalf("the EHR received %s", rec.Body.Bytes())
			}
			if leg, outcome := lastOutcome(t, env.originator); leg != "dtr-questionnaire-fetch" || outcome != "ok" {
				t.Fatalf("recorded %s %s", leg, outcome)
			}
		})
	}
}

func TestDTRIngress_CoverageObtainedOnlyWhenAbsent(t *testing.T) {
	noCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire, `{"name":"context","valueString":"ctx-1"}`)
	sorCov := sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)

	t.Run("a request with coverage is sent as it is", func(t *testing.T) {
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"))
		s := newPrefetchSoR()
		p, status, msg := prefetchGateway(s).prepareDTRPackageRequest(context.Background(), body)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || !bytes.Equal(relay.BytesForTest(p.request), body) {
			t.Fatalf("%d %s: ownership %v", status, msg, p.request.Ownership())
		}
		// Nothing is obtained, so the system's id for the patient is not read.
		if searched, read := s.calls(); len(searched) != 0 || len(read) != 0 || s.idReads != 0 {
			t.Fatalf("the system of record was consulted: searched %v, read %v, id reads %d", searched, read, s.idReads)
		}
	})

	t.Run("a request without coverage gains the system of record's", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "Coverage", page("", "", sorEntry(sorCov), includeEntry(`{"resourceType":"Organization","id":"org-1","name":"Payer"}`)))
		obs := &observed{}
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = s.sor()
		env.originator.cfg.Observer = obs.observe
		env.originator.cfg.Clock = fixedClock
		declareFramedDTR(t, env, true)
		env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
		rec := postDTRIngress(env, noCoverage)
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		_, sent := sentOperation(t, env)
		// The EHR's bytes up to the end of its last parameter and from the
		// end of the array on are unchanged; between them is one new element
		// holding the system of record's Coverage byte for byte.
		k := bytes.LastIndex(noCoverage, []byte("\n  ]"))
		if !bytes.HasPrefix(sent, noCoverage[:k]) || !bytes.HasSuffix(sent, noCoverage[k:]) {
			t.Fatalf("the EHR's bytes changed:\n%s", sent)
		}
		added := strings.TrimSpace(string(sent[k : len(sent)-(len(noCoverage)-k)]))
		if added != `,{"name":"coverage","resource":`+sorCov+`}` && !strings.HasPrefix(added, ",") {
			t.Fatalf("added %q", added)
		}
		if strings.TrimSpace(strings.TrimPrefix(added, ",")) != `{"name":"coverage","resource":`+sorCov+`}` {
			t.Fatalf("added element %q", added)
		}
		p, _, _ := prefetchGateway(s).prepareDTRPackageRequest(context.Background(), noCoverage)
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditDTRCoverageObtain}) {
			t.Fatalf("edits %v", got)
		}
		ev, ok := obs.prefetchOn(t, "dtr-questionnaire-fetch")["coverage"]
		wantEv := prefetchObtained{Key: "coverage", Operation: shnsdk.FrameOperationQuestionnairePackage, Source: "system-of-record",
			Query: "Coverage?patient=Patient%2Fexample&_include=Coverage%3Apayor", Outcome: SearchOK, Count: 1, Pages: 1, RetrievedAt: fixedClock().UTC()}
		if !ok || ev != wantEv {
			t.Fatalf("provenance %+v\nwant %+v", ev, wantEv)
		}
	})

	refused := func(t *testing.T, s *prefetchSoR, body []byte, status int, msg string) {
		t.Helper()
		env, rec := dtrIngressRow(t, s, body)
		refusedBeforeTheNetwork(t, env, rec, status, msg)
	}
	t.Run("no coverage in the system of record", func(t *testing.T) {
		refused(t, newPrefetchSoR(), noCoverage, http.StatusUnprocessableEntity, "no coverage in request or system of record")
	})
	t.Run("a coverage parameter without a resource is the EHR's, and refused", func(t *testing.T) {
		// The system of record holds a Coverage the gateway would add to a
		// request that had no coverage parameter; beside the EHR's own
		// coverage parameter it adds nothing, and one without a resource is
		// refused before any search or routing.
		for _, param := range []string{
			`{"name":"coverage","valueReference":{"reference":"Coverage/cov-1"}}`,
			`{"name":"coverage"}`,
		} {
			s := newPrefetchSoR()
			s.answer(t, "Coverage", searchPage(sorCov))
			body := ehrParams(ehrOrderParam("sr1", prefetchMember), param, dtrQuestionnaire)
			refused(t, s, body, http.StatusBadRequest, "coverage parameter carries no resource")
			if searched, _ := s.calls(); len(searched) != 0 {
				t.Fatalf("%s: the system of record was searched: %v", param, searched)
			}
		}
	})
	t.Run("a system that cannot search", func(t *testing.T) {
		s := newPrefetchSoR()
		s.search = false
		refused(t, s, noCoverage, http.StatusUnprocessableEntity, "cannot search for it")
	})
	t.Run("a system that is unavailable", func(t *testing.T) {
		s := newPrefetchSoR()
		s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}
		refused(t, s, noCoverage, http.StatusServiceUnavailable, "coverage unavailable")
	})
	t.Run("coverages naming two payers", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "Coverage", searchPage(sorCov, sorCoverage("cov-2", "00078")))
		refused(t, s, noCoverage, http.StatusUnprocessableEntity, "ambiguous coverage")
	})
	t.Run("a system naming the patient differently", func(t *testing.T) {
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		s.answer(t, "Coverage", searchPage(sorCov))
		refused(t, s, noCoverage, http.StatusUnprocessableEntity, dtrCoverageNamedDifferently)
		if searched, _ := s.calls(); len(searched) != 0 {
			t.Fatalf("searched %v", searched)
		}
	})
	t.Run("a coverage about another patient", func(t *testing.T) {
		s := newPrefetchSoR()
		s.answer(t, "Coverage", searchPage(strings.Replace(sorCov, "Patient/"+prefetchSoRID, "Patient/other", 1)))
		refused(t, s, noCoverage, http.StatusBadGateway, "another patient's resource")
	})
}

// includeEntry is an entry the search included.
func includeEntry(res string) string {
	return "{ \"fullUrl\": \"https://sor.example/fhir/Organization/org-1\", \"resource\": " + res + ", \"search\": { \"mode\": \"include\" } }"
}

func TestDTRIngress_NoPatient422(t *testing.T) {
	for name, body := range map[string][]byte{
		"a canonical only":         ehrParams(dtrQuestionnaire),
		"context only":             ehrParams(`{"name":"context","valueString":"ctx-1"}`),
		"no parameters":            []byte(`{"resourceType":"Parameters","parameter":[]}`),
		"no parameter member":      []byte(`{"resourceType":"Parameters"}`),
		"a referenced Bundle only": ehrParams(`{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			env, rec := dtrIngressRow(t, newPrefetchSoR(), body)
			refusedBeforeTheNetwork(t, env, rec, http.StatusUnprocessableEntity, "cannot bind the request to a patient")
		})
	}
	t.Run("not a Parameters", func(t *testing.T) {
		env, rec := dtrIngressRow(t, newPrefetchSoR(), []byte(`{"canonical":"http://x/q"}`))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "parse questionnaire-package parameters failed")
	})
	t.Run("a coverage that is not a Coverage", func(t *testing.T) {
		env, rec := dtrIngressRow(t, newPrefetchSoR(), ehrParams(`{"name":"coverage","resource":{"resourceType":"Patient","id":"example"}}`))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadRequest, "not a Coverage")
	})
}

func TestDTRIngress_CoveragesTwoPayers422(t *testing.T) {
	router, err := NewConfigPayerRouter([]PayerDirectoryEntry{
		{System: shnsdk.CMSPayerIdentity.System, Value: "00001", HolderID: "payer"},
		{System: shnsdk.CMSPayerIdentity.System, Value: "00078", HolderID: "payer-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]struct {
		body   []byte
		status int
		msg    string
	}{
		"two payers":          {ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire, ehrCoverageParam(prefetchMember, "00078")), http.StatusUnprocessableEntity, "coverages name more than one payer"},
		"an unknown payer":    {ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrCoverageParam(prefetchMember, "99999")), http.StatusUnprocessableEntity, "no registered payer"},
		"a coverage no payer": {ehrParams(`{"name":"coverage","resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/example"}}}`), http.StatusUnprocessableEntity, "no payer identifier"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			env.originator.cfg.PayerRouter = router
			declareFramedDTR(t, env, true)
			refusedBeforeTheNetwork(t, env, postDTRIngress(env, row.body), row.status, row.msg)
		})
	}
	t.Run("two coverages naming one payer are both carried", func(t *testing.T) {
		body := ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire, strings.Replace(ehrCoverageParam(prefetchMember, "00001"), "cov-00001", "cov-b", 1))
		env, rec := dtrIngressRow(t, newPrefetchSoR(), body)
		if _, sent := sentOperation(t, env); rec.Code != http.StatusOK || !bytes.Equal(sent, body) {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestDTRIngress_SubjectBindAllResources: a valid request, then the same
// request with one resource about another patient, which is refused before
// anything is sent.
func TestDTRIngress_SubjectBindAllResources(t *testing.T) {
	valid := []string{ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire}
	env, rec := dtrIngressRow(t, newPrefetchSoR(), ehrParams(valid...))
	if rec.Code != http.StatusOK {
		t.Fatalf("the valid request: %d %s", rec.Code, rec.Body.String())
	}
	if _, sent := sentOperation(t, env); !bytes.Equal(sent, ehrParams(valid...)) {
		t.Fatal("the valid request was not relayed exactly")
	}
	other := func(s string) string { return strings.ReplaceAll(s, "Patient/"+prefetchMember, "Patient/other") }
	for _, row := range []struct {
		name   string
		extra  []string
		status int
		msg    string
	}{
		{"a second coverage for another patient", []string{other(ehrCoverageParam(prefetchMember, "00001"))}, http.StatusForbidden, "inconsistent patient reference"},
		{"a second order for another patient", []string{other(ehrOrderParam("sr2", prefetchMember))}, http.StatusForbidden, "inconsistent patient reference"},
		{"an order naming no patient", []string{`{"name":"order","resource":{"resourceType":"ServiceRequest","id":"sr3","status":"active","intent":"order"}}`}, http.StatusForbidden, "names no patient"},
		{"a referenced record about another patient", []string{`{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"},"subject":{"reference":"Patient/other"}}}]}}`}, http.StatusForbidden, "parameter referenced refused"},
		{"another patient's record", []string{`{"name":"referenced","resource":{"resourceType":"Patient","id":"other"}}`}, http.StatusForbidden, "parameter referenced refused"},
		{"a record in a part about another patient", []string{`{"name":"x","part":[{"name":"y","resource":{"resourceType":"Condition","id":"c1","subject":{"reference":"Patient/other"}}}]}`}, http.StatusForbidden, "parameter x.y refused"},
		{"a contained record about another patient", []string{`{"name":"referenced","resource":{"resourceType":"Condition","id":"c2","subject":{"reference":"Patient/example"},"contained":[{"resourceType":"Patient","id":"p","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"other"}]}]}}`}, http.StatusForbidden, "parameter referenced refused"},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := ehrParams(append(slices.Clone(valid), row.extra...)...)
			env, rec := dtrIngressRow(t, newPrefetchSoR(), body)
			refusedBeforeTheNetwork(t, env, rec, row.status, row.msg)
		})
	}
	t.Run("a patient the system of record does not know, known members required", func(t *testing.T) {
		body := ehrParams(ehrCoverageParam("MBR-UNKNOWN", "00001"), dtrQuestionnaire)
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = newPrefetchSoR().sor()
		env.originator.cfg.RequireKnownMembers = true
		declareFramedDTR(t, env, true)
		env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
		rec := postDTRIngress(env, body)
		refusedBeforeTheNetwork(t, env, rec, http.StatusForbidden, "request patient does not resolve")
	})
}

// TestProviderDTR_RefusedWithoutPeerCapability: a payer whose registration
// does not declare framed DTR operations is refused before anything is sent,
// whatever sends the operation.
func TestProviderDTR_RefusedWithoutPeerCapability(t *testing.T) {
	want := shnsdk.ErrFramedDTRUnsupported.Error()
	t.Run("the ingress", func(t *testing.T) {
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = newPrefetchSoR().sor()
		declareFramedDTR(t, env, false)
		rec := postDTRIngress(env, ehrParams(ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire))
		refusedBeforeTheNetwork(t, env, rec, http.StatusBadGateway, want)
	})
	t.Run("the leg itself", func(t *testing.T) {
		env := newInProcessExchange(t)
		declareFramedDTR(t, env, false)
		p, err := relay.Authored(relay.BuilderSDKDTRPackage, ehrParams(ehrCoverageParam("MBR-COVERED", "00001")), dtrPackageContentType)
		if err != nil {
			t.Fatal(err)
		}
		_, err = env.originator.OriginateLeg(env.ctx, env.req, env.payerID, "dtr-questionnaire-fetch", "pci", "corr-1", "",
			Content{WorkstreamType: workstreamPA, Payload: p, Operation: shnsdk.FrameOperationQuestionnairePackage})
		if !errors.Is(err, shnsdk.ErrFramedDTRUnsupported) || env.routeHitCount() != 0 {
			t.Fatalf("err %v, route hits %d", err, env.routeHitCount())
		}
	})
	t.Run("a payer missing from the registry", func(t *testing.T) {
		env := newInProcessExchange(t)
		if status, msg := env.originator.framedDTRRefusal("nobody"); status != http.StatusBadGateway || msg != want {
			t.Fatalf("%d %s", status, msg)
		}
	})
}
