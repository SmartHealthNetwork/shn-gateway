package engine

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// recordSoR is prefetchSoR with the member's subject derived from its own
// Patient record, as a FHIR system of record derives it — so the subject the
// provider binds can be checked against a receiver's independently held source.
type recordSoR struct{ searchingPrefetchSoR }

func (s recordSoR) ResolvePatientContext(ctx context.Context, member string) (string, Demo, bool, error) {
	if member == prefetchMember {
		demo, ok := PatientDemographics(s.reads["Patient/"+prefetchSoRID])
		if !ok {
			return "", Demo{}, false, nil
		}
		return shnsdk.ResolvePCI(member, demo.BirthDate, demo.FamilyName), demo, true, nil
	}
	return s.searchingPrefetchSoR.ResolvePatientContext(ctx, member)
}

func seamDTRGateway(s *prefetchSoR) *Gateway {
	g := prefetchGateway(s)
	g.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
	g.cfg.AcceptUnknownMembers = true
	return g
}

// The reachable DTR authoring path uses records obtained from the participant's
// source by CRD origination. The older assembleDTRPackageRequest cases below
// describe an isolated explicit helper, not native-ingress behavior or a
// production request to insert a Patient parameter.
func TestDTRAuthoredSourceAndIsolatedAssembly(t *testing.T) {
	withCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	sorPatient := string(newPrefetchSoR().reads["Patient/"+prefetchSoRID])
	ctx := context.Background()
	t.Run("reachable authored DTR uses source records", func(t *testing.T) {
		s := newPrefetchSoR()
		coverage := sorCoverage("cov-1", "00001")
		s.answer(t, "Coverage", searchPage(coverage))
		obs := &observed{}
		g := prefetchGateway(s)
		g.cfg.Observer = obs.observe
		g.cfg.Clock = fixedClock
		recs, status, msg := g.originCRDRecords(ctx, "crd-order-select", prefetchMember)
		if status != 0 {
			t.Fatalf("source action: %d %s", status, msg)
		}
		order := []byte(sorRequest("sr1", "Patient/"+prefetchMember))
		body, sealed, err := originatedPackageRequest("2.0", recs, order, "http://x/q|2.1.0", "assertion-1")
		if err != nil {
			t.Fatal(err)
		}
		if sealed.Ownership() != relay.OwnershipAuthored || !bytes.Contains(body, []byte(coverage)) || !bytes.Contains(body, order) || bytes.Contains(body, []byte(`"name":"referenced"`)) {
			t.Fatalf("authored package changed source records or inserted an unrequested Patient: %s", body)
		}
		observationFlush(t, g)
		for _, key := range []string{"patient", "coverage"} {
			if ev := obs.prefetch(t)[key]; ev.Outcome != SearchOK || ev.Source != "system-of-record" {
				t.Fatalf("%s source provenance %+v", key, ev)
			}
		}
	})

	t.Run("isolated explicit assembly can copy a source Patient", func(t *testing.T) {
		s := newPrefetchSoR()
		g := seamDTRGateway(s)
		g.cfg.AcceptUnknownMembers = false
		p, status, msg := g.assembleDTRPackageRequest(ctx, withCoverage, true)
		if status != 0 {
			t.Fatalf("%d %s", status, msg)
		}
		sent := relay.BytesForTest(p.request)
		k := bytes.LastIndex(withCoverage, []byte("\n  ]"))
		if !bytes.HasPrefix(sent, withCoverage[:k]) || !bytes.HasSuffix(sent, withCoverage[k:]) {
			t.Fatalf("the EHR's bytes changed:\n%s", sent)
		}
		added := strings.TrimSpace(string(sent[k : len(sent)-(len(withCoverage)-k)]))
		if strings.TrimSpace(strings.TrimPrefix(added, ",")) != `{"name":"referenced","resource":`+sorPatient+`}` {
			t.Fatalf("added element %q", added)
		}
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditDTRPatientObtain}) {
			t.Fatalf("edits %v", got)
		}
		// This helper has no mounted production caller. Only the reachable
		// originated CRD/DTR action above can establish source provenance.
		// Local consumption independently requires the receiver's source record.
		// A payload Patient cannot supply missing authority or identity.
		payer := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}, AcceptUnknownMembers: true}}
		if status, _ := payer.bindPackageParameters(ctx, sent, p.pci); status != http.StatusBadRequest {
			t.Fatalf("unheld local consumer status=%d", status)
		}
		payer.cfg.SoR = recordSoR{searchingPrefetchSoR{newPrefetchSoR()}}
		if status, msg := payer.bindPackageParameters(ctx, sent, p.pci); status != 0 {
			t.Fatalf("known local consumer: %d %s", status, msg)
		}
		if status, _ := payer.bindPackageParameters(ctx, sent, "pci:foreign"); status != http.StatusForbidden {
			t.Fatalf("foreign token status=%d", status)
		}

	})

	leftAlone := func(t *testing.T, g *Gateway, s *prefetchSoR, body []byte) {
		t.Helper()
		p, status, msg := g.assembleDTRPackageRequest(ctx, body, true)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || !bytes.Equal(relay.BytesForTest(p.request), body) {
			t.Fatalf("%d %s: ownership %v, edits %v", status, msg, p.request.Ownership(), p.request.Edits())
		}
		if _, read := s.calls(); len(read) != 0 {
			t.Fatalf("the system of record was read: %v", read)
		}
	}
	t.Run("a request carrying a Patient is left alone", func(t *testing.T) {
		patient := `{"resourceType":"Patient","id":"` + prefetchMember + `","name":[{"family":"Sent"}],"birthDate":"1961-01-01"}`
		for name, body := range map[string][]byte{
			"as a referenced parameter": ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), `{"name":"referenced","resource":`+patient+`}`),
			"as a part":                 ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), `{"name":"referenced","part":[{"name":"patient","resource":`+patient+`}]}`),
			"in a bundle":               ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), `{"name":"referenced","resource":{"resourceType":"Bundle","type":"collection","entry":[{"resource":`+patient+`}]}}`),
		} {
			t.Run(name, func(t *testing.T) {
				s := newPrefetchSoR()
				leftAlone(t, seamDTRGateway(s), s, body)
			})
		}
	})
	t.Run("native preparation without an assembly request is left alone", func(t *testing.T) {
		s := newPrefetchSoR()
		g := seamDTRGateway(s)
		g.cfg.AcceptUnknownMembers = false
		p, status, msg := g.prepareDTRPackageRequest(ctx, withCoverage)
		if status != 0 || !bytes.Equal(relay.BytesForTest(p.request), withCoverage) {
			t.Fatalf("%d %s: %v", status, msg, p.request)
		}
		if _, reads := s.calls(); len(reads) != 0 {
			t.Fatalf("the system of record was read: %v", reads)
		}
	})
	t.Run("local assembly refuses a member the provider does not hold", func(t *testing.T) {
		s := newPrefetchSoR()
		body := ehrParams(ehrOrderParam("sr1", strangerMember), ehrCoverageParam(strangerMember, "00001"), dtrQuestionnaire)
		g := seamDTRGateway(s)
		p, status, msg := g.assembleDTRPackageRequest(ctx, body, true)
		if status != http.StatusForbidden || p.pci != "" {
			t.Fatalf("%d %s: ownership %v pci %q", status, msg, p.request.Ownership(), p.pci)
		}
	})
	t.Run("a system of record naming the patient differently adds nothing", func(t *testing.T) {
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		p, status, msg := seamDTRGateway(s).assembleDTRPackageRequest(ctx, withCoverage, true)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("%d %s: ownership %v", status, msg, p.request.Ownership())
		}
	})
	t.Run("a request without coverage or Patient gains both", func(t *testing.T) {
		s := newPrefetchSoR()
		sorCov := sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)
		s.answer(t, "Coverage", searchPage(sorCov))
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
		p, status, msg := seamDTRGateway(s).assembleDTRPackageRequest(ctx, body, true)
		if status != 0 {
			t.Fatalf("%d %s", status, msg)
		}
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditDTRCoverageObtain, relay.EditDTRPatientObtain}) {
			t.Fatalf("edits %v", got)
		}
		sent := relay.BytesForTest(p.request)
		k := bytes.LastIndex(body, []byte("\n  ]"))
		if !bytes.HasPrefix(sent, body[:k]) || !bytes.HasSuffix(sent, body[k:]) {
			t.Fatalf("the EHR's bytes changed:\n%s", sent)
		}
		added := string(sent[k : len(sent)-(len(body)-k)])
		if !strings.Contains(added, `{"name":"coverage","resource":`+sorCov+`}`) || !strings.Contains(added, `{"name":"referenced","resource":`+sorPatient+`}`) ||
			strings.Index(added, `"coverage"`) > strings.Index(added, `"referenced"`) {
			t.Fatalf("added %q", added)
		}
	})
	t.Run("another patient's record is refused", func(t *testing.T) {
		s := newPrefetchSoR()
		s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Patient","id":"other","name":[{"family":"Other"}],"birthDate":"1960-01-01"}`)
		_, status, msg := seamDTRGateway(s).assembleDTRPackageRequest(ctx, withCoverage, true)
		if status != http.StatusBadGateway || msg != "system of record returned another patient's resource" {
			t.Fatalf("%d %s", status, msg)
		}
	})
}

