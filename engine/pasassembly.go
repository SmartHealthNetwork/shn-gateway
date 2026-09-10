package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
)

const pasGraphMaxBytes = 8 << 20
const pasGraphMaxResources = 256
const pasGraphMaxReferences = 4096
const pasGraphMaxDepth = 64

func pasAssemblyError() error { return errors.New("engine: invalid or incomplete PAS response graph") }

// assembleTerminalPASBundle preserves the payer graph and entry
// identity while replacing only its ClaimResponse. Certification and authority
// remain the caller's responsibility; this helper performs no external reads.
func assembleTerminalPASBundle(original, terminal []byte, assembledAt time.Time) ([]byte, error) {
	g, err := readPASGraph(original)
	if err != nil {
		return nil, err
	}
	if err = g.validate(); err != nil {
		return nil, err
	}
	// The original Bundle signature cannot certify changed content. Preserve
	// signed direct relays, but never carry that signature into an assembly.
	if _, signed := g.bundle["signature"]; signed {
		return nil, pasAssemblyError()
	}
	if len(terminal) > pasGraphMaxBytes {
		return nil, pasAssemblyError()
	}
	var replacement map[string]any
	if decodePASObject(terminal, &replacement) != nil || replacement["resourceType"] != "ClaimResponse" {
		return nil, pasAssemblyError()
	}
	old := g.response.resource
	if replacement["id"] != old["id"] {
		return nil, pasAssemblyError()
	}
	for _, field := range []string{"patient", "request"} {
		before, ok := old[field].(map[string]any)
		if !ok {
			return nil, pasAssemblyError()
		}
		after, ok := replacement[field].(map[string]any)
		if !ok {
			return nil, pasAssemblyError()
		}
		a, aok := before["reference"].(string)
		b, bok := after["reference"].(string)
		if !aok || !bok || a == "" || a != b {
			return nil, pasAssemblyError()
		}
	}
	g.entries[g.response.index].(map[string]any)["resource"] = replacement
	g.bundle["timestamp"] = assembledAt.UTC().Format(time.RFC3339Nano)
	result, err := json.Marshal(g.bundle)
	if err != nil {
		return nil, pasAssemblyError()
	}
	if err = validatePASBundleGraph(result); err != nil {
		return nil, err
	}
	return result, nil
}

func validatePASBundleGraph(raw []byte) error {
	g, err := readPASGraph(raw)
	if err != nil {
		return err
	}
	return g.validate()
}

type pasGraphEntry struct {
	fullURL  string
	resource map[string]any
	index    int
}
type pasGraph struct {
	bundle          map[string]any
	entries         []any
	byURL           map[string]*pasGraphEntry
	response        *pasGraphEntry
	resources, refs int
}

func readPASGraph(raw []byte) (*pasGraph, error) {
	if len(raw) > pasGraphMaxBytes {
		return nil, pasAssemblyError()
	}
	g := &pasGraph{byURL: make(map[string]*pasGraphEntry)}
	if decodePASObject(raw, &g.bundle) != nil || g.bundle["resourceType"] != "Bundle" || g.bundle["type"] != "collection" {
		return nil, pasAssemblyError()
	}
	var ok bool
	g.entries, ok = g.bundle["entry"].([]any)
	if !ok || len(g.entries) == 0 || len(g.entries) > pasGraphMaxResources {
		return nil, pasAssemblyError()
	}
	for i, v := range g.entries {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, pasAssemblyError()
		}
		full, ok := e["fullUrl"].(string)
		if !ok || full == "" {
			return nil, pasAssemblyError()
		}
		u, err := url.Parse(full)
		if err != nil || !u.IsAbs() || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(full, "/_history/") {
			return nil, pasAssemblyError()
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return nil, pasAssemblyError()
		}
		typ, tok := r["resourceType"].(string)
		id, iok := r["id"].(string)
		if !tok || !iok || typ == "" || !pasSafeResourceID(id) {
			return nil, pasAssemblyError()
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			if u.Host == "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) {
				return nil, pasAssemblyError()
			}
		}
		if _, exists := g.byURL[full]; exists {
			return nil, pasAssemblyError()
		}
		entry := &pasGraphEntry{full, r, i}
		g.byURL[full] = entry
		if typ == "ClaimResponse" {
			if g.response != nil {
				return nil, pasAssemblyError()
			}
			g.response = entry
		}
	}
	if g.response == nil {
		return nil, pasAssemblyError()
	}
	return g, nil
}

