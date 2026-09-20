package fhirsor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// The subject a holder derives from its own record and the one a non-holder derives from
// the same Patient carried in a request must be one identifier: this connector's read and
// engine.PatientDemographics read the same two fields the same way.
func TestResolvePatient_DerivationMatchesCarriedPatient(t *testing.T) {
	for name, patient := range map[string]string{
		"plain":      `{"resourceType":"Patient","id":"p1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"m1"}],"name":[{"family":"Nakamura","given":["Iris"]}],"birthDate":"1962-03-11"}`,
		"mixed case": `{"resourceType":"Patient","id":"p1","name":[{"family":"O'Brien-Smith"}],"birthDate":"1962-03-11"}`,
		"two names":  `{"resourceType":"Patient","id":"p1","name":[{"use":"official","family":"First"},{"family":"Second"}],"birthDate":"1962-03-11"}`,
		"year only":  `{"resourceType":"Patient","id":"p1","name":[{"family":"Nakamura"}],"birthDate":"1962"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(searchResource(patient))) }))
			defer srv.Close()
			pci, demo, found, err := NewFromURL(srv.URL, srv.Client()).ResolvePatientContext(context.Background(), "m1")
			if err != nil || !found {
				t.Fatalf("found=%v err=%v", found, err)
			}
			carried, ok := engine.PatientDemographics([]byte(patient))
			if !ok || carried != demo {
				t.Fatalf("carried demographics %+v (ok=%v), record read %+v", carried, ok, demo)
			}
			if want := shnsdk.ResolvePCI("m1", carried.BirthDate, carried.FamilyName); pci != want {
				t.Fatalf("record pci %q, carried-derived %q", pci, want)
			}
		})
	}
	// A Patient this connector refuses as invalid carries no usable demographics either.
	for name, patient := range map[string]string{
		"no demographics": `{"resourceType":"Patient","id":"p1"}`,
		"empty family":    `{"resourceType":"Patient","id":"p1","name":[{"family":""}],"birthDate":"1962-03-11"}`,
		"no birthDate":    `{"resourceType":"Patient","id":"p1","name":[{"family":"Nakamura"}]}`,
	} {
		t.Run("invalid/"+name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(searchResource(patient))) }))
			defer srv.Close()
			if _, _, _, err := NewFromURL(srv.URL, srv.Client()).ResolvePatientContext(context.Background(), "m1"); err == nil {
				t.Fatal("record read accepted a Patient without demographics")
			}
			if carried, ok := engine.PatientDemographics([]byte(patient)); ok {
				t.Fatalf("carried read accepted %+v", carried)
			}
		})
	}
}
