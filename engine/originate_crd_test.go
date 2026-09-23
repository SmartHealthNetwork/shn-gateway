package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The originated-request rows: the CDS Hooks requests this gateway originates for
// its own participant carry the participant's own records (its system of record's
// Patient and Coverage search result, the patient named by the member id), fire
// the hook of the workflow they check, name no FHIR server, and hand the order the
// payer returned to the questionnaire step.

// originSoR is prefetchSoR (the patient prefetchMember, searchable) with an open
// order, a Coverage record for routing and a supplier.
type originSoR struct {
	searchingPrefetchSoR
	order    []byte
	coverage []byte
}

func (s originSoR) OpenOrderContext(context.Context, string) ([]byte, bool, error) {
	return s.order, s.order != nil, nil
}

func (s originSoR) OpenCoverageContext(context.Context, string) ([][]byte, error) {
	return [][]byte{s.coverage}, nil
}

// originPayer answers the gateway's legs as a payer would, per transaction
// type, and keeps every request it received, opened.
type originPayer struct {
	authzPriv      ed25519.PrivateKey
	providerEncPub *[32]byte
	payerEncPub    *[32]byte
	payerEncPriv   *[32]byte
	clock          func() time.Time
	answer         func(tx string, request []byte) (status int, body []byte)

	mu       sync.Mutex
	requests map[string][][]byte
}

func (p *originPayer) sent(tx string) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[tx]
}

func (p *originPayer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	stub := &relaySubstrate{authzPriv: p.authzPriv, providerEncPub: p.providerEncPub, clock: p.clock}
	if strings.HasSuffix(req.URL.Path, "/authorize") {
		return stub.handleAuthorize(body)
	}
	env, err := shnsdk.DecodeEnvelope(body)
	if err != nil {
		return errResp("decode: " + err.Error()), nil
	}
	plain, err := shnsdk.Open(env, p.payerEncPub, p.payerEncPriv)
	if err != nil {
		return errResp("open: " + err.Error()), nil
	}
	tx := env.Metadata.TransactionType
	p.mu.Lock()
	p.requests[tx] = append(p.requests[tx], plain)
	p.mu.Unlock()
	status, answer := p.answer(tx, plain)
	lr := LegResult{Status: status}
	if answer != nil {
		lr.Response = relay.ForTest(answer, "application/json")
	}
	stub.setResult(lr)
	return stub.handleRoute(body)
}

// originGateway is a provider gateway on profile over sor, reaching a payer that
// answers with answer.
func originGateway(t *testing.T, profile string, sor SystemOfRecord, pop Populator, answer func(tx string, request []byte) (int, []byte)) (*Gateway, *originPayer) {
	t.Helper()
	authzPub, authzPriv := genED25519(t)
	provEncPub, provEncPriv := genKeyPair(t)
	_, provSignPriv := genED25519(t)
	payerEncPub, payerEncPriv := genKeyPair(t)
	payerSignPub, _ := genED25519(t)
	clock := func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) }
	payer := &originPayer{authzPriv: authzPriv, providerEncPub: provEncPub, payerEncPub: payerEncPub, payerEncPriv: payerEncPriv,
		clock: clock, answer: answer, requests: map[string][][]byte{}}
	reg := shnsdk.NewRegistry()
	reg.Set("provider", shnsdk.RegistryEntry{ID: "provider", Role: "provider", EncPub: provEncPub, SignPub: authzPub, MessageFrames: shnsdk.SupportedMessageFrames()})
	reg.Set("payer", shnsdk.RegistryEntry{ID: "payer", Role: "payer", EncPub: payerEncPub, SignPub: payerSignPub, MessageFrames: shnsdk.SupportedMessageFrames(), RequestFrames: dtrOperationFrames})
	n := 0
	g := mustNew(t, Config{
		Role:               "provider",
		HolderID:           "provider",
		PayerRouter:        payerRouterFor(t, "payer"),
		Identity:           shnsdk.Identity{HolderID: "provider", SignPriv: provSignPriv, EncPub: provEncPub, EncPriv: provEncPriv},
		AuthzURL:           "http://origin.test",
		AuthzPub:           authzPub,
		HubTransportPub:    authzPub,
		HubURL:             "http://origin.test",
		Reg:                reg,
		Validator:          syntheticFakeValidator(),
		SoR:                sor,
		Store:              newCensusSoR(),
		Clock:              clock,
		NPI:                "1234567890",
		OriginationProfile: profile,
		Populator:          pop,
		CorrelationGen: func() string {
			n++
			return strings.Repeat("0", 31) + string(rune('0'+n%10))
		},
		Client: &http.Client{Transport: payer},
	})
	return g, payer
}

