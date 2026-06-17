package shngateway_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoSubstrateInternalImports asserts the shn-gateway module's PRODUCTION
// closure (go list .Imports — NOT .TestImports) imports no shn-platform/internal/*
// package. Mirrors internal/gateway/boundary_test.go's production-only scoping: the
// lifted test files (gateway/engine + connectors/fhirsor) may have their own imports,
// but only production imports gate liftability. A substrate-internal import here would
// mean the lift is incomplete (and would not compile cross-module anyway).
func TestNoSubstrateInternalImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps=false",
		"-f", "{{.ImportPath}}|{{range .Imports}}{{.}} {{end}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		pkg, imports := parts[0], parts[1]
		for _, imp := range strings.Fields(imports) {
			if strings.Contains(imp, "SmartHealthNetwork/shn-platform/internal/") {
				t.Errorf("%s imports forbidden substrate-internal package %s — the gateway lift must be self-contained (shn-sdk + stdlib/external + gateway-module-internal only)", pkg, imp)
			}
		}
	}
}
