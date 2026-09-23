package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A peer's absent media type is part of its answer. Exercise a real HTTP
// writer/client because net/http can infer Content-Type from nonempty bytes.
func TestParticipantAnswerHTTPMediaFidelity(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		media  string
		body   []byte
	}{
		{"empty error", 400, "", nil},
		{"opaque error", 429, "", []byte("peer failure\n")},
		{"no-content success", 204, "", nil},
		{"opaque success", 202, "", []byte("peer accepted\n")},
		{"declared error", 422, "application/problem+json", []byte(`{"error":"peer"}`)},
	} {
		for _, writer := range []string{"message", "legacy error"} {
			if writer == "legacy error" && row.status < 400 {
				continue
			}
			t.Run(row.name+"/"+writer, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					g := &Gateway{}
					if writer == "message" {
						g.writeApplicationReply(w, ApplicationReply{Status: row.status, Payload: relay.Exact(relay.NewBody(row.body, relay.OriginPeerFrame), row.media)}, "pas-claim")
						return
					}
					g.relayOriginationError(w, &RelayError{Status: row.status, Body: row.body, ContentType: row.media, leg: "pas-claim"})
				}))
				defer server.Close()
				resp, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				_, mediaPresent := resp.Header["Content-Type"]
				if resp.StatusCode != row.status || resp.Header.Get("Content-Type") != row.media || (row.media == "" && mediaPresent) || !bytes.Equal(body, row.body) {
					t.Fatalf("peer answer changed: status=%d media=%q present=%t body=%q; want %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), mediaPresent, body, row.status, row.media, row.body)
				}
			})
		}
	}
}

func TestNativeApplicationStatus(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 400, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			want := []byte("opaque\x00answer\n")
			if status == 204 || status == 422 {
				want = nil
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Type") != "application/fhir+json" {
					t.Errorf("request media changed: %q", r.Header.Get("Content-Type"))
				}
				w.Header().Set("Content-Type", "application/custom+json")
				w.WriteHeader(status)
				_, _ = w.Write(want)
			}))
			defer upstream.Close()
			n := NewNativeResponder(upstream.Client(), upstream.URL, "", nil, nil)
			result, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
			if err != nil {
				t.Fatal(err)
			}
			g, requester := newInboundTestGateway(t, true)
			rec := httptest.NewRecorder()
			r := newSignedInboundRequest(t, g, requester.ID)
			if status/100 == 2 {
				g.respondLeg(rec, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", "corr-1", result, "pci-1", requester.ID, "", "")
			} else {
				g.respondLegError(rec, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", "corr-1", result, "pci-1", requester.ID, "", "")
			}
			header, got, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if header.Status != status || header.Headers["Content-Type"] != "application/custom+json" || !bytes.Equal(got, want) {
				t.Fatalf("upstream reply changed: header=%+v body=%q", header, got)
			}
		})
	}
}

func TestNativeAbsentMediaErrorFrame(t *testing.T) {
	for _, row := range []struct {
		status int
		body   []byte
	}{{400, nil}, {429, []byte("opaque peer failure\n")}} {
		t.Run(fmt.Sprint(row.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header()["Content-Type"] = nil
				w.WriteHeader(row.status)
				_, _ = w.Write(row.body)
			}))
			defer upstream.Close()
			n := NewNativeResponder(upstream.Client(), upstream.URL, "", nil, nil)
			result, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
			if err != nil || result.ResponseContentType() != "" {
				t.Fatalf("native answer media=%q err=%v", result.ResponseContentType(), err)
			}
			g, requester := newInboundTestGateway(t, true)
			rec := httptest.NewRecorder()
			r := newSignedInboundRequest(t, g, requester.ID)
			g.respondLegError(rec, r, "payer-coverage", "dtr-questionnaire", "dtr-questionnaire-fetch", "corr-1", result, "pci-1", requester.ID, "", "")
			hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			if err != nil || hdr.Status != row.status || hdr.Headers["Content-Type"] != "" || !bytes.Equal(body, row.body) {
				t.Fatalf("peer frame changed: header=%+v body=%q err=%v", hdr, body, err)
			}
		})
	}
}

