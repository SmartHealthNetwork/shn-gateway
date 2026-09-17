package fhirsor_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// TestFacilityRecordsContextKeepsServerBytes: the facility's records are
// returned exactly as its server holds them; the subject is not rewritten.
func TestFacilityRecordsContextKeepsServerBytes(t *testing.T) {
	dr := "{ \"resourceType\" : \"DiagnosticReport\", \"id\" : \"dr-1\", \"status\" : \"final\", \"code\" : { \"text\" : \"a <b> & c\" }, \"subject\" : { \"reference\" : \"Patient/fac-77\" }, \"extension\" : [ { \"url\" : \"x\", \"valueDecimal\" : 1.50 } ] }"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/Patient"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"fac-77","identifier":[{"system":"urn:shn:member","value":"MBR-UC05"}],"name":[{"family":"X"}],"birthDate":"1970-01-01"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/DiagnosticReport"):
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + dr + `}]}`))
		default:
			w.Write([]byte(`{"resourceType":"Bundle","type":"searchset"}`))
		}
	}))
	t.Cleanup(srv.Close)
	recs, found, err := fhirsor.New(fhirclient.New(srv.URL, nil)).FacilityRecordsContext(context.Background(), "MBR-UC05")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if string(recs["DiagnosticReport"]) != dr {
		t.Fatalf("record changed: %s", recs["DiagnosticReport"])
	}
}
