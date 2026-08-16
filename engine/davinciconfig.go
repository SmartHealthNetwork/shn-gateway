package engine

import (
	"encoding/json"
	"net/http"
	"strings"
)

// shnParticipantIDSystem namespaces the REQUIRED well-known identifier
// (HRexWellknownDefinition.identifier, 1..1). SHN has no FHIR NamingSystem
// for participant ids yet, so a URN is used and documented in
// PARTICIPANT_PROTOCOL — additive to replace with a resolvable
// canonical later.
const shnParticipantIDSystem = "urn:shn:participant-id"

// hrexCodeForContract maps an SHN contract name to its HRex 1.2.0 endpoint
// code (CodeSystem hrex-endpoint-name; version-specific form is
// "<code>#<major.minor>"). pa.pdex is deliberately absent HERE — its
// patient-access endpoint belongs to the payer edge, which has no
// config-pinned self base URL yet (a recorded deferral). The checks prober
// holds the reverse map; extend BOTH or neither.
var hrexCodeForContract = map[string]string{
	"pa.crd": "davinci_crd_hook_endpoint",
	"pa.dtr": "davinci_dtr_qpackage_endpoint",
	"pa.pas": "davinci_pas_submission_endpoint",
}

// buildDavinciConfiguration renders the HRex .well-known/davinci-configuration
// document for the ingress edge: every natively-declared contract line that
// has an HRex endpoint code, pointing at the ingress base (HRex endpoint
// values are operation BASE urls — callers append /Claim/$submit etc.).
// Deterministic: json.Marshal of a map sorts keys.
func buildDavinciConfiguration(baseURL, holderID string, declared []string) ([]byte, error) {
	endpoints := map[string]string{}
	for _, token := range declared {
		contract, line, ok := strings.Cut(token, "@")
		if !ok {
			continue
		}
		code, ok := hrexCodeForContract[contract]
		if !ok {
			continue
		}
		endpoints[code+"#"+line] = baseURL
	}
	doc := map[string]any{
		"identifier": map[string]string{"system": shnParticipantIDSystem, "value": holderID},
		"endpoints":  endpoints,
	}
	return json.Marshal(doc)
}

// handleDavinciConfiguration serves the well-known document (plain TLS, no
// auth — HRex requires it be readable without mTLS; it names public base
// URLs only).
func (g *Gateway) handleDavinciConfiguration(w http.ResponseWriter, _ *http.Request) {
	b, err := buildDavinciConfiguration(g.cfg.IngressBaseURL, g.cfg.HolderID, g.declaredContractVersions())
	if err != nil {
		http.Error(w, "davinci-configuration build failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
