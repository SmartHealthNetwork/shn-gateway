package engine_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	shnsdk "github.com/SmartHealthNetwork/shn-sdk"
)

// The participant's own FHIR system the end-to-end rows read through the
// bundled FHIR connector (workstream_fqs_repeated_e2e_test.go,
// workstream_pa_crd_ingress_repeated_e2e_test.go).

// e2eFHIRServer is a participant's own FHIR system: the member's Patient by
// search (search) and by read (read), and nothing else.
func e2eFHIRServer(t *testing.T, search, read string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Patient":
			_, _ = w.Write([]byte(search))
		case "/Patient/e2e-1":
			_, _ = w.Write([]byte(read))
		default:
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// e2ePatient is member's Patient as the participant's FHIR system holds it.
func e2ePatient(member, birth, family string) string {
	return `{"resourceType":"Patient","id":"e2e-1","identifier":[{"system":"` + shnsdk.MemberSystem + `","value":"` + member + `"}],"name":[{"family":"` + family + `"}],"birthDate":"` + birth + `"}`
}

func e2eSearchset(resource string) string {
	return `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + resource + `}]}`
}
