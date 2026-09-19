// pasgraph.go — the PAS response-graph REFERENCE-CLOSURE RULE (FR-G28).
//
// A payer's answer must CARRY EVERY RESOURCE IT NAMES. This reads a PAS response
// Bundle as a graph — one ClaimResponse, every entry identified by an absolute
// fullUrl, every Reference resolving to an entry, a contained resource of its own
// owner, or a version the graph actually holds — and refuses anything else. It
// changes not one byte: the rule decides whether a payer's bytes are relayed or
// refused, never what they say.
//
// It is a PRODUCT GUARD, not a helper of the terminal-response assembly that used
// to live beside it. That assembly is gone (a relayed decision is the payer's own
// message); the rule stayed, because a receiver that accepts an answer naming
// records it does not carry has accepted a message it cannot read. It is held to
// answers a REAL reference payer emitted, unpatched and corrected, by
// test/adversarial/payer_graph_capture_test.go — every raw capture refused, every
// corrected one accepted byte for byte.
//
// The bounded decoder below is part of the rule: a duplicate JSON member, a
// number reshaped by float conversion or a trailing document would each make
// "what this answer says" ambiguous before any reference was walked.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const pasGraphMaxBytes = 8 << 20
const pasGraphMaxResources = 256
const pasGraphMaxReferences = 4096
const pasGraphMaxDepth = 64

func pasGraphError() error { return errors.New("engine: invalid or incomplete PAS response graph") }

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
		return nil, pasGraphError()
	}
	g := &pasGraph{byURL: make(map[string]*pasGraphEntry)}
	if decodePASObject(raw, &g.bundle) != nil || g.bundle["resourceType"] != "Bundle" || g.bundle["type"] != "collection" {
		return nil, pasGraphError()
	}
	var ok bool
	g.entries, ok = g.bundle["entry"].([]any)
	if !ok || len(g.entries) == 0 || len(g.entries) > pasGraphMaxResources {
		return nil, pasGraphError()
	}
	for i, v := range g.entries {
		e, ok := v.(map[string]any)
		if !ok {
			return nil, pasGraphError()
		}
		full, ok := e["fullUrl"].(string)
		if !ok || full == "" {
			return nil, pasGraphError()
		}
		u, err := url.Parse(full)
		if err != nil || !u.IsAbs() || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(full, "/_history/") {
			return nil, pasGraphError()
		}
		r, ok := e["resource"].(map[string]any)
		if !ok {
			return nil, pasGraphError()
		}
		typ, tok := r["resourceType"].(string)
		id, iok := r["id"].(string)
		if !tok || !iok || typ == "" || !pasSafeResourceID(id) {
			return nil, pasGraphError()
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			if u.Host == "" || !strings.HasSuffix(u.Path, "/"+typ+"/"+id) {
				return nil, pasGraphError()
			}
		}
		if _, exists := g.byURL[full]; exists {
			return nil, pasGraphError()
		}
		entry := &pasGraphEntry{full, r, i}
		g.byURL[full] = entry
		if typ == "ClaimResponse" {
			if g.response != nil {
				return nil, pasGraphError()
			}
			g.response = entry
		}
	}
	if g.response == nil {
		return nil, pasGraphError()
	}
	return g, nil
}

func (g *pasGraph) validate() error { return g.validateWithReferencePolicy(nil) }

func (g *pasGraph) validateWithReferencePolicy(policy *authoredPASReferencePolicy) error {
	if !policy.matches(g) {
		return pasGraphError()
	}
	// Bundle and entry metadata have no RESTful containing resource fullUrl.
	// Their references must therefore be explicit absolute identities.
	for key, value := range g.bundle {
		if key != "entry" && key != "resourceType" {
			if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, "", policy); err != nil {
				return err
			}
		}
	}
	for _, entry := range g.entries {
		for key, value := range entry.(map[string]any) {
			if key != "resource" {
				if err := g.walkWithReferencePolicy(value, nil, nil, 0, false, "", policy); err != nil {
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
				return pasGraphError()
			}
			for _, v := range arr {
				r, ok := v.(map[string]any)
				if !ok {
					return pasGraphError()
				}
				typ, tok := r["resourceType"].(string)
				id, ok := r["id"].(string)
				if !tok || typ == "" || !ok || !pasSafeResourceID(id) || contained[id] != nil {
					return pasGraphError()
				}
				contained[id] = r
			}
		}
		if err := g.walkWithReferencePolicy(e.resource, e, contained, 0, false, "", policy); err != nil {
			return err
		}
	}
	return nil
}

func (g *pasGraph) walk(v any, owner *pasGraphEntry, contained map[string]map[string]any, depth int, inContained bool) error {
	return g.walkWithReferencePolicy(v, owner, contained, depth, inContained, "", nil)
}

func (g *pasGraph) walkWithReferencePolicy(v any, owner *pasGraphEntry, contained map[string]map[string]any, depth int, inContained bool, path string, policy *authoredPASReferencePolicy) error {
	if depth > pasGraphMaxDepth {
		return pasGraphError()
	}
	switch x := v.(type) {
	case map[string]any:
		if typ, ok := x["resourceType"].(string); ok {
			g.resources++
			if g.resources > pasGraphMaxResources || typ == "Bundle" || typ == "Parameters" {
				return pasGraphError()
			}
			if depth > 0 {
				inContained = true
				if _, exists := x["contained"]; exists {
					return pasGraphError()
				}
			}
		}
		if ref, exists := x["reference"]; exists {
			s, ok := ref.(string)
			g.refs++
			if !ok || s == "" || g.refs > pasGraphMaxReferences || (!g.resolve(s, owner, contained, inContained) && !policy.allows(owner, path, x)) {
				return pasGraphError()
			}
		}
		for key, child := range x {
			if err := g.walkWithReferencePolicy(child, owner, contained, depth+1, inContained, pasReferencePath(path, key), policy); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range x {
			if err := g.walkWithReferencePolicy(child, owner, contained, depth+1, inContained, pasReferencePath(path, strconv.Itoa(index)), policy); err != nil {
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
		return pasGraphError()
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return pasGraphError()
	}
	if _, err = d.Token(); err != io.EOF {
		return pasGraphError()
	}
	*dst = obj
	return nil
}
func decodePASValue(d *json.Decoder, depth int) (any, error) {
	if depth > pasGraphMaxDepth {
		return nil, pasGraphError()
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
				return nil, pasGraphError()
			}
			if _, exists := obj[s]; exists {
				return nil, pasGraphError()
			}
			child, err := decodePASValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[s] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, pasGraphError()
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
			return nil, pasGraphError()
		}
		return arr, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, pasGraphError()
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
