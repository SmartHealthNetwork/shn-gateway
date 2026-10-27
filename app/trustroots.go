//go:build !shn_coreonly

package app

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/SmartHealthNetwork/shn-gateway/internal/endpointcert"
)

// systemTrustFiles and systemTrustDirs are the bundle files and directories
// Go reads on Linux when SSL_CERT_FILE and SSL_CERT_DIR are unset
// (crypto/x509, root_linux.go): the image's or the operating system's own
// roots. Naming one of them in either setting does not make it a custom
// bundle (deploy/compose.two-ri.yml names /etc/ssl/certs beside its own CA).
var (
	systemTrustFiles = []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/cert.pem",
	}
	systemTrustDirs = []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}
)

// customTrustRoot is the endpoint a custom bundle's reading is kept under:
// one series however many files and directories it spans.
const customTrustRoot = "custom-bundle"

// recordTrustRoots records the soonest expiry of the custom trust roots a
// participant added through SSL_CERT_FILE or SSL_CERT_DIR (MON-R25), as one
// trust-root reading. The image's and the system's own roots are never read:
// a stock bundle carries roots nothing uses, and its soonest says nothing
// about the participant's own. What cannot be read is skipped, as Go's own
// loader skips it; nothing here fails a boot.
func recordTrustRoots(rec *endpointcert.Recorder, getenv func(string) string) {
	var paths []string
	if f := getenv("SSL_CERT_FILE"); f != "" && !slices.Contains(systemTrustFiles, filepath.Clean(f)) {
		paths = append(paths, f)
	}
	for _, d := range filepath.SplitList(getenv("SSL_CERT_DIR")) {
		if d == "" || slices.Contains(systemTrustDirs, filepath.Clean(d)) {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			paths = append(paths, filepath.Join(d, e.Name()))
		}
	}
	var soonest time.Time
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for len(data) > 0 {
			var block *pem.Block
			block, data = pem.Decode(data)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			if soonest.IsZero() || c.NotAfter.Before(soonest) {
				soonest = c.NotAfter
			}
		}
	}
	if !soonest.IsZero() {
		rec.Record(endpointcert.Of(endpointcert.TrustRootKind), endpointcert.TrustRoot, customTrustRoot, soonest)
	}
}
