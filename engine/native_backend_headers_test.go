package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

// A partner system that routes on a fixed request header (a tenant or
// plan key its API gateway reads before any payload) needs that header on
// every request this gateway sends it: the CDS listing read, each CRD post,
// the DTR and PAS operations. WithBackendHeaders
// carries it; the bytes of the message are untouched.
func TestNativeResponder_BackendHeadersOnEveryPartnerRequest(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Library"}}]}`)
	p.respByPath["/cds-services/order-sign"] = []byte(`{"cards":[]}`)
	hdr := http.Header{"X-Route-Key": {"plan-7"}, "X-Tenant": {"t-1"}}
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "order-sign", nil, nil, WithBackendHeaders(hdr))

	want := func(t *testing.T, got http.Header, where string) {
		t.Helper()
		for k, v := range hdr {
			if got.Get(k) != v[0] {
				t.Errorf("%s: header %s = %q, want %q", where, k, got.Get(k), v[0])
			}
		}
	}

	t.Run("DTR questionnaire-package post", func(t *testing.T) {
		res, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: status=%d err=%v", res.Status, err)
		}
		want(t, p.lastHeader, "DTR post")
	})

	t.Run("CDS listing read and CRD post", func(t *testing.T) {
		res, err := n.Handle(context.Background(), "crd-order-select", "corr", "pci", cdsRequest("order-sign"))
		if err != nil || res.Status != 0 {
			t.Fatalf("Handle: status=%d err=%v", res.Status, err)
		}
		want(t, p.listingHeader, "CDS listing GET")
		want(t, p.lastHeader, "CRD post")
	})

	t.Run("message bytes are untouched", func(t *testing.T) {
		if _, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
			t.Fatal(err)
		}
		var sent map[string]any
		if err := json.Unmarshal(p.lastBody, &sent); err != nil {
			t.Fatalf("partner received %q: %v", p.lastBody, err)
		}
		if sent["resourceType"] != "Parameters" {
			t.Errorf("partner received %s, want the Parameters request", p.lastBody)
		}
	})
}

// Without the option nothing is added: the requests are the ones every
// deployment sends today.
func TestNativeResponder_NoBackendHeadersByDefault(t *testing.T) {
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Library"}}]}`)
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
	if _, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq); err != nil {
		t.Fatal(err)
	}
	for k := range p.lastHeader {
		if k == "X-Route-Key" || k == "X-Tenant" {
			t.Errorf("header %s sent without the option", k)
		}
	}
}

// The header is addressing for the partner's own system, never credential
// material: the token endpoint (a different party in many deployments) does
// not receive it, while the partner request carries both it and the bearer
// that endpoint issued.
func TestNativeResponder_BackendHeadersNeverReachTokenEndpoint(t *testing.T) {
	var tokenHeader http.Header
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"bearer","expires_in":300}`))
	}))
	t.Cleanup(tokenSrv.Close)
	p := newStubPartner(t)
	p.respByPath["/Questionnaire/$questionnaire-package"] = []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Library"}}]}`)

	client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tokenSrv.URL, ClientID: "c1", ClientSecret: "s1", Scope: "x"})
	if err != nil {
		t.Fatal(err)
	}
	n := NewNativeResponder(client, p.srv.URL, "", nil, nil, WithBackendHeaders(http.Header{"X-Route-Key": {"plan-7"}}))
	res, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
	if err != nil || res.Status != 0 {
		t.Fatalf("Handle: status=%d err=%v", res.Status, err)
	}
	if tokenHeader == nil {
		t.Fatal("the token endpoint was never called")
	}
	if got := tokenHeader.Get("X-Route-Key"); got != "" {
		t.Errorf("token endpoint received X-Route-Key=%q; the header must never reach it", got)
	}
	if got := p.lastHeader.Get("X-Route-Key"); got != "plan-7" {
		t.Errorf("partner received X-Route-Key=%q, want plan-7", got)
	}
	if got := p.lastHeader.Get("Authorization"); got != "Bearer tok-1" {
		t.Errorf("partner Authorization = %q, want the issued bearer", got)
	}
}
