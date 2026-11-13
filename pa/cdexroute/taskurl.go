package cdexroute

import (
	"errors"
	"net/url"
	"strings"
)

// TaskSegment is the last path segment of a provider's Task endpoint whose
// gateway reads its Tasks for payers: the FHIR resource type, below which a
// read names "Task/<id>" and a vread "Task/<id>/_history/<vid>".
const TaskSegment = "Task"

// The ways a provider's Task endpoint is not one its gateway can read a Task
// below (TaskReadURL).
var (
	// ErrTaskURLNotAbsolute: the endpoint is not an absolute http or https
	// URL with a host.
	ErrTaskURLNotAbsolute = errors.New("cdexroute: Task endpoint is not an absolute http or https URL")
	// ErrTaskURLQueryOrFragment: the endpoint carries a query or a fragment
	// ("?" or "#" at all: a bare one parses as empty, and a path appended
	// after it would not be a path).
	ErrTaskURLQueryOrFragment = errors.New("cdexroute: Task endpoint carries a query or fragment")
	// ErrTaskURLDotSegment: the endpoint's path holds a "." or ".." segment,
	// spelled or percent-escaped, which a server may resolve to another path.
	ErrTaskURLDotSegment = errors.New(`cdexroute: Task endpoint's path has a "." or ".." segment`)
	// ErrTaskURLNotTask: the endpoint's path does not end in the segment
	// "Task", exactly (a trailing slash ends it in an empty segment).
	ErrTaskURLNotTask = errors.New(`cdexroute: Task endpoint's path does not end in a "Task" segment`)
)

// TaskReadURL reads raw, a provider's own Task endpoint, as the base its
// gateway reads a payer's Task below: an absolute http or https URL with a
// host, no query and no fragment, whose path has no "." or ".." segment and
// ends in the segment "Task" (TaskSegment), exactly. A read is that URL's path
// with "/<id>" or "/<id>/_history/<vid>" appended (ReadTarget), so the shape
// is decided once, here, where a gateway's boot check and a hosted tenant's
// configuration check both read it. The errors are this file's, each its own
// way the endpoint fails.
func TaskReadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Opaque != "" {
		return nil, ErrTaskURLNotAbsolute
	}
	if strings.ContainsAny(raw, "?#") {
		return nil, ErrTaskURLQueryOrFragment
	}
	segs := strings.Split(u.EscapedPath(), "/")
	for _, seg := range segs {
		if v, err := url.PathUnescape(seg); err == nil && (v == "." || v == "..") {
			return nil, ErrTaskURLDotSegment
		}
	}
	if segs[len(segs)-1] != TaskSegment {
		return nil, ErrTaskURLNotTask
	}
	return u, nil
}

// ReadTarget is the URL a read below the Task endpoint u (as TaskReadURL
// returns it) goes to: u's path, escaped as it was given, with "/" and tail
// appended, and rawQuery as the query exactly as given (hasQuery keeps an empty
// query, "Task/1?", as sent). tail is what a read path names after "Task/":
// "<id>" or "<id>/_history/<vid>", FHIR ids whose characters need no escaping.
// Nothing is parsed again, so the query reaches the provider's system byte for
// byte. A path that cannot be read back (ErrReadTarget) is an error, never a
// URL whose path could name the Task endpoint itself.
func ReadTarget(u *url.URL, tail, rawQuery string, hasQuery bool) (*url.URL, error) {
	t := *u
	escaped := u.EscapedPath() + "/" + tail
	p, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, ErrReadTarget
	}
	t.Path, t.RawPath = p, escaped
	t.RawQuery, t.ForceQuery = rawQuery, hasQuery && rawQuery == ""
	t.Fragment, t.RawFragment = "", ""
	return &t, nil
}

// ErrReadTarget is a read's path that cannot be read back as a path below the
// Task endpoint (ReadTarget).
var ErrReadTarget = errors.New("cdexroute: the read's path below the Task endpoint cannot be read back")

// TaskReadBase is the FHIR base a Task endpoint u (as TaskReadURL returns it)
// serves its Tasks under: u with its last segment, "Task", removed, as an
// absolute URL with no ending slash.
func TaskReadBase(u *url.URL) string {
	t := *u
	escaped := strings.TrimSuffix(u.EscapedPath(), "/"+TaskSegment)
	t.RawPath = escaped
	if p, err := url.PathUnescape(escaped); err == nil {
		t.Path = p
	}
	return t.String()
}
