package engine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

func TestIngressContextCRDFrameContract(t *testing.T) {
	for _, frames := range [][]string{nil, {shnsdk.RequestFrameV1}, {shnsdk.RequestFrameV1Op}, {shnsdk.RequestFrameV1CRD}} {
		t.Run("frames-"+joinFrames(frames), func(t *testing.T) {
			env := newTransportExchange(t)
			advertiseRecipientFrameV1(t, env)
			answer, frameErr := shnsdk.EncodeHTTPFrame(http.StatusOK, "application/json", []byte(`{"cards":[]}`))
			if frameErr != nil {
				t.Fatal(frameErr)
			}
			sealBare(env, answer)
			peer, _ := env.originator.cfg.Reg.Lookup(env.payerID)
			peer.RequestFrames = frames
			env.originator.cfg.Reg.Set(env.payerID, peer)
			_, err := env.originator.OriginateLegMessage(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-crd-frame", "", Content{WorkstreamType: workstreamPA, CRDHook: "order-sign", Payload: testRequest(env.crdReq)})
			if !shnsdk.SupportsRequestFrameV1CRD(frames) {
				if !errors.Is(err, errFramedCRDUnsupported) || env.routeHitCount() != 0 {
					t.Fatalf("err=%v dispatch=%d", err, env.routeHitCount())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h, b, e := shnsdk.DecodeHTTPFrame(env.lastRequestPayload())
			if e != nil || h.Headers[shnsdk.FrameHeaderCRDHook] != "order-sign" || string(b) != string(env.crdReq) {
				t.Fatalf("header=%+v body=%q err=%v", h, b, e)
			}
		})
	}
	for _, row := range []struct{ leg, hook string }{{"pas-claim", "order-sign"}, {"crd-order-select", "order-dispatch"}, {"crd-order-dispatch", "order-select"}} {
		t.Run(row.leg+row.hook, func(t *testing.T) {
			env := newTransportExchange(t)
			peer, _ := env.originator.cfg.Reg.Lookup(env.payerID)
			peer.RequestFrames = []string{shnsdk.RequestFrameV1CRD}
			env.originator.cfg.Reg.Set(env.payerID, peer)
			_, err := env.originator.OriginateLegMessage(env.ctx, env.req, env.payerID, row.leg, "pci-1", "corr-invalid-hook", "", Content{WorkstreamType: workstreamPA, CRDHook: row.hook, Payload: testRequest(env.crdReq)})
			if err == nil || env.routeHitCount() != 0 {
				t.Fatalf("err=%v dispatch=%d", err, env.routeHitCount())
			}
			frame, e := shnsdk.EncodeHTTPFrameHeaders(200, map[string]string{shnsdk.FrameHeaderCRDHook: row.hook}, []byte("{opaque"))
			if e != nil {
				t.Fatal(e)
			}
			if _, status, _ := inboundFrameCRDHook(row.leg, frame); status != http.StatusBadRequest {
				t.Fatalf("invalid inbound hook status=%d", status)
			}
		})
	}
}
func joinFrames(frames []string) string {
	if len(frames) == 0 {
		return "none"
	}
	return frames[0]
}
func TestIngressContextErrorCarrier(t *testing.T) {
	for _, status := range []int{400, 401, 403, 503} {
		w := httptest.NewRecorder()
		g := &Gateway{}
		if !g.relayOriginationError(w, contextError(status, "context_invalid")) || w.Code != status || w.Body.String() != "{\"error\":\"context_invalid\"}\n" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if status == 503 && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("outage cacheable")
		}
	}
}

func TestIngressContextEngineRefusesCRDHookOnResponse(t *testing.T) {
	for _, hook := range []string{"", "order-sign"} {
		t.Run("hook="+hook, func(t *testing.T) {
			env := newTransportExchange(t)
			headers := map[string]string{"Content-Type": "application/json"}
			if hook != "" {
				headers[shnsdk.FrameHeaderCRDHook] = hook
			}
			body := []byte(`{"cards":[]}`)
			frame, err := shnsdk.EncodeHTTPFrameHeaders(http.StatusOK, headers, body)
			if err != nil {
				t.Fatal(err)
			}
			sealBare(env, frame)
			reply, err := env.originator.OriginateLegMessage(env.ctx, env.req, env.payerID, "crd-order-select", "pci-1", "corr-response-hook", "", Content{WorkstreamType: workstreamPA, Payload: testRequest(env.crdReq)})
			if env.routeHitCount() != 1 {
				t.Fatalf("did not reach authenticated response: dispatches=%d", env.routeHitCount())
			}
			if hook == "" {
				if err != nil || reply.Status != http.StatusOK {
					t.Fatalf("valid baseline response refused: %+v %v", reply, err)
				}
				return
			}
			if err == nil || err.Error() != "CRD hook is request-only" || reply.Status != 0 || reply.Payload.Ownership() != 0 {
				t.Fatalf("request-only response header not refused: %+v %v", reply, err)
			}
		})
	}
}
