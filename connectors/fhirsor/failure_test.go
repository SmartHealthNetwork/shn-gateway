package fhirsor

import (
	"context"
	"errors"
	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
	"github.com/SmartHealthNetwork/shn-gateway/engine"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPatientFailure(t *testing.T) {
	for _, body := range []string{`{`, `{"resourceType":"Patient"}`, `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient"}}]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		reader, ok := any(NewFromURL(srv.URL, srv.Client())).(engine.ContextSystemOfRecord)
		if !ok {
			srv.Close()
			t.Fatal("FHIR connector lacks error-aware reads")
		}
		_, _, found, err := reader.ResolvePatientContext(context.Background(), "member")
		srv.Close()
		if found || err == nil {
			t.Fatalf("backend failure became absence: %v %v", found, err)
		}
	}
}

const failurePatient = `{"resourceType":"Patient","id":"p","identifier":[{"system":"urn:shn:member","value":"MBR-OP"}],"name":[{"family":"Op"}],"birthDate":"1970-01-01"}`
const emptySearch = `{"resourceType":"Bundle","type":"searchset"}`

func searchResource(raw string) string {
	return `{"resourceType":"Bundle","type":"searchset","entry":[{"resource":` + raw + `}]}`
}
func TestPatientFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, want int
		found        bool
	}{
		{"valid", searchResource(failurePatient), 200, 0, true}, {"empty", emptySearch, 200, 0, false},
		{"bad-json", "private-response-sentinel", 200, 502, false},
		{"wrong-bundle", failurePatient, 200, 502, false},
		{"wrong-resource", searchResource(`{"resourceType":"Organization","id":"p"}`), 200, 502, false},
		{"no-id", searchResource(`{"resourceType":"Patient"}`), 200, 502, false},
		{"no-demographics", searchResource(`{"resourceType":"Patient","id":"p"}`), 200, 502, false},
		{"bad-demographics", searchResource(`{"resourceType":"Patient","id":"p","birthDate":42}`), 200, 502, false},
		{"ambiguous", strings.Replace(searchResource(failurePatient), `"entry":`, `"total":2,"entry":`, 1), 200, 502, false},
		{"next", strings.Replace(searchResource(failurePatient), `"entry":`, `"link":[{"relation":"next","url":"private-response-sentinel"}],"entry":`, 1), 200, 502, false},
		{"401", "private-response-sentinel", 401, 502, false}, {"403", "private-response-sentinel", 403, 502, false},
		{"429", "private-response-sentinel", 429, 503, false}, {"503", "private-response-sentinel", 503, 503, false}, {"search-404", "", 404, 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer srv.Close()
			_, _, found, err := NewFromURL(srv.URL, srv.Client()).ResolvePatientContext(context.Background(), "private-member-sentinel")
			if found != tc.found {
				t.Fatalf("found=%v", found)
			}
			if tc.want == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			assertFailure(t, err, tc.want)
		})
	}
}
func assertFailure(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatal("missing backend error")
	}
	got, msg := engine.SoRFailureResponse(err)
	if got != status || strings.Contains(msg, "sentinel") || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("unsafe/wrong error: %d %s %v", got, msg, err)
	}
}
func TestSecondaryFailure(t *testing.T) {
	methods := []struct {
		name, path string
		read       func(*SoR) (bool, error)
	}{
		{"reference", "/Organization/p", func(s *SoR) (bool, error) {
			_, f, e := s.ResolveByReferenceContext(context.Background(), "Organization/p")
			return f, e
		}},
		{"coverage", "/Coverage", func(s *SoR) (bool, error) {
			f, _, e := s.CoverageInforceContext(context.Background(), "m")
			return f, e
		}},
		{"open-coverage", "/Coverage", func(s *SoR) (bool, error) { _, f, e := s.OpenCoverageContext(context.Background(), "m"); return f, e }},
		{"order", "/DeviceRequest", func(s *SoR) (bool, error) { _, f, e := s.OpenOrderContext(context.Background(), "m"); return f, e }},
		{"report", "/DiagnosticReport", func(s *SoR) (bool, error) {
			_, f, e := s.SupplementalReportContext(context.Background(), "m")
			return f, e
		}},
		{"facility-late", "/DocumentReference", func(s *SoR) (bool, error) {
			v, f, e := s.FacilityRecordsContext(context.Background(), "m")
			if e != nil && v != nil {
				t.Fatal("partial records")
			}
			return f, e
		}},
		{"clinical-late", "/Observation", func(s *SoR) (bool, error) {
			v, f, e := s.ClinicalContextContext(context.Background(), "m")
			if e != nil && v.ConditionRef != "" {
				t.Fatal("partial clinical facts")
			}
			return f, e
		}},
		{"patient-ref", "/Patient", func(s *SoR) (bool, error) { _, f, e := s.PatientFHIRRefContext(context.Background(), "m"); return f, e }},
	}
	for _, tc := range methods {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					w.WriteHeader(503)
					return
				}
				switch r.URL.Path {
				case "/Patient":
					w.Write([]byte(searchResource(failurePatient)))
				case "/Condition":
					w.Write([]byte(searchResource(`{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"J44.9"}]}}`)))
				case "/DiagnosticReport":
					w.Write([]byte(searchResource(`{"resourceType":"DiagnosticReport","id":"r"}`)))
				case "/ServiceRequest":
					t.Error("fell through after DeviceRequest failure")
				default:
					w.Write([]byte(emptySearch))
				}
			}))
			defer srv.Close()
			found, err := tc.read(NewFromURL(srv.URL, srv.Client()))
			if found {
				t.Fatal("failure returned found")
			}
			assertFailure(t, err, 503)
		})
	}
}

func TestMalformedConsumedFieldFailure(t *testing.T) {
	for _, raw := range []string{`{"resourceType":"DiagnosticReport","id":"r","subject":42}`, `{"resourceType":"DiagnosticReport","id":"r","code":42}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Patient" {
				w.Write([]byte(searchResource(failurePatient)))
			} else {
				w.Write([]byte(searchResource(raw)))
			}
		}))
		_, found, err := NewFromURL(srv.URL, srv.Client()).SupplementalReportContext(context.Background(), "m")
		srv.Close()
		if found {
			t.Fatal("malformed consumed field returned success")
		}
		assertFailure(t, err, 502)
	}
}

