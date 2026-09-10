package smartauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenWaitingCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write([]byte(`{"access_token":"token","expires_in":3600}`))
	}))
	defer srv.Close()
	ts := &TokenSource{Config: Config{TokenURL: srv.URL, ClientID: "client", ClientSecret: "secret"}}
	done := make(chan error, 1)
	go func() { _, err := ts.Token(context.Background()); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiting := make(chan error, 1)
	go func() { _, err := ts.Token(ctx); waiting <- err }()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Error("canceled waiter blocked on token acquisition")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
