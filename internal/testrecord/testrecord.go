// Package testrecord replays recorded real answers of external APIs in tests.
//
// A fake written by hand answers what its author imagined, and a fake more
// lenient than the real service is green in CI and red live. A recording is
// what the service actually answered. This package serves one recording as a
// CLI command runner (Cmd) or an HTTP server (Server). Where the AWS SDK is a
// dependency, awsconfig.go beside this file adds an SDK configuration aimed at
// that server (AWSConfig), so the SDK's own deserializers and typed errors run
// against the recorded wire bytes.
//
// Replay is strict:
//   - a CLI request matches only an identical recorded argv; an HTTP request
//     matches only the recorded method, decoded path, whole query (an
//     unparseable query never matches), listed headers (each sent once with
//     the recorded value; one recorded as "" must be absent) and body (a JSON
//     body must be one JSON value, equal to the recorded one; no body matches
//     only a request with no body); a miss fails the test, names the closest
//     recorded request, and gets no plausible answer;
//   - an AWS JSON-protocol request (POST / with a JSON body) must record its
//     X-Amz-Target, since the body alone does not say which operation it is;
//   - exchanges recorded for the same request are served in recorded order,
//     and only the last of them may say "repeat": true to serve again;
//   - every exchange must be served by the end of the test, unless the test
//     calls Subset and says why.
//
// A recording lives under a testdata/recordings directory with a README.md
// beside it that names it and states its capture and scrub. Load refuses a
// recording anywhere else, one without a scrub statement or README, and one
// that still carries:
//   - a run of exactly twelve digits (an AWS account id's shape) other than
//     the documentation account 111122223333;
//   - an IPv4 address, bare or in an ip-a-b-c-d host name, outside
//     192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 and loopback;
//   - an IPv6 address outside 2001:db8::/32 and loopback.
//
// A value that only looks like one of these (an OID, a four-part version) is
// listed in the recording's "allow" with a reason; an allowed value excuses
// only an exactly equal match, never a longer value that contains it. A
// twelve-digit run inside a decimal number (a mantissa, a metric value), a
// longer token (a hex task id) or ending a UUID is not an account id. Log message bodies, and
// identifiers inside encoded payloads (base64, a JWT), cannot be recognised
// mechanically: the capture replaces them and the scrub statement says so.
// Every committed file under a testdata/recordings directory must pass these
// checks, whether or not a test loads it.
//
// A value in an answer too large or too specific to commit, which no test
// reads, is replaced at capture and declared in scrubbed: Parse checks that the
// declared place holds exactly the declared replacement.
//
// A state no capture can produce on demand (an alarm firing, a failed task)
// is a derived recording: it names its source capture in derivedFrom and the
// values it replaces in changed, and Parse verifies it equals the source apart
// from exactly those values, so only a value is ever authored, never a shape.
// An exchange that could not be captured at all (a write call the capture
// window did not allow) says why in handWritten.
//
// Pagination tokens (NextToken, nextForwardToken and the like, in an answer
// or sent back with --next-token) must be "<pagination token N>" markers.
//
// Recorded credentials are refused outright: an Authorization, security
// token, cookie or API key header with a value, a bearer token, or an AWS
// access key id.
//
// Each module that replays recordings has its own copy of this file, kept
// identical to the others and alone in its package; it is internal to its
// module and imported only by tests, never a published helper. awsconfig.go
// has no copy where the AWS SDK is not a dependency.
package testrecord

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// T is the part of *testing.T the helper uses.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Recording is one captured scenario.
type Recording struct {
	Captured  Captured   `json:"captured"`
	Scrub     []string   `json:"scrub"`
	Allow     []Allowed  `json:"allow,omitempty"`
	Exchanges []Exchange `json:"exchanges"`

	// DerivedFrom names a captured recording in the same directory that this
	// one repeats with only the values in Changed replaced: a state a capture
	// cannot produce on demand (an alarm firing, a failed task), in a shape a
	// capture did produce. Changed maps a JSON pointer into the recording
	// document to the value placed there. Parse verifies the derivation.
	DerivedFrom string                     `json:"derivedFrom,omitempty"`
	Changed     map[string]json.RawMessage `json:"changed,omitempty"`

	// Scrubbed lists values in the recorded answers replaced at capture:
	// content too large or too specific to commit that no test reads (a
	// server's full capability listing). Parse verifies each names a place
	// inside a recorded answer's body that holds exactly its replacement.
	Scrubbed []Scrubbed `json:"scrubbed,omitempty"`

	path   string
	t      T
	mu     sync.Mutex
	served []int
	subset bool
}

