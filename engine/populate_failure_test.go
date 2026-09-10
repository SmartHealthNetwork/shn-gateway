package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SmartHealthNetwork/shn-gateway/connectors/smartauth"
)

const failurePackage = `{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Questionnaire","url":"https://example.test/CANARY-CANONICAL"}}]}`

var failureContext = PopulateContext{Member: "CANARY-MEMBER", PatientRef: "Patient/CANARY-LOGICAL", SubjectFHIRRef: "Patient/CANARY-STORE", CoverageRef: "Coverage/CANARY-COVERAGE", OrderRef: "ServiceRequest/CANARY-ORDER"}

const failureQR = `{"resourceType":"QuestionnaireResponse","subject":{"reference":"Patient/CANARY-STORE"},"item":[{"linkId":"CANARY-ITEM"}]}`

type failureReader struct{ err error }

func (r failureReader) Read([]byte) (int, error) { return 0, r.err }
func (r failureReader) Close() error             { return nil }

func TestNativePopulateFailureDiscrimination(t *testing.T) {
	rows := []struct {
		name, stage, reason string
		status              int
		calls               int32
		body                string
		failure             error
		token               bool
	}{
		{name: "request URL", stage: "request_build", reason: "other"},
		{name: "token401", stage: "token_acquisition", reason: "other", token: true},
		{name: "imitated token error", stage: "transport", reason: "other", calls: 1, failure: errors.New("smartauth: acquire token: CANARY-ERROR")},
		{name: "transport cancel", stage: "transport", reason: "canceled", calls: 1, failure: fmt.Errorf("CANARY-ERROR: %w", context.Canceled)},
		{name: "transport deadline", stage: "transport", reason: "deadline", calls: 1, failure: fmt.Errorf("CANARY-ERROR: %w", context.DeadlineExceeded)},
		{name: "token cancel", stage: "token_acquisition", reason: "canceled", token: true, failure: context.Canceled},
		{name: "token deadline", stage: "token_acquisition", reason: "deadline", token: true, failure: context.DeadlineExceeded},
		{name: "read200", stage: "body_read", reason: "other", status: 200, calls: 1, failure: errors.New("CANARY-READER")},
		{name: "read503 precedence", stage: "body_read", reason: "other", status: 503, calls: 1, failure: errors.New("CANARY-READER")},
		{name: "read cancel", stage: "body_read", reason: "canceled", status: 200, calls: 1, failure: context.Canceled},
		{name: "read deadline", stage: "body_read", reason: "deadline", status: 200, calls: 1, failure: context.DeadlineExceeded},
		{name: "status401", stage: "http_status", reason: "non_2xx", status: 401, calls: 1, body: "CANARY-QR-BODY"},
		{name: "status503", stage: "http_status", reason: "non_2xx", status: 503, calls: 1, body: "CANARY-QR-BODY"},
		{name: "invalid JSON", stage: "qr_extract", reason: "invalid_json", status: 200, calls: 1, body: `{"CANARY-QR-BODY":`},
		{name: "wrong resource", stage: "qr_extract", reason: "wrong_resource_type", status: 200, calls: 1, body: `{"resourceType":"CANARY-RESOURCE"}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var calls, tokenCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("CANARY-HEADER", "CANARY-HEADER-VALUE")
				w.WriteHeader(row.status)
				_, _ = io.WriteString(w, row.body)
			}))
			defer srv.Close()
			client := srv.Client()
			endpoint := srv.URL + "/CANARY-URL?identifier=CANARY-QUERY"
			if row.name == "request URL" {
				endpoint = "http://[CANARY-URL"
			}
			if row.failure != nil && !row.token {
				client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					if row.stage == "body_read" {
						return &http.Response{StatusCode: row.status, Header: http.Header{"CANARY-HEADER": []string{"CANARY-HEADER-VALUE"}}, Body: failureReader{row.failure}}, nil
					}
					return nil, row.failure
				})}
			}
			if row.token {
				tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					tokenCalls.Add(1)
					w.Header().Set("CANARY-HEADER", "CANARY-HEADER-VALUE")
					w.WriteHeader(401)
					_, _ = io.WriteString(w, "CANARY-TOKEN-BODY")
				}))
				defer tok.Close()
				tokenClient := tok.Client()
				if row.failure != nil {
					tokenClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
						tokenCalls.Add(1)
						return nil, fmt.Errorf("CANARY-TOKEN-ERROR: %w", row.failure)
					})}
				}
				var err error
				client, err = smartauth.NewHTTPClient(smartauth.Config{TokenURL: tok.URL + "/CANARY-TOKEN-URL", ClientID: "CANARY-CLIENT", ClientSecret: "CANARY-SECRET", HTTPClient: tokenClient})
				if err != nil {
					t.Fatal(err)
				}
			}
			var notes []PopulateFailure
			pop := NewNativePopulatorWithFailureObserver(client, endpoint, func(note PopulateFailure) { notes = append(notes, note) })
			qr, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext)
			if err != errPopulateUpstream {
				t.Fatalf("error = %v", err)
			}
			if len(notes) != 1 || notes[0].Stage != row.stage || notes[0].Reason != row.reason || notes[0].Status != row.status {
				t.Fatalf("notes = %#v", notes)
			}
			if qr != nil {
				t.Fatal("failed upstream returned QR")
			}
			if calls.Load() != row.calls {
				t.Fatalf("request count = %d, want %d", calls.Load(), row.calls)
			}
			if row.token && tokenCalls.Load() != 1 {
				t.Fatalf("token calls = %d", tokenCalls.Load())
			}
			wire, err := json.Marshal(notes)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "CANARY") || strings.Contains(string(wire), srv.URL) {
				t.Fatalf("sensitive record: %s", wire)
			}
			var fields []map[string]any
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields[0]) != 3 {
				t.Fatalf("unexpected fields: %s", wire)
			}
		})
	}
}

func TestNativePopulateFailureCompatibility(t *testing.T) {
	var old func(*http.Client, string) *nativePopulator = NewNativePopulator
	var observed func(*http.Client, string, func(PopulateFailure)) *nativePopulator = NewNativePopulatorWithFailureObserver
	var calls atomic.Int32
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"CANARY-BEARER","expires_in":3600}`)
	}))
	defer tok.Close()
	client, err := smartauth.NewHTTPClient(smartauth.Config{TokenURL: tok.URL, ClientID: "CANARY-CLIENT", ClientSecret: "CANARY-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer CANARY-BEARER" {
			t.Error("missing bearer")
		}
		body := failureQR
		if r.URL.Path == "/foreign" {
			body = strings.ReplaceAll(body, "CANARY-STORE", "CANARY-FOREIGN")
		}
		if r.URL.Path == "/fail" {
			w.WriteHeader(503)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	for _, row := range []struct {
		name, path string
		want       error
	}{{"success", "", nil}, {"foreign subject", "/foreign", errPopulateForeignSubject}} {
		t.Run(row.name, func(t *testing.T) {
			var notes []PopulateFailure
			pop := observed(client, srv.URL+row.path, func(n PopulateFailure) { notes = append(notes, n) })
			qr, fill, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext)
			if err != row.want || len(notes) != 0 || fill != nil {
				t.Fatalf("err=%v notes=%#v fill=%v", err, notes, fill)
			}
			if err == nil {
				subject, e := questionnaireResponseSubject(qr)
				if e != nil || subject != failureContext.PatientRef || !strings.Contains(string(qr), "CANARY-ITEM") {
					t.Fatalf("normalized QR = %s (%v)", qr, e)
				}
			} else if qr != nil {
				t.Fatal("foreign subject returned QR")
			}
		})
	}
	for _, pop := range []*nativePopulator{old(client, srv.URL+"/fail"), observed(client, srv.URL+"/fail", nil)} {
		if _, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext); err != errPopulateUpstream {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestNativePopulateFailureConcurrent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	var mu sync.Mutex
	var notes []PopulateFailure
	pop := NewNativePopulatorWithFailureObserver(srv.Client(), srv.URL, func(n PopulateFailure) { mu.Lock(); defer mu.Unlock(); notes = append(notes, n) })
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext); err != errPopulateUpstream {
				t.Errorf("err=%v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 16 || len(notes) != 16 {
		t.Fatalf("calls=%d notes=%#v", calls.Load(), notes)
	}
	for _, n := range notes {
		if n != (PopulateFailure{Stage: "http_status", Reason: "non_2xx", Status: 503}) {
			t.Fatalf("note=%#v", n)
		}
	}
}

func TestNativePopulateFailureRedirectAndStatusBounds(t *testing.T) {
	t.Run("redirect refusal retains observed response", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, "/CANARY-REDIRECT", 302)
		}))
		defer srv.Close()
		client := srv.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("CANARY-REDIRECT-ERROR") }
		var notes []PopulateFailure
		pop := NewNativePopulatorWithFailureObserver(client, srv.URL, func(n PopulateFailure) { notes = append(notes, n) })
		if _, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext); err != errPopulateUpstream {
			t.Fatal(err)
		}
		if calls.Load() != 1 || len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "transport", Reason: "other", Status: 302}) {
			t.Fatalf("calls=%d notes=%#v", calls.Load(), notes)
		}
	})
	for _, status := range []int{99, 600} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("CANARY-BODY"))}, nil
			})}
			var notes []PopulateFailure
			pop := NewNativePopulatorWithFailureObserver(client, "http://example.test", func(n PopulateFailure) { notes = append(notes, n) })
			if _, _, err := pop.Populate(context.Background(), []byte(failurePackage), failureContext); err != errPopulateUpstream {
				t.Fatal(err)
			}
			if len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "http_status", Reason: "non_2xx", Status: 0}) {
				t.Fatalf("notes=%#v", notes)
			}
		})
	}
}