func TestDTRIngress_UnknownMemberFlagDoesNotAssemblePatient(t *testing.T) {
	s := newPrefetchSoR()
	body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	env := newTransportExchange(t) // Native source-assembly absence is a transport property.
	env.originator.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
	env.originator.cfg.SubjectReferenceResolver = subjectResolverFunc(func(_ context.Context, ref PatientReference) (string, bool, error) {
		if ref != (PatientReference{Holder: "provider", System: "fhir-relative", Value: "Patient/" + prefetchMember}) {
			return "", false, nil
		}
		return "pci:prefetch-source", true, nil
	})
	env.originator.cfg.AcceptUnknownMembers = true
	declareFramedDTR(t, env, true)
	env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
	rec := postDTRIngress(env, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
	}
	_, sent := sentOperation(t, env)
	if !bytes.Equal(sent, body) {
		t.Fatalf("native request gained an unrequested source Patient: %s", sent)
	}
	if _, reads := s.calls(); len(reads) != 0 {
		t.Fatalf("native request read source resources: %v", reads)
	}
}

type dtrPatientReadFailure struct{ *prefetchSoR }

func (s dtrPatientReadFailure) ResolveByReferenceContext(context.Context, string) ([]byte, bool, error) {
	return nil, false, &SoRReadError{Kind: SoRUnavailable}
}

