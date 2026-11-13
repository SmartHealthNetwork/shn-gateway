package cdexroute

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider's Task endpoint a read is made below: absolute http or https
// with a host, no query or fragment, no dot segment, and a last segment that
// is "Task" exactly. Each way it fails is its own error.
func TestTaskReadURL(t *testing.T) {
	dotDot := strings.Repeat(".", 2)
	for raw, want := range map[string]error{
		"https://ehr.example/fhir/Task":             nil,
		"https://ehr.example/Task":                  nil,
		"http://ehr.example:8080/r4/fhir/Task":      nil,
		"https://ehr.example/a%2Fb/Task":            nil,
		"https://ehr.example/.../Task":              nil,
		"https://ehr.example/fhir/Task?x=1":         ErrTaskURLQueryOrFragment,
		"https://ehr.example/fhir/Task?":            ErrTaskURLQueryOrFragment,
		"https://ehr.example/fhir/Task#":            ErrTaskURLQueryOrFragment,
		"https://ehr.example/fhir/Task#x":           ErrTaskURLQueryOrFragment,
		"https://ehr.example/fhir/Task/":            ErrTaskURLNotTask,
		"https://ehr.example/fhir/task":             ErrTaskURLNotTask,
		"https://ehr.example/fhir/Tasks":            ErrTaskURLNotTask,
		"https://ehr.example/fhir/Ta%73k":           ErrTaskURLNotTask,
		"https://ehr.example":                       ErrTaskURLNotTask,
		"https://ehr.example/fhir/Task/1":           ErrTaskURLNotTask,
		"https://ehr.example/./Task":                ErrTaskURLDotSegment,
		"https://ehr.example/x/" + dotDot + "/Task": ErrTaskURLDotSegment,
		"https://ehr.example/%2E%2E/Task":           ErrTaskURLDotSegment,
		"https://ehr.example/%2e/Task":              ErrTaskURLDotSegment,
		"ftp://ehr.example/fhir/Task":               ErrTaskURLNotAbsolute,
		"/fhir/Task":                                ErrTaskURLNotAbsolute,
		"https:///fhir/Task":                        ErrTaskURLNotAbsolute,
		"https:ehr.example/Task":                    ErrTaskURLNotAbsolute,
		"https://ehr example/Task":                  ErrTaskURLNotAbsolute,
		"":                                          ErrTaskURLNotAbsolute,
	} {
		_, err := TaskReadURL(raw)
		if !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("TaskReadURL(%q) = %v, want %v", raw, err, want)
		}
	}
}

// A read's target is the endpoint's path, escaped as given, with the read's
// tail appended, and the query exactly as given; the request line a client
// sends for it is that, byte for byte, and the base the endpoint serves its
// Tasks under is its path less "Task".
func TestReadTarget(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotURI = r.RequestURI }))
	defer srv.Close()
	for _, c := range []struct {
		endpoint, tail, query string
		hasQuery              bool
		want, base            string
	}{
		{"/fhir/Task", "t-1", "", false, "/fhir/Task/t-1", "/fhir"},
		{"/fhir/Task", "t-1/_history/2", "", false, "/fhir/Task/t-1/_history/2", "/fhir"},
		{"/fhir/Task", "t-1", "", true, "/fhir/Task/t-1?", "/fhir"},
		{"/fhir/Task", "t-1", "a=x|y&b=c%2Fd&c=%zz", true, "/fhir/Task/t-1?a=x|y&b=c%2Fd&c=%zz", "/fhir"},
		{"/a%2Fb/Task", "t-1", "", false, "/a%2Fb/Task/t-1", "/a%2Fb"},
		{"/Task", "...", "", false, "/Task/...", ""},
	} {
		u, err := TaskReadURL(srv.URL + c.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		target, err := ReadTarget(u, c.tail, c.query, c.hasQuery)
		if err != nil {
			t.Fatal(err)
		}
		if target.RequestURI() != c.want {
			t.Errorf("%s + %s: request URI %q, want %q", c.endpoint, c.tail, target.RequestURI(), c.want)
		}
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.URL = target
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotURI != c.want {
			t.Errorf("%s + %s: the server got %q, want %q", c.endpoint, c.tail, gotURI, c.want)
		}
		if got := TaskReadBase(u); got != srv.URL+c.base {
			t.Errorf("TaskReadBase(%s) = %q, want %q", c.endpoint, got, srv.URL+c.base)
		}
		if u.String() != srv.URL+c.endpoint {
			t.Errorf("ReadTarget changed the endpoint: %s", u)
		}
	}
}

// A tail that cannot be read back as a path is an error, never a target that
// could name the Task endpoint or its collection.
func TestReadTargetFailsClosed(t *testing.T) {
	u, err := TaskReadURL("https://ehr.example/fhir/Task")
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{"%zz", "t-1%", "%2"} {
		if got, err := ReadTarget(u, tail, "", false); !errors.Is(err, ErrReadTarget) || got != nil {
			t.Errorf("ReadTarget(%q) = %v, %v; want ErrReadTarget and no URL", tail, got, err)
		}
	}
}
