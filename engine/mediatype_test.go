package engine

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestCarriedFHIRMediaType(t *testing.T) {
	for in, want := range map[string]string{
		"application/fhir+json":                      "application/fhir+json",
		"application/fhir+json; fhirVersion=4.0":     "application/fhir+json; fhirVersion=4.0",
		" application/fhir+json;charset=utf-8 ":      "application/fhir+json;charset=utf-8",
		"application/json":                           "application/json",
		"text/html":                                  "",
		"application/xml":                            "",
		"application/fhir+json\r\nX-Injected: 1":     "",
		"application/fhir+json; fhirVersion=4.0\x00": "",
		"not a media type;;;":                        "",
		"":                                           "",
	} {
		if got := carriedFHIRMediaType(in); got != want {
			t.Errorf("carriedFHIRMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAcceptFor(t *testing.T) {
	for in, want := range map[string]string{
		"application/fhir+json":                             "application/fhir+json",
		"application/fhir+json; charset=utf-8":              "application/fhir+json",
		"application/fhir+json; fhirVersion=4.0; charset=x": "application/fhir+json; fhirVersion=4.0",
		"application/json":                                  "application/json",
	} {
		if got := acceptFor(in); got != want {
			t.Errorf("acceptFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// Each leg's own media type reaches the payer; a FHIR leg takes the sender's
// declared FHIR media type when one was framed; a CDS Hooks leg never does
// (older senders stamp application/fhir+json on every frame).
func TestNativeRequestMediaType(t *testing.T) {
	fhir := relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginPeerFrame), mediaFHIRJSON)
	cds := relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginPeerFrame), mediaJSON)
	bg := context.Background()
	declared := withRequestMediaType(bg, "application/fhir+json; fhirVersion=4.0")
	cases := []struct {
		name string
		ctx  context.Context
		p    relay.Payload
		want string
	}{
		{"fhir leg, nothing declared", bg, fhir, mediaFHIRJSON},
		{"fhir leg, declared", declared, fhir, "application/fhir+json; fhirVersion=4.0"},
		{"cds leg, nothing declared", bg, cds, mediaJSON},
		{"cds leg ignores a framed fhir type", withRequestMediaType(bg, mediaFHIRJSON), cds, mediaJSON},
	}
	for _, c := range cases {
		if got := nativeRequestMediaType(c.ctx, c.p); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInboundFrameMediaType(t *testing.T) {
	framed, err := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{"Content-Type": "application/fhir+json; fhirVersion=4.0"}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := inboundFrameMediaType(framed); got != "application/fhir+json; fhirVersion=4.0" {
		t.Fatalf("framed: %q", got)
	}
	bad, _ := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{"Content-Type": "text/plain"}, []byte(`{}`))
	if got := inboundFrameMediaType(bad); got != "" {
		t.Fatalf("a non-FHIR declared type was carried: %q", got)
	}
	if got := inboundFrameMediaType([]byte(`{"resourceType":"Bundle"}`)); got != "" {
		t.Fatalf("bare: %q", got)
	}
}

// End to end on the native forward: the payer's system receives each leg's
// own media type as Content-Type and Accept — never application/json on a
// FHIR operation — and a FHIR leg carries the sender's declared type.
func TestNativeForwardSendsEachLegsMediaType(t *testing.T) {
	declared := "application/fhir+json; fhirVersion=4.0; charset=utf-8"
	t.Run("DTR", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection"}`)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "shn-order-select", nil, nil)
		if _, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatal(err)
		}
		if ct, acc := p.lastHeader.Get("Content-Type"), p.lastHeader.Get("Accept"); ct != mediaFHIRJSON || acc != mediaFHIRJSON {
			t.Fatalf("DTR sent Content-Type %q Accept %q, want %s", ct, acc, mediaFHIRJSON)
		}
		if _, err := n.Handle(withRequestMediaType(dtrPkgCtx(context.Background()), declared), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatal(err)
		}
		if ct, acc := p.lastHeader.Get("Content-Type"), p.lastHeader.Get("Accept"); ct != declared || acc != "application/fhir+json; fhirVersion=4.0" {
			t.Fatalf("DTR with a declared type sent Content-Type %q Accept %q", ct, acc)
		}
	})
	t.Run("PAS submit", func(t *testing.T) {
		var got http.Header
		approved := fixturePASResponse(t, approvedClaimResponse(nil), true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Header().Set("Content-Type", mediaFHIRJSON)
			_, _ = w.Write(approved)
		}))
		t.Cleanup(srv.Close)
		n := NewNativeResponder(srv.Client(), srv.URL, "shn-order-select", newCensusSoR(), fixedClock)
		if _, err := n.Handle(context.Background(), "pas-claim", "corr-media", "PCI-1", originatorBuiltConformantBundle(t, "MBR-COVERED")); err != nil {
			t.Fatal(err)
		}
		if ct, acc := got.Get("Content-Type"), got.Get("Accept"); ct != mediaFHIRJSON || acc != mediaFHIRJSON {
			t.Fatalf("PAS sent Content-Type %q Accept %q, want %s", ct, acc, mediaFHIRJSON)
		}
	})
	t.Run("CRD stays application/json even under a framed FHIR type", func(t *testing.T) {
		p := newStubPartner(t)
		p.respByPath["/cds-services/order-dispatch-crd"] = []byte(`{"cards":[]}`)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
		ctx := withRequestMediaType(context.Background(), mediaFHIRJSON)
		if _, err := n.Handle(ctx, "crd-order-dispatch", "c", "pci", cdsRequest("order-dispatch")); err != nil {
			t.Fatal(err)
		}
		if ct, acc := p.lastHeader.Get("Content-Type"), p.lastHeader.Get("Accept"); ct != mediaJSON || acc != mediaJSON {
			t.Fatalf("CRD sent Content-Type %q Accept %q, want %s (path %s)", ct, acc, mediaJSON, p.lastPath)
		}
	})
}

