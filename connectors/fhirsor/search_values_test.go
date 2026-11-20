package fhirsor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Every search the connector builds from a value it was given or read is sent
// as that value: a member id is escaped as a token value, so its own comma,
// bar, dollar sign or backslash never joins or splits a token, and a member is
// matched as written.
func TestAMemberIdIsSearchedAsWritten(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Query().Get("identifier"))
		mu.Unlock()
		w.Write([]byte(emptySearch))
	}))
	defer srv.Close()
	s := NewFromURL(srv.URL, srv.Client())
	for member, want := range map[string]string{
		"MBR-OP":            "urn:shn:member|MBR-OP",
		"x,urn:shn:member|": `urn:shn:member|x\,urn:shn:member\|`,
		"a$b":               `urn:shn:member|a\$b`,
		`a\b`:               `urn:shn:member|a\\b`,
		`a\,b`:              `urn:shn:member|a\\\,b`,
		" MBR-OP":           "urn:shn:member| MBR-OP",
		"Patient/MBR-OP":    "urn:shn:member|Patient/MBR-OP",
	} {
		mu.Lock()
		got = nil
		mu.Unlock()
		if _, _, found, err := s.ResolvePatientContext(context.Background(), member); found || err != nil {
			t.Fatalf("member %q: found %v, err %v", member, found, err)
		}
		mu.Lock()
		if len(got) != 1 || got[0] != want {
			t.Errorf("member %q: searched %q, want %q", member, got, want)
		}
		mu.Unlock()
	}
}

// The Patient id the system of record answers with scopes every later search
// for the member's records: one that is not a FHIR id is an answer out of
// shape, never sent on as another search's value.
func TestAPatientIdThatIsNoFHIRIdIsRefused(t *testing.T) {
	// "." and ".." are a FHIR id's characters but a dot segment in a path.
	for _, id := range []string{"p,q", "p|q", "p q", "p/q", "p?q", "p&x=1", "", ".", ".."} {
		body := searchResource(`{"resourceType":"Patient","id":"` + id + `","identifier":[{"system":"urn:shn:member","value":"MBR-OP"}],"name":[{"family":"Op"}],"birthDate":"1970-01-01"}`)
		var searches int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			searches++
			w.Write([]byte(body))
		}))
		s := NewFromURL(srv.URL, srv.Client())
		found, _, err := s.CoverageInforceContext(context.Background(), "MBR-OP")
		srv.Close()
		if found || err == nil || searches != 1 {
			t.Errorf("Patient id %q: coverage %v, err %v after %d searches; want refused after the Patient search", id, found, err, searches)
		}
	}
}

// A reference is read only as Type/id: one of another shape is sent nowhere.
func TestAReferenceOfAnotherShapeIsNotRead(t *testing.T) {
	var reads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Write([]byte(`{"resourceType":"Organization","id":"o1"}`))
	}))
	defer srv.Close()
	s := NewFromURL(srv.URL, srv.Client())
	for _, ref := range []string{"Organization/o1?x=1", "Organization/o1/_history/", "Organization/o1/_history/1?x", "Organization/o1/other/1", "Organization/o 1", "organization/o1",
		"Organization/", "/o1", "Organization/o1#x", "Patient?identifier=urn:shn:member|", "Organization/o1,o2",
		// A dot segment, as the id or as the version id: a server or proxy
		// cleaning the path would read another resource, or the history.
		"Organization/.", "Organization/..", "Organization/o1/_history/.", "Organization/o1/_history/..", "Organization/../_history/1"} {
		reads = 0
		if body, found, err := s.ResolveByReferenceContext(context.Background(), ref); found || err != nil || body != nil || reads != 0 {
			t.Errorf("reference %q: found %v, err %v, %d reads; want nothing read", ref, found, err, reads)
		}
	}
	for _, ref := range []string{"Organization/o1", "Organization/o1/_history/1", "Organization/o.1", "Organization/o1/_history/1.0"} {
		reads = 0
		if _, found, err := s.ResolveByReferenceContext(context.Background(), ref); !found || err != nil || reads != 1 {
			t.Fatalf("control %q: found %v, err %v, %d reads", ref, found, err, reads)
		}
	}
}