// Captured says where, when and with what the recording was made.
type Captured struct {
	When   string `json:"when"`
	Env    string `json:"env"`
	By     string `json:"by"`
	Tool   string `json:"tool"`
	Region string `json:"region,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// Scrubbed is one value replaced at capture: where (a JSON pointer into the
// recording, inside /exchanges/<n>/response/body/), what now stands there,
// why, and the size and SHA-256 of the whole answer body as captured (after
// any content decoding), as provenance. The original is not committed, so
// Parse checks the declaration's consistency, not the removed bytes.
type Scrubbed struct {
	Pointer        string          `json:"pointer"`
	Replacement    json.RawMessage `json:"replacement"`
	Reason         string          `json:"reason"`
	OriginalBytes  int             `json:"originalBytes"`
	OriginalSHA256 string          `json:"originalSha256"`
}

// Allowed is a literal the scrub checks would refuse but that is not an
// account id or an address, with the reason it is kept.
type Allowed struct {
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// Exchange is one request and the answer it got.
type Exchange struct {
	Note     string   `json:"note,omitempty"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
	// HandWritten says why this one exchange could not be captured (a write
	// call, a state no read can produce) and what replaces it. An exchange
	// without it is a capture.
	HandWritten string `json:"handWritten,omitempty"`
	// Repeat serves this answer again for every later identical request,
	// once the exchanges recorded before it are used up. It models a
	// steady state the capture observed, never a guess.
	Repeat bool `json:"repeat,omitempty"`
}

// Request identifies a recorded request: a CLI argv (command name first), or
// an HTTP method and path with an optional query, headers and body. A JSON
// Body matches a JSON-equal request body; BodyText matches byte for byte.
type Request struct {
	Argv     []string            `json:"argv,omitempty"`
	Method   string              `json:"method,omitempty"`
	Path     string              `json:"path,omitempty"`
	Query    map[string][]string `json:"query,omitempty"`
	Headers  map[string]string   `json:"headers,omitempty"`
	Body     json.RawMessage     `json:"body,omitempty"`
	BodyText string              `json:"bodyText,omitempty"`
}

// Response is the recorded answer: Status, Headers and Body or BodyText for
// HTTP; Stdout or StdoutText, Stderr and Exit for a CLI.
type Response struct {
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BodyText   string            `json:"bodyText,omitempty"`
	Stdout     json.RawMessage   `json:"stdout,omitempty"`
	StdoutText string            `json:"stdoutText,omitempty"`
	Stderr     string            `json:"stderr,omitempty"`
	Exit       int               `json:"exit,omitempty"`
}

// DocumentationAccountID is the only AWS account id a recording may carry.
const DocumentationAccountID = "111122223333"

// DocumentationRDSID stands for the account-specific host id in an RDS
// endpoint (<instance>.<id>.<region>.rds.amazonaws.com), which public DNS
// resolves; a recording carries this one instead.
const DocumentationRDSID = "abcdefghijkl"

// scrubPassed holds the SHA-256 of each recording whose scrub check passed in
// this process. The check reads nothing but the bytes (the allow list is
// decoded from them), so a recording loaded again, as many tests load the same
// lane, skips the repeat scan; a changed byte is a new digest and is checked in
// full, and a failure is never remembered.
var scrubPassed sync.Map

func checkScrubbedOnce(raw []byte, allow []Allowed) error {
	sum := sha256.Sum256(raw)
	if _, ok := scrubPassed.Load(sum); ok {
		return nil
	}
	if err := CheckScrubbed(raw, allow); err != nil {
		return err
	}
	scrubPassed.Store(sum, struct{}{})
	return nil
}

