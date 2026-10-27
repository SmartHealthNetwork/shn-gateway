package checks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docs/CONFIGURATION.md says, in one sentence, every failure code a check
// result can carry (its "`code` is one of" list). This holds that list to the
// package's own failure codes, read from the Fail constants of its non-test
// files: a code added on either side fails it until the other names it.

const configurationPath = "../docs/CONFIGURATION.md"

// documentedCodes are the codes the "`code` is one of" sentence names, each a
// backticked code followed by its meaning in parentheses.
func documentedCodes(doc string) []string {
	start := strings.Index(doc, "`code` is one of")
	if start < 0 {
		return nil
	}
	sentence := doc[start:]
	if end := strings.Index(sentence, "; `hint`"); end >= 0 {
		sentence = sentence[:end]
	}
	var codes []string
	for _, m := range regexp.MustCompile("`([a-z][a-z-]*)`\\s*\\(").FindAllStringSubmatch(sentence, -1) {
		codes = append(codes, m[1])
	}
	return codes
}

// failureCodes are the values of the Fail constants the package's non-test
// files declare, by file name.
func failureCodes(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var codes []string
	for name, src := range srcs {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				for i, n := range v.Names {
					if !strings.HasPrefix(n.Name, "Fail") || i >= len(v.Values) {
						continue
					}
					lit, ok := v.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: %s is not a string literal", name, n.Name)
					}
					code, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					codes = append(codes, code)
				}
			}
		}
	}
	return codes
}

func codeListProblems(t *testing.T, doc string, srcs map[string]string) []string {
	t.Helper()
	documented, codes := documentedCodes(doc), failureCodes(t, srcs)
	if len(codes) < 5 {
		return []string{"the package's failure codes no longer parse"}
	}
	if len(documented) < 5 {
		return []string{"CONFIGURATION.md's \"`code` is one of\" list no longer parses"}
	}
	var problems []string
	seen := map[string]int{}
	for _, c := range documented {
		seen[c]++
		if seen[c] == 2 {
			problems = append(problems, "CONFIGURATION.md names "+c+" twice")
		}
	}
	sort.Strings(documented)
	sort.Strings(codes)
	if strings.Join(documented, ",") != strings.Join(codes, ",") {
		problems = append(problems, "CONFIGURATION.md's failure codes are "+strings.Join(documented, ", ")+"; the checks report "+strings.Join(codes, ", "))
	}
	return problems
}

// readCodeListInputs is the document and every non-test Go file of the
// package, by name.
func readCodeListInputs(t *testing.T) (string, map[string]string) {
	t.Helper()
	d, err := os.ReadFile(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[f] = string(b)
	}
	if _, ok := srcs["checks.go"]; !ok {
		t.Fatal("checks.go not found")
	}
	return string(d), srcs
}

func TestConfigurationNamesEveryFailureCode(t *testing.T) {
	doc, srcs := readCodeListInputs(t)
	for _, p := range codeListProblems(t, doc, srcs) {
		t.Error(p)
	}
}

func TestConfigurationFailureCodeFenceRejects(t *testing.T) {
	doc, srcs := readCodeListInputs(t)
	for _, row := range []struct{ name, file, from, to, want string }{
		{"a code the document leaves out", "doc", "`version-drift` (", "version drift (", "the checks report"},
		{"a code the document names that the checks dropped", "doc", "`internal` (a gateway-side bug", "`internal` (a gateway-side bug), `gone` (dropped", "the checks report"},
		{"a new failure code", "checks.go", "\tFailInternal = \"internal\"\n", "\tFailInternal = \"internal\"\n\tFailNew = \"new-code\"\n", "the checks report"},
		{"a code named twice", "doc", "`internal` (a gateway-side bug", "`internal` (a gateway-side bug), `internal` (again", "twice"},
	} {
		t.Run(row.name, func(t *testing.T) {
			d, s := doc, map[string]string{}
			for k, v := range srcs {
				s[k] = v
			}
			text := d
			if row.file != "doc" {
				text = s[row.file]
			}
			if strings.Count(text, row.from) != 1 {
				t.Fatalf("mutation anchor %q found %d times", row.from, strings.Count(text, row.from))
			}
			text = strings.Replace(text, row.from, row.to, 1)
			if row.file == "doc" {
				d = text
			} else {
				s[row.file] = text
			}
			got := strings.Join(codeListProblems(t, d, s), "\n")
			if !strings.Contains(got, row.want) {
				t.Errorf("want a problem containing %q, got:\n%s", row.want, got)
			}
		})
	}
}
