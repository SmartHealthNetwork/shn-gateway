package engine

import (
	"bytes"
	"encoding/json"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A successful response must never present discarded observation bytes as its
// actual empty body, including when only the legacy observer is configured.
func TestObserverConcurrentCaptureLossIsExplicit(t *testing.T) {
	var mu sync.Mutex
	var events []ObserverEvent
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: func(e ObserverEvent) {
		mu.Lock()
		defer mu.Unlock()
		e.Payload = bytes.Clone(e.Payload)
		events = append(events, e)
	}}}
	read := make(chan struct{}, 9)
	written := make(chan struct{}, 9)
	writeRelease := make(chan struct{})
	finish := make(chan struct{})
	h := g.observeIngress("test", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		read <- struct{}{}
		<-writeRelease
		w.Write([]byte(`{"answer":"nonempty"}`))
		written <- struct{}{}
		<-finish
	})
	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"request":true}`)))
			observationFlush(t, g)
			if w.Code != 200 || w.Body.String() != `{"answer":"nonempty"}` {
				t.Errorf("capture changed response: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	for i := 0; i < 9; i++ {
		<-read
	}
	close(writeRelease)
	for i := 0; i < 9; i++ {
		<-written
	}
	close(finish)
	wg.Wait()
	observationFlush(t, g)
	if len(events) != 18 {
		t.Fatalf("events=%d, want 18", len(events))
	}
	var missingRequest, missingResponse bool
	for _, e := range events {
		expected := `{"request":true}`
		if e.Kind == "ingress.responded" {
			expected = `{"answer":"nonempty"}`
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		json.Unmarshal(raw, &fields)
		if string(e.Payload) != expected {
			if fields["payloadIncomplete"] != true {
				t.Fatalf("silent %s loss: %s", e.Kind, raw)
			}
			if e.Kind == "ingress.responded" {
				missingResponse = true
				if e.Detail != "200" {
					t.Fatalf("status changed: %+v", e)
				}
			} else {
				missingRequest = true
			}
		} else if _, ok := fields["payloadIncomplete"]; ok {
			t.Fatalf("complete event JSON gained an incomplete flag: %s", raw)
		}
	}
	if !missingRequest || !missingResponse {
		t.Fatalf("fixture did not exercise request and response shedding: request=%v response=%v", missingRequest, missingResponse)
	}
}

func TestObserverUnreadBodyIsExplicitlyIncomplete(t *testing.T) {
	var first ObserverEvent
	g := &Gateway{cfg: Config{Clock: time.Now, Observer: func(e ObserverEvent) {
		if e.Kind == "ingress.received" {
			first = e
		}
	}}}
	w := httptest.NewRecorder()
	g.observeIngress("pas", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })(w, httptest.NewRequest("POST", "/Claim/$submit", bytes.NewBufferString("unread")))
	observationFlush(t, g)
	raw, _ := json.Marshal(first)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if w.Code != 401 || fields["payloadIncomplete"] != true {
		t.Fatalf("unread body evidence: status=%d event=%s", w.Code, raw)
	}
}

func TestObserverPayloadCompleteness(t *testing.T) {
	large := bytes.Repeat([]byte(" "), (8<<20)+1)
	for _, tc := range []struct {
		name                                  string
		request, response                     []byte
		consume                               bool
		requestIncomplete, responseIncomplete bool
	}{
		{name: "complete", request: []byte(`{}`), response: []byte(`{}`), consume: true},
		{name: "empty", consume: true},
		{name: "request retention cap", request: large, response: []byte(`{}`), consume: true, requestIncomplete: true},
		{name: "response retention cap", response: large, consume: true, responseIncomplete: true},
		{name: "unread", request: []byte(`{}`), response: []byte(`{}`), requestIncomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []ObserverEvent
			g := &Gateway{cfg: Config{Clock: time.Now, Observer: func(e ObserverEvent) { events = append(events, e) }}}
			w := httptest.NewRecorder()
			g.observeIngress("test", func(w http.ResponseWriter, r *http.Request) {
				if tc.consume {
					io.Copy(io.Discard, r.Body)
				}
				w.Write(tc.response)
			})(w, httptest.NewRequest("POST", "/", bytes.NewReader(tc.request)))
			observationFlush(t, g)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tc.response) {
				t.Fatal("observation changed response")
			}
			if len(events) != 2 || events[0].Kind != "ingress.received" || events[0].PayloadIncomplete != tc.requestIncomplete || events[1].PayloadIncomplete != tc.responseIncomplete {
				t.Fatalf("request incomplete=%v, response incomplete=%v", events[0].PayloadIncomplete, events[1].PayloadIncomplete)
			}
		})
	}
}

// Retain a foreign error's declared media type, including its absence.
func TestRelayedErrorPreservesDeclaredOrAbsentMediaType(t *testing.T) {
	for _, mediaType := range []string{"application/problem+json", "text/plain; charset=utf-8", ""} {
		t.Run(mediaType, func(t *testing.T) {
			g, requester := newInboundTestGateway(t, true)
			raw := []byte("synthetic upstream error\n")
			rec := httptest.NewRecorder()
			g.respondLegError(rec, newSignedInboundRequest(t, g, requester.ID), "payer-coverage", "crd-cards", "crd-order-select", "corr-1", LegResult{Status: 429, Response: relay.Exact(relay.NewBody(raw, relay.OriginUpstreamResponse), mediaType)}, "pci-1", requester.ID, "", "")
			hdr, body, err := shnsdk.DecodeHTTPFrame(openResponseLeg(t, requester, rec.Body.Bytes()))
			_, present := hdr.Headers["Content-Type"]
			if err != nil || hdr.Status != 429 || hdr.Headers["Content-Type"] != mediaType || (mediaType == "" && present) || !bytes.Equal(body, raw) {
				t.Fatalf("framed error: header=%+v body=%q err=%v", hdr, body, err)
			}
		})
	}
}