var (
	digitRun     = regexp.MustCompile(`[0-9]+`)
	dottedRun    = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)
	ipv4Host     = regexp.MustCompile(`(?:ip|ec2)-([0-9]{1,3})-([0-9]{1,3})-([0-9]{1,3})-([0-9]{1,3})`)
	hexColonRun  = regexp.MustCompile(`[0-9A-Fa-f:]+`)
	uuidHead     = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-$`)
	bearerToken  = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	accessKeyID  = regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)
	rdsHost      = regexp.MustCompile(`(?i)[a-z0-9-]+\.(?:cluster-(?:ro-|custom-)?|proxy-)?([a-z0-9]{12})\.[a-z0-9-]+\.rds\.amazonaws\.com`)
	secretHeader = map[string]bool{"authorization": true, "x-amz-security-token": true, "cookie": true, "set-cookie": true, "x-api-key": true}
	allowedNets  = mustNets("192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "127.0.0.0/8", "2001:db8::/32", "::1/128")
)

func mustNets(cidrs ...string) []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}

// Load reads and checks one recording, and at the end of the test fails it
// if an exchange was never served (see Subset).
func Load(t T, path string) *Recording {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testrecord: %v", err)
		return nil
	}
	r, err := Parse(path, raw)
	if err != nil {
		t.Fatalf("testrecord: %s: %v", path, err)
		return nil
	}
	r.t = t
	t.Cleanup(r.checkServed)
	return r
}

// InRecordings reports whether path is under a testdata/recordings directory.
func InRecordings(path string) bool {
	return strings.Contains(filepath.ToSlash(filepath.Clean(path)), "testdata/recordings/")
}

// Parse checks a recording's shape, scrub and README without serving it.
func Parse(path string, raw []byte) (*Recording, error) {
	if !InRecordings(path) {
		return nil, fmt.Errorf("a recording lives under a testdata/recordings directory")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	r := &Recording{path: path}
	if err := dec.Decode(r); err != nil {
		return nil, err
	}
	switch {
	case r.Captured.When == "" || r.Captured.Env == "" || r.Captured.By == "" || r.Captured.Tool == "":
		return nil, fmt.Errorf("captured must say when, env, by (a role) and tool")
	case len(r.Scrub) == 0:
		return nil, fmt.Errorf("no scrub statement: say what was replaced before commit")
	case len(r.Exchanges) == 0:
		return nil, fmt.Errorf("no exchanges")
	}
	for i, a := range r.Allow {
		if a.Value == "" || a.Reason == "" {
			return nil, fmt.Errorf("allow %d: an allowed value needs the value and the reason it is kept", i)
		}
	}
	lastOf := map[string]int{}
	for i, ex := range r.Exchanges {
		if err := checkExchange(ex); err != nil {
			return nil, fmt.Errorf("exchange %d: %v", i, err)
		}
		lastOf[requestKey(ex.Request)] = i
	}
	for i, ex := range r.Exchanges {
		if ex.Repeat && lastOf[requestKey(ex.Request)] != i {
			return nil, fmt.Errorf("exchange %d: only the last exchange recorded for a request may repeat", i)
		}
	}
	if err := checkScrubbedOnce(raw, r.Allow); err != nil {
		return nil, err
	}
	if err := checkPaginationTokens(r); err != nil {
		return nil, err
	}
	readme, err := os.ReadFile(filepath.Join(filepath.Dir(path), "README.md"))
	if err != nil {
		return nil, fmt.Errorf("no README.md beside the recording to state its capture and scrub: %v", err)
	}
	named := regexp.MustCompile(`(?:^|[^A-Za-z0-9._-])` + regexp.QuoteMeta(filepath.Base(path)) + `(?:$|[^A-Za-z0-9._-])`)
	if !named.Match(readme) {
		return nil, fmt.Errorf("README.md beside the recording does not name %s", filepath.Base(path))
	}
	if err := r.checkDerived(raw); err != nil {
		return nil, err
	}
	if err := r.checkReplaced(raw); err != nil {
		return nil, err
	}
	r.served = make([]int, len(r.Exchanges))
	return r, nil
}

var (
	answerBodyPointer = regexp.MustCompile(`^/exchanges/[0-9]+/response/body/.`)
	sha256Hex         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// checkReplaced verifies the scrubbed declarations: each points inside a
// recorded answer's body, at a value equal to its replacement, overlaps no
// other, and gives a reason and the captured answer's size and digest.
func (r *Recording) checkReplaced(raw []byte) error {
	if len(r.Scrubbed) == 0 {
		return nil
	}
	doc, err := decodeJSON(raw)
	if err != nil {
		return err
	}
	for i, s := range r.Scrubbed {
		switch {
		case !answerBodyPointer.MatchString(s.Pointer):
			return fmt.Errorf("scrubbed %d: %q is not inside a recorded answer's body (/exchanges/<n>/response/body/...)", i, s.Pointer)
		case s.Reason == "":
			return fmt.Errorf("scrubbed %s: say why the value was replaced", s.Pointer)
		case s.OriginalBytes <= 0 || !sha256Hex.MatchString(s.OriginalSHA256):
			return fmt.Errorf("scrubbed %s: give the captured answer's size and SHA-256", s.Pointer)
		}
		for j, o := range r.Scrubbed {
			if j != i && (o.Pointer == s.Pointer || strings.HasPrefix(s.Pointer, o.Pointer+"/") || strings.HasPrefix(o.Pointer, s.Pointer+"/")) {
				return fmt.Errorf("scrubbed %s overlaps %s", s.Pointer, o.Pointer)
			}
		}
		want, err := decodeJSON(s.Replacement)
		if err != nil {
			return fmt.Errorf("scrubbed %s: replacement: %v", s.Pointer, err)
		}
		got, err := pointerSet(doc, s.Pointer, nil)
		if err != nil {
			return fmt.Errorf("scrubbed %s is not a place in the recorded answer: %v", s.Pointer, err)
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("scrubbed %s holds a value other than its replacement", s.Pointer)
		}
	}
	return nil
}

// checkDerived verifies a derived recording. Its source is a recording in the
// same directory that is not itself derived. Every changed pointer lies in an
// answer (under /exchanges/<n>/response/), exists in the source with a
// different value, and overlaps no other. Every object a changed value holds
// has a shape (key set) some capture beside it already has. The two documents
// are equal once those values are placed in the source.
func (r *Recording) checkDerived(raw []byte) error {
	if r.DerivedFrom == "" {
		if len(r.Changed) > 0 {
			return fmt.Errorf("changed values without derivedFrom")
		}
		return nil
	}
	if len(r.Changed) == 0 {
		return fmt.Errorf("derivedFrom %s changes nothing; use the source itself", r.DerivedFrom)
	}
	if filepath.Base(r.DerivedFrom) != r.DerivedFrom {
		return fmt.Errorf("derivedFrom %s must name a recording in the same directory", r.DerivedFrom)
	}
	srcPath := filepath.Join(filepath.Dir(r.path), r.DerivedFrom)
	srcRaw, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("derivedFrom: %v", err)
	}
	// Refuse a derived source before parsing it, so a cycle cannot recurse.
	var peek struct {
		DerivedFrom string `json:"derivedFrom"`
	}
	if err := json.Unmarshal(srcRaw, &peek); err != nil {
		return fmt.Errorf("derivedFrom %s: %v", r.DerivedFrom, err)
	}
	if peek.DerivedFrom != "" {
		return fmt.Errorf("derivedFrom %s is itself derived; derive from the recording it derives from", r.DerivedFrom)
	}
	if _, err := Parse(srcPath, srcRaw); err != nil {
		return fmt.Errorf("derivedFrom %s: %v", r.DerivedFrom, err)
	}
	var ptrs []string
	for ptr := range r.Changed {
		if !responsePointer.MatchString(ptr) {
			return fmt.Errorf("changed %s: a derivation changes an answer, under /exchanges/<n>/response/", ptr)
		}
		ptrs = append(ptrs, ptr)
	}
	sort.Strings(ptrs)
	for i := 1; i < len(ptrs); i++ {
		if strings.HasPrefix(ptrs[i], ptrs[i-1]+"/") {
			return fmt.Errorf("changed %s and %s overlap", ptrs[i-1], ptrs[i])
		}
	}
	shapes, err := captureShapes(filepath.Dir(r.path), srcRaw)
	if err != nil {
		return err
	}
	want, err := decodeJSON(srcRaw)
	if err != nil {
		return err
	}
	got, err := decodeJSON(raw)
	if err != nil {
		return err
	}
	gotDoc, _ := got.(map[string]any)
	delete(gotDoc, "derivedFrom")
	delete(gotDoc, "changed")
	for _, ptr := range ptrs {
		v, err := decodeJSON(r.Changed[ptr])
		if err != nil {
			return fmt.Errorf("changed %s: %v", ptr, err)
		}
		if missing := unseenShape(v, shapes); missing != "" {
			return fmt.Errorf("changed %s: an object with keys %s appears in no capture beside this recording; a derivation authors values, not shapes", ptr, missing)
		}
		old, err := pointerSet(want, ptr, v)
		if err != nil {
			return fmt.Errorf("changed %s: %v", ptr, err)
		}
		if reflect.DeepEqual(old, v) {
			return fmt.Errorf("changed %s: the source already holds that value", ptr)
		}
	}
	if !reflect.DeepEqual(want, gotDoc) {
		return fmt.Errorf("differs from %s beyond its changed values", r.DerivedFrom)
	}
	return nil
}

var responsePointer = regexp.MustCompile(`^/exchanges/(0|[1-9][0-9]*)/response/`)

// captureShapes collects the key set of every JSON object in the source and in
// every capture (a recording that is not derived) in the same directory.
func captureShapes(dir string, srcRaw []byte) (map[string]bool, error) {
	shapes := map[string]bool{}
	// Only captured answers count: a hand-written exchange, or a derived
	// recording, has no shape a service was seen to give.
	add := func(raw []byte) {
		v, err := decodeJSON(raw)
		doc, _ := v.(map[string]any)
		if err != nil || doc["derivedFrom"] != nil {
			return
		}
		// A value replaced at capture was never seen from the service.
		scrubbed, _ := doc["scrubbed"].([]any)
		for _, s := range scrubbed {
			if m, ok := s.(map[string]any); ok {
				if ptr, ok := m["pointer"].(string); ok {
					_, _ = pointerSet(doc, ptr, nil)
				}
			}
		}
		exchanges, _ := doc["exchanges"].([]any)
		for _, e := range exchanges {
			ex, _ := e.(map[string]any)
			if _, hand := ex["handWritten"]; hand {
				continue
			}
			collectShapes(ex["response"], shapes)
		}
	}
	add(srcRaw)
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		add(raw)
	}
	return shapes, nil
}

func shapeKey(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func collectShapes(v any, shapes map[string]bool) {
	switch n := v.(type) {
	case map[string]any:
		shapes[shapeKey(n)] = true
		for _, c := range n {
			collectShapes(c, shapes)
		}
	case []any:
		for _, c := range n {
			collectShapes(c, shapes)
		}
	}
}

// unseenShape returns the key set of the first object in v that no capture
// has, or "".
func unseenShape(v any, shapes map[string]bool) string {
	switch n := v.(type) {
	case map[string]any:
		if k := shapeKey(n); !shapes[k] {
			return "{" + k + "}"
		}
		for _, c := range n {
			if m := unseenShape(c, shapes); m != "" {
				return m
			}
		}
	case []any:
		for _, c := range n {
			if m := unseenShape(c, shapes); m != "" {
				return m
			}
		}
	}
	return ""
}

func decodeJSON(raw []byte) (any, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// pointerSet replaces the value at an RFC 6901 JSON pointer, which must
// already exist, and returns the value it held.
func pointerSet(doc any, ptr string, value any) (any, error) {
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("a JSON pointer starts with /")
	}
	tokens := strings.Split(ptr[1:], "/")
	node := doc
	for i, tok := range tokens {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		last := i == len(tokens)-1
		switch n := node.(type) {
		case map[string]any:
			child, ok := n[tok]
			if !ok {
				return nil, fmt.Errorf("no %q in the source", tok)
			}
			if last {
				n[tok] = value
				return child, nil
			}
			node = child
		case []any:
			idx := -1
			if _, err := fmt.Sscanf(tok, "%d", &idx); err != nil || idx < 0 || idx >= len(n) || fmt.Sprint(idx) != tok {
				return nil, fmt.Errorf("no element %q in the source", tok)
			}
			if last {
				old := n[idx]
				n[idx] = value
				return old, nil
			}
			node = n[idx]
		default:
			return nil, fmt.Errorf("%q indexes a scalar", tok)
		}
	}
	return nil, fmt.Errorf("empty pointer")
}

func checkExchange(ex Exchange) error {
	req, resp := ex.Request, ex.Response
	cli, web := len(req.Argv) > 0, req.Method != "" || req.Path != ""
	switch {
	case cli == web:
		return fmt.Errorf("a request is either a CLI argv or an HTTP method and path")
	case len(req.Body) > 0 && req.BodyText != "":
		return fmt.Errorf("request has both body and bodyText")
	case cli && (req.Query != nil || req.Headers != nil || len(req.Body) > 0 || req.BodyText != ""):
		return fmt.Errorf("a CLI request is its argv alone")
	case cli && (resp.Status != 0 || resp.Headers != nil || len(resp.Body) > 0 || resp.BodyText != ""):
		return fmt.Errorf("a CLI answer is stdout, stderr and exit, not an HTTP response")
	case cli && len(resp.Stdout) > 0 && resp.StdoutText != "":
		return fmt.Errorf("answer has both stdout and stdoutText")
	case web && (req.Method == "" || req.Path == "" || resp.Status == 0):
		return fmt.Errorf("an HTTP exchange needs method, path and a response status")
	case web && (len(resp.Stdout) > 0 || resp.StdoutText != "" || resp.Stderr != "" || resp.Exit != 0):
		return fmt.Errorf("an HTTP answer is status, headers and body, not CLI output")
	case web && len(resp.Body) > 0 && resp.BodyText != "":
		return fmt.Errorf("answer has both body and bodyText")
	case web && req.Method == http.MethodPost && req.Path == "/" && len(req.Body) > 0 && req.Headers["X-Amz-Target"] == "":
		return fmt.Errorf("an AWS JSON-protocol request must record its X-Amz-Target")
	}
	for _, headers := range []map[string]string{req.Headers, resp.Headers} {
		for k, v := range headers {
			if secretHeader[strings.ToLower(k)] && v != "" {
				return fmt.Errorf("recorded credential header %s; record it as \"\" (absent) or leave it out", k)
			}
		}
	}
	return nil
}

// requestKey is a request's identity, with a JSON body in canonical form.
func requestKey(req Request) string {
	if req.Headers != nil {
		canonical := map[string]string{}
		for k, v := range req.Headers {
			canonical[http.CanonicalHeaderKey(k)] = v
		}
		req.Headers = canonical
	}
	if len(req.Body) > 0 {
		var v any
		dec := json.NewDecoder(bytes.NewReader(req.Body))
		dec.UseNumber()
		if dec.Decode(&v) == nil {
			req.Body, _ = json.Marshal(v)
		}
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// PaginationMarker is the form a recorded pagination token takes: the capture
// replaces each real token (an opaque, often base64 blob a secret scanner
// cannot tell from a key) with "<pagination token N>", consistently, so a
// request that sends a token back still matches the answer that gave it.
var PaginationMarker = regexp.MustCompile(`^<pagination token [0-9]+>$`)

var paginationKeys = map[string]bool{"nexttoken": true, "nextforwardtoken": true, "nextbackwardtoken": true,
	"nextmarker": true, "marker": true, "paginationtoken": true, "continuationtoken": true, "nextcontinuationtoken": true}

// checkPaginationTokens refuses a pagination token, in an answer or passed
// back with --next-token (or --starting-token), that is not a marker.
func checkPaginationTokens(r *Recording) error {
	for i, ex := range r.Exchanges {
		for j, a := range ex.Request.Argv {
			if (a == "--next-token" || a == "--starting-token") && j+1 < len(ex.Request.Argv) && !PaginationMarker.MatchString(ex.Request.Argv[j+1]) {
				return fmt.Errorf("exchange %d: %s %q is a real pagination token; record it as <pagination token N>", i, a, ex.Request.Argv[j+1])
			}
		}
		for _, body := range [][]byte{ex.Response.Body, ex.Response.Stdout} {
			if len(body) == 0 {
				continue
			}
			v, err := decodeJSON(body)
			if err != nil {
				continue
			}
			if bad := unmarkedToken(v); bad != "" {
				return fmt.Errorf("exchange %d: %s is a real pagination token; record it as <pagination token N>", i, bad)
			}
		}
	}
	return nil
}

func unmarkedToken(v any) string {
	switch n := v.(type) {
	case map[string]any:
		for k, c := range n {
			if s, ok := c.(string); ok && paginationKeys[strings.ToLower(k)] && s != "" && !PaginationMarker.MatchString(s) {
				return k
			}
			if bad := unmarkedToken(c); bad != "" {
				return bad
			}
		}
	case []any:
		for _, c := range n {
			if bad := unmarkedToken(c); bad != "" {
				return bad
			}
		}
	}
	return ""
}

// CheckScrubbed refuses credentials, account ids and addresses a capture
// must replace. An allowed value excuses only a match exactly equal to it.
func CheckScrubbed(raw []byte, allow []Allowed) error {
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a.Value] = true
	}
	if m := bearerToken.Find(raw); m != nil {
		return fmt.Errorf("recorded bearer token %.12s…; replace it at capture", m)
	}
	for _, m := range rdsHost.FindAllSubmatch(raw, -1) {
		if strings.ToLower(string(m[1])) != DocumentationRDSID {
			return fmt.Errorf("unscrubbed RDS endpoint %s; its host id is the account's, use %s", m[0], DocumentationRDSID)
		}
	}
	if m := accessKeyID.Find(raw); m != nil {
		return fmt.Errorf("recorded AWS access key id %.8s…; replace it at capture", m)
	}
	for _, loc := range digitRun.FindAllIndex(raw, -1) {
		m := string(raw[loc[0]:loc[1]])
		if len(m) != 12 || m == DocumentationAccountID || allowed[m] {
			continue
		}
		before := loc[0] > 0 && raw[loc[0]-1] == '.'
		after := loc[1]+1 < len(raw) && raw[loc[1]] == '.' && raw[loc[1]+1] >= '0' && raw[loc[1]+1] <= '9'
		if before || after {
			continue // part of a decimal number, not an identifier
		}
		if loc[0] >= 24 && uuidHead.Match(raw[loc[0]-24:loc[0]]) && (loc[1] == len(raw) || !isHexByte(raw[loc[1]])) {
			continue // the last group of a UUID, which is twelve hex digits
		}
		if inLongHexToken(raw, loc[0], loc[1]) {
			continue // digits inside a hex identifier (a task id), not an account id
		}
		return fmt.Errorf("unscrubbed AWS account id %s; use %s, or allow it with a reason if it is not one", m, DocumentationAccountID)
	}
	type addr struct{ text, ip string }
	var addrs []addr
	for _, m := range dottedRun.FindAll(raw, -1) {
		if strings.Count(string(m), ".") == 3 {
			addrs = append(addrs, addr{string(m), string(m)})
		}
	}
	for _, m := range ipv4Host.FindAllSubmatch(raw, -1) {
		addrs = append(addrs, addr{string(m[0]), fmt.Sprintf("%s.%s.%s.%s", m[1], m[2], m[3], m[4])})
	}
	for _, loc := range hexColonRun.FindAllIndex(raw, -1) {
		m := string(raw[loc[0]:loc[1]])
		// An address stands alone: text such as arn:aws:iam::… or AWS::ECS::
		// is a name with colons in it, not an address.
		if (loc[0] > 0 && isWordByte(raw[loc[0]-1])) || (loc[1] < len(raw) && isWordByte(raw[loc[1]])) {
			continue
		}
		groups := 0
		for _, g := range strings.Split(m, ":") {
			if g != "" {
				groups++
			}
		}
		if ip := net.ParseIP(m); ip != nil && ip.To4() == nil && groups >= 2 {
			addrs = append(addrs, addr{m, m})
		}
	}
	for _, a := range addrs {
		ip := net.ParseIP(a.ip)
		if ip == nil || allowed[a.text] {
			continue
		}
		inRange := false
		for _, n := range allowedNets {
			inRange = inRange || n.Contains(ip)
		}
		if !inRange {
			return fmt.Errorf("unscrubbed IP address %s; use 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 or 2001:db8::/32, or allow it with a reason if it is not one", a.text)
		}
	}
	return nil
}

// inLongHexToken reports whether raw[from:to] lies inside a run of hex
// characters at least 16 long: a task or resource id, where twelve digits in
// a row are chance, not an account id. (A run that long around exactly twelve
// digits must hold a hex letter.) An escape's letter (\n, \t), an underscore
// or any other separator ends the run.
func inLongHexToken(raw []byte, from, to int) bool {
	start, end := from, to
	for start > 0 && isHexByte(raw[start-1]) {
		start--
	}
	for end < len(raw) && isHexByte(raw[end]) {
		end++
	}
	return end-start >= 16
}

const (
	hexBytes  = "0123456789abcdefABCDEF"
	wordBytes = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_"
)

func isHexByte(b byte) bool { return strings.IndexByte(hexBytes, b) >= 0 }

func isWordByte(b byte) bool { return strings.IndexByte(wordBytes, b) >= 0 }

// Join replays several recordings as one: a scenario built from the
// recordings of its parts (the deploys, the alarms, a task's log). Each
// request must be recorded in exactly one of them, so which file answers is
// never ambiguous, and the joined replay keeps every rule: unrecorded requests
// fail, and every exchange of every part must be served unless the test calls
// Subset on the joined replay.
func Join(t T, recs ...*Recording) *Recording {
	t.Helper()
	j := &Recording{t: t}
	owner := map[string]int{}
	var names []string
	for i, r := range recs {
		if r == nil {
			t.Fatalf("testrecord: Join of a recording that did not load")
			return nil
		}
		names = append(names, filepath.Base(r.path))
		for _, ex := range r.Exchanges {
			key := requestKey(ex.Request)
			if other, ok := owner[key]; ok && other != i {
				t.Fatalf("testrecord: %s and %s both record %s; a joined request must come from one recording", recs[other].path, r.path, ex.Request.describe())
				return nil
			}
			owner[key] = i
			j.Exchanges = append(j.Exchanges, ex)
		}
		r.Subset() // the joined replay checks these exchanges instead
	}
	j.path = "join(" + strings.Join(names, ", ") + ")"
	j.served = make([]int, len(j.Exchanges))
	t.Cleanup(j.checkServed)
	return j
}

// Subset lets the test leave exchanges unserved. Say why at the call site:
// an unserved exchange is usually a recording the code no longer reaches.
func (r *Recording) Subset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subset = true
}

func (r *Recording) checkServed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subset {
		return
	}
	for i, n := range r.served {
		if n == 0 {
			r.t.Errorf("testrecord: %s: exchange %d (%s) was never served", r.path, i, r.Exchanges[i].Request.describe())
		}
	}
}

// take returns the next answer for a request that matches, or reports the
// miss against the closest recorded request.
func (r *Recording) take(match func(Request) bool, got string) (*Exchange, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := -1
	for i := range r.Exchanges {
		if !match(r.Exchanges[i].Request) {
			continue
		}
		if r.served[i] == 0 {
			r.served[i]++
			return &r.Exchanges[i], true
		}
		last = i
	}
	if last >= 0 && r.Exchanges[last].Repeat {
		r.served[last]++
		return &r.Exchanges[last], true
	}
	if last >= 0 {
		r.t.Errorf("testrecord: %s: %s asked more times than recorded", r.path, got)
		return nil, false
	}
	r.t.Errorf("testrecord: %s: no recorded exchange for %s; closest recorded: %s", r.path, got, r.closest(got))
	return nil, false
}

func (r *Recording) closest(got string) string {
	best, bestScore := "(none)", -1
	for _, ex := range r.Exchanges {
		d := ex.Request.describe()
		n := 0
		for n < len(d) && n < len(got) && d[n] == got[n] {
			n++
		}
		if n > bestScore {
			best, bestScore = d, n
		}
	}
	return best
}

func (req Request) describe() string {
	if len(req.Argv) > 0 {
		return strings.Join(req.Argv, " ")
	}
	return describeHTTP(req.Method, req.Path, url.Values(req.Query), req.Headers["X-Amz-Target"])
}

func describeHTTP(method, path string, query url.Values, target string) string {
	d := method + " " + path
	if len(query) > 0 {
		d += "?" + query.Encode()
	}
	if target != "" {
		d += " " + target
	}
	return d
}

// Cmd returns a command runner shaped like a wrapper over
// exec.Command(...).Output(): name and args must equal a recorded argv
// exactly. A recorded non-zero exit returns the recorded stdout with an error
// carrying the exit status and the recorded stderr.
func (r *Recording) Cmd() func(name string, args ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		r.t.Helper()
		argv := append([]string{name}, args...)
		got := strings.Join(argv, " ")
		ex, ok := r.take(func(req Request) bool { return reflect.DeepEqual(req.Argv, argv) }, got)
		if !ok {
			return nil, fmt.Errorf("testrecord: no recorded answer for %s", got)
		}
		out := []byte(ex.Response.StdoutText)
		if len(ex.Response.Stdout) > 0 {
			out = append([]byte(nil), ex.Response.Stdout...)
		}
		if ex.Response.Exit != 0 {
			return out, fmt.Errorf("%s: exit status %d: %s", got, ex.Response.Exit, ex.Response.Stderr)
		}
		return out, nil
	}
}

// Server returns an HTTP server that answers only recorded requests. A miss
// fails the test and answers 599, a status no client retries or mistakes for
// a real answer.
func (r *Recording) Server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		got := describeHTTP(req.Method, req.URL.Path, req.URL.Query(), req.Header.Get("X-Amz-Target"))
		ex, ok := r.take(func(rec Request) bool { return matchHTTP(rec, req, body) }, got)
		if !ok {
			http.Error(w, "testrecord: no recorded exchange", 599)
			return
		}
		keys := make([]string, 0, len(ex.Response.Headers))
		for k := range ex.Response.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w.Header().Set(k, ex.Response.Headers[k])
		}
		w.WriteHeader(ex.Response.Status)
		if len(ex.Response.Body) > 0 {
			_, _ = w.Write(ex.Response.Body)
		} else {
			_, _ = io.WriteString(w, ex.Response.BodyText)
		}
	}))
	r.t.Cleanup(srv.Close)
	return srv
}

func matchHTTP(rec Request, req *http.Request, body []byte) bool {
	if rec.Method == "" || rec.Method != req.Method || rec.Path != req.URL.Path {
		return false
	}
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil || len(rec.Query) != len(q) || (len(q) > 0 && !reflect.DeepEqual(url.Values(rec.Query), q)) {
		return false
	}
	for k, v := range rec.Headers {
		if v == "" {
			if _, present := req.Header[http.CanonicalHeaderKey(k)]; present {
				return false
			}
			continue
		}
		if got := req.Header.Values(k); len(got) != 1 || got[0] != v {
			return false
		}
	}
	switch {
	case len(rec.Body) > 0:
		return jsonEqual(rec.Body, body)
	case rec.BodyText != "":
		return rec.BodyText == string(body)
	default:
		return len(body) == 0
	}
}

func jsonEqual(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return nil, false
		}
		if _, err := dec.Token(); err != io.EOF {
			return nil, false // trailing data after the one JSON value
		}
		return v, true
	}
	x, okA := decode(a)
	y, okB := decode(b)
	return okA && okB && reflect.DeepEqual(x, y)
}
