//go:build !shn_coreonly

package engine

import (
	"context"
	"strings"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// HRex 1.2.0 defines the PAS and DTR endpoints a payer publishes in its
// .well-known/davinci-configuration as the base each operation is invoked on
// (davinci_pas_submission_endpoint: the base for $submit and $inquire;
// davinci_dtr_qpackage_endpoint: the base for $questionnaire-package). Every
// forward that follows a published value (same-origin, for the routed line)
// invokes its own operation under it, as the CDex forward already does
// (TestCDexPayer_ResolvesTheSubmitAttachmentURL). The value is read as a base
// whatever its path holds: nothing is inferred from how the path ends.
func TestNativeForwardInvokesTheOperationUnderThePublishedBase(t *testing.T) {
	approved := fixturePASResponse(t, []byte(`{"resourceType":"ClaimResponse","outcome":"complete","preAuthRef":"P-1","preAuthPeriod":{"end":"2030-01-01"}}`), true)
	inquiryAnswer := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"ClaimResponse","outcome":"complete","patient":{"reference":"Patient/SubscriberExample"}}}]}`)
	const pci, origCorr = "PCI-HREX-BASE", "corr-hrex-base-submit"

	type op struct {
		leg, line, path string
		answer          []byte
		ctx             func(context.Context) context.Context
		body            func(t *testing.T) []byte
		store           func(t *testing.T) *censusSoR
	}
	plain := func(context.Context) context.Context { return context.Background() }
	ops := map[string]op{
		"PAS $submit": {leg: "pas-claim", line: "pa.pas@2.2", path: "/Claim/$submit", answer: approved, ctx: plain,
			body:  func(t *testing.T) []byte { return conformantSubmitBundle(t, ownIdentity, true) },
			store: func(*testing.T) *censusSoR { return newCensusSoR() }},
		"PAS update ($submit)": {leg: "pas-claim-update", line: "pa.pas@2.2", path: "/Claim/$submit", answer: approved, ctx: plain,
			body: func(t *testing.T) []byte {
				return conformantUpdateBundle(t, ownIdentity, true, "corr-hrex-base-update", origCorr)
			},
			store: func(t *testing.T) *censusSoR {
				s := newCensusSoR()
				if err := s.RecordPendedClaim(pci, origCorr); err != nil {
					t.Fatalf("RecordPendedClaim: %v", err)
				}
				return s
			}},
		"PAS $inquire": {leg: "pas-claim-inquire", line: "pa.pas@2.2", path: "/Claim/$inquire", answer: inquiryAnswer, ctx: plain,
			body:  func(*testing.T) []byte { return inquiryBundleWithPayor(ownIdentity) },
			store: func(*testing.T) *censusSoR { return newCensusSoR() }},
		"DTR $questionnaire-package": {leg: "dtr-questionnaire-fetch", line: "pa.dtr@2.2", path: "/Questionnaire/$questionnaire-package",
			answer: []byte(`{"resourceType":"Bundle","type":"collection","entry":[]}`), ctx: dtrPkgCtx,
			body:  func(*testing.T) []byte { return dtrFetchReq },
			store: func(*testing.T) *censusSoR { return newCensusSoR() }},
		"DTR $next-question": {leg: "dtr-questionnaire-fetch", line: "pa.dtr@2.2", path: "/Questionnaire/$next-question",
			answer: []byte(nextQuestionQR("MBR-1")),
			ctx: func(ctx context.Context) context.Context {
				return withRequestFrameOperation(ctx, shnsdk.FrameOperationNextQuestion)
			},
			body:  func(*testing.T) []byte { return []byte(nextQuestionQR("MBR-1")) },
			store: func(*testing.T) *censusSoR { return newCensusSoR() }},
	}
	// published is the value the payer publishes for the routed line, under
	// the stub's origin; want is the path its system is called at, before the
	// operation's own path.
	rows := map[string]struct{ published, want string }{
		"a published base":                        {"/pas22", "/pas22"},
		"a published base with a trailing slash":  {"/pas22/", "/pas22"},
		"a published bare origin":                 {"", ""},
		"a published bare origin with a slash":    {"/", ""},
		"a value whose path names the operation":  {"/pas22{op}", "/pas22{op}"},
		"no published value: the configured base": {"-", ""},
		// A value on another origin is never followed (SetEndpointEvidence):
		// the forward goes under the configured base.
		"a published base on another origin": {"https://elsewhere.example/pas22", ""},
		// The query does not swallow the operation's path, which goes under
		// the base's path (TestPublishedOperationURL pins the query itself).
		"a published base with a query": {"/pas22?tenant=7", "/pas22"},
	}
	for opName, o := range ops {
		for rowName, row := range rows {
			t.Run(opName+"/"+rowName, func(t *testing.T) {
				p := newStubPartner(t)
				n := NewNativeResponder(p.srv.Client(), p.srv.URL, fixedClock, WithCRDService("shn-order-select"), WithPASStore(o.store(t)))
				want := strings.ReplaceAll(row.want, "{op}", o.path) + o.path
				p.respByPath[want] = o.answer
				switch {
				case row.published == "-":
				case strings.HasPrefix(row.published, "https://"):
					n.SetEndpointEvidence(map[string]string{o.line: row.published})
					if got := n.EndpointEvidenceForTest(); len(got) != 0 {
						t.Fatalf("a published value on another origin was kept: %v", got)
					}
				default:
					n.SetEndpointEvidence(map[string]string{o.line: p.srv.URL + strings.ReplaceAll(row.published, "{op}", o.path)})
					if got := n.EndpointEvidenceForTest(); len(got) != 1 {
						t.Fatalf("the same-origin published value was not kept: %v", got)
					}
				}
				ctx := withAnswerLine(o.ctx(context.Background()), o.line)
				if _, err := n.Handle(ctx, o.leg, "corr", pci, o.body(t)); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if p.lastPath != want {
					t.Fatalf("the payer's system was called at %q, want %q", p.lastPath, want)
				}
			})
		}
	}
}

// publishedOperationURL adds the operation's path where the published
// value's path ends, before any query or fragment, and changes no other byte
// of the URL it builds. (How the HTTP client then escapes that URL on the wire
// is the client's, the same for any URL the gateway posts to.)
func TestPublishedOperationURL(t *testing.T) {
	t.Parallel()
	for base, want := range map[string]string{
		"https://p.example/fhir":               "https://p.example/fhir/Claim/$submit",
		"https://p.example/fhir/":              "https://p.example/fhir/Claim/$submit",
		"https://p.example/fhir//":             "https://p.example/fhir/Claim/$submit",
		"https://p.example":                    "https://p.example/Claim/$submit",
		"https://p.example/":                   "https://p.example/Claim/$submit",
		"https://p.example:8443/fhir":          "https://p.example:8443/fhir/Claim/$submit",
		"https://p.example/fhir?tenant=7":      "https://p.example/fhir/Claim/$submit?tenant=7",
		"https://p.example/fhir/?tenant=7":     "https://p.example/fhir/Claim/$submit?tenant=7",
		"https://p.example?tenant=7":           "https://p.example/Claim/$submit?tenant=7",
		"https://p.example/fhir#frag":          "https://p.example/fhir/Claim/$submit#frag",
		"https://p.example/a%2Fb/fhir":         "https://p.example/a%2Fb/fhir/Claim/$submit",
		"https://p.example/a%2Fb/fhir%2F":      "https://p.example/a%2Fb/fhir%2F/Claim/$submit",
		"https://p.example/fhir/%2F":           "https://p.example/fhir/%2F/Claim/$submit",
		"https://p.example/a%2Fb/fhír":         "https://p.example/a%2Fb/fhír/Claim/$submit",
		"https://p.example/a b%2F":             "https://p.example/a b%2F/Claim/$submit",
		"https://p.example/fhir;v=2":           "https://p.example/fhir;v=2/Claim/$submit",
		"HTTPS://P.EXAMPLE/fhir":               "HTTPS://P.EXAMPLE/fhir/Claim/$submit",
		"https://u:p@p.example/fhir":           "https://u:p@p.example/fhir/Claim/$submit",
		"https://p.example/fhir/../r4":         "https://p.example/fhir/../r4/Claim/$submit",
		"https://p.example/fhir//r4":           "https://p.example/fhir//r4/Claim/$submit",
		"https://p.example/fhir/Claim/$submit": "https://p.example/fhir/Claim/$submit/Claim/$submit",
	} {
		if got := publishedOperationURL(base, "/Claim/$submit"); got != want {
			t.Errorf("publishedOperationURL(%q) = %q, want %q", base, got, want)
		}
	}
}
