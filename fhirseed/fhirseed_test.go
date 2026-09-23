package fhirseed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/provider"):
			var request struct {
				Entry []json.RawMessage `json:"entry"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, "bad transaction", http.StatusBadRequest)
				return
			}
			entries := make([]map[string]any, len(request.Entry))
			for i := range entries {
				entries[i] = map[string]any{"response": map[string]any{"status": "200 OK"}}
			}
			_, _ = w.Write(mustJSON(map[string]any{"resourceType": "Bundle", "type": "transaction-response", "entry": entries}))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}
	})
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
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

// A transaction endpoint can answer HTTP 200 while recording a failed entry.
// The seeder must not count that as a complete synthetic source.
func TestPostTransaction_RefusesFailedEntryDespiteHTTP200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"422 Unprocessable Entity","outcome":{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"invalid"}]}}}]}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/fhir"}
	err := c.PostTransaction(context.Background(), "provider", []byte(`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"PUT","url":"Patient/p"}}]}`))
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("failed transaction entry accepted: %v", err)
	}
}

func TestPostTransaction_RequiresTerminalWriteAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name, method, outer, entry string
		wantError                  bool
	}{
		{"put-created", "PUT", "200", "201 Created", false},
		{"put-updated", "PUT", "200", "200 OK", false},
		{"pending-outer", "PUT", "202", "200 OK", true},
		{"pending-entry", "PUT", "200", "202 Accepted", true},
		{"put-no-content", "PUT", "200", "204 No Content", true},
		{"delete-complete", "DELETE", "200", "204 No Content", false},
		{"unknown-method", "BREW", "200", "200 OK", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				code, _ := strconv.Atoi(tc.outer)
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":%q}}]}`, tc.entry)
			}))
			defer srv.Close()
			c := &Client{Base: srv.URL + "/fhir"}
			bundle := []byte(fmt.Sprintf(`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":%q,"url":"Patient/p"}}]}`, tc.method))
			err := c.PostTransaction(context.Background(), "provider", bundle)
			if (err != nil) != tc.wantError {
				t.Fatalf("outer=%s method=%s entry=%s error=%v, wantError=%v", tc.outer, tc.method, tc.entry, err, tc.wantError)
			}
		})
	}
}

func TestPostTransaction_RefusesOversizedResponseEvenAfterValidPrefix(t *testing.T) {
	response := `{"resourceType":"Bundle","type":"transaction-response","entry":[{"response":{"status":"200 OK"}}]}` + strings.Repeat(" ", 4<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/fhir"}
	err := c.PostTransaction(context.Background(), "provider", []byte(`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"PUT","url":"Patient/p"}}]}`))
	if err == nil {
		t.Fatal("truncated but JSON-valid response prefix counted as a completed transaction")
	}
}

func TestSeedPrerequisitesRejectPendingAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"partition create", func(c *Client) error { return c.CreatePartitions(context.Background(), []string{"provider"}) }},
		{"CR Library PUT", func(c *Client) error { return c.InstallCRLibraries(context.Background()) }},
		{"ELM warm-up", func(c *Client) error { return c.WarmUpPopulate(context.Background()) }},
		{"marker DELETE", func(c *Client) error { return c.ClearSeedMarker(context.Background(), "provider") }},
		{"certified marker PUT", func(c *Client) error { return c.WriteSeedMarker(context.Background(), "provider") }},
		{"uncertified marker PUT", func(c *Client) error { return c.WriteSyntheticUncertifiedSeedMarker(context.Background(), "provider") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			if err := tc.call(&Client{Base: srv.URL + "/fhir"}); err == nil {
				t.Fatal("202 pending acknowledgement counted as completed source prerequisite")
			}
		})
	}
}

func TestSeedMarkerRejectsPartialOrBodylessPut(t *testing.T) {
	for _, row := range []struct {
		name, method string
		status       int
	}{
		{"partial marker clear", http.MethodDelete, http.StatusPartialContent},
		{"partial marker write", http.MethodPut, http.StatusPartialContent},
		{"bodyless marker write", http.MethodPut, http.StatusNoContent},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != row.method {
					t.Errorf("method %s, want %s", r.Method, row.method)
				}
				w.WriteHeader(row.status)
			}))
			defer srv.Close()
			c := &Client{Base: srv.URL + "/fhir"}
			var err error
			if row.method == http.MethodDelete {
				err = c.ClearSeedMarker(context.Background(), "provider")
			} else {
				err = c.WriteSyntheticUncertifiedSeedMarker(context.Background(), "provider")
			}
			if err == nil {
				t.Fatalf("incomplete marker %s status %d accepted", row.method, row.status)
			}
		})
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

func TestDemoProviderPersonasBundle(t *testing.T) {
	b := DemoProviderPersonasBundle()
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
	if DemoProviderPersonasBundle()[0] == 'X' {
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
