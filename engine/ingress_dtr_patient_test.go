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

func seamDTRGateway(s *prefetchSoR) *Gateway {
	g := prefetchGateway(s)
	g.cfg.SoR = recordSoR{searchingPrefetchSoR{s}}
	g.cfg.AcceptUnknownMembers = true
	return g
}

// Under the connectathon seam a questionnaire-package request about a member
// the provider holds, carrying no Patient, gains the provider's own Patient
// record as a referenced resource, so a payer that does not hold the member
// derives the same subject from the request that the provider derived from
// its record.
func TestDTRIngress_PatientObtainedUnderSeam(t *testing.T) {
	withCoverage := ehrParams(ehrOrderParam("sr1", prefetchMember), ehrCoverageParam(prefetchMember, "00001"), dtrQuestionnaire)
	sorPatient := string(newPrefetchSoR().reads["Patient/"+prefetchSoRID])
	ctx := context.Background()

	t.Run("a request carrying no Patient gains the system of record's", func(t *testing.T) {
		s := newPrefetchSoR()
		obs := &observed{}
		g := seamDTRGateway(s)
		g.cfg.Observer = obs.observe
		g.cfg.Clock = fixedClock
		p, status, msg := g.prepareDTRPackageRequest(ctx, withCoverage)
		if status != 0 {
			t.Fatalf("%d %s", status, msg)
		}
		sent := relay.BytesForTest(p.request)
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
		if got := p.request.Edits(); !slices.Equal(got, []relay.EditID{relay.EditDTRPatientObtain}) {
			t.Fatalf("edits %v", got)
		}
		observationFlush(t, g)
		ev, ok := obs.prefetchOn(t, "dtr-questionnaire-fetch")["patient"]
		wantEv := prefetchObtained{Key: "patient", Operation: shnsdk.FrameOperationQuestionnairePackage, Source: "system-of-record",
			Query: "Patient/" + prefetchSoRID, Outcome: SearchOK, Count: 1, RetrievedAt: fixedClock().UTC()}
		if !ok || ev != wantEv {
			t.Fatalf("provenance %+v\nwant %+v", ev, wantEv)
		}
		// The payer, which does not hold the member, binds what was sent to
		// the subject the provider derived from its record — and no longer to
		// a subject derived from the id alone.
		payer := &Gateway{cfg: Config{SoR: noMemberSoR{newPrefetchSoR()}, AcceptUnknownMembers: true}}
		if status, msg := payer.bindPackageParameters(ctx, sent, p.pci); status != 0 {
			t.Fatalf("payer bind to the provider's subject: %d %s", status, msg)
		}
		if status, _ := payer.bindPackageParameters(ctx, sent, shnsdk.ResolvePCI(prefetchMember, "", "")); status != http.StatusForbidden {
			t.Fatalf("payer bind to the id-alone subject: %d, want 403", status)
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
				leftAlone(t, seamDTRGateway(s), s, body)
			})
		}
	})
	t.Run("without the seam the request is left alone", func(t *testing.T) {
		s := newPrefetchSoR()
		g := seamDTRGateway(s)
		g.cfg.AcceptUnknownMembers = false
		leftAlone(t, g, s, withCoverage)
	})
	t.Run("a member the provider does not hold sends no Patient and binds by id", func(t *testing.T) {
		s := newPrefetchSoR()
		body := ehrParams(ehrOrderParam("sr1", strangerMember), ehrCoverageParam(strangerMember, "00001"), dtrQuestionnaire)
		g := seamDTRGateway(s)
		p, status, msg := g.prepareDTRPackageRequest(ctx, body)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed || p.pci != strangerPCI() {
			t.Fatalf("%d %s: ownership %v pci %q", status, msg, p.request.Ownership(), p.pci)
		}
	})
	t.Run("a system of record naming the patient differently adds nothing", func(t *testing.T) {
		s := newPrefetchSoR()
		s.sorID = "sor-9"
		p, status, msg := seamDTRGateway(s).prepareDTRPackageRequest(ctx, withCoverage)
		if status != 0 || p.request.Ownership() != relay.OwnershipRelayed {
			t.Fatalf("%d %s: ownership %v", status, msg, p.request.Ownership())
		}
	})
	t.Run("a request without coverage or Patient gains both", func(t *testing.T) {
		s := newPrefetchSoR()
		sorCov := sorCoverage("cov-1", shnsdk.CMSPayerIdentity.Value)
		s.answer(t, "Coverage", searchPage(sorCov))
		body := ehrParams(ehrOrderParam("sr1", prefetchMember), dtrQuestionnaire)
		p, status, msg := seamDTRGateway(s).prepareDTRPackageRequest(ctx, body)
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
		_, status, msg := seamDTRGateway(s).prepareDTRPackageRequest(ctx, withCoverage)
		if status != http.StatusBadGateway || msg != "system of record returned another patient's resource" {
			t.Fatalf("%d %s", status, msg)
		}
	})
}
