package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// cdsPayer is a payer's CDS Hooks server: a service listing and, per service
// id, a recorded request and a programmed answer.
type cdsPayer struct {
	srv *httptest.Server

	mu          sync.Mutex
	services    []CDSService
	listings    int
	posts       map[string][][]byte
	status      int
	contentType string
	answer      []byte
}

func newCDSPayer(t *testing.T, services ...CDSService) *cdsPayer {
	t.Helper()
	p := &cdsPayer{services: services, posts: map[string][][]byte{}, status: http.StatusOK, contentType: "application/json", answer: realCRDAnswer(t)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/cds-services" {
			p.listings++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"services": p.services})
			return
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/cds-services/")
		if r.Method != http.MethodPost || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		p.posts[id] = append(p.posts[id], b)
		w.Header().Set("Content-Type", p.contentType)
		w.WriteHeader(p.status)
		_, _ = w.Write(p.answer)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *cdsPayer) sent(id string) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.posts[id])
}

func (p *cdsPayer) sentAny() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, b := range p.posts {
		n += len(b)
	}
	return n
}

func (p *cdsPayer) respond(status int, contentType string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.contentType, p.answer = status, contentType, body
}

// readTestdata reads a file under testdata.
func readTestdata(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// realCRDAnswer is the reference payer's recorded order-sign answer.
func realCRDAnswer(t *testing.T) []byte {
	t.Helper()
	return readTestdata(t, "br-payer", "crd-response.json")
}

// referencePayerServices is the reference payer's listing for the hooks this
// network carries.
var referencePayerServices = []CDSService{
	{ID: "order-select-crd", Hook: "order-select"},
	{ID: "order-sign-crd", Hook: "order-sign"},
	{ID: "order-dispatch-crd", Hook: "order-dispatch"},
	{ID: "appointment-book-crd", Hook: "appointment-book"},
}

// cdsRequest is a CDS Hooks request with the given hook.
func cdsRequest(hook string) []byte {
	return []byte(`{"hookInstance":"h-1","hook":"` + hook + `","context":{"patientId":"p1"},"prefetch":{"coverage":{"resourceType":"Coverage","id":"c1"}}}`)
}

func legFor(hook string) string {
	if hook == "order-dispatch" {
		return "crd-order-dispatch"
	}
	return "crd-order-select"
}

// refusalBody is the JSON body a refusal carries.
func refusalBody(t *testing.T, lr LegResult) (msg string, offered []string) {
	t.Helper()
	if lr.Response.Ownership() != relay.OwnershipAuthored || lr.Response.Builder() != relay.BuilderGatewayRefusal {
		t.Fatalf("refusal body is %v %q, want the gateway's own refusal", lr.Response.Ownership(), lr.Response.Builder())
	}
	var body struct {
		Error   string    `json:"error"`
		Offered *[]string `json:"offered"`
	}
	if err := json.Unmarshal(relay.BytesForTest(lr.Response), &body); err != nil || body.Offered == nil {
		t.Fatalf("refusal body %s: %v", relay.BytesForTest(lr.Response), err)
	}
	return body.Error, *body.Offered
}

func TestServiceSelectedByHook(t *testing.T) {
	for _, hook := range []string{"order-select", "order-sign", "order-dispatch"} {
		t.Run(hook, func(t *testing.T) {
			p := newCDSPayer(t, referencePayerServices...)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
			req := cdsRequest(hook)
			res, err := n.Handle(context.Background(), legFor(hook), "corr", "pci", req)
			if err != nil || res.Status != 0 {
				t.Fatalf("Handle: %v %d %s", err, res.Status, res.Message)
			}
			sent := p.sent(hook + "-crd")
			if len(sent) != 1 || !bytes.Equal(sent[0], req) {
				t.Fatalf("service %s-crd received %q, want the request exactly", hook, sent)
			}
			if p.sentAny() != 1 {
				t.Fatalf("%d requests reached the payer, want 1", p.sentAny())
			}
		})
	}
	t.Run("the listing is read once and reused", func(t *testing.T) {
		p := newCDSPayer(t, referencePayerServices...)
		now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, func() time.Time { return now })
		for range 2 {
			if res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign")); err != nil || res.Status != 0 {
				t.Fatalf("Handle: %v %+v", err, res)
			}
		}
		if p.listings != 1 {
			t.Fatalf("listing read %d times, want 1", p.listings)
		}
		now = now.Add(cdsServiceListingTTL)
		if res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign")); err != nil || res.Status != 0 {
			t.Fatalf("Handle: %v %+v", err, res)
		}
		if p.listings != 2 {
			t.Fatalf("an expired listing was not read again (%d reads)", p.listings)
		}
	})
	t.Run("a configured service for the request's hook is used", func(t *testing.T) {
		p := newCDSPayer(t, append(slices.Clone(referencePayerServices), CDSService{ID: "pa-sign", Hook: "order-sign"})...)
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "pa-sign", nil, nil, WithCRDDispatchService("order-dispatch-crd"))
		for _, hook := range []string{"order-sign", "order-dispatch"} {
			if res, err := n.Handle(context.Background(), legFor(hook), "c", "pci", cdsRequest(hook)); err != nil || res.Status != 0 {
				t.Fatalf("%s: %v %+v", hook, err, res)
			}
		}
		if len(p.sent("pa-sign")) != 1 || len(p.sent("order-dispatch-crd")) != 1 || len(p.sent("order-sign-crd")) != 0 {
			t.Fatalf("posts %v", p.posts)
		}
	})
	for _, row := range []struct {
		name, leg, hook, msg string
		status               int
	}{
		{"a request without a hook", "crd-order-select", "", "names no hook", http.StatusBadRequest},
		{"a dispatch hook on the order-select leg", "crd-order-select", "order-dispatch", "hook order-dispatch is not carried on crd-order-select", http.StatusBadRequest},
		{"an ordering hook on the dispatch leg", "crd-order-dispatch", "order-sign", "hook order-sign is not carried on crd-order-dispatch", http.StatusBadRequest},
		{"a hook the network does not carry", "crd-order-select", "appointment-book", "hook appointment-book is not carried", http.StatusBadRequest},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := newCDSPayer(t, referencePayerServices...)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
			res, err := n.Handle(context.Background(), row.leg, "c", "pci", cdsRequest(row.hook))
			if err != nil || res.Status != row.status || !strings.Contains(res.Message, row.msg) {
				t.Fatalf("got %v %d %q, want %d %q", err, res.Status, res.Message, row.status, row.msg)
			}
			if p.sentAny() != 0 {
				t.Fatal("a refused request reached the payer")
			}
		})
	}
	t.Run("an unreadable listing refuses", func(t *testing.T) {
		for _, listing := range []string{`{"services":[{"id":"x"}]}`, `{}`, `not json`} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(listing))
					return
				}
				t.Error("a request was sent without a readable listing")
			}))
			n := NewNativeResponder(srv.Client(), srv.URL, "order-sign-crd", nil, nil)
			res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
			srv.Close()
			if err != nil || res.Status != http.StatusBadGateway || res.Message != "payer CDS service listing unavailable" {
				t.Fatalf("listing %s: %v %+v", listing, err, res)
			}
		}
	})
}

