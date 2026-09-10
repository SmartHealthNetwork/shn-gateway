package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestBuildPopulateParameters_UsesSubjectFHIRRef: the $populate subject is the FHIR-store ref
// (a scoped id), not the logical SHN ref — so the engine's CQL retrieves hit the right
// compartment. Falls back to PatientRef when SubjectFHIRRef is empty.
func TestBuildPopulateParameters_UsesSubjectFHIRRef(t *testing.T) {
	q := []byte(`{"resourceType":"Questionnaire","url":"u"}`)
	b, err := buildPopulateParameters(q, PopulateContext{PatientRef: "Patient/MBR-COVERED", SubjectFHIRRef: "Patient/pat-mbrcovered-provider"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(string(b), `"reference":"Patient/pat-mbrcovered-provider"`) {
		t.Fatalf("subject is not the FHIR ref:\n%s", b)
	}
	if strings.Contains(string(b), `"reference":"Patient/MBR-COVERED"`) {
		t.Fatalf("used the logical ref instead of the FHIR ref:\n%s", b)
	}
	b2, err := buildPopulateParameters(q, PopulateContext{PatientRef: "Patient/MBR-COVERED"})
	if err != nil {
		t.Fatalf("build (fallback): %v", err)
	}
	if !strings.Contains(string(b2), `"reference":"Patient/MBR-COVERED"`) {
		t.Fatalf("empty SubjectFHIRRef did not fall back to PatientRef:\n%s", b2)
	}
}

// TestSetQuestionnaireResponseSubject: rewrites subject.reference, preserves other fields.
func TestSetQuestionnaireResponseSubject(t *testing.T) {
	in := []byte(`{"resourceType":"QuestionnaireResponse","status":"in-progress","subject":{"reference":"Patient/pat-x-provider"},"item":[{"linkId":"a"}]}`)
	out := setQuestionnaireResponseSubject(in, "Patient/MBR-COVERED")
	subj, err := questionnaireResponseSubject(out)
	if err != nil || subj != "Patient/MBR-COVERED" {
		t.Fatalf("subject = %q (err=%v), want Patient/MBR-COVERED", subj, err)
	}
	for _, keep := range []string{`"status":"in-progress"`, `"linkId":"a"`, `"resourceType":"QuestionnaireResponse"`} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("normalization dropped %q:\n%s", keep, out)
		}
	}
}

// TestNativePopulator_UpstreamRefusalFailsClosed: a non-2xx from the $populate endpoint — a 401
// from the auth gate in front of it is the case that matters — is errPopulateUpstream after
// exactly one attempt. The populator never retries (unauthenticated or otherwise); an
// authenticated client's token refusal takes the same path (transport error → one attempt).
func TestNativePopulator_UpstreamRefusalFailsClosed(t *testing.T) {
	pkg := []byte(`{"resourceType":"Bundle","type":"collection","entry":[{"resource":{"resourceType":"Questionnaire","url":"http://example.org/q"}}]}`)
	pc := PopulateContext{PatientRef: "Patient/MBR-COVERED", SubjectFHIRRef: "Patient/pat-mbrcovered-provider"}

	t.Run("401 from the gate", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, `{"resourceType":"OperationOutcome"}`, http.StatusUnauthorized)
		}))
		defer srv.Close()
		_, _, err := NewNativePopulator(srv.Client(), srv.URL+"/Questionnaire/$populate").Populate(context.Background(), pkg, pc)
		if !errors.Is(err, errPopulateUpstream) {
			t.Fatalf("err = %v, want errPopulateUpstream", err)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("endpoint hit %d times, want exactly 1 (no retry)", got)
		}
	})

	t.Run("token refusal before the request leaves", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Write([]byte(`{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/pat-mbrcovered-provider"}}`))
		}))
		defer srv.Close()
		// An authenticated client whose token fetch fails surfaces a transport error from Do;
		// the populator must map it to errPopulateUpstream without falling back to a plain client.
		refusing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("smartauth: token endpoint status 401: invalid_client")
		})}
		_, _, err := NewNativePopulator(refusing, srv.URL+"/Questionnaire/$populate").Populate(context.Background(), pkg, pc)
		if !errors.Is(err, errPopulateUpstream) {
			t.Fatalf("err = %v, want errPopulateUpstream", err)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Fatalf("endpoint hit %d times after a token refusal, want 0", got)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
