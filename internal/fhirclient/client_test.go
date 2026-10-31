package fhirclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
	"github.com/SmartHealthNetwork/shn-gateway/internal/jsonmember"
)

func TestSearch_ParsesBundleAndBuildsURL(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAccept = r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/fhir+json")
		_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1"}}]}`))
	}))
	defer srv.Close()

	c := fhirclient.New(srv.URL, nil)
	b, err := c.Search(context.Background(), "Patient", url.Values{"identifier": {"urn:shn:member|MBR-COVERED"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/Patient" {
		t.Errorf("path = %q, want /Patient", gotPath)
	}
	if gotQuery != "identifier=urn%3Ashn%3Amember%7CMBR-COVERED" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAccept != "application/fhir+json" {
		t.Errorf("Accept = %q, want application/fhir+json", gotAccept)
	}
	if b == nil || len(b.Entry) != 1 {
		t.Fatalf("want 1 entry, got %+v", b)
	}
}

func TestSearch_MalformedJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json at all")
	}))
	defer srv.Close()
	if _, err := fhirclient.New(srv.URL, nil).Search(context.Background(), "Patient", nil); err == nil {
		t.Fatal("want error for malformed JSON body, got nil")
	}
}

func TestSearch_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fhirclient.New(srv.URL, nil).Search(context.Background(), "Patient", nil); err == nil {
		t.Fatal("want error on 500, got nil")
	}
}

// A body that repeats a member name, exactly or under case folding, can be
// read two ways, so neither Search nor Read reads it: it is an invalid
// response that says so by structure (errors.Is relay.ErrDuplicateKey),
// wherever the repeat is. A body with no repeat is read as before.
func TestSearchAndRead_RepeatedMemberIsRefused(t *testing.T) {
	const patient = `{"resourceType":"Patient","id":"p1","birthDate":"1970-01-01"}`
	for name, row := range map[string]struct {
		search, read string
		repeated     bool
	}{
		"clean": {`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`, patient, false},
		"a repeat at the Bundle level": {
			`{"resourceType":"Bundle","type":"searchset","type":"searchset","entry":[{"resource":` + patient + `}]}`,
			`{"resourceType":"Patient","resourceType":"Patient","id":"p1"}`, true},
		"a repeat inside the resource": {
			`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1","birthDate":"1970-01-01","birthDate":"1980-01-01"}}]}`,
			`{"resourceType":"Patient","id":"p1","birthDate":"1970-01-01","birthDate":"1980-01-01"}`, true},
		"a repeat under case folding": {
			`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"p1","birthDate":"1970-01-01","BirthDate":"1980-01-01"}}]}`,
			`{"resourceType":"Patient","id":"p1","birthDate":"1970-01-01","BirthDate":"1980-01-01"}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Patient/p1" {
				_, _ = w.Write([]byte(row.read))
				return
			}
			_, _ = w.Write([]byte(row.search))
		}))
		c := fhirclient.New(srv.URL, srv.Client())
		_, searchErr := c.Search(context.Background(), "Patient", url.Values{"identifier": {"x"}})
		_, found, readErr := c.Read(context.Background(), "Patient", "p1")
		srv.Close()
		for how, err := range map[string]error{"search": searchErr, "read": readErr} {
			var invalid *fhirclient.InvalidResponseError
			switch {
			case !row.repeated && err != nil:
				t.Errorf("%s, %s: %v, want it read", name, how, err)
			case row.repeated && (!errors.As(err, &invalid) || !errors.Is(err, relay.ErrDuplicateKey)):
				t.Errorf("%s, %s: %v, want an invalid response that repeats a member", name, how, err)
			}
		}
		if row.repeated && found {
			t.Errorf("%s: a read repeating a member was found", name)
		}
	}
}

// A repeat after something the strict reader stops at (an escape that is not
// a whole character, a byte that is not UTF-8, nesting past its depth bound)
// is still found, wherever the decoder that reads the body would read it: the
// repeat is refused, and the same body without it is read as before.
func TestSearchAndRead_RepeatPastTheStrictReadersLimitsIsRefused(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 70) + `1` + strings.Repeat(`}`, 70)
	for name, prefix := range map[string]string{
		"a lone surrogate escape":  `"name":[{"family":"\ud800"}],`,
		"a byte that is not UTF-8": "\"name\":[{\"family\":\"\xff\"}],",
		"nesting past the bound":   `"deep":` + deep + `,`,
	} {
		for _, repeated := range []bool{false, true} {
			patient := `{"resourceType":"Patient",` + prefix + `"id":"p1","birthDate":"1970-01-01"`
			if repeated {
				patient += `,"birthDate":"1980-01-01"`
			}
			patient += `}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Patient/p1" {
					_, _ = w.Write([]byte(patient))
					return
				}
				_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient + `}]}`))
			}))
			c := fhirclient.New(srv.URL, srv.Client())
			_, searchErr := c.Search(context.Background(), "Patient", url.Values{"identifier": {"x"}})
			_, _, readErr := c.Read(context.Background(), "Patient", "p1")
			srv.Close()
			for how, err := range map[string]error{"search": searchErr, "read": readErr} {
				if got := errors.Is(err, relay.ErrDuplicateKey); got != repeated || (!repeated && err != nil) {
					t.Errorf("%s, repeated=%v, %s: %v", name, repeated, how, err)
				}
			}
		}
	}
}

// A body nested past encoding/json's depth bound (jsonmember.MaxDepth) is read
// by no decoder: where its only repeat is past the bound, it is the invalid
// response any unreadable body is, not a repeat. One nested exactly to the
// bound is read as before.
func TestSearchAndRead_DepthBound(t *testing.T) {
	nested := func(depth int, inner string) string {
		return strings.Repeat(`{"a":`, depth-1) + inner + strings.Repeat(`}`, depth-1)
	}
	// patient is a Patient whose member x nests the body to depth, counting
	// from the Patient itself (base levels above it: 0 for a read, 3 for the
	// searchset, its entry array and the entry).
	patient := func(base, depth int, inner string) string {
		return `{"resourceType":"Patient","id":"p1","x":` + nested(depth-base-1, inner) + `}`
	}
	for name, row := range map[string]struct {
		inner    string
		depth    int
		readable bool
	}{
		"nested to the bound":              {`{"k":1}`, jsonmember.MaxDepth, true},
		"nested past the bound, repeating": {`{"k":1,"k":2}`, jsonmember.MaxDepth + 1, false},
		"nested past the bound, no repeat": {`{"k":1}`, jsonmember.MaxDepth + 1, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Patient/p1" {
				_, _ = w.Write([]byte(patient(0, row.depth, row.inner)))
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + patient(3, row.depth, row.inner) + `}]}`))
		}))
		c := fhirclient.New(srv.URL, srv.Client())
		_, searchErr := c.Search(context.Background(), "Patient", url.Values{"identifier": {"x"}})
		_, _, readErr := c.Read(context.Background(), "Patient", "p1")
		srv.Close()
		for how, err := range map[string]error{"search": searchErr, "read": readErr} {
			var invalid *fhirclient.InvalidResponseError
			switch {
			case row.readable && err != nil:
				t.Errorf("%s, %s: %v, want it read", name, how, err)
			case !row.readable && (!errors.As(err, &invalid) || errors.Is(err, relay.ErrDuplicateKey)):
				t.Errorf("%s, %s: %v, want the invalid response, not a repeat", name, how, err)
			}
		}
	}
}
