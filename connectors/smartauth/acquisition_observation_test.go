package smartauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTokenAcquisitionObservationFailureBoundary(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(401)
	}))
	defer token.Close()
	client, err := NewHTTPClient(Config{TokenURL: token.URL, ClientID: "client", ClientSecret: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	client.Timeout = time.Second
	ctx, observation := WithTokenAcquisitionObservation(context.Background())
	if observation.Failed() {
		t.Fatal("fresh observation marked")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, e := client.Do(req); e == nil {
			t.Error("expected token failure")
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("token endpoint not reached")
	}
	if observation.Failed() {
		t.Fatal("in-progress acquisition incorrectly marked failed")
	}
	close(release)
	// Readers may inspect the handle concurrently with acquisition completion.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 64 {
				_ = observation.Failed()
			}
		}()
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("token acquisition did not return")
	}
	wg.Wait()
	if !observation.Failed() {
		t.Fatal("completed token failure not marked")
	}
	child, fresh := WithTokenAcquisitionObservation(ctx)
	if child == ctx || fresh == observation || fresh.Failed() || !observation.Failed() {
		t.Fatal("derived context reused observation")
	}
}

func TestTokenAcquisitionObservationPreservesContext(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
	defer cancel()
	deadline := time.Now().Add(time.Hour)
	parent, deadlineCancel := context.WithDeadline(parent, deadline)
	defer deadlineCancel()
	derived, observation := WithTokenAcquisitionObservation(parent)
	if derived.Value(key{}) != "value" {
		t.Fatal("lost context value")
	}
	if got, ok := derived.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatal("changed deadline")
	}
	cancel()
	if !errors.Is(derived.Err(), context.Canceled) {
		t.Fatalf("context error=%v", derived.Err())
	}
	if observation.Failed() {
		t.Fatal("cancellation alone is not observed token failure")
	}
	var zero TokenAcquisitionObservation
	if zero.Failed() {
		t.Fatal("zero observation marked")
	}
}
