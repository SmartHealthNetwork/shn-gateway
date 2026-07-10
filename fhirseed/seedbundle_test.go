package fhirseed

import (
	"encoding/json"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// helper: parse a transaction Bundle's entries into (method,url) keys.
func bundleKeys(t *testing.T, b []byte) []struct{ Method, URL string } {
	t.Helper()
	var parsed struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			Request struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"request"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	if parsed.ResourceType != "Bundle" || parsed.Type != "transaction" {
		t.Fatalf("not a transaction Bundle: %s/%s", parsed.ResourceType, parsed.Type)
	}
	keys := make([]struct{ Method, URL string }, 0, len(parsed.Entry))
	for _, e := range parsed.Entry {
		keys = append(keys, struct{ Method, URL string }{e.Request.Method, e.Request.URL})
	}
	return keys
}

func TestMergeProviderTransactions_CollapsesIdenticalDuplicate(t *testing.T) {
	// Same (PUT, Organization/org) — one inline, one multi-line — identical resource.
	inline := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[` +
		`{"fullUrl":"Organization/org","resource":{"resourceType":"Organization","id":"org","name":"X"},"request":{"method":"PUT","url":"Organization/org"}}]}`)
	multiline := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[
		{"fullUrl":"Organization/org","resource":{
			"resourceType":"Organization",
			"id":"org",
			"name":"X"
		},"request":{"method":"PUT","url":"Organization/org"}}]}`)
	out, err := mergeProviderTransactions([][]byte{inline, multiline})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if keys := bundleKeys(t, out); len(keys) != 1 {
		t.Fatalf("want 1 entry after collapse, got %d: %v", len(keys), keys)
	}
}

func TestMergeProviderTransactions_HardErrorsOnConflict(t *testing.T) {
	a := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[` +
		`{"resource":{"resourceType":"Organization","id":"org","name":"A"},"request":{"method":"PUT","url":"Organization/org"}}]}`)
	b := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[` +
		`{"resource":{"resourceType":"Organization","id":"org","name":"B"},"request":{"method":"PUT","url":"Organization/org"}}]}`)
	if _, err := mergeProviderTransactions([][]byte{a, b}); err == nil {
		t.Fatal("want hard error on same-url different-resource, got nil")
	}
}

// The merge emits entries through the seedEntry struct, so any entry/request
// field beyond fullUrl/resource/request{method,url} would be silently dropped.
// Guard against that: an entry carrying an unmodeled field (here request.ifNoneExist)
// must hard-error, not silently strip it, so a future fixture can't lose a
// conditional-create guard in the merged download.
func TestMergeProviderTransactions_RejectsUnmodeledEntryField(t *testing.T) {
	b := []byte(`{"resourceType":"Bundle","type":"transaction","entry":[` +
		`{"resource":{"resourceType":"Organization","id":"org"},"request":{"method":"PUT","url":"Organization/org","ifNoneExist":"identifier=x"}}]}`)
	if _, err := mergeProviderTransactions([][]byte{b}); err == nil {
		t.Fatal("want error on entry with an unmodeled field (request.ifNoneExist), got nil")
	}
}

func TestProviderDataSeedBundle_AllPersonasNoDupURL(t *testing.T) {
	out, err := ProviderDataSeedBundle()
	if err != nil {
		t.Fatalf("ProviderDataSeedBundle: %v", err)
	}
	keys := bundleKeys(t, out)
	seen := map[string]bool{}
	for _, k := range keys {
		id := k.Method + " " + k.URL
		if seen[id] {
			t.Fatalf("duplicate request.url in merged bundle: %s", id)
		}
		seen[id] = true
	}
	// Exactly one org-cms-payer PUT survives the 11-way collapse.
	if !seen["PUT Organization/org-cms-payer"] {
		t.Fatal("merged bundle missing PUT Organization/org-cms-payer")
	}
	// Every persona's Patient PUT is present (12 personas → 12 distinct member Patients).
	for _, p := range shnsdk.ProviderDataPersonas() {
		raw, err := shnsdk.ProviderDataBundle(p)
		if err != nil {
			t.Fatalf("ProviderDataBundle(%q): %v", p, err)
		}
		var b struct {
			Entry []struct {
				Request  struct{ URL string } `json:"request"`
				Resource struct {
					ResourceType string `json:"resourceType"`
				} `json:"resource"`
			} `json:"entry"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("unmarshal persona %q: %v", p, err)
		}
		for _, e := range b.Entry {
			if e.Resource.ResourceType == "Patient" && !seen["PUT "+e.Request.URL] {
				t.Fatalf("persona %q Patient %q missing from merged bundle", p, e.Request.URL)
			}
		}
	}
}

func TestProviderDataSeedBundle_Deterministic(t *testing.T) {
	a, err := ProviderDataSeedBundle()
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProviderDataSeedBundle()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("ProviderDataSeedBundle is nondeterministic")
	}
}

func TestConformantSeedBundle_Members(t *testing.T) {
	out := ConformantSeedBundle()
	var parsed struct {
		Type  string `json:"type"`
		Entry []struct {
			Request struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"request"`
			Resource struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "transaction" {
		t.Fatalf("type = %q, want transaction", parsed.Type)
	}
	want := map[string]bool{
		"MBR-COVERED": true, "MBR-NOTCOVERED": true, "MBR-UC06": true,
		"MBR-UC07HCPCS": true, "MBR-UC08": true,
	}
	got := map[string]bool{}
	for _, e := range parsed.Entry {
		if e.Resource.ResourceType != "Patient" {
			t.Fatalf("entry %q resourceType = %q, want Patient", e.Resource.ID, e.Resource.ResourceType)
		}
		if e.Request.Method != "PUT" {
			t.Fatalf("entry %q method = %q, want PUT", e.Resource.ID, e.Request.Method)
		}
		got[e.Resource.ID] = true
	}
	for m := range want {
		if !got[m] {
			t.Fatalf("conformant bundle missing member %s", m)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("member count = %d, want %d (%v)", len(got), len(want), got)
	}
}

func TestSeedBundles_Disjoint(t *testing.T) {
	prov, err := ProviderDataSeedBundle()
	if err != nil {
		t.Fatal(err)
	}
	conf := ConformantSeedBundle()
	// No shared (method,url).
	pk := map[string]bool{}
	for _, k := range bundleKeys(t, prov) {
		pk[k.Method+" "+k.URL] = true
	}
	for _, k := range bundleKeys(t, conf) {
		if pk[k.Method+" "+k.URL] {
			t.Fatalf("provider and conformant share request %s %s", k.Method, k.URL)
		}
	}
	// No shared urn:shn:member id.
	members := func(b []byte) map[string]bool {
		var parsed struct {
			Entry []struct {
				Resource struct {
					Identifier []struct {
						System string `json:"system"`
						Value  string `json:"value"`
					} `json:"identifier"`
				} `json:"resource"`
			} `json:"entry"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatalf("members: unmarshal: %v", err)
		}
		m := map[string]bool{}
		for _, e := range parsed.Entry {
			for _, id := range e.Resource.Identifier {
				if id.System == shnsdk.MemberSystem {
					m[id.Value] = true
				}
			}
		}
		return m
	}
	pm := members(prov)
	cm := members(conf)
	// Guard against a false pass on empty maps if either shape ever drifts.
	if len(pm) < 12 {
		t.Fatalf("provider member set = %d, want >= 12", len(pm))
	}
	if len(cm) != 5 {
		t.Fatalf("conformant member set = %d, want 5", len(cm))
	}
	for id := range cm {
		if pm[id] {
			t.Fatalf("provider and conformant share member id %s", id)
		}
	}
}
