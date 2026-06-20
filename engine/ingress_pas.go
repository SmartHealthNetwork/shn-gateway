// ingress_pas.go — PAS ($submit) ingress: terminate the inbound Claim Bundle, cross-field
// subject-bind it (the origination mirror of bindBundleSubject), drive the EXISTING
// pas-claim substrate leg, and relay the ClaimResponse Bundle.
package engine

import (
	"net/http"
	"strings"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// ingressPASSubjectPCI proves the Claim patient, the QR subject, the SR subject, and (when
// present) the DiagnosticReport subject all resolve to ONE pci. Mirrors bindBundleSubject
// on the origination side. Returns (pci, 0, "") or ("", status, msg).
//
// The ingress authenticates the inbound CLIENT (a provider org) via SMART Backend Services
// (ingressAuthOK). It does NOT bind the request to a patient SUBJECT, and that is
// intentional, not a gap: provider-side origination authority is org-level TPO
// (Treatment/Payment/Operations) by design. A client_credentials bearer carries no
// patient subject; patient-subject binding belongs on the RESTful Patient Access edge
// (the credentialing three-layer posture), not here. The correct invariant for THIS
// edge is the in-body cross-field subject-consistency below (every patient reference
// resolves to one pci), re-checked payer-side by bindBundleSubject (defense in depth).
// Per-client patient-scoping would only become load-bearing under multi-tenant (multiple
// orgs behind one shared SoR), which is out of scope.
func (g *Gateway) ingressPASSubjectPCI(bundleJSON []byte) (string, int, string) {
	cb, err := shnsdk.ParseClaimBundle(bundleJSON)
	if err != nil {
		return "", http.StatusBadRequest, "parse claim bundle failed"
	}
	member := strings.TrimPrefix(cb.ClaimPatient, "Patient/")
	pci, _, found := g.cfg.SoR.ResolvePatient(member)
	if !found {
		return "", http.StatusBadRequest, "unknown member"
	}
	if cb.QRSubject == "" {
		return "", http.StatusForbidden, "PAS bundle QuestionnaireResponse missing subject"
	}
	if strings.TrimPrefix(cb.SRSubject, "Patient/") != member {
		return "", http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	if strings.TrimPrefix(cb.QRSubject, "Patient/") != member {
		return "", http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	if cb.HasDiagnosticReport && strings.TrimPrefix(cb.DiagnosticReportSubject, "Patient/") != member {
		return "", http.StatusForbidden, "inconsistent patient in PAS bundle"
	}
	return pci, 0, ""
}
