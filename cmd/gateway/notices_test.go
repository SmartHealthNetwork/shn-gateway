//go:build !shn_coreonly

package main

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"testing"
)

// noticeRow is one module row of THIRD_PARTY_NOTICES.md.
type noticeRow struct{ version, license, link string }

// noticeRowShape is a table row: the module, its version, its licence (an
// SPDX identifier or expression, which can hold spaces) and its link.
var noticeRowShape = regexp.MustCompile("^\\| `([^`]+)` \\| ([^ |]+) \\| ([^|]*[^ |]) \\| ([^ |]+) \\|$")

// noticeLicenses are the licences a linked module may carry. "MIT AND
// Apache-2.0" is go.yaml.in/yaml/v3's: its original code is MIT and the rest
// Apache-2.0, both texts and its NOTICE carried.
var noticeLicenses = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "MIT", "MIT AND Apache-2.0"}

// imageGOOS is the operating system the image builds the gateway for; the
// notices list the modules that build links.
const imageGOOS = "linux"

// noticeProblems compares the notices' rows with the modules the binary
// links and the versions go.mod sets. imageOS is whether the binary was
// built for the image's operating system: one built for another can link
// fewer modules (the Prometheus client links procfs on linux only), so only
// there is a row for a module it does not link reported.
func noticeProblems(linked []string, required map[string]string, rows map[string]noticeRow, imageOS bool) []string {
	var out []string
	for _, path := range linked {
		row, ok := rows[path]
		switch {
		case !ok:
			out = append(out, path+": linked but has no row")
		case row.version != required[path]:
			out = append(out, fmt.Sprintf("%s: row says %s, go.mod requires %s", path, row.version, required[path]))
		case !slices.Contains(noticeLicenses, row.license):
			out = append(out, fmt.Sprintf("%s: licence %q is not one a linked module may carry", path, row.license))
		case row.link != fmt.Sprintf("https://pkg.go.dev/%s@%s?tab=licenses", path, row.version):
			out = append(out, path+": the link is not the version's licence page")
		}
	}
	for path := range rows {
		if imageOS && !slices.Contains(linked, path) {
			out = append(out, path+": has a row but is not linked")
		}
	}
	sort.Strings(out)
	return out
}

func readNoticeRows(t *testing.T, doc string) map[string]noticeRow {
	t.Helper()
	rows := map[string]noticeRow{}
	for _, line := range strings.Split(doc, "\n") {
		if m := noticeRowShape.FindStringSubmatch(line); m != nil {
			rows[m[1]] = noticeRow{m[2], m[3], m[4]}
		}
	}
	return rows
}

// goModVersions are the versions go.mod requires.
func goModVersions(gomod string) map[string]string {
	out := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
			continue
		case line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inBlock:
			continue
		}
		if f := strings.Fields(line); len(f) >= 2 {
			out[f[0]] = f[1]
		}
	}
	return out
}

// The image's third-party notices name every module the released binary
// links (a core-only build links fewer, so the test runs in the others), at
// the version the gateway's go.mod sets, with its licence, and the image
// carries them (MON-R43).
func TestThirdPartyNoticesCoverTheBinary_MON_R43(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	var linked []string
	for _, d := range bi.Deps {
		linked = append(linked, d.Path)
	}
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatal(err)
	}
	rows := readNoticeRows(t, string(doc))
	for _, p := range noticeProblems(linked, goModVersions(string(gomod)), rows, runtime.GOOS == imageGOOS) {
		t.Error(p)
	}
	// Each module's licence text follows the table, and the Go standard
	// library's last.
	for path, row := range rows {
		if !strings.Contains(string(doc), "\n### `"+path+"` "+row.version+"\n\nLICEN") {
			t.Errorf("%s: no licence text in the notices", path)
		}
	}
	if !strings.Contains(string(doc), "\n### The Go standard library (") {
		t.Error("the notices carry no licence for the Go standard library")
	}
	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "COPY THIRD_PARTY_NOTICES.md LICENSE /licenses/") {
		t.Error("the image does not carry the notices and the licence in /licenses")
	}
}

// The comparison's rejection rows: each mutation of a matching table is
// reported.
// A row is read whole whatever its licence: an SPDX expression holds spaces.
func TestNoticeRowsReadALicenceExpression(t *testing.T) {
	rows := readNoticeRows(t, "| `example.com/a` | v1.0.0 | MIT AND Apache-2.0 | https://pkg.go.dev/example.com/a@v1.0.0?tab=licenses |\n"+
		"| `example.com/b` | v2.0.0 | MIT | https://pkg.go.dev/example.com/b@v2.0.0?tab=licenses |\n")
	if got := rows["example.com/a"]; got.license != "MIT AND Apache-2.0" || got.version != "v1.0.0" {
		t.Errorf("an expression licence reads %+v", got)
	}
	if got := rows["example.com/b"]; got.license != "MIT" {
		t.Errorf("a single licence reads %+v", got)
	}
}

func TestThirdPartyNoticeProblems(t *testing.T) {
	linked := []string{"example.com/a", "example.com/b"}
	required := map[string]string{"example.com/a": "v1.0.0", "example.com/b": "v2.0.0"}
	good := func() map[string]noticeRow {
		return map[string]noticeRow{
			"example.com/a": {"v1.0.0", "MIT", "https://pkg.go.dev/example.com/a@v1.0.0?tab=licenses"},
			"example.com/b": {"v2.0.0", "Apache-2.0", "https://pkg.go.dev/example.com/b@v2.0.0?tab=licenses"},
		}
	}
	if p := noticeProblems(linked, required, good(), true); len(p) != 0 {
		t.Fatalf("a matching table reports %v", p)
	}
	// Built for another operating system, a row the binary does not link
	// passes (the image's build reports it), and a linked module without a
	// row still fails.
	stale := good()
	stale["example.com/linux-only"] = noticeRow{"v1", "MIT", "x"}
	if p := noticeProblems(linked, required, stale, false); len(p) != 0 {
		t.Errorf("off the image's OS, a row for a module not linked here reports %v", p)
	}
	missing := good()
	delete(missing, "example.com/b")
	if p := noticeProblems(linked, required, missing, false); len(p) != 1 {
		t.Errorf("off the image's OS, a missing row reports %v, want exactly one", p)
	}
	for name, mutate := range map[string]func(map[string]noticeRow){
		"missing row":   func(r map[string]noticeRow) { delete(r, "example.com/b") },
		"stale row":     func(r map[string]noticeRow) { r["example.com/gone"] = noticeRow{"v1", "MIT", "x"} },
		"wrong version": func(r map[string]noticeRow) { a := r["example.com/a"]; a.version = "v0.9.0"; r["example.com/a"] = a },
		"licence":       func(r map[string]noticeRow) { a := r["example.com/a"]; a.license = "GPL-3.0"; r["example.com/a"] = a },
		"link": func(r map[string]noticeRow) {
			a := r["example.com/a"]
			a.link = "https://example.com"
			r["example.com/a"] = a
		},
	} {
		rows := good()
		mutate(rows)
		if p := noticeProblems(linked, required, rows, true); len(p) != 1 {
			t.Errorf("%s: problems %v, want exactly one", name, p)
		}
	}
}
