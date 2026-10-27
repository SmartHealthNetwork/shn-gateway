package otelsetup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// A configuration file's Prometheus pull reader (MON-R8). otelconf's stable
// package parses the rest of the file, but it decodes a pull reader's
// exporter to an empty PullMetricExporter: the prometheus/development type
// lives only in its experimental package, which would link resource
// detectors for five clouds the gateway never runs. So the gateway reads
// that one block itself, from the file's YAML node after the same ${VAR}
// substitution otelconf applies to the whole file, and only its scalar keys.
// Nothing else in the file is read here.
//
// TestOtelconfStillDropsThePullExporter fails once otelconf's stable package
// types the block: then read it from otelconf's own type and delete this
// decoder and its substitution. TestFileSubstitutionMatchesOtelconf holds the
// substitution to otelconf's on the pinned version.

// pullExporterKey is the one pull exporter the gateway builds.
const pullExporterKey = "prometheus/development"

// filePullExporter returns reader i's pull exporter node from the file, its
// ${VAR} references read through getenv.
func filePullExporter(file []byte, i int, getenv func(string) string) (*yaml.Node, error) {
	b, err := substituteEnv(file, getenv)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("not a YAML document")
	}
	readers := mapValue(mapValue(resolveAlias(doc.Content[0]), "meter_provider"), "readers")
	if readers == nil || readers.Kind != yaml.SequenceNode || i >= len(readers.Content) {
		return nil, errors.New("no such reader")
	}
	ex := mapValue(mapValue(resolveAlias(readers.Content[i]), "pull"), "exporter")
	if ex == nil || ex.Kind != yaml.MappingNode {
		return nil, errors.New("no pull exporter")
	}
	return ex, nil
}

// mapValue is key's value in mapping node m, nil when m is not a mapping or
// has no such key.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for _, kv := range mapPairs(m) {
		if kv[0].Value == key {
			return kv[1]
		}
	}
	return nil
}

// mapPairs are mapping node m's key/value pairs with merge keys (<<)
// expanded, as YAML, and so otelconf, reads them: a key the mapping writes
// wins over a merged one, and an earlier merged mapping over a later one.
func mapPairs(m *yaml.Node) [][2]*yaml.Node {
	return mergedPairs(m, map[*yaml.Node]bool{})
}

// mergedPairs is mapPairs, expanding each mapping once: a mapping that merges
// itself, which YAML allows (an anchor is set before its own children are
// read) and otelconf passes in a block it does not decode, adds nothing more,
// rather than recursing until the stack runs out (MON-R44).
func mergedPairs(m *yaml.Node, seen map[*yaml.Node]bool) [][2]*yaml.Node {
	m = resolveAlias(m)
	if m == nil || m.Kind != yaml.MappingNode || seen[m] {
		return nil
	}
	seen[m] = true
	var own, merged [][2]*yaml.Node
	written := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], resolveAlias(m.Content[i+1])
		if k.Tag == "!!merge" {
			srcs := []*yaml.Node{v}
			if v != nil && v.Kind == yaml.SequenceNode {
				srcs = v.Content
			}
			for _, s := range srcs {
				merged = append(merged, mergedPairs(s, seen)...)
			}
			continue
		}
		own = append(own, [2]*yaml.Node{k, v})
		written[k.Value] = true
	}
	for _, kv := range merged {
		if !written[kv[0].Value] {
			own = append(own, kv)
			written[kv[0].Value] = true
		}
	}
	return own
}

