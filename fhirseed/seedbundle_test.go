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

// The provider-data seed Bundle is an embedded, pre-baked artifact (assembled +
// drift-guarded in the monorepo). This validates the shipped embed getter itself,
// STRUCTURALLY and deliberately independent of the shn-sdk pin (per F1 the
// published module embeds a frozen artifact — persona-completeness vs the SDK is
// asserted in the monorepo's TestBakeProviderDataSeedBundle_AllPersonasNoDupURL,
// so the public clone's tests never depend on the exact pin): one valid
// transaction Bundle, no duplicate request.url, the single collapsed org-cms-payer
// PUT, and a Patient PUT per provider persona (12).
func TestProviderDataSeedBundle_EmbedValid(t *testing.T) {
	var parsed struct {
		ResourceType string `json:"resourceType"`
		Type         string `json:"type"`
		Entry        []struct {
			Request struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"request"`
			Resource struct {
				ResourceType string `json:"resourceType"`
			} `json:"resource"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(ProviderDataSeedBundle(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ResourceType != "Bundle" || parsed.Type != "transaction" {
		t.Fatalf("not a transaction Bundle: %s/%s", parsed.ResourceType, parsed.Type)
	}
	seen := map[string]bool{}
	patients := 0
	for _, e := range parsed.Entry {
		id := e.Request.Method + " " + e.Request.URL
		if seen[id] {
			t.Fatalf("duplicate request.url in embedded bundle: %s", id)
		}
		seen[id] = true
		if e.Resource.ResourceType == "Patient" {
			patients++
		}
	}
	if !seen["PUT Organization/org-cms-payer"] {
		t.Fatal("embedded provider bundle missing PUT Organization/org-cms-payer")
	}
	if patients < 12 {
		t.Fatalf("embedded provider bundle has %d Patient PUTs, want >= 12", patients)
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
	prov := ProviderDataSeedBundle()
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