func (g *pasGraph) validate() error {
	// Bundle and entry metadata have no RESTful containing resource fullUrl.
	// Their references must therefore be explicit absolute identities.
	for key, value := range g.bundle {
		if key != "entry" && key != "resourceType" {
			if err := g.walk(value, nil, nil, 0, false); err != nil {
				return err
			}
		}
	}
	for _, entry := range g.entries {
		for key, value := range entry.(map[string]any) {
			if key != "resource" {
				if err := g.walk(value, nil, nil, 0, false); err != nil {
					return err
				}
			}
		}
	}
	// Each identity is traversed once, including disconnected retained siblings.
	// Reference cycles therefore do not recurse through the referenced resource.
	for _, e := range g.byURL {
		contained := make(map[string]map[string]any)
		if list, exists := e.resource["contained"]; exists {
			arr, ok := list.([]any)
			if !ok {
				return pasAssemblyError()
			}
			for _, v := range arr {
				r, ok := v.(map[string]any)
				if !ok {
					return pasAssemblyError()
				}
				typ, tok := r["resourceType"].(string)
				id, ok := r["id"].(string)
				if !tok || typ == "" || !ok || !pasSafeResourceID(id) || contained[id] != nil {
					return pasAssemblyError()
				}
				contained[id] = r
			}
		}
		if err := g.walk(e.resource, e, contained, 0, false); err != nil {
			return err
		}
	}
	return nil
}

func (g *pasGraph) walk(v any, owner *pasGraphEntry, contained map[string]map[string]any, depth int, inContained bool) error {
	if depth > pasGraphMaxDepth {
		return pasAssemblyError()
	}
	switch x := v.(type) {
	case map[string]any:
		if typ, ok := x["resourceType"].(string); ok {
			g.resources++
			if g.resources > pasGraphMaxResources || typ == "Bundle" || typ == "Parameters" {
				return pasAssemblyError()
			}
			if depth > 0 {
				inContained = true
				if _, exists := x["contained"]; exists {
					return pasAssemblyError()
				}
			}
		}
		if ref, exists := x["reference"]; exists {
			s, ok := ref.(string)
			g.refs++
			if !ok || s == "" || g.refs > pasGraphMaxReferences || !g.resolve(s, owner, contained, inContained) {
				return pasAssemblyError()
			}
		}
		for _, child := range x {
			if err := g.walk(child, owner, contained, depth+1, inContained); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := g.walk(child, owner, contained, depth+1, inContained); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *pasGraph) resolve(ref string, owner *pasGraphEntry, contained map[string]map[string]any, inContained bool) bool {
	if strings.HasPrefix(ref, "#") {
		if owner == nil {
			return false
		}
		return (ref == "#" && inContained) || contained[strings.TrimPrefix(ref, "#")] != nil
	}
	u, err := url.Parse(ref)
	if err != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
		return false
	}
	full := ref
	if !u.IsAbs() {
		if owner == nil || u.RawPath != "" {
			return false
		}
		// FHIR R4 defines relative resolution only for a RESTful owner fullUrl.
		base, err := url.Parse(owner.fullURL)
		if err != nil || base.Host == "" || base.RawPath != "" || (base.Scheme != "http" && base.Scheme != "https") || strings.HasPrefix(ref, "/") {
			return false
		}
		typ := owner.resource["resourceType"].(string)
		id := owner.resource["id"].(string)
		suffix := "/" + typ + "/" + id
		if !strings.HasSuffix(base.Path, suffix) {
			return false
		}
		base.Path = strings.TrimSuffix(base.Path, suffix) + "/" + u.Path
		full = base.String()
	}
	version := ""
	if pos := strings.Index(full, "/_history/"); pos >= 0 {
		version = full[pos+10:]
		full = full[:pos]
		if version == "" || strings.Contains(version, "/") {
			return false
		}
	}
	target := g.byURL[full]
	if target == nil {
		return false
	}
	if version != "" {
		meta, ok := target.resource["meta"].(map[string]any)
		return ok && meta["versionId"] == version
	}
	return true
}

// Decode without float conversion or last-key-wins ambiguity. The same bounded
// parser protects both the retained response and the polled replacement.
func decodePASObject(raw []byte, dst *map[string]any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodePASValue(d, 0)
	if err != nil {
		return pasAssemblyError()
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return pasAssemblyError()
	}
	if _, err = d.Token(); err != io.EOF {
		return pasAssemblyError()
	}
	*dst = obj
	return nil
}
func decodePASValue(d *json.Decoder, depth int) (any, error) {
	if depth > pasGraphMaxDepth {
		return nil, pasAssemblyError()
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		obj := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			s, ok := key.(string)
			if !ok {
				return nil, pasAssemblyError()
			}
			if _, exists := obj[s]; exists {
				return nil, pasAssemblyError()
			}
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, pasAssemblyError()
		}
		return obj, nil
	case json.Delim('['):
		arr := []any{}
		for d.More() {
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, child)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, pasAssemblyError()
		}
		return arr, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, pasAssemblyError()
		}
		return t, nil
	}
}

func pasSafeResourceID(id string) bool {
	if len(id) == 0 || len(id) > 64 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
