package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

func awaitPopulateSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("population fixture did not reach expected boundary")
	}
}

// Expire the actual outer client timeout, which can replace the transport's
// error chain. Neither a synthetic deadline error nor a timed server sleep
// reproduces the request/cancellation coordination asserted here.
func TestNativePopulateFailureOuterTimeout(t *testing.T) {
	for _, tokenTimeout := range []bool{true, false} {
		name, stage := "resource timeout", "transport"
		if tokenTimeout {
			name, stage = "token timeout", "token_acquisition"
		}
		t.Run(name, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			var tokenCalls, calls atomic.Int32
			waitForCancel := func(r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				if tokenTimeout {
					waitForCancel(r)
					return
				}
				_, _ = io.WriteString(w, `{"access_token":"CANARY-BEARER","expires_in":3600}`)
			}))
			defer token.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer CANARY-BEARER" {
					t.Error("missing bearer")
				}
				waitForCancel(r)
			}))
			defer resource.Close()
			client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL + "/CANARY-TOKEN", ClientID: "CANARY-CLIENT", ClientSecret: "CANARY-SECRET", HTTPClient: token.Client()})
			if err != nil {
				t.Fatal(err)
			}
			client.Timeout = time.Second
			var notes []PopulateFailure
			pop := NewNativePopulatorWithFailureObserver(client, resource.URL+"/CANARY-URL", func(note PopulateFailure) { notes = append(notes, note) })
			done := make(chan struct{})
			var qr []byte
			var populateErr error
			go func() {
				defer close(done)
				qr, _, populateErr = pop.Populate(context.Background(), []byte(failurePackage), failureContext)
			}()
			awaitPopulateSignal(t, entered)
			awaitPopulateSignal(t, done)
			awaitPopulateSignal(t, canceled)
			if populateErr != errPopulateUpstream || qr != nil {
				t.Fatalf("error=%v qr=%s", populateErr, qr)
			}
			if len(notes) != 1 || notes[0] != (PopulateFailure{Stage: stage, Reason: "deadline", Status: 0}) {
				t.Fatalf("notes=%#v", notes)
			}
			wantCalls := int32(1)
			if tokenTimeout {
				wantCalls = 0
			}
			if tokenCalls.Load() != 1 || calls.Load() != wantCalls {
				t.Fatalf("token=%d population=%d", tokenCalls.Load(), calls.Load())
			}
			wire, err := json.Marshal(notes)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "CANARY") || strings.Contains(string(wire), token.URL) || strings.Contains(string(wire), resource.URL) {
				t.Fatalf("sensitive note=%s", wire)
			}
		})
	}
}

func TestNativePopulateFailureObservationIsolation(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "sequential"
		if concurrent {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			firstToken, releaseFirst, secondRequest := make(chan struct{}), make(chan struct{}), make(chan struct{})
			resourceEntered, resourceCanceled := make(chan struct{}), make(chan struct{})
			var tokenCalls, calls, requests atomic.Int32
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tokenCalls.Add(1) == 1 {
					close(firstToken)
					select {
					case <-releaseFirst:
					case <-r.Context().Done():
					}
					w.WriteHeader(401)
					_, _ = io.WriteString(w, "CANARY-TOKEN-FAILURE")
					return
				}
				_, _ = io.WriteString(w, `{"access_token":"CANARY-BEARER","expires_in":3600}`)
			}))
			defer token.Close()
			resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				close(resourceEntered)
				<-r.Context().Done()
				close(resourceCanceled)
			}))
			defer resource.Close()
			client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "CANARY-CLIENT", ClientSecret: "CANARY-SECRET", HTTPClient: token.Client()})
			if err != nil {
				t.Fatal(err)
			}
			client.Timeout = time.Second
			base := client.Transport
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if requests.Add(1) == 2 {
					close(secondRequest)
				}
				return base.RoundTrip(r)
			})
			var mu sync.Mutex
			var notes []PopulateFailure
			pop := NewNativePopulatorWithFailureObserver(client, resource.URL, func(n PopulateFailure) { mu.Lock(); defer mu.Unlock(); notes = append(notes, n) })
			run := func(done chan struct{}) {
				defer close(done)
				qr, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext)
				if err != errPopulateUpstream || qr != nil {
					t.Errorf("error=%v qr=%s", err, qr)
				}
			}
			firstDone, secondDone := make(chan struct{}), make(chan struct{})
			go run(firstDone)
			awaitPopulateSignal(t, firstToken)
			if concurrent {
				go run(secondDone)
				awaitPopulateSignal(t, secondRequest)
			}
			close(releaseFirst)
			awaitPopulateSignal(t, firstDone)
			if !concurrent {
				go run(secondDone)
			}
			awaitPopulateSignal(t, resourceEntered)
			awaitPopulateSignal(t, secondDone)
			awaitPopulateSignal(t, resourceCanceled)
			if len(notes) != 2 {
				t.Fatalf("notes=%#v", notes)
			}
			seen := map[PopulateFailure]int{}
			for _, n := range notes {
				seen[n]++
			}
			for _, want := range []PopulateFailure{{Stage: "token_acquisition", Reason: "other"}, {Stage: "transport", Reason: "deadline"}} {
				if seen[want] != 1 {
					t.Fatalf("notes=%#v want=%#v", notes, want)
				}
			}
			if tokenCalls.Load() != 2 || calls.Load() != 1 || requests.Load() != 2 {
				t.Fatalf("token=%d population=%d requests=%d", tokenCalls.Load(), calls.Load(), requests.Load())
			}
			wire, err := json.Marshal(notes)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "CANARY") {
				t.Fatalf("sensitive notes=%s", wire)
			}
		})
	}
}

func TestNativePopulateFailureShadowsCallerObservation(t *testing.T) {
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "CANARY-TOKEN-FAILURE")
	}))
	defer token.Close()
	authenticated, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "CANARY-CLIENT", ClientSecret: "CANARY-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, inherited := smartauth.WithTokenAcquisitionObservation(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authenticated.Do(req); err == nil || !inherited.Failed() {
		t.Fatalf("initial observation err=%v failed=%v", err, inherited.Failed())
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("CANARY-RESOURCE: %w", context.DeadlineExceeded)
	})}
	var notes []PopulateFailure
	pop := NewNativePopulatorWithFailureObserver(client, "http://example.test", func(n PopulateFailure) { notes = append(notes, n) })
	if _, _, err := pop.Populate(ctx, []byte(failurePackage), failureContext); err != errPopulateUpstream {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "transport", Reason: "deadline"}) {
		t.Fatalf("notes=%#v", notes)
	}
	// Even with no callback, a population call must not mark the caller's handle.
	for _, withCallback := range []bool{false, true} {
		ctx, parent := smartauth.WithTokenAcquisitionObservation(context.Background())
		pop := NewNativePopulator(authenticated, "http://example.test")
		if withCallback {
			pop = NewNativePopulatorWithFailureObserver(authenticated, "http://example.test", func(PopulateFailure) {})
		}
		if _, _, err := pop.Populate(ctx, []byte(failurePackage), failureContext); err != errPopulateUpstream {
			t.Fatal(err)
		}
		if parent.Failed() {
			t.Fatal("population call marked caller observation")
		}
	}
	// Preserve the existing request-construction failure for a nil context.
	notes = nil
	if _, _, err := pop.Populate(nil, []byte(failurePackage), failureContext); err != errPopulateUpstream {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "request_build", Reason: "other"}) {
		t.Fatalf("notes=%#v", notes)
	}
}