// TestServiceSelectedByHook_FailedListingReadAgain: a listing that could not
// be read is not kept. Requests within the short retry window are refused
// without another read; the first request after it reads the listing again
// and is served.
func TestServiceSelectedByHook_FailedListingReadAgain(t *testing.T) {
	var mu sync.Mutex
	reads, broken := 0, true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			reads++
			if broken {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"services": referencePayerServices})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(realCRDAnswer(t))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	n := NewNativeResponder(srv.Client(), srv.URL, "", nil, func() time.Time { return now })
	handle := func() LegResult {
		t.Helper()
		res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := handle(); res.Status != http.StatusBadGateway {
		t.Fatalf("unreadable listing: %+v", res)
	}
	mu.Lock()
	broken = false
	mu.Unlock()
	if res := handle(); res.Status != http.StatusBadGateway || reads != 1 {
		t.Fatalf("within the retry window: %+v after %d reads, want a refusal without another read", res, reads)
	}
	now = now.Add(cdsServiceListingRetryAfter)
	if res := handle(); res.Status != 0 || reads != 2 {
		t.Fatalf("after the retry window: %+v after %d reads, want the listing read again and the request served", res, reads)
	}
	if res := handle(); res.Status != 0 || reads != 2 {
		t.Fatalf("a listing read after a failure is kept: %+v after %d reads", res, reads)
	}
}