type failureTransport func(*http.Request) (*http.Response, error)

func (f failureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("private-response-sentinel") }
func (brokenBody) Close() error             { return nil }
func TestTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   failureTransport
		want int
	}{
		{"transport", func(*http.Request) (*http.Response, error) { return nil, errors.New("private-response-sentinel") }, 503},
		{"read", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: brokenBody{}, Header: make(http.Header)}, nil
		}, 503},
		{"oversize", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(emptySearch + strings.Repeat(" ", 8<<20))), Header: make(http.Header)}, nil
		}, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, found, err := NewFromURL("http://example.invalid", &http.Client{Transport: tc.rt}).ResolvePatientContext(context.Background(), "m")
			if found {
				t.Fatal("failure found")
			}
			assertFailure(t, err, tc.want)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached server") }))
	defer srv.Close()
	_, _, _, err := NewFromURL(srv.URL, srv.Client()).ResolvePatientContext(ctx, "m")
	assertFailure(t, err, 503)
}
func TestSMARTFailure(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var protected atomic.Int32
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				if status == 200 {
					w.Write([]byte(`{"access_token":"private-token-sentinel","expires_in":3600}`))
				} else {
					w.Write([]byte("private-response-sentinel"))
				}
			}))
			defer token.Close()
			fhir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { protected.Add(1); w.Write([]byte(emptySearch)) }))
			defer fhir.Close()
			hc, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: token.URL, ClientID: "client", ClientSecret: "private-secret-sentinel"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, observation := smartauth.WithTokenAcquisitionObservation(context.Background())
			_, _, found, err := NewFromURL(fhir.URL, hc).ResolvePatientContext(ctx, "private-member-sentinel")
			if found {
				t.Fatal("empty/failure found")
			}
			if status == 200 {
				if err != nil || protected.Load() != 1 || observation.Failed() {
					t.Fatal("positive SMART control failed")
				}
				return
			}
			want := 502
			if status == 429 || status >= 500 {
				want = 503
			}
			assertFailure(t, err, want)
			if protected.Load() != 0 || !observation.Failed() {
				t.Fatal("failed acquisition lost observation or accessed FHIR")
			}
		})
	}
}

