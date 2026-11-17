package pasroute

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute"
)

func TestBase(t *testing.T) {
	for _, tc := range []struct {
		base, payer, want string
	}{
		{"https://provider-gw.example", "payer-1", "https://provider-gw.example/pas/payer/payer-1"},
		{"https://provider-gw.example/", "payer-1", "https://provider-gw.example/pas/payer/payer-1"},
		{"https://provider-gw.example/fhir", "payer-1", "https://provider-gw.example/fhir/pas/payer/payer-1"},
		{"http://provider-gw:8080/", "p", "http://provider-gw:8080/pas/payer/p"},
		// A separator in an id stays inside its one segment.
		{"https://provider-gw.example", "org/7", "https://provider-gw.example/pas/payer/org%2F7"},
	} {
		got, err := Base(tc.base, tc.payer)
		if err != nil || got != tc.want {
			t.Errorf("Base(%q, %q) = %q, %v; want %q", tc.base, tc.payer, got, err, tc.want)
		}
	}
}

// A base and an id are held to cdexroute.ProviderBase's rules, and refused
// with its errors: a base that is not an absolute http or https URL, carries
// user information, a query or a fragment, or is not canonical; an id that is
// empty, a dot segment, or carries a space or a control character.
func TestBaseRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, base, payer string
		want              error
	}{
		{"empty base", "", "payer-1", nil},
		{"relative base", "/fhir", "payer-1", nil},
		{"no host", "https://", "payer-1", nil},
		{"other scheme", "ftp://gw.example", "payer-1", nil},
		{"query", "https://gw.example?x=1", "payer-1", nil},
		{"empty query", "https://gw.example?", "payer-1", nil},
		{"fragment", "https://gw.example#f", "payer-1", nil},
		{"user information", "https://u:p@gw.example", "payer-1", nil},
		{"empty payer", "https://gw.example", "", nil},
		{"dot payer", "https://gw.example", ".", nil},
		{"dot-dot payer", "https://gw.example", "..", nil},
		{"space in payer", "https://gw.example", "payer 1", nil},
		{"control in payer", "https://gw.example", "payer\n1", nil},
		{"dot segment", "https://gw.example/./fhir", "payer-1", cdexroute.ErrIngressBaseDotSegment},
		{"escaped dot segment", "https://gw.example/a/%2e%2E/b", "payer-1", cdexroute.ErrIngressBaseDotSegment},
		{"not ASCII", "https://gü.example", "payer-1", cdexroute.ErrIngressBaseNotASCII},
		{"space", "https://gw.example/f hir", "payer-1", cdexroute.ErrIngressBaseSpaceOrControl},
		{"backslash", "https://gw.example/a\\b", "payer-1", cdexroute.ErrIngressBaseNotURLCharacter},
	} {
		got, err := Base(tc.base, tc.payer)
		if err == nil || got != "" || !strings.HasPrefix(err.Error(), "pasroute: ") {
			t.Errorf("%s: Base(%q, %q) = %q, %v; want a pasroute error", tc.name, tc.base, tc.payer, got, err)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: Base(%q, %q) = %v; want %v", tc.name, tc.base, tc.payer, err, tc.want)
		}
		// The same base and id are refused by the CDex per-provider base, so
		// the two bases a gateway's ingress serves accept the same bases.
		if _, cerr := cdexroute.ProviderBase(tc.base, tc.payer); cerr == nil {
			t.Errorf("%s: cdexroute.ProviderBase accepts what Base refuses", tc.name)
		}
	}
}

// The address Base builds, with an operation's path appended, reaches a
// route on Prefix with the payer id read back exactly.
func TestBaseRoundTripsThroughAMux(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix+"{payer}/Claim/$submit", func(w http.ResponseWriter, r *http.Request) { got = append(got, r.PathValue("payer")) })
	for _, payer := range []string{"payer-1", "org/7", "ü"} {
		base, err := Base("https://gw.example", payer)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(base + "/Claim/$submit")
		if err != nil {
			t.Fatal(err)
		}
		got = nil
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, u.RequestURI(), strings.NewReader("{}")))
		if len(got) != 1 || got[0] != payer {
			t.Errorf("POST %s/Claim/$submit: routed payer %q, want [%q]", base, got, payer)
		}
	}
}