// originRecords are the system of record's records for prefetchMember, named
// sorID there.
func originRecords(sorID string) (patient, coverage, order []byte) {
	bs := string(rune(92))
	patient = []byte("{ \"resourceType\" : \"Patient\", \"id\" : \"" + sorID + "\",\n  \"identifier\" : [ { \"system\" : \"urn:shn:member\", \"value\" : \"" + prefetchMember + "\" } ],\n  \"name\" : [ { \"family\" : \"T" + bs + "u00e9st\" } ], \"birthDate\" : \"1960-01-01\" }")
	coverage = []byte("{ \"resourceType\" : \"Coverage\", \"id\" : \"cov-1\", \"status\" : \"active\",\n  \"beneficiary\" : { \"reference\" : \"Patient/" + sorID + "\" },\n  \"subscriber\" : { \"reference\" : \"Patient/" + sorID + "\" },\n  \"payor\" : [ { \"reference\" : \"Organization/pay-1\" } ],\n  \"costToBeneficiary\" : [ { \"valueMoney\" : { \"value\" : 10.50 } } ] }")
	order = []byte("{ \"resourceType\" : \"ServiceRequest\", \"id\" : \"sr-9\", \"status\" : \"active\", \"intent\" : \"order\",\n  \"code\" : { \"coding\" : [ { \"system\" : \"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets\", \"code\" : \"L8000\" } ] },\n  \"quantityQuantity\" : { \"value\" : 1.50 },\n  \"subject\" : { \"reference\" : \"Patient/" + sorID + "\", \"display\" : \"a " + bs + "u003c b\" } }")
	return patient, coverage, order
}

// draftTwin is order as a draft.
func draftTwin(order []byte) []byte {
	return bytes.Replace(order, []byte(`"status" : "active"`), []byte(`"status" : "draft"`), 1)
}

const payerOrganization = `{"resourceType":"Organization","id":"pay-1","identifier":[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}]}`

// originSystem is a system of record holding originRecords under sorID, whose
// Coverage search includes the payer Organization.
func originSystem(t *testing.T, sorID string) (originSoR, []byte) {
	t.Helper()
	patient, coverage, order := originRecords(sorID)
	s := newPrefetchSoR()
	s.sorID = sorID
	// The payer organization the Coverage names is readable too: the origination
	// carries the participant's own record for the payer as the request's insurer
	// entry, and a system that could not serve it could not originate at all.
	s.reads = map[string][]byte{"Patient/" + sorID: patient, "Organization/pay-1": []byte(payerOrganization)}
	page := []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://sor.example/fhir/Coverage/cov-1","resource":` +
		string(coverage) + `,"search":{"mode":"match"}},{"fullUrl":"https://sor.example/fhir/Organization/pay-1","resource":` +
		payerOrganization + `,"search":{"mode":"include"}}]}`)
	s.answer(t, "Coverage", page)
	// The order is signed (active); the system also holds it as the draft the
	// coverage check (order-select) is about.
	s.answer(t, "ServiceRequest", searchsetOf(matchOf(string(draftTwin(order)))))
	routing := []byte(`{"resourceType":"Coverage","id":"cov-1","beneficiary":{"reference":"Patient/` + sorID + `"},"payor":[{"identifier":{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001"}}]}`)
	return originSoR{searchingPrefetchSoR: searchingPrefetchSoR{s}, order: order, coverage: routing}, page
}