// The provider's DTR ingress frames the media type the participant declared,
// so the payer gateway can send it on; an undeclared or unusable one frames
// application/fhir+json as before.
func TestDTRIngressFramesTheDeclaredMediaType(t *testing.T) {
	for declared, want := range map[string]string{
		"application/fhir+json; fhirVersion=4.0": "application/fhir+json; fhirVersion=4.0",
		"application/fhir+json":                  "application/fhir+json",
		"text/plain":                             "application/fhir+json",
	} {
		t.Run(declared, func(t *testing.T) {
			env := newInProcessExchange(t)
			env.originator.cfg.SoR = newPrefetchSoR().sor()
			declareFramedDTR(t, env, true)
			env.payerReturns(LegResult{Response: testResponse(packageAnswer)})
			body := ehrParams(ehrCoverageParam(prefetchMember, "00001"), ehrOrderParam("sr1", prefetchMember))
			req := httptest.NewRequest(http.MethodPost, "/Questionnaire/$questionnaire-package", bytes.NewReader(body))
			req.Header.Set("Content-Type", declared)
			rec := httptest.NewRecorder()
			env.originator.handleDTRIngress(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d %s", rec.Code, rec.Body.String())
			}
			hdr, _, err := shnsdk.DecodeHTTPFrame(env.lastRequestPayload())
			if err != nil {
				t.Fatal(err)
			}
			if got := hdr.Headers["Content-Type"]; got != want {
				t.Fatalf("framed Content-Type %q, want %q", got, want)
			}
		})
	}
}

// The receiver half, end to end through the payer gateway's real inbound
// handler: the media type a request frame declares reaches the payer's system
// on a FHIR leg, and never on a CDS Hooks leg, where an older sender's blanket
// application/fhir+json frame must still go out as application/json.
func TestPayerInboundSendsTheFramedMediaTypeOn(t *testing.T) {
	declared := "application/fhir+json; fhirVersion=4.0"
	t.Run("DTR package with a declared type", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementNone)
		body := dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember)), resourceParam("order", dtrOrder(dtrFrameMember)))
		got := p.sendFramed(t, "dtr-questionnaire-fetch", map[string]string{
			shnsdk.FrameHeaderOperation: shnsdk.FrameOperationQuestionnairePackage, "Content-Type": declared}, body, p.pci)
		if got.status != http.StatusOK {
			t.Fatalf("answer %d %s", got.status, got.body)
		}
		if ct, acc := p.partner.lastHeader.Get("Content-Type"), p.partner.lastHeader.Get("Accept"); ct != declared || acc != declared {
			t.Fatalf("payer received Content-Type %q Accept %q, want %q", ct, acc, declared)
		}
	})
	t.Run("DTR package from an older sender", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementNone)
		body := dtrPackage(resourceParam("coverage", dtrCoverage("cov-1", dtrFrameMember)), resourceParam("order", dtrOrder(dtrFrameMember)))
		got := p.sendFramed(t, "dtr-questionnaire-fetch", map[string]string{
			shnsdk.FrameHeaderOperation: shnsdk.FrameOperationQuestionnairePackage, "Content-Type": mediaFHIRJSON}, body, p.pci)
		if got.status != http.StatusOK {
			t.Fatalf("answer %d %s", got.status, got.body)
		}
		if ct := p.partner.lastHeader.Get("Content-Type"); ct != mediaFHIRJSON {
			t.Fatalf("payer received Content-Type %q", ct)
		}
	})
	t.Run("coverage eligibility to a declared endpoint", func(t *testing.T) {
		for framed, want := range map[string]string{declared: declared, "": mediaFHIRJSON} {
			p := eligibilityPayer(t, EnforcementNone, true)
			p.partner.respByPath[eligibilityPath] = payersEligibilityAnswer("Patient/" + dtrFrameMember)
			hdr := map[string]string{}
			if framed != "" {
				hdr["Content-Type"] = framed
			}
			got := p.sendFramed(t, "coverage-eligibility", hdr, eligibilityRequest(t, dtrFrameMember), p.pci)
			if got.status != http.StatusOK {
				t.Fatalf("answer %d %s", got.status, got.body)
			}
			if ct, acc := p.partner.lastHeader.Get("Content-Type"), p.partner.lastHeader.Get("Accept"); ct != want || acc != want {
				t.Fatalf("framed %q: payer received Content-Type %q Accept %q, want %q", framed, ct, acc, want)
			}
		}
	})
	t.Run("CRD under an older sender's fhir+json frame", func(t *testing.T) {
		p := newLevelPayer(t, EnforcementNone)
		body := conformantCRD("MBR-COVERED", "72148")
		got := p.sendFramed(t, "crd-order-select", map[string]string{"Content-Type": mediaFHIRJSON}, body, p.pci)
		if got.status != http.StatusOK {
			t.Fatalf("answer %d %s", got.status, got.body)
		}
		if ct, acc := p.partner.lastHeader.Get("Content-Type"), p.partner.lastHeader.Get("Accept"); ct != mediaJSON || acc != mediaJSON {
			t.Fatalf("CRD reached the CDS service as Content-Type %q Accept %q, want %s", ct, acc, mediaJSON)
		}
	})
}
