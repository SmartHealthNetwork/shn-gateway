package engine

import (
	"fmt"
	"slices"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The gateway's own contract sets. shn-sdk v0.62.0 still lists pa.pdex@2.1
// (shnsdk.ContractPAPDex21) natively and by default, and its parser accepts
// it; this gateway declares no PDex line: its Patient Access decision
// ExplanationOfBenefit is base FHIR R4 and does not meet Da Vinci PDex 2.1, so
// a declaration must not claim it. Every gateway read of the native set, the
// build default and a declared set goes through these, never the SDK's
// directly (the SDK drops the line in the release after v0.62.0).

// NativeContractVersions is every contract-version token this gateway builds
// natively: the SDK's native set without pa.pdex@2.1, in its order.
func NativeContractVersions() []string { return withoutPDex(shnsdk.NativeContractVersions()) }

// DefaultContractVersions is this gateway's build default declaration: the
// SDK's default without pa.pdex@2.1.
func DefaultContractVersions() []string { return withoutPDex(shnsdk.SupportedContractVersions()) }

// ParseDeclaredContractVersions reads a deployment's declared set as
// shnsdk.ParseDeclaredContractVersions does, and refuses a set naming
// pa.pdex@2.1, saying to remove it. A value that names no token (empty, or
// only commas and spaces) is this gateway's default: the SDK answers it with
// its own default, which still lists the line, so the result never carries it.
func ParseDeclaredContractVersions(csv string) ([]string, error) {
	for _, raw := range strings.Split(csv, ",") {
		if strings.TrimSpace(raw) == shnsdk.ContractPAPDex21 {
			return nil, ErrPDexUndeclared
		}
	}
	parsed, err := shnsdk.ParseDeclaredContractVersions(csv)
	if err != nil {
		return nil, err
	}
	return withoutPDex(parsed), nil
}

// ErrPDexUndeclared refuses a declared set naming pa.pdex@2.1.
var ErrPDexUndeclared = fmt.Errorf("contract-version token %q is no longer declared by any gateway: the Patient Access API's decision ExplanationOfBenefit is base FHIR R4 and does not meet Da Vinci PDex 2.1, so a declaration must not claim it: remove it from the declared set (SHN_CONTRACT_VERSIONS)", shnsdk.ContractPAPDex21)

func withoutPDex(set []string) []string {
	return slices.DeleteFunc(set, func(tok string) bool { return tok == shnsdk.ContractPAPDex21 })
}
