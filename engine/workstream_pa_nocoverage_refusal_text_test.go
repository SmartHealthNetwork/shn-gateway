//go:build !shn_coreonly

package engine

import (
	"strings"
	"testing"
)

// TestNoCoverageRefusals_KeepTheirDocumentedPrefixes pins the Da Vinci
// ingress's no-coverage refusals byte for byte. The protocol tells partners
// that a CDS Hooks reason of this kind begins "no coverage in request or system
// of record" or "no coverage to route by:", so each row also holds its prefix: a
// change to a prefix breaks a partner that matches on it, and only the
// explanation in parentheses may change, in a release that names it. DTR's
// refusals are not documented that way; they keep the same beginnings, pinned
// here.
func TestNoCoverageRefusals_KeepTheirDocumentedPrefixes(t *testing.T) {
	const (
		inRequestOrSoR = "no coverage in request or system of record"
		toRouteBy      = "no coverage to route by:"
	)
	rows := map[string]struct {
		got, want, prefix string
	}{
		"CRD and DTR, the system holds the member": {
			noCoverageInRequestOrSoR,
			"no coverage in request or system of record",
			inRequestOrSoR,
		},
		"CRD, a member the gateway names no patient for": {
			crdNoCoverageToRouteBy,
			"no coverage to route by: send prefetch.coverage or fhirServer (this gateway's records name no patient for this member)",
			toRouteBy,
		},
		"CRD, the fhirServer read off": {
			crdNoCoverageReadOff,
			"no coverage to route by: send prefetch.coverage (this gateway's records name no patient for this member, and it does not read fhirServer)",
			toRouteBy,
		},
		"DTR, a member the gateway names no patient for": {
			dtrNoCoverageToRouteBy,
			"no coverage to route by: send the coverage parameter (this gateway's records name no patient for this member)",
			toRouteBy,
		},
	}
	for name, r := range rows {
		t.Run(name, func(t *testing.T) {
			if r.got != r.want {
				t.Errorf("refusal text = %q, want %q", r.got, r.want)
			}
			if !strings.HasPrefix(r.got, r.prefix) {
				t.Errorf("refusal %q does not begin with its documented prefix %q", r.got, r.prefix)
			}
		})
	}
}