// payerAnswerFor is a payer's CRD answer returning order with coverage
// information (questionnaire when canonical is set) and no card.
func payerAnswerFor(t *testing.T, order []byte, covered, paNeeded, canonical string) []byte {
	t.Helper()
	ci := shnsdk.CoverageInformationInput{Coverage: "Coverage/cov-1", Covered: covered, PANeeded: paNeeded, Date: "2026-09-17", CoverageAssertionID: "ca-77"}
	if canonical != "" {
		ci.Questionnaires, ci.DocNeeded = []string{canonical}, []string{"clinical"}
	}
	out, err := shnsdk.BuildCRDResponse("2.0", shnsdk.CRDResponseInputs{Orders: []shnsdk.CRDOrderCoverage{{Order: order, Description: "Add coverage information", Coverage: []shnsdk.CoverageInformationInput{ci}}}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type crdSent struct {
	Hook              string          `json:"hook"`
	FHIRServer        json.RawMessage `json:"fhirServer"`
	FHIRAuthorization json.RawMessage `json:"fhirAuthorization"`
	Context           struct {
		PatientID        string          `json:"patientId"`
		UserID           string          `json:"userId"`
		Selections       []string        `json:"selections"`
		DraftOrders      json.RawMessage `json:"draftOrders"`
		DispatchedOrders []string        `json:"dispatchedOrders"`
		Performer        string          `json:"performer"`
	} `json:"context"`
	Prefetch map[string]json.RawMessage `json:"prefetch"`
}

func decodeSent(t *testing.T, b []byte) crdSent {
	t.Helper()
	var s crdSent
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("sent request: %v", err)
	}
	return s
}

// renameInText names the patient by the member id in a record's text.
func renameInText(b []byte, sorID string) []byte {
	b = bytes.ReplaceAll(b, []byte(`"id" : "`+sorID+`"`), []byte(`"id":"`+prefetchMember+`"`))
	return bytes.ReplaceAll(b, []byte(`"reference" : "Patient/`+sorID+`"`), []byte(`"reference":"Patient/`+prefetchMember+`"`))
}

func TestOriginatedCRD_UsesSoRPatientAndCoverage(t *testing.T) {
	for _, sorID := range []string{prefetchMember, "pat-77"} {
		t.Run("system names the patient "+sorID, func(t *testing.T) {
			sor, page := originSystem(t, sorID)
			patient, coverage, order := originRecords(sorID)
			g, payer := originGateway(t, "provider-data", sor, nil, func(tx string, req []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
			})
			rec := httptest.NewRecorder()
			g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", nil), prefetchMember)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s", rec.Code, rec.Body)
			}
			reqs := payer.sent("crd-order-select")
			if len(reqs) != 1 {
				t.Fatalf("%d CRD requests", len(reqs))
			}
			sent := decodeSent(t, reqs[0])
			wantPatient, wantCoverage, wantOrder := patient, []byte(sorAssembly(t, "Coverage", page)), draftTwin(order)
			if sorID != prefetchMember {
				// The patient is named by the member id, and nothing else changes.
				wantPatient = []byte(strings.Replace(string(patient), `"id" : "`+sorID+`"`, `"id" : "`+prefetchMember+`"`, 1))
				wantCoverage = bytes.Replace(wantCoverage, []byte(`"beneficiary" : { "reference" : "Patient/`+sorID+`" }`), []byte(`"beneficiary" : { "reference" : "Patient/`+prefetchMember+`" }`), 1)
				wantOrder = bytes.Replace(wantOrder, []byte(`"reference" : "Patient/`+sorID+`"`), []byte(`"reference" : "Patient/`+prefetchMember+`"`), 1)
				_ = coverage
			}
			if got := string(sent.Prefetch["patient"]); got != string(wantPatient) {
				t.Errorf("patient\n got %s\nwant %s", got, wantPatient)
			}
			if got := string(sent.Prefetch["coverage"]); got != string(wantCoverage) {
				t.Errorf("coverage\n got %s\nwant %s", got, wantCoverage)
			}
			if !bytes.Contains(sent.Context.DraftOrders, wantOrder) {
				t.Errorf("draft orders %s\nwant the order %s", sent.Context.DraftOrders, wantOrder)
			}
			if sent.Context.PatientID != prefetchMember {
				t.Errorf("patientId %q", sent.Context.PatientID)
			}
			if sorID != prefetchMember {
				// A subscriber reference is not the Coverage's binding path: it stays as held.
				if !bytes.Contains(sent.Prefetch["coverage"], []byte(`"subscriber" : { "reference" : "Patient/`+sorID+`" }`)) {
					t.Errorf("subscriber changed: %s", sent.Prefetch["coverage"])
				}
				// History values are left out when the system names the patient differently.
				for _, k := range []string{"serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"} {
					if _, ok := sent.Prefetch[k]; ok {
						t.Errorf("history %s sent", k)
					}
				}
				return
			}
			for _, k := range []string{"serviceHistory", "deviceHistory", "medicationHistory", "questionnaireResponses"} {
				if string(sent.Prefetch[k]) == "" {
					t.Errorf("history %s missing", k)
				}
			}
		})
	}
	t.Run("refused before anything is sent", func(t *testing.T) {
		for name, mutate := range map[string]func(*originSoR){
			"no Patient in the system of record": func(s *originSoR) { s.reads = map[string][]byte{} },
			"no Coverage in the system of record": func(s *originSoR) {
				s.searches["Coverage"] = searchAnswer{res: SearchResult{Pages: [][]byte{[]byte(`{"resourceType":"Bundle","type":"searchset"}`)}}}
			},
			"coverage search unavailable": func(s *originSoR) {
				s.searches["Coverage"] = searchAnswer{err: &SearchError{Outcome: SearchUnavailable, Reason: "down"}}
			},
			"another patient's Coverage": func(s *originSoR) {
				other := `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Coverage","id":"c","beneficiary":{"reference":"Patient/someone-else"}},"search":{"mode":"match"}}]}`
				s.answer(t, "Coverage", []byte(other))
			},
		} {
			t.Run(name, func(t *testing.T) {
				sor, _ := originSystem(t, prefetchMember)
				mutate(&sor)
				g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) { return 0, nil })
				rec := httptest.NewRecorder()
				g.originateNoPACRD(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc02", nil), prefetchMember)
				if rec.Code < 400 {
					t.Fatalf("status %d %s", rec.Code, rec.Body)
				}
				if n := len(payer.sent("crd-order-select")); n != 0 {
					t.Fatalf("%d requests sent", n)
				}
			})
		}
	})
}

