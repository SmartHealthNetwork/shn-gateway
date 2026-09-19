package pgstore

// ddl_fence_test.go is the HERMETIC half of the schema's guard: it parses the very
// `ddl` text EnsureSchema executes (no database, no build tag — it runs in every
// `go test` of this package) and asserts the two structural properties the shared
// replica state depends on:
//
//  1. no gw_* table declares an opaque blob column (BYTEA/JSON/JSONB) — an opaque
//     column is where clinical Content could hide, and a durable, shared store that
//     carried Content would silently become the longitudinal record AI-1 forbids.
//     Exemptions are per COLUMN, named one by one with a reason, below — never per
//     table, which would leave a table's FUTURE columns unfenced;
//  2. gw_exchange_leg's non-key columns are exactly engine.LegRecord's flattened
//     field set — a new LegRecord field with no column (silently dropped on the
//     durable path while the in-memory mirror keeps it) or a column with no field
//     (a persisted value nothing reads) is red.
//
// The parse is deliberately small and literal: split on CREATE TABLE IF NOT EXISTS,
// take the parenthesised column list, split it on top-level commas, and read the
// first two tokens of each declaration. It does not implement SQL; it implements
// "read the schema the way a reviewer does".

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	engine "github.com/SmartHealthNetwork/shn-gateway/engine"
)

// expectedDDLTables is every table `ddl` must declare. Pinned so a parse that
// silently matched nothing (an unchanged marker string, a reshaped DDL) cannot pass
// this fence by walking an empty table set.
var expectedDDLTables = []string{
	"gw_auth_number", "gw_pended_claim", "gw_pended_claim_key", "gw_eob", // business Store
	"gw_pa_continuation", "gw_pa_continuation_item", // the continuation store
	"gw_ingress_key", "gw_replay", "gw_exchange", "gw_exchange_leg", // shared replica state
}

// contentBearingTypes are the column types a metadata-only table must never declare.
var contentBearingTypes = map[string]bool{"BYTEA": true, "JSON": true, "JSONB": true}

// ddlContentExceptions exempts individual COLUMNS, never whole tables: an exempt
// table would let a blob column added to it tomorrow ship under a green fence. The
// key is "<table>.<column>" and the value is the reason it is not Content.
//
// Coverage is opt-OUT — every column of every gw_* table `ddl` declares is fenced
// unless its exact (table, column) pair is written here — and each entry is checked
// for staleness below, so an exemption cannot outlive the column it excuses.
//
// The design names gw_ingress_key as the one table allowed to hold key material, but
// it needs NO entry today: its PKCS#8 PEM lives in a TEXT column, so nothing about it
// is exempt. If key material ever needs a binary column, that is a deliberate
// "gw_ingress_key.<column>" entry here, reviewed on its own terms — not a standing
// blanket pass on the table.
var ddlContentExceptions = map[string]string{
	"gw_eob.eob_json": "pre-existing business Store column (not shared replica state): the holder's own PA-decision EOB resource, stored as BYTEA since the E-mid Store slice",
}

// legRecordColumns maps every persisted engine.LegRecord field path (LegPhysics
// flattened one level) to its gw_exchange_leg column.
var legRecordColumns = map[string]string{
	"Type":             "leg_type",
	"CorrelationID":    "correlation_id",
	"Subjects":         "subjects",
	"Physics.Kind":     "kind",
	"Physics.Effect":   "effect",
	"Physics.Timing":   "timing",
	"Physics.Locality": "locality",
	"Outcome":          "outcome",
}

// legKeyColumns address and order the row; they are not LegRecord fields.
var legKeyColumns = map[string]bool{"holder_id": true, "exchange_id": true, "seq": true}

// TestReplayDDL_KeyBoundMatchesTheEngineConstant: gw_replay's CHECK is the backstop for
// engine.MaxReplayKeyBytes, and a const string cannot interpolate the constant — so the
// literal is pinned here. If the engine bound moves and the DDL does not, a key the
// callers accept would be refused by the database as a store error.
//
// The assertion is scoped to gw_replay's OWN block. Two tables now declare a
// `key` column with its own bound, and a check against the whole `ddl` string would
// pass on either table's literal — so a bound that drifted on one of them would
// match the other's and stay green.
func TestReplayDDL_KeyBoundMatchesTheEngineConstant(t *testing.T) {
	want := fmt.Sprintf("CHECK (octet_length(key) <= %d)", engine.MaxReplayKeyBytes)
	if body := ddlTableBody(t, "gw_replay"); !strings.Contains(body, want) {
		t.Fatalf("gw_replay does not declare %q — the schema and engine.MaxReplayKeyBytes have drifted.\n%s", want, body)
	}
}

