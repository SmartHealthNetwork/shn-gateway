//go:build !shn_coreonly

package engine

import (
	"net/http"
	"testing"
)

// Accessors for the end-to-end rows in package engine_test
// (workstream_fqs_repeated_e2e_test.go), which wire the bundled FHIR connector
// as the system of record: this package's own test harnesses, with the
// system of record the caller gives.

// RepeatedMemberE2EMember is the member the harnesses' requests name, with its
// birth date and family name as the test system of record holds them.
func RepeatedMemberE2EMember() (member, birthDate, family string) {
	_, demo, _ := newCensusSoR().ResolvePatient(dtrFrameMember)
	return dtrFrameMember, demo.BirthDate, demo.FamilyName
}

// FederatedQueryWithSoRForTest delivers a consented federated query for that
// member to a facility gateway at level whose system of record is sor, and
// returns the requester's answer and the exchange record.
func FederatedQueryWithSoRForTest(t *testing.T, level ConformanceEnforcement, sor SystemOfRecord) (int, []byte, ExchangeRecord) {
	t.Helper()
	p, got := newRecordingLevelPayer(t, level)
	p.answering(t, "federated-query")
	p.g.cfg.ConsentURL = "http://consent.test"
	p.g.cfg.Client = &http.Client{Transport: consentPermits{inner: p.g.cfg.Client.Transport}}
	p.g.cfg.SoR = sor
	ans := sendConsented(t, p, "federated-query", cdexTaskFor(t, dtrFrameMember), p.pci, "Consent/c-1")
	return ans.status, ans.body, got.only(t)
}