// dispatchSystem is originSystem holding a dispatched DeviceRequest with its
// supplier, the device history holding it.
func dispatchSystem(t *testing.T) (originSoR, []byte) {
	t.Helper()
	sor, _ := originSystem(t, prefetchMember)
	order := []byte(`{"resourceType":"DeviceRequest","id":"dr-5","status":"active","intent":"order","codeCodeableConcept":{"coding":[{"system":"http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets","code":"E0431"}]},"subject":{"reference":"Patient/` + prefetchMember + `"},"performer":{"reference":"Organization/sup-1"}}`)
	sor.order = order
	sor.reads["Organization/sup-1"] = []byte(`{"resourceType":"Organization","id":"sup-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1922334455"}]}`)
	sor.answer(t, "DeviceRequest", []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"fullUrl":"https://sor.example/fhir/DeviceRequest/dr-5","resource":`+string(order)+`,"search":{"mode":"match"}},{"fullUrl":"https://sor.example/fhir/Organization/sup-1","resource":`+string(sor.reads["Organization/sup-1"])+`,"search":{"mode":"include"}}]}`))
	return sor, order
}

func TestOriginatedCRD_NoCallbackFields(t *testing.T) {
	sor, _ := originSystem(t, prefetchMember)
	_, _, order := originRecords(prefetchMember)
	dsor, dorder := dispatchSystem(t)
	for name, run := range map[string]func() [][]byte{
		"order-select": func() [][]byte {
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
			})
			g.originateNoPACRD(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
			return payer.sent("crd-order-select")
		},
		"order-sign": func() [][]byte {
			g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, order, shnsdk.CoveredNotCovered, "", "")
			})
			g.runCRDThenDTROrder(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
			return payer.sent("crd-order-select")
		},
		"order-dispatch": func() [][]byte {
			g, payer := originGateway(t, "provider-data", dsor, nil, func(string, []byte) (int, []byte) {
				return 0, payerAnswerFor(t, dorder, shnsdk.CoveredNotCovered, "", "")
			})
			g.originateDispatch(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
			return payer.sent("crd-order-dispatch")
		},
	} {
		t.Run(name, func(t *testing.T) {
			reqs := run()
			if len(reqs) != 1 {
				t.Fatalf("%d requests", len(reqs))
			}
			sent := decodeSent(t, reqs[0])
			if sent.FHIRServer != nil || sent.FHIRAuthorization != nil {
				t.Fatalf("request names a callback: %s", reqs[0])
			}
			for _, needle := range []string{"fhirServer", "fhirAuthorization", "provider.example"} {
				if bytes.Contains(reqs[0], []byte(needle)) {
					t.Fatalf("request carries %q: %s", needle, reqs[0])
				}
			}
		})
	}
}

func TestOriginatedCRD_HookMatchesWorkflow(t *testing.T) {
	sor, _ := originSystem(t, prefetchMember)
	_, _, order := originRecords(prefetchMember)
	dsor, dorder := dispatchSystem(t)
	t.Run("the no-authorization coverage check is order-select", func(t *testing.T) {
		g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
			return 0, payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededNoAuth, "")
		})
		g.originateNoPACRD(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
		sent := decodeSent(t, payer.sent("crd-order-select")[0])
		if sent.Hook != "order-select" || len(sent.Context.Selections) != 1 || sent.Context.Selections[0] != "ServiceRequest/sr-9" {
			t.Fatalf("hook %q selections %v", sent.Hook, sent.Context.Selections)
		}
	})
	t.Run("a signed order going to prior authorization is order-sign", func(t *testing.T) {
		g, payer := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
			return 0, payerAnswerFor(t, order, shnsdk.CoveredNotCovered, "", "")
		})
		g.runCRDThenDTROrder(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
		sent := decodeSent(t, payer.sent("crd-order-select")[0])
		if sent.Hook != "order-sign" || sent.Context.Selections != nil || sent.Context.UserID != "Practitioner/1234567890" {
			t.Fatalf("hook %q selections %v user %q", sent.Hook, sent.Context.Selections, sent.Context.UserID)
		}
	})
	t.Run("a dispatched order is order-dispatch, resolved from the device history", func(t *testing.T) {
		g, payer := originGateway(t, "provider-data", dsor, nil, func(string, []byte) (int, []byte) {
			return 0, payerAnswerFor(t, dorder, shnsdk.CoveredNotCovered, "", "")
		})
		g.originateDispatch(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
		sent := decodeSent(t, payer.sent("crd-order-dispatch")[0])
		if sent.Hook != "order-dispatch" || len(sent.Context.DispatchedOrders) != 1 || sent.Context.DispatchedOrders[0] != "DeviceRequest/dr-5" || sent.Context.Performer != "Organization/sup-1" {
			t.Fatalf("request %+v", sent.Context)
		}
		if !bytes.Contains(sent.Prefetch["deviceHistory"], dorder) {
			t.Fatalf("device history %s does not hold the dispatched order", sent.Prefetch["deviceHistory"])
		}
		// The supplier the order names travels as a record the device search
		// included, exactly as the system of record holds it.
		supplier := `{"resourceType":"Organization","id":"sup-1","identifier":[{"system":"http://hl7.org/fhir/sid/us-npi","value":"1922334455"}]}`
		if !bytes.Contains(sent.Prefetch["deviceHistory"], []byte(supplier+`,"search":{"mode":"include"}`)) {
			t.Fatalf("device history %s does not carry the supplier as an included record", sent.Prefetch["deviceHistory"])
		}
	})
	t.Run("a device history that does not hold the dispatched order is refused", func(t *testing.T) {
		hsor, horder := dispatchSystem(t)
		other := bytes.Replace(horder, []byte(`"id":"dr-5"`), []byte(`"id":"dr-6"`), 1)
		hsor.answer(t, "DeviceRequest", []byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":`+string(other)+`,"search":{"mode":"match"}}]}`))
		g, payer := originGateway(t, "provider-data", hsor, nil, func(string, []byte) (int, []byte) {
			return 0, payerAnswerFor(t, horder, shnsdk.CoveredNotCovered, "", "")
		})
		rec := httptest.NewRecorder()
		g.originateDispatch(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember)
		if rec.Code != http.StatusUnprocessableEntity || !bytes.Contains(rec.Body.Bytes(), []byte("the system of record's device history does not hold the dispatched order DeviceRequest/dr-5")) {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
		if n := len(payer.sent("crd-order-dispatch")); n != 0 {
			t.Fatalf("%d dispatch requests sent", n)
		}
	})
	t.Run("a payer that does not offer the hook refuses, and the refusal is returned", func(t *testing.T) {
		refusal := []byte(`{"error":"payer offers no CDS service for hook order-sign","offered":["order-select"]}`)
		g, _ := originGateway(t, "provider-data", sor, nil, func(string, []byte) (int, []byte) {
			return http.StatusUnprocessableEntity, refusal
		})
		rec := httptest.NewRecorder()
		g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
		if rec.Code != http.StatusUnprocessableEntity || !bytes.Contains(rec.Body.Bytes(), []byte("no CDS service for hook order-sign")) {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})
}

