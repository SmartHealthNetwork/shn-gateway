package fhirsor_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/fhirsor"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"github.com/SmartHealthNetwork/shn-gateway/engine/relay"
	"github.com/SmartHealthNetwork/shn-gateway/internal/fhirclient"
)

// The evidence a provider-data PAS request carries is read by reference
// through this connector (ResolveByReferenceContext), which reads a system's
// answer as a resource of the referenced type before the gateway decides its
// shape. Each row pins what reaches the gateway: the record as answered
// (found), absence (only on the system's definite 404 or 410), or a failed
// read of the system (a *engine.SoRReadError), never absence for an answer
// that is not one. The gateway's answer for each is in
// engine's evidenceSubjectRows; the participant docs state both.
func TestSoR_ResolveByReference_EvidenceShapes(t *testing.T) {
	const ok = `{"resourceType":"Condition","id":"cond-reason","code":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"J44.9"}]},"subject":{"reference":"Patient/pat-77"}}`
	repl := func(old, new string) string { return strings.Replace(ok, old, new, 1) }
	const (
		found    = "found"
		absent   = "absent"
		readFail = "failed read"
	)
	for _, row := range []struct {
		name   string
		status int
		body   string
		want   string
		kind   engine.SoRFailureKind // for a failed read
		ref    string                // the reference read; Condition/cond-reason when unset
	}{
		{"the record", 200, ok, found, "", ""},
		{"another patient", 200, repl(`Patient/pat-77`, `Patient/someone-else`), found, "", ""},
		{"a Group", 200, repl(`Patient/pat-77`, `Group/cohort-1`), found, "", ""},
		{"a contained reference", 200, repl(`Patient/pat-77`, `#p1`), found, "", ""},
		{"an empty reference", 200, repl(`Patient/pat-77`, ``), found, "", ""},
		{"no subject", 200, repl(`,"subject":{"reference":"Patient/pat-77"}`, ``), found, "", ""},
		{"a subject with no reference", 200, repl(`{"reference":"Patient/pat-77"}`, `{"display":"the member"}`), found, "", ""},
		{"another id", 200, repl(`"id":"cond-reason"`, `"id":"cond-other"`), found, "", ""},
		{"bytes that are not UTF-8", 200, repl(`"J44.9"`, "\"J44.9\xff\""), found, "", ""},
		{"a lone surrogate escape", 200, repl(`"J44.9"`, `"J44.9\ud800"`), found, "", ""},
		{"a reference that is not a string", 200, repl(`"Patient/pat-77"`, `42`), readFail, engine.SoRInvalidResponse, ""},
		// The connector reads a Condition, Observation or DiagnosticReport
		// into its typed model; a Goal or ClinicalImpression it checks only
		// for its resourceType and id, so the gateway decides the rest.
		{"a Goal whose reference is not a string", 200, `{"resourceType":"Goal","id":"g1","subject":{"reference":42}}`, found, "", "Goal/g1"},
		{"a ClinicalImpression whose reference is not a string", 200, `{"resourceType":"ClinicalImpression","id":"ci1","subject":{"reference":42}}`, found, "", "ClinicalImpression/ci1"},
		{"longer than 8 MiB", 200, repl(`"J44.9"`, `"`+strings.Repeat("x", 8<<20)+`"`), readFail, engine.SoRInvalidResponse, ""},
		{"not a JSON object", 200, `[]`, readFail, engine.SoRInvalidResponse, ""},
		{"another resourceType", 200, repl(`"resourceType":"Condition"`, `"resourceType":"Observation"`), readFail, engine.SoRInvalidResponse, ""},
		{"a member named twice", 200, repl(`"id":"cond-reason"`, `"id":"cond-reason","id":"cond-reason"`), readFail, engine.SoRInvalidResponse, ""},
		{"a member named again in another case", 200, repl(`"code":`, `"Code":{},"code":`), readFail, engine.SoRInvalidResponse, ""},
		{"not found", 404, `{"resourceType":"OperationOutcome"}`, absent, "", ""},
		{"gone", 410, `{"resourceType":"OperationOutcome"}`, absent, "", ""},
		{"a server error", 500, `{"resourceType":"OperationOutcome"}`, readFail, engine.SoRUnavailable, ""},
		{"unavailable", 503, `{"resourceType":"OperationOutcome"}`, readFail, engine.SoRUnavailable, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/fhir+json")
				w.WriteHeader(row.status)
				_, _ = w.Write([]byte(row.body))
			}))
			defer srv.Close()
			ref := row.ref
			if ref == "" {
				ref = "Condition/cond-reason"
			}
			raw, isFound, err := fhirsor.New(fhirclient.New(srv.URL, srv.Client())).ResolveByReferenceContext(context.Background(), ref)
			var readErr *engine.SoRReadError
			got := found
			switch {
			case err != nil:
				got = readFail
				if !errors.As(err, &readErr) {
					t.Fatalf("err %v (%T), want a *engine.SoRReadError", err, err)
				}
			case !isFound:
				got = absent
			}
			if got != row.want {
				t.Fatalf("%s, want %s (err %v)", got, row.want, err)
			}
			if got == found && string(raw) != row.body {
				t.Fatalf("answered %q, want the record as the system answered it", raw)
			}
			if got == readFail && readErr.Kind != row.kind {
				t.Fatalf("failed read of kind %s, want %s", readErr.Kind, row.kind)
			}
			// A repeated member name is an answer that reads two ways: the
			// failed read says so, and the gateway records it as message
			// integrity.
			if dup := strings.Contains(row.name, "member named"); errors.Is(err, relay.ErrDuplicateKey) != dup {
				t.Fatalf("failed read %v: repeats a member %v, want %v", err, !dup, dup)
			}
		})
	}
}