// ddlTableBody returns the parenthesised column list of ONE CREATE TABLE block, so
// an assertion about a table's own declaration cannot be satisfied by another
// table's identical text.
func ddlTableBody(t *testing.T, table string) string {
	t.Helper()
	const marker = "CREATE TABLE IF NOT EXISTS "
	src := stripSQLComments(ddl)
	at := strings.Index(src, marker+table+" ")
	if at < 0 {
		at = strings.Index(src, marker+table+"\n")
	}
	if at < 0 {
		t.Fatalf("ddl declares no CREATE TABLE block for %s", table)
	}
	rest := src[at+len(marker)+len(table):]
	open := strings.Index(rest, "(")
	if open < 0 {
		t.Fatalf("%s: CREATE TABLE block with no column list", table)
	}
	body, ok := balancedParens(rest[open:])
	if !ok {
		t.Fatalf("%s: unbalanced parentheses in the column list", table)
	}
	return body
}

// TestDDLFence_NoNewExceptions: the exception list can only SHRINK. The pend
// ledger's tables were designed with no opaque column precisely so they would need
// no entry here — a new exemption is how a JSON column for "just the keys" would
// arrive, and it never gets to arrive quietly.
func TestDDLFence_NoNewExceptions(t *testing.T) {
	want := map[string]bool{"gw_eob.eob_json": true}
	for qual := range ddlContentExceptions {
		if !want[qual] {
			t.Errorf("ddlContentExceptions gained %q. A gw_* table needing an opaque column is a DESIGN "+
				"change (AI-1), reviewed on its own terms — not a line added to get a schema green.", qual)
		}
	}
	for qual := range want {
		if _, ok := ddlContentExceptions[qual]; !ok {
			t.Errorf("ddlContentExceptions lost %q — if the column is gone, shrink this list too", qual)
		}
	}
}

// TestPendKeyDDL_BoundMatchesTheEngineConstant: gw_pended_claim_key's CHECK is the
// backstop for engine.MaxPendKeyBytes, and a const string cannot interpolate the
// constant — so the literal is pinned here, exactly as gw_replay's is. If the
// engine bound moves and the DDL does not, a key every store accepts would be
// refused by the database as a store error.
func TestPendKeyDDL_BoundMatchesTheEngineConstant(t *testing.T) {
	want := fmt.Sprintf("CHECK (octet_length(key) <= %d)", engine.MaxPendKeyBytes)
	if body := ddlTableBody(t, "gw_pended_claim_key"); !strings.Contains(body, want) {
		t.Fatalf("gw_pended_claim_key does not declare %q — the schema and engine.MaxPendKeyBytes have drifted.\n%s", want, body)
	}
}

func TestExchangeDDL_Fence(t *testing.T) {
	tables := parseDDLTables(t, ddl)
	for _, want := range expectedDDLTables {
		if _, ok := tables[want]; !ok {
			t.Fatalf("ddl declares no CREATE TABLE block for %s (parsed: %s)", want, sortedNames(tables))
		}
	}

	t.Run("no content-bearing columns", func(t *testing.T) {
		for _, name := range sortedNames(tables) {
			for _, col := range tables[name] {
				if !contentBearingTypes[normalizeSQLType(col.typ)] {
					continue
				}
				qual := name + "." + col.name
				if reason, exempt := ddlContentExceptions[qual]; exempt {
					t.Logf("%s (%s): exempt — %s", qual, col.typ, reason)
					continue
				}
				t.Errorf("%s is %s: a gw_* table must declare no opaque blob column (AI-1: metadata only) — "+
					"an opaque column is where Content would hide in a durable, shared store. "+
					"If this column truly is not Content, add %q to ddlContentExceptions with the reason.",
					qual, col.typ, qual)
			}
		}
		// Staleness is per ENTRY: an exemption must still name a real column that is
		// still of a banned type, or it is excusing nothing and must go.
		for _, qual := range sortedKeys(mapKeys(ddlContentExceptions)) {
			name, colName, ok := strings.Cut(qual, ".")
			if !ok {
				t.Errorf("ddlContentExceptions key %q is not \"<table>.<column>\"", qual)
				continue
			}
			cols, ok := tables[name]
			if !ok {
				t.Errorf("ddlContentExceptions exempts %s, but ddl no longer declares table %s — drop the stale exemption", qual, name)
				continue
			}
			typ, ok := columnType(cols, colName)
			if !ok {
				t.Errorf("ddlContentExceptions exempts %s, but %s has no column %s — drop the stale exemption", qual, name, colName)
				continue
			}
			if !contentBearingTypes[normalizeSQLType(typ)] {
				t.Errorf("ddlContentExceptions exempts %s, but it is now %s (not a content-bearing type) — "+
					"drop the stale exemption so the column is fenced again", qual, typ)
			}
		}
	})

	t.Run("gw_exchange_leg columns match engine.LegRecord", func(t *testing.T) {
		fields := flattenedFieldPaths(reflect.TypeOf(engine.LegRecord{}))
		for _, f := range fields {
			if _, ok := legRecordColumns[f]; !ok {
				t.Errorf("engine.LegRecord field %s has no gw_exchange_leg column: the durable store would drop it "+
					"while the in-memory store keeps it — add the column and the mapping", f)
			}
		}
		known := map[string]bool{}
		for _, f := range fields {
			known[f] = true
		}
		for f := range legRecordColumns {
			if !known[f] {
				t.Errorf("legRecordColumns maps %s, which engine.LegRecord no longer has — drop the column and the mapping", f)
			}
		}

		got := map[string]bool{}
		for _, col := range tables["gw_exchange_leg"] {
			if legKeyColumns[col.name] {
				continue
			}
			got[col.name] = true
		}
		for _, key := range sortedKeys(legKeyColumns) {
			if _, ok := columnType(tables["gw_exchange_leg"], key); !ok {
				t.Errorf("gw_exchange_leg declares no key column %s — legKeyColumns is stale", key)
			}
		}
		for _, f := range sortedKeys(mapKeys(legRecordColumns)) {
			col := legRecordColumns[f]
			if !got[col] {
				t.Errorf("gw_exchange_leg has no column %s for engine.LegRecord field %s", col, f)
			}
			delete(got, col)
		}
		for _, extra := range sortedKeys(got) {
			t.Errorf("gw_exchange_leg column %s maps to no engine.LegRecord field — a persisted value nothing reads", extra)
		}
	})
}

