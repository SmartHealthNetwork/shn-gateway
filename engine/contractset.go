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
// workstream is compiled in. A workstream may also have retired a token it no
// longer declares (contractRetirer): a declared set naming one is refused with
// the workstream's own reason. Every gateway read of the native set, the build
// default and a declared set goes through these, never the SDK's directly.

// NativeContractVersions is every contract-version token this gateway builds
// natively: each compiled-in workstream's, in registry order (PA's are the
// SDK's native set, in its order).
func NativeContractVersions() []string {
	return workstreamContractVersions(func(native, _ []string) []string { return native })
}

// DefaultContractVersions is this gateway's build default declaration: each
// compiled-in workstream's default, in registry order (PA's is the SDK's
// default).
func DefaultContractVersions() []string {
	return workstreamContractVersions(func(_, defaults []string) []string { return defaults })
}

// ParseDeclaredContractVersions reads a deployment's declared set as
// shnsdk.ParseDeclaredContractVersions does, against this gateway's native
// set rather than the SDK's, and refuses a set naming a token a compiled-in
// workstream retired, with that workstream's reason, wherever it stands and
// whatever else the set names. A value that names no token (empty, or only
// commas and spaces) is this gateway's default.
func ParseDeclaredContractVersions(csv string) ([]string, error) {
	for _, raw := range strings.Split(csv, ",") {
		if err := retiredContractVersion(strings.TrimSpace(raw)); err != nil {
			return nil, err
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

// sdkContractVersions are the SDK's native set and default: the PA contract
// lines the PA workstream supplies. The SDK the gateway requires carries no
// retired PA token in either (a test pins that). It is the one read of the
// SDK's sets outside the gateway's own (above).
func sdkContractVersions() (native, defaults []string) {
	return shnsdk.NativeContractVersions(), shnsdk.SupportedContractVersions()
}
