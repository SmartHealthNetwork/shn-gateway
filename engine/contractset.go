package engine

import (
	"fmt"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The gateway's own contract sets: the union of the compiled-in workstreams'
// (workstream.contractVersions). The SDK publishes PA's contract lines, and PA
// supplies them; the SDK's set is not the whole native set once another
// workstream is compiled in. shn-sdk v0.62.0 still lists pa.pdex@2.1
// (shnsdk.ContractPAPDex21) natively and by default, and its parser accepts
// it; this gateway declares no PDex surface: it serves no Patient Access API
// and builds no PDex resource, so a declaration must not claim it. Every gateway read of the native set, the
// build default and a declared set goes through these, never the SDK's
// directly (the SDK drops the line in the release after v0.62.0).

// NativeContractVersions is every contract-version token this gateway builds
// natively: each compiled-in workstream's, in registry order (PA's are the
// SDK's native set without pa.pdex@2.1, in its order).
func NativeContractVersions() []string {
	return workstreamContractVersions(func(native, _ []string) []string { return native })
}

// DefaultContractVersions is this gateway's build default declaration: each
// compiled-in workstream's default, in registry order (PA's is the SDK's
// default without pa.pdex@2.1).
func DefaultContractVersions() []string {
	return workstreamContractVersions(func(_, defaults []string) []string { return defaults })
}

// ParseDeclaredContractVersions reads a deployment's declared set as
// shnsdk.ParseDeclaredContractVersions does, against this gateway's native
// set rather than the SDK's, and refuses a set naming pa.pdex@2.1, saying to
// remove it. A value that names no token (empty, or only commas and spaces) is
// this gateway's default.
func ParseDeclaredContractVersions(csv string) ([]string, error) {
	for _, raw := range strings.Split(csv, ",") {
		if strings.TrimSpace(raw) == shnsdk.ContractPAPDex21 {
			return nil, ErrPDexUndeclared
		}
	}
	native := NativeContractVersions()
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		contract, line, ok := strings.Cut(tok, "@")
		if !ok || contract == "" || line == "" {
			return nil, fmt.Errorf("contract-version token %q is malformed: want \"<contract>@<line>\" (e.g. pa.pas@2.0)", tok)
		}
		if !slices.Contains(native, tok) {
			return nil, fmt.Errorf("contract-version token %q is not natively buildable by this build (native set: %s)", tok, strings.Join(native, ","))
		}
		out = append(out, tok)
	}
	if len(out) == 0 {
		return DefaultContractVersions(), nil
	}
	return out, nil
}

// ErrPDexUndeclared refuses a declared set naming pa.pdex@2.1.
var ErrPDexUndeclared = fmt.Errorf("contract-version token %q is no longer declared by any gateway: a gateway serves no Patient Access API and builds no PDex resource, so a declaration must not claim it: remove it from the declared set (SHN_CONTRACT_VERSIONS)", shnsdk.ContractPAPDex21)

// sdkContractVersions are the SDK's native set and default without
// pa.pdex@2.1: the PA contract lines the PA workstream supplies. It is the one
// read of the SDK's sets outside the gateway's own (above).
func sdkContractVersions() (native, defaults []string) {
	return withoutPDex(shnsdk.NativeContractVersions()), withoutPDex(shnsdk.SupportedContractVersions())
}

func withoutPDex(set []string) []string {
	return slices.DeleteFunc(set, func(tok string) bool { return tok == shnsdk.ContractPAPDex21 })
}