// resolveAlias follows a YAML alias to the node it names.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// filePull resolves a configuration file's pull reader at path at from its
// exporter node. The bind's host and port are the file's; its protection is
// METRICS_PULL_*, through the same rule as in environment mode (MON-R7).
// Every other key, and any value that is not a scalar, is reported and
// ignored, its value never quoted (MON-R10, R44).
func (c *Config) filePull(at string, ex *yaml.Node, getenv func(string) string) *Pull {
	var block *yaml.Node
	for _, kv := range mapPairs(ex) {
		k := kv[0].Value
		if k == pullExporterKey {
			block = kv[1]
			continue
		}
		c.Problems = append(c.Problems, Problem{Setting: at + ".exporter." + k,
			Effect: "a pull exporter this gateway does not build is ignored", Fix: "use " + pullExporterKey})
	}
	pe := at + ".exporter." + pullExporterKey
	if block == nil {
		c.problem(at+".exporter", "Prometheus pull", "use the "+pullExporterKey+" exporter")
		return nil
	}
	var host, port string
	switch {
	case block.Kind == yaml.ScalarNode && block.Tag == "!!null":
		// prometheus/development: with no settings takes every default.
	case block.Kind != yaml.MappingNode:
		c.problem(pe, "Prometheus pull", "give the exporter's settings as a mapping, or none")
		return nil
	}
	for _, kv := range mapPairs(block) {
		k, v := kv[0].Value, kv[1]
		path := pe + "." + k
		if v.Kind != yaml.ScalarNode {
			c.Problems = append(c.Problems, Problem{Setting: path,
				Effect: "it is ignored: the gateway reads this exporter's host, port and naming settings only, each a single value",
				Fix:    "remove it"})
			continue
		}
		if v.Tag == "!!null" {
			continue // null takes the default, as the schema says
		}
		switch k {
		case "host":
			host = v.Value
		case "port":
			port = v.Value
		case "without_scope_info":
			c.fixedNaming(path, v, "true", "use true, or leave it unset: the gateway serves no instrumentation scope labels")
		case "without_target_info":
			c.fixedNaming(path, v, "false", "use false, or leave it unset: the gateway's resource is served on target_info")
		case "translation_strategy":
			c.fixedNaming(path, v, "underscore_escaping_with_suffixes", "use underscore_escaping_with_suffixes, or leave it unset")
		default:
			c.Problems = append(c.Problems, Problem{Setting: path,
				Effect: "it is ignored: this gateway does not read it", Fix: "remove it"})
		}
	}
	return c.resolvePull(host, port, pe+".host", pe+".port", getenv)
}

// fixedNaming reports a naming setting that asks for other names than the
// gateway's own, which are fixed so a scrape's series stay as documented
// (MON-R6): the setting is ignored.
func (c *Config) fixedNaming(path string, v *yaml.Node, gateway, fix string) {
	got := v.Value
	if v.Tag == "!!bool" {
		var b bool
		if err := v.Decode(&b); err == nil {
			got = fmt.Sprint(b)
		}
	}
	if got != gateway {
		c.Problems = append(c.Problems, Problem{Setting: path,
			Effect: "it is ignored: the gateway's Prometheus names are fixed", Fix: fix})
	}
}

// The substitution of ${VAR} references in a configuration file, as otelconf
// v0.27.0 applies it to the whole file before parsing: $$ not followed by {
// is a $; ${NAME}, ${env:NAME} and ${NAME:-default} take the variable's
// value, or the default when it is unset or empty (with $$ in the default a
// $); an even run of $ before { escapes the reference; a value that is not
// a single YAML scalar refuses the file.
var (
	fileVarName      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	fileDoubleDollar = regexp.MustCompile(`\$\$([^{$])`)
	fileVarRef       = regexp.MustCompile(`([$]*)\{([a-zA-Z_][a-zA-Z0-9_]*-?[^}]*)\}`)
)

// substituteEnv replaces a file's ${VAR} references through getenv: the
// process environment in a running gateway, which is what otelconf reads.
func substituteEnv(in []byte, getenv func(string) string) ([]byte, error) {
	out := fileDoubleDollar.ReplaceAllFunc(in, func(s []byte) []byte {
		return append([]byte("$"), fileDoubleDollar.FindSubmatch(s)[1]...)
	})
	var err error
	out = fileVarRef.ReplaceAllFunc(out, func(s []byte) []byte {
		m := fileVarRef.FindSubmatch(s)
		dollars, ref := string(m[1]), string(m[2])
		half := dollars[:len(dollars)/2]
		if len(dollars)%2 == 1 {
			var v []byte
			if v, err = fileVarValue(ref, getenv); err != nil {
				return nil
			}
			return append([]byte(half), v...)
		}
		// An escaped reference keeps its text; a default in it that holds a
		// reference is itself substituted.
		_, def, hasDef := strings.Cut(strings.TrimPrefix(ref, "env:"), ":-")
		if !hasDef || !strings.Contains(def, "$") {
			return []byte(half + "{" + ref + "}")
		}
		var v []byte
		if v, err = substituteEnv([]byte(ref+"}"), getenv); err != nil {
			return nil
		}
		return append([]byte(half+"{"), v...)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fileVarValue is one reference's value.
func fileVarValue(ref string, getenv func(string) string) ([]byte, error) {
	name, def, hasDef := strings.Cut(strings.TrimPrefix(ref, "env:"), ":-")
	if strings.Contains(name, ":") || !fileVarName.MatchString(name) {
		return nil, errors.New("invalid environment variable name")
	}
	val := getenv(name)
	if val == "" && hasDef {
		val = strings.ReplaceAll(def, "$$", "$")
	}
	if val == "" {
		return nil, nil
	}
	var raw any
	if err := yaml.Unmarshal([]byte(val), &raw); err != nil {
		return nil, err
	}
	switch raw.(type) {
	case int, int32, int64, float32, float64, bool, string, time.Time:
		return []byte(val), nil
	}
	return nil, errors.New("a value that is not a single scalar")
}
