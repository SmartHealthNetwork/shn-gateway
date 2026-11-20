package fhirsor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"

	"github.com/SmartHealthNetwork/shn-gateway/engine"
)

// A member id that names no one is never searched. An identifier search
// with an empty value ("<system>|") matches any member of that system, so a
// system of record holding one member would answer with that member.
// It is an error the caller refuses on, never "not found": not found can
// send a leg on as an unknown member. Every member-keyed read goes through
// the same patient lookup; the beneficiary lookup of CoverageInforce is one.
func TestAMemberNamingNoOneIsNeverSearched(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		w.Write([]byte(searchResource(failurePatient)))
	}))
	defer srv.Close()
	s := NewFromURL(srv.URL, srv.Client())
	ctx := context.Background()
	for name, member := range map[string]string{
		"empty":                "",
		"blank":                " \t ",
		"the type with no id":  "Patient/",
		"the type, a blank id": "Patient/  ",
	} {
		t.Run(name, func(t *testing.T) {
			searches.Store(0)
			pci, demo, found, err := s.ResolvePatientContext(ctx, member)
			if pci != "" || demo != (engine.Demo{}) || found || err == nil {
				t.Fatalf("ResolvePatientContext(%q) = %q, %+v, found %v, err %v; want a refusal", member, pci, demo, found, err)
			}
			var read *engine.SoRReadError
			if !errors.As(err, &read) {
				t.Fatalf("ResolvePatientContext(%q) error %v, want a system-of-record read error the caller refuses on", member, err)
			}
			// The identifier's own guard, past the search's: the member
			// derives no identifier, refused with the same error.
			pci, demo, found, err = patientContext(member, engine.Demo{BirthDate: "1970-01-01", FamilyName: "Doe"})
			if pci != "" || demo != (engine.Demo{}) || found || !errors.As(err, &read) {
				t.Fatalf("patientContext(%q) = %q, %+v, found %v, err %v; want the same refusal", member, pci, demo, found, err)
			}
			inforce, reason, err := s.CoverageInforceContext(ctx, member)
			if inforce || reason != "" || err == nil {
				t.Fatalf("CoverageInforceContext(%q) = %v %q %v; want a refusal", member, inforce, reason, err)
			}
			if _, found, err := s.PatientFHIRRefContext(ctx, member); found || err == nil {
				t.Fatalf("PatientFHIRRefContext(%q) = found %v, err %v; want a refusal", member, found, err)
			}
			if n := searches.Load(); n != 0 {
				t.Fatalf("%d searches issued for a member naming no one", n)
			}
		})
	}
	// Control: a member id is searched as before, and derives the identifier
	// ResolvePCI derives.
	searches.Store(0)
	pci, demo, found, err := s.ResolvePatientContext(ctx, "MBR-OP")
	if !found || err != nil || searches.Load() != 1 {
		t.Fatalf("control: found %v, err %v, searches %d; want the member found by one search", found, err, searches.Load())
	}
	if want := shnsdk.ResolvePCI("MBR-OP", demo.BirthDate, demo.FamilyName); pci == "" || pci != want {
		t.Fatalf("control: identifier %q, want %q", pci, want)
	}
}