func TestApplicationReplyMessageAndLegacy(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 400, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := newTransportExchange(t)
			want := []byte("opaque\x00answer\n")
			if status == 204 || status == 422 {
				want = nil
			}
			version := "pa.crd@2.0"
			frame, err := shnsdk.EncodeHTTPFrameHeaders(status, map[string]string{"Content-Type": "application/octet-stream", shnsdk.FrameHeaderContractVersion: version}, want)
			if err != nil {
				t.Fatal(err)
			}
			sealBare(e, frame)
			var events []ObserverEvent
			e.originator.cfg.Observer = func(event ObserverEvent) { events = append(events, event) }
			reply, err := e.originator.OriginateLegMessage(e.ctx, e.req, e.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, ProfileID: version, Payload: testRequest(e.crdReq)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := reply.bytes("crd-order-select")
			if err != nil || reply.Status != status || reply.Payload.ContentType() != "application/octet-stream" || reply.DeclaredVersion != version || reply.VersionSource != "producer" || !bytes.Equal(got, want) {
				t.Fatalf("changed reply: %+v %q %v", reply, got, err)
			}
			rec := httptest.NewRecorder()
			e.originator.writeApplicationReply(rec, reply, "crd-order-select")
			assertRelayedVerbatim(t, rec, status, "application/octet-stream", want)
			if e.routeHitCount() != 1 {
				t.Fatal("message API dispatched more than once")
			}
			responses := 0
			observationFlush(t, e.originator)
			for _, event := range events {
				if event.Kind == "leg.failed" {
					t.Fatal("application reply classified as failure")
				}
				if event.Kind == "leg.response" {
					responses++
					if event.Status != status || !bytes.Equal(event.Payload, want) {
						t.Fatalf("wrong response event: %+v", event)
					}
				}
			}
			if responses != 1 {
				t.Fatalf("responses=%d", responses)
			}
			_, err = reply.legacy("crd-order-select", nil)
			if status/100 != 2 {
				var re *RelayError
				if !errors.As(fmt.Errorf("helper: %w", err), &re) || re.leg != "crd-order-select" || re.Status != status || !bytes.Equal(re.Body, want) || re.ContentType != "application/octet-stream" {
					t.Fatalf("legacy reply changed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApplicationResultContract(t *testing.T) {
	p := relay.Exact(relay.NewBody(nil, relay.OriginUpstreamResponse), "application/octet-stream")
	for _, r := range []LegResult{
		{Status: 422, ApplicationStatus: 500, Response: p},
		{ApplicationStatus: 200},
		{ApplicationStatus: 600, Response: p},
	} {
		if _, err := normalizeResult(r); err == nil {
			t.Fatalf("accepted invalid result: %+v", r)
		}
	}
	for _, r := range []LegResult{{Status: 422, Response: p}, {ApplicationStatus: 422, Response: p}, {Status: 422, ApplicationStatus: 422, Response: p}, {Response: p}} {
		got, err := normalizeResult(r)
		if err != nil || got.Response.Ownership() == 0 || got.ApplicationStatus == 0 {
			t.Fatalf("rejected application reply %+v: %v", r, err)
		}
	}
}

func TestApplicationEmptyErrorRemainsEmpty(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	rec := httptest.NewRecorder()
	g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select", "corr-1", LegResult{ApplicationStatus: 422, Response: relay.Exact(relay.NewBody(nil, relay.OriginUpstreamResponse), "application/problem+json")}, "pci-1", requester.ID, "", "")
	hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
	if err != nil || hdr.Status != 422 || hdr.Headers["Content-Type"] != "application/problem+json" || len(body) != 0 {
		t.Fatalf("empty application error changed: %+v %q %v", hdr, body, err)
	}
}

func TestApplicationBackendTimeoutIsNotReply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("expired request reached backend") }))
	defer upstream.Close()
	n := NewNativeResponder(upstream.Client(), upstream.URL, "", nil, nil)
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	result, err := n.Handle(dtrPkgCtx(ctx), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
	if !errors.Is(err, context.DeadlineExceeded) || result.Response.Ownership() != 0 || result.ApplicationStatus != 0 {
		t.Fatalf("timeout became application answer: %+v %v", result, err)
	}
}

func TestApplicationBackendRedirectPreservesFirstReply(t *testing.T) {
	for _, status := range []int{302, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			hits := 0
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
			defer second.Close()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", second.URL)
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("redirect answer"))
			}))
			defer first.Close()
			n := NewNativeResponder(first.Client(), first.URL, "", nil, nil)
			result, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
			if err != nil || result.ApplicationStatus != status || !bytes.Equal(responseBytes(result), []byte("redirect answer")) || hits != 0 {
				t.Fatalf("redirect changed reply or dispatched again: status=%d hits=%d err=%v", result.ApplicationStatus, hits, err)
			}
		})
	}
}

func TestApplicationBackendOverflowIsNotTruncatedReply(t *testing.T) {
	for _, status := range []int{200, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write(bytes.Repeat([]byte("x"), maxPartnerBody+1))
			}))
			defer upstream.Close()
			n := NewNativeResponder(upstream.Client(), upstream.URL, "", nil, nil)
			result, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
			if err == nil || result.Response.Ownership() != 0 {
				t.Fatalf("oversize reply became truncated answer: status=%d bytes=%d err=%v", result.ApplicationStatus, result.Response.Len(), err)
			}
		})
	}
}

func TestApplicationIngressPreservesStatusAndMedia(t *testing.T) {
	for _, status := range []int{201, 202, 400, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := newTransportExchange(t)
			want := []byte(`{"cards":[]}`)
			if status/100 != 2 {
				want = []byte("malformed { answer\n")
			}
			frame, err := shnsdk.EncodeHTTPFrame(status, "application/custom+json", want)
			if err != nil {
				t.Fatal(err)
			}
			sealBare(e, frame)
			rec := httptest.NewRecorder()
			e.originator.handleCRDIngress(rec, e.crdIngressRequest(t))
			assertRelayedVerbatim(t, rec, status, "application/custom+json", want)
			if e.routeHitCount() != 1 {
				t.Fatalf("dispatches=%d", e.routeHitCount())
			}
		})
	}
}

