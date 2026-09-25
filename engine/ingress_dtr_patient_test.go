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
// provider binds can be compared with what a non-holding payer derives from
// the Patient the request carries.
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

// defaultDTRGateway carries unknown members (the default) under the E-05
// enrichment seam, over a system of record that derives the subject from its own
// Patient record.
func defaultDTRGateway(s *prefetchSoR) *Gateway {
	g := prefetchGateway(s)
	g.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
	g.cfg.enrichDTRPatient = true // the enrichment seam these rows pin
	return g
}

// Under the enrichment seam (Config.enrichDTRPatient) a
// questionnaire-package request about a member the provider holds, carrying no
// Patient, gains the provider's own Patient record as a referenced resource, so
// a payer that does not hold the member derives the same subject from the
// request that the provider derived from its record.
func TestDTRIngress_PatientObtainedUnderEnrichment(t *testing.T) {
	withCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	sorPatient := string(newPrefetchSoR().reads["Patient/"+prefetchSoRID])
	ctx := context.Background()

	t.Run("a request carrying no Patient gains the system of record's", func(t *testing.T) {
		s := newPrefetchSoR()
		obs := &observed{}
		env := newInProcessExchange(t)
		env.originator.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
		env.originator.cfg.enrichDTRPatient = true
		env.originator.cfg.Observer = obs.observe
		env.originator.cfg.Clock = fixedClock
		declareFramedDTR(t, env, true)
		env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
		rec := postDTRIngress(env, withCoverage)
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
		}
		_, sent := sentOperation(t, env)
		// The EHR's bytes are unchanged but for one element appended to the
		// parameter array: the system of record's Patient, byte for byte.
		k := bytes.LastIndex(withCoverage, []byte("\n  ]"))
		if !bytes.HasPrefix(sent, withCoverage[:k]) || !bytes.HasSuffix(sent, withCoverage[k:]) {
			t.Fatalf("the EHR's bytes changed:\n%s", sent)
		}
		added := strings.TrimSpace(string(sent[k : len(sent)-(len(withCoverage)-k)]))
		if strings.TrimSpace(strings.TrimPrefix(added, ",")) != `{"name":"referenced","resource":`+sorPatient+`}` {
			t.Fatalf("added element %q", added)
		}
		p, status, msg := defaultDTRGateway(s).prepareDTRPackageRequest(ctx, withCoverage)
		if status != 0 {
			t.Fatalf("%d %s", status, msg)
		}
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditDTRPatientObtain}) {
			t.Fatalf("edits %v", got)
		}
		ev, ok := obs.prefetchOn(t, "dtr-questionnaire-fetch")["patient"]
		wantEv := prefetchObtained{Key: "patient", Operation: shnsdk.FrameOperationQuestionnairePackage, Source: "system-of-record",
			Query: "Patient/" + prefetchSoRID, Outcome: SearchOK, Count: 1, RetrievedAt: fixedClock().UTC()}
		if !ok || ev != wantEv {
			t.Fatalf("provenance %+v\nwant %+v", ev, wantEv)
		}
		// The payer, which does not hold the member, binds it by the Patient
		// the request now carries: the same subject the provider derived.
		payer := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}}}
		if pci, status, msg := payer.bindPackageParameters(ctx, sent); status != 0 || pci != p.pci {
			t.Fatalf("payer bind: %d %s pci=%q, want %q", status, msg, pci, p.pci)
		}
	})

	leftAlone := func(t *testing.T, g *Gateway, s *prefetchSoR, body []byte) {
		t.Helper()
		p, status, msg := g.prepareDTRPackageRequest(ctx, body)
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
				leftAlone(t, defaultDTRGateway(s), s, body)
			})
		}
	})
	// Native traffic is carried as sent: without the enrichment seam nothing is
	// appended, whether or not the participant requires known members.
	for _, require := range []bool{false, true} {
		t.Run(map[bool]string{false: "by default a native request is carried as sent", true: "requiring known members, a native request is carried as sent"}[require], func(t *testing.T) {
			s := newPrefetchSoR()
			g := defaultDTRGateway(s)
			g.cfg.enrichDTRPatient = false
			g.cfg.RequireKnownMembers = require
			leftAlone(t, g, s, withCoverage)
		})
	}
	t.Run("a member the provider does not hold sends no Patient and binds by id", func(t *testing.T) {
		s := newPrefetchSoR()
		body := ehrParams(ehrOrderParam("sr1", strangerMember), ehrCoverageParam(strangerMember, "00001"), dtrQuestionnaire)
		g := defaultDTRGateway(s)
		p, status, msg := g.prepareDTRPackageRequest(ctx, body)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || p.pci != strangerPCI() {
			t.Fatalf("%d %s: ownership %v pci %q", status, msg, p.request.Ownership(), p.pci)
		}
	})
	t.Run("a system of record naming the patient differently adds nothing", func(t *testing.T) {
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		p, status, msg := defaultDTRGateway(s).prepareDTRPackageRequest(ctx, withCoverage)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("%d %s: ownership %v", status, msg, p.request.Ownership())
		}
	})
	t.Run("a request without coverage or Patient gains both", func(t *testing.T) {
		s := newPrefetchSoR()
		sorCov := sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)
		s.answer(t, "Coverage", searchPage(sorCov))
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
		p, status, msg := defaultDTRGateway(s).prepareDTRPackageRequest(ctx, body)
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
		_, status, msg := defaultDTRGateway(s).prepareDTRPackageRequest(ctx, withCoverage)
		if status != http.StatusBadGateway || msg != "system of record returned another patient's resource" {
			t.Fatalf("%d %s", status, msg)
		}
	})
}
