package fhirseed

import (
	"embed"
	"strings"
)

//go:embed crlibraries/Library-*.json
var crLibraryFS embed.FS

// CRPrepopLibraries returns the operated-$populate prepop CQL Libraries, keyed by Library id
// (the filename's "Library-<id>.json" stem), with the file bytes as value. These are installed
// into the CR HAPI DEFAULT partition (HAPI-1318: a Library cannot live in a tenant partition);
// HAPI CR compiles each text/cql → ELM on first $populate reference. Single source of truth shared
// with the live conformance gate's stack orchestration, which installs the same files. Cribbed
// from br-payer a8bece4 (MIT).
func CRPrepopLibraries() map[string][]byte {
	entries, err := crLibraryFS.ReadDir("crlibraries")
	if err != nil {
		panic("fhirseed: embedded crlibraries unreadable: " + err.Error())
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "Library-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "Library-"), ".json")
		b, err := crLibraryFS.ReadFile("crlibraries/" + name)
		if err != nil {
			panic("fhirseed: read embedded library " + name + ": " + err.Error())
		}
		out[id] = b
	}
	return out
}