// capturePopulator records what the questionnaire step received and stops.
type capturePopulator struct {
	got []PopulateContext
}

var errCaptured = errors.New("captured")

func (c *capturePopulator) Populate(_ context.Context, _ []byte, pc PopulateContext) ([]byte, []FilledItem, error) {
	c.got = append(c.got, pc)
	return nil, nil, errCaptured
}

func TestRunCRDThenDTR_UsesPayerUpdatedOrder(t *testing.T) {
	const canonical = "http://example.org/fhir/Questionnaire/PriorAuthRequired"
	questionnaire := []byte(`{"resourceType":"Questionnaire","id":"q","url":"` + canonical + `","status":"active","item":[{"linkId":"1","text":"x","type":"string"}]}`)
	pkg, err := testQuestionnairePackage(questionnaire)
	if err != nil {
		t.Fatal(err)
	}
	sor, _ := originSystem(t, prefetchMember)
	_, _, order := originRecords(prefetchMember)
	// The payer's answer: no card; the coverage information and the questionnaire are
	// only on the order it returns.
	answer := payerAnswerFor(t, order, shnsdk.CoveredCovered, shnsdk.PANeededAuthNeeded, canonical)
	obs, err := shnsdk.ParseCRDResponse(answer)
	if err != nil || len(obs.Orders) != 1 {
		t.Fatal(err)
	}
	updated := obs.Orders[0].Order
	pop := &capturePopulator{}
	g, payer := originGateway(t, "provider-data", sor, pop, func(tx string, _ []byte) (int, []byte) {
		if tx == "dtr-questionnaire-fetch" {
			return 0, pkg
		}
		return 0, answer
	})
	rec := httptest.NewRecorder()
	g.runCRDThenDTROrder(rec, httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
	fetches := payer.sent("dtr-questionnaire-fetch")
	if len(fetches) != 1 || !bytes.Contains(fetches[0], []byte(canonical)) {
		t.Fatalf("questionnaire requests %q, want one for the canonical the payer's order carries (status %d %s)", fetches, rec.Code, rec.Body)
	}
	if len(pop.got) != 1 {
		t.Fatalf("questionnaire step ran %d times (status %d %s)", len(pop.got), rec.Code, rec.Body)
	}
	if !bytes.Equal(pop.got[0].Order, updated) || bytes.Equal(updated, order) {
		t.Fatalf("questionnaire step got order\n%s\nwant the payer's updated order\n%s", pop.got[0].Order, updated)
	}
	if pop.got[0].OrderRef != "ServiceRequest/sr-9" {
		t.Fatalf("order ref %q", pop.got[0].OrderRef)
	}
	t.Run("the order sent when the payer returns none", func(t *testing.T) {
		legacy := []byte(`{"cards":[{"summary":"s","indicator":"info","source":{"label":"p","topic":{"system":"http://terminology.hl7.org/CodeSystem/cdshooks-card-type","code":"coverage"}},"extension":{"covered":"covered","paNeeded":"auth-needed","questionnaires":["` + canonical + `"]}}]}`)
		pop := &capturePopulator{}
		g, _ := originGateway(t, "provider-data", sor, pop, func(tx string, _ []byte) (int, []byte) {
			if tx == "dtr-questionnaire-fetch" {
				return 0, pkg
			}
			return 0, legacy
		})
		g.runCRDThenDTROrder(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), prefetchMember, "", "", "", "", false)
		if len(pop.got) != 1 || !bytes.Equal(pop.got[0].Order, order) {
			t.Fatalf("questionnaire step got %+v, want the order sent", pop.got)
		}
	})
}