func TestServiceSelectedByHook_NoServiceForHookRefusedWithOffered(t *testing.T) {
	p := newCDSPayer(t, CDSService{ID: "sign", Hook: "order-sign"}, CDSService{ID: "dispatch", Hook: "order-dispatch"}, CDSService{ID: "sign-2", Hook: "order-sign"})
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
	res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-select"))
	if err != nil || res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("got %v %+v", err, res)
	}
	msg, offered := refusalBody(t, res)
	if msg != "payer offers no CDS service for hook order-select" || !slices.Equal(offered, []string{"order-sign", "order-dispatch"}) {
		t.Fatalf("refusal %q offered %v", msg, offered)
	}
	if res.Response.ContentType() != "application/json" {
		t.Fatalf("content type %q", res.Response.ContentType())
	}
	if p.sentAny() != 0 {
		t.Fatal("the refused request reached the payer")
	}
	t.Run("a configured service the payer does not list", func(t *testing.T) {
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "retired-service", nil, nil)
		res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
		if err != nil || res.Status != http.StatusUnprocessableEntity {
			t.Fatalf("got %v %+v", err, res)
		}
		if msg, offered := refusalBody(t, res); msg != "payer offers no CDS service retired-service" || !slices.Equal(offered, []string{"order-sign", "order-dispatch"}) {
			t.Fatalf("refusal %q offered %v", msg, offered)
		}
		if p.sentAny() != 0 {
			t.Fatal("the refused request reached the payer")
		}
	})
	t.Run("the refusal reaches the requester as the gateway's answer", func(t *testing.T) {
		want := relay.BytesForTest(res.Response)
		for _, framed := range []bool{true, false} {
			g, requester := newInboundTestGateway(t, framed)
			rec := httptest.NewRecorder()
			g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select",
				"corr-1", res, "pci-1", requester.ID, "", "")
			if !framed {
				if rec.Code != http.StatusUnprocessableEntity || !bytes.Equal(rec.Body.Bytes(), want) {
					t.Fatalf("legacy requester got %d %s", rec.Code, rec.Body.Bytes())
				}
				continue
			}
			hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			if err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if rec.Code != http.StatusOK || hdr.Status != http.StatusUnprocessableEntity || hdr.Headers["Content-Type"] != "application/json" || !bytes.Equal(body, want) {
				t.Fatalf("requester got %d %v %s", hdr.Status, hdr.Headers, body)
			}
		}
	})
}

func TestServiceSelectedByHook_SeveralServicesForHookAmbiguous(t *testing.T) {
	p := newCDSPayer(t, CDSService{ID: "a", Hook: "order-sign"}, CDSService{ID: "b", Hook: "order-sign"})
	n := NewNativeResponder(p.srv.Client(), p.srv.URL, "", nil, nil)
	res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign"))
	if err != nil || res.Status != http.StatusUnprocessableEntity || res.Message != "payer offers several CDS services for hook order-sign" {
		t.Fatalf("got %v %+v", err, res)
	}
	if p.sentAny() != 0 {
		t.Fatal("an ambiguous request reached the payer")
	}
	t.Run("a configured service settles it", func(t *testing.T) {
		n := NewNativeResponder(p.srv.Client(), p.srv.URL, "b", nil, nil)
		if res, err := n.Handle(context.Background(), "crd-order-select", "c", "pci", cdsRequest("order-sign")); err != nil || res.Status != 0 {
			t.Fatalf("got %v %+v", err, res)
		}
		if len(p.sent("b")) != 1 || len(p.sent("a")) != 0 {
			t.Fatalf("posts %v", p.posts)
		}
	})
}

