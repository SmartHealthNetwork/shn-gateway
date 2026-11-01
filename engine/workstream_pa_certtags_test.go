//go:build !shn_coreonly

package engine

import (
	"net/http"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// tagSpy records the endpoint tag each request's context carries.
type tagSpy struct {
	base http.RoundTripper
	mu   sync.Mutex
	tags []string
}

func (s *tagSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	tag, ok := endpointcert.TagOf(r.Context())
	name := "untagged"
	if ok {
		name = tag.Kind().String()
	}
	s.mu.Lock()
	s.tags = append(s.tags, r.URL.Path+" "+name)
	s.mu.Unlock()
	return s.base.RoundTrip(r)
}

// MON-R25: each payer setting that can name a host of its own is its own
// kind, so every forward the payer gateway makes to its system marks its
// request with the kind of the setting its URL came from: CDS for the CDS
// Hooks base, DTR, PAS and eligibility for theirs. Unmarked requests go to
// payer-davinci, the shared base.
func TestMONR25PayerForwardsMarkTheirSettingsKind(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		leg, operation string
		body           func(t *testing.T) []byte
		want           string
	}{
		{"crd-order-select", "", func(*testing.T) []byte { return conformantCRD("MBR-COVERED", "72148") }, "cds"},
		{"dtr-questionnaire-fetch", shnsdk.FrameOperationQuestionnairePackage, func(*testing.T) []byte { return dtrFetchReq }, "payer-dtr"},
		{"pas-claim", "", func(t *testing.T) []byte { return originatorBuiltConformantBundle(t, "MBR-COVERED") }, "payer-pas"},
		{"pas-claim-inquire", "", func(*testing.T) []byte { return inquiryBundle("MBR-COVERED", "", "TRN-1", "72148") }, "payer-pas"},
		{"coverage-eligibility", "", func(t *testing.T) []byte { return eligibilityRequest(t, dtrFrameMember) }, "payer-eligibility"},
	} {
		t.Run(r.leg, func(t *testing.T) {
			p := eligibilityPayer(t, EnforcementObserve, true)
			n := p.g.cfg.Responder.(*nativeResponder)
			base := n.client.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			spy := &tagSpy{base: base}
			c := *n.client
			c.Transport = spy
			n.client = &c
			p.send(t, r.leg, r.operation, r.body(t))
			if len(spy.tags) == 0 {
				t.Fatal("no request reached the payer's system")
			}
			for _, got := range spy.tags {
				if got[len(got)-len(r.want):] != r.want {
					t.Errorf("request %q, want every request marked %s", got, r.want)
				}
			}
		})
	}
}
