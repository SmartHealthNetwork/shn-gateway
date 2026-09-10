package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
)

// completePASRequest retains only clinical evidence selected by the request. Reads
// use the configured holder-local SoR authority, never a reference URL transport.
// The complete graph then follows the existing transform and validation path.
func (g *Gateway) completePASRequest(ctx context.Context, body []byte) ([]byte, error) {
	if g.cfg.OriginationProfile != "provider-data" {
		return body, nil
	}
	if g.cfg.SoR == nil {
		return nil, errors.New("PAS evidence unavailable")
	}
	return retainPASRequestEvidence(ctx, body, ReadSystemOfRecord(g.cfg.SoR).ResolveByReferenceContext)
}

func retainPASRequestEvidence(ctx context.Context, body []byte, read func(context.Context, string) ([]byte, bool, error)) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, errors.New("invalid or incomplete PAS request evidence") }
	if len(body) > pasGraphMaxBytes {
		return fail()
	}
	var bundle map[string]any
	if decodePASObject(body, &bundle) != nil || bundle["resourceType"] != "Bundle" || bundle["type"] != "collection" {
		return fail()
	}
	entries, ok := bundle["entry"].([]any)
	if !ok || len(entries) == 0 || len(entries) > pasGraphMaxResources {
		return fail()
	}
	graph := &pasGraph{bundle: bundle, entries: entries, byURL: map[string]*pasGraphEntry{}}
	patient := ""
	add := func(v any) bool {
		e, ok := v.(map[string]any)
		if !ok {
			return false
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return false
		}
		full, ok := e["fullUrl"].(string)
		if !ok {
			return false
		}
		typ, tok := r["resourceType"].(string)
		id, iok := r["id"].(string)
		u, err := url.Parse(full)
		if err != nil || !tok || !iok || !pasSafeResourceID(id) || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) || graph.byURL[full] != nil {
			return false
		}
		graph.byURL[full] = &pasGraphEntry{fullURL: full, resource: r}
		if typ == "Patient" {
			if patient != "" && patient != full {
				return false
			}
			patient = full
		}
		return true
	}
	for _, e := range entries {
		if !add(e) {
			return fail()
		}
	}
	if patient == "" {
		return fail()
	}
	refs := 0
	total := len(body)
	for i := 0; i < len(entries); i++ {
		entry := entries[i].(map[string]any)
		owner := graph.byURL[entry["fullUrl"].(string)]
		var selected []string
		var walk func(any, int) bool
		walk = func(v any, depth int) bool {
			if depth > pasGraphMaxDepth {
				return false
			}
			switch x := v.(type) {
			case map[string]any:
				if ref, exists := x["reference"]; exists {
					s, ok := ref.(string)
					refs++
					if !ok || refs > pasGraphMaxReferences {
						return false
					}
					selected = append(selected, s)
				}
				keys := make([]string, 0, len(x))
				for k := range x {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if !walk(x[k], depth+1) {
						return false
					}
				}
			case []any:
				for _, c := range x {
					if !walk(c, depth+1) {
						return false
					}
				}
			}
			return true
		}
		if !walk(owner.resource, 0) {
			return fail()
		}
		for _, ref := range selected {
			if strings.HasPrefix(ref, "#") {
				continue
			}
			if graph.resolve(ref, owner, nil, false) {
				continue
			}
			// Only unversioned relative clinical identities are eligible for local lookup.
			// Absolute references stay exact and are never reduced to a different authority.
			parts := strings.Split(ref, "/")
			if len(parts) != 2 || !pasSafeResourceID(parts[1]) {
				return fail()
			}
			switch parts[0] {
			case "Condition", "ClinicalImpression", "Goal", "Observation", "DiagnosticReport":
			default:
				return fail()
			}
			base := strings.TrimSuffix(owner.fullURL, "/"+owner.resource["resourceType"].(string)+"/"+owner.resource["id"].(string))
			full := base + "/" + ref
			if len(entries) >= pasGraphMaxResources {
				return fail()
			}
			if err := ctx.Err(); err != nil {
				return nil, safeSoRError(err)
			}
			raw, found, err := read(ctx, ref)
			if err != nil {
				return nil, safeSoRError(err)
			}
			total += len(raw)
			if !found || total > pasGraphMaxBytes {
				return fail()
			}
			var r map[string]any
			if decodePASObject(raw, &r) != nil || r["resourceType"] != parts[0] || r["id"] != parts[1] {
				return fail()
			}
			subject, ok := r["subject"].(map[string]any)
			if !ok {
				return fail()
			}
			s, ok := subject["reference"].(string)
			if !ok {
				return fail()
			}
			if s != patient && base+"/"+s != patient {
				return fail()
			}
			e := map[string]any{"fullUrl": full, "resource": r}
			if !add(e) {
				return fail()
			}
			entries = append(entries, e)
		}
	}
	graph.entries = entries
	bundle["entry"] = entries
	if graph.validate() != nil || !consistentPASGraphSubjects(graph, patient) {
		return fail()
	}
	result, err := json.Marshal(bundle)
	if err != nil || len(result) > pasGraphMaxBytes {
		return fail()
	}
	return result, nil
}
