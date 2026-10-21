package engine

import (
	"bufio"
	"bytes"
	"go/build/constraint"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// inThisBuild reports whether the file name in dir is compiled into the build
// this test binary is: its //go:build line, if it has one, holds under the
// build profile tags the binary was built with (thisBuildTags). A type check
// of the package's sources must see one build's files, not both sides of a
// build tag. (No file in this package is selected by its name's GOOS or
// GOARCH suffix.)
func inThisBuild(t *testing.T, dir, name string) bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return expr.Eval(func(tag string) bool {
			if !knownBuildTag(tag) {
				t.Fatalf("%s: build tag %q is not one inThisBuild knows: extend knownBuildTag", name, tag)
			}
			return tag == runtime.GOOS || tag == runtime.GOARCH || tag == "unix" && unixGOOS[runtime.GOOS] || slices.Contains(thisBuildTags, tag)
		})
	}
	return true
}

// unixGOOS are the operating systems the "unix" build tag selects.
var unixGOOS = map[string]bool{"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "linux": true, "netbsd": true, "openbsd": true, "solaris": true}

// knownBuildTag reports whether inThisBuild can decide tag: an operating
// system or architecture, "unix", or a build profile tag. Any other tag
// (cgo, a release tag, a custom one) stops the test rather than reading as
// false, which the compiler might not agree with.
func knownBuildTag(tag string) bool {
	return unixGOOS[tag] || tag == "unix" || tag == "windows" || tag == "js" || tag == "wasip1" || tag == "plan9" ||
		slices.Contains([]string{"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "riscv64", "s390x", "wasm"}, tag) ||
		tag == "shn_toyworkstream"
}

// catalogLegs are the legs of every compiled-in workstream's catalog, sorted:
// PA's and FQS's in a release build, and the test profiles' beside them under
// their tags.
func catalogLegs() []string {
	var out []string
	for _, c := range workstreamCatalogs() {
		out = append(out, slices.Collect(maps.Keys(c))...)
	}
	slices.Sort(out)
	return out
}

// networkWorkstream reports whether id is one of the network's workstreams,
// which every build compiles in, rather than a test profile.
func networkWorkstream(id string) bool { return id == workstreamPA || id == workstreamFQS }

// networkLegs are the legs of the network's workstreams' catalogs (PA's and
// FQS's), sorted: every leg of a release build, and no test profile's.
func networkLegs() []string {
	var out []string
	for _, w := range workstreams {
		if networkWorkstream(w.id()) {
			out = append(out, slices.Collect(maps.Keys(w.catalog()))...)
		}
	}
	slices.Sort(out)
	return out
}

// profileLegs are the legs the compiled-in test profiles' workstreams add:
// none in a release build.
func profileLegs() []string {
	var out []string
	for _, w := range workstreams {
		if !networkWorkstream(w.id()) {
			out = append(out, slices.Collect(maps.Keys(w.catalog()))...)
		}
	}
	slices.Sort(out)
	return out
}

// profileContracts are the contracts the compiled-in test profiles add: none
// in a release build.
func profileContracts() map[string]bool {
	out := map[string]bool{}
	for _, w := range workstreams {
		if networkWorkstream(w.id()) {
			continue
		}
		native, _ := w.contractVersions()
		for _, tok := range native {
			c, _, _ := strings.Cut(tok, "@")
			out[c] = true
		}
	}
	return out
}

// paNativeContractVersions is the PA adapter's native set, which PA-pinned
// tests enumerate in every build.
func paNativeContractVersions() []string {
	native, _ := paAdapter.contractVersions()
	return native
}
