package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckTerminalMarkerDiagnosticWithoutPublicService(t *testing.T) {
	path := markerPath(t)
	st := initialState(sameJVM())
	st.State = "failed"
	st.Row = readinessIdentities("2.2")[0]
	st.Failure = "wrong response status"
	if err := writeState(path, st); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old }()
	if check("http://127.0.0.1:1/fhir", envLine("2.2"), path, time.Second, sameJVM) != 1 {
		t.Fatal("terminal marker accepted")
	}
	f.Seek(0, 0)
	raw, _ := io.ReadAll(f)
	if !strings.Contains(string(raw), "warm-up failed: "+st.Row+" wrong response status") {
		t.Fatalf("terminal failure masked: %s", raw)
	}
}
func TestCheckRereadsMarkerAfterPublicMetadata(t *testing.T) {
	for _, mutation := range []string{"failed", "stale", "incomplete"} {
		t.Run(mutation, func(t *testing.T) {
			path := markerPath(t)
			st := initialState(sameJVM())
			st.State = "ready"
			st.Warm = readinessIdentities("2.2")
			if err := writeState(path, st); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mutation {
				case "failed":
					st.State = "failed"
					st.Failure = "child exited"
				case "stale":
					st.Key = otherJVM()
				case "incomplete":
					st.Warm = st.Warm[:1]
				}
				if err := writeState(path, st); err != nil {
					t.Error(err)
				}
				io.WriteString(w, `{"resourceType":"CapabilityStatement"}`)
			}))
			defer srv.Close()
			if check(srv.URL+"/fhir", envLine("2.2"), path, time.Second, sameJVM) != 1 {
				t.Fatal("accepted stale pre-request snapshot")
			}
		})
	}
}
