package smartauth

import (
	"github.com/SmartHealthNetwork/shn-gateway/diagnostics"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTransportObservationSeesInjectedBearer(t *testing.T) {
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"synthetic-observed-bearer","expires_in":300}`)
	}))
	defer token.Close()
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "answer") }))
	defer resource.Close()
	q := diagnostics.NewQueue(diagnostics.Limits{})
	hc, err := NewHTTPClient(Config{TokenURL: token.URL, ClientID: "test", ClientSecret: "synthetic-secret", HTTPClient: token.Client(), Transport: diagnostics.ObserveTransport(resource.Client().Transport, q.TryEmit, nil, 1024, nil)})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", resource.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	e, err := q.Next(req.Context())
	if err != nil {
		t.Fatal(err)
	}
	if e.Headers.Get("Authorization") != "Bearer synthetic-observed-bearer" {
		t.Fatalf("missing actual bearer: %+v", e)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("caller headers mutated")
	}
}