func TestApplicationWorkflowErrorRemainsResponse(t *testing.T) {
	e := newTransportExchange(t)
	want := []byte("not json\x00\n")
	frame, err := shnsdk.EncodeHTTPFrame(422, "text/plain", want)
	if err != nil {
		t.Fatal(err)
	}
	sealBare(e, frame)
	var events []ObserverEvent
	e.originator.cfg.Observer = func(event ObserverEvent) { events = append(events, event) }
	rec := httptest.NewRecorder()
	e.originator.handleUC03(rec, httptest.NewRequest(http.MethodPost, "/scenario/uc03", nil))
	assertRelayedVerbatim(t, rec, 422, "text/plain", want)
	responses := 0
	observationFlush(t, e.originator)
	for _, event := range events {
		if event.Kind == "leg.failed" {
			t.Fatal("application error classified as failure")
		}
		if event.Kind == "leg.response" {
			responses++
			if event.Status != 422 || !bytes.Equal(event.Payload, want) {
				t.Fatalf("wrong response event: %+v", event)
			}
		}
	}
	if responses != 1 || e.routeHitCount() != 1 {
		t.Fatalf("responses=%d dispatches=%d", responses, e.routeHitCount())
	}
}

func TestApplicationResponderConflictRefusesBeforeSeal(t *testing.T) {
	g, requester := newInboundTestGateway(t, true)
	rec := httptest.NewRecorder()
	g.respondLeg(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select", "corr-1", LegResult{Status: 422, ApplicationStatus: 202, Response: testResponse([]byte(`{}`))}, "pci-1", requester.ID, "", "")
	if rec.Code != 500 {
		t.Fatalf("conflicting responder fields not refused: %d %s", rec.Code, rec.Body.String())
	}
}

func TestApplicationRequestCarriesSuppliedMediaAndDeclaration(t *testing.T) {
	e := newTransportExchange(t)
	holder, _ := e.originator.cfg.Reg.Lookup(e.payerID)
	holder.RequestFrames = []string{shnsdk.RequestFrameV1}
	e.originator.cfg.Reg.Set(e.payerID, holder)
	payload := relay.Exact(relay.NewBody([]byte("opaque request"), relay.OriginIngressRequest), "application/custom")
	_, err := e.originator.OriginateLegMessage(e.ctx, e.req, e.payerID, "crd-order-select", "pci-1", "corr-1", "", Content{WorkstreamType: workstreamPA, ProfileID: "pa.crd@2.0", DeclaredVersion: "pa.crd@2.1", VersionSource: "producer", Payload: payload, Carried: true})
	if err != nil {
		t.Fatal(err)
	}
	e.substrate.mu.Lock()
	wire := bytes.Clone(e.substrate.lastReq)
	e.substrate.mu.Unlock()
	hdr, body, err := shnsdk.DecodeHTTPFrame(wire)
	if err != nil || hdr.Headers["Content-Type"] != "application/custom" || hdr.Headers[shnsdk.FrameHeaderContractVersion] != "pa.crd@2.1" || !bytes.Equal(body, []byte("opaque request")) {
		t.Fatalf("request changed: %+v %q %v", hdr, body, err)
	}
}

func TestApplicationReplyExplicitResponseDeclaration(t *testing.T) {
	result := LegResult{ApplicationStatus: 202, Response: relay.Exact(relay.NewBody([]byte(`{}`), relay.OriginUpstreamResponse), "application/json"), ResponseContractVersion: "pa.pas@2.1", ResponseVersionSource: "producer"}
	if got := stampForBuiltAnswer(result, "pa.pas@2.0"); got != result.ResponseContractVersion {
		t.Fatalf("producer declaration lost: %q", got)
	}
}

// A reader reports a partial body before a deterministic deadline error. A
// complete HTTP response header is not evidence that its body was received.
func TestApplicationPartialBodyDeadlineIsNotReply(t *testing.T) {
	for _, status := range []int{202, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/fhir+json"}}, Body: &partialDeadlineBody{}}, nil
			})}
			n := NewNativeResponder(client, "https://synthetic-participant.example", "", nil, nil)
			result, err := n.Handle(dtrPkgCtx(context.Background()), "dtr-questionnaire-fetch", "corr", "pci", dtrFetchReq)
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || result.ApplicationStatus != 0 || result.Response.Ownership() != 0 {
				t.Fatalf("partial read became reply: %+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

type partialDeadlineBody struct{ sent bool }

func (b *partialDeadlineBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, []byte("partial private body")), nil
	}
	return 0, context.DeadlineExceeded
}
func (*partialDeadlineBody) Close() error { return nil }
