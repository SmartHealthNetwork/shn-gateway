package lanequalify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordedRequest is the one exchange of a recording that asked method path.
func recordedRequest(t *testing.T, exchanges []recordedExchange, method, path string) recordedExchange {
	t.Helper()
	var found []recordedExchange
	for _, ex := range exchanges {
		if ex.method == method && ex.path == path {
			found = append(found, ex)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d recorded exchanges for %s %s, want 1", len(found), method, path)
	}
	return found[0]
}

// Each lane's real answer to GET /fhir/metadata is a metadata answer.
func TestMetadataAcceptsTheRecordedCapabilityStatement(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			srv, _ := replayRecording(t, "lane-"+line+"-metadata")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := metadata(ctx, httpClient(), srv.URL+"/fhir/metadata"); err != nil {
				t.Fatalf("recorded CapabilityStatement refused: %v", err)
			}
		})
	}
}

// Rejection rows, from what each lane really answered to requests it refuses.
// A $validate the lane refused (an unknown resource type, 404; a body it could
// not parse, 400) fails its row for the status whatever the row expected, and
// the failure carries the lane's refusal. A path that is not the FHIR root's
// metadata answers 404 and is not metadata.
func TestRecordedRefusalsAreNotVerdicts(t *testing.T) {
	for _, line := range []string{"2.0", "2.1", "2.2"} {
		t.Run(line, func(t *testing.T) {
			exchanges := laneRecording(t, "lane-"+line+"-errors")
			srv, _ := replayRecording(t, "lane-"+line+"-errors")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, tc := range []struct {
				resourceType, refusal string
				status                int
			}{
				{"NotAResourceType", "HAPI-0302: Unknown resource type 'NotAResourceType'", http.StatusNotFound},
				{"Patient", "HAPI-0450: Failed to parse request body", http.StatusBadRequest},
			} {
				ex := recordedRequest(t, exchanges, http.MethodPost, "/fhir/"+tc.resourceType+"/$validate")
				if ex.status != tc.status || !strings.Contains(string(ex.answer), tc.refusal) {
					t.Fatalf("recorded %s answer is %d %.80s, want %d %s", tc.resourceType, ex.status, ex.answer, tc.status, tc.refusal)
				}
				// The row asks exactly the recorded request: that path, no
				// profile query, the recorded body.
				row := warmup{identity: "recorded-refusal", resourceType: tc.resourceType, mode: verdictPositive, line: line}
				err := submitValidation(ctx, httpClient(), srv.URL+"/fhir", row, ex.body)
				var oe *outcomeError
				if err == nil || err.Error() != "wrong response status" || !errors.As(err, &oe) || !strings.Contains(oe.excerpt, tc.refusal) {
					t.Fatalf("recorded %d refusal: %v, want \"wrong response status\" carrying %q", tc.status, err, tc.refusal)
				}
				for _, mode := range []verdictMode{verdictInitialize, verdictPrime, verdictPositive, verdictNegative, verdictSupportNegative, verdictExplicitProfileNegative} {
					row.mode = mode
					if err := assertVerdict(row, ex.status, ex.answer); err == nil || err.Error() != "wrong response status" {
						t.Fatalf("mode %d: recorded %d refusal = %v, want \"wrong response status\"", mode, tc.status, err)
					}
				}
			}
			notRoot := recordedRequest(t, exchanges, http.MethodGet, "/fhir/not-a-path/at-all")
			if notRoot.status != http.StatusNotFound {
				t.Fatalf("recorded unknown path answered %d, want 404", notRoot.status)
			}
			if err := metadata(ctx, httpClient(), srv.URL+notRoot.path); err == nil || err.Error() != "metadata unavailable" {
				t.Fatalf("metadata at a path that is not the FHIR root = %v, want \"metadata unavailable\"", err)
			}
		})
	}
}

// The metadata answer's size bound and completeness. The recorded answers do
// not carry their size (their scrub replaces the resource listing), so these
// rows are written by hand.
func TestMetadataRequiresCompleteBoundedBody(t *testing.T) {
	for _, mode := range []string{"incomplete", "oversize", "hang", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "incomplete":
					w.Header().Set("Content-Length", "500")
					fmt.Fprint(w, "short")
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", (4<<20)+1))
				case "hang":
					<-r.Context().Done()
				case "redirect":
					w.Header().Set("Location", "/metadata")
					w.WriteHeader(302)
				}
			}))
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if metadata(ctx, httpClient(), s.URL) == nil {
				t.Fatal("metadata accepted incomplete response")
			}
		})
	}
}

func TestMetadataIndependentSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		wantError bool
	}{{"at limit", 4 << 20, false}, {"over limit", (4 << 20) + 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat(" ", tc.size)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := metadata(ctx, httpClient(), server.URL)
			if (err != nil) != tc.wantError {
				t.Fatalf("metadata bytes=%d error=%v wantError=%v", tc.size, err, tc.wantError)
			}
		})
	}
}
