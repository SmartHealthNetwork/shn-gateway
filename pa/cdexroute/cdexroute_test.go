package cdexroute

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReturnBase(t *testing.T) {
	for _, tc := range []struct {
		base, payer, want string
	}{
		{"https://gw.example", "payer-1", "https://gw.example/cdex/payer/payer-1"},
		{"https://gw.example/", "payer-1", "https://gw.example/cdex/payer/payer-1"},
		{"https://gw.example/fhir", "payer-1", "https://gw.example/fhir/cdex/payer/payer-1"},
		{"http://provider-gw:8080/", "p", "http://provider-gw:8080/cdex/payer/p"},
		// A separator in an id stays inside its one segment.
		{"https://gw.example", "org/7", "https://gw.example/cdex/payer/org%2F7"},
	} {
		got, err := ReturnBase(tc.base, tc.payer)
		if err != nil || got != tc.want {
			t.Errorf("ReturnBase(%q, %q) = %q, %v; want %q", tc.base, tc.payer, got, err, tc.want)
		}
	}
}

func TestReturnBaseRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, base, payer string
	}{
		{"empty base", "", "payer-1"},
		{"relative base", "/fhir", "payer-1"},
		{"no host", "https://", "payer-1"},
		{"other scheme", "ftp://gw.example", "payer-1"},
		{"query", "https://gw.example?x=1", "payer-1"},
		{"empty query", "https://gw.example?", "payer-1"},
		{"fragment", "https://gw.example#f", "payer-1"},
		{"empty fragment", "https://gw.example/x#", "payer-1"},
		{"empty fragment, no path", "https://gw.example#", "payer-1"},
		{"user information", "https://u:p@gw.example", "payer-1"},
		{"unparseable", "https://gw.example/%zz", "payer-1"},
		{"empty payer", "https://gw.example", ""},
		{"dot payer", "https://gw.example", "."},
		{"dot-dot payer", "https://gw.example", ".."},
		{"space in payer", "https://gw.example", "payer 1"},
		{"control in payer", "https://gw.example", "payer\n1"},
	} {
		if got, err := ReturnBase(tc.base, tc.payer); err == nil {
			t.Errorf("%s: ReturnBase(%q, %q) = %q, want an error", tc.name, tc.base, tc.payer, got)
		}
	}
}

// The address ReturnBase builds, with and without the operation segment,
// reaches a route on Prefix with the payer id read back exactly.
func TestReturnBaseRoundTripsThroughAMux(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	h := func(w http.ResponseWriter, r *http.Request) { got = append(got, r.PathValue("payer")) }
	mux.HandleFunc("POST "+Prefix+"{payer}", h)
	mux.HandleFunc("POST "+Prefix+"{payer}/"+Operation, h)
	for _, payer := range []string{"payer-1", "org/7", "ü"} {
		base, err := ReturnBase("https://gw.example", payer)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{base, base + "/" + Operation} {
			u, err := url.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			got = nil
			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, u.RequestURI(), strings.NewReader("{}")))
			if len(got) != 1 || got[0] != payer {
				t.Errorf("POST %s: routed payer %q, want [%q]", target, got, payer)
			}
		}
	}
}

func TestProviderBase(t *testing.T) {
	for _, tc := range []struct {
		base, provider, want string
	}{
		{"https://payer-gw.example", "provider-1", "https://payer-gw.example/cdex/provider/provider-1"},
		{"https://payer-gw.example/", "provider-1", "https://payer-gw.example/cdex/provider/provider-1"},
		{"https://payer-gw.example/fhir", "provider-1", "https://payer-gw.example/fhir/cdex/provider/provider-1"},
		{"https://payer-gw.example", "org/7", "https://payer-gw.example/cdex/provider/org%2F7"},
	} {
		got, err := ProviderBase(tc.base, tc.provider)
		if err != nil || got != tc.want {
			t.Errorf("ProviderBase(%q, %q) = %q, %v; want %q", tc.base, tc.provider, got, err, tc.want)
		}
	}
	// The same rules as a return path, base and id alike.
	for _, tc := range []struct{ name, base, provider string }{
		{"relative base", "/fhir", "provider-1"},
		{"query", "https://gw.example?", "provider-1"},
		{"fragment", "https://gw.example#", "provider-1"},
		{"user information", "https://u:p@gw.example", "provider-1"},
		{"empty provider", "https://gw.example", ""},
		{"dot-dot provider", "https://gw.example", ".."},
		{"space in provider", "https://gw.example", "provider 1"},
	} {
		if got, err := ProviderBase(tc.base, tc.provider); err == nil || !strings.Contains(err.Error(), "cdexroute") {
			t.Errorf("%s: ProviderBase(%q, %q) = %q, %v; want an error", tc.name, tc.base, tc.provider, got, err)
		}
	}
}

// The address ProviderBase builds, with the Task endpoint appended, reaches a
// route on ProviderPrefix with the provider id read back exactly.
func TestProviderBaseRoundTripsThroughAMux(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ProviderPrefix+"{provider}/Task", func(w http.ResponseWriter, r *http.Request) { got = append(got, r.PathValue("provider")) })
	for _, provider := range []string{"provider-1", "org/7", "ü"} {
		base, err := ProviderBase("https://gw.example", provider)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(base + "/Task")
		if err != nil {
			t.Fatal(err)
		}
		got = nil
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, u.RequestURI(), strings.NewReader("{}")))
		if len(got) != 1 || got[0] != provider {
			t.Errorf("POST %s/Task: routed provider %q, want [%q]", base, got, provider)
		}
	}
}
