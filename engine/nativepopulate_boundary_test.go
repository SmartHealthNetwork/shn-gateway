package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The response is an action result, not native carriage: a successful prefix
// must never hide bytes beyond the closed transport limit.
func TestNativePopulateCompleteResponseBoundary(t *testing.T) {
	for _, row := range []struct {
		name   string
		size   int
		tail   string
		refuse bool
	}{
		{"max-minus-one", maxPartnerBody - 1, "", false}, {"max", maxPartnerBody, "", false},
		{"max-plus-one", maxPartnerBody + 1, "", true}, {"valid-prefix-trailing-payload", maxPartnerBody, `{"private":"CANARY"}`, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			body := failureQR + strings.Repeat(" ", row.size-len(failureQR)) + row.tail
			var calls, closed atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, body) }))
			defer srv.Close()
			client := srv.Client()
			base := client.Transport
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := base.RoundTrip(r)
				if err == nil {
					resp.Body = &populationTrackedBody{ReadCloser: resp.Body, closed: &closed}
				}
				return resp, err
			})
			var notes []PopulateFailure
			qr, _, err := NewNativePopulatorWithFailureObserver(client, srv.URL, func(n PopulateFailure) { notes = append(notes, n) }).Populate(context.Background(), []byte(failurePackage), failureContext)
			if row.refuse {
				if err != errPopulateUpstream || qr != nil || len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "body_read", Reason: "other", Status: 200}) {
					t.Errorf("overflow accepted or misclassified: err=%v qrBytes=%d notes=%+v", err, len(qr), notes)
				}
			} else if err != nil || len(qr) == 0 || len(notes) != 0 {
				t.Errorf("complete response refused: %v notes=%+v", err, notes)
			}
			if calls.Load() != 1 || closed.Load() != 1 {
				t.Errorf("calls=%d closed=%d", calls.Load(), closed.Load())
			}
		})
	}
}

type populationTrackedBody struct {
	io.ReadCloser
	closed *atomic.Int32
	cancel context.CancelFunc
}

func (b *populationTrackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return n, err
}

func (b *populationTrackedBody) Close() error { b.closed.Add(1); return b.ReadCloser.Close() }

func TestNativePopulateRealReadFailuresCloseBody(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRead), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(failureQR)+32))
				io.WriteString(w, failureQR)
				w.(http.Flusher).Flush()
				if cancelRead {
					<-r.Context().Done()
				}
			}))
			defer srv.Close()
			var closed atomic.Int32
			client := srv.Client()
			base := client.Transport
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := base.RoundTrip(r)
				if err == nil {
					resp.Body = &populationTrackedBody{ReadCloser: resp.Body, closed: &closed}
					if cancelRead {
						resp.Body.(*populationTrackedBody).cancel = cancel
					}
				}
				return resp, err
			})
			var notes []PopulateFailure
			qr, _, err := NewNativePopulatorWithFailureObserver(client, srv.URL, func(n PopulateFailure) { notes = append(notes, n) }).Populate(ctx, []byte(failurePackage), failureContext)
			reason := "other"
			if cancelRead {
				reason = "canceled"
			}
			if err != errPopulateUpstream || qr != nil || len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "body_read", Reason: reason, Status: 200}) || closed.Load() != 1 {
				t.Fatalf("err=%v notes=%+v closed=%d", err, notes, closed.Load())
			}
		})
	}
}

