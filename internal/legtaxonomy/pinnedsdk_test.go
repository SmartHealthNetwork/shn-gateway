package legtaxonomy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// catalogTaxonomies are the taxonomies the release build's engine derives its
// catalog from: every file this package embeds, the toy test profile aside.
// TestCatalogTaxonomies_AreEveryEmbeddedFile ties the list to the files, so a
// workstream added here is checked against the pinned SDK too.
func catalogTaxonomies() []Taxonomy {
	return []Taxonomy{PA(), FQS()}
}

// TestCatalogTaxonomies_AreEveryEmbeddedFile: each taxonomy file in this
// package (testprofile/ holds the toy) is one of catalogTaxonomies, and each
// of those is a file here.
func TestCatalogTaxonomies_AreEveryEmbeddedFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		files = append(files, tx.Workstream)
	}
	var listed []string
	for _, tx := range catalogTaxonomies() {
		listed = append(listed, tx.Workstream)
	}
	slices.Sort(files)
	slices.Sort(listed)
	if len(files) == 0 || !slices.Equal(files, listed) {
		t.Fatalf("the taxonomy files here are workstreams %q, catalogTaxonomies %q: list every file the engine derives a catalog from", files, listed)
	}
}

// missingWireNames names each leg of the catalog the engine derives from the
// taxonomies (every routable leg) whose flat and prefixed names the SDK's leg
// table (flat name to prefixed name) does not map to each other. The
// gateway's audit-append guard reads that table, so a release built against
// an SDK whose table lacks a catalog leg refuses every module record naming
// it.
func missingWireNames(taxonomies []Taxonomy, table map[string]string) []string {
	var missing []string
	for _, t := range taxonomies {
		for _, l := range t.Legs {
			if !l.Routable() {
				continue
			}
			switch prefixed, ok := table[l.TransactionType]; {
			case !ok:
				missing = append(missing, fmt.Sprintf("%s: the SDK's leg table has no %s", t.Workstream, l.TransactionType))
			case prefixed != l.StagedName:
				missing = append(missing, fmt.Sprintf("%s: the SDK's leg table names %s %q, the catalog %q", t.Workstream, l.TransactionType, prefixed, l.StagedName))
			}
		}
	}
	slices.Sort(missing)
	return missing
}

// TestMissingWireNames_NamesEachLegThePinLacks: a table naming every catalog
// leg passes; a table lacking a leg, or naming it under another prefix, fails
// naming that leg; a leg outside the catalog is not asked for.
func TestMissingWireNames_NamesEachLegThePinLacks(t *testing.T) {
	catalog := catalogTaxonomies()
	every := map[string]string{}
	for _, tx := range catalog {
		for _, l := range tx.Legs {
			if l.Routable() {
				every[l.TransactionType] = l.StagedName
			}
		}
	}
	if len(every) < 2 {
		t.Fatalf("the catalog has %d legs; the rows below need at least two", len(every))
	}
	without := func(flat string) map[string]string {
		m := map[string]string{}
		for k, v := range every {
			if k != flat {
				m[k] = v
			}
		}
		return m
	}
	with := func(flat, prefixed string) map[string]string {
		m := without(flat)
		m[flat] = prefixed
		return m
	}
	notRoutable := PA()
	notRoutable.Legs = append(notRoutable.Legs, Leg{TransactionType: "not-routable", StagedName: "pa.not-routable"})
	for _, c := range []struct {
		name       string
		taxonomies []Taxonomy
		table      map[string]string
		want       []string
	}{
		{"a table naming every leg", catalog, every, nil},
		{"a table lacking a PA leg", catalog, without("cdex-attachment-submit"), []string{"pa: the SDK's leg table has no cdex-attachment-submit"}},
		{"a table lacking the federated query", catalog, without("federated-query"), []string{"fqs: the SDK's leg table has no federated-query"}},
		{"a table naming a leg under another prefix", catalog, with("pas-claim", "fqs.pas-claim"), []string{`pa: the SDK's leg table names pas-claim "fqs.pas-claim", the catalog "pa.pas-claim"`}},
		{"an empty table", []Taxonomy{FQS()}, map[string]string{}, []string{"fqs: the SDK's leg table has no federated-query"}},
		{"a leg that is not routable", []Taxonomy{notRoutable}, without("federated-query"), nil},
	} {
		if got := missingWireNames(c.taxonomies, c.table); !slices.Equal(got, c.want) {
			t.Errorf("%s: missing %q, want %q", c.name, got, c.want)
		}
	}
	if got := missingWireNames(catalog, map[string]string{}); len(got) != len(every) || !strings.Contains(strings.Join(got, "\n"), "has no pas-claim") {
		t.Errorf("an empty table names %d legs (%q), want all %d", len(got), got, len(every))
	}
}
