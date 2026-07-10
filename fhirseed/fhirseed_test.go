package fhirseed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hapiStub records requests and serves canned responses for the seed-loader flow.
type hapiStub struct {
	mu          sync.Mutex
	requests    []string // "METHOD path"
	partExists  bool     // second create-partition call answers "already defined"
	markerReady bool
}

func (h *hapiStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/DEFAULT/metadata"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "$partition-management-create-partition"):
			if h.partExists {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"issue":[{"diagnostics":"HAPI-1309: Partition name \"provider\" is already defined"}]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/Basic/seed-complete") && r.Method == http.MethodGet:
			if h.markerReady {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	})
}

func TestClient_SeedFlow(t *testing.T) {
	stub := &hapiStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	c := &Client{Base: srv.URL, Logf: t.Logf}
	ctx := context.Background()

	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.CreatePartitions(ctx, []string{"provider"}); err != nil {
		t.Fatal(err)
	}
	stub.partExists = true // idempotent re-run: "already defined" is success
	if err := c.CreatePartitions(ctx, []string{"provider"}); err != nil {
		t.Fatalf("already-defined partition must be idempotent success: %v", err)
	}
	if err := c.InstallCRLibraries(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.WarmUpPopulate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadProviderDataBundles(ctx, "provider"); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteSeedMarker(ctx, "provider"); err != nil {
		t.Fatal(err)
	}
	stub.markerReady = true
	if err := c.WaitForSeedMarker(ctx, "provider", 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// The 4 CR libraries were PUT into DEFAULT and warmed.
	var libPuts, evals, txns int
	stub.mu.Lock()
	for _, r := range stub.requests {
		if strings.Contains(r, "PUT ") && strings.Contains(r, "/DEFAULT/Library/") {
			libPuts++
		}
		if strings.Contains(r, "$evaluate") {
			evals++
		}
		if strings.HasPrefix(r, "POST ") && strings.HasSuffix(r, "/provider") {
			txns++
		}
	}
	stub.mu.Unlock()
	if libPuts != 4 || evals != 4 {
		t.Fatalf("library PUTs=%d $evaluate=%d, want 4/4", libPuts, evals)
	}
	if txns == 0 {
		t.Fatal("no provider-data transaction bundles POSTed")
	}
}

func TestFreshenObservations(t *testing.T) {
	in := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[
	  {"resource":{"resourceType":"Observation","effectiveDateTime":"2020-01-01T00:00:00Z"}},
	  {"resource":{"resourceType":"Patient","birthDate":"1958-07-14"}}]}`)
	out, err := FreshenObservations(in)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Entry []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	eff, _ := b.Entry[0].Resource["effectiveDateTime"].(string)
	ts, err := time.Parse(time.RFC3339, eff)
	if err != nil || time.Since(ts) > time.Minute {
		t.Fatalf("Observation not freshened to now: %q (%v)", eff, err)
	}
	if b.Entry[1].Resource["birthDate"] != "1958-07-14" {
		t.Fatal("non-Observation entries must be untouched")
	}
}

func TestSandboxProviderPersonasBundle(t *testing.T) {
	b := SandboxProviderPersonasBundle()
	var bundle struct {
		Type  string           `json:"type"`
		Entry []map[string]any `json:"entry"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Type != "transaction" || len(bundle.Entry) == 0 {
		t.Fatalf("embedded persona bundle: type=%q entries=%d", bundle.Type, len(bundle.Entry))
	}
	b[0] = 'X'
	if SandboxProviderPersonasBundle()[0] == 'X' {
		t.Fatal("accessor must return a copy")
	}
}

// TestFreshenObservations_ScopeMirrorsJQRecipe pins the exact scope the
// INTEGRATION.md "Keep the provider-data Observations recent" jq recipe mirrors:
// FreshenObservations stamps Observation.effectiveDateTime to now and touches
// nothing else — not non-Observation resources, not other Observation fields.
// The doc recipe is a prose reimplementation of this function; if this scope
// ever changes (e.g. effectivePeriod handling), that recipe half-freshens —
// update the recipe in the same change and re-point this test.
func TestFreshenObservations_ScopeMirrorsJQRecipe(t *testing.T) {
	in := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[
	  {"resource":{"resourceType":"Observation","effectiveDateTime":"2020-01-01T00:00:00Z","valueString":"keep-me"}},
	  {"resource":{"resourceType":"Condition","effectiveDateTime":"2020-01-01T00:00:00Z"}}
	]}`)
	out, err := FreshenObservations(in)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Entry []struct {
			Resource map[string]any `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	obs, cond := parsed.Entry[0].Resource, parsed.Entry[1].Resource
	if obs["effectiveDateTime"] == "2020-01-01T00:00:00Z" {
		t.Fatal("Observation.effectiveDateTime was not freshened")
	}
	if obs["valueString"] != "keep-me" {
		t.Fatalf("FreshenObservations touched a non-effectiveDateTime field: valueString=%v", obs["valueString"])
	}
	if cond["effectiveDateTime"] != "2020-01-01T00:00:00Z" {
		t.Fatalf("FreshenObservations changed a non-Observation resource: Condition.effectiveDateTime=%v", cond["effectiveDateTime"])
	}
}