func TestOptionalAbsenceControls(t *testing.T) {
	for _, observation := range []string{
		`{"resourceType":"Observation","id":"o"}`,
		`{"resourceType":"Observation","id":"o","dataAbsentReason":{"text":"not available"}}`,
		`{"resourceType":"Observation","id":"o","valueQuantity":{}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/Patient":
				w.Write([]byte(searchResource(failurePatient)))
			case "/Condition":
				w.Write([]byte(searchResource(`{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"J44.9"}]}}`)))
			case "/Observation":
				w.Write([]byte(searchResource(observation)))
			default:
				w.Write([]byte(emptySearch))
			}
		}))
		cc, found, err := NewFromURL(srv.URL, srv.Client()).ClinicalContextContext(context.Background(), "m")
		srv.Close()
		if !found || err != nil || cc.OxygenSaturationPct != "" {
			t.Fatalf("optional missing value failed: %v %v", found, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(searchResource(`{"resourceType":"Patient","id":"p"}`)))
	}))
	defer srv.Close()
	ref, found, err := NewFromURL(srv.URL, srv.Client()).PatientFHIRRefContext(context.Background(), "m")
	if ref != "Patient/p" || !found || err != nil {
		t.Fatalf("reference unnecessarily requires demographics: %s %v %v", ref, found, err)
	}
}

func TestCoverageMissingStatusFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Patient" {
			w.Write([]byte(searchResource(failurePatient)))
		} else {
			w.Write([]byte(searchResource(`{"resourceType":"Coverage","id":"c"}`)))
		}
	}))
	defer srv.Close()
	active, _, err := NewFromURL(srv.URL, srv.Client()).CoverageInforceContext(context.Background(), "m")
	if active {
		t.Fatal("missing status became active coverage")
	}
	assertFailure(t, err, 502)
}

func TestActualRequestCancellationFailure(t *testing.T) {
	for _, smart := range []bool{false, true} {
		t.Run(strconv.FormatBool(smart), func(t *testing.T) {
			entered := make(chan struct{})
			stopped := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
					close(stopped)
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			hc := srv.Client()
			if smart {
				var err error
				hc, err = smartauth.NewHTTPClient(smartauth.Config{TokenURL: srv.URL, ClientID: "client", ClientSecret: "secret"})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, _, _, err := NewFromURL(srv.URL, hc).ResolvePatientContext(ctx, "m"); result <- err }()
			<-entered
			cancel()
			assertFailure(t, <-result, 503)
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("request cancellation did not reach endpoint")
			}
		})
	}
}

func TestObservationQuantityJSONTypeFailure(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		wantError   bool
		want        string
	}{
		{"quoted-number", `"89"`, true, ""},
		{"number", `89`, false, "89"},
		{"absent", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := `{"resourceType":"Observation","id":"o"}`
			if tc.value != "" {
				observation = `{"resourceType":"Observation","id":"o","valueQuantity":{"value":` + tc.value + `}}`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/Patient":
					w.Write([]byte(searchResource(failurePatient)))
				case "/Condition":
					w.Write([]byte(searchResource(`{"resourceType":"Condition","id":"c","code":{"coding":[{"system":"http://hl7.org/fhir/sid/icd-10-cm","code":"J44.9"}]}}`)))
				case "/Observation":
					w.Write([]byte(searchResource(observation)))
				default:
					w.Write([]byte(emptySearch))
				}
			}))
			defer srv.Close()
			cc, found, err := NewFromURL(srv.URL, srv.Client()).ClinicalContextContext(context.Background(), "m")
			if tc.wantError {
				if found || cc.ConditionRef != "" {
					t.Fatal("malformed quantity became clinical evidence")
				}
				assertFailure(t, err, 502)
				return
			}
			if !found || err != nil || cc.OxygenSaturationPct != tc.want {
				t.Fatalf("valid/absent quantity changed: found=%v err=%v quantity=%q", found, err, cc.OxygenSaturationPct)
			}
		})
	}
}
