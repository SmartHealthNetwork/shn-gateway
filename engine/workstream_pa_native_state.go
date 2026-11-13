//go:build !shn_coreonly

package engine

import "net/http"

// paNativeState is the PA workstream's own forward state on one native
// responder (workstream.newNativeState): where its operations go, which CDS
// service answers a CRD leg, and the pend ledger its PAS legs keep.
type paNativeState struct {
	// cdsBaseURL is the CDS Hooks base (/cds-services/{id}); the responder's
	// base unless WithCDSBaseURL names another.
	cdsBaseURL string
	// dtrBaseURL / pasBaseURL are the per-operation FHIR bases for a partner
	// that serves DTR (/Questionnaire/$questionnaire-package, $next-question)
	// and PAS (/Claim/$submit) from different bases. Empty ⇒ that operation uses
	// the responder's base (dtrBase/pasBase apply the fallback) — the
	// byte-identical fallback every deployment that sets neither relies on.
	// Published endpoint evidence (resolvedURL) still takes precedence over
	// either.
	dtrBaseURL string
	pasBaseURL string
	// eligibilityURL is the payer's own coverage-eligibility endpoint, when it
	// declares one (WithEligibilityURL). Empty: the payer's gateway answers
	// eligibility from the payer's records itself.
	eligibilityURL string
	// crdServiceID and crdDispatchServiceID optionally name the partner's CDS service
	// for the order-select and order-dispatch legs; empty selects the listed service
	// whose hook is the request's hook (workstream_pa_cds_services.go). Either way the service must be
	// listed for the request's hook.
	crdServiceID         string
	crdDispatchServiceID string
	// cds caches the partner's CDS service listing.
	cds cdsServiceListing
	// cdsClient sends the CDS calls (the service listing and each CRD hook)
	// when they authenticate other than the responder's other operations: a
	// CDS Hooks client JWT (WithCDSClient). nil: they go with the
	// responder's own client, as DTR and PAS do.
	cdsClient *http.Client
	// store is the gateway-owned Store the PAS legs keep the pend ledger in
	// (WithPASStore); nil ⇒ the PAS legs keep none.
	store Store
}

// newNativeState: the PA workstream's state on a new native responder, its
// CDS Hooks base the responder's own until an option names another.
func (paWorkstream) newNativeState(n *nativeResponder) nativeState {
	return &paNativeState{cdsBaseURL: n.baseURL}
}

// paNative is the PA workstream's state on n. NewNativeResponder makes it for
// every responder; one assembled without it (a test's struct literal) has none
// and must not reach a PA forward or option.
func (n *nativeResponder) paNative() *paNativeState {
	st, _ := n.states[workstreamPA].(*paNativeState)
	return st
}

// WithCRDService names the partner's CDS service for the crd-order-select leg
// (PAYER_DAVINCI_CRD_SERVICE_ID). Empty (the default) selects the listed
// service for the order-select hook (FR-G26). Its sibling for the
// order-dispatch leg is WithCRDDispatchService.
func WithCRDService(serviceID string) NativeOption {
	return func(n *nativeResponder) { n.paNative().crdServiceID = serviceID }
}

// WithPASStore is the gateway-owned Store the PAS legs keep the pend ledger
// in. Without it (or with nil) the responder forwards the PAS legs and keeps
// no ledger: a read-only responder.
func WithPASStore(s Store) NativeOption {
	return func(n *nativeResponder) { n.paNative().store = s }
}