func TestNamePatientByMember(t *testing.T) {
	bs := string(rune(92))
	record := []byte("{\"resourceType\":\"Bundle\",\"type\":\"searchset\",\"entry\":[\n" +
		" {\"resource\":{\"resourceType\":\"Coverage\",\"id\":\"c\",\"beneficiary\":{\"reference\":\"https://sor.example/fhir/Patient/p-1/_history/2\",\"display\":\"x" + bs + "u00e9\"}," +
		"\"subscriber\":{\"reference\":\"Patient/p-1\"},\"contained\":[{\"resourceType\":\"Patient\",\"id\":\"p-1\"},{\"resourceType\":\"Observation\",\"id\":\"co\",\"subject\":{\"reference\":\"Patient/p-1/_history/3\"},\"performer\":[{\"reference\":\"Patient/p-1\"}]}],\"payor\":[{\"reference\":\"Organization/o\"}],\"n\":1.50}},\n" +
		" {\"resource\":{\"resourceType\":\"ServiceRequest\",\"id\":\"s\",\"subject\":{\"reference\":\"Patient/p-1\"}}},\n" +
		" {\"resource\":{\"resourceType\":\"Patient\",\"id\":\"p-1\",\"link\":[{\"other\":{\"reference\":\"Patient/p-1\"}}]}},\n" +
		" {\"resource\":{\"resourceType\":\"Observation\",\"id\":\"o\",\"subject\":{\"reference\":\"Patient/p-10\"},\"performer\":[{\"reference\":\"Patient/p-1\"}]}}\n]}")
	got, err := namePatientByMember(record, "p-1", "MBR-1")
	if err != nil {
		t.Fatal(err)
	}
	// Only relative references are renamed (an absolute one names a server
	// the gateway does not resolve, and is left as held); a contained
	// resource's binding reference is renamed, a contained Patient's local id
	// is not.
	want := bytes.Replace(record, []byte(`"subject":{"reference":"Patient/p-1/_history/3"}`), []byte(`"subject":{"reference":"Patient/MBR-1"}`), 1)
	want = bytes.Replace(want, []byte(`{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/p-1"}}`), []byte(`{"resourceType":"ServiceRequest","id":"s","subject":{"reference":"Patient/MBR-1"}}`), 1)
	want = bytes.Replace(want, []byte(`{"resourceType":"Patient","id":"p-1","link"`), []byte(`{"resourceType":"Patient","id":"MBR-1","link"`), 1)
	if !bytes.Contains(want, []byte(`"reference":"https://sor.example/fhir/Patient/p-1/_history/2"`)) || !bytes.Contains(want, []byte(`[{"resourceType":"Patient","id":"p-1"},`)) {
		t.Fatal("fixture: the absolute reference and the contained Patient must stay")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if same, err := namePatientByMember(record, "MBR-1", "MBR-1"); err != nil || !bytes.Equal(same, record) {
		t.Fatalf("same id changed the record: %v", err)
	}
	t.Run("a faulty rename is refused", func(t *testing.T) {
		renameFaultHook = func(b []byte) {
			i := bytes.Index(b, []byte("1.50"))
			b[i+3] = '1'
		}
		defer func() { renameFaultHook = nil }()
		if out, err := namePatientByMember(record, "p-1", "MBR-1"); err == nil {
			t.Fatalf("a changed number was not refused: %s", out)
		}
	})
	t.Run("the check refuses any other change", func(t *testing.T) {
		edits := []patientRename{{start: bytes.Index(record, []byte(`"Patient/p-1"`)), value: "Patient/MBR-1", path: []any{"entry", 0, "resource", "subscriber", "reference"}}}
		edits[0].end = edits[0].start + len(`"Patient/p-1"`)
		good := bytes.Replace(record, []byte(`"subscriber":{"reference":"Patient/p-1"}`), []byte(`"subscriber":{"reference":"Patient/MBR-1"}`), 1)
		if err := checkPatientRenames(record, good, edits); err != nil {
			t.Fatalf("a declared change refused: %v", err)
		}
		for name, bad := range map[string][]byte{
			"a byte outside the names": bytes.Replace(good, []byte(`1.50`), []byte(`1.5`), 1),
			"another value":            bytes.Replace(good, []byte(`Patient/MBR-1`), []byte(`Patient/MBR-2`), 1),
			"a whitespace change":      bytes.Replace(good, []byte("[\n {"), []byte("[{"), 1),
		} {
			if err := checkPatientRenames(record, bad, edits); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		wrongPath := []patientRename{{start: edits[0].start, end: edits[0].end, value: "Patient/MBR-1", path: []any{"entry", 0, "resource", "beneficiary", "reference"}}}
		if err := checkPatientRenames(record, good, wrongPath); err == nil {
			t.Error("a change at an undeclared place accepted")
		}
	})
}
