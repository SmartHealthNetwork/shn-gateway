package cdexroute

import (
	"errors"
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

// A per-provider base is built only under a canonical ingress base: one with
// a "." or ".." path segment, spelled or percent-escaped, is refused
// (ErrIngressBaseDotSegment), and so is one with a character that is not
// ASCII, its host included (ErrIngressBaseNotASCII), a space or a control
// character (ErrIngressBaseSpaceOrControl), or another character outside
// RFC 3986's set, a backslash among them (ErrIngressBaseNotURLCharacter),
// which relay.URLCharacter decides for both; the core relay refuses
// the same in the Location a created Task is answered with (E-16). A
// canonical base, with a port, an IPv6 host, no path or a path prefix, a dot
// inside a segment, or a segment of three dots, is built under.
func TestProviderBaseCanonicalIngressBase(t *testing.T) {
	dotDot := strings.Repeat(".", 2)
	for base, want := range map[string]error{
		"https://gw.example/./fhir":             ErrIngressBaseDotSegment,
		"https://gw.example/fhir/.":             ErrIngressBaseDotSegment,
		"https://gw.example/fhir/./":            ErrIngressBaseDotSegment,
		"https://gw.example/" + dotDot:          ErrIngressBaseDotSegment,
		"https://gw.example/a/" + dotDot + "/b": ErrIngressBaseDotSegment,
		"https://gw.example/%2E":                ErrIngressBaseDotSegment,
		"https://gw.example/a/%2e%2E/b":         ErrIngressBaseDotSegment,
		"https://g\u00fc.example":               ErrIngressBaseNotASCII,
		"https://gw.example/f\u00fchr":          ErrIngressBaseNotASCII,
		"https://gw.example/fhir\u00a0":         ErrIngressBaseNotASCII,
		"https://gw.example:8443/\u65e5\u672c":  ErrIngressBaseNotASCII,
		"https://gw.example/f hir":              ErrIngressBaseSpaceOrControl,
		"https://gw.example/fhir ":              ErrIngressBaseSpaceOrControl,
		" https://gw.example":                   ErrIngressBaseSpaceOrControl,
		"https://gw.example/f\thir":             ErrIngressBaseSpaceOrControl,
		"https://gw.example/fhir\x7f":           ErrIngressBaseSpaceOrControl,
		"https://gw.example/fhir\x00":           ErrIngressBaseSpaceOrControl,
		"https://gw.example/fhir\n":             ErrIngressBaseSpaceOrControl,
		"https://gw.example/a\\b":               ErrIngressBaseNotURLCharacter,
		"https://gw.example/a\"b":               ErrIngressBaseNotURLCharacter,
		"https://gw.example/a<b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a>b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a{b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a}b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a|b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a^b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a`b":                ErrIngressBaseNotURLCharacter,
		"https://gw.example/a\\..\\b":           ErrIngressBaseNotURLCharacter,
	} {
		if got, err := ProviderBase(base, "provider-1"); !errors.Is(err, want) || got != "" {
			t.Errorf("ProviderBase(%q) = %q, %v; want %v", base, got, err, want)
		}
	}
	for base, want := range map[string]string{
		"https://gw.example":             "https://gw.example/cdex/provider/provider-1",
		"https://gw.example:8443":        "https://gw.example:8443/cdex/provider/provider-1",
		"https://[::1]:8443":             "https://[::1]:8443/cdex/provider/provider-1",
		"http://[2001:db8::1]/fhir":      "http://[2001:db8::1]/fhir/cdex/provider/provider-1",
		"https://gw.example/fhir/r4":     "https://gw.example/fhir/r4/cdex/provider/provider-1",
		"https://gw.example/fhir.v2/...": "https://gw.example/fhir.v2/.../cdex/provider/provider-1",
		"https://xn--g-eha.example/fhir": "https://xn--g-eha.example/fhir/cdex/provider/provider-1",
		"https://gw.example/caf%C3%A9":   "https://gw.example/caf%C3%A9/cdex/provider/provider-1",
	} {
		if got, err := ProviderBase(base, "provider-1"); err != nil || got != want {
			t.Errorf("ProviderBase(%q) = %q, %v; want %q", base, got, err, want)
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
