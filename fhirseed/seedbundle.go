package fhirseed

import (
	"bytes"
	"encoding/json"
	"fmt"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// seedEntry is one transaction Bundle entry. Resource stays raw so dedupe can
// compare canonicalized resources without a full FHIR model.
type seedEntry struct {
	FullURL  string          `json:"fullUrl,omitempty"`
	Resource json.RawMessage `json:"resource"`
	Request  struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	} `json:"request"`
}

// ProviderDataSeedBundle concatenates every shnsdk.ProviderDataPersonas()
// transaction Bundle into ONE transaction Bundle a partner can POST to their
// own FHIR server in a single request — the same synthetic personas SHN's own
// provider SoR is seeded from (SHN's loader freshens Observation dates and POSTs
// each persona separately, so these bytes are not byte-identical to what it
// PUTs). Each source persona is already a valid standalone transaction with
// idempotent PUTs — the single-file merge is a download-UX convenience.
//
// SDK-PIN LOCKSTEP: the output is assembled from whatever shn-sdk this module is
// built against. In the monorepo that is ./sdk (via go.work); in the published
// module it is the go.mod pin. The committed gateway/seed/provider-personas.json
// is generated from ./sdk, so the published pin MUST carry the same provider
// fixtures or the download file and this function disagree. A hermetic test
// cannot see the pinned (proxy) SDK, so this is enforced at publish (bump the
// pin in lockstep with any ./sdk provider-data change) — see the gateway
// publish runbook.
//
// Because HAPI rejects a transaction with two entries on the same request.url,
// the merge dedupes: 11 of the 12 personas repeat PUT Organization/org-cms-payer
// (the 12th, uc02-payerb, carries org-payerb). Identity is judged on the
// DECODED resource, not raw bytes — uc08 formats its org-cms-payer stanza inline
// while the others span multiple lines. Identical duplicates collapse (first
// occurrence wins its position); a genuine conflict (same url, different decoded
// resource) is a hard error. Deterministic (no time.Now): freshening is a
// serve/recipe concern, not this assembler's.
func ProviderDataSeedBundle() ([]byte, error) {
	personas := shnsdk.ProviderDataPersonas()
	bundles := make([][]byte, 0, len(personas))
	for _, p := range personas {
		raw, err := shnsdk.ProviderDataBundle(p)
		if err != nil {
			return nil, fmt.Errorf("ProviderDataSeedBundle: %w", err)
		}
		bundles = append(bundles, raw)
	}
	return mergeProviderTransactions(bundles)
}

// mergeProviderTransactions merges transaction Bundles into one, deduping
// entries by (request.method, request.url) on decoded-resource equality.
func mergeProviderTransactions(bundles [][]byte) ([]byte, error) {
	type key struct{ method, url string }
	canonByKey := map[key][]byte{}
	var out []seedEntry
	for i, raw := range bundles {
		var b struct {
			Entry []json.RawMessage `json:"entry"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("merge: unmarshal bundle %d: %w", i, err)
		}
		for _, rawEntry := range b.Entry {
			// Strict-decode each entry: seedEntry re-marshals the output, so any
			// entry/request field it doesn't model would be silently dropped.
			// DisallowUnknownFields turns that into a loud error (resource stays a
			// verbatim RawMessage, so this only fences entry/request-level keys).
			var e seedEntry
			dec := json.NewDecoder(bytes.NewReader(rawEntry))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&e); err != nil {
				return nil, fmt.Errorf("merge: cannot strict-decode entry (an unmodeled field beyond fullUrl/resource/request{method,url} — extend seedEntry to preserve it — or malformed JSON): %w", err)
			}
			k := key{e.Request.Method, e.Request.URL}
			canon, err := canonicalJSON(e.Resource)
			if err != nil {
				return nil, fmt.Errorf("merge: canonicalize %s %s: %w", k.method, k.url, err)
			}
			if prev, ok := canonByKey[k]; ok {
				if !bytes.Equal(prev, canon) {
					return nil, fmt.Errorf("merge: conflicting resources for %s %s across personas", k.method, k.url)
				}
				continue // identical duplicate — collapse
			}
			canonByKey[k] = canon
			out = append(out, e)
		}
	}
	res, err := json.MarshalIndent(map[string]any{
		"resourceType": "Bundle",
		"type":         "transaction",
		"entry":        out,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(res, '\n'), nil
}

// canonicalJSON returns a stable byte form of a JSON value (unmarshal → marshal;
// Go sorts object keys), so formatting/key-order differences don't read as
// content differences during dedupe.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
