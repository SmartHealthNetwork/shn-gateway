//go:build !shn_coreonly

package engine

import (
	"net/http"
	"strconv"
)

// handleCDSClientJWKS serves the public JWK Set of the payer client key this
// payer's gateway signs its CDS Hooks client JWTs with (CDS Hooks 2.0,
// "Trusting CDS Clients": the client publishes its public key as a JWK Set,
// RFC 7517). It needs no credential: a JWK Set holds public keys only, and
// New refuses one that carries a private member.
func (g *Gateway) handleCDSClientJWKS(w http.ResponseWriter, r *http.Request) {
	body := g.cfg.CDSClientJWKS
	h := w.Header()
	h.Set("Content-Type", "application/jwk-set+json")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