func TestDTRIngress_ExplicitPatientAssemblyRejections(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
	}{
		{"not held", http.StatusUnprocessableEntity},
		{"wrong resource type", http.StatusBadGateway},
		{"other patient", http.StatusBadGateway},
		{"source unavailable", http.StatusServiceUnavailable},
		{"signed request", http.StatusUnprocessableEntity},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newPrefetchSoR()
			g := prefetchGateway(s)
			body := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"))
			switch row.name {
			case "not held":
				delete(s.reads, "Patient/"+prefetchSoRID)
			case "wrong resource type":
				s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Observation","id":"example"}`)
			case "other patient":
				s.reads["Patient/"+prefetchSoRID] = []byte(`{"resourceType":"Patient","id":"other"}`)
			case "source unavailable":
				g.cfg.SoR = dtrPatientReadFailure{s}
			case "signed request":
				body = bytes.Replace(body, []byte(`"resourceType" : "Parameters",`), []byte(`"resourceType" : "Parameters","signature":{"type":[],"when":"2026-01-01","who":{}},`), 1)
			}
			p, status, msg := g.assembleDTRPackageRequest(context.Background(), body, true)
			if status != row.status || p.request.Ownership() != 0 {
				t.Fatalf("status=%d msg=%s payload=%v", status, msg, p.request)
			}
			if row.name == "signed request" && !strings.Contains(msg, "signed content cannot be edited") {
				t.Fatalf("wrong signed refusal: %s", msg)
			}
		})
	}
}
