package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/internal/testrecord"
	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// recordCertificationClients replaces certificationClient for one test with a
// constructor whose clients answer in memory, and returns the endpoints it
// built clients for and the URLs they were asked. The answer is a real 2.1
// lane's, replayed strictly (testdata/recordings/lane-2.1-certify-warm.json):
// the one request these rows send is the 2.1 warm-up, and any other request
// fails the test.
func recordCertificationClients(t *testing.T) (built func() []string, asked func() []string) {
	t.Helper()
	var mu sync.Mutex
	var endpoints, urls []string
	rec := testrecord.Load(t, laneRecording(t, "lane-2.1-certify-warm"))
	rec.Subset() // a row that builds clients but sends nothing leaves it unused; the warm-up row counts its request
	replay := rec.Server()
	old := certificationClient
	certificationClient = func(endpoint string) *shnsdk.OperationValidator {
		mu.Lock()
		endpoints = append(endpoints, endpoint)
		mu.Unlock()
		return &shnsdk.OperationValidator{BaseURL: endpoint, Client: &http.Client{Transport: lifecycleTransport(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			urls = append(urls, r.URL.String())
			mu.Unlock()
			answer := httptest.NewRecorder()
			replay.Config.Handler.ServeHTTP(answer, r)
			res := answer.Result()
			res.Request = r
			return res, nil
		})}}
	}
	t.Cleanup(func() { certificationClient = old })
	snapshot := func(s *[]string) func() []string {
		return func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := slices.Clone(*s)
			slices.Sort(out)
			return out
		}
	}
	return snapshot(&endpoints), snapshot(&urls)
}

// Every certification client configured by address comes from
// certificationClient, so a test that fixtures the network can reach it.
func TestAddressConfiguredCertificationClientsUseTheConstructor(t *testing.T) {
	built, _ := recordCertificationClients(t)
	cfg := config{FHIRCertifyURL21: "http://certify21.test/fhir", FHIRValidateURL22: "http://validate22.test/fhir"}
	got := certificationValidators(func(string) string { return "" }, cfg, "http://canonical.test/fhir", nil, nil)
	want := []string{"http://canonical.test/fhir", "http://certify21.test/fhir", "http://validate22.test/fhir"}
	if b := built(); !slices.Equal(b, want) {
		t.Fatalf("certificationClient built %v, want %v", b, want)
	}
	if len(got) != 3 {
		t.Fatalf("certificationValidators = %v, want a client per line", got)
	}
}

// The boot warm-up builds its own client per endpoint from certificationClient,
// and sends its request through it.
func TestCertificationWarmUpUsesTheConstructor(t *testing.T) {
	built, asked := recordCertificationClients(t)
	warmCertification(context.Background(), map[string]shnsdk.Validator{"2.1": &shnsdk.OperationValidator{BaseURL: "http://warm21.test/fhir"}})
	if b := built(); !slices.Equal(b, []string{"http://warm21.test/fhir"}) {
		t.Fatalf("certificationClient built %v, want the warmed endpoint", b)
	}
	if a := asked(); len(a) != 1 || !strings.HasPrefix(a[0], "http://warm21.test/fhir/") {
		t.Fatalf("the warm-up's requests = %v, want one through the constructed client", a)
	}
}