// --- the parse ---

// ddlColumn is one parsed column declaration.
type ddlColumn struct{ name, typ string }

// parseDDLTables returns the column declarations of every CREATE TABLE block in src.
func parseDDLTables(t *testing.T, src string) map[string][]ddlColumn {
	t.Helper()
	src = stripSQLComments(src)
	out := map[string][]ddlColumn{}
	const marker = "CREATE TABLE IF NOT EXISTS"
	blocks := strings.Split(src, marker)
	if len(blocks) < 2 {
		t.Fatalf("no %q blocks found in the ddl constant", marker)
	}
	for _, block := range blocks[1:] {
		open := strings.Index(block, "(")
		if open < 0 {
			t.Fatalf("CREATE TABLE block with no column list: %.60q", block)
		}
		name := strings.TrimSpace(block[:open])
		body, ok := balancedParens(block[open:])
		if !ok {
			t.Fatalf("%s: unbalanced parentheses in the column list", name)
		}
		var cols []ddlColumn
		for _, decl := range splitTopLevelCommas(body) {
			decl = strings.TrimSpace(decl)
			if decl == "" || isTableConstraint(decl) {
				continue
			}
			f := strings.Fields(decl)
			if len(f) < 2 {
				t.Fatalf("%s: column declaration %q has no type", name, decl)
			}
			cols = append(cols, ddlColumn{name: f[0], typ: f[1]})
		}
		if len(cols) == 0 {
			t.Fatalf("%s: parsed no columns — the parse is broken, not the schema", name)
		}
		if _, dup := out[name]; dup {
			t.Fatalf("%s: declared twice in ddl", name)
		}
		out[name] = cols
	}
	return out
}

// stripSQLComments removes `-- …` to end of line so a comma or parenthesis inside a
// comment cannot confuse the split.
func stripSQLComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "--"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// balancedParens returns the content between s[0]=='(' and its matching ')'.
func balancedParens(s string) (string, bool) {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// splitTopLevelCommas splits on commas at parenthesis depth 0, so the commas inside
// `PRIMARY KEY (a, b)` stay with their constraint.
func splitTopLevelCommas(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// isTableConstraint reports whether a declaration is a table constraint rather than
// a column. A bare CHECK is one: its expression would otherwise parse as a column named
// "CHECK" with a type of "(octet_length(key)".
func isTableConstraint(decl string) bool {
	up := strings.ToUpper(decl)
	for _, kw := range []string{"PRIMARY KEY", "FOREIGN KEY", "CONSTRAINT", "UNIQUE", "CHECK"} {
		if strings.HasPrefix(up, kw) {
			return true
		}
	}
	return false
}

// normalizeSQLType uppercases a declared type and drops an array suffix, so TEXT[]
// compares as TEXT and a hypothetical jsonb[] still trips the fence.
func normalizeSQLType(typ string) string {
	return strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(typ)), "[]")
}

// columnType returns the declared type of the named column, if the table has it.
func columnType(cols []ddlColumn, name string) (string, bool) {
	for _, c := range cols {
		if c.name == name {
			return c.typ, true
		}
	}
	return "", false
}

// flattenedFieldPaths walks every field of t, flattening a struct field one level
// (LegPhysics → Physics.Kind, …) — the shape the DDL persists. Unexported fields are
// NOT filtered out on purpose: a LegRecord that grew an unexported field would be
// persisting something the column set does not describe, and the fence should say so.
func flattenedFieldPaths(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Type.Kind() == reflect.Struct {
			for j := 0; j < f.Type.NumField(); j++ {
				out = append(out, f.Name+"."+f.Type.Field(j).Name)
			}
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

func sortedNames(tables map[string][]ddlColumn) []string {
	out := make([]string, 0, len(tables))
	for name := range tables {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mapKeys(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
