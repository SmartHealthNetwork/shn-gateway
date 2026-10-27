package checks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type refuseTransport struct{}

func (refuseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the runner's client was used")
}

// MON-R25: a target's own Client, when set, is what its plain probes read
// through (the gateway tags it with the target's endpoint kind); a target
// without one keeps the runner's.
func TestTargetClientIsThePlainProbesClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		fmt.Fprint(w, `{"resourceType":"CapabilityStatement","fhirVersion":"4.0.1"}`)
	}))
	defer srv.Close()
	runner := &http.Client{Transport: refuseTransport{}}
	for _, kind := range []Kind{KindReachable, KindFHIRMetadata, KindDavinciConfig} {
		own := NewRunner([]Target{{ID: "own", Kind: kind, URL: srv.URL, Client: srv.Client()}}, runner, time.Now)
		res, err := own.Run(context.Background())
		if err != nil || len(res) != 1 || res[0].Failure != nil && res[0].Failure.Code == FailUnreachable {
			t.Fatalf("%s with its own client: %+v, %v; want it to reach the server through Target.Client", kind, res, err)
		}
		shared := NewRunner([]Target{{ID: "shared", Kind: kind, URL: srv.URL}}, runner, time.Now)
		res, err = shared.Run(context.Background())
		if err != nil || len(res) != 1 || res[0].OK || res[0].Failure == nil || res[0].Failure.Code != FailUnreachable {
			t.Fatalf("%s with no client of its own: %+v, %v; want it unreachable through the runner's (refusing) client", kind, res, err)
		}
	}
}
