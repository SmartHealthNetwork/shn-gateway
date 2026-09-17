package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func controlledResponse(profile, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path+"?"+url.Values{"profile": {profile}}.Encode(), strings.NewReader(body))
	w := httptest.NewRecorder()
	serveValidation(w, r, []byte(body))
	return w
}
func TestExplicitProfileControls(t *testing.T) {
	for _, profile := range []string{"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|9.9.9", "https://example.org/fhir/StructureDefinition/unavailable-profile"} {
		t.Run(profile, func(t *testing.T) {
			w := controlledResponse(profile, "/fhir/ClaimResponse/$validate", `{"resourceType":"ClaimResponse","id":"synthetic"}`)
			encoded, _ := json.Marshal(profile)
			want := `{"resourceType":"OperationOutcome","issue":[{"severity":"error","code":"processing","details":{"coding":[{"system":"http://hl7.org/fhir/java-core-messageId","code":"Validation_VAL_Profile_Unknown"}]},"diagnostics":"Invalid profile. Failed to retrieve explicitly requested profile with url=` + string(encoded[1:len(encoded)-1]) + `"}]}`
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
				t.Fatalf("status=%d body=%s want=%s", w.Code, w.Body.String(), want)
			}
			for _, body := range []string{`{`, `null`, `[]`, `{}`, `{"resourceType":null}`, `{"resourceType":1}`, `{"resourceType":"Claim"}`} {
				if got := controlledResponse(profile, "/fhir/ClaimResponse/$validate", body); got.Code == http.StatusOK {
					t.Errorf("malformed/mismatched body accepted: %s", body)
				}
			}
			for _, path := range []string{"/fhir/Claim/$validate", "/fhir/Bundle/$validate", "/fhir/ClaimResponse/other"} {
				if got := controlledResponse(profile, path, `{"resourceType":"ClaimResponse"}`); got.Code == http.StatusOK {
					t.Errorf("mismatched path accepted: %s", path)
				}
			}
		})
	}
}
func TestOrdinaryProfilesRetainInformationalResponse(t *testing.T) {
	for _, profile := range []string{"", "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse", "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.0.1", "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|2.2.1", "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|9.9.90", "https://example.org/fhir/StructureDefinition/unavailable-profile-extra"} {
		w := controlledResponse(profile, "/fhir/ClaimResponse/$validate", `{"resourceType":"ClaimResponse"}`)
		if w.Code != http.StatusOK || w.Body.String() != `{"resourceType":"OperationOutcome","issue":[{"severity":"information","code":"informational","diagnostics":"Validation successful"}]}` {
			t.Fatalf("profile=%s status=%d body=%s", profile, w.Code, w.Body.String())
		}
	}
}
func TestExistingMutationResponsesRemain(t *testing.T) {
	w := controlledResponse("", "/fhir/ClaimResponse/$validate", `{"resourceType":"ClaimResponse","valueBoolean":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":"Extension_EXT_Type"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// The committed fixture directory is deliberately unavailable in this host test.
	// The fixture branch must fail explicitly rather than return an informational pass.
	w = controlledResponse("", "/fhir/Bundle/$validate", `{"resourceType":"Bundle","code":"L9999"}`)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "controlled fixture unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
}