func TestServiceSelectedByHook_OverrideHookMismatchRefused(t *testing.T) {
	for _, row := range []struct {
		name, override, dispatch, hook, msg, offered string
	}{
		{"order-select to the configured order-sign service", "order-sign-crd", "", "order-select", "payer CDS service order-sign-crd is for hook order-sign, not order-select", "order-sign"},
		{"order-sign to a configured order-select service", "order-select-crd", "", "order-sign", "payer CDS service order-select-crd is for hook order-select, not order-sign", "order-select"},
		{"order-dispatch to a configured ordering service", "", "order-sign-crd", "order-dispatch", "payer CDS service order-sign-crd is for hook order-sign, not order-dispatch", "order-sign"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := newCDSPayer(t, referencePayerServices...)
			n := NewNativeResponder(p.srv.Client(), p.srv.URL, row.override, nil, nil, WithCRDDispatchService(row.dispatch))
			res, err := n.Handle(context.Background(), legFor(row.hook), "c", "pci", cdsRequest(row.hook))
			if err != nil || res.Status != http.StatusUnprocessableEntity {
				t.Fatalf("got %v %+v", err, res)
			}
			if msg, offered := refusalBody(t, res); msg != row.msg || !slices.Equal(offered, []string{row.offered}) {
				t.Fatalf("refusal %q offered %v", msg, offered)
			}
			if p.sentAny() != 0 {
				t.Fatal("the refused request reached the payer")
			}
		})
	}
}

// crdAnswerFor is a payer's CRD answer in the reference payer's shape: no
// card, and the order returned in an update system action carrying cov as its
// coverage information.
func crdAnswerFor(cov shnsdk.CardCoverage) []byte {
	subs := []map[string]string{{"url": "covered", "valueCode": cov.Covered}}
	if cov.PANeeded != "" {
		subs = append(subs, map[string]string{"url": "pa-needed", "valueCode": cov.PANeeded})
	}
	for _, q := range cov.Questionnaires {
		subs = append(subs, map[string]string{"url": "questionnaire", "valueCanonical": q})
	}
	if cov.SatisfiedPaID != "" {
		subs = append(subs, map[string]string{"url": "satisfied-pa-id", "valueString": cov.SatisfiedPaID})
	}
	b, err := json.Marshal(map[string]any{
		"cards": []any{},
		"systemActions": []any{map[string]any{
			"type": "update", "description": "Add coverage information to the order",
			"resource": map[string]any{
				"resourceType": "ServiceRequest", "id": "sr1",
				"extension": []any{map[string]any{"url": shnsdk.CoverageInformationURL, "extension": subs}},
			},
		}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// TestCRDCoverage_ReadsTheOrderNotACardExtension: a payer's advisory card may
// carry a CDS Hooks extension object of its own; the coverage answer is still
// read from the order the payer returned.
func TestCRDCoverage_ReadsTheOrderNotACardExtension(t *testing.T) {
	answer := []byte(`{"cards":[{"summary":"Verify supplier status before dispatch","indicator":"info","source":{"label":"P","topic":` + crdTopic + `},"extension":{"davinci-crd.configuration":{"x":true}}}],` +
		`"systemActions":[{"type":"update","description":"d","resource":{"resourceType":"DeviceRequest","id":"dr1","extension":[{"url":"` + shnsdk.CoverageInformationURL + `","extension":[{"url":"covered","valueCode":"conditional"},{"url":"questionnaire","valueCanonical":"http://example.org/Questionnaire/HomeOxygen"}]}]}}]}`)
	cov, err := crdCoverage(answer)
	if err != nil || cov.Covered != "conditional" || !cov.NeedsDTR() || cov.Questionnaires[0] != "http://example.org/Questionnaire/HomeOxygen" {
		t.Fatalf("got %+v %v", cov, err)
	}
	if _, err := crdCoverage([]byte(`{"cards":[]}`)); err == nil {
		t.Fatal("an answer without coverage information must not read as a coverage answer")
	}
}