func TestNativePopulateNeverFollowsRedirect(t *testing.T) {
	for _, cross := range []bool{false, true} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, custom := range []bool{false, true} {
				t.Run(fmt.Sprintf("cross=%t/status=%d/accepting-callback=%t", cross, status, custom), func(t *testing.T) {
					var sourceHits, targetHits, callbackHits atomic.Int32
					target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1); io.WriteString(w, failureQR) }))
					defer target.Close()
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/target" {
							targetHits.Add(1)
							io.WriteString(w, failureQR)
							return
						}
						sourceHits.Add(1)
						if r.Method != "POST" {
							t.Error("initial method changed")
						}
						location := "/target"
						if cross {
							location = target.URL
						}
						w.Header().Set("Location", location)
						w.WriteHeader(status)
					}))
					defer srv.Close()
					client := srv.Client()
					if custom {
						client.CheckRedirect = func(*http.Request, []*http.Request) error { callbackHits.Add(1); return nil }
					}
					original := reflect.ValueOf(client.CheckRedirect).Pointer()
					var notes []PopulateFailure
					qr, _, err := NewNativePopulatorWithFailureObserver(client, srv.URL, func(n PopulateFailure) { notes = append(notes, n) }).Populate(context.Background(), []byte(failurePackage), failureContext)
					if err != errPopulateUpstream || qr != nil || sourceHits.Load() != 1 || targetHits.Load() != 0 || len(notes) != 1 || notes[0] != (PopulateFailure{Stage: "http_status", Reason: "non_2xx", Status: status}) {
						t.Errorf("err=%v source=%d target=%d notes=%+v", err, sourceHits.Load(), targetHits.Load(), notes)
					}
					if reflect.ValueOf(client.CheckRedirect).Pointer() != original {
						t.Error("caller redirect callback mutated")
					}
					if custom && callbackHits.Load() != 1 {
						t.Error("caller callback intent not consulted")
					}
				})
			}
		}
	}
}

func TestNativePopulateLogicalPatientAndResponseSubject(t *testing.T) {
	for _, row := range []struct {
		name, logical, scoped, returned string
		want                            error
		calls                           int32
	}{
		{"both-empty", "", "", "", errNoClinicalContext, 0}, {"missing-logical", "", "Patient/store", "Patient/store", errNoClinicalContext, 0},
		{"fallback", "Patient/logical", "", "Patient/logical", nil, 1}, {"scoped", "Patient/logical", "Patient/store", "Patient/store", nil, 1},
		{"foreign", "Patient/logical", "Patient/store", "Patient/foreign", errPopulateForeignSubject, 1}, {"missing-subject", "Patient/logical", "Patient/store", "", errPopulateForeignSubject, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if row.returned == "" {
					io.WriteString(w, `{"resourceType":"QuestionnaireResponse"}`)
				} else {
					fmt.Fprintf(w, `{"resourceType":"QuestionnaireResponse","subject":{"reference":%q}}`, row.returned)
				}
			}))
			defer srv.Close()
			var notes []PopulateFailure
			qr, _, err := NewNativePopulatorWithFailureObserver(srv.Client(), srv.URL, func(n PopulateFailure) { notes = append(notes, n) }).Populate(context.Background(), []byte(failurePackage), PopulateContext{PatientRef: row.logical, SubjectFHIRRef: row.scoped})
			if !errors.Is(err, row.want) || calls.Load() != row.calls || len(notes) != 0 {
				t.Fatalf("err=%v calls=%d notes=%+v", err, calls.Load(), notes)
			}
			if err == nil {
				if subject, e := questionnaireResponseSubject(qr); e != nil || subject != row.logical {
					t.Fatalf("subject=%q err=%v", subject, e)
				}
			} else if qr != nil {
				t.Fatal("refusal returned QR")
			}
		})
	}
}

func TestNativePopulateCallerClientConcurrent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", 302)
			return
		}
		if r.URL.Path == "/populate" {
			if c, e := r.Cookie("session"); e != nil || c.Value != "retained" {
				t.Error("caller jar lost")
			}
			io.WriteString(w, failureQR)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	jar, _ := cookiejar.New(nil)
	client.Jar = jar
	client.Timeout = 3 * time.Second
	req, _ := http.NewRequest("GET", srv.URL, nil)
	jar.SetCookies(req.URL, []*http.Cookie{{Name: "session", Value: "retained"}})
	transport := client.Transport
	pop := NewNativePopulator(client, srv.URL+"/populate")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, e := pop.Populate(context.Background(), []byte(failurePackage), failureContext); e != nil {
				t.Error(e)
			}
			resp, e := client.Get(srv.URL + "/redirect")
			if e != nil {
				t.Error(e)
				return
			}
			resp.Body.Close()
			if resp.Request.URL.Path != "/target" {
				t.Error("population changed caller redirect behavior")
			}
		}()
	}
	wg.Wait()
	if client.Transport != transport || client.Jar != jar || client.Timeout != 3*time.Second || client.CheckRedirect != nil || hits.Load() != 24 {
		t.Fatalf("client configuration changed or requests lost: hits=%d", hits.Load())
	}
}
