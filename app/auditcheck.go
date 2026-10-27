//go:build !shn_coreonly

package app

import (
	"net/url"

	checks "github.com/SmartHealthNetwork/shn-gateway/checks"
)

// auditCheckTarget is the AUDIT_ENDPOINT check: the audit endpoint discovery
// named, probed as reachable; or, when it named none, a failing
// endpoint-unknown result, so an operator sees that the gateway's audit
// records are queued, not sent, without reading its log.
func auditCheckTarget(endpoint string) checks.Target {
	if endpoint == "" {
		return checks.Target{ID: "AUDIT_ENDPOINT", Kind: checks.KindUndiscovered}
	}
	return checks.Target{ID: "AUDIT_ENDPOINT", Kind: checks.KindReachable, URL: endpoint}
}

// checksTargetOf is a URL's scheme and host only, for the boot log.
func checksTargetOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
